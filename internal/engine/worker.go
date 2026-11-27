package engine

import (
	"context"
	"errors"

	"github.com/shutx-net/bellona/internal/agent"
	"github.com/shutx-net/bellona/internal/domain"
)

// resultKind says what a workerResult reports.
type resultKind int

const (
	// resultStartFailed: Runtime.Start returned an error. It is the only
	// result of the worker.
	resultStartFailed resultKind = iota + 1
	// resultLaunched: the execution exists. The worker sends it first and
	// resultDone after it.
	resultLaunched
	// resultDone: Execution.Wait returned. It is the last result of the
	// worker.
	resultDone
)

// workerResult is what a worker sends to the control loop. A worker reports
// facts; the loop decides which events they become.
type workerResult struct {
	id     domain.AttemptID
	kind   resultKind
	result agent.Result // resultDone
	err    error        // resultStartFailed, and resultDone when Wait failed
}

// final reports whether the result is the last one of its worker.
func (r workerResult) final() bool { return r.kind != resultLaunched }

// runAttempt is the body of a worker goroutine. It starts the execution,
// reports the launch, waits for the end and reports the result. It never
// touches the store or the state of the Run: everything it knows goes to the
// control loop through out. Every send also selects on loopDone, so a worker
// never blocks after the loop has stopped.
//
// ctx must live as long as the execution: the runtime stops the execution
// when it is cancelled. The loop cancels it when the Run handle stops.
func runAttempt(ctx context.Context, rt agent.Runtime, id domain.AttemptID, req agent.Request,
	out chan<- workerResult, loopDone <-chan struct{}) {
	send := func(r workerResult) {
		r.id = id
		select {
		case out <- r:
		case <-loopDone:
		}
	}
	exec, err := rt.Start(ctx, req)
	if err == nil && exec == nil {
		err = errors.New("the runtime returned no execution and no error")
	}
	if err != nil {
		send(workerResult{kind: resultStartFailed, err: err})
		return
	}
	// The normalized agent events are logged from M1a; M0 only has to keep
	// the runtime from waiting for a consumer.
	go func() {
		for range exec.Events() {
		}
	}()
	send(workerResult{kind: resultLaunched})
	// Wait is called even when the loop is gone, to reap the execution.
	res, werr := exec.Wait()
	send(workerResult{kind: resultDone, result: res, err: werr})
}
