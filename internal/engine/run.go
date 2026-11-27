package engine

import (
	"context"
	"sync"

	"github.com/shutx-net/bellona/internal/domain"
	"github.com/shutx-net/bellona/internal/projection"
)

// AttemptSpec describes an Attempt for StartAttempt.
type AttemptSpec struct {
	// Role is the role of the agent. Only the Implementer has an output
	// contract in M0.
	Role domain.Role
	// PhaseID is the Phase of the current Plan Revision that the Attempt works
	// for. An Implementer Attempt moves the Phase from Ready to Running.
	PhaseID domain.PhaseID
	// Instructions is the text the agent receives for the Turn.
	Instructions string
	// Model and Effort are passed to the runtime; empty means the host's
	// default.
	Model  string
	Effort string
}

// Run is the handle of an open Run. It holds the Run lock from Create or Open
// until Close, so at most one handle per Run exists across all processes. A
// Run holds no store and no state: every method sends a request to the control
// loop of the Run (see the package documentation) and waits for the answer, so
// the methods are safe for concurrent use and the loop stays the only writer.
type Run struct {
	id       domain.RunID
	requests chan<- request
	done     <-chan struct{}
	exit     *exitInfo
	stop     func() // makes the loop stop without waiting for workers
}

func newRun(l *loop) *Run {
	var once sync.Once
	return &Run{
		id:       l.id,
		requests: l.requests,
		done:     l.done,
		exit:     l.exit,
		stop:     func() { once.Do(func() { close(l.force) }) },
	}
}

// ID returns the Run ID.
func (r *Run) ID() domain.RunID { return r.id }

// call sends a request to the control loop and waits for its reply. When ctx
// ends after the request was accepted, the loop still carries it out: the
// caller learns only the context error, and State shows what happened.
func (r *Run) call(ctx context.Context, req request) (reply, error) {
	req.ctx = ctx
	req.reply = make(chan reply, 1)
	select {
	case r.requests <- req:
	case <-r.done:
		return reply{}, ErrClosed
	case <-ctx.Done():
		return reply{}, ctx.Err()
	}
	select {
	case rep := <-req.reply:
		return rep, rep.err
	case <-ctx.Done():
		return reply{}, ctx.Err()
	}
}

// State returns a copy of the projected state of the Run; changing the copy
// changes nothing. It fails with ErrClosed after Close and with an error
// wrapping ErrRunFailed after a storage failure.
func (r *Run) State(ctx context.Context) (projection.State, error) {
	rep, err := r.call(ctx, request{kind: reqState})
	if err != nil {
		return projection.State{}, err
	}
	return rep.state, nil
}

// StartAttempt schedules and starts one Attempt and returns its ID. It commits
// AttemptScheduled, which for an Implementer moves its Phase from Ready to
// Running, and only after that commit succeeded does it start the execution in
// a worker goroutine. The execution's launch and result are committed later
// by the control loop (AttemptStarted, then AttemptFinished, AttemptFailed or
// AttemptOutcomeUnknown); AwaitAttempt waits for them. An execution whose
// start or wait failed is never retried.
//
// The call is refused with ErrClosed after Close, with an error wrapping
// ErrRunFailed after a storage failure, with ErrNoOutputContract for a role
// that has no output contract, with ErrNoRuntime when the engine has no
// runtime, and with the reducer's error (fsm.ErrIllegalTransition or
// projection.ErrInvalidEvent) when AttemptScheduled is not allowed, for
// example for a Phase that is not Ready. Nothing is launched in any of these
// cases.
func (r *Run) StartAttempt(ctx context.Context, spec AttemptSpec) (domain.AttemptID, error) {
	rep, err := r.call(ctx, request{kind: reqStart, spec: spec})
	if err != nil {
		return "", err
	}
	return rep.id, nil
}

// AwaitAttempt blocks until the Attempt is no longer Scheduled or Running and
// returns a copy of it. An Attempt in Unknown or WaitingForUser counts as no
// longer running: the caller sees its state. It returns ErrUnknownAttempt for
// an Attempt that the Run does not have, ErrClosed if the Run handle stops
// first, and an error wrapping ErrRunFailed if a storage failure stops it.
func (r *Run) AwaitAttempt(ctx context.Context, id domain.AttemptID) (projection.Attempt, error) {
	rep, err := r.call(ctx, request{kind: reqAwait, attempt: id})
	if err != nil {
		return projection.Attempt{}, err
	}
	return rep.attempt, nil
}

// propose commits payload as the next event, with the artifacts that it needs,
// and returns the event as it was encoded and decoded. It is the commit path
// of the engine's own use and of the tests of this package; no exported
// method lets a caller commit an arbitrary event (invariant 9). A rejection
// returns the reducer's error and leaves the journal, the snapshot, the files
// and the state unchanged.
func (r *Run) propose(ctx context.Context, payload domain.Payload, files ...newFile) (domain.Event, error) {
	rep, err := r.call(ctx, request{kind: reqPropose, payload: payload, files: files})
	if err != nil {
		return domain.Event{}, err
	}
	return rep.event, nil
}

// Close stops the Run handle and releases the Run lock. It is idempotent. It
// stops accepting StartAttempt and propose (ErrClosed), keeps committing the
// results of running Attempts until none is left or ctx is done, then stops the
// control loop, cancels the workers, and releases the lock. It writes nothing
// else, not even after a storage failure.
//
// When ctx ends first, Close stops the loop at once and returns ctx.Err(); the
// Attempts that had not finished stay Scheduled or Running in the journal and
// the next Open reports them as uncertain.
func (r *Run) Close(ctx context.Context) error {
	select {
	case r.requests <- request{kind: reqClose}:
	case <-r.done:
		return r.exit.err
	}
	select {
	case <-r.done:
		return r.exit.err
	case <-ctx.Done():
		r.stop()
		<-r.done
		return ctx.Err()
	}
}
