package engine

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"unicode/utf8"

	"github.com/shutx-net/bellona/internal/agent"
	"github.com/shutx-net/bellona/internal/domain"
	"github.com/shutx-net/bellona/internal/projection"
	"github.com/shutx-net/bellona/internal/storage"
)

// newFile is an artifact that a commit finalizes before it appends its event.
type newFile struct {
	Path string // Run-relative
	Data []byte
}

// reqKind is the kind of a request to the control loop.
type reqKind int

const (
	reqState reqKind = iota + 1
	reqPropose
	reqStart
	reqAwait
	reqClose
)

// request is a message from a Run handle to the control loop. The reply
// channel has room for one reply, so the loop never blocks on a caller.
type request struct {
	kind    reqKind
	ctx     context.Context
	reply   chan reply
	payload domain.Payload // reqPropose
	files   []newFile      // reqPropose
	spec    AttemptSpec    // reqStart
	attempt domain.AttemptID
}

// reply is the answer to a request.
type reply struct {
	state   projection.State // reqState
	event   domain.Event     // reqPropose
	id      domain.AttemptID // reqStart
	attempt projection.Attempt
	err     error
}

// waiter is a pending AwaitAttempt.
type waiter struct {
	id    domain.AttemptID
	reply chan reply
}

// exitInfo carries the result of stopping the loop to the Run handles. The
// loop sets it before it closes the done channel.
type exitInfo struct{ err error }

// loop is the control loop of one open Run. Its goroutine is the only code that
// calls Store methods or touches the projected state, the waiters, the count of
// running workers and the flags below (invariants 9 and 14).
type loop struct {
	eng   *Engine
	id    domain.RunID
	store *storage.Store
	state projection.State

	requests chan request
	results  chan workerResult
	force    chan struct{} // closed to stop at once, without waiting for workers
	done     chan struct{} // closed when the loop has stopped
	exit     *exitInfo

	workCtx    context.Context // parent of every worker context; lives as long as the loop
	cancelWork context.CancelFunc

	active  int   // workers that have not delivered their last result
	closing bool  // Close was requested: no new work, wait for the workers
	failed  error // a storage failure stopped the Run handle (fail-stop)
	waiters []waiter
	caps    agent.Capabilities
	hasCaps bool
}

// startRun starts the control loop of a Run that owns store, with state as the
// state that the journal produces. It returns the handle.
func (e *Engine) startRun(id domain.RunID, store *storage.Store, state projection.State) *Run {
	workCtx, cancel := context.WithCancel(context.Background())
	l := &loop{
		eng:        e,
		id:         id,
		store:      store,
		state:      state,
		requests:   make(chan request),
		results:    make(chan workerResult),
		force:      make(chan struct{}),
		done:       make(chan struct{}),
		exit:       &exitInfo{},
		workCtx:    workCtx,
		cancelWork: cancel,
	}
	go l.run()
	return newRun(l)
}

// run is the loop. It serves requests and worker results one at a time until a
// Close finds no worker left (or the Run failed), or a forced stop arrives.
func (l *loop) run() {
	for {
		select {
		case req := <-l.requests:
			l.handle(req)
		case res := <-l.results:
			l.handleResult(res)
		case <-l.force:
			l.finish()
			return
		}
		if l.closing && (l.active == 0 || l.failed != nil) {
			l.finish()
			return
		}
	}
}

// finish stops the loop: it cancels the workers, releases the Run lock, which
// writes nothing, and answers everyone who still waits.
func (l *loop) finish() {
	l.cancelWork()
	err := l.store.Close()
	if err != nil {
		l.eng.logger.Error("releasing the Run failed", "run", l.id, "error", err)
	}
	waitErr := ErrClosed
	if l.failed != nil {
		waitErr = l.failedErr()
	}
	for _, w := range l.waiters {
		w.reply <- reply{err: waitErr}
	}
	l.waiters = nil
	l.exit.err = err
	close(l.done)
}

// failedErr is the error of every call after a storage failure.
func (l *loop) failedErr() error { return fmt.Errorf("%w: %w", ErrRunFailed, l.failed) }

// fail stops the Run handle after a storage failure (fail-stop): the files stay
// as the failure left them, every later request gets ErrRunFailed, the workers
// are cancelled and their results are dropped. It returns the error that the
// failed call reports.
func (l *loop) fail(what string, cause error) error {
	if l.failed == nil {
		l.failed = fmt.Errorf("%s: %w", what, cause)
		l.cancelWork()
		l.eng.logger.Error("the Run handle stopped after a storage failure", "run", l.id, "error", l.failed)
		for _, w := range l.waiters {
			w.reply <- reply{err: l.failedErr()}
		}
		l.waiters = nil
	}
	return l.failedErr()
}

// handle serves one request.
func (l *loop) handle(req request) {
	if req.kind == reqClose {
		l.closing = true
		return
	}
	if err := req.ctx.Err(); err != nil {
		req.reply <- reply{err: err}
		return
	}
	switch req.kind {
	case reqState:
		if l.failed != nil {
			req.reply <- reply{err: l.failedErr()}
			return
		}
		req.reply <- reply{state: l.state.Clone()}
	case reqPropose:
		if l.failed == nil && l.closing {
			req.reply <- reply{err: ErrClosed}
			return
		}
		ev, err := l.commit(req.payload, req.files)
		req.reply <- reply{event: ev, err: err}
	case reqStart:
		req.reply <- l.startAttempt(req)
	case reqAwait:
		l.await(req)
	}
}

// commit is the one path by which an event enters the journal. It proposes
// payload as the next event of the Run, with the files that the payload's
// artifact references need:
//
//  1. fail if the Run handle stopped;
//  2. build the event with the next sequence number and the clock time;
//  3. encode it and decode the record again, so that what the reducer applies
//     is exactly what the journal will hold;
//  4. apply it to the state: a rejection returns the reducer's error and
//     nothing has been written;
//  5. check, before any write, that every artifact reference of the payload is
//     one of the files or an artifact that an earlier event registered;
//  6. finalize the files;
//  7. append and sync the event;
//  8. adopt the new state;
//  9. replace the snapshot;
//  10. answer the waiters.
//
// A storage error in steps 6 to 9 stops the Run handle.
func (l *loop) commit(payload domain.Payload, files []newFile) (domain.Event, error) {
	if l.failed != nil {
		return domain.Event{}, l.failedErr()
	}
	if payload == nil || reflect.TypeOf(payload).Kind() == reflect.Pointer {
		return domain.Event{}, fmt.Errorf("%w: a payload is a non-nil value, not %T", projection.ErrInvalidEvent, payload)
	}
	ev := domain.Event{
		SchemaVersion: domain.EventSchemaVersion,
		Seq:           l.state.LastSeq + 1,
		Type:          payload.EventType(),
		RunID:         l.id,
		Time:          l.eng.clock().UTC().Round(0),
		Payload:       payload,
	}
	// An event that does not even encode is an invalid event like one that the
	// reducer refuses, so both classes of rejection match projection.ErrInvalidEvent.
	record, err := domain.EncodeEvent(ev)
	if err != nil {
		return domain.Event{}, fmt.Errorf("%w: %w", projection.ErrInvalidEvent, err)
	}
	applied, err := domain.DecodeEvent(record)
	if err != nil {
		return domain.Event{}, fmt.Errorf("%w: %w", projection.ErrInvalidEvent, err)
	}
	next, err := projection.Apply(l.state, applied)
	if err != nil {
		return domain.Event{}, err
	}
	planned, err := l.checkFiles(applied, files)
	if err != nil {
		return domain.Event{}, err
	}
	for i, f := range files {
		ref, err := l.store.WriteArtifact(f.Path, f.Data)
		if err != nil {
			return domain.Event{}, l.fail("finalize "+f.Path, err)
		}
		if ref != planned[i] {
			return domain.Event{}, l.fail("finalize "+f.Path, errors.New("the stored reference differs from the computed one"))
		}
	}
	if err := l.store.AppendEvent(record); err != nil {
		return domain.Event{}, l.fail(fmt.Sprintf("append %s (seq %d)", ev.Type, ev.Seq), err)
	}
	l.state = next
	snapshot, err := projection.EncodeSnapshot(next)
	if err != nil {
		return applied, l.fail("encode the snapshot", err)
	}
	if err := l.store.WriteSnapshot(snapshot); err != nil {
		return applied, l.fail(fmt.Sprintf("update the snapshot after %s (seq %d)", ev.Type, ev.Seq), err)
	}
	l.resolveWaiters()
	return applied, nil
}

// checkFiles validates the files of a commit before anything is written and
// returns their references. Every artifact reference of the applied event must
// be the reference of one of the files or of an artifact that an earlier event
// registered. A file that the payload does not refer to directly is allowed,
// because some files are referenced through another artifact: the role
// definitions through manifest.json, which Open verifies transitively.
func (l *loop) checkFiles(applied domain.Event, files []newFile) ([]domain.ArtifactRef, error) {
	registered := make(map[domain.ArtifactRef]bool, len(l.state.Artifacts))
	byPath := make(map[string]domain.ArtifactRef, len(l.state.Artifacts))
	for _, a := range l.state.Artifacts {
		registered[a] = true
		byPath[a.Path] = a
	}
	planned := make([]domain.ArtifactRef, len(files))
	provided := make(map[domain.ArtifactRef]bool, len(files))
	seen := make(map[string]bool, len(files))
	for i, f := range files {
		ref, err := domain.NewArtifactRef(f.Path, f.Data)
		switch {
		case err != nil:
			return nil, fmt.Errorf("%w: file %q: %w", projection.ErrInvalidEvent, f.Path, err)
		case domain.IsReservedPath(f.Path):
			return nil, fmt.Errorf("%w: file %q: %w", projection.ErrInvalidEvent, f.Path, storage.ErrReservedPath)
		case int64(len(f.Data)) > storage.DefaultMaxArtifactSize:
			return nil, fmt.Errorf("%w: file %q: %w", projection.ErrInvalidEvent, f.Path, storage.ErrArtifactTooLarge)
		case seen[f.Path]:
			return nil, fmt.Errorf("%w: file %q is listed twice", projection.ErrInvalidEvent, f.Path)
		}
		seen[f.Path] = true
		if known, ok := byPath[f.Path]; ok && known != ref {
			return nil, fmt.Errorf("%w: file %q differs from the artifact registered at that path", ErrArtifactRefMismatch, f.Path)
		}
		planned[i] = ref
		provided[ref] = true
	}
	for _, ref := range applied.Payload.ArtifactRefs() {
		if !provided[ref] && !registered[ref] {
			return nil, fmt.Errorf("%w: %s (size %d, sha256 %s)", ErrArtifactRefMismatch, ref.Path, ref.Size, ref.SHA256)
		}
	}
	return planned, nil
}

// resolveWaiters answers the AwaitAttempt requests whose Attempt is no longer
// Scheduled or Running.
func (l *loop) resolveWaiters() {
	kept := l.waiters[:0]
	for _, w := range l.waiters {
		a, ok := l.state.Attempt(w.id)
		if ok && pending(a) {
			kept = append(kept, w)
			continue
		}
		w.reply <- reply{attempt: a}
	}
	clear(l.waiters[len(kept):])
	l.waiters = kept
}

// pending reports whether the Attempt is Scheduled or Running, so that its
// worker still owes a result.
func pending(a projection.Attempt) bool {
	return a.State == domain.AttemptStateScheduled || a.State == domain.AttemptStateRunning
}

// await serves an AwaitAttempt request.
func (l *loop) await(req request) {
	if l.failed != nil {
		req.reply <- reply{err: l.failedErr()}
		return
	}
	a, ok := l.state.Attempt(req.attempt)
	switch {
	case !ok:
		req.reply <- reply{err: fmt.Errorf("%w: %s", ErrUnknownAttempt, req.attempt)}
	case !pending(a):
		req.reply <- reply{attempt: a}
	default:
		l.waiters = append(l.waiters, waiter{id: req.attempt, reply: req.reply})
	}
}

// startAttempt serves a StartAttempt request. AttemptScheduled is committed
// before the runtime is called: when that commit fails, nothing is launched.
func (l *loop) startAttempt(req request) reply {
	spec := req.spec
	switch {
	case l.failed != nil:
		return reply{err: l.failedErr()}
	case l.closing:
		return reply{err: ErrClosed}
	}
	if _, ok := contractFor(spec.Role); !ok {
		return reply{err: fmt.Errorf("%w: %q", ErrNoOutputContract, string(spec.Role))}
	}
	rt := l.eng.runtime
	if rt == nil {
		return reply{err: ErrNoRuntime}
	}
	if !l.hasCaps {
		caps, err := rt.Capabilities(req.ctx)
		if err != nil {
			return reply{err: fmt.Errorf("engine: query the runtime capabilities: %w", err)}
		}
		l.caps, l.hasCaps = caps, true
	}
	id, err := domain.NewAttemptID(l.eng.rand)
	if err != nil {
		return reply{err: fmt.Errorf("engine: draw an Attempt ID: %w", err)}
	}
	var session domain.SessionID
	if l.caps.SessionIDBeforeLaunch {
		if session, err = domain.NewSessionID(l.eng.rand); err != nil {
			return reply{err: fmt.Errorf("engine: draw a session ID: %w", err)}
		}
	}
	scheduled := domain.AttemptScheduled{AttemptID: id, Role: spec.Role, PhaseID: spec.PhaseID, SessionID: session}
	if spec.Role.PhaseBound() {
		scheduled.Revision = l.state.Run.PlanRevision
	}
	if _, err := l.commit(scheduled, nil); err != nil {
		return reply{err: err}
	}
	l.active++
	go runAttempt(l.workCtx, rt, id, agent.Request{
		Role:         spec.Role,
		Model:        spec.Model,
		Effort:       spec.Effort,
		Instructions: spec.Instructions,
		SessionID:    session,
	}, l.results, l.done)
	return reply{id: id}
}

// handleResult turns a report of a worker into an event. A rejected event is
// logged and dropped: the worker has nothing more to say, and a Run that has
// stopped drops every result.
func (l *loop) handleResult(res workerResult) {
	if res.final() {
		l.active--
	}
	if l.failed != nil {
		return
	}
	switch res.kind {
	case resultLaunched:
		// The session ID was assigned before launch where the host allows it;
		// otherwise the draft contract has no way to learn it before Wait.
		l.record(domain.AttemptStarted{AttemptID: res.id})
	case resultStartFailed:
		// The launch may have partly happened, so the outcome is unknown and
		// the Attempt is never retried here (invariant 11).
		l.eng.logger.Warn("starting the agent failed", "run", l.id, "attempt", res.id, "error", res.err)
		l.record(domain.AttemptOutcomeUnknown{AttemptID: res.id})
	case resultDone:
		l.finishAttempt(res)
	}
}

// finishAttempt commits the outcome of an execution that has ended.
func (l *loop) finishAttempt(res workerResult) {
	id := res.id
	if res.err != nil {
		l.eng.logger.Warn("waiting for the agent failed", "run", l.id, "attempt", id, "error", res.err)
		l.record(domain.AttemptOutcomeUnknown{AttemptID: id})
		return
	}
	switch outcome := res.result.Outcome; outcome {
	case domain.OutcomeCompleted:
		l.finishCompleted(id, res.result.Output)
	case domain.OutcomeOutputInvalid:
		l.record(domain.AttemptFailed{AttemptID: id, Outcome: outcome, ErrorClass: domain.ErrorClassInvalidAgentOutput})
	case domain.OutcomeLimitReached:
		l.record(domain.AttemptFailed{AttemptID: id, Outcome: outcome, ErrorClass: domain.ErrorClassTurnLimitReached})
	case domain.OutcomeFailed, domain.OutcomeInterrupted:
		l.record(domain.AttemptFailed{AttemptID: id, Outcome: outcome})
	default:
		// The host reported an outcome that the contract does not know.
		l.eng.logger.Warn("the agent reported an unknown outcome", "run", l.id, "attempt", id, "outcome", string(outcome))
		l.record(domain.AttemptOutcomeUnknown{AttemptID: id})
	}
}

// finishCompleted validates the output of a completed execution with the output
// contract of the Attempt's role. Valid output is stored and referenced by
// AttemptFinished; invalid output fails the Attempt as output_invalid.
func (l *loop) finishCompleted(id domain.AttemptID, output []byte) {
	a, ok := l.state.Attempt(id)
	if !ok {
		l.eng.logger.Error("a result arrived for an unknown Attempt", "run", l.id, "attempt", id)
		return
	}
	var stored []byte
	var err error
	if c, ok := contractFor(a.Role); ok {
		stored, err = c.validate(output)
	} else {
		err = fmt.Errorf("%w: %q", ErrNoOutputContract, string(a.Role))
	}
	if err != nil {
		l.record(domain.AttemptFailed{
			AttemptID:  id,
			Outcome:    domain.OutcomeOutputInvalid,
			ErrorClass: domain.ErrorClassInvalidAgentOutput,
			Detail:     detailText(err),
		})
		return
	}
	path := domain.AttemptOutputPath(id, 1)
	ref, err := domain.NewArtifactRef(path, stored)
	if err != nil {
		l.eng.logger.Error("the output path is invalid", "run", l.id, "attempt", id, "error", err)
		return
	}
	l.record(domain.AttemptFinished{AttemptID: id, Output: ref}, newFile{Path: path, Data: stored})
}

// record commits an event that a worker result caused. A commit that was
// rejected without a storage failure is logged: the result is dropped.
func (l *loop) record(p domain.Payload, files ...newFile) {
	if _, err := l.commit(p, files); err != nil && l.failed == nil {
		l.eng.logger.Error("an event caused by an agent result was rejected", "run", l.id, "event", string(p.EventType()), "error", err)
	}
}

// detailText shortens an error message to what an event may hold: valid UTF-8
// of at most domain.MaxTextBytes bytes.
func detailText(err error) string {
	s := strings.ToValidUTF8(err.Error(), "�")
	if len(s) > domain.MaxTextBytes {
		s = s[:domain.MaxTextBytes]
		for !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
	}
	return s
}
