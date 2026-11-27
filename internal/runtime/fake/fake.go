// Package fake is a deterministic, goroutine-safe implementation of
// agent.Runtime for tests (R-TECH-02). It starts no process: a script chooses,
// for every call, what the execution does, and the Runtime counts and records
// the calls so that a test can assert on them.
//
// Only _test.go files may import this package. It follows the provisional
// agent contract, so it may change with that contract (see package agent).
package fake

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/shutx-net/bellona/internal/agent"
	"github.com/shutx-net/bellona/internal/domain"
)

// interruptedExitCode is the exit code of an interrupted execution: 128 plus
// the number of SIGINT.
const interruptedExitCode = 130

// ErrWaitCalledTwice is returned by the second call of Wait on an execution.
// The agent contract allows one call, so a second one is a bug in the caller.
var ErrWaitCalledTwice = errors.New("fake runtime: Wait was called more than once")

// Behavior says what one call of Start or Resume does.
type Behavior struct {
	// StartErr, when set, makes the call fail with this error. No execution
	// exists then, and Started is not signalled.
	StartErr error
	// Gate holds the execution: Wait blocks until the channel is closed,
	// Cancel is called or the context of the call is cancelled. Without a
	// Gate, Wait does not block.
	Gate <-chan struct{}
	// Started, when not nil, receives the Call once the execution exists and
	// is counted by Active. The send does not outlive the context of the
	// call.
	Started chan<- Call
	// Outcome is the outcome of the result. The zero value means completed;
	// any other value, valid or not, is passed through unchanged.
	Outcome domain.ExecutionOutcome
	// Output is the output of the result.
	Output []byte
	// ExitCode is the exit code of the result.
	ExitCode int
	// WaitErr is returned by Wait together with the result.
	WaitErr error
	// Events are delivered on the Events channel of the execution.
	Events []agent.Event
}

// Call records one call of Start or Resume.
type Call struct {
	// Seq numbers the calls from 1, Start and Resume together.
	Seq int
	// Resume is true for a call of Resume.
	Resume bool
	// SessionID is the session ID that the execution reports in its result:
	// the given one for Resume, the one in the request for Start when the
	// capabilities advertise SessionIDBeforeLaunch and the request has one,
	// and "fake-<Seq>" otherwise.
	SessionID domain.SessionID
	// Request is the request as the caller passed it.
	Request agent.Request
}

// Runtime is a fake agent.Runtime. Create it with New.
type Runtime struct {
	caps   agent.Capabilities
	script func(Call) Behavior

	mu      sync.Mutex
	calls   []Call
	starts  int
	resumes int
	active  int
}

var _ agent.Runtime = (*Runtime)(nil)

// New returns a Runtime that reports caps and asks script for the Behavior of
// every call. A nil script gives the zero Behavior: a completed execution
// with no output.
//
// The Runtime calls script with its lock held, so the calls are made one at a
// time in the order of Call.Seq and the script needs no locking of its own;
// for the same reason it must not call a method of the Runtime. It receives a
// copy of the Call.
//
// The capabilities are reported, but they are not enforced except that
// SessionIDBeforeLaunch decides the session ID of a started execution. To
// simulate a host that cannot resume, make the script return StartErr for a
// call whose Resume field is true.
func New(caps agent.Capabilities, script func(Call) Behavior) *Runtime {
	return &Runtime{caps: caps, script: script}
}

// Capabilities returns the capabilities given to New.
func (r *Runtime) Capabilities(context.Context) (agent.Capabilities, error) {
	return r.caps, nil
}

// Start records the call, consults the script and, unless the Behavior has a
// StartErr, returns a new execution. It signals Behavior.Started if there is
// one, and gives up that send, not the execution, when ctx is cancelled first.
func (r *Runtime) Start(ctx context.Context, req agent.Request) (agent.Execution, error) {
	return r.launch(ctx, false, "", req)
}

// Resume is like Start for an existing session: the execution reports
// sessionID as its session ID.
func (r *Runtime) Resume(ctx context.Context, sessionID domain.SessionID, req agent.Request) (agent.Execution, error) {
	return r.launch(ctx, true, sessionID, req)
}

// Starts returns the number of calls of Start, including failed ones.
func (r *Runtime) Starts() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.starts
}

// Resumes returns the number of calls of Resume, including failed ones.
func (r *Runtime) Resumes() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.resumes
}

// Calls returns a copy of the recorded calls in the order of Seq.
func (r *Runtime) Calls() []Call {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Call, len(r.calls))
	for i, c := range r.calls {
		out[i] = cloneCall(c)
	}
	return out
}

// Active returns the number of executions whose Wait has not returned. A
// call that failed with StartErr never counts.
func (r *Runtime) Active() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active
}

// record numbers and stores the call, asks the script for its Behavior and
// counts the new execution as active, all under the lock, so that a test that
// has received the Call from Started finds it counted.
func (r *Runtime) record(resume bool, given domain.SessionID, req agent.Request) (Call, Behavior) {
	r.mu.Lock()
	defer r.mu.Unlock()
	seq := len(r.calls) + 1
	call := Call{Seq: seq, Resume: resume, Request: cloneRequest(req)}
	switch {
	case resume:
		call.SessionID = given
		r.resumes++
	case r.caps.SessionIDBeforeLaunch && req.SessionID != "":
		call.SessionID = req.SessionID
		r.starts++
	default:
		call.SessionID = domain.SessionID(fmt.Sprintf("fake-%d", seq))
		r.starts++
	}
	r.calls = append(r.calls, call)
	var b Behavior
	if r.script != nil {
		b = r.script(cloneCall(call))
	}
	if b.StartErr == nil {
		r.active++
	}
	return call, b
}

func (r *Runtime) launch(ctx context.Context, resume bool, given domain.SessionID, req agent.Request) (agent.Execution, error) {
	call, b := r.record(resume, given, req)
	if b.StartErr != nil {
		return nil, b.StartErr
	}
	outcome := b.Outcome
	if outcome == "" {
		outcome = domain.OutcomeCompleted
	}
	events := make(chan agent.Event, len(b.Events))
	for _, ev := range b.Events {
		ev.Data = slices.Clone(ev.Data)
		events <- ev
	}
	close(events)
	e := &execution{
		rt:       r,
		ctx:      ctx,
		gate:     b.Gate,
		events:   events,
		cancelCh: make(chan struct{}),
		result: agent.Result{
			SessionID: call.SessionID,
			Outcome:   outcome,
			Output:    slices.Clone(b.Output),
			ExitCode:  b.ExitCode,
		},
		waitErr: b.WaitErr,
	}
	if b.Started != nil {
		signal(ctx, b.Started, cloneCall(call))
	}
	return e, nil
}

// signal sends call on ch. It sends at once when it can, and otherwise waits
// for a receiver or for ctx, whichever comes first. It holds no lock.
func signal(ctx context.Context, ch chan<- Call, call Call) {
	select {
	case ch <- call:
		return
	default:
	}
	select {
	case ch <- call:
	case <-ctx.Done():
	}
}

var _ agent.Execution = (*execution)(nil)

// execution is one fake process. It is decided when Wait runs: a Cancel or a
// cancelled context seen by then makes the result interrupted, otherwise the
// scripted result is returned once the gate, if any, is open.
type execution struct {
	rt       *Runtime
	ctx      context.Context
	gate     <-chan struct{}
	events   chan agent.Event // filled and closed when the execution is created
	cancelCh chan struct{}    // closed by the first Cancel

	cancelOnce sync.Once
	waited     atomic.Bool

	result  agent.Result
	waitErr error
}

// Events returns the channel of scripted events. It is already filled and
// closed, so a consumer can drain it before, during or instead of Wait.
func (e *execution) Events() <-chan agent.Event { return e.events }

// Wait blocks until the gate is closed, Cancel is called or the context of the
// call is cancelled. A cancellation, also one that happened before Wait, wins
// over an open gate and gives the outcome interrupted with exit code 130, the
// session ID, no output and no error. Otherwise Wait returns the scripted
// result and error. The second call returns ErrWaitCalledTwice.
func (e *execution) Wait() (agent.Result, error) {
	if !e.waited.CompareAndSwap(false, true) {
		return agent.Result{}, ErrWaitCalledTwice
	}
	defer e.rt.release()
	if e.gate != nil && !e.cancelled() {
		select {
		case <-e.gate:
		case <-e.cancelCh:
		case <-e.ctx.Done():
		}
	}
	if e.cancelled() {
		return agent.Result{
			SessionID: e.result.SessionID,
			Outcome:   domain.OutcomeInterrupted,
			ExitCode:  interruptedExitCode,
		}, nil
	}
	return e.result, e.waitErr
}

// Cancel marks the execution as cancelled. It is idempotent, takes no notice
// of ctx and always returns nil. After Wait has returned it has no effect.
func (e *execution) Cancel(context.Context) error {
	e.cancelOnce.Do(func() { close(e.cancelCh) })
	return nil
}

// cancelled reports whether Cancel was called or the context of the call is
// done.
func (e *execution) cancelled() bool {
	select {
	case <-e.cancelCh:
		return true
	default:
	}
	return e.ctx.Err() != nil
}

// release uncounts an execution whose Wait is returning.
func (r *Runtime) release() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.active--
}

func cloneRequest(req agent.Request) agent.Request {
	req.Inputs = slices.Clone(req.Inputs)
	return req
}

func cloneCall(c Call) Call {
	c.Request = cloneRequest(c.Request)
	return c
}
