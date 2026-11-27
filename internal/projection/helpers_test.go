package projection

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/shutx-net/bellona/internal/domain"
	"github.com/shutx-net/bellona/internal/fsm"
)

// ---------------------------------------------------------------------------
// Fixed values
// ---------------------------------------------------------------------------

const (
	testRunID domain.RunID     = "run-0123456789abcdef"
	phase1    domain.PhaseID   = "phase-01"
	phase2    domain.PhaseID   = "phase-02"
	session1  domain.SessionID = "0b0f1f0e-1d2c-4b3a-8f4e-5d6c7b8a9f01"
)

// baseTime is the origin of the event times of the tests. The event with
// sequence number n has the time baseTime + n seconds, so times strictly
// increase and are fixed.
var baseTime = time.Date(2026, 11, 28, 0, 0, 0, 123456789, time.UTC)

func timeAt(seq uint64) time.Time { return baseTime.Add(time.Duration(seq) * time.Second) }

func attemptID(n int) domain.AttemptID { return domain.AttemptID(fmt.Sprintf("attempt-%016x", n)) }
func requestID(n int) domain.RequestID { return domain.RequestID(fmt.Sprintf("req-%016x", n)) }

// ref returns a valid reference to a file at path whose content is derived from
// the path.
func ref(tb testing.TB, path string) domain.ArtifactRef {
	tb.Helper()
	return refWith(tb, path, "content of "+path)
}

// refWith returns a valid reference to a file at path with the given content.
func refWith(tb testing.TB, path, content string) domain.ArtifactRef {
	tb.Helper()
	r, err := domain.NewArtifactRef(path, []byte(content))
	if err != nil {
		tb.Fatalf("NewArtifactRef(%q): %v", path, err)
	}
	return r
}

// mkEvent stamps an event: current schema version, the fixed Run ID and the
// time that belongs to the sequence number.
func mkEvent(seq uint64, p domain.Payload) domain.Event {
	return domain.Event{
		SchemaVersion: domain.EventSchemaVersion,
		Seq:           seq,
		Type:          p.EventType(),
		RunID:         testRunID,
		Time:          timeAt(seq),
		Payload:       p,
	}
}

// nextEvent returns the event that follows s with the payload p.
func nextEvent(s State, p domain.Payload) domain.Event { return mkEvent(s.LastSeq+1, p) }

// canon returns the canonical bytes of s and fails the test when s cannot be
// encoded. The error is never discarded: two States that cannot be encoded must
// not look equal.
func canon(tb testing.TB, s State) []byte {
	tb.Helper()
	b, err := Canonical(s)
	if err != nil {
		tb.Fatalf("Canonical: %v", err)
	}
	return b
}

// apply applies p as the next event of s, requires success, and checks that s
// was not modified.
func apply(tb testing.TB, s State, p domain.Payload) State {
	tb.Helper()
	before := canon(tb, s)
	e := nextEvent(s, p)
	got, err := Apply(s, e)
	if err != nil {
		tb.Fatalf("Apply(%s) at seq %d: %v", e.Type, e.Seq, err)
	}
	if after := canon(tb, s); !bytes.Equal(before, after) {
		tb.Fatalf("Apply(%s) modified its argument:\nbefore %s\nafter  %s", e.Type, before, after)
	}
	if got.LastSeq != e.Seq {
		tb.Fatalf("Apply(%s): LastSeq is %d, want %d", e.Type, got.LastSeq, e.Seq)
	}
	if !got.Run.UpdatedAt.Equal(e.Time) {
		tb.Fatalf("Apply(%s): Run.UpdatedAt is %s, want the event time %s", e.Type, got.Run.UpdatedAt, e.Time)
	}
	return got
}

// mustReject applies p as the next event of s and requires an *ApplyError that
// matches the sentinel (ErrInvalidEvent or fsm.ErrIllegalTransition) and not
// the other one. It also checks that neither the argument nor the returned
// state differs from s.
func mustReject(tb testing.TB, s State, p domain.Payload, sentinel error) error {
	tb.Helper()
	return mustRejectEvent(tb, s, nextEvent(s, p), sentinel)
}

// mustRejectEvent is mustReject for an event with a hand-made envelope.
func mustRejectEvent(tb testing.TB, s State, e domain.Event, sentinel error) error {
	tb.Helper()
	other := fsm.ErrIllegalTransition
	if sentinel == fsm.ErrIllegalTransition {
		other = ErrInvalidEvent
	} else if sentinel != ErrInvalidEvent {
		tb.Fatalf("mustReject: %v is not one of the two sentinels", sentinel)
	}
	before := canon(tb, s)
	got, err := Apply(s, e)
	if err == nil {
		tb.Fatalf("Apply(%s) at seq %d succeeded, want an error matching %v", e.Type, e.Seq, sentinel)
	}
	var ae *ApplyError
	if !errors.As(err, &ae) {
		tb.Fatalf("Apply(%s): the error %T is not an *ApplyError", e.Type, err)
	}
	if ae.Seq != e.Seq || ae.Type != e.Type {
		tb.Errorf("ApplyError identifies event %d (%s), want %d (%s)", ae.Seq, ae.Type, e.Seq, e.Type)
	}
	if !errors.Is(err, sentinel) {
		tb.Errorf("Apply(%s): %v\n does not match %v", e.Type, err, sentinel)
	}
	if errors.Is(err, other) {
		tb.Errorf("Apply(%s): %v\n matches %v as well; the classes must be disjoint", e.Type, err, other)
	}
	if after := canon(tb, s); !bytes.Equal(before, after) {
		tb.Errorf("Apply(%s) modified its argument:\nbefore %s\nafter  %s", e.Type, before, after)
	}
	if after := canon(tb, got); !bytes.Equal(before, after) {
		tb.Errorf("Apply(%s) returned a state that differs from the input:\nbefore %s\nafter  %s", e.Type, before, after)
	}
	return err
}

// ---------------------------------------------------------------------------
// A scripted Run
// ---------------------------------------------------------------------------

// run drives a State through legal events, remembers them, and numbers
// Attempts and requests. Every transition it performs goes through Apply, so
// the fixtures are reachable states rather than hand-built ones.
type run struct {
	tb       testing.TB
	state    State
	events   []domain.Event
	attempts int
	requests int
}

func newRun(tb testing.TB) *run { return &run{tb: tb} }

// do applies p as the next event and records it.
func (r *run) do(p domain.Payload) *run {
	r.tb.Helper()
	e := nextEvent(r.state, p)
	r.state = apply(r.tb, r.state, p)
	r.events = append(r.events, e)
	return r
}

// reject checks that p is rejected at this point, with the sentinel, and
// leaves the run as it is.
func (r *run) reject(p domain.Payload, sentinel error) error {
	r.tb.Helper()
	return mustReject(r.tb, r.state, p, sentinel)
}

func (r *run) newAttempt() domain.AttemptID {
	r.attempts++
	return attemptID(r.attempts)
}

func (r *run) newRequest() domain.RequestID {
	r.requests++
	return requestID(r.requests)
}

// lastAttempt returns the ID of the Attempt scheduled most recently.
func (r *run) lastAttempt() domain.AttemptID {
	if r.attempts == 0 {
		r.tb.Fatal("no Attempt has been scheduled")
	}
	return attemptID(r.attempts)
}

func (r *run) task() domain.ArtifactRef { return ref(r.tb, domain.TaskPath) }

// created brings an empty run to Created: RunCreated and ManifestFrozen.
func (r *run) created() *run {
	r.tb.Helper()
	r.do(domain.RunCreated{Task: r.task()})
	return r.do(domain.ManifestFrozen{
		Manifest: ref(r.tb, domain.ManifestPath),
		Policy:   ref(r.tb, domain.PolicyPath),
		Limits:   domain.Limits{MaxPhaseRetries: 2, MaxReplans: 1, Timeout: "1h30m0s", MaxAgentSteps: 50, MaxTurnCostUSD: 1.5},
	})
}

// schedule schedules an Attempt with the payload and returns its ID.
func (r *run) schedule(role domain.Role, revision int, phase domain.PhaseID) domain.AttemptID {
	r.tb.Helper()
	id := r.newAttempt()
	r.do(domain.AttemptScheduled{AttemptID: id, Role: role, Revision: revision, PhaseID: phase, SessionID: session1})
	return id
}

// finish takes a Scheduled Attempt through AttemptStarted and AttemptFinished
// and returns the output reference.
func (r *run) finish(id domain.AttemptID) domain.ArtifactRef {
	r.tb.Helper()
	r.do(domain.AttemptStarted{AttemptID: id})
	out := ref(r.tb, domain.AttemptOutputPath(id, 1))
	r.do(domain.AttemptFinished{AttemptID: id, Output: out})
	return out
}

// fail takes a Scheduled Attempt through AttemptStarted and AttemptFailed.
func (r *run) fail(id domain.AttemptID) {
	r.tb.Helper()
	r.do(domain.AttemptStarted{AttemptID: id})
	r.do(domain.AttemptFailed{AttemptID: id, Outcome: domain.OutcomeFailed, ErrorClass: domain.ErrorClassTransientError, Detail: "the host exited"})
}

// plannerFinished schedules a planner Attempt and finishes it; it returns the
// output, which is the plan candidate.
func (r *run) plannerFinished() (domain.AttemptID, domain.ArtifactRef) {
	r.tb.Helper()
	id := r.schedule(domain.RolePlanner, 0, "")
	return id, r.finish(id)
}

// planCandidate takes a Planning run to PlanValidation.
func (r *run) planCandidate() domain.ArtifactRef {
	r.tb.Helper()
	id, out := r.plannerFinished()
	r.do(domain.PlanGenerated{AttemptID: id, Artifact: out})
	return out
}

// planDefs is the Phase graph of the fixtures: phase-02 depends on phase-01.
func planDefs() []domain.PhaseDefinition {
	return []domain.PhaseDefinition{
		{ID: phase1, DependsOn: []domain.PhaseID{}},
		{ID: phase2, DependsOn: []domain.PhaseID{phase1}},
	}
}

// accept applies PlanAccepted for the next revision with the fixture graph.
func (r *run) accept() *run {
	r.tb.Helper()
	n := r.state.Run.PlanRevision + 1
	return r.do(domain.PlanAccepted{Revision: n, Plan: ref(r.tb, domain.PlanPath(n)), Phases: planDefs()})
}

// reachRun returns a run that is in the given Run state, which may be any
// nonterminal state, along the shortest legal path. A suspended state is
// entered from Executing.
func reachRun(tb testing.TB, target domain.RunState) *run {
	tb.Helper()
	r := newRun(tb).created()
	switch target {
	case domain.RunStateCreated:
	case domain.RunStateStrategySelection:
		r.do(domain.RunPrepared{})
	case domain.RunStatePlanning:
		r.do(domain.RunPrepared{}).do(domain.StrategySelected{})
	case domain.RunStatePlanValidation:
		r.do(domain.RunPrepared{}).do(domain.StrategySelected{})
		r.planCandidate()
	case domain.RunStateWaitingApproval:
		r.do(domain.RunPrepared{}).do(domain.StrategySelected{})
		out := r.planCandidate()
		r.do(domain.PlanApprovalRequested{Artifact: out})
	case domain.RunStateExecuting:
		r.toExecuting()
	case domain.RunStateFinalVerification:
		r.toExecuting()
		r.do(domain.FinalVerificationStarted{})
	case domain.RunStateFinalEvaluation:
		r.toExecuting()
		r.do(domain.FinalVerificationStarted{}).do(domain.FinalEvaluationStarted{})
	case domain.RunStateWaitingForUser, domain.RunStateBlocked, domain.RunStateRecoveryRequired:
		r.toExecuting()
		r.suspend(target)
	default:
		tb.Fatalf("reachRun: %s is not a nonterminal Run state", target)
	}
	return r
}

// toExecuting takes a Created run to Executing with revision 1: the scripted
// prefix of the tests.
func (r *run) toExecuting() *run {
	r.tb.Helper()
	r.do(domain.RunPrepared{}).do(domain.StrategySelected{})
	r.planCandidate()
	return r.accept()
}

// suspendPayload returns the RunSuspended payload that moves the run from its
// current state to the suspended state.
func (r *run) suspendPayload(to domain.RunState) domain.RunSuspended {
	p := domain.RunSuspended{To: to, ResumeState: r.state.Run.State}
	switch to {
	case domain.RunStateWaitingForUser:
		p.Reason = domain.SuspendReasonUserInput
		p.RequestID = r.newRequest()
	case domain.RunStateBlocked:
		p.Reason = domain.SuspendReasonLimitReached
	case domain.RunStateRecoveryRequired:
		p.Reason = domain.SuspendReasonUnknownOutcome
	default:
		r.tb.Fatalf("%s is not a suspended Run state", to)
	}
	return p
}

// suspend suspends the run into the given state.
func (r *run) suspend(to domain.RunState) *run {
	r.tb.Helper()
	return r.do(r.suspendPayload(to))
}

// reachSuspended returns a run that is in the suspended state with the given
// recorded resume state. When the resume state is itself suspended, the
// suspensions are nested.
func reachSuspended(tb testing.TB, state, resume domain.RunState) *run {
	tb.Helper()
	r := reachRun(tb, resume)
	r.suspend(state)
	return r
}

// ---------------------------------------------------------------------------
// Phase fixtures
// ---------------------------------------------------------------------------

// implement schedules an implementer Attempt for the Phase of revision 1.
func (r *run) implement(phase domain.PhaseID) domain.AttemptID {
	r.tb.Helper()
	return r.schedule(domain.RoleImplementer, 1, phase)
}

// reachPhase returns a run in Executing, revision 1, in which phase-01 is in
// the given Phase state. phase-02 stays Pending. The implementer Attempt of
// phase-01 is the run's first Attempt after the planner's.
func reachPhase(tb testing.TB, state domain.PhaseState) *run {
	tb.Helper()
	r := reachRun(tb, domain.RunStateExecuting)
	p := func(payload domain.Payload) { r.do(payload) }
	switch state {
	case domain.PhaseStatePending:
	case domain.PhaseStateReady:
		p(domain.PhaseReady{Revision: 1, PhaseID: phase1})
	case domain.PhaseStateRunning:
		p(domain.PhaseReady{Revision: 1, PhaseID: phase1})
		r.implement(phase1)
	case domain.PhaseStateVerifying, domain.PhaseStateEvaluating, domain.PhaseStateIntegrationPending,
		domain.PhaseStateIntegrating, domain.PhaseStateAccepted:
		r.toVerifying()
		switch state {
		case domain.PhaseStateEvaluating:
			p(domain.PhaseEvaluationStarted{Revision: 1, PhaseID: phase1})
		case domain.PhaseStateIntegrationPending, domain.PhaseStateIntegrating, domain.PhaseStateAccepted:
			p(domain.PhaseIntegrationQueued{Revision: 1, PhaseID: phase1})
			if state != domain.PhaseStateIntegrationPending {
				p(domain.PhaseIntegrationStarted{Revision: 1, PhaseID: phase1})
			}
			if state == domain.PhaseStateAccepted {
				p(domain.PhaseAccepted{Revision: 1, PhaseID: phase1, AttemptID: r.lastAttempt()})
			}
		}
	case domain.PhaseStateRecovering, domain.PhaseStateFailed:
		p(domain.PhaseReady{Revision: 1, PhaseID: phase1})
		id := r.implement(phase1)
		r.fail(id)
		p(domain.PhaseRecovering{Revision: 1, PhaseID: phase1, Cause: domain.PhaseRecoveryCauseAttemptFailed})
		if state == domain.PhaseStateFailed {
			p(domain.PhaseFailed{Revision: 1, PhaseID: phase1})
		}
	case domain.PhaseStateSuperseded:
		p(domain.PhaseSuperseded{Revision: 1, PhaseID: phase1})
	case domain.PhaseStateWaitingForUser:
		p(domain.PhaseReady{Revision: 1, PhaseID: phase1})
		r.implement(phase1)
		p(domain.PhaseSuspended{Revision: 1, PhaseID: phase1, To: domain.PhaseStateWaitingForUser,
			ResumeState: domain.PhaseStateRunning, RequestID: r.newRequest()})
	case domain.PhaseStateRecoveryRequired:
		p(domain.PhaseReady{Revision: 1, PhaseID: phase1})
		r.implement(phase1)
		p(domain.PhaseSuspended{Revision: 1, PhaseID: phase1, To: domain.PhaseStateRecoveryRequired, ResumeState: domain.PhaseStateRunning})
	default:
		tb.Fatalf("reachPhase: unknown Phase state %s", state)
	}
	return r
}

// toVerifying takes phase-01 from Pending to Verifying with a finished
// implementer Attempt.
func (r *run) toVerifying() *run {
	r.tb.Helper()
	r.do(domain.PhaseReady{Revision: 1, PhaseID: phase1})
	id := r.implement(phase1)
	r.finish(id)
	return r.do(domain.PhaseVerificationStarted{Revision: 1, PhaseID: phase1, AttemptID: id})
}

// phaseOf returns phase-01 or fails the test.
func phaseOf(tb testing.TB, s State, id domain.PhaseID) Phase {
	tb.Helper()
	ph, ok := s.Phase(1, id)
	if !ok {
		tb.Fatalf("no phase %s in revision 1", id)
	}
	return ph
}

// attemptOf returns the Attempt or fails the test.
func attemptOf(tb testing.TB, s State, id domain.AttemptID) Attempt {
	tb.Helper()
	a, ok := s.Attempt(id)
	if !ok {
		tb.Fatalf("no attempt %s", id)
	}
	return a
}

// ---------------------------------------------------------------------------
// Attempt fixtures
// ---------------------------------------------------------------------------

// reachAttempt returns a run in Planning that has one planner Attempt in the
// given state (the Attempt returned by lastAttempt).
func reachAttempt(tb testing.TB, state domain.AttemptState) *run {
	tb.Helper()
	r := reachRun(tb, domain.RunStatePlanning)
	id := r.schedule(domain.RolePlanner, 0, "")
	switch state {
	case domain.AttemptStateScheduled:
	case domain.AttemptStateRunning:
		r.do(domain.AttemptStarted{AttemptID: id})
	case domain.AttemptStateFinished:
		r.finish(id)
	case domain.AttemptStateFailed:
		r.fail(id)
	case domain.AttemptStateTimedOut:
		r.do(domain.AttemptStarted{AttemptID: id}).do(domain.AttemptTimedOut{AttemptID: id})
	case domain.AttemptStateCancelled:
		r.do(domain.AttemptStarted{AttemptID: id}).do(domain.AttemptCancelled{AttemptID: id})
	case domain.AttemptStateWaitingForUser:
		r.do(domain.AttemptStarted{AttemptID: id}).do(domain.AttemptAwaitingInput{AttemptID: id, RequestID: r.newRequest()})
	case domain.AttemptStateAbandoned:
		r.do(domain.AttemptAbandoned{AttemptID: id, Reason: domain.AbandonReasonLaunchNotOccurred})
	case domain.AttemptStateUnknown:
		r.do(domain.AttemptStarted{AttemptID: id}).do(domain.AttemptOutcomeUnknown{AttemptID: id})
	default:
		tb.Fatalf("reachAttempt: unknown Attempt state %s", state)
	}
	return r
}

// recordedResume returns the resume state of the top suspension, or "" when
// the Run is not suspended.
func (r *run) recordedResume() domain.RunState {
	if n := len(r.state.Run.Suspensions); n > 0 {
		return r.state.Run.Suspensions[n-1].ResumeState
	}
	return ""
}
