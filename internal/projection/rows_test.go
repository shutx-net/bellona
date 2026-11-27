package projection

import (
	"fmt"
	"slices"
	"testing"

	"github.com/shutx-net/bellona/internal/domain"
	"github.com/shutx-net/bellona/internal/fsm"
	"github.com/shutx-net/bellona/internal/strictjson"
)

// The tests of this file (T-01, R-FSM-01, R-FSM-02) walk every edge that the
// transition tables yield
// (fsm.Edges) and apply the event that commits it to a state in the source
// state of the edge. The state is reached through legal events, and the payload
// is built for the row. Edges that two rows share (run-02 and run-22 from
// Created to Blocked, phase-09 and phase-11 from Evaluating to Recovering, and
// so on) are told apart by the reason or cause in the payload.

// edgeLabel names an edge, and a resume state for the edges whose target is the
// recorded one, for the names of subtests.
func edgeLabel(e fsm.Edge, resume string) string {
	if e.ToResume {
		return fmt.Sprintf("%s/%s-to-resume-%s", e.Row, e.From, resume)
	}
	return fmt.Sprintf("%s/%s-to-%s", e.Row, e.From, e.To)
}

// edgeKey identifies an edge for the coverage count.
func edgeKey(e fsm.Edge) string {
	return fmt.Sprintf("%s %s %s %t %s", e.Row, e.From, e.To, e.ToResume, e.Event)
}

// resumeChoices returns the states that can be recorded as the resume state of
// a machine instance that is in e.From: for an edge with a resume target, every
// source state of an edge of the suspending event that leads to e.From (taken
// from the tables, not from the reducer); for any other edge a single empty
// choice.
func resumeChoices(m fsm.Machine, e fsm.Edge, suspend domain.EventType) []string {
	if !e.ToResume {
		return []string{""}
	}
	var out []string
	for _, s := range fsm.Edges(m) {
		if s.Event == suspend && s.To == e.From && !slices.Contains(out, s.From) {
			out = append(out, s.From)
		}
	}
	return out
}

// requireCovered fails the test unless every edge of the machine was applied
// successfully (for a resume edge, with at least one resume state).
func requireCovered(t *testing.T, m fsm.Machine, covered map[string]bool) {
	t.Helper()
	edges := fsm.Edges(m)
	if len(edges) == 0 {
		t.Fatalf("the %s machine has no edges", m)
	}
	var missing []string
	for _, e := range edges {
		if !covered[edgeKey(e)] {
			missing = append(missing, edgeLabel(e, "?"))
		}
	}
	if len(missing) > 0 {
		t.Errorf("%d of %d edges of the %s machine were not applied: %v", len(missing), len(edges), m, missing)
	}
	t.Logf("%s machine: %d edges, all applied through their committing events", m, len(edges))
}

// encoded returns the canonical encoding of a part of a state.
func encoded(tb testing.TB, v any) string {
	tb.Helper()
	b, err := strictjson.Encode(v)
	if err != nil {
		tb.Fatalf("encode %T: %v", v, err)
	}
	return string(b)
}

// requireSame fails the test unless the two values have the same encoding.
func requireSame(tb testing.TB, what string, before, after any) {
	tb.Helper()
	if b, a := encoded(tb, before), encoded(tb, after); a != b {
		tb.Errorf("%s changed:\nbefore %s\nafter  %s", what, b, a)
	}
}

// requireRefs checks that every artifact reference of the payload is in the
// state exactly once.
func requireRefs(t *testing.T, after State, p domain.Payload) {
	t.Helper()
	for _, r := range p.ArtifactRefs() {
		if n := countRef(after.Artifacts, r); n != 1 {
			t.Errorf("artifact %s is registered %d times, want once", r.Path, n)
		}
	}
}

func countRef(refs []domain.ArtifactRef, r domain.ArtifactRef) int {
	n := 0
	for _, x := range refs {
		if x == r {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// Run table
// ---------------------------------------------------------------------------

func TestRowsRunEdges(t *testing.T) {
	covered := make(map[string]bool)
	for _, edge := range fsm.Edges(fsm.MachineRun) {
		for _, resume := range resumeChoices(fsm.MachineRun, edge, domain.EventRunSuspended) {
			t.Run(edgeLabel(edge, resume), func(t *testing.T) {
				exerciseRunEdge(t, edge, resume)
				covered[edgeKey(edge)] = true
			})
		}
	}
	requireCovered(t, fsm.MachineRun, covered)
}

// runEdgePrefix builds the state and the payload for an edge of the Run table.
func runEdgePrefix(t *testing.T, edge fsm.Edge, resume string) (*run, domain.Payload) {
	t.Helper()
	from := domain.RunState(edge.From)
	to := domain.RunState(edge.To)
	switch edge.Event {
	case domain.EventRunPrepared:
		return reachRun(t, from), domain.RunPrepared{}
	case domain.EventStrategySelected:
		return reachRun(t, from), domain.StrategySelected{Decision: ref(t, "attempts/"+string(attemptID(99))+"/turns/0001/output.json")}
	case domain.EventPlanGenerated:
		r := reachRun(t, from)
		id, out := r.plannerFinished()
		return r, domain.PlanGenerated{AttemptID: id, Artifact: out}
	case domain.EventPlanValidationFailed:
		return reachRun(t, from), domain.PlanValidationFailed{Summary: "phase-02 depends on an unknown phase", Report: ref(t, "plans/rev-0001/validation.json")}
	case domain.EventPlanApprovalRequested:
		r := reachRun(t, from)
		return r, domain.PlanApprovalRequested{Artifact: r.state.Run.PendingPlan}
	case domain.EventPlanAccepted:
		r := reachRun(t, from)
		return r, domain.PlanAccepted{Revision: 1, Plan: ref(t, domain.PlanPath(1)), Phases: planDefs()}
	case domain.EventPlanChangesRequested:
		return reachRun(t, from), domain.PlanChangesRequested{Feedback: ref(t, "plans/rev-0001/feedback.json")}
	case domain.EventFinalVerificationStarted:
		return reachRun(t, from), domain.FinalVerificationStarted{}
	case domain.EventReplanningStarted:
		cause := map[string]domain.ReplanCause{
			"run-12": domain.ReplanCauseReplanAuthorized,
			"run-16": domain.ReplanCauseFinalVerificationFailed,
			"run-18": domain.ReplanCauseFinalEvaluationRejected,
		}[edge.Row]
		return reachRun(t, from), domain.ReplanningStarted{Cause: cause}
	case domain.EventFinalEvaluationStarted:
		return reachRun(t, from), domain.FinalEvaluationStarted{}
	case domain.EventRunCompleted:
		return reachRun(t, from), domain.RunCompleted{}
	case domain.EventFinalEvaluationRetried:
		r := reachRun(t, from)
		id := r.schedule(domain.RoleEvaluator, 0, "")
		r.fail(id)
		return r, domain.FinalEvaluationRetried{FailedAttemptID: id}
	case domain.EventRunSuspended:
		r := reachRun(t, from)
		p := r.suspendPayload(to)
		p.Reason = map[string]domain.SuspendReason{
			"run-02": domain.SuspendReasonBaselineFailed,
			"run-13": domain.SuspendReasonNoProgress,
			"run-20": domain.SuspendReasonUserInput,
			"run-22": domain.SuspendReasonLimitReached,
			"run-24": domain.SuspendReasonUnknownOutcome,
		}[edge.Row]
		return r, p
	case domain.EventRunResumed:
		r := reachSuspended(t, from, domain.RunState(resume))
		return r, domain.RunResumed{To: domain.RunState(resume)}
	case domain.EventRunCancelled:
		reason := domain.CancelReasonUserRequest
		if edge.Row == "run-10" {
			reason = domain.CancelReasonPlanRejected
		}
		return reachRun(t, from), domain.RunCancelled{Reason: reason}
	}
	t.Fatalf("no payload for the event %s of row %s", edge.Event, edge.Row)
	return nil, nil
}

func exerciseRunEdge(t *testing.T, edge fsm.Edge, resume string) {
	t.Helper()
	r, p := runEdgePrefix(t, edge, resume)
	before := r.state.Clone()
	if got := string(before.Run.State); got != edge.From {
		t.Fatalf("the fixture is in %s, want %s", got, edge.From)
	}
	after := apply(t, r.state, p)

	want := edge.To
	if edge.ToResume {
		want = resume
	}
	if got := string(after.Run.State); got != want {
		t.Fatalf("the Run is in %s after %s, want %s", got, edge.Event, want)
	}
	requireRefs(t, after, p)

	// Nothing but the Run changes, except where the event creates entities.
	requireSame(t, "attempts", before.Attempts, after.Attempts)
	if edge.Event != domain.EventPlanAccepted {
		requireSame(t, "revisions", before.Revisions, after.Revisions)
	}
	requireSame(t, "limits", before.Limits, after.Limits)
	requireSame(t, "amendments", before.Amendments, after.Amendments)

	ev := nextEvent(before, p)
	switch p := p.(type) {
	case domain.RunSuspended:
		if len(after.Run.Suspensions) != len(before.Run.Suspensions)+1 {
			t.Fatalf("the suspension stack has %d entries, want %d", len(after.Run.Suspensions), len(before.Run.Suspensions)+1)
		}
		requireSame(t, "the lower suspensions", before.Run.Suspensions, after.Run.Suspensions[:len(before.Run.Suspensions)])
		top := after.Run.Suspensions[len(after.Run.Suspensions)-1]
		wantTop := Suspension{State: p.To, ResumeState: p.ResumeState, Reason: p.Reason, Detail: p.Detail,
			RequestID: p.RequestID, Seq: ev.Seq, Since: ev.Time}
		if top != wantTop {
			t.Errorf("pushed %+v, want %+v", top, wantTop)
		}
		if top.ResumeState != before.Run.State {
			t.Errorf("the resume state is %s, want the state %s that the Run left", top.ResumeState, before.Run.State)
		}
	case domain.RunResumed:
		n := len(before.Run.Suspensions)
		if len(after.Run.Suspensions) != n-1 {
			t.Fatalf("the suspension stack has %d entries, want %d", len(after.Run.Suspensions), n-1)
		}
		requireSame(t, "the remaining suspensions", before.Run.Suspensions[:n-1], after.Run.Suspensions)
		if got := before.Run.Suspensions[n-1].ResumeState; got != p.To {
			t.Errorf("popped a suspension that resumes to %s, the event resumes to %s", got, p.To)
		}
	case domain.RunCancelled:
		// Cancelling clears the stack; it is checked below.
	default:
		requireSame(t, "suspensions", before.Run.Suspensions, after.Run.Suspensions)
	}
	switch p := p.(type) {
	case domain.PlanGenerated:
		if after.Run.PendingPlan != p.Artifact {
			t.Errorf("PendingPlan is %v, want %v", after.Run.PendingPlan, p.Artifact)
		}
	case domain.PlanValidationFailed:
		if !after.Run.PendingPlan.IsZero() {
			t.Errorf("PendingPlan is %v, want none", after.Run.PendingPlan)
		}
	case domain.PlanChangesRequested:
		if !after.Run.PendingPlan.IsZero() || after.Run.ChangeRequestCount != before.Run.ChangeRequestCount+1 {
			t.Errorf("PendingPlan %v, ChangeRequestCount %d (was %d)", after.Run.PendingPlan, after.Run.ChangeRequestCount, before.Run.ChangeRequestCount)
		}
	case domain.PlanAccepted:
		if len(after.Revisions) != len(before.Revisions)+1 || after.Run.PlanRevision != 1 || !after.Run.PendingPlan.IsZero() {
			t.Fatalf("revisions %d (was %d), PlanRevision %d, PendingPlan %v", len(after.Revisions), len(before.Revisions), after.Run.PlanRevision, after.Run.PendingPlan)
		}
		rev, _ := after.CurrentRevision()
		if rev.Number != 1 || rev.Plan != p.Plan || !rev.AcceptedAt.Equal(ev.Time) || len(rev.Phases) != len(p.Phases) {
			t.Fatalf("revision %+v does not match %+v", rev, p)
		}
		for i, ph := range rev.Phases {
			def := p.Phases[i]
			if ph.ID != def.ID || !slices.Equal(ph.DependsOn, def.DependsOn) || ph.State != domain.PhaseStatePending || len(ph.Attempts) != 0 || !ph.UpdatedAt.Equal(ev.Time) {
				t.Errorf("phase %d is %+v, want %s pending with dependencies %v", i, ph, def.ID, def.DependsOn)
			}
		}
	case domain.ReplanningStarted:
		if after.Run.ReplanCount != before.Run.ReplanCount+1 {
			t.Errorf("ReplanCount is %d, want %d", after.Run.ReplanCount, before.Run.ReplanCount+1)
		}
	case domain.RunCancelled:
		if after.Run.CancelReason != p.Reason || len(after.Run.Suspensions) != 0 {
			t.Errorf("CancelReason %q, %d suspensions", after.Run.CancelReason, len(after.Run.Suspensions))
		}
	}
	if after.Run.CreatedAt != before.Run.CreatedAt {
		t.Errorf("CreatedAt changed")
	}
}

// ---------------------------------------------------------------------------
// Phase table
// ---------------------------------------------------------------------------

func TestRowsPhaseEdges(t *testing.T) {
	covered := make(map[string]bool)
	for _, edge := range fsm.Edges(fsm.MachinePhase) {
		for _, resume := range resumeChoices(fsm.MachinePhase, edge, domain.EventPhaseSuspended) {
			t.Run(edgeLabel(edge, resume), func(t *testing.T) {
				exercisePhaseEdge(t, edge, resume)
				covered[edgeKey(edge)] = true
			})
		}
	}
	requireCovered(t, fsm.MachinePhase, covered)
}

// phaseEdgePrefix builds the state and the payload for an edge of the Phase
// table. Every payload names phase-01 of revision 1.
func phaseEdgePrefix(t *testing.T, edge fsm.Edge, resume string) (*run, domain.Payload) {
	t.Helper()
	from := domain.PhaseState(edge.From)
	to := domain.PhaseState(edge.To)
	switch edge.Event {
	case domain.EventPhaseReady:
		return reachPhase(t, from), domain.PhaseReady{Revision: 1, PhaseID: phase1}
	case domain.EventAttemptScheduled:
		r := reachPhase(t, from)
		return r, domain.AttemptScheduled{AttemptID: r.newAttempt(), Role: domain.RoleImplementer, Revision: 1, PhaseID: phase1, SessionID: session1}
	case domain.EventPhaseVerificationStarted:
		r := reachPhase(t, from)
		id := r.lastAttempt()
		r.finish(id)
		return r, domain.PhaseVerificationStarted{Revision: 1, PhaseID: phase1, AttemptID: id}
	case domain.EventPhaseRecovering:
		cause := map[string]domain.PhaseRecoveryCause{
			"phase-04": domain.PhaseRecoveryCauseAttemptFailed,
			"phase-07": domain.PhaseRecoveryCauseScopeViolation,
			"phase-09": domain.PhaseRecoveryCauseRejected,
			"phase-11": domain.PhaseRecoveryCauseEvaluatorRetriesExhausted,
			"phase-14": domain.PhaseRecoveryCauseIntegrationConflict,
		}[edge.Row]
		return reachPhase(t, from), domain.PhaseRecovering{Revision: 1, PhaseID: phase1, Cause: cause}
	case domain.EventPhaseEvaluationStarted:
		return reachPhase(t, from), domain.PhaseEvaluationStarted{Revision: 1, PhaseID: phase1}
	case domain.EventPhaseIntegrationQueued:
		return reachPhase(t, from), domain.PhaseIntegrationQueued{Revision: 1, PhaseID: phase1}
	case domain.EventPhaseEvaluationRetried:
		r := reachPhase(t, from)
		id := r.schedule(domain.RoleEvaluator, 1, phase1)
		r.fail(id)
		return r, domain.PhaseEvaluationRetried{Revision: 1, PhaseID: phase1, FailedAttemptID: id}
	case domain.EventPhaseIntegrationStarted:
		return reachPhase(t, from), domain.PhaseIntegrationStarted{Revision: 1, PhaseID: phase1}
	case domain.EventPhaseAccepted:
		r := reachPhase(t, from)
		return r, domain.PhaseAccepted{Revision: 1, PhaseID: phase1, AttemptID: r.lastAttempt()}
	case domain.EventPhaseRetryAuthorized:
		return reachPhase(t, from), domain.PhaseRetryAuthorized{Revision: 1, PhaseID: phase1}
	case domain.EventPhaseFailed:
		return reachPhase(t, from), domain.PhaseFailed{Revision: 1, PhaseID: phase1}
	case domain.EventPhaseSuspended:
		r := reachPhase(t, from)
		p := domain.PhaseSuspended{Revision: 1, PhaseID: phase1, To: to, ResumeState: from}
		if to == domain.PhaseStateWaitingForUser {
			p.RequestID = r.newRequest()
		}
		return r, p
	case domain.EventPhaseResumed:
		r := reachPhase(t, domain.PhaseState(resume))
		r.do(domain.PhaseSuspended{Revision: 1, PhaseID: phase1, To: domain.PhaseStateWaitingForUser,
			ResumeState: domain.PhaseState(resume), RequestID: r.newRequest()})
		return r, domain.PhaseResumed{Revision: 1, PhaseID: phase1, To: domain.PhaseState(resume)}
	case domain.EventPhaseReconciled:
		return reachPhase(t, from), domain.PhaseReconciled{Revision: 1, PhaseID: phase1, To: to}
	case domain.EventPhaseSuperseded:
		return reachPhase(t, from), domain.PhaseSuperseded{Revision: 1, PhaseID: phase1}
	}
	t.Fatalf("no payload for the event %s of row %s", edge.Event, edge.Row)
	return nil, nil
}

func exercisePhaseEdge(t *testing.T, edge fsm.Edge, resume string) {
	t.Helper()
	r, p := phaseEdgePrefix(t, edge, resume)
	before := r.state.Clone()
	if got := phaseOf(t, before, phase1).State; string(got) != edge.From {
		t.Fatalf("the fixture's Phase is in %s, want %s", got, edge.From)
	}
	after := apply(t, r.state, p)
	ev := nextEvent(before, p)

	want := edge.To
	if edge.ToResume {
		want = resume
	}
	ph := phaseOf(t, after, phase1)
	old := phaseOf(t, before, phase1)
	if string(ph.State) != want {
		t.Fatalf("the Phase is in %s after %s, want %s", ph.State, edge.Event, want)
	}
	if !ph.UpdatedAt.Equal(ev.Time) {
		t.Errorf("Phase.UpdatedAt is %s, want the event time %s", ph.UpdatedAt, ev.Time)
	}
	requireRefs(t, after, p)

	// The other Phase, the Run and the revisions' bookkeeping do not change.
	requireSame(t, "phase-02", phaseOf(t, before, phase2), phaseOf(t, after, phase2))
	requireSame(t, "the Run", before.Run, withUpdatedAt(after.Run, before.Run))
	if len(after.Revisions) != len(before.Revisions) {
		t.Errorf("the number of revisions changed")
	}

	// The resume state is recorded only while the Phase waits for the user.
	switch {
	case ph.State == domain.PhaseStateWaitingForUser:
		if ph.ResumeState != old.State {
			t.Errorf("ResumeState is %q, want %s", ph.ResumeState, old.State)
		}
	case ph.ResumeState != "":
		t.Errorf("ResumeState is %q in %s, want none", ph.ResumeState, ph.State)
	}

	switch p := p.(type) {
	case domain.AttemptScheduled:
		if want := append(slices.Clone(old.Attempts), p.AttemptID); !slices.Equal(ph.Attempts, want) {
			t.Errorf("Phase.Attempts is %v, want %v", ph.Attempts, want)
		}
		a := attemptOf(t, after, p.AttemptID)
		wantAttempt := Attempt{ID: p.AttemptID, Role: domain.RoleImplementer, Revision: 1, PhaseID: phase1, SessionID: session1,
			State: domain.AttemptStateScheduled, ScheduledAt: ev.Time, UpdatedAt: ev.Time}
		if a != wantAttempt {
			t.Errorf("created %+v, want %+v", a, wantAttempt)
		}
		if len(after.Attempts) != len(before.Attempts)+1 {
			t.Errorf("%d Attempts, want %d", len(after.Attempts), len(before.Attempts)+1)
		}
	case domain.PhaseRecovering:
		if ph.RecoveryCause != p.Cause {
			t.Errorf("RecoveryCause is %q, want %q", ph.RecoveryCause, p.Cause)
		}
		requireSame(t, "attempts", before.Attempts, after.Attempts)
	default:
		requireSame(t, "attempts", before.Attempts, after.Attempts)
	}
	if _, ok := p.(domain.AttemptScheduled); !ok {
		if !slices.Equal(ph.Attempts, old.Attempts) {
			t.Errorf("Phase.Attempts changed from %v to %v", old.Attempts, ph.Attempts)
		}
	}
}

// withUpdatedAt returns after with the UpdatedAt of before, so that the Run of
// a Phase event can be compared apart from the time it was touched.
func withUpdatedAt(after, before Run) Run {
	after.UpdatedAt = before.UpdatedAt
	return after
}

// A reconciliation never determines Failed: a Phase reaches Failed only from
// Recovering.
func TestRowsPhaseReconciledNeverFailed(t *testing.T) {
	r := reachPhase(t, domain.PhaseStateRecoveryRequired)
	err := r.reject(domain.PhaseReconciled{Revision: 1, PhaseID: phase1, To: domain.PhaseStateFailed}, fsm.ErrIllegalTransition)
	if err == nil {
		t.Fatal("no error")
	}
	for _, to := range []domain.PhaseState{domain.PhaseStatePending, domain.PhaseStateSuperseded, domain.PhaseStateWaitingForUser, domain.PhaseStateRecoveryRequired} {
		r.reject(domain.PhaseReconciled{Revision: 1, PhaseID: phase1, To: to}, fsm.ErrIllegalTransition)
	}
	// Every state of the reconciliation set is accepted.
	for _, to := range fsm.ReconciliationTargets() {
		got := apply(t, r.state, domain.PhaseReconciled{Revision: 1, PhaseID: phase1, To: to})
		if s := phaseOf(t, got, phase1).State; s != to {
			t.Errorf("reconciled to %s, the Phase is in %s", to, s)
		}
	}
}

// ---------------------------------------------------------------------------
// Attempt table
// ---------------------------------------------------------------------------

func TestRowsAttemptEdges(t *testing.T) {
	covered := make(map[string]bool)
	for _, edge := range fsm.Edges(fsm.MachineAttempt) {
		t.Run(edgeLabel(edge, ""), func(t *testing.T) {
			exerciseAttemptEdge(t, edge)
			covered[edgeKey(edge)] = true
		})
	}
	requireCovered(t, fsm.MachineAttempt, covered)
}

// attemptEdgePayload returns the payload for an edge of the Attempt table that
// applies to the Attempt id of the run.
func attemptEdgePayload(t *testing.T, r *run, edge fsm.Edge) domain.Payload {
	t.Helper()
	id := r.lastAttempt()
	to := domain.AttemptState(edge.To)
	switch edge.Event {
	case domain.EventAttemptStarted:
		return domain.AttemptStarted{AttemptID: id, SessionID: session1}
	case domain.EventAttemptAbandoned:
		reason := domain.AbandonReasonLaunchNotOccurred
		if edge.Row == "attempt-09" {
			reason = domain.AbandonReasonSessionUnavailable
		}
		return domain.AttemptAbandoned{AttemptID: id, Reason: reason}
	case domain.EventAttemptFinished:
		return domain.AttemptFinished{AttemptID: id, Output: ref(t, domain.AttemptOutputPath(id, 1))}
	case domain.EventAttemptFailed:
		return domain.AttemptFailed{AttemptID: id, Outcome: domain.OutcomeOutputInvalid, ErrorClass: domain.ErrorClassInvalidAgentOutput, Detail: "the output is not valid"}
	case domain.EventAttemptTimedOut:
		return domain.AttemptTimedOut{AttemptID: id}
	case domain.EventAttemptCancelled:
		return domain.AttemptCancelled{AttemptID: id}
	case domain.EventAttemptAwaitingInput:
		return domain.AttemptAwaitingInput{AttemptID: id, RequestID: r.newRequest()}
	case domain.EventAttemptResumed:
		return domain.AttemptResumed{AttemptID: id}
	case domain.EventAttemptOutcomeUnknown:
		return domain.AttemptOutcomeUnknown{AttemptID: id}
	case domain.EventAttemptReconciled:
		p := domain.AttemptReconciled{AttemptID: id, To: to}
		if to == domain.AttemptStateFinished {
			p.Output = ref(t, domain.AttemptOutputPath(id, 2))
		}
		return p
	}
	t.Fatalf("no payload for the event %s of row %s", edge.Event, edge.Row)
	return nil
}

func exerciseAttemptEdge(t *testing.T, edge fsm.Edge) {
	t.Helper()
	r := reachAttempt(t, domain.AttemptState(edge.From))
	p := attemptEdgePayload(t, r, edge)
	before := r.state.Clone()
	id := r.lastAttempt()
	old := attemptOf(t, before, id)
	if string(old.State) != edge.From {
		t.Fatalf("the fixture's Attempt is in %s, want %s", old.State, edge.From)
	}
	after := apply(t, r.state, p)
	ev := nextEvent(before, p)
	a := attemptOf(t, after, id)
	if string(a.State) != edge.To {
		t.Fatalf("the Attempt is in %s after %s, want %s", a.State, edge.Event, edge.To)
	}
	requireRefs(t, after, p)
	if !a.UpdatedAt.Equal(ev.Time) {
		t.Errorf("Attempt.UpdatedAt is %s, want %s", a.UpdatedAt, ev.Time)
	}
	if a.ScheduledAt != old.ScheduledAt || a.ID != old.ID || a.Role != old.Role {
		t.Errorf("identity fields changed: %+v to %+v", old, a)
	}

	// Every other Attempt, the Phases and the Run's state stay as they are.
	if len(after.Attempts) != len(before.Attempts) {
		t.Errorf("the number of Attempts changed")
	}
	for _, other := range before.Attempts {
		if other.ID != id {
			got := attemptOf(t, after, other.ID)
			requireSame(t, "attempt "+string(other.ID), other, got)
		}
	}
	requireSame(t, "revisions", before.Revisions, after.Revisions)
	if after.Run.State != before.Run.State {
		t.Errorf("the Run state changed from %s to %s", before.Run.State, after.Run.State)
	}

	switch p := p.(type) {
	case domain.AttemptStarted:
		if a.SessionID != session1 || !a.StartedAt.Equal(ev.Time) || !a.EndedAt.IsZero() {
			t.Errorf("SessionID %q, StartedAt %s, EndedAt %s", a.SessionID, a.StartedAt, a.EndedAt)
		}
	case domain.AttemptAbandoned:
		if a.AbandonReason != p.Reason || !a.EndedAt.Equal(ev.Time) {
			t.Errorf("AbandonReason %q, EndedAt %s", a.AbandonReason, a.EndedAt)
		}
	case domain.AttemptFinished:
		if a.Output != p.Output || !a.EndedAt.Equal(ev.Time) {
			t.Errorf("Output %v, EndedAt %s", a.Output, a.EndedAt)
		}
	case domain.AttemptFailed:
		if a.Outcome != p.Outcome || a.ErrorClass != p.ErrorClass || a.Detail != p.Detail || !a.EndedAt.Equal(ev.Time) {
			t.Errorf("Outcome %q, ErrorClass %q, Detail %q, EndedAt %s", a.Outcome, a.ErrorClass, a.Detail, a.EndedAt)
		}
	case domain.AttemptTimedOut, domain.AttemptCancelled:
		if !a.EndedAt.Equal(ev.Time) {
			t.Errorf("EndedAt %s", a.EndedAt)
		}
	case domain.AttemptAwaitingInput:
		if a.RequestID != p.RequestID {
			t.Errorf("RequestID %q, want %q", a.RequestID, p.RequestID)
		}
	case domain.AttemptResumed:
		if a.StartedAt != old.StartedAt || !a.EndedAt.IsZero() {
			t.Errorf("StartedAt %s (was %s), EndedAt %s", a.StartedAt, old.StartedAt, a.EndedAt)
		}
	case domain.AttemptOutcomeUnknown:
		if a.StartedAt != old.StartedAt || !a.EndedAt.IsZero() {
			t.Errorf("StartedAt %s (was %s), EndedAt %s", a.StartedAt, old.StartedAt, a.EndedAt)
		}
	case domain.AttemptReconciled:
		if a.Output != p.Output || !a.EndedAt.Equal(ev.Time) {
			t.Errorf("Output %v, EndedAt %s", a.Output, a.EndedAt)
		}
	}
}

// ---------------------------------------------------------------------------
// An oracle built from the tables alone
// ---------------------------------------------------------------------------

// oracle answers whether the tables allow a transition. It is built from
// fsm.Edges and shares no code with the check functions that Apply calls, so a
// test that compares the two does not argue in a circle.
type oracle struct {
	fixed  map[oracleKey]bool // edges with a listed target
	resume map[oracleKey]bool // edges whose target is the recorded resume state
	// events are the event types that commit a transition of each machine.
	events map[fsm.Machine]map[domain.EventType]bool
	// from lists, per machine and state, the event types that have an edge
	// from the state, in the order of the tables.
	from map[fsm.Machine]map[string][]domain.EventType
}

type oracleKey struct {
	machine  fsm.Machine
	event    domain.EventType
	from, to string // to is empty for a resume edge
}

func newOracle() *oracle {
	o := &oracle{
		fixed:  make(map[oracleKey]bool),
		resume: make(map[oracleKey]bool),
		events: make(map[fsm.Machine]map[domain.EventType]bool),
		from:   make(map[fsm.Machine]map[string][]domain.EventType),
	}
	for _, m := range fsm.Machines() {
		o.events[m] = make(map[domain.EventType]bool)
		o.from[m] = make(map[string][]domain.EventType)
		for _, e := range fsm.Edges(m) {
			k := oracleKey{machine: m, event: e.Event, from: e.From, to: e.To}
			if e.ToResume {
				o.resume[k] = true
			} else {
				o.fixed[k] = true
			}
			o.events[m][e.Event] = true
			if !slices.Contains(o.from[m][e.From], e.Event) {
				o.from[m][e.From] = append(o.from[m][e.From], e.Event)
			}
		}
	}
	return o
}

// allowed reports whether an event of machine m may move an instance from one
// state to another. For a resume edge the target must be the recorded resume
// state, which is never empty.
func (o *oracle) allowed(m fsm.Machine, ev domain.EventType, from, to, resume string) bool {
	if o.fixed[oracleKey{machine: m, event: ev, from: from, to: to}] {
		return true
	}
	return resume != "" && to == resume && o.resume[oracleKey{machine: m, event: ev, from: from}]
}

// uniqueTarget returns the one target state that all edges of the event have in
// machine m. It is meant for the events whose payload does not name a target;
// it fails the test when the event has several targets or a resume target.
func (o *oracle) uniqueTarget(tb testing.TB, m fsm.Machine, ev domain.EventType) string {
	tb.Helper()
	target := ""
	for k := range o.fixed {
		if k.machine != m || k.event != ev {
			continue
		}
		if target != "" && k.to != target {
			tb.Fatalf("the event %s has the targets %s and %s", ev, target, k.to)
		}
		target = k.to
	}
	if target == "" {
		tb.Fatalf("the event %s has no listed target in the %s machine", ev, m)
	}
	return target
}

// A transition that the tables do not list is rejected by the reducer with
// ErrIllegalTransition, in every state, for every event of the tables and for
// every target that a payload can name (T-01, R-FSM-02). The expectation comes
// from the oracle. The combinations that the tables allow are applied by the
// edge tests above.
func TestRowsUnlistedTransitionsAreRejected(t *testing.T) {
	o := newOracle()
	rejected := 0

	t.Run("run", func(t *testing.T) {
		for _, from := range []domain.RunState{
			domain.RunStateCreated, domain.RunStateStrategySelection, domain.RunStatePlanning, domain.RunStatePlanValidation,
			domain.RunStateWaitingApproval, domain.RunStateExecuting, domain.RunStateFinalVerification, domain.RunStateFinalEvaluation,
			domain.RunStateWaitingForUser, domain.RunStateBlocked, domain.RunStateRecoveryRequired,
		} {
			r := reachRun(t, from)
			recorded := string(r.recordedResume())
			rev := r.state.Run.PlanRevision + 1
			implicit := []domain.Payload{
				domain.RunPrepared{},
				domain.StrategySelected{},
				domain.PlanGenerated{AttemptID: attemptID(90), Artifact: ref(t, domain.AttemptOutputPath(attemptID(90), 1))},
				domain.PlanValidationFailed{Summary: "invalid"},
				domain.PlanApprovalRequested{Artifact: ref(t, domain.AttemptOutputPath(attemptID(90), 1))},
				domain.PlanAccepted{Revision: rev, Plan: ref(t, domain.PlanPath(rev)), Phases: planDefs()},
				domain.PlanChangesRequested{Feedback: ref(t, "plans/rev-0001/feedback.json")},
				domain.FinalVerificationStarted{},
				domain.ReplanningStarted{Cause: domain.ReplanCauseReplanAuthorized},
				domain.FinalEvaluationStarted{},
				domain.RunCompleted{},
				domain.FinalEvaluationRetried{FailedAttemptID: attemptID(90)},
				domain.RunCancelled{Reason: domain.CancelReasonUserRequest},
			}
			for _, p := range implicit {
				to := o.uniqueTarget(t, fsm.MachineRun, p.EventType())
				if !o.allowed(fsm.MachineRun, p.EventType(), string(from), to, recorded) {
					rejected++
					r.reject(p, fsm.ErrIllegalTransition)
				}
			}
			for _, to := range []domain.RunState{domain.RunStateWaitingForUser, domain.RunStateBlocked, domain.RunStateRecoveryRequired} {
				p := domain.RunSuspended{To: to, ResumeState: from}
				if from == to {
					p.ResumeState = domain.RunStateExecuting // the payload must be valid: the resume state differs from To
				}
				switch to {
				case domain.RunStateWaitingForUser:
					p.Reason, p.RequestID = domain.SuspendReasonUserInput, requestID(7)
				case domain.RunStateBlocked:
					p.Reason = domain.SuspendReasonLimitReached
				case domain.RunStateRecoveryRequired:
					p.Reason = domain.SuspendReasonUnknownOutcome
				}
				if !o.allowed(fsm.MachineRun, p.EventType(), string(from), string(to), recorded) {
					rejected++
					r.reject(p, fsm.ErrIllegalTransition)
				}
			}
			for _, to := range domain.RunStates() {
				if to.Terminal() {
					continue
				}
				if !o.allowed(fsm.MachineRun, domain.EventRunResumed, string(from), string(to), recorded) {
					rejected++
					r.reject(domain.RunResumed{To: to}, fsm.ErrIllegalTransition)
				}
			}
		}
	})

	t.Run("phase", func(t *testing.T) {
		for _, from := range domain.PhaseStates() {
			r := reachPhase(t, from)
			recorded := string(phaseOf(t, r.state, phase1).ResumeState)
			unknown := attemptID(90)
			implicit := []domain.Payload{
				domain.PhaseReady{Revision: 1, PhaseID: phase1},
				domain.AttemptScheduled{AttemptID: attemptID(91), Role: domain.RoleImplementer, Revision: 1, PhaseID: phase1},
				domain.PhaseVerificationStarted{Revision: 1, PhaseID: phase1, AttemptID: unknown},
				domain.PhaseEvaluationStarted{Revision: 1, PhaseID: phase1},
				domain.PhaseIntegrationQueued{Revision: 1, PhaseID: phase1},
				domain.PhaseEvaluationRetried{Revision: 1, PhaseID: phase1, FailedAttemptID: unknown},
				domain.PhaseIntegrationStarted{Revision: 1, PhaseID: phase1},
				domain.PhaseAccepted{Revision: 1, PhaseID: phase1, AttemptID: unknown},
				domain.PhaseRetryAuthorized{Revision: 1, PhaseID: phase1},
				domain.PhaseFailed{Revision: 1, PhaseID: phase1},
				domain.PhaseSuperseded{Revision: 1, PhaseID: phase1},
			}
			for _, cause := range domain.PhaseRecoveryCauses() {
				implicit = append(implicit, domain.PhaseRecovering{Revision: 1, PhaseID: phase1, Cause: cause})
			}
			for _, p := range implicit {
				to := o.uniqueTarget(t, fsm.MachinePhase, p.EventType())
				if !o.allowed(fsm.MachinePhase, p.EventType(), string(from), to, recorded) {
					rejected++
					r.reject(p, fsm.ErrIllegalTransition)
				}
			}
			for _, to := range []domain.PhaseState{domain.PhaseStateWaitingForUser, domain.PhaseStateRecoveryRequired} {
				p := domain.PhaseSuspended{Revision: 1, PhaseID: phase1, To: to, ResumeState: domain.PhaseStateRunning}
				if from == domain.PhaseStateRunning || to == domain.PhaseStateRunning {
					p.ResumeState = domain.PhaseStateEvaluating
				}
				if to == domain.PhaseStateWaitingForUser {
					p.RequestID = requestID(7)
				}
				if !o.allowed(fsm.MachinePhase, p.EventType(), string(from), string(to), recorded) {
					rejected++
					r.reject(p, fsm.ErrIllegalTransition)
				}
			}
			for _, to := range domain.PhaseStates() {
				if !o.allowed(fsm.MachinePhase, domain.EventPhaseResumed, string(from), string(to), recorded) {
					rejected++
					r.reject(domain.PhaseResumed{Revision: 1, PhaseID: phase1, To: to}, fsm.ErrIllegalTransition)
				}
				if !o.allowed(fsm.MachinePhase, domain.EventPhaseReconciled, string(from), string(to), recorded) {
					rejected++
					r.reject(domain.PhaseReconciled{Revision: 1, PhaseID: phase1, To: to}, fsm.ErrIllegalTransition)
				}
			}
		}
	})

	t.Run("attempt", func(t *testing.T) {
		for _, from := range domain.AttemptStates() {
			r := reachAttempt(t, from)
			id := r.lastAttempt()
			implicit := []domain.Payload{
				domain.AttemptStarted{AttemptID: id},
				domain.AttemptFinished{AttemptID: id, Output: ref(t, domain.AttemptOutputPath(id, 1))},
				domain.AttemptFailed{AttemptID: id, Outcome: domain.OutcomeFailed},
				domain.AttemptTimedOut{AttemptID: id},
				domain.AttemptCancelled{AttemptID: id},
				domain.AttemptAwaitingInput{AttemptID: id, RequestID: requestID(5)},
				domain.AttemptResumed{AttemptID: id},
				domain.AttemptOutcomeUnknown{AttemptID: id},
			}
			for _, reason := range domain.AbandonReasons() {
				implicit = append(implicit, domain.AttemptAbandoned{AttemptID: id, Reason: reason})
			}
			for _, p := range implicit {
				to := o.uniqueTarget(t, fsm.MachineAttempt, p.EventType())
				if !o.allowed(fsm.MachineAttempt, p.EventType(), string(from), to, "") {
					rejected++
					r.reject(p, fsm.ErrIllegalTransition)
				}
			}
			for _, to := range []domain.AttemptState{domain.AttemptStateFinished, domain.AttemptStateFailed, domain.AttemptStateAbandoned} {
				p := domain.AttemptReconciled{AttemptID: id, To: to}
				if to == domain.AttemptStateFinished {
					p.Output = ref(t, domain.AttemptOutputPath(id, 2))
				}
				if !o.allowed(fsm.MachineAttempt, p.EventType(), string(from), string(to), "") {
					rejected++
					r.reject(p, fsm.ErrIllegalTransition)
				}
			}
		}
	})

	if rejected < 500 {
		t.Errorf("only %d unlisted combinations were checked; the loops are not exercising the tables", rejected)
	}
	t.Logf("%d unlisted transitions were rejected with ErrIllegalTransition", rejected)
}
