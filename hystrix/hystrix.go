package hystrix

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type runFunc func() error
type fallbackFunc func(error) error
type runFuncC func(context.Context) error
type fallbackFuncC func(context.Context, error) error

// A CircuitError is an error which models various failure states of execution,
// such as the circuit being open or a timeout.
type CircuitError struct {
	Message string
}

func (e CircuitError) Error() string {
	return "hystrix: " + e.Message
}

// command models the state used for a single execution on a circuit. "hystrix command" is commonly
// used to describe the pairing of your run/fallback functions with a circuit.
type command struct {
	sync.Mutex

	ticket      *struct{}
	start       time.Time
	errChan     chan error
	finished    chan bool
	circuit     *CircuitBreaker
	run         runFuncC
	fallback    fallbackFuncC
	runDuration time.Duration
	events      []string
}

var (
	// ErrMaxConcurrency occurs when too many of the same named command are executed at the same time.
	ErrMaxConcurrency = CircuitError{Message: "max concurrency"}
	// ErrCircuitOpen returns when an execution attempt "short circuits". This happens due to the circuit being measured as unhealthy.
	ErrCircuitOpen = CircuitError{Message: "circuit open"}
	// ErrTimeout occurs when the provided function takes too long to execute.
	ErrTimeout = CircuitError{Message: "timeout"}
)

// Go runs your function while tracking the health of previous calls to it.
// If your function begins slowing down or failing repeatedly, we will block
// new calls to it for you to give the dependent service time to repair.
//
// Define a fallback function if you want to define some code to execute during outages.

// 异步执行，将 runFunc 和 fallbackFunc 都用无 deadline 的 context 包装，复用下面的 GoC
func Go(name string, run runFunc, fallback fallbackFunc) chan error {
	runC := func(ctx context.Context) error {
		return run()
	}
	var fallbackC fallbackFuncC
	if fallback != nil {
		fallbackC = func(ctx context.Context, err error) error {
			return fallback(err)
		}
	}
	return GoC(context.Background(), name, runC, fallbackC)
}

// GoC runs your function while tracking the health of previous calls to it.
// If your function begins slowing down or failing repeatedly, we will block
// new calls to it for you to give the dependent service time to repair.
//
// Define a fallback function if you want to define some code to execute during outages.

// 异步执行，支持带 context 取消的 circuit breaker 执行
func GoC(ctx context.Context, name string, run runFuncC, fallback fallbackFuncC) chan error {
	cmd := &command{
		run:      run,
		fallback: fallback,
		start:    time.Now(),
		errChan:  make(chan error, 1),
		finished: make(chan bool, 1),
	}

	// dont have methods with explicit params and returns
	// let data come in and out naturally, like with any closure
	// explicit error return to give place for us to kill switch the operation (fallback)

	circuit, _, err := GetCircuit(name)
	if err != nil {
		cmd.errChan <- err
		return cmd.errChan
	}
	cmd.circuit = circuit
	ticketCond := sync.NewCond(cmd) // 为 ticket 创建条件变量，使用等待唤醒模式
	ticketChecked := false          // 当前请求还未检票
	// When the caller extracts error from returned errChan, it's assumed that
	// the ticket's been returned to executorPool. Therefore, returnTicket() can
	// not run after cmd.errorWithFallback().
	returnTicket := func() {
		cmd.Lock()
		// Avoid releasing before a ticket is acquired.
		for !ticketChecked { // 即便从 wait 中醒来，有可能仍然还没有检过票，所以需要 for 循环检查票是否检过
			ticketCond.Wait()
		}
		cmd.circuit.executorPool.Return(cmd.ticket)
		cmd.Unlock()
	}
	// Shared by the following two goroutines. It ensures only the faster
	// goroutine runs errWithFallback() and reportAllEvent().

	// returnOnce 保证只有一个 goroutine 执行 errWithFallback() and reportAllEvent().
	returnOnce := &sync.Once{}
	reportAllEvent := func() {
		err := cmd.circuit.ReportEvent(cmd.events, cmd.start, cmd.runDuration)
		if err != nil {
			log.Printf(err.Error())
		}
	}

	go func() {
		defer func() { cmd.finished <- true }() // 通知另外一个 goroutine 当前请求已经结束（1. 不允许请求访问，直接返回 2. 允许请求访问 a. 请求执行结束 b. 达到请求最大并发量，拿不到票进场）

		// Circuits get opened when recent executions have shown to have a high error rate.
		// Rejecting new executions allows backends to recover, and the circuit will allow
		// new traffic when it feels a healthly state has returned.
		if !cmd.circuit.AllowRequest() {
			// 不允许请求进入
			cmd.Lock()
			// It's safe for another goroutine to go ahead releasing a nil ticket.
			// 标记票已经 check，是为了让另外一个 负责超时/cancel 的goroutine （如果已经超时/cancel）从 wait 逻辑里醒来之后跳出 for 循环。
			// 实际上，当前并没有拿到 ticket, cmd.ticket 是 nil, 但是另外一个 goroutine return a nil ticket 不会做任何事
			ticketChecked = true
			ticketCond.Signal()
			cmd.Unlock()
			// 执行 return nil ticket, 根据  ErrCircuitOpen 执行 fallback 函数，将该请求中所有的统计数据 发出
			returnOnce.Do(func() {
				returnTicket()
				cmd.errorWithFallback(ctx, ErrCircuitOpen)
				reportAllEvent()
			})
			return
		}

		// 允许请求进入

		// As backends falter, requests take longer but don't always fail.
		//
		// When requests slow down but the incoming rate of requests stays the same, you have to
		// run more at a time to keep up. By controlling concurrency during these situations, you can
		// shed load which accumulates due to the increasing ratio of active commands to incoming requests.
		cmd.Lock()
		select {
		case cmd.ticket = <-circuit.executorPool.Tickets:
			// 拿到 ticket，检票
			ticketChecked = true
			ticketCond.Signal()
			// 唤醒可能进入 cond.wait 状态的 负责 超时/cancel 的 goroutine，让其能 退票、执行 errorWithFallback
			cmd.Unlock()
		default:
			// 拿不到票，并发请求数太多
			ticketChecked = true
			ticketCond.Signal()
			cmd.Unlock()
			// 报并发量限制错误
			returnOnce.Do(func() {
				returnTicket()
				cmd.errorWithFallback(ctx, ErrMaxConcurrency)
				reportAllEvent()
			})
			return
		}

		// 执行真正的调用逻辑
		runStart := time.Now()
		runErr := run(ctx) // 假设正在执行的过程中， timeout 这时另一个 goroutine 执行 return ticket,
		returnOnce.Do(func() {
			defer reportAllEvent()
			cmd.runDuration = time.Since(runStart)
			returnTicket()
			if runErr != nil {
				cmd.errorWithFallback(ctx, runErr)
				return
			}
			cmd.reportEvent("success")
		})
	}()

	go func() {
		timer := time.NewTimer(getSettings(name).Timeout)
		defer timer.Stop()

		select {
		case <-cmd.finished: // 另一个 goroutine 结束
			// returnOnce has been executed in another goroutine
		case <-ctx.Done(): // context cancel
			returnOnce.Do(func() {
				returnTicket()
				cmd.errorWithFallback(ctx, ctx.Err())
				reportAllEvent()
			})
			return
		case <-timer.C: // 超时
			returnOnce.Do(func() {
				returnTicket()
				cmd.errorWithFallback(ctx, ErrTimeout)
				reportAllEvent()
			})
			return
		}
	}()

	return cmd.errChan
}

// Do runs your function in a synchronous manner, blocking until either your function succeeds
// or an error is returned, including hystrix circuit errors
func Do(name string, run runFunc, fallback fallbackFunc) error {
	runC := func(ctx context.Context) error {
		return run()
	}
	var fallbackC fallbackFuncC
	if fallback != nil {
		fallbackC = func(ctx context.Context, err error) error {
			return fallback(err)
		}
	}
	return DoC(context.Background(), name, runC, fallbackC)
}

// DoC runs your function in a synchronous manner, blocking until either your function succeeds
// or an error is returned, including hystrix circuit errors
func DoC(ctx context.Context, name string, run runFuncC, fallback fallbackFuncC) error {
	done := make(chan struct{}, 1)

	r := func(ctx context.Context) error {
		err := run(ctx)
		if err != nil {
			return err
		}

		done <- struct{}{}
		return nil
	}

	f := func(ctx context.Context, e error) error {
		err := fallback(ctx, e)
		if err != nil {
			return err
		}

		done <- struct{}{}
		return nil
	}

	var errChan chan error
	if fallback == nil {
		errChan = GoC(ctx, name, r, nil)
	} else {
		errChan = GoC(ctx, name, r, f)
	}

	select {
	case <-done:
		return nil
	case err := <-errChan:
		return err
	}
}

func (c *command) reportEvent(eventType string) {
	c.Lock()
	defer c.Unlock()

	c.events = append(c.events, eventType)
}

// errorWithFallback triggers the fallback while reporting the appropriate metric events.
func (c *command) errorWithFallback(ctx context.Context, err error) {
	eventType := "failure"
	if err == ErrCircuitOpen {
		eventType = "short-circuit"
	} else if err == ErrMaxConcurrency {
		eventType = "rejected"
	} else if err == ErrTimeout {
		eventType = "timeout"
	} else if err == context.Canceled {
		eventType = "context_canceled"
	} else if err == context.DeadlineExceeded {
		eventType = "context_deadline_exceeded"
	}

	c.reportEvent(eventType)
	fallbackErr := c.tryFallback(ctx, err)
	if fallbackErr != nil {
		c.errChan <- fallbackErr
	}
}

func (c *command) tryFallback(ctx context.Context, err error) error {
	if c.fallback == nil {
		// If we don't have a fallback return the original error.
		return err
	}

	fallbackErr := c.fallback(ctx, err)
	if fallbackErr != nil {
		c.reportEvent("fallback-failure")
		return fmt.Errorf("fallback failed with '%v'. run error was '%v'", fallbackErr, err)
	}

	c.reportEvent("fallback-success")

	return nil
}
