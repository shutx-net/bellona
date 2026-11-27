package fsm

import "github.com/shutx-net/bellona/internal/domain"

// The three transition tables of SPEC §6, in the order of SPEC. A row's
// Condition is the text of the SPEC cell, verbatim; the SPEC parity test
// compares every row with SPEC.md. Do not reorder or reinterpret rows: change
// SPEC.md first, then this file, and bump WorkflowVersion.
//
// A row's Event is the event type that commits the transition. The same
// (from, to) pair can appear in several rows with different conditions
// (run-02 and run-22, phase-09 and phase-11); the event does not carry the row
// ID, and its payload records which condition applied.

var runRows = rows(MachineRun, []Row{
	{ID: "run-01", From: src(domain.RunStateCreated), Condition: "Manifest frozen, workspaces prepared (§13), baseline checks passed (§9)", To: dst(domain.RunStateStrategySelection), Event: domain.EventRunPrepared},
	{ID: "run-02", From: src(domain.RunStateCreated), Condition: "Baseline checks failed or the environment is missing", To: dst(domain.RunStateBlocked), Event: domain.EventRunSuspended},
	{ID: "run-03", From: src(domain.RunStateStrategySelection), Condition: "No decision is delegated to the Coordinator, or its decision is validated", To: dst(domain.RunStatePlanning), Event: domain.EventStrategySelected},
	{ID: "run-04", From: src(domain.RunStatePlanning), Condition: "Plan candidate received", To: dst(domain.RunStatePlanValidation), Event: domain.EventPlanGenerated},
	{ID: "run-05", From: src(domain.RunStatePlanValidation), Condition: "Invalid, retry allowed", To: dst(domain.RunStatePlanning), Event: domain.EventPlanValidationFailed},
	{ID: "run-06", From: src(domain.RunStatePlanValidation), Condition: "Valid, approval required", To: dst(domain.RunStateWaitingApproval), Event: domain.EventPlanApprovalRequested},
	{ID: "run-07", From: src(domain.RunStatePlanValidation), Condition: "Valid, no approval required", To: dst(domain.RunStateExecuting), Event: domain.EventPlanAccepted},
	{ID: "run-08", From: src(domain.RunStateWaitingApproval), Condition: "Approved (bound to the plan hash, §6.2)", To: dst(domain.RunStateExecuting), Event: domain.EventPlanAccepted},
	{ID: "run-09", From: src(domain.RunStateWaitingApproval), Condition: "Changes requested with user feedback", To: dst(domain.RunStatePlanning), Event: domain.EventPlanChangesRequested},
	{ID: "run-10", From: src(domain.RunStateWaitingApproval), Condition: "Rejected", To: dst(domain.RunStateCancelled), Event: domain.EventRunCancelled},
	{ID: "run-11", From: src(domain.RunStateExecuting), Condition: "All Phases `Accepted`; no active Attempts, unresolved questions, or pending integrations", To: dst(domain.RunStateFinalVerification), Event: domain.EventFinalVerificationStarted},
	{ID: "run-12", From: src(domain.RunStateExecuting), Condition: "Replan authorized and all active Attempts terminated and reconciled", To: dst(domain.RunStatePlanning), Event: domain.EventReplanningStarted},
	{ID: "run-13", From: src(domain.RunStateExecuting), Condition: "No Phase can make progress, and replanning is not authorized", To: dst(domain.RunStateBlocked), Event: domain.EventRunSuspended},
	{ID: "run-14", From: src(domain.RunStateFinalVerification), Condition: "Checks passed, final review required", To: dst(domain.RunStateFinalEvaluation), Event: domain.EventFinalEvaluationStarted},
	{ID: "run-15", From: src(domain.RunStateFinalVerification), Condition: "Checks passed, all reviews satisfied", To: dst(domain.RunStateCompleted), Event: domain.EventRunCompleted},
	{ID: "run-16", From: src(domain.RunStateFinalVerification), Condition: "Checks failed", To: dstNote("repair replanning", domain.RunStatePlanning), Event: domain.EventReplanningStarted},
	{ID: "run-17", From: src(domain.RunStateFinalEvaluation), Condition: "Accepted, evidence current", To: dst(domain.RunStateCompleted), Event: domain.EventRunCompleted},
	{ID: "run-18", From: src(domain.RunStateFinalEvaluation), Condition: "Rejected", To: dstNote("repair replanning", domain.RunStatePlanning), Event: domain.EventReplanningStarted},
	{ID: "run-19", From: src(domain.RunStateFinalEvaluation), Condition: "Evaluator execution failed or returned invalid output, retry allowed", To: dstNote("new Evaluator Attempt", domain.RunStateFinalEvaluation), Event: domain.EventFinalEvaluationRetried},
	{ID: "run-20", From: src(domain.RunStateStrategySelection, domain.RunStatePlanning, domain.RunStateExecuting, domain.RunStateFinalEvaluation), Condition: "Validated question request and all execution processes reaped", To: dst(domain.RunStateWaitingForUser), Event: domain.EventRunSuspended},
	{ID: "run-21", From: src(domain.RunStateWaitingForUser), Condition: "All mandatory answers received and freshness checks passed (§6.2)", To: dstResume(), Event: domain.EventRunResumed},
	{ID: "run-22", From: anyNonterminal(), Condition: "A Run-wide limit is reached (replan count, Run time, budget, or retries of a Planner, Coordinator, or final Evaluator Attempt), or the issue cannot be resolved without the user, such as a missing environment", To: dst(domain.RunStateBlocked), Event: domain.EventRunSuspended},
	{ID: "run-23", From: src(domain.RunStateBlocked), Condition: "User resumes after resolving the cause or amending the policy (§6.3)", To: dstResume(), Event: domain.EventRunResumed},
	{ID: "run-24", From: anyNonterminal(), Condition: "Unknown external execution outcome", To: dst(domain.RunStateRecoveryRequired), Event: domain.EventRunSuspended},
	{ID: "run-25", From: src(domain.RunStateRecoveryRequired), Condition: "Reconciled (§12.1)", To: dstResume(), Event: domain.EventRunResumed},
	{ID: "run-26", From: anyNonterminal(), Condition: "Cancellation requested and all active Attempts terminated and reconciled", To: dst(domain.RunStateCancelled), Event: domain.EventRunCancelled},
})

var phaseRows = rows(MachinePhase, []Row{
	{ID: "phase-01", From: src(domain.PhaseStatePending), Condition: "All dependencies `Accepted`", To: dst(domain.PhaseStateReady), Event: domain.EventPhaseReady},
	{ID: "phase-02", From: src(domain.PhaseStateReady), Condition: "Slot and all required locks acquired; launch intent persisted", To: dst(domain.PhaseStateRunning), Event: domain.EventAttemptScheduled},
	{ID: "phase-03", From: src(domain.PhaseStateRunning), Condition: "Implementer reported `implemented` and its process was reaped", To: dst(domain.PhaseStateVerifying), Event: domain.EventPhaseVerificationStarted},
	{ID: "phase-04", From: src(domain.PhaseStateRunning), Condition: "Execution failed, output invalid, or a status other than `implemented` reported", To: dst(domain.PhaseStateRecovering), Event: domain.EventPhaseRecovering},
	{ID: "phase-05", From: src(domain.PhaseStateVerifying), Condition: "Checks passed, phase review required", To: dst(domain.PhaseStateEvaluating), Event: domain.EventPhaseEvaluationStarted},
	{ID: "phase-06", From: src(domain.PhaseStateVerifying), Condition: "Checks passed, review deferred to final evaluation", To: dst(domain.PhaseStateIntegrationPending), Event: domain.EventPhaseIntegrationQueued},
	{ID: "phase-07", From: src(domain.PhaseStateVerifying), Condition: "Change scope violated, or checks failed on the candidate snapshot (§10.2)", To: dst(domain.PhaseStateRecovering), Event: domain.EventPhaseRecovering},
	{ID: "phase-08", From: src(domain.PhaseStateEvaluating), Condition: "Accepted verdict received", To: dst(domain.PhaseStateIntegrationPending), Event: domain.EventPhaseIntegrationQueued},
	{ID: "phase-09", From: src(domain.PhaseStateEvaluating), Condition: "Rejected verdict received", To: dst(domain.PhaseStateRecovering), Event: domain.EventPhaseRecovering},
	{ID: "phase-10", From: src(domain.PhaseStateEvaluating), Condition: "Evaluator execution failed or returned invalid output, retry allowed", To: dstNote("new Evaluator Attempt", domain.PhaseStateEvaluating), Event: domain.EventPhaseEvaluationRetried},
	{ID: "phase-11", From: src(domain.PhaseStateEvaluating), Condition: "Evaluator retries exhausted", To: dst(domain.PhaseStateRecovering), Event: domain.EventPhaseRecovering},
	{ID: "phase-12", From: src(domain.PhaseStateIntegrationPending), Condition: "Selected by the serialized integration queue", To: dst(domain.PhaseStateIntegrating), Event: domain.EventPhaseIntegrationStarted},
	{ID: "phase-13", From: src(domain.PhaseStateIntegrating), Condition: "Integrated, post-integration evidence valid, integration ref advanced", To: dst(domain.PhaseStateAccepted), Event: domain.EventPhaseAccepted},
	{ID: "phase-14", From: src(domain.PhaseStateIntegrating), Condition: "Git conflict or post-integration checks failed", To: dst(domain.PhaseStateRecovering), Event: domain.EventPhaseRecovering},
	{ID: "phase-15", From: src(domain.PhaseStateRecovering), Condition: "Retry authorized", To: dstNote("new Attempt", domain.PhaseStateReady), Event: domain.EventPhaseRetryAuthorized},
	{ID: "phase-16", From: src(domain.PhaseStateRecovering), Condition: "The Phase cannot succeed under the current plan and needs replanning", To: dst(domain.PhaseStateFailed), Event: domain.EventPhaseFailed},
	{ID: "phase-17", From: src(domain.PhaseStateRunning, domain.PhaseStateEvaluating, domain.PhaseStateRecovering), Condition: "Validated question request", To: dst(domain.PhaseStateWaitingForUser), Event: domain.EventPhaseSuspended},
	{ID: "phase-18", From: src(domain.PhaseStateWaitingForUser), Condition: "Answers received", To: dstResume(), Event: domain.EventPhaseResumed},
	{ID: "phase-19", From: src(domain.PhaseStateRunning, domain.PhaseStateVerifying, domain.PhaseStateEvaluating, domain.PhaseStateIntegrating), Condition: "Unknown external execution outcome", To: dst(domain.PhaseStateRecoveryRequired), Event: domain.EventPhaseSuspended},
	{ID: "phase-20", From: src(domain.PhaseStateRecoveryRequired), Condition: "Reconciled (§12.1)", To: dstReconciliation(), Event: domain.EventPhaseReconciled},
	{ID: "phase-21", From: src(domain.PhaseStateAccepted), Condition: "A new Plan Revision does not carry the acceptance over", To: dst(domain.PhaseStateSuperseded), Event: domain.EventPhaseSuperseded},
	{ID: "phase-22", From: anyExcept(domain.PhaseStateAccepted, domain.PhaseStateSuperseded), Condition: "A new Plan Revision removes or redefines the Phase", To: dst(domain.PhaseStateSuperseded), Event: domain.EventPhaseSuperseded},
})

var attemptRows = rows(MachineAttempt, []Row{
	{ID: "attempt-01", From: src(domain.AttemptStateScheduled), Condition: "Launch confirmed", To: dst(domain.AttemptStateRunning), Event: domain.EventAttemptStarted},
	{ID: "attempt-02", From: src(domain.AttemptStateScheduled), Condition: "Launch did not occur (confirmed by reconciliation)", To: dst(domain.AttemptStateAbandoned), Event: domain.EventAttemptAbandoned},
	{ID: "attempt-03", From: src(domain.AttemptStateRunning), Condition: "Valid output received and process reaped", To: dst(domain.AttemptStateFinished), Event: domain.EventAttemptFinished},
	{ID: "attempt-04", From: src(domain.AttemptStateRunning), Condition: "Process failed or output invalid after permitted repairs", To: dst(domain.AttemptStateFailed), Event: domain.EventAttemptFailed},
	{ID: "attempt-05", From: src(domain.AttemptStateRunning), Condition: "Turn timeout", To: dst(domain.AttemptStateTimedOut), Event: domain.EventAttemptTimedOut},
	{ID: "attempt-06", From: src(domain.AttemptStateRunning), Condition: "Cancelled and process reaped", To: dst(domain.AttemptStateCancelled), Event: domain.EventAttemptCancelled},
	{ID: "attempt-07", From: src(domain.AttemptStateRunning), Condition: "Validated question request and process reaped", To: dst(domain.AttemptStateWaitingForUser), Event: domain.EventAttemptAwaitingInput},
	{ID: "attempt-08", From: src(domain.AttemptStateWaitingForUser), Condition: "Answers received and the session resumed (next Turn)", To: dst(domain.AttemptStateRunning), Event: domain.EventAttemptResumed},
	{ID: "attempt-09", From: src(domain.AttemptStateWaitingForUser), Condition: "Session cannot be restored", To: dstNote("a new Attempt continues, §6.1", domain.AttemptStateAbandoned), Event: domain.EventAttemptAbandoned},
	{ID: "attempt-10", From: src(domain.AttemptStateScheduled, domain.AttemptStateRunning), Condition: "Outcome cannot be determined", To: dst(domain.AttemptStateUnknown), Event: domain.EventAttemptOutcomeUnknown},
	{ID: "attempt-11", From: src(domain.AttemptStateUnknown), Condition: "Reconciled (§12.1)", To: dst(domain.AttemptStateFinished, domain.AttemptStateFailed, domain.AttemptStateAbandoned), Event: domain.EventAttemptReconciled},
})

// rows returns the rows of one machine with Machine filled in.
func rows(m Machine, rs []Row) []Row {
	for i := range rs {
		rs[i].Machine = m
	}
	return rs
}

func names[S ~string](states []S) []string {
	out := make([]string, len(states))
	for i, s := range states {
		out[i] = string(s)
	}
	return out
}

// src selects the listed states.
func src[S ~string](states ...S) Selector {
	return Selector{Kind: SelectStates, States: names(states)}
}

// anyNonterminal selects every nonterminal state.
func anyNonterminal() Selector { return Selector{Kind: SelectAnyNonterminal} }

// anyExcept selects every state except the listed ones.
func anyExcept[S ~string](states ...S) Selector {
	return Selector{Kind: SelectAnyExcept, States: names(states)}
}

// dst targets the listed states.
func dst[S ~string](states ...S) Target {
	return Target{Kind: TargetStates, States: names(states)}
}

// dstNote targets the listed states and carries the parenthetical note of the
// SPEC cell.
func dstNote[S ~string](note string, states ...S) Target {
	return Target{Kind: TargetStates, States: names(states), Note: note}
}

// dstResume targets the recorded resume state.
func dstResume() Target { return Target{Kind: TargetResume} }

// dstReconciliation targets the state determined by reconciliation.
func dstReconciliation() Target { return Target{Kind: TargetReconciliation} }
