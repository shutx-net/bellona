package domain

import (
	"fmt"
	"strings"
)

// The M0 event catalogue: one payload type per event type, named exactly like
// the event type. A field is optional if and only if its json tag has
// omitzero (the convention of package strictjson); every other field is
// required. Text fields are valid UTF-8 of at most MaxTextBytes bytes, a
// revision is 1 or more unless stated otherwise, and every artifact reference
// validates when it is present.
//
// Later milestones add fields to these payloads, for example the effort and
// model of AttemptScheduled (M2) and the base commit and baseline evidence of
// RunPrepared (M1b). M0 defines only what it can fill.
//
// The names UserInputRequested and UserInputReceived of the example in SPEC
// §12 are reserved for M2 and are not defined here.

// The event types of the catalogue. The comment on each payload type says
// which row of the transition tables in SPEC §6 the event commits.
const (
	EventRunCreated               EventType = "RunCreated"
	EventManifestFrozen           EventType = "ManifestFrozen"
	EventPolicyAmended            EventType = "PolicyAmended"
	EventAttemptScheduled         EventType = "AttemptScheduled"
	EventRunPrepared              EventType = "RunPrepared"
	EventStrategySelected         EventType = "StrategySelected"
	EventPlanGenerated            EventType = "PlanGenerated"
	EventPlanValidationFailed     EventType = "PlanValidationFailed"
	EventPlanApprovalRequested    EventType = "PlanApprovalRequested"
	EventPlanAccepted             EventType = "PlanAccepted"
	EventPlanChangesRequested     EventType = "PlanChangesRequested"
	EventFinalVerificationStarted EventType = "FinalVerificationStarted"
	EventReplanningStarted        EventType = "ReplanningStarted"
	EventFinalEvaluationStarted   EventType = "FinalEvaluationStarted"
	EventRunCompleted             EventType = "RunCompleted"
	EventFinalEvaluationRetried   EventType = "FinalEvaluationRetried"
	EventRunSuspended             EventType = "RunSuspended"
	EventRunResumed               EventType = "RunResumed"
	EventRunCancelled             EventType = "RunCancelled"
	EventPhaseReady               EventType = "PhaseReady"
	EventPhaseVerificationStarted EventType = "PhaseVerificationStarted"
	EventPhaseRecovering          EventType = "PhaseRecovering"
	EventPhaseEvaluationStarted   EventType = "PhaseEvaluationStarted"
	EventPhaseIntegrationQueued   EventType = "PhaseIntegrationQueued"
	EventPhaseEvaluationRetried   EventType = "PhaseEvaluationRetried"
	EventPhaseIntegrationStarted  EventType = "PhaseIntegrationStarted"
	EventPhaseAccepted            EventType = "PhaseAccepted"
	EventPhaseRetryAuthorized     EventType = "PhaseRetryAuthorized"
	EventPhaseFailed              EventType = "PhaseFailed"
	EventPhaseSuspended           EventType = "PhaseSuspended"
	EventPhaseResumed             EventType = "PhaseResumed"
	EventPhaseReconciled          EventType = "PhaseReconciled"
	EventPhaseSuperseded          EventType = "PhaseSuperseded"
	EventAttemptStarted           EventType = "AttemptStarted"
	EventAttemptAbandoned         EventType = "AttemptAbandoned"
	EventAttemptFinished          EventType = "AttemptFinished"
	EventAttemptFailed            EventType = "AttemptFailed"
	EventAttemptTimedOut          EventType = "AttemptTimedOut"
	EventAttemptCancelled         EventType = "AttemptCancelled"
	EventAttemptAwaitingInput     EventType = "AttemptAwaitingInput"
	EventAttemptResumed           EventType = "AttemptResumed"
	EventAttemptOutcomeUnknown    EventType = "AttemptOutcomeUnknown"
	EventAttemptReconciled        EventType = "AttemptReconciled"
)

// catalogue lists the payload types in catalogue order: the events that no
// table row commits, the Run events, the Phase events and the Attempt events,
// each group in the order of its table. AttemptScheduled is committed by the
// Phase table (row phase-02) although it also creates Attempts of roles that
// belong to no Phase.
var catalogue = []payloadEntry{
	entryOf[RunCreated](),
	entryOf[ManifestFrozen](),
	entryOf[PolicyAmended](),
	entryOf[AttemptScheduled](),

	entryOf[RunPrepared](),
	entryOf[StrategySelected](),
	entryOf[PlanGenerated](),
	entryOf[PlanValidationFailed](),
	entryOf[PlanApprovalRequested](),
	entryOf[PlanAccepted](),
	entryOf[PlanChangesRequested](),
	entryOf[FinalVerificationStarted](),
	entryOf[ReplanningStarted](),
	entryOf[FinalEvaluationStarted](),
	entryOf[RunCompleted](),
	entryOf[FinalEvaluationRetried](),
	entryOf[RunSuspended](),
	entryOf[RunResumed](),
	entryOf[RunCancelled](),

	entryOf[PhaseReady](),
	entryOf[PhaseVerificationStarted](),
	entryOf[PhaseRecovering](),
	entryOf[PhaseEvaluationStarted](),
	entryOf[PhaseIntegrationQueued](),
	entryOf[PhaseEvaluationRetried](),
	entryOf[PhaseIntegrationStarted](),
	entryOf[PhaseAccepted](),
	entryOf[PhaseRetryAuthorized](),
	entryOf[PhaseFailed](),
	entryOf[PhaseSuspended](),
	entryOf[PhaseResumed](),
	entryOf[PhaseReconciled](),
	entryOf[PhaseSuperseded](),

	entryOf[AttemptStarted](),
	entryOf[AttemptAbandoned](),
	entryOf[AttemptFinished](),
	entryOf[AttemptFailed](),
	entryOf[AttemptTimedOut](),
	entryOf[AttemptCancelled](),
	entryOf[AttemptAwaitingInput](),
	entryOf[AttemptResumed](),
	entryOf[AttemptOutcomeUnknown](),
	entryOf[AttemptReconciled](),
}

// ---------------------------------------------------------------------------
// Validation helpers
// ---------------------------------------------------------------------------

// refs returns the non-zero references among rs, or nil.
func refs(rs ...ArtifactRef) []ArtifactRef {
	var out []ArtifactRef
	for _, r := range rs {
		if !r.IsZero() {
			out = append(out, r)
		}
	}
	return out
}

// checkRef validates a required reference.
func checkRef(pointer string, r ArtifactRef) error {
	if err := r.Validate(); err != nil {
		return fieldError(pointer, err)
	}
	return nil
}

// checkOptionalRef validates a reference that may be absent (zero).
func checkOptionalRef(pointer string, r ArtifactRef) error {
	if r.IsZero() {
		return nil
	}
	return checkRef(pointer, r)
}

// checkFixedPath validates a required reference that must have a fixed path.
func checkFixedPath(pointer string, r ArtifactRef, want string) error {
	if err := checkRef(pointer, r); err != nil {
		return err
	}
	if r.Path != want {
		return invalidField(pointer+"/path", "%q, want %q", r.Path, want)
	}
	return nil
}

// validatableID is the method set shared by the identifier types.
type validatableID interface {
	~string
	Validate() error
}

// requireID validates a required identifier.
func requireID[T validatableID](pointer string, id T) error {
	if err := id.Validate(); err != nil {
		return fieldError(pointer, err)
	}
	return nil
}

// optionalID validates an identifier that may be absent (empty).
func optionalID[T validatableID](pointer string, id T) error {
	if id == "" {
		return nil
	}
	return requireID(pointer, id)
}

// checkRevision validates a Plan Revision number.
func checkRevision(pointer string, revision int) error {
	if revision < 1 {
		return invalidField(pointer, "revision %d, want 1 or more", revision)
	}
	return nil
}

// checkPhaseRef validates the revision and the Phase ID that every Phase
// event carries.
func checkPhaseRef(revision int, id PhaseID) error {
	if err := checkRevision("/revision", revision); err != nil {
		return err
	}
	return requireID("/phaseId", id)
}

// checkRequestPresence enforces that a request ID is present if and only if
// the suspended state is WaitingForUser.
func checkRequestPresence(waiting bool, id RequestID) error {
	if waiting {
		if id == "" {
			return invalidField("/requestId", "required when the target state is WaitingForUser")
		}
	} else if id != "" {
		return invalidField("/requestId", "allowed only when the target state is WaitingForUser")
	}
	return optionalID("/requestId", id)
}

// ---------------------------------------------------------------------------
// Events that no row of the transition tables commits
// ---------------------------------------------------------------------------

// RunCreated is the first event of a Run (sequence number 1). It records the
// task artifact, task.json.
type RunCreated struct {
	Task ArtifactRef `json:"task"`
}

func (RunCreated) EventType() EventType          { return EventRunCreated }
func (p RunCreated) ArtifactRefs() []ArtifactRef { return refs(p.Task) }
func (p RunCreated) Validate() error             { return checkFixedPath("/task", p.Task, TaskPath) }

// ManifestFrozen is the second event of a Run. It records the frozen Run
// manifest and policy artifacts and the execution limits in force at the
// start (SPEC §12.4, §6.3).
type ManifestFrozen struct {
	Manifest ArtifactRef `json:"manifest"`
	Policy   ArtifactRef `json:"policy"`
	Limits   Limits      `json:"limits"`
}

func (ManifestFrozen) EventType() EventType          { return EventManifestFrozen }
func (p ManifestFrozen) ArtifactRefs() []ArtifactRef { return refs(p.Manifest, p.Policy) }
func (p ManifestFrozen) Validate() error {
	if err := checkFixedPath("/manifest", p.Manifest, ManifestPath); err != nil {
		return err
	}
	if err := checkFixedPath("/policy", p.Policy, PolicyPath); err != nil {
		return err
	}
	if err := p.Limits.Validate(); err != nil {
		return fieldError("/limits", err)
	}
	return nil
}

// PolicyAmended records a user policy amendment of execution limits (SPEC
// §6.3): for each changed limit the previous and the new canonical text (see
// Limits.Value). It has one to five entries, with distinct limits, and every
// value must be the canonical text of a valid value of its limit.
type PolicyAmended struct {
	Changes []LimitChange `json:"changes"`
}

func (PolicyAmended) EventType() EventType        { return EventPolicyAmended }
func (PolicyAmended) ArtifactRefs() []ArtifactRef { return nil }
func (p PolicyAmended) Validate() error {
	if n := len(p.Changes); n < 1 || n > len(limitNames) {
		return invalidField("/changes", "%d entries, want 1 to %d", n, len(limitNames))
	}
	seen := make(map[LimitName]bool, len(p.Changes))
	for i, c := range p.Changes {
		at := fmt.Sprintf("/changes/%d", i)
		switch {
		case !c.Limit.Valid():
			return invalidField(at+"/limit", "%q is not a limit name", string(c.Limit))
		case seen[c.Limit]:
			return invalidField(at+"/limit", "%s is amended twice", c.Limit)
		case c.Previous == c.New:
			return invalidField(at+"/new", "%s is %q before and after", c.Limit, c.New)
		}
		seen[c.Limit] = true
		if _, err := (Limits{}).With(c.Limit, c.Previous); err != nil {
			return fieldError(at+"/previous", err)
		}
		if _, err := (Limits{}).With(c.Limit, c.New); err != nil {
			return fieldError(at+"/new", err)
		}
	}
	return nil
}

// AttemptScheduled persists the launch intent of an Attempt before the agent
// is launched (SPEC §12, Crash recovery). The Attempt starts in Scheduled.
//
// The Phase table commits it (row phase-02, Ready to Running) when the role
// is an Implementer. An Implementer requires the Phase and the Plan Revision.
// An Evaluator has both (a phase review) or neither (the final evaluation of
// the Run). A Planner and a Coordinator have no Phase and an optional
// revision. A zero revision is absent.
type AttemptScheduled struct {
	AttemptID AttemptID `json:"attemptId"`
	Role      Role      `json:"role"`
	Revision  int       `json:"revision,omitzero"`
	PhaseID   PhaseID   `json:"phaseId,omitzero"`
	SessionID SessionID `json:"sessionId,omitzero"`
}

func (AttemptScheduled) EventType() EventType        { return EventAttemptScheduled }
func (AttemptScheduled) ArtifactRefs() []ArtifactRef { return nil }
func (p AttemptScheduled) Validate() error {
	if err := requireID("/attemptId", p.AttemptID); err != nil {
		return err
	}
	if err := p.Role.Validate(); err != nil {
		return fieldError("/role", err)
	}
	if p.Revision < 0 {
		return invalidField("/revision", "revision %d, want 1 or more or absent", p.Revision)
	}
	if err := optionalID("/phaseId", p.PhaseID); err != nil {
		return err
	}
	if err := optionalID("/sessionId", p.SessionID); err != nil {
		return err
	}
	switch p.Role {
	case RoleImplementer:
		if p.PhaseID == "" || p.Revision == 0 {
			return invalidField("", "an implementer Attempt requires phaseId and revision")
		}
	case RoleEvaluator:
		if (p.PhaseID == "") != (p.Revision == 0) {
			return invalidField("", "an evaluator Attempt has both phaseId and revision (a phase review) or neither (the final evaluation)")
		}
	default:
		if p.PhaseID != "" {
			return invalidField("/phaseId", "a %s Attempt belongs to no Phase", p.Role)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Run transitions (table in SPEC §6, Run states)
// ---------------------------------------------------------------------------

// RunPrepared commits Created to StrategySelection (run-01): the manifest is
// frozen, the workspaces are prepared and the baseline checks passed.
type RunPrepared struct{}

func (RunPrepared) EventType() EventType        { return EventRunPrepared }
func (RunPrepared) ArtifactRefs() []ArtifactRef { return nil }
func (RunPrepared) Validate() error             { return nil }

// StrategySelected commits StrategySelection to Planning (run-03). The
// decision is the validated Coordinator decision, absent when no decision was
// delegated.
type StrategySelected struct {
	Decision ArtifactRef `json:"decision,omitzero"`
}

func (StrategySelected) EventType() EventType          { return EventStrategySelected }
func (p StrategySelected) ArtifactRefs() []ArtifactRef { return refs(p.Decision) }
func (p StrategySelected) Validate() error             { return checkOptionalRef("/decision", p.Decision) }

// PlanGenerated commits Planning to PlanValidation (run-04): a plan candidate
// was received from the Planner Attempt, stored as the artifact.
type PlanGenerated struct {
	AttemptID AttemptID   `json:"attemptId"`
	Artifact  ArtifactRef `json:"artifact"`
}

func (PlanGenerated) EventType() EventType          { return EventPlanGenerated }
func (p PlanGenerated) ArtifactRefs() []ArtifactRef { return refs(p.Artifact) }
func (p PlanGenerated) Validate() error {
	if err := requireID("/attemptId", p.AttemptID); err != nil {
		return err
	}
	return checkRef("/artifact", p.Artifact)
}

// PlanValidationFailed commits PlanValidation to Planning (run-05): the
// candidate is invalid and a retry is allowed. The summary is not empty; the
// report is the optional validation report artifact.
type PlanValidationFailed struct {
	Summary string      `json:"summary"`
	Report  ArtifactRef `json:"report,omitzero"`
}

func (PlanValidationFailed) EventType() EventType          { return EventPlanValidationFailed }
func (p PlanValidationFailed) ArtifactRefs() []ArtifactRef { return refs(p.Report) }
func (p PlanValidationFailed) Validate() error {
	if strings.TrimSpace(p.Summary) == "" {
		return invalidField("/summary", "the summary is empty")
	}
	if err := checkText("/summary", p.Summary); err != nil {
		return err
	}
	return checkOptionalRef("/report", p.Report)
}

// PlanApprovalRequested commits PlanValidation to WaitingApproval (run-06).
// The artifact is the plan that the approval is bound to (SPEC §6.2).
type PlanApprovalRequested struct {
	Artifact ArtifactRef `json:"artifact"`
}

func (PlanApprovalRequested) EventType() EventType          { return EventPlanApprovalRequested }
func (p PlanApprovalRequested) ArtifactRefs() []ArtifactRef { return refs(p.Artifact) }
func (p PlanApprovalRequested) Validate() error             { return checkRef("/artifact", p.Artifact) }

// maxPlanPhases is the largest number of Phases that one plan revision may
// define.
const maxPlanPhases = 256

// PhaseDefinition is the part of a Phase that the journal records: its ID and
// the IDs of the Phases it depends on. The rest of the definition is in the
// plan artifact.
type PhaseDefinition struct {
	ID        PhaseID   `json:"id"`
	DependsOn []PhaseID `json:"dependsOn"`
}

// PlanAccepted commits PlanValidation to Executing (run-07) or WaitingApproval
// to Executing (run-08): a plan revision is accepted. The plan artifact is at
// PlanPath(revision). Phases lists the Phases of the revision in plan order,
// with their dependencies; one to 256 Phases with distinct IDs, each
// dependency naming another listed Phase, none repeated, and no cycle.
type PlanAccepted struct {
	Revision int               `json:"revision"`
	Plan     ArtifactRef       `json:"plan"`
	Phases   []PhaseDefinition `json:"phases"`
}

func (PlanAccepted) EventType() EventType          { return EventPlanAccepted }
func (p PlanAccepted) ArtifactRefs() []ArtifactRef { return refs(p.Plan) }
func (p PlanAccepted) Validate() error {
	if err := checkRevision("/revision", p.Revision); err != nil {
		return err
	}
	if err := checkFixedPath("/plan", p.Plan, PlanPath(p.Revision)); err != nil {
		return err
	}
	return checkPhaseGraph("/phases", p.Phases)
}

// checkPhaseGraph validates the Phase definitions of a plan revision.
func checkPhaseGraph(pointer string, phases []PhaseDefinition) error {
	if n := len(phases); n < 1 || n > maxPlanPhases {
		return invalidField(pointer, "%d phases, want 1 to %d", n, maxPlanPhases)
	}
	index := make(map[PhaseID]int, len(phases))
	for i, ph := range phases {
		at := fmt.Sprintf("%s/%d", pointer, i)
		if err := requireID(at+"/id", ph.ID); err != nil {
			return err
		}
		if _, dup := index[ph.ID]; dup {
			return invalidField(at+"/id", "phase %q is defined twice", string(ph.ID))
		}
		index[ph.ID] = i
	}
	pending := make([]int, len(phases)) // unresolved dependencies of each phase
	dependents := make([][]int, len(phases))
	for i, ph := range phases {
		at := fmt.Sprintf("%s/%d/dependsOn", pointer, i)
		seen := make(map[PhaseID]bool, len(ph.DependsOn))
		for j, dep := range ph.DependsOn {
			depAt := fmt.Sprintf("%s/%d", at, j)
			if err := requireID(depAt, dep); err != nil {
				return err
			}
			k, known := index[dep]
			switch {
			case dep == ph.ID:
				return invalidField(depAt, "phase %q depends on itself", string(dep))
			case !known:
				return invalidField(depAt, "phase %q depends on %q, which is not defined", string(ph.ID), string(dep))
			case seen[dep]:
				return invalidField(depAt, "phase %q lists %q twice", string(ph.ID), string(dep))
			}
			seen[dep] = true
			pending[i]++
			dependents[k] = append(dependents[k], i)
		}
	}
	// Kahn's algorithm: whatever cannot be ordered lies on or behind a cycle.
	var ready []int
	for i, n := range pending {
		if n == 0 {
			ready = append(ready, i)
		}
	}
	ordered := 0
	for len(ready) > 0 {
		i := ready[len(ready)-1]
		ready = ready[:len(ready)-1]
		ordered++
		for _, d := range dependents[i] {
			if pending[d]--; pending[d] == 0 {
				ready = append(ready, d)
			}
		}
	}
	if ordered != len(phases) {
		for i, n := range pending {
			if n > 0 {
				return invalidField(fmt.Sprintf("%s/%d", pointer, i), "phase %q is part of, or depends on, a dependency cycle", string(phases[i].ID))
			}
		}
	}
	return nil
}

// PlanChangesRequested commits WaitingApproval to Planning (run-09): the user
// asked for changes. The feedback is the artifact that holds it.
type PlanChangesRequested struct {
	Feedback ArtifactRef `json:"feedback"`
}

func (PlanChangesRequested) EventType() EventType          { return EventPlanChangesRequested }
func (p PlanChangesRequested) ArtifactRefs() []ArtifactRef { return refs(p.Feedback) }
func (p PlanChangesRequested) Validate() error             { return checkRef("/feedback", p.Feedback) }

// FinalVerificationStarted commits Executing to FinalVerification (run-11).
type FinalVerificationStarted struct{}

func (FinalVerificationStarted) EventType() EventType        { return EventFinalVerificationStarted }
func (FinalVerificationStarted) ArtifactRefs() []ArtifactRef { return nil }
func (FinalVerificationStarted) Validate() error             { return nil }

// ReplanningStarted commits a return to Planning for a new Plan Revision:
// Executing (run-12), FinalVerification (run-16) or FinalEvaluation (run-18).
// The cause says which one applied; it determines the source state.
type ReplanningStarted struct {
	Cause ReplanCause `json:"cause"`
}

func (ReplanningStarted) EventType() EventType        { return EventReplanningStarted }
func (ReplanningStarted) ArtifactRefs() []ArtifactRef { return nil }
func (p ReplanningStarted) Validate() error {
	if !p.Cause.Valid() {
		return invalidField("/cause", "%q is not a replan cause", string(p.Cause))
	}
	return nil
}

// FinalEvaluationStarted commits FinalVerification to FinalEvaluation
// (run-14).
type FinalEvaluationStarted struct{}

func (FinalEvaluationStarted) EventType() EventType        { return EventFinalEvaluationStarted }
func (FinalEvaluationStarted) ArtifactRefs() []ArtifactRef { return nil }
func (FinalEvaluationStarted) Validate() error             { return nil }

// RunCompleted commits FinalVerification to Completed (run-15) or
// FinalEvaluation to Completed (run-17).
type RunCompleted struct{}

func (RunCompleted) EventType() EventType        { return EventRunCompleted }
func (RunCompleted) ArtifactRefs() []ArtifactRef { return nil }
func (RunCompleted) Validate() error             { return nil }

// FinalEvaluationRetried commits FinalEvaluation to FinalEvaluation (run-19):
// the final Evaluator Attempt failed or returned invalid output and a new
// Attempt follows.
type FinalEvaluationRetried struct {
	FailedAttemptID AttemptID `json:"failedAttemptId"`
}

func (FinalEvaluationRetried) EventType() EventType        { return EventFinalEvaluationRetried }
func (FinalEvaluationRetried) ArtifactRefs() []ArtifactRef { return nil }
func (p FinalEvaluationRetried) Validate() error {
	return requireID("/failedAttemptId", p.FailedAttemptID)
}

// RunSuspended commits a suspension of the Run: to WaitingForUser (run-20),
// Blocked (run-02, run-13, run-22) or RecoveryRequired (run-24). To is the
// suspended state, Reason determines it (see SuspendReason.Target), and
// ResumeState is the state that a later RunResumed returns to: valid, not
// terminal and different from To. RequestID is present if and only if To is
// WaitingForUser. Detail is optional text.
type RunSuspended struct {
	To          RunState      `json:"to"`
	Reason      SuspendReason `json:"reason"`
	ResumeState RunState      `json:"resumeState"`
	Detail      string        `json:"detail,omitzero"`
	RequestID   RequestID     `json:"requestId,omitzero"`
}

func (RunSuspended) EventType() EventType        { return EventRunSuspended }
func (RunSuspended) ArtifactRefs() []ArtifactRef { return nil }
func (p RunSuspended) Validate() error {
	switch {
	case !p.To.Suspended():
		return invalidField("/to", "%q is not a suspended Run state", string(p.To))
	case !p.Reason.Valid():
		return invalidField("/reason", "%q is not a suspend reason", string(p.Reason))
	case p.Reason.Target() != p.To:
		return invalidField("/reason", "reason %s leads to %s, not to %s", p.Reason, p.Reason.Target(), p.To)
	case !p.ResumeState.Valid():
		return invalidField("/resumeState", "%q is not a Run state", string(p.ResumeState))
	case p.ResumeState.Terminal():
		return invalidField("/resumeState", "a Run cannot resume to the terminal state %s", p.ResumeState)
	case p.ResumeState == p.To:
		return invalidField("/resumeState", "the resume state equals the suspended state %s", p.To)
	}
	if err := checkText("/detail", p.Detail); err != nil {
		return err
	}
	return checkRequestPresence(p.To == RunStateWaitingForUser, p.RequestID)
}

// RunResumed commits the return from a suspended state to the recorded resume
// state (run-21, run-23, run-25). To is that state: valid and not terminal.
// RequestID is the answered request, when the Run was WaitingForUser.
type RunResumed struct {
	To        RunState  `json:"to"`
	RequestID RequestID `json:"requestId,omitzero"`
}

func (RunResumed) EventType() EventType        { return EventRunResumed }
func (RunResumed) ArtifactRefs() []ArtifactRef { return nil }
func (p RunResumed) Validate() error {
	switch {
	case !p.To.Valid():
		return invalidField("/to", "%q is not a Run state", string(p.To))
	case p.To.Terminal():
		return invalidField("/to", "a Run cannot resume to the terminal state %s", p.To)
	}
	return optionalID("/requestId", p.RequestID)
}

// RunCancelled commits the cancellation of the Run (run-10, run-26).
type RunCancelled struct {
	Reason CancelReason `json:"reason"`
}

func (RunCancelled) EventType() EventType        { return EventRunCancelled }
func (RunCancelled) ArtifactRefs() []ArtifactRef { return nil }
func (p RunCancelled) Validate() error {
	if !p.Reason.Valid() {
		return invalidField("/reason", "%q is not a cancel reason", string(p.Reason))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Phase transitions (table in SPEC §6, Phase states)
//
// Every payload names the Phase by its Plan Revision and its ID. The types
// repeat the two fields because a contract type cannot embed another struct.
// ---------------------------------------------------------------------------

// PhaseReady commits Pending to Ready (phase-01): all dependencies are
// Accepted.
type PhaseReady struct {
	Revision int     `json:"revision"`
	PhaseID  PhaseID `json:"phaseId"`
}

func (PhaseReady) EventType() EventType        { return EventPhaseReady }
func (PhaseReady) ArtifactRefs() []ArtifactRef { return nil }
func (p PhaseReady) Validate() error           { return checkPhaseRef(p.Revision, p.PhaseID) }

// PhaseVerificationStarted commits Running to Verifying (phase-03): the
// Implementer Attempt reported implemented and its process was reaped.
type PhaseVerificationStarted struct {
	Revision  int       `json:"revision"`
	PhaseID   PhaseID   `json:"phaseId"`
	AttemptID AttemptID `json:"attemptId"`
}

func (PhaseVerificationStarted) EventType() EventType        { return EventPhaseVerificationStarted }
func (PhaseVerificationStarted) ArtifactRefs() []ArtifactRef { return nil }
func (p PhaseVerificationStarted) Validate() error {
	if err := checkPhaseRef(p.Revision, p.PhaseID); err != nil {
		return err
	}
	return requireID("/attemptId", p.AttemptID)
}

// PhaseRecovering commits a Phase to Recovering: from Running (phase-04),
// Verifying (phase-07), Evaluating (phase-09, phase-11) or Integrating
// (phase-14). The cause records which condition applied and determines the
// source state (see PhaseRecoveryCause.Source).
type PhaseRecovering struct {
	Revision int                `json:"revision"`
	PhaseID  PhaseID            `json:"phaseId"`
	Cause    PhaseRecoveryCause `json:"cause"`
}

func (PhaseRecovering) EventType() EventType        { return EventPhaseRecovering }
func (PhaseRecovering) ArtifactRefs() []ArtifactRef { return nil }
func (p PhaseRecovering) Validate() error {
	if err := checkPhaseRef(p.Revision, p.PhaseID); err != nil {
		return err
	}
	if !p.Cause.Valid() {
		return invalidField("/cause", "%q is not a phase recovery cause", string(p.Cause))
	}
	return nil
}

// PhaseEvaluationStarted commits Verifying to Evaluating (phase-05): the
// checks passed and a phase review is required.
type PhaseEvaluationStarted struct {
	Revision int     `json:"revision"`
	PhaseID  PhaseID `json:"phaseId"`
}

func (PhaseEvaluationStarted) EventType() EventType        { return EventPhaseEvaluationStarted }
func (PhaseEvaluationStarted) ArtifactRefs() []ArtifactRef { return nil }
func (p PhaseEvaluationStarted) Validate() error           { return checkPhaseRef(p.Revision, p.PhaseID) }

// PhaseIntegrationQueued commits Verifying to IntegrationPending (phase-06,
// review deferred to the final evaluation) or Evaluating to IntegrationPending
// (phase-08, accepted verdict).
type PhaseIntegrationQueued struct {
	Revision int     `json:"revision"`
	PhaseID  PhaseID `json:"phaseId"`
}

func (PhaseIntegrationQueued) EventType() EventType        { return EventPhaseIntegrationQueued }
func (PhaseIntegrationQueued) ArtifactRefs() []ArtifactRef { return nil }
func (p PhaseIntegrationQueued) Validate() error           { return checkPhaseRef(p.Revision, p.PhaseID) }

// PhaseEvaluationRetried commits Evaluating to Evaluating (phase-10): the
// Evaluator Attempt failed or returned invalid output and a new Evaluator
// Attempt follows.
type PhaseEvaluationRetried struct {
	Revision        int       `json:"revision"`
	PhaseID         PhaseID   `json:"phaseId"`
	FailedAttemptID AttemptID `json:"failedAttemptId"`
}

func (PhaseEvaluationRetried) EventType() EventType        { return EventPhaseEvaluationRetried }
func (PhaseEvaluationRetried) ArtifactRefs() []ArtifactRef { return nil }
func (p PhaseEvaluationRetried) Validate() error {
	if err := checkPhaseRef(p.Revision, p.PhaseID); err != nil {
		return err
	}
	return requireID("/failedAttemptId", p.FailedAttemptID)
}

// PhaseIntegrationStarted commits IntegrationPending to Integrating
// (phase-12).
type PhaseIntegrationStarted struct {
	Revision int     `json:"revision"`
	PhaseID  PhaseID `json:"phaseId"`
}

func (PhaseIntegrationStarted) EventType() EventType        { return EventPhaseIntegrationStarted }
func (PhaseIntegrationStarted) ArtifactRefs() []ArtifactRef { return nil }
func (p PhaseIntegrationStarted) Validate() error           { return checkPhaseRef(p.Revision, p.PhaseID) }

// PhaseAccepted commits Integrating to Accepted (phase-13). The Attempt is the
// Implementer Attempt whose result was integrated.
type PhaseAccepted struct {
	Revision  int       `json:"revision"`
	PhaseID   PhaseID   `json:"phaseId"`
	AttemptID AttemptID `json:"attemptId"`
}

func (PhaseAccepted) EventType() EventType        { return EventPhaseAccepted }
func (PhaseAccepted) ArtifactRefs() []ArtifactRef { return nil }
func (p PhaseAccepted) Validate() error {
	if err := checkPhaseRef(p.Revision, p.PhaseID); err != nil {
		return err
	}
	return requireID("/attemptId", p.AttemptID)
}

// PhaseRetryAuthorized commits Recovering to Ready (phase-15): a retry is
// authorized and a new Attempt follows.
type PhaseRetryAuthorized struct {
	Revision int     `json:"revision"`
	PhaseID  PhaseID `json:"phaseId"`
}

func (PhaseRetryAuthorized) EventType() EventType        { return EventPhaseRetryAuthorized }
func (PhaseRetryAuthorized) ArtifactRefs() []ArtifactRef { return nil }
func (p PhaseRetryAuthorized) Validate() error           { return checkPhaseRef(p.Revision, p.PhaseID) }

// PhaseFailed commits Recovering to Failed (phase-16): the Phase cannot
// succeed under the current plan and needs replanning.
type PhaseFailed struct {
	Revision int     `json:"revision"`
	PhaseID  PhaseID `json:"phaseId"`
}

func (PhaseFailed) EventType() EventType        { return EventPhaseFailed }
func (PhaseFailed) ArtifactRefs() []ArtifactRef { return nil }
func (p PhaseFailed) Validate() error           { return checkPhaseRef(p.Revision, p.PhaseID) }

// PhaseSuspended commits a Phase to WaitingForUser (phase-17) or
// RecoveryRequired (phase-19). ResumeState is the state to return to: valid
// and different from To. RequestID is present if and only if To is
// WaitingForUser.
type PhaseSuspended struct {
	Revision    int        `json:"revision"`
	PhaseID     PhaseID    `json:"phaseId"`
	To          PhaseState `json:"to"`
	ResumeState PhaseState `json:"resumeState"`
	RequestID   RequestID  `json:"requestId,omitzero"`
}

func (PhaseSuspended) EventType() EventType        { return EventPhaseSuspended }
func (PhaseSuspended) ArtifactRefs() []ArtifactRef { return nil }
func (p PhaseSuspended) Validate() error {
	if err := checkPhaseRef(p.Revision, p.PhaseID); err != nil {
		return err
	}
	switch {
	case p.To != PhaseStateWaitingForUser && p.To != PhaseStateRecoveryRequired:
		return invalidField("/to", "%q is not WaitingForUser or RecoveryRequired", string(p.To))
	case !p.ResumeState.Valid():
		return invalidField("/resumeState", "%q is not a Phase state", string(p.ResumeState))
	case p.ResumeState == p.To:
		return invalidField("/resumeState", "the resume state equals the suspended state %s", p.To)
	}
	return checkRequestPresence(p.To == PhaseStateWaitingForUser, p.RequestID)
}

// PhaseResumed commits the return of a Phase from WaitingForUser to the
// recorded resume state (phase-18).
type PhaseResumed struct {
	Revision int        `json:"revision"`
	PhaseID  PhaseID    `json:"phaseId"`
	To       PhaseState `json:"to"`
}

func (PhaseResumed) EventType() EventType        { return EventPhaseResumed }
func (PhaseResumed) ArtifactRefs() []ArtifactRef { return nil }
func (p PhaseResumed) Validate() error {
	if err := checkPhaseRef(p.Revision, p.PhaseID); err != nil {
		return err
	}
	if !p.To.Valid() {
		return invalidField("/to", "%q is not a Phase state", string(p.To))
	}
	return nil
}

// PhaseReconciled commits the exit of a Phase from RecoveryRequired to the
// state that reconciliation determined (phase-20).
type PhaseReconciled struct {
	Revision int        `json:"revision"`
	PhaseID  PhaseID    `json:"phaseId"`
	To       PhaseState `json:"to"`
}

func (PhaseReconciled) EventType() EventType        { return EventPhaseReconciled }
func (PhaseReconciled) ArtifactRefs() []ArtifactRef { return nil }
func (p PhaseReconciled) Validate() error {
	if err := checkPhaseRef(p.Revision, p.PhaseID); err != nil {
		return err
	}
	if !p.To.Valid() {
		return invalidField("/to", "%q is not a Phase state", string(p.To))
	}
	return nil
}

// PhaseSuperseded commits a Phase to Superseded (phase-21, phase-22): a new
// Plan Revision removes, redefines or does not carry over the Phase. The
// revision is the one of the Phase that is superseded.
type PhaseSuperseded struct {
	Revision int     `json:"revision"`
	PhaseID  PhaseID `json:"phaseId"`
}

func (PhaseSuperseded) EventType() EventType        { return EventPhaseSuperseded }
func (PhaseSuperseded) ArtifactRefs() []ArtifactRef { return nil }
func (p PhaseSuperseded) Validate() error           { return checkPhaseRef(p.Revision, p.PhaseID) }

// ---------------------------------------------------------------------------
// Attempt transitions (table in SPEC §6, Attempt states)
// ---------------------------------------------------------------------------

// AttemptStarted commits Scheduled to Running (attempt-01): the launch is
// confirmed. SessionID is the session that the host reported, when it was not
// assigned before launch.
type AttemptStarted struct {
	AttemptID AttemptID `json:"attemptId"`
	SessionID SessionID `json:"sessionId,omitzero"`
}

func (AttemptStarted) EventType() EventType        { return EventAttemptStarted }
func (AttemptStarted) ArtifactRefs() []ArtifactRef { return nil }
func (p AttemptStarted) Validate() error {
	if err := requireID("/attemptId", p.AttemptID); err != nil {
		return err
	}
	return optionalID("/sessionId", p.SessionID)
}

// AttemptAbandoned commits an Attempt to Abandoned: from Scheduled
// (attempt-02) or WaitingForUser (attempt-09). The reason determines the
// source state (see AbandonReason.Source).
type AttemptAbandoned struct {
	AttemptID AttemptID     `json:"attemptId"`
	Reason    AbandonReason `json:"reason"`
}

func (AttemptAbandoned) EventType() EventType        { return EventAttemptAbandoned }
func (AttemptAbandoned) ArtifactRefs() []ArtifactRef { return nil }
func (p AttemptAbandoned) Validate() error {
	if err := requireID("/attemptId", p.AttemptID); err != nil {
		return err
	}
	if !p.Reason.Valid() {
		return invalidField("/reason", "%q is not an abandon reason", string(p.Reason))
	}
	return nil
}

// AttemptFinished commits Running to Finished (attempt-03): valid output was
// received and the process was reaped. The output is the validated output
// artifact.
type AttemptFinished struct {
	AttemptID AttemptID   `json:"attemptId"`
	Output    ArtifactRef `json:"output"`
}

func (AttemptFinished) EventType() EventType          { return EventAttemptFinished }
func (p AttemptFinished) ArtifactRefs() []ArtifactRef { return refs(p.Output) }
func (p AttemptFinished) Validate() error {
	if err := requireID("/attemptId", p.AttemptID); err != nil {
		return err
	}
	return checkRef("/output", p.Output)
}

// AttemptFailed commits Running to Failed (attempt-04). The outcome is the
// host's outcome and is not completed. The error class is optional because
// classification arrives in M3.
type AttemptFailed struct {
	AttemptID  AttemptID        `json:"attemptId"`
	Outcome    ExecutionOutcome `json:"outcome"`
	ErrorClass ErrorClass       `json:"errorClass,omitzero"`
	Detail     string           `json:"detail,omitzero"`
}

func (AttemptFailed) EventType() EventType        { return EventAttemptFailed }
func (AttemptFailed) ArtifactRefs() []ArtifactRef { return nil }
func (p AttemptFailed) Validate() error {
	if err := requireID("/attemptId", p.AttemptID); err != nil {
		return err
	}
	switch {
	case !p.Outcome.Valid():
		return invalidField("/outcome", "%q is not an execution outcome", string(p.Outcome))
	case p.Outcome == OutcomeCompleted:
		return invalidField("/outcome", "a failed Attempt cannot have the outcome %s", p.Outcome)
	case p.ErrorClass != "" && !p.ErrorClass.Valid():
		return invalidField("/errorClass", "%q is not an error class", string(p.ErrorClass))
	}
	return checkText("/detail", p.Detail)
}

// AttemptTimedOut commits Running to TimedOut (attempt-05): the Turn timeout.
type AttemptTimedOut struct {
	AttemptID AttemptID `json:"attemptId"`
}

func (AttemptTimedOut) EventType() EventType        { return EventAttemptTimedOut }
func (AttemptTimedOut) ArtifactRefs() []ArtifactRef { return nil }
func (p AttemptTimedOut) Validate() error           { return requireID("/attemptId", p.AttemptID) }

// AttemptCancelled commits Running to Cancelled (attempt-06): the Attempt was
// cancelled and its process reaped.
type AttemptCancelled struct {
	AttemptID AttemptID `json:"attemptId"`
}

func (AttemptCancelled) EventType() EventType        { return EventAttemptCancelled }
func (AttemptCancelled) ArtifactRefs() []ArtifactRef { return nil }
func (p AttemptCancelled) Validate() error           { return requireID("/attemptId", p.AttemptID) }

// AttemptAwaitingInput commits Running to WaitingForUser (attempt-07): a
// validated question request, identified by the request ID.
type AttemptAwaitingInput struct {
	AttemptID AttemptID `json:"attemptId"`
	RequestID RequestID `json:"requestId"`
}

func (AttemptAwaitingInput) EventType() EventType        { return EventAttemptAwaitingInput }
func (AttemptAwaitingInput) ArtifactRefs() []ArtifactRef { return nil }
func (p AttemptAwaitingInput) Validate() error {
	if err := requireID("/attemptId", p.AttemptID); err != nil {
		return err
	}
	return requireID("/requestId", p.RequestID)
}

// AttemptResumed commits WaitingForUser to Running (attempt-08): answers were
// received and the session resumed with the next Turn.
type AttemptResumed struct {
	AttemptID AttemptID `json:"attemptId"`
}

func (AttemptResumed) EventType() EventType        { return EventAttemptResumed }
func (AttemptResumed) ArtifactRefs() []ArtifactRef { return nil }
func (p AttemptResumed) Validate() error           { return requireID("/attemptId", p.AttemptID) }

// AttemptOutcomeUnknown commits Scheduled or Running to Unknown (attempt-10):
// the outcome cannot be determined.
type AttemptOutcomeUnknown struct {
	AttemptID AttemptID `json:"attemptId"`
}

func (AttemptOutcomeUnknown) EventType() EventType        { return EventAttemptOutcomeUnknown }
func (AttemptOutcomeUnknown) ArtifactRefs() []ArtifactRef { return nil }
func (p AttemptOutcomeUnknown) Validate() error           { return requireID("/attemptId", p.AttemptID) }

// AttemptReconciled commits Unknown to Finished, Failed or Abandoned
// (attempt-11) after reconciliation. The output is present if and only if To
// is Finished.
type AttemptReconciled struct {
	AttemptID AttemptID    `json:"attemptId"`
	To        AttemptState `json:"to"`
	Output    ArtifactRef  `json:"output,omitzero"`
}

func (AttemptReconciled) EventType() EventType          { return EventAttemptReconciled }
func (p AttemptReconciled) ArtifactRefs() []ArtifactRef { return refs(p.Output) }
func (p AttemptReconciled) Validate() error {
	if err := requireID("/attemptId", p.AttemptID); err != nil {
		return err
	}
	switch p.To {
	case AttemptStateFinished:
		if p.Output.IsZero() {
			return invalidField("/output", "required when the Attempt is reconciled to Finished")
		}
	case AttemptStateFailed, AttemptStateAbandoned:
		if !p.Output.IsZero() {
			return invalidField("/output", "allowed only when the Attempt is reconciled to Finished")
		}
	default:
		return invalidField("/to", "%q is not Finished, Failed or Abandoned", string(p.To))
	}
	return checkOptionalRef("/output", p.Output)
}
