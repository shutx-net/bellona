package projection

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/shutx-net/bellona/internal/domain"
	"github.com/shutx-net/bellona/internal/fsm"
)

var (
	inv = ErrInvalidEvent
	ill = fsm.ErrIllegalTransition
)

// ---------------------------------------------------------------------------
// Illegal transitions
// ---------------------------------------------------------------------------

func TestApplyIllegalTransitions(t *testing.T) {
	tests := []struct {
		name  string
		state func(t *testing.T) *run
		p     func(r *run) domain.Payload
	}{
		{"PhaseAccepted on a Ready Phase", func(t *testing.T) *run { return reachPhase(t, domain.PhaseStateReady) },
			func(r *run) domain.Payload {
				return domain.PhaseAccepted{Revision: 1, PhaseID: phase1, AttemptID: attemptID(40)}
			}},
		{"RunCompleted from Executing", func(t *testing.T) *run { return reachRun(t, domain.RunStateExecuting) },
			func(r *run) domain.Payload { return domain.RunCompleted{} }},
		{"AttemptFinished on a Scheduled Attempt", func(t *testing.T) *run { return reachAttempt(t, domain.AttemptStateScheduled) },
			func(r *run) domain.Payload {
				id := r.lastAttempt()
				return domain.AttemptFinished{AttemptID: id, Output: ref(r.tb, domain.AttemptOutputPath(id, 1))}
			}},
		{"RunSuspended to Blocked while Blocked", func(t *testing.T) *run { return reachRun(t, domain.RunStateBlocked) },
			func(r *run) domain.Payload {
				return domain.RunSuspended{To: domain.RunStateBlocked, Reason: domain.SuspendReasonLimitReached, ResumeState: domain.RunStateExecuting}
			}},
		{"RunResumed to a state other than the recorded one", func(t *testing.T) *run { return reachSuspended(t, domain.RunStateBlocked, domain.RunStateExecuting) },
			func(r *run) domain.Payload { return domain.RunResumed{To: domain.RunStatePlanning} }},
		{"RunResumed while not suspended", func(t *testing.T) *run { return reachRun(t, domain.RunStateExecuting) },
			func(r *run) domain.Payload { return domain.RunResumed{To: domain.RunStateExecuting} }},
		{"PhaseResumed to a state other than ResumeState", func(t *testing.T) *run { return reachPhase(t, domain.PhaseStateWaitingForUser) },
			func(r *run) domain.Payload {
				return domain.PhaseResumed{Revision: 1, PhaseID: phase1, To: domain.PhaseStateEvaluating}
			}},
		{"PhaseSuperseded on a Superseded Phase", func(t *testing.T) *run { return reachPhase(t, domain.PhaseStateSuperseded) },
			func(r *run) domain.Payload { return domain.PhaseSuperseded{Revision: 1, PhaseID: phase1} }},
		{"a second Implementer Attempt on a Running Phase", func(t *testing.T) *run { return reachPhase(t, domain.PhaseStateRunning) },
			func(r *run) domain.Payload {
				return domain.AttemptScheduled{AttemptID: r.newAttempt(), Role: domain.RoleImplementer, Revision: 1, PhaseID: phase1}
			}},
		{"an Implementer Attempt on a Pending Phase", func(t *testing.T) *run { return reachPhase(t, domain.PhaseStatePending) },
			func(r *run) domain.Payload {
				return domain.AttemptScheduled{AttemptID: r.newAttempt(), Role: domain.RoleImplementer, Revision: 1, PhaseID: phase1}
			}},
		{"AttemptReconciled on a Running Attempt", func(t *testing.T) *run { return reachAttempt(t, domain.AttemptStateRunning) },
			func(r *run) domain.Payload {
				return domain.AttemptReconciled{AttemptID: r.lastAttempt(), To: domain.AttemptStateFailed}
			}},
		{"PlanAccepted while Planning", func(t *testing.T) *run { return reachRun(t, domain.RunStatePlanning) },
			func(r *run) domain.Payload {
				return domain.PlanAccepted{Revision: 1, Plan: ref(r.tb, domain.PlanPath(1)), Phases: planDefs()}
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := tt.state(t)
			r.reject(tt.p(r), ill)
		})
	}
}

// ---------------------------------------------------------------------------
// Entities that the payload names
// ---------------------------------------------------------------------------

func TestApplyUnknownEntities(t *testing.T) {
	t.Run("attempt", func(t *testing.T) {
		r := reachAttempt(t, domain.AttemptStateRunning)
		unknown := attemptID(77)
		for _, p := range []domain.Payload{
			domain.AttemptStarted{AttemptID: unknown},
			domain.AttemptFinished{AttemptID: unknown, Output: ref(t, domain.AttemptOutputPath(unknown, 1))},
			domain.AttemptFailed{AttemptID: unknown, Outcome: domain.OutcomeFailed},
			domain.AttemptTimedOut{AttemptID: unknown},
			domain.AttemptCancelled{AttemptID: unknown},
			domain.AttemptAwaitingInput{AttemptID: unknown, RequestID: requestID(1)},
			domain.AttemptResumed{AttemptID: unknown},
			domain.AttemptAbandoned{AttemptID: unknown, Reason: domain.AbandonReasonLaunchNotOccurred},
			domain.AttemptOutcomeUnknown{AttemptID: unknown},
			domain.AttemptReconciled{AttemptID: unknown, To: domain.AttemptStateFailed},
		} {
			r.reject(p, inv)
		}
	})
	t.Run("phase and revision", func(t *testing.T) {
		r := reachPhase(t, domain.PhaseStateReady)
		for _, p := range []domain.Payload{
			domain.PhaseReady{Revision: 1, PhaseID: "phase-99"},
			domain.PhaseReady{Revision: 2, PhaseID: phase1},
			domain.PhaseSuperseded{Revision: 2, PhaseID: phase1},
			domain.PhaseSuperseded{Revision: 1, PhaseID: "phase-99"},
			domain.PhaseFailed{Revision: 1, PhaseID: "phase-99"},
			domain.AttemptScheduled{AttemptID: r.newAttempt(), Role: domain.RoleImplementer, Revision: 1, PhaseID: "phase-99"},
		} {
			r.reject(p, inv)
		}
	})
	t.Run("no revision before the first plan", func(t *testing.T) {
		r := reachRun(t, domain.RunStatePlanning)
		r.reject(domain.PhaseReady{Revision: 1, PhaseID: phase1}, inv)
	})
}

// twoRevisions returns a run that accepted revision 1, replanned, and accepted
// revision 2 with the same graph. Revision 1 is then an old revision.
func twoRevisions(t *testing.T) *run {
	t.Helper()
	r := reachRun(t, domain.RunStateExecuting)
	r.do(domain.ReplanningStarted{Cause: domain.ReplanCauseReplanAuthorized})
	r.planCandidate()
	r.accept()
	if r.state.Run.PlanRevision != 2 || len(r.state.Revisions) != 2 {
		t.Fatalf("PlanRevision %d, %d revisions", r.state.Run.PlanRevision, len(r.state.Revisions))
	}
	return r
}

func TestApplyOldRevision(t *testing.T) {
	r := twoRevisions(t)
	// Phase events other than PhaseSuperseded must name the current revision,
	// and so must the Attempts that work for a Phase.
	r.reject(domain.PhaseReady{Revision: 1, PhaseID: phase1}, inv)
	r.reject(domain.AttemptScheduled{AttemptID: r.newAttempt(), Role: domain.RoleImplementer, Revision: 1, PhaseID: phase1}, inv)
	r.reject(domain.AttemptScheduled{AttemptID: r.newAttempt(), Role: domain.RoleEvaluator, Revision: 1, PhaseID: phase1}, inv)
	r.reject(domain.AttemptScheduled{AttemptID: r.newAttempt(), Role: domain.RolePlanner, Revision: 1}, inv)
	// The current revision works, and an old Phase can still be superseded.
	r.do(domain.PhaseReady{Revision: 2, PhaseID: phase1})
	r.do(domain.PhaseSuperseded{Revision: 1, PhaseID: phase1})
	if got := phaseOf(t, r.state, phase1).State; got != domain.PhaseStateSuperseded {
		t.Errorf("phase-01 of revision 1 is %s, want Superseded", got)
	}
	p2, ok := r.state.Phase(2, phase1)
	if !ok || p2.State != domain.PhaseStateReady {
		t.Errorf("phase-01 of revision 2 is %+v, want Ready", p2)
	}
	if rev, ok := r.state.CurrentRevision(); !ok || rev.Number != 2 {
		t.Errorf("CurrentRevision() = %+v, %v", rev, ok)
	}
}

// ---------------------------------------------------------------------------
// AttemptScheduled
// ---------------------------------------------------------------------------

func TestApplyAttemptScheduled(t *testing.T) {
	t.Run("not while Created, Blocked, WaitingForUser or RecoveryRequired", func(t *testing.T) {
		for _, state := range []domain.RunState{
			domain.RunStateCreated, domain.RunStateBlocked, domain.RunStateWaitingForUser, domain.RunStateRecoveryRequired,
		} {
			r := reachRun(t, state)
			r.reject(domain.AttemptScheduled{AttemptID: r.newAttempt(), Role: domain.RoleCoordinator}, inv)
		}
	})
	t.Run("a new Attempt ID", func(t *testing.T) {
		r := reachAttempt(t, domain.AttemptStateScheduled)
		r.reject(domain.AttemptScheduled{AttemptID: r.lastAttempt(), Role: domain.RoleCoordinator}, inv)
	})
	t.Run("a revision that is not the current one", func(t *testing.T) {
		r := reachRun(t, domain.RunStatePlanning)
		r.reject(domain.AttemptScheduled{AttemptID: r.newAttempt(), Role: domain.RolePlanner, Revision: 1}, inv)
		r = reachRun(t, domain.RunStateExecuting)
		r.reject(domain.AttemptScheduled{AttemptID: r.newAttempt(), Role: domain.RolePlanner, Revision: 2}, inv)
		r.do(domain.AttemptScheduled{AttemptID: r.newAttempt(), Role: domain.RoleCoordinator, Revision: 1})
	})
	t.Run("a phase review needs an Evaluating Phase", func(t *testing.T) {
		for _, state := range []domain.PhaseState{domain.PhaseStateRunning, domain.PhaseStateVerifying, domain.PhaseStateIntegrationPending, domain.PhaseStateRecovering} {
			r := reachPhase(t, state)
			r.reject(domain.AttemptScheduled{AttemptID: r.newAttempt(), Role: domain.RoleEvaluator, Revision: 1, PhaseID: phase1}, inv)
		}
		r := reachPhase(t, domain.PhaseStateEvaluating)
		before := phaseOf(t, r.state, phase1)
		id := r.schedule(domain.RoleEvaluator, 1, phase1)
		after := phaseOf(t, r.state, phase1)
		if after.State != domain.PhaseStateEvaluating {
			t.Errorf("the Phase moved to %s", after.State)
		}
		if got, want := len(after.Attempts), len(before.Attempts)+1; got != want || after.Attempts[got-1] != id {
			t.Errorf("Phase.Attempts is %v, want %s appended to %v", after.Attempts, id, before.Attempts)
		}
		if !after.UpdatedAt.Equal(timeAt(r.state.LastSeq)) {
			t.Errorf("Phase.UpdatedAt is %s, want the event time", after.UpdatedAt)
		}
	})
	t.Run("the final evaluation needs a FinalEvaluation Run", func(t *testing.T) {
		for _, state := range []domain.RunState{domain.RunStateExecuting, domain.RunStateFinalVerification, domain.RunStatePlanning} {
			r := reachRun(t, state)
			r.reject(domain.AttemptScheduled{AttemptID: r.newAttempt(), Role: domain.RoleEvaluator}, inv)
		}
		r := reachRun(t, domain.RunStateFinalEvaluation)
		id := r.schedule(domain.RoleEvaluator, 0, "")
		if a := attemptOf(t, r.state, id); a.PhaseID != "" || a.State != domain.AttemptStateScheduled {
			t.Errorf("the final evaluator Attempt is %+v", a)
		}
	})
	t.Run("a planner, a coordinator and the payload fields", func(t *testing.T) {
		r := reachRun(t, domain.RunStatePlanning)
		id := r.newAttempt()
		r.do(domain.AttemptScheduled{AttemptID: id, Role: domain.RolePlanner, SessionID: session1})
		got := attemptOf(t, r.state, id)
		want := Attempt{ID: id, Role: domain.RolePlanner, SessionID: session1, State: domain.AttemptStateScheduled,
			ScheduledAt: timeAt(r.state.LastSeq), UpdatedAt: timeAt(r.state.LastSeq)}
		if got != want {
			t.Errorf("got %+v, want %+v", got, want)
		}
		r.do(domain.AttemptScheduled{AttemptID: r.newAttempt(), Role: domain.RoleCoordinator})
	})
}

// Results of Attempts that are still running are saved while the Run is
// suspended (SPEC §6.1): only new Attempts are refused.
func TestApplyAttemptEventsWhileSuspended(t *testing.T) {
	r := reachRun(t, domain.RunStateExecuting)
	r.do(domain.PhaseReady{Revision: 1, PhaseID: phase1})
	id := r.implement(phase1)
	r.do(domain.AttemptStarted{AttemptID: id})
	r.suspend(domain.RunStateBlocked)
	out := ref(t, domain.AttemptOutputPath(id, 1))
	r.do(domain.AttemptFinished{AttemptID: id, Output: out})
	r.do(domain.PhaseVerificationStarted{Revision: 1, PhaseID: phase1, AttemptID: id})
	if got := r.state.Run.State; got != domain.RunStateBlocked {
		t.Errorf("the Run is %s", got)
	}
}

// ---------------------------------------------------------------------------
// Rules that relate a payload to the state
// ---------------------------------------------------------------------------

func TestApplyPlanGenerated(t *testing.T) {
	newRunInPlanning := func(t *testing.T) *run { return reachRun(t, domain.RunStatePlanning) }
	t.Run("unknown attempt", func(t *testing.T) {
		r := newRunInPlanning(t)
		r.reject(domain.PlanGenerated{AttemptID: attemptID(9), Artifact: ref(t, domain.AttemptOutputPath(attemptID(9), 1))}, inv)
	})
	t.Run("not a planner Attempt", func(t *testing.T) {
		r := newRunInPlanning(t)
		id := r.schedule(domain.RoleCoordinator, 0, "")
		out := r.finish(id)
		r.reject(domain.PlanGenerated{AttemptID: id, Artifact: out}, inv)
	})
	t.Run("an unfinished planner Attempt", func(t *testing.T) {
		for _, state := range []domain.AttemptState{domain.AttemptStateScheduled, domain.AttemptStateRunning, domain.AttemptStateFailed, domain.AttemptStateWaitingForUser} {
			r := reachAttempt(t, state)
			id := r.lastAttempt()
			r.reject(domain.PlanGenerated{AttemptID: id, Artifact: ref(t, domain.AttemptOutputPath(id, 1))}, inv)
		}
	})
	t.Run("a different artifact", func(t *testing.T) {
		r := newRunInPlanning(t)
		id, out := r.plannerFinished()
		other := refWith(t, out.Path, "other content")
		r.reject(domain.PlanGenerated{AttemptID: id, Artifact: other}, inv)
		other = ref(t, "plans/rev-0001/other.json")
		r.reject(domain.PlanGenerated{AttemptID: id, Artifact: other}, inv)
		r.do(domain.PlanGenerated{AttemptID: id, Artifact: out})
		if r.state.Run.PendingPlan != out {
			t.Errorf("PendingPlan is %v, want %v", r.state.Run.PendingPlan, out)
		}
	})
}

func TestApplyPlanApprovalAndAcceptance(t *testing.T) {
	t.Run("approval is bound to the plan under validation", func(t *testing.T) {
		r := reachRun(t, domain.RunStatePlanValidation)
		r.reject(domain.PlanApprovalRequested{Artifact: ref(t, "plans/rev-0001/other.json")}, inv)
		r.reject(domain.PlanApprovalRequested{Artifact: refWith(t, r.state.Run.PendingPlan.Path, "changed")}, inv)
		r.do(domain.PlanApprovalRequested{Artifact: r.state.Run.PendingPlan})
	})
	t.Run("the revision follows the current one", func(t *testing.T) {
		r := reachRun(t, domain.RunStateExecuting)
		r.do(domain.ReplanningStarted{Cause: domain.ReplanCauseReplanAuthorized})
		r.planCandidate()
		for _, rev := range []int{1, 3, 4} {
			r.reject(domain.PlanAccepted{Revision: rev, Plan: ref(t, domain.PlanPath(rev)), Phases: planDefs()}, inv)
		}
		r.accept()
		if r.state.Run.PlanRevision != 2 || r.state.Run.ReplanCount != 1 {
			t.Errorf("PlanRevision %d, ReplanCount %d", r.state.Run.PlanRevision, r.state.Run.ReplanCount)
		}
	})
	t.Run("the first revision is 1", func(t *testing.T) {
		r := reachRun(t, domain.RunStatePlanValidation)
		r.reject(domain.PlanAccepted{Revision: 2, Plan: ref(t, domain.PlanPath(2)), Phases: planDefs()}, inv)
	})
	t.Run("changes requested leave the replan counter alone", func(t *testing.T) {
		r := reachRun(t, domain.RunStateWaitingApproval)
		r.do(domain.PlanChangesRequested{Feedback: ref(t, "plans/rev-0001/feedback.json")})
		if r.state.Run.ReplanCount != 0 || r.state.Run.ChangeRequestCount != 1 || !r.state.Run.PendingPlan.IsZero() {
			t.Errorf("ReplanCount %d, ChangeRequestCount %d, PendingPlan %v", r.state.Run.ReplanCount, r.state.Run.ChangeRequestCount, r.state.Run.PendingPlan)
		}
	})
}

// T-01 (M0 share): the reducer refuses a plan whose Phase dependencies are
// cyclic, and the other defects of a Phase graph.
func TestApplyRejectsCyclicPhaseDependencies(t *testing.T) {
	def := func(id domain.PhaseID, deps ...domain.PhaseID) domain.PhaseDefinition {
		if deps == nil {
			deps = []domain.PhaseID{}
		}
		return domain.PhaseDefinition{ID: id, DependsOn: deps}
	}
	graphs := map[string][]domain.PhaseDefinition{
		"a cycle of two":      {def("a", "b"), def("b", "a")},
		"a cycle of three":    {def("a"), def("b", "a", "d"), def("c", "b"), def("d", "c")},
		"a self dependency":   {def("a", "a")},
		"an unknown phase":    {def("a", "x")},
		"a duplicate phase":   {def("a"), def("a")},
		"a repeated depends":  {def("a"), def("b", "a", "a")},
		"no phase":            {},
		"an invalid phase ID": {def("a/b")},
	}
	r := reachRun(t, domain.RunStatePlanValidation)
	for name, phases := range graphs {
		t.Run(name, func(t *testing.T) {
			r.reject(domain.PlanAccepted{Revision: 1, Plan: ref(t, domain.PlanPath(1)), Phases: phases}, inv)
		})
	}
	// A dependency may be listed before the phase that it names.
	r.do(domain.PlanAccepted{Revision: 1, Plan: ref(t, domain.PlanPath(1)),
		Phases: []domain.PhaseDefinition{def("b", "a"), def("a")}})
	rev, _ := r.state.CurrentRevision()
	if len(rev.Phases) != 2 || rev.Phases[0].ID != "b" || len(rev.Phases[0].DependsOn) != 1 {
		t.Errorf("revision %+v", rev)
	}
}

func TestApplyReplanningStarted(t *testing.T) {
	cases := []struct {
		from  domain.RunState
		cause domain.ReplanCause
		ok    bool
	}{
		{domain.RunStateExecuting, domain.ReplanCauseReplanAuthorized, true},
		{domain.RunStateExecuting, domain.ReplanCauseFinalVerificationFailed, false},
		{domain.RunStateExecuting, domain.ReplanCauseFinalEvaluationRejected, false},
		{domain.RunStateFinalVerification, domain.ReplanCauseFinalVerificationFailed, true},
		{domain.RunStateFinalVerification, domain.ReplanCauseReplanAuthorized, false},
		{domain.RunStateFinalEvaluation, domain.ReplanCauseFinalEvaluationRejected, true},
		{domain.RunStateFinalEvaluation, domain.ReplanCauseFinalVerificationFailed, false},
	}
	for _, c := range cases {
		t.Run(string(c.from)+"/"+string(c.cause), func(t *testing.T) {
			r := reachRun(t, c.from)
			p := domain.ReplanningStarted{Cause: c.cause}
			if !c.ok {
				r.reject(p, inv)
				return
			}
			r.do(p)
			if r.state.Run.State != domain.RunStatePlanning || r.state.Run.ReplanCount != 1 {
				t.Errorf("state %s, ReplanCount %d", r.state.Run.State, r.state.Run.ReplanCount)
			}
		})
	}
}

func TestApplyFinalEvaluationRetried(t *testing.T) {
	r := reachRun(t, domain.RunStateFinalEvaluation)
	failed := r.schedule(domain.RoleEvaluator, 0, "")
	r.reject(domain.FinalEvaluationRetried{FailedAttemptID: failed}, inv) // Scheduled
	r.fail(failed)
	timedOut := r.schedule(domain.RoleEvaluator, 0, "")
	r.do(domain.AttemptStarted{AttemptID: timedOut}).do(domain.AttemptTimedOut{AttemptID: timedOut})
	finished := r.schedule(domain.RoleEvaluator, 0, "")
	r.finish(finished)
	r.reject(domain.FinalEvaluationRetried{FailedAttemptID: finished}, inv)
	r.reject(domain.FinalEvaluationRetried{FailedAttemptID: attemptID(70)}, inv)
	planner := reachRun(t, domain.RunStateExecuting)
	pid := planner.newAttempt()
	planner.do(domain.AttemptScheduled{AttemptID: pid, Role: domain.RoleCoordinator})
	planner.do(domain.AttemptStarted{AttemptID: pid}).do(domain.AttemptFailed{AttemptID: pid, Outcome: domain.OutcomeFailed})
	planner.do(domain.FinalVerificationStarted{}).do(domain.FinalEvaluationStarted{})
	planner.reject(domain.FinalEvaluationRetried{FailedAttemptID: pid}, inv) // not an evaluator
	r.do(domain.FinalEvaluationRetried{FailedAttemptID: failed})
	r.do(domain.FinalEvaluationRetried{FailedAttemptID: timedOut})
	if r.state.Run.State != domain.RunStateFinalEvaluation {
		t.Errorf("the Run is %s", r.state.Run.State)
	}

	// An evaluator Attempt of a Phase review is not a Run-level one.
	pr := reachPhase(t, domain.PhaseStateEvaluating)
	eval := pr.schedule(domain.RoleEvaluator, 1, phase1)
	pr.fail(eval)
	pr.do(domain.PhaseEvaluationRetried{Revision: 1, PhaseID: phase1, FailedAttemptID: eval})
	pr.do(domain.FinalVerificationStarted{}).do(domain.FinalEvaluationStarted{})
	pr.reject(domain.FinalEvaluationRetried{FailedAttemptID: eval}, inv)
}

func TestApplyRunCancelled(t *testing.T) {
	t.Run("plan_rejected only from WaitingApproval", func(t *testing.T) {
		for _, state := range []domain.RunState{domain.RunStateExecuting, domain.RunStatePlanning, domain.RunStateBlocked, domain.RunStateCreated} {
			r := reachRun(t, state)
			r.reject(domain.RunCancelled{Reason: domain.CancelReasonPlanRejected}, inv)
		}
		r := reachRun(t, domain.RunStateWaitingApproval)
		r.do(domain.RunCancelled{Reason: domain.CancelReasonPlanRejected})
		if r.state.Run.State != domain.RunStateCancelled || r.state.Run.CancelReason != domain.CancelReasonPlanRejected {
			t.Errorf("state %s, reason %s", r.state.Run.State, r.state.Run.CancelReason)
		}
	})
	t.Run("user_request from any nonterminal state", func(t *testing.T) {
		for _, state := range []domain.RunState{domain.RunStateWaitingApproval, domain.RunStateExecuting, domain.RunStateRecoveryRequired} {
			r := reachRun(t, state)
			r.do(domain.RunCancelled{Reason: domain.CancelReasonUserRequest})
			if len(r.state.Run.Suspensions) != 0 || r.state.Run.CancelReason != domain.CancelReasonUserRequest {
				t.Errorf("from %s: %d suspensions, reason %s", state, len(r.state.Run.Suspensions), r.state.Run.CancelReason)
			}
		}
	})
}

func TestApplyPhaseRules(t *testing.T) {
	t.Run("PhaseVerificationStarted names the latest implementer Attempt, finished", func(t *testing.T) {
		r := reachRun(t, domain.RunStateExecuting)
		r.do(domain.PhaseReady{Revision: 1, PhaseID: phase1})
		id := r.implement(phase1)
		r.reject(domain.PhaseVerificationStarted{Revision: 1, PhaseID: phase1, AttemptID: id}, inv) // Scheduled
		r.do(domain.AttemptStarted{AttemptID: id})
		r.reject(domain.PhaseVerificationStarted{Revision: 1, PhaseID: phase1, AttemptID: id}, inv) // Running
		r.reject(domain.PhaseVerificationStarted{Revision: 1, PhaseID: phase1, AttemptID: attemptID(70)}, inv)
		r.reject(domain.PhaseVerificationStarted{Revision: 1, PhaseID: phase1, AttemptID: attemptID(1)}, inv) // the planner, finished
		r.do(domain.AttemptFinished{AttemptID: id, Output: ref(t, domain.AttemptOutputPath(id, 1))})
		r.do(domain.PhaseVerificationStarted{Revision: 1, PhaseID: phase1, AttemptID: id})
	})
	t.Run("PhaseVerificationStarted needs the Attempt of that Phase", func(t *testing.T) {
		r := reachRun(t, domain.RunStateExecuting)
		r.do(domain.PhaseReady{Revision: 1, PhaseID: phase1})
		first := r.implement(phase1)
		r.finish(first)
		// A finished Attempt of another Phase does not do.
		r.do(domain.PhaseVerificationStarted{Revision: 1, PhaseID: phase1, AttemptID: first})
		r.do(domain.PhaseIntegrationQueued{Revision: 1, PhaseID: phase1})
		r.do(domain.PhaseIntegrationStarted{Revision: 1, PhaseID: phase1})
		r.do(domain.PhaseAccepted{Revision: 1, PhaseID: phase1, AttemptID: first})
		r.do(domain.PhaseReady{Revision: 1, PhaseID: phase2})
		second := r.implement(phase2)
		r.finish(second)
		r.reject(domain.PhaseVerificationStarted{Revision: 1, PhaseID: phase2, AttemptID: first}, inv)
		r.do(domain.PhaseVerificationStarted{Revision: 1, PhaseID: phase2, AttemptID: second})
		r.do(domain.PhaseIntegrationQueued{Revision: 1, PhaseID: phase2})
		r.do(domain.PhaseIntegrationStarted{Revision: 1, PhaseID: phase2})
		r.reject(domain.PhaseAccepted{Revision: 1, PhaseID: phase2, AttemptID: first}, inv)
		r.do(domain.PhaseAccepted{Revision: 1, PhaseID: phase2, AttemptID: second})
	})
	t.Run("PhaseRecovering's cause names the source state", func(t *testing.T) {
		for _, from := range []domain.PhaseState{domain.PhaseStateRunning, domain.PhaseStateVerifying, domain.PhaseStateEvaluating, domain.PhaseStateIntegrating} {
			r := reachPhase(t, from)
			for _, cause := range domain.PhaseRecoveryCauses() {
				p := domain.PhaseRecovering{Revision: 1, PhaseID: phase1, Cause: cause}
				if cause.Source() == from {
					got := apply(t, r.state, p)
					if ph := phaseOf(t, got, phase1); ph.State != domain.PhaseStateRecovering || ph.RecoveryCause != cause {
						t.Errorf("%s from %s: Phase %+v", cause, from, ph)
					}
				} else {
					r.reject(p, inv)
				}
			}
		}
	})
	t.Run("PhaseEvaluationRetried names a failed evaluator Attempt of the Phase", func(t *testing.T) {
		r := reachPhase(t, domain.PhaseStateEvaluating)
		scheduled := r.schedule(domain.RoleEvaluator, 1, phase1)
		r.reject(domain.PhaseEvaluationRetried{Revision: 1, PhaseID: phase1, FailedAttemptID: scheduled}, inv) // not failed
		r.fail(scheduled)
		implementer := attemptID(2)
		r.reject(domain.PhaseEvaluationRetried{Revision: 1, PhaseID: phase1, FailedAttemptID: implementer}, inv) // not an evaluator
		r.reject(domain.PhaseEvaluationRetried{Revision: 1, PhaseID: phase1, FailedAttemptID: attemptID(70)}, inv)
		r.do(domain.PhaseEvaluationRetried{Revision: 1, PhaseID: phase1, FailedAttemptID: scheduled})
		timedOut := r.schedule(domain.RoleEvaluator, 1, phase1)
		r.do(domain.AttemptStarted{AttemptID: timedOut}).do(domain.AttemptTimedOut{AttemptID: timedOut})
		r.do(domain.PhaseEvaluationRetried{Revision: 1, PhaseID: phase1, FailedAttemptID: timedOut})
		if got := phaseOf(t, r.state, phase1); got.State != domain.PhaseStateEvaluating || len(got.Attempts) != 3 {
			t.Errorf("Phase %+v", got)
		}
	})
	t.Run("PhaseSuspended records the state it leaves", func(t *testing.T) {
		r := reachPhase(t, domain.PhaseStateRunning)
		r.reject(domain.PhaseSuspended{Revision: 1, PhaseID: phase1, To: domain.PhaseStateWaitingForUser,
			ResumeState: domain.PhaseStateEvaluating, RequestID: requestID(3)}, inv)
		r.reject(domain.PhaseSuspended{Revision: 1, PhaseID: phase1, To: domain.PhaseStateRecoveryRequired,
			ResumeState: domain.PhaseStateReady}, inv)
		r.do(domain.PhaseSuspended{Revision: 1, PhaseID: phase1, To: domain.PhaseStateWaitingForUser,
			ResumeState: domain.PhaseStateRunning, RequestID: requestID(3)})
		if got := phaseOf(t, r.state, phase1); got.State != domain.PhaseStateWaitingForUser || got.ResumeState != domain.PhaseStateRunning {
			t.Errorf("Phase %+v", got)
		}
		r.do(domain.PhaseResumed{Revision: 1, PhaseID: phase1, To: domain.PhaseStateRunning})
		if got := phaseOf(t, r.state, phase1); got.State != domain.PhaseStateRunning || got.ResumeState != "" {
			t.Errorf("Phase %+v", got)
		}
	})
	t.Run("RecoveryRequired keeps no resume state, and reconciliation chooses the target", func(t *testing.T) {
		r := reachPhase(t, domain.PhaseStateRecoveryRequired)
		if got := phaseOf(t, r.state, phase1); got.ResumeState != "" {
			t.Errorf("ResumeState is %q", got.ResumeState)
		}
		r.do(domain.PhaseReconciled{Revision: 1, PhaseID: phase1, To: domain.PhaseStateEvaluating})
	})
	t.Run("a superseded Waiting Phase keeps no resume state", func(t *testing.T) {
		r := reachPhase(t, domain.PhaseStateWaitingForUser)
		r.do(domain.PhaseSuperseded{Revision: 1, PhaseID: phase1})
		if got := phaseOf(t, r.state, phase1); got.State != domain.PhaseStateSuperseded || got.ResumeState != "" {
			t.Errorf("Phase %+v", got)
		}
	})
}

func TestApplyAttemptRules(t *testing.T) {
	t.Run("a session that differs from the recorded one", func(t *testing.T) {
		r := reachRun(t, domain.RunStatePlanning)
		id := r.schedule(domain.RolePlanner, 0, "") // scheduled with session1
		r.reject(domain.AttemptStarted{AttemptID: id, SessionID: "another-session"}, inv)
		r.do(domain.AttemptStarted{AttemptID: id, SessionID: session1})
		if got := attemptOf(t, r.state, id).SessionID; got != session1 {
			t.Errorf("SessionID is %q", got)
		}
	})
	t.Run("a session reported at launch is recorded", func(t *testing.T) {
		r := reachRun(t, domain.RunStatePlanning)
		id := r.newAttempt()
		r.do(domain.AttemptScheduled{AttemptID: id, Role: domain.RolePlanner})
		r.do(domain.AttemptStarted{AttemptID: id, SessionID: "host-session-1"})
		if got := attemptOf(t, r.state, id).SessionID; got != "host-session-1" {
			t.Errorf("SessionID is %q", got)
		}
	})
	t.Run("no session reported keeps the assigned one", func(t *testing.T) {
		r := reachAttempt(t, domain.AttemptStateRunning)
		if got := attemptOf(t, r.state, r.lastAttempt()).SessionID; got != session1 {
			t.Errorf("SessionID is %q", got)
		}
	})
	t.Run("the abandon reason names the state", func(t *testing.T) {
		r := reachAttempt(t, domain.AttemptStateScheduled)
		r.reject(domain.AttemptAbandoned{AttemptID: r.lastAttempt(), Reason: domain.AbandonReasonSessionUnavailable}, inv)
		r = reachAttempt(t, domain.AttemptStateWaitingForUser)
		r.reject(domain.AttemptAbandoned{AttemptID: r.lastAttempt(), Reason: domain.AbandonReasonLaunchNotOccurred}, inv)
		r.do(domain.AttemptAbandoned{AttemptID: r.lastAttempt(), Reason: domain.AbandonReasonSessionUnavailable})
		if got := attemptOf(t, r.state, r.lastAttempt()); got.State != domain.AttemptStateAbandoned || got.AbandonReason != domain.AbandonReasonSessionUnavailable {
			t.Errorf("Attempt %+v", got)
		}
	})
	t.Run("an Attempt finishes only once", func(t *testing.T) {
		r := reachAttempt(t, domain.AttemptStateFinished)
		id := r.lastAttempt()
		r.reject(domain.AttemptFinished{AttemptID: id, Output: ref(t, domain.AttemptOutputPath(id, 2))}, ill)
	})
	t.Run("a reconciliation to Finished records the output", func(t *testing.T) {
		r := reachAttempt(t, domain.AttemptStateUnknown)
		id := r.lastAttempt()
		out := ref(t, domain.AttemptOutputPath(id, 1))
		r.do(domain.AttemptReconciled{AttemptID: id, To: domain.AttemptStateFinished, Output: out})
		if got := attemptOf(t, r.state, id); got.State != domain.AttemptStateFinished || got.Output != out || got.EndedAt.IsZero() {
			t.Errorf("Attempt %+v", got)
		}
	})
	t.Run("the failure fields are recorded", func(t *testing.T) {
		r := reachAttempt(t, domain.AttemptStateFailed)
		got := attemptOf(t, r.state, r.lastAttempt())
		if got.Outcome != domain.OutcomeFailed || got.ErrorClass != domain.ErrorClassTransientError || got.Detail != "the host exited" {
			t.Errorf("Attempt %+v", got)
		}
	})
	t.Run("UncertainAttempts lists Scheduled, Running and Unknown Attempts in commit order", func(t *testing.T) {
		r := reachRun(t, domain.RunStatePlanning)
		scheduled := r.schedule(domain.RoleCoordinator, 0, "")
		running := r.schedule(domain.RoleCoordinator, 0, "")
		r.do(domain.AttemptStarted{AttemptID: running})
		unknown := r.schedule(domain.RoleCoordinator, 0, "")
		r.do(domain.AttemptStarted{AttemptID: unknown}).do(domain.AttemptOutcomeUnknown{AttemptID: unknown})
		finished := r.schedule(domain.RoleCoordinator, 0, "")
		r.finish(finished)
		waiting := r.schedule(domain.RoleCoordinator, 0, "")
		r.do(domain.AttemptStarted{AttemptID: waiting}).do(domain.AttemptAwaitingInput{AttemptID: waiting, RequestID: requestID(1)})
		got := r.state.UncertainAttempts()
		want := []domain.AttemptID{scheduled, running, unknown}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("UncertainAttempts() = %v, want %v", got, want)
		}
		if got := reachRun(t, domain.RunStateCreated).state.UncertainAttempts(); len(got) != 0 {
			t.Errorf("UncertainAttempts() of a new Run = %v", got)
		}
	})
}

// ---------------------------------------------------------------------------
// Suspension and policy amendment
// ---------------------------------------------------------------------------

func TestApplyNestedSuspension(t *testing.T) {
	r := reachRun(t, domain.RunStateExecuting)
	r.do(domain.RunSuspended{To: domain.RunStateRecoveryRequired, Reason: domain.SuspendReasonUnknownOutcome, ResumeState: domain.RunStateExecuting})
	blocked := domain.RunSuspended{To: domain.RunStateBlocked, Reason: domain.SuspendReasonAuthenticationFailed,
		ResumeState: domain.RunStateRecoveryRequired, Detail: "login expired"}
	r.reject(domain.RunSuspended{To: domain.RunStateBlocked, Reason: domain.SuspendReasonLimitReached, ResumeState: domain.RunStateExecuting}, inv)
	r.do(blocked)
	stack := r.state.Run.Suspensions
	if len(stack) != 2 || stack[0].State != domain.RunStateRecoveryRequired || stack[1].State != domain.RunStateBlocked ||
		stack[1].ResumeState != domain.RunStateRecoveryRequired || stack[1].Detail != "login expired" {
		t.Fatalf("stack %+v", stack)
	}
	// The recorded resume state is the top one.
	r.reject(domain.RunResumed{To: domain.RunStateExecuting}, ill)
	r.do(domain.RunResumed{To: domain.RunStateRecoveryRequired})
	if r.state.Run.State != domain.RunStateRecoveryRequired || len(r.state.Run.Suspensions) != 1 {
		t.Fatalf("state %s, %d suspensions", r.state.Run.State, len(r.state.Run.Suspensions))
	}
	r.reject(domain.RunResumed{To: domain.RunStateBlocked}, ill)
	r.do(domain.RunResumed{To: domain.RunStateExecuting})
	if r.state.Run.State != domain.RunStateExecuting || len(r.state.Run.Suspensions) != 0 {
		t.Fatalf("state %s, %d suspensions", r.state.Run.State, len(r.state.Run.Suspensions))
	}
	// A suspended Run can be suspended again from the other suspended state: a
	// WaitingForUser Run that needs reconciliation.
	r.suspend(domain.RunStateWaitingForUser)
	r.do(domain.RunSuspended{To: domain.RunStateRecoveryRequired, Reason: domain.SuspendReasonUnknownOutcome, ResumeState: domain.RunStateWaitingForUser})
	// WaitingForUser is reachable only from the four states of row run-20.
	r.reject(domain.RunSuspended{To: domain.RunStateWaitingForUser, Reason: domain.SuspendReasonUserInput,
		ResumeState: domain.RunStateRecoveryRequired, RequestID: requestID(9)}, ill)
}

func TestApplyRunResumedRequestID(t *testing.T) {
	r := reachRun(t, domain.RunStateExecuting)
	r.suspend(domain.RunStateWaitingForUser)
	req := r.state.Run.Suspensions[0].RequestID
	r.reject(domain.RunResumed{To: domain.RunStateExecuting, RequestID: requestID(40)}, inv)
	// The request ID may be left out, but if it is given it must match.
	got := apply(t, r.state, domain.RunResumed{To: domain.RunStateExecuting, RequestID: req})
	if got.Run.State != domain.RunStateExecuting {
		t.Errorf("state %s", got.Run.State)
	}
	got = apply(t, r.state, domain.RunResumed{To: domain.RunStateExecuting})
	if got.Run.State != domain.RunStateExecuting {
		t.Errorf("state %s", got.Run.State)
	}
	// A Blocked Run has no request.
	b := reachRun(t, domain.RunStateBlocked)
	b.reject(domain.RunResumed{To: domain.RunStateExecuting, RequestID: req}, inv)
	b.do(domain.RunResumed{To: domain.RunStateExecuting})
}

func TestApplyRunSuspendedResumeState(t *testing.T) {
	r := reachRun(t, domain.RunStateExecuting)
	r.reject(domain.RunSuspended{To: domain.RunStateBlocked, Reason: domain.SuspendReasonLimitReached, ResumeState: domain.RunStatePlanning}, inv)
	r.reject(domain.RunSuspended{To: domain.RunStateRecoveryRequired, Reason: domain.SuspendReasonUnknownOutcome, ResumeState: domain.RunStateBlocked}, inv)
	r.do(domain.RunSuspended{To: domain.RunStateBlocked, Reason: domain.SuspendReasonNoProgress, ResumeState: domain.RunStateExecuting})
	if got := r.state.Run.Suspensions[0]; got.Reason != domain.SuspendReasonNoProgress || got.Seq != r.state.LastSeq || !got.Since.Equal(timeAt(r.state.LastSeq)) {
		t.Errorf("suspension %+v", got)
	}
}

func TestApplyPolicyAmended(t *testing.T) {
	t.Run("only while Blocked", func(t *testing.T) {
		for _, state := range []domain.RunState{domain.RunStateExecuting, domain.RunStateWaitingForUser, domain.RunStateRecoveryRequired, domain.RunStateCreated, domain.RunStatePlanning} {
			r := reachRun(t, state)
			r.reject(domain.PolicyAmended{Changes: []domain.LimitChange{{Limit: domain.LimitMaxReplans, Previous: "1", New: "3"}}}, inv)
		}
	})
	t.Run("the previous value must be the one in force", func(t *testing.T) {
		r := reachRun(t, domain.RunStateBlocked)
		r.reject(domain.PolicyAmended{Changes: []domain.LimitChange{{Limit: domain.LimitMaxReplans, Previous: "2", New: "3"}}}, inv)
		r.reject(domain.PolicyAmended{Changes: []domain.LimitChange{{Limit: domain.LimitTimeout, Previous: "unlimited", New: "2h0m0s"}}}, inv)
		r.reject(domain.PolicyAmended{Changes: []domain.LimitChange{
			{Limit: domain.LimitMaxReplans, Previous: "1", New: "3"},
			{Limit: domain.LimitMaxPhaseRetries, Previous: "5", New: "6"},
		}}, inv)
		if len(r.state.Amendments) != 0 || r.state.Limits.MaxReplans != 1 {
			t.Errorf("a rejected amendment left %+v and %+v", r.state.Amendments, r.state.Limits)
		}
	})
	t.Run("changes apply in order and are recorded", func(t *testing.T) {
		r := reachRun(t, domain.RunStateBlocked)
		before := r.state.Limits
		changes := []domain.LimitChange{
			{Limit: domain.LimitMaxReplans, Previous: "1", New: "3"},
			{Limit: domain.LimitTimeout, Previous: "1h30m0s", New: "unlimited"},
			{Limit: domain.LimitMaxTurnCostUSD, Previous: "1.5", New: "2.25"},
		}
		r.do(domain.PolicyAmended{Changes: changes})
		want := before
		want.MaxReplans, want.Timeout, want.MaxTurnCostUSD = 3, "", 2.25
		if r.state.Limits != want {
			t.Errorf("Limits %+v, want %+v", r.state.Limits, want)
		}
		if len(r.state.Amendments) != 1 {
			t.Fatalf("%d amendments", len(r.state.Amendments))
		}
		a := r.state.Amendments[0]
		if a.Seq != r.state.LastSeq || !a.Time.Equal(timeAt(r.state.LastSeq)) || !reflect.DeepEqual(a.Changes, changes) {
			t.Errorf("amendment %+v", a)
		}
		if r.state.Run.State != domain.RunStateBlocked || len(r.state.Run.Suspensions) != 1 {
			t.Errorf("the Run is %s with %d suspensions", r.state.Run.State, len(r.state.Run.Suspensions))
		}
		// The amendment does not alias the payload.
		changes[0].New = "99"
		if r.state.Amendments[0].Changes[0].New != "3" {
			t.Errorf("the recorded amendment follows a change of the payload")
		}
		// The previous value of a second amendment is the amended one.
		r.reject(domain.PolicyAmended{Changes: []domain.LimitChange{{Limit: domain.LimitMaxReplans, Previous: "1", New: "4"}}}, inv)
		r.do(domain.PolicyAmended{Changes: []domain.LimitChange{{Limit: domain.LimitMaxReplans, Previous: "3", New: "4"}}})
		if len(r.state.Amendments) != 2 || r.state.Limits.MaxReplans != 4 {
			t.Errorf("%d amendments, MaxReplans %d", len(r.state.Amendments), r.state.Limits.MaxReplans)
		}
		r.do(domain.RunResumed{To: domain.RunStateExecuting})
	})
	t.Run("a value that is not valid", func(t *testing.T) {
		r := reachRun(t, domain.RunStateBlocked)
		r.reject(domain.PolicyAmended{Changes: []domain.LimitChange{{Limit: domain.LimitMaxReplans, Previous: "1", New: "-1"}}}, inv)
	})
}

// ---------------------------------------------------------------------------
// Envelope and global rules
// ---------------------------------------------------------------------------

func TestApplyGlobalRules(t *testing.T) {
	r := reachRun(t, domain.RunStateExecuting)
	s := r.state
	good := domain.PhaseReady{Revision: 1, PhaseID: phase1}

	t.Run("sequence number", func(t *testing.T) {
		for _, seq := range []uint64{0, 1, s.LastSeq - 1, s.LastSeq, s.LastSeq + 2, s.LastSeq + 100} {
			mustRejectEvent(t, s, mkEvent(seq, good), inv)
		}
		apply(t, s, good)
	})
	t.Run("run ID", func(t *testing.T) {
		e := nextEvent(s, good)
		e.RunID = "run-fedcba9876543210"
		mustRejectEvent(t, s, e, inv)
	})
	t.Run("envelope", func(t *testing.T) {
		e := nextEvent(s, good)
		bad := e
		bad.SchemaVersion = 2
		mustRejectEvent(t, s, bad, inv)
		bad = e
		bad.Payload = nil
		mustRejectEvent(t, s, bad, inv)
		bad = e
		bad.Type = domain.EventPhaseFailed
		mustRejectEvent(t, s, bad, inv)
		bad = e
		bad.Time = time.Time{}
		mustRejectEvent(t, s, bad, inv)
		bad = e
		bad.Time = e.Time.In(time.FixedZone("JST", 9*3600))
		mustRejectEvent(t, s, bad, inv)
		bad = e
		bad.Time = e.Time.In(time.Local) // the zone is not UTC, even if the offset is zero
		mustRejectEvent(t, s, bad, inv)
	})
	t.Run("payload that does not validate", func(t *testing.T) {
		mustReject(t, s, domain.PhaseReady{Revision: 0, PhaseID: phase1}, inv)
		mustReject(t, s, domain.PhaseReady{Revision: 1, PhaseID: "not a phase id"}, inv)
		mustReject(t, s, domain.RunSuspended{To: domain.RunStateBlocked, Reason: domain.SuspendReasonUserInput, ResumeState: domain.RunStateExecuting}, inv)
	})
	t.Run("a pointer payload", func(t *testing.T) {
		e := nextEvent(s, good)
		e.Payload = &good
		mustRejectEvent(t, s, e, inv)
		// A nil pointer must be rejected, not called: the methods of a payload
		// have value receivers and panic on a nil pointer.
		var nilPointer *domain.PhaseReady
		e.Payload = nilPointer
		mustRejectEvent(t, s, e, inv)
		first := mkEvent(1, domain.RunCreated{Task: ref(t, domain.TaskPath)})
		first.Payload = (*domain.RunCreated)(nil)
		mustRejectEvent(t, State{}, first, inv)
	})
	t.Run("a second RunCreated", func(t *testing.T) {
		mustReject(t, s, domain.RunCreated{Task: ref(t, domain.TaskPath)}, inv)
		mustRejectEvent(t, s, mkEvent(1, domain.RunCreated{Task: ref(t, domain.TaskPath)}), inv)
	})
	t.Run("ManifestFrozen is the second event only", func(t *testing.T) {
		mf := domain.ManifestFrozen{Manifest: ref(t, domain.ManifestPath), Policy: ref(t, domain.PolicyPath), Limits: domain.Limits{}}
		mustReject(t, s, mf, inv)
		first := apply(t, State{}, domain.RunCreated{Task: ref(t, domain.TaskPath)})
		mustReject(t, first, domain.RunPrepared{}, inv)
		mustReject(t, first, domain.PhaseReady{Revision: 1, PhaseID: phase1}, inv)
		mustReject(t, first, domain.AttemptScheduled{AttemptID: attemptID(1), Role: domain.RoleCoordinator}, inv)
		second := apply(t, first, mf)
		if second.Run.State != domain.RunStateCreated || second.Run.Manifest != mf.Manifest || second.Run.Policy != mf.Policy || second.Limits != mf.Limits {
			t.Errorf("state after ManifestFrozen: %+v", second.Run)
		}
		mustReject(t, second, mf, inv)
	})
	t.Run("the first event", func(t *testing.T) {
		empty := State{}
		task := domain.RunCreated{Task: ref(t, domain.TaskPath)}
		mustRejectEvent(t, empty, mkEvent(2, task), inv)
		mustRejectEvent(t, empty, mkEvent(0, task), inv)
		mustRejectEvent(t, empty, mkEvent(1, domain.RunPrepared{}), inv)
		mustRejectEvent(t, empty, mkEvent(1, domain.ManifestFrozen{Manifest: ref(t, domain.ManifestPath), Policy: ref(t, domain.PolicyPath)}), inv)
		bad := mkEvent(1, task)
		bad.RunID = "not-a-run-id"
		mustRejectEvent(t, empty, bad, inv)
		got := apply(t, empty, task)
		if got.RunID != testRunID || got.LastSeq != 1 || got.Run.State != domain.RunStateCreated || got.Run.Task != task.Task ||
			!got.Run.CreatedAt.Equal(timeAt(1)) || !got.Run.UpdatedAt.Equal(timeAt(1)) {
			t.Errorf("state after RunCreated: %+v", got)
		}
		if !reflect.DeepEqual(got.Artifacts, []domain.ArtifactRef{task.Task}) {
			t.Errorf("Artifacts is %v", got.Artifacts)
		}
	})
}

// No event is accepted once the Run is Completed or Cancelled.
func TestApplyNothingAfterTheRunEnded(t *testing.T) {
	completed := reachRun(t, domain.RunStateExecuting)
	completed.do(domain.FinalVerificationStarted{}).do(domain.RunCompleted{})
	cancelled := reachRun(t, domain.RunStatePlanning)
	cancelled.do(domain.RunCancelled{Reason: domain.CancelReasonUserRequest})
	for name, r := range map[string]*run{"Completed": completed, "Cancelled": cancelled} {
		t.Run(name, func(t *testing.T) {
			payloads := catalogueSamples(t)
			if len(payloads) != len(domain.EventTypes()) {
				t.Fatalf("%d samples for %d event types", len(payloads), len(domain.EventTypes()))
			}
			for _, p := range payloads {
				err := r.reject(p, inv)
				if !strings.Contains(err.Error(), "no further event") {
					t.Errorf("%s: %v\n is not the rejection of an event after the end", p.EventType(), err)
				}
			}
		})
	}
}

// catalogueSamples returns one valid payload per event type, in the order of
// domain.EventTypes. Their entities need not exist in a state.
func catalogueSamples(tb testing.TB) []domain.Payload {
	tb.Helper()
	a1, a2, req := attemptID(1), attemptID(2), requestID(1)
	out := ref(tb, domain.AttemptOutputPath(a1, 1))
	samples := []domain.Payload{
		domain.RunCreated{Task: ref(tb, domain.TaskPath)},
		domain.ManifestFrozen{Manifest: ref(tb, domain.ManifestPath), Policy: ref(tb, domain.PolicyPath), Limits: domain.Limits{MaxPhaseRetries: 1, MaxReplans: 1}},
		domain.PolicyAmended{Changes: []domain.LimitChange{{Limit: domain.LimitMaxReplans, Previous: "1", New: "2"}}},
		domain.AttemptScheduled{AttemptID: a2, Role: domain.RoleImplementer, Revision: 1, PhaseID: phase1, SessionID: session1},

		domain.RunPrepared{},
		domain.StrategySelected{Decision: ref(tb, domain.AttemptOutputPath(a2, 1))},
		domain.PlanGenerated{AttemptID: a1, Artifact: out},
		domain.PlanValidationFailed{Summary: "invalid", Report: ref(tb, "plans/rev-0001/validation.json")},
		domain.PlanApprovalRequested{Artifact: out},
		domain.PlanAccepted{Revision: 1, Plan: ref(tb, domain.PlanPath(1)), Phases: planDefs()},
		domain.PlanChangesRequested{Feedback: ref(tb, "plans/rev-0001/feedback.json")},
		domain.FinalVerificationStarted{},
		domain.ReplanningStarted{Cause: domain.ReplanCauseReplanAuthorized},
		domain.FinalEvaluationStarted{},
		domain.RunCompleted{},
		domain.FinalEvaluationRetried{FailedAttemptID: a1},
		domain.RunSuspended{To: domain.RunStateWaitingForUser, Reason: domain.SuspendReasonUserInput, ResumeState: domain.RunStatePlanning, RequestID: req},
		domain.RunResumed{To: domain.RunStatePlanning, RequestID: req},
		domain.RunCancelled{Reason: domain.CancelReasonUserRequest},

		domain.PhaseReady{Revision: 1, PhaseID: phase1},
		domain.PhaseVerificationStarted{Revision: 1, PhaseID: phase1, AttemptID: a1},
		domain.PhaseRecovering{Revision: 1, PhaseID: phase1, Cause: domain.PhaseRecoveryCauseChecksFailed},
		domain.PhaseEvaluationStarted{Revision: 1, PhaseID: phase1},
		domain.PhaseIntegrationQueued{Revision: 1, PhaseID: phase1},
		domain.PhaseEvaluationRetried{Revision: 1, PhaseID: phase1, FailedAttemptID: a2},
		domain.PhaseIntegrationStarted{Revision: 1, PhaseID: phase1},
		domain.PhaseAccepted{Revision: 1, PhaseID: phase1, AttemptID: a1},
		domain.PhaseRetryAuthorized{Revision: 1, PhaseID: phase1},
		domain.PhaseFailed{Revision: 1, PhaseID: phase1},
		domain.PhaseSuspended{Revision: 1, PhaseID: phase1, To: domain.PhaseStateWaitingForUser, ResumeState: domain.PhaseStateRunning, RequestID: req},
		domain.PhaseResumed{Revision: 1, PhaseID: phase1, To: domain.PhaseStateRunning},
		domain.PhaseReconciled{Revision: 1, PhaseID: phase1, To: domain.PhaseStateVerifying},
		domain.PhaseSuperseded{Revision: 1, PhaseID: phase1},

		domain.AttemptStarted{AttemptID: a1, SessionID: session1},
		domain.AttemptAbandoned{AttemptID: a1, Reason: domain.AbandonReasonLaunchNotOccurred},
		domain.AttemptFinished{AttemptID: a1, Output: out},
		domain.AttemptFailed{AttemptID: a1, Outcome: domain.OutcomeFailed, ErrorClass: domain.ErrorClassTransientError, Detail: "failed"},
		domain.AttemptTimedOut{AttemptID: a1},
		domain.AttemptCancelled{AttemptID: a1},
		domain.AttemptAwaitingInput{AttemptID: a1, RequestID: req},
		domain.AttemptResumed{AttemptID: a1},
		domain.AttemptOutcomeUnknown{AttemptID: a1},
		domain.AttemptReconciled{AttemptID: a2, To: domain.AttemptStateFinished, Output: ref(tb, domain.AttemptOutputPath(a2, 2))},
	}
	for i, typ := range domain.EventTypes() {
		if i >= len(samples) || samples[i].EventType() != typ {
			tb.Fatalf("sample %d does not match the event type %s", i, typ)
		}
		if err := samples[i].Validate(); err != nil {
			tb.Fatalf("the sample for %s is not valid: %v", typ, err)
		}
	}
	return samples
}

// Every event type of the catalogue has a case in the reducer: none falls
// through to the "unsupported payload" rejection.
func TestApplyHandlesEveryEventType(t *testing.T) {
	r := reachRun(t, domain.RunStateExecuting)
	for _, p := range catalogueSamples(t) {
		_, err := Apply(r.state, nextEvent(r.state, p))
		if err != nil && strings.Contains(err.Error(), "unsupported payload") {
			t.Errorf("%s: %v", p.EventType(), err)
		}
	}
}

func TestApplyErrorValue(t *testing.T) {
	r := reachRun(t, domain.RunStateExecuting)
	e := nextEvent(r.state, domain.RunCompleted{})
	_, err := Apply(r.state, e)
	var ae *ApplyError
	if !errors.As(err, &ae) {
		t.Fatalf("%T is not an *ApplyError", err)
	}
	if ae.Seq != e.Seq || ae.Type != domain.EventRunCompleted {
		t.Errorf("ApplyError{Seq: %d, Type: %s}", ae.Seq, ae.Type)
	}
	var te *fsm.TransitionError
	if !errors.As(err, &te) || te.From != "Executing" || te.To != "Completed" {
		t.Errorf("the transition error is %+v", te)
	}
	if !strings.Contains(err.Error(), "RunCompleted") || !strings.Contains(err.Error(), "10") {
		t.Errorf("Error() = %q", err.Error())
	}
	// Success returns a literal nil error, not a typed nil.
	if _, err := Apply(r.state, nextEvent(r.state, domain.FinalVerificationStarted{})); err != nil {
		t.Errorf("Apply returned %v", err)
	}
}

// The state keeps no reference into the payload of an event: the slices of the
// payload may be reused by the caller after the event was applied.
func TestApplyDoesNotAliasThePayload(t *testing.T) {
	r := reachRun(t, domain.RunStatePlanValidation)
	phases := planDefs()
	r.do(domain.PlanAccepted{Revision: 1, Plan: ref(t, domain.PlanPath(1)), Phases: phases})
	before := canon(t, r.state)
	phases[0].ID = "changed"
	phases[1].DependsOn[0] = "changed"
	phases[1].DependsOn = append(phases[1].DependsOn, "more")
	if after := canon(t, r.state); !bytes.Equal(before, after) {
		t.Errorf("a change of the payload reached the state:\nbefore %s\nafter  %s", before, after)
	}

	b := reachRun(t, domain.RunStateBlocked)
	changes := []domain.LimitChange{{Limit: domain.LimitMaxReplans, Previous: "1", New: "3"}}
	b.do(domain.PolicyAmended{Changes: changes})
	before = canon(t, b.state)
	changes[0].New = "changed"
	if after := canon(t, b.state); !bytes.Equal(before, after) {
		t.Errorf("a change of the payload reached the amendment")
	}

	// Replay does not alias the events either.
	pv := reachRun(t, domain.RunStatePlanValidation)
	events := append([]domain.Event(nil), pv.events...)
	phases = planDefs()
	events = append(events, mkEvent(pv.state.LastSeq+1, domain.PlanAccepted{Revision: 1, Plan: ref(t, domain.PlanPath(1)), Phases: phases}))
	got, err := Replay(events)
	if err != nil {
		t.Fatal(err)
	}
	before = canon(t, got)
	phases[1].DependsOn[0] = "changed"
	if after := canon(t, got); !bytes.Equal(before, after) {
		t.Errorf("a change of an event reached the replayed state")
	}
}

// ---------------------------------------------------------------------------
// Artifacts
// ---------------------------------------------------------------------------

func TestApplyArtifactReferences(t *testing.T) {
	t.Run("a path is bound to one size and hash", func(t *testing.T) {
		r := reachRun(t, domain.RunStatePlanValidation)
		report := ref(t, "plans/rev-0001/validation.json")
		r.do(domain.PlanValidationFailed{Summary: "invalid", Report: report})
		n := len(r.state.Artifacts)
		r.planCandidate()
		n2 := len(r.state.Artifacts)
		// A different hash on a known path is refused, a different size too.
		r.reject(domain.PlanValidationFailed{Summary: "invalid", Report: refWith(t, report.Path, "other content of the same path")}, inv)
		changed := report
		changed.Size++
		r.reject(domain.PlanValidationFailed{Summary: "invalid", Report: changed}, inv)
		// The identical reference is accepted and not listed twice.
		r.do(domain.PlanValidationFailed{Summary: "invalid again", Report: report})
		if got := countRef(r.state.Artifacts, report); got != 1 {
			t.Errorf("the report is listed %d times", got)
		}
		if len(r.state.Artifacts) != n2 {
			t.Errorf("%d artifacts, want %d (and %d before the second plan)", len(r.state.Artifacts), n2, n)
		}
	})
	t.Run("artifacts are listed in commit order, unique by path", func(t *testing.T) {
		r := reachRun(t, domain.RunStateExecuting)
		var paths []string
		for _, a := range r.state.Artifacts {
			paths = append(paths, a.Path)
		}
		want := []string{
			domain.TaskPath, domain.ManifestPath, domain.PolicyPath,
			domain.AttemptOutputPath(attemptID(1), 1), domain.PlanPath(1),
		}
		if !reflect.DeepEqual(paths, want) {
			t.Errorf("artifacts %v, want %v", paths, want)
		}
	})
	t.Run("the paths of the engine are not artifacts", func(t *testing.T) {
		r := reachRun(t, domain.RunStatePlanValidation)
		for _, p := range []string{domain.EventsPath, domain.SnapshotPath, domain.LockPath, "logs/agent.log", "worktrees/w1/file"} {
			r.reject(domain.PlanValidationFailed{Summary: "invalid", Report: ref(t, p)}, inv)
		}
	})
	t.Run("a rejected event registers nothing", func(t *testing.T) {
		r := reachRun(t, domain.RunStatePlanValidation)
		before := len(r.state.Artifacts)
		r.reject(domain.PlanApprovalRequested{Artifact: ref(t, "plans/rev-0001/new.json")}, inv)
		if len(r.state.Artifacts) != before {
			t.Errorf("%d artifacts, want %d", len(r.state.Artifacts), before)
		}
	})
}

// ---------------------------------------------------------------------------
// Accessors and Clone
// ---------------------------------------------------------------------------

func TestStateAccessorsReturnCopies(t *testing.T) {
	r := reachPhase(t, domain.PhaseStateRunning)
	ph := phaseOf(t, r.state, phase2)
	if len(ph.DependsOn) != 1 {
		t.Fatalf("phase-02 depends on %v", ph.DependsOn)
	}
	ph.DependsOn[0] = "changed"
	if got := phaseOf(t, r.state, phase2).DependsOn[0]; got != phase1 {
		t.Errorf("a change of the returned Phase reached the state: %s", got)
	}
	rev, ok := r.state.CurrentRevision()
	if !ok {
		t.Fatal("no current revision")
	}
	rev.Phases[0].Attempts = append(rev.Phases[0].Attempts, "attempt-ffffffffffffffff")
	rev.Phases[1].DependsOn[0] = "changed"
	if got, _ := r.state.Phase(1, phase2); got.DependsOn[0] != phase1 {
		t.Errorf("a change of the current revision reached the state")
	}
	if _, ok := r.state.Phase(2, phase1); ok {
		t.Error("found a Phase in a revision that does not exist")
	}
	if _, ok := r.state.Phase(1, "phase-99"); ok {
		t.Error("found a Phase that does not exist")
	}
	if _, ok := r.state.Attempt(attemptID(99)); ok {
		t.Error("found an Attempt that does not exist")
	}
	if _, ok := (State{}).CurrentRevision(); ok {
		t.Error("the empty State has a current revision")
	}
}

// richState returns a state in which every slice has content and every
// optional field that the reducer sets is set.
func richState(t *testing.T) State {
	t.Helper()
	r := reachRun(t, domain.RunStateExecuting)
	r.do(domain.PhaseReady{Revision: 1, PhaseID: phase1})
	id := r.implement(phase1)
	r.do(domain.AttemptStarted{AttemptID: id, SessionID: session1})
	r.do(domain.PhaseSuspended{Revision: 1, PhaseID: phase1, To: domain.PhaseStateWaitingForUser,
		ResumeState: domain.PhaseStateRunning, RequestID: r.newRequest()})
	r.suspend(domain.RunStateBlocked)
	r.do(domain.PolicyAmended{Changes: []domain.LimitChange{
		{Limit: domain.LimitMaxReplans, Previous: "1", New: "2"},
		{Limit: domain.LimitMaxPhaseRetries, Previous: "2", New: "3"},
	}})
	return r.state
}

// slicePaths lists the path of every slice in the type tree of t, and of every
// struct type in a slice, in the form "Revisions[].Phases[].DependsOn".
func slicePaths(t reflect.Type, path string, out map[string]bool) {
	switch t.Kind() {
	case reflect.Struct:
		if t == reflect.TypeFor[time.Time]() {
			return
		}
		for f := range t.Fields() {
			next := f.Name
			if path != "" {
				next = path + "." + f.Name
			}
			slicePaths(f.Type, next, out)
		}
	case reflect.Slice:
		out[path] = true
		slicePaths(t.Elem(), path+"[]", out)
	case reflect.Map, reflect.Pointer, reflect.Interface, reflect.Chan, reflect.Func, reflect.Array:
		out["unsupported kind "+t.Kind().String()+" at "+path] = true
	}
}

// Clone shares no memory with the original: every slice of the clone, at every
// depth, has its own backing array, and nil stays nil. The fixture fills every
// slice that the type tree of State has, so a slice added to a type without a
// line in Clone fails here.
func TestCloneSharesNothing(t *testing.T) {
	s := richState(t)
	c := s.Clone()
	if !Equal(s, c) {
		t.Fatalf("the clone differs from the original")
	}
	found := make(map[string]bool)
	var walk func(path string, a, b reflect.Value)
	walk = func(path string, a, b reflect.Value) {
		switch a.Kind() {
		case reflect.Struct:
			if a.Type() == reflect.TypeFor[time.Time]() {
				return
			}
			for i := range a.NumField() {
				next := a.Type().Field(i).Name
				if path != "" {
					next = path + "." + next
				}
				walk(next, a.Field(i), b.Field(i))
			}
		case reflect.Slice:
			if a.IsNil() != b.IsNil() {
				t.Errorf("%s: nil-ness differs", path)
			}
			if a.Len() == 0 {
				return
			}
			found[path] = true
			if a.Pointer() == b.Pointer() {
				t.Errorf("%s: the clone shares the backing array", path)
			}
			for i := range a.Len() {
				walk(path+"[]", a.Index(i), b.Index(i))
			}
		}
	}
	walk("", reflect.ValueOf(s), reflect.ValueOf(c))

	want := make(map[string]bool)
	slicePaths(reflect.TypeFor[State](), "", want)
	for path := range want {
		if strings.HasPrefix(path, "unsupported") {
			t.Errorf("%s: Clone does not know how to copy it", path)
		}
		if !found[path] {
			t.Errorf("the fixture leaves %s empty, so Clone is not tested for it", path)
		}
	}
	if len(want) != 9 {
		t.Errorf("State has %d slices %v, want 9; update Clone and this test together", len(want), want)
	}

	// And a change of the clone does not reach the original.
	before := canon(t, s)
	c.Run.Suspensions[0].Detail = "changed"
	c.Amendments[0].Changes[0].New = "changed"
	c.Revisions[0].Phases[0].Attempts[0] = "attempt-ffffffffffffffff"
	c.Revisions[0].Phases[1].DependsOn[0] = "changed"
	c.Attempts[0].Detail = "changed"
	c.Artifacts[0].Size++
	if after := canon(t, s); string(before) != string(after) {
		t.Errorf("a change of the clone reached the original")
	}
}

func TestCanonicalAndEqual(t *testing.T) {
	s := richState(t)
	if !Equal(s, s) || !Equal(s, s.Clone()) {
		t.Error("a state differs from itself or from its clone")
	}
	other := s.Clone()
	other.Run.ReplanCount++
	if Equal(s, other) {
		t.Error("states that differ are equal")
	}
	// A nil slice and an empty slice have the same canonical encoding.
	a, b := s.Clone(), s.Clone()
	a.Run.Suspensions, b.Run.Suspensions = nil, []Suspension{}
	if !Equal(a, b) {
		t.Error("a nil slice and an empty slice are not equal")
	}
	// The empty State is equal to itself and to nothing else.
	if !Equal(State{}, State{}) || Equal(State{}, s) {
		t.Error("equality of the empty State")
	}
	if b, err := Canonical(State{}); err != nil || string(b) != "{}" {
		t.Errorf("Canonical(State{}) = %q, %v", b, err)
	}
	// A State that cannot be encoded is never equal, not even to itself.
	broken := s.Clone()
	broken.Run.State = "Nonsense"
	if _, err := Canonical(broken); err == nil {
		t.Error("Canonical accepted an invalid Run state")
	}
	if Equal(broken, broken) || Equal(broken, s) || Equal(s, broken) {
		t.Error("a State that cannot be encoded is equal to something")
	}
	partial := State{LastSeq: 3}
	if _, err := Canonical(partial); err == nil {
		t.Error("Canonical accepted a State with a sequence number and no Run ID")
	}
}
