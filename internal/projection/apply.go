package projection

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"time"

	"github.com/shutx-net/bellona/internal/domain"
	"github.com/shutx-net/bellona/internal/fsm"
)

// ErrInvalidEvent is matched, under errors.Is, by the error of an event that
// breaks a rule that depends on the state or on the event itself rather than
// on the transition tables: a wrong sequence number or Run ID, an unknown
// Attempt, Phase or revision, an event after the Run ended, a payload that
// does not validate, a limit that does not match, and so on. A transition
// that the tables do not list matches fsm.ErrIllegalTransition instead; the
// two classes are disjoint.
var ErrInvalidEvent = errors.New("event is not valid in the current state")

// ApplyError is the error of Apply and Replay for a rejected event. Seq and
// Type identify the offending record; Err is the reason and matches
// ErrInvalidEvent or fsm.ErrIllegalTransition under errors.Is.
type ApplyError struct {
	Seq  uint64
	Type domain.EventType
	Err  error
}

// Error implements the error interface.
func (e *ApplyError) Error() string {
	return fmt.Sprintf("projection: event %d (%s) rejected: %v", e.Seq, e.Type, e.Err)
}

// Unwrap returns the reason.
func (e *ApplyError) Unwrap() error { return e.Err }

// invalid returns an error that wraps ErrInvalidEvent.
func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidEvent, fmt.Sprintf(format, args...))
}

// Apply returns the state that follows s when the committed event e is
// applied. It never modifies s: it works on a deep copy and, for an event that
// is illegal or invalid, returns s itself with an *ApplyError. The success
// result shares no memory with s.
//
// The checks run in a fixed order, which decides the class of the error when
// an event breaks more than one rule: (1) the event itself and the global
// rules (a value payload that validates, the Run ID, the sequence number, no
// event after the Run ended, RunCreated only first, ManifestFrozen only
// second), then, per event type, (2) the entity whose machine the event moves
// (the Attempt of an Attempt event, the revision and the Phase of a Phase
// event; for AttemptScheduled the new ID, the Run state, the revision and the
// Phase), (3) the transition tables of package fsm, and (4) the rules that
// relate the payload to the state, including the Attempts that an event cites
// as evidence (the planner Attempt of PlanGenerated, the failed Evaluator
// Attempt of a retry, the Implementer Attempt of PhaseVerificationStarted and
// PhaseAccepted), the cause or reason that names the source state, and the
// resume state. Classes (1), (2) and (4) yield ErrInvalidEvent; (3) yields
// fsm.ErrIllegalTransition. So an unknown cited Attempt in a state that the
// tables do not allow is an illegal transition, not an invalid event.
//
// Apply reads no clock: every timestamp of the new state is e.Time.
func Apply(s State, e domain.Event) (State, error) {
	next := s.Clone()
	if err := next.apply(e); err != nil {
		return s, &ApplyError{Seq: e.Seq, Type: e.Type, Err: err}
	}
	return next, nil
}

// apply applies e to s in place. When it fails, s may have been changed
// partially and must be discarded.
func (s *State) apply(e domain.Event) error {
	if err := checkEvent(e); err != nil {
		return err
	}
	if s.LastSeq == 0 {
		return s.start(e)
	}
	switch {
	case e.RunID != s.RunID:
		return invalid("the event belongs to run %s, this is run %s", e.RunID, s.RunID)
	case e.Seq != s.LastSeq+1:
		return invalid("sequence number %d, want %d", e.Seq, s.LastSeq+1)
	case s.Run.State.Terminal():
		return invalid("the Run is %s and accepts no further event", s.Run.State)
	case e.Type == domain.EventRunCreated:
		return invalid("RunCreated is the first event of a Run and cannot be applied again")
	case e.Seq == 2 && e.Type != domain.EventManifestFrozen:
		return invalid("the second event of a Run is ManifestFrozen, not %s", e.Type)
	case e.Type == domain.EventManifestFrozen && e.Seq != 2:
		return invalid("ManifestFrozen is the second event of a Run, not event %d", e.Seq)
	}

	var err error
	switch p := e.Payload.(type) {
	case domain.ManifestFrozen:
		err = s.manifestFrozen(p)
	case domain.PolicyAmended:
		err = s.policyAmended(e, p)
	case domain.AttemptScheduled:
		err = s.attemptScheduled(e, p)

	case domain.RunPrepared:
		err = s.moveRun(e, domain.RunStateStrategySelection)
	case domain.StrategySelected:
		err = s.moveRun(e, domain.RunStatePlanning)
	case domain.PlanGenerated:
		err = s.planGenerated(e, p)
	case domain.PlanValidationFailed:
		if err = s.moveRun(e, domain.RunStatePlanning); err == nil {
			s.Run.PendingPlan = domain.ArtifactRef{}
		}
	case domain.PlanApprovalRequested:
		if err = s.moveRun(e, domain.RunStateWaitingApproval); err == nil && p.Artifact != s.Run.PendingPlan {
			err = invalid("the approval is requested for %s, but the plan under validation is %s", p.Artifact.Path, s.Run.PendingPlan.Path)
		}
	case domain.PlanAccepted:
		err = s.planAccepted(e, p)
	case domain.PlanChangesRequested:
		if err = s.moveRun(e, domain.RunStatePlanning); err == nil {
			s.Run.PendingPlan = domain.ArtifactRef{}
			s.Run.ChangeRequestCount++
		}
	case domain.FinalVerificationStarted:
		err = s.moveRun(e, domain.RunStateFinalVerification)
	case domain.ReplanningStarted:
		err = s.replanningStarted(e, p)
	case domain.FinalEvaluationStarted:
		err = s.moveRun(e, domain.RunStateFinalEvaluation)
	case domain.RunCompleted:
		err = s.moveRun(e, domain.RunStateCompleted)
	case domain.FinalEvaluationRetried:
		err = s.finalEvaluationRetried(e, p)
	case domain.RunSuspended:
		err = s.runSuspended(e, p)
	case domain.RunResumed:
		err = s.runResumed(e, p)
	case domain.RunCancelled:
		err = s.runCancelled(e, p)

	case domain.PhaseReady:
		err = s.phaseMove(e, p.Revision, p.PhaseID, domain.PhaseStateReady, nil)
	case domain.PhaseVerificationStarted:
		err = s.phaseMove(e, p.Revision, p.PhaseID, domain.PhaseStateVerifying, func(ph *Phase, _ domain.PhaseState) error {
			if n := len(ph.Attempts); n == 0 || ph.Attempts[n-1] != p.AttemptID {
				return invalid("attempt %s is not the latest Attempt of phase %s", p.AttemptID, ph.ID)
			}
			return s.requireAttempt(p.AttemptID, domain.RoleImplementer, domain.AttemptStateFinished)
		})
	case domain.PhaseRecovering:
		err = s.phaseMove(e, p.Revision, p.PhaseID, domain.PhaseStateRecovering, func(ph *Phase, from domain.PhaseState) error {
			if p.Cause.Source() != from {
				return invalid("recovery cause %s applies to a Phase in %s, not in %s", p.Cause, p.Cause.Source(), from)
			}
			ph.RecoveryCause = p.Cause
			return nil
		})
	case domain.PhaseEvaluationStarted:
		err = s.phaseMove(e, p.Revision, p.PhaseID, domain.PhaseStateEvaluating, nil)
	case domain.PhaseIntegrationQueued:
		err = s.phaseMove(e, p.Revision, p.PhaseID, domain.PhaseStateIntegrationPending, nil)
	case domain.PhaseEvaluationRetried:
		err = s.phaseMove(e, p.Revision, p.PhaseID, domain.PhaseStateEvaluating, func(ph *Phase, _ domain.PhaseState) error {
			return s.requirePhaseAttempt(p.FailedAttemptID, p.Revision, ph.ID, domain.RoleEvaluator,
				domain.AttemptStateFailed, domain.AttemptStateTimedOut)
		})
	case domain.PhaseIntegrationStarted:
		err = s.phaseMove(e, p.Revision, p.PhaseID, domain.PhaseStateIntegrating, nil)
	case domain.PhaseAccepted:
		err = s.phaseMove(e, p.Revision, p.PhaseID, domain.PhaseStateAccepted, func(ph *Phase, _ domain.PhaseState) error {
			return s.requirePhaseAttempt(p.AttemptID, p.Revision, ph.ID, domain.RoleImplementer, domain.AttemptStateFinished)
		})
	case domain.PhaseRetryAuthorized:
		err = s.phaseMove(e, p.Revision, p.PhaseID, domain.PhaseStateReady, nil)
	case domain.PhaseFailed:
		err = s.phaseMove(e, p.Revision, p.PhaseID, domain.PhaseStateFailed, nil)
	case domain.PhaseSuspended:
		err = s.phaseMove(e, p.Revision, p.PhaseID, p.To, func(ph *Phase, from domain.PhaseState) error {
			if p.ResumeState != from {
				return invalid("the resume state %s is not the state %s that phase %s leaves", p.ResumeState, from, ph.ID)
			}
			if p.To == domain.PhaseStateWaitingForUser {
				ph.ResumeState = p.ResumeState
			}
			return nil
		})
	case domain.PhaseResumed:
		err = s.phaseMove(e, p.Revision, p.PhaseID, p.To, nil)
	case domain.PhaseReconciled:
		err = s.phaseMove(e, p.Revision, p.PhaseID, p.To, nil)
	case domain.PhaseSuperseded:
		err = s.phaseMove(e, p.Revision, p.PhaseID, domain.PhaseStateSuperseded, nil)

	case domain.AttemptStarted:
		err = s.attemptMove(e, p.AttemptID, domain.AttemptStateRunning, func(a *Attempt, _ domain.AttemptState) error {
			if p.SessionID != "" {
				if a.SessionID != "" && a.SessionID != p.SessionID {
					return invalid("attempt %s runs in session %s, not in %s", a.ID, a.SessionID, p.SessionID)
				}
				a.SessionID = p.SessionID
			}
			a.StartedAt = e.Time
			return nil
		})
	case domain.AttemptAbandoned:
		err = s.attemptMove(e, p.AttemptID, domain.AttemptStateAbandoned, func(a *Attempt, from domain.AttemptState) error {
			if p.Reason.Source() != from {
				return invalid("abandon reason %s applies to an Attempt in %s, not in %s", p.Reason, p.Reason.Source(), from)
			}
			a.AbandonReason = p.Reason
			a.EndedAt = e.Time
			return nil
		})
	case domain.AttemptFinished:
		err = s.attemptMove(e, p.AttemptID, domain.AttemptStateFinished, func(a *Attempt, _ domain.AttemptState) error {
			a.Output = p.Output
			a.EndedAt = e.Time
			return nil
		})
	case domain.AttemptFailed:
		err = s.attemptMove(e, p.AttemptID, domain.AttemptStateFailed, func(a *Attempt, _ domain.AttemptState) error {
			a.Outcome = p.Outcome
			a.ErrorClass = p.ErrorClass
			a.Detail = p.Detail
			a.EndedAt = e.Time
			return nil
		})
	case domain.AttemptTimedOut:
		err = s.attemptMove(e, p.AttemptID, domain.AttemptStateTimedOut, func(a *Attempt, _ domain.AttemptState) error {
			a.EndedAt = e.Time
			return nil
		})
	case domain.AttemptCancelled:
		err = s.attemptMove(e, p.AttemptID, domain.AttemptStateCancelled, func(a *Attempt, _ domain.AttemptState) error {
			a.EndedAt = e.Time
			return nil
		})
	case domain.AttemptAwaitingInput:
		err = s.attemptMove(e, p.AttemptID, domain.AttemptStateWaitingForUser, func(a *Attempt, _ domain.AttemptState) error {
			a.RequestID = p.RequestID
			return nil
		})
	case domain.AttemptResumed:
		err = s.attemptMove(e, p.AttemptID, domain.AttemptStateRunning, nil)
	case domain.AttemptOutcomeUnknown:
		err = s.attemptMove(e, p.AttemptID, domain.AttemptStateUnknown, nil)
	case domain.AttemptReconciled:
		err = s.attemptMove(e, p.AttemptID, p.To, func(a *Attempt, _ domain.AttemptState) error {
			if p.To == domain.AttemptStateFinished {
				a.Output = p.Output
			}
			a.EndedAt = e.Time
			return nil
		})

	default:
		err = invalid("unsupported payload type %T for event type %s", e.Payload, e.Type)
	}
	if err != nil {
		return err
	}
	return s.commit(e)
}

// checkEvent checks what can be decided from the event alone: the schema
// version, a payload of the type that the event type names, a UTC time, and
// the validity of the payload (which includes the rules that the catalogue
// puts on every payload, such as an acyclic Phase graph in PlanAccepted).
func checkEvent(e domain.Event) error {
	switch {
	case e.SchemaVersion != domain.EventSchemaVersion:
		return invalid("schema version %d, want %d", e.SchemaVersion, domain.EventSchemaVersion)
	case e.Payload == nil:
		return invalid("%s event has no payload", e.Type)
	case reflect.TypeOf(e.Payload).Kind() == reflect.Pointer:
		// Checked before any method is called: the methods of a payload have
		// value receivers, so calling one on a nil pointer would panic.
		return invalid("the payload of a %s event is a %T; payloads are values", e.Type, e.Payload)
	case e.Payload.EventType() != e.Type:
		return invalid("a payload of event type %s in an event of type %s", e.Payload.EventType(), e.Type)
	case e.Time.IsZero():
		return invalid("the event time is not set")
	case e.Time.Location() != time.UTC:
		return invalid("the event time %s is not in UTC", e.Time.Format(time.RFC3339Nano))
	}
	if err := e.Payload.Validate(); err != nil {
		return fmt.Errorf("%w: %s payload: %w", ErrInvalidEvent, e.Type, err)
	}
	return nil
}

// start applies the first event of a Run to the empty State.
func (s *State) start(e domain.Event) error {
	p, ok := e.Payload.(domain.RunCreated)
	switch {
	case e.Seq != 1:
		return invalid("the first event of a Run has sequence number 1, not %d", e.Seq)
	case !ok:
		return invalid("the first event of a Run is RunCreated, not %s", e.Type)
	}
	if err := e.RunID.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidEvent, err)
	}
	*s = State{
		RunID:      e.RunID,
		Run:        Run{State: domain.RunStateCreated, Suspensions: []Suspension{}, CreatedAt: e.Time, Task: p.Task},
		Amendments: []Amendment{},
		Revisions:  []Revision{},
		Attempts:   []Attempt{},
		Artifacts:  []domain.ArtifactRef{},
	}
	return s.commit(e)
}

// commit registers the artifact references of e, then records the sequence
// number and the time of e.
func (s *State) commit(e domain.Event) error {
	for _, ref := range e.Payload.ArtifactRefs() {
		if err := s.register(ref); err != nil {
			return err
		}
	}
	s.LastSeq = e.Seq
	s.Run.UpdatedAt = e.Time
	return nil
}

// register adds an artifact reference to s.Artifacts (R-PER-07). An identical
// reference that is already there is not added again; a known path with another
// size or hash is rejected, because finalized artifacts are immutable, and so
// is a path that the engine owns.
func (s *State) register(ref domain.ArtifactRef) error {
	if domain.IsReservedPath(ref.Path) {
		return invalid("%q is reserved for the engine and cannot be an artifact", ref.Path)
	}
	for _, known := range s.Artifacts {
		switch {
		case known.Path != ref.Path:
		case known == ref:
			return nil
		default:
			return invalid("artifact %q is already registered with size %d and sha256 %s, not with size %d and sha256 %s",
				ref.Path, known.Size, known.SHA256, ref.Size, ref.SHA256)
		}
	}
	s.Artifacts = append(s.Artifacts, ref)
	return nil
}

// ---------------------------------------------------------------------------
// Run
// ---------------------------------------------------------------------------

// resumeState returns the state that a RunResumed must return to: the
// ResumeState of the top Suspension, or the empty state when the Run is not
// suspended.
func (s *State) resumeState() domain.RunState {
	if n := len(s.Run.Suspensions); n > 0 {
		return s.Run.Suspensions[n-1].ResumeState
	}
	return ""
}

// moveRun moves the Run to the given state when the Run table allows it for
// the event, with the recorded resume state as the resume argument.
func (s *State) moveRun(e domain.Event, to domain.RunState) error {
	if _, err := fsm.CheckRun(s.Run.State, to, e.Type, s.resumeState()); err != nil {
		return err
	}
	s.Run.State = to
	return nil
}

func (s *State) manifestFrozen(p domain.ManifestFrozen) error {
	if s.Run.State != domain.RunStateCreated {
		return invalid("ManifestFrozen applies to a Run in Created, not in %s", s.Run.State)
	}
	s.Run.Manifest = p.Manifest
	s.Run.Policy = p.Policy
	s.Limits = p.Limits
	return nil
}

// policyAmended applies the changes of a user policy amendment in order. It is
// the only way in which the execution limits change after ManifestFrozen
// (R-CFG-01, SPEC §6.3). It requires a Blocked Run, and the previous value of
// every change must be the value in force at that point.
func (s *State) policyAmended(e domain.Event, p domain.PolicyAmended) error {
	if s.Run.State != domain.RunStateBlocked {
		return invalid("a policy amendment applies to a Blocked Run, not to one in %s", s.Run.State)
	}
	limits := s.Limits
	for i, c := range p.Changes {
		if have := limits.Value(c.Limit); c.Previous != have {
			return invalid("change %d: the previous value of %s is %q, but the limit is %q", i, c.Limit, c.Previous, have)
		}
		next, err := limits.With(c.Limit, c.New)
		if err != nil {
			return fmt.Errorf("%w: change %d: %w", ErrInvalidEvent, i, err)
		}
		limits = next
	}
	s.Limits = limits
	s.Amendments = append(s.Amendments, Amendment{Seq: e.Seq, Time: e.Time, Changes: slices.Clone(p.Changes)})
	return nil
}

func (s *State) planGenerated(e domain.Event, p domain.PlanGenerated) error {
	if err := s.moveRun(e, domain.RunStatePlanValidation); err != nil {
		return err
	}
	a := s.attempt(p.AttemptID)
	switch {
	case a == nil:
		return invalid("unknown attempt %s", p.AttemptID)
	case a.Role != domain.RolePlanner:
		return invalid("attempt %s is a %s Attempt, not a planner Attempt", a.ID, a.Role)
	case a.State != domain.AttemptStateFinished:
		return invalid("planner attempt %s is %s, not Finished", a.ID, a.State)
	case a.Output != p.Artifact:
		return invalid("planner attempt %s produced %s, not %s", a.ID, a.Output.Path, p.Artifact.Path)
	}
	s.Run.PendingPlan = p.Artifact
	return nil
}

func (s *State) planAccepted(e domain.Event, p domain.PlanAccepted) error {
	if err := s.moveRun(e, domain.RunStateExecuting); err != nil {
		return err
	}
	if p.Revision != s.Run.PlanRevision+1 {
		return invalid("plan revision %d, want %d", p.Revision, s.Run.PlanRevision+1)
	}
	phases := make([]Phase, len(p.Phases))
	for i, def := range p.Phases {
		phases[i] = Phase{
			ID:        def.ID,
			DependsOn: append([]domain.PhaseID{}, def.DependsOn...),
			State:     domain.PhaseStatePending,
			Attempts:  []domain.AttemptID{},
			UpdatedAt: e.Time,
		}
	}
	s.Revisions = append(s.Revisions, Revision{Number: p.Revision, Plan: p.Plan, AcceptedAt: e.Time, Phases: phases})
	s.Run.PlanRevision = p.Revision
	s.Run.PendingPlan = domain.ArtifactRef{}
	return nil
}

func (s *State) replanningStarted(e domain.Event, p domain.ReplanningStarted) error {
	from := s.Run.State
	if err := s.moveRun(e, domain.RunStatePlanning); err != nil {
		return err
	}
	if p.Cause.Source() != from {
		return invalid("replan cause %s applies to a Run in %s, not in %s", p.Cause, p.Cause.Source(), from)
	}
	s.Run.ReplanCount++
	return nil
}

func (s *State) finalEvaluationRetried(e domain.Event, p domain.FinalEvaluationRetried) error {
	if err := s.moveRun(e, domain.RunStateFinalEvaluation); err != nil {
		return err
	}
	a := s.attempt(p.FailedAttemptID)
	switch {
	case a == nil:
		return invalid("unknown attempt %s", p.FailedAttemptID)
	case a.Role != domain.RoleEvaluator || a.PhaseID != "":
		return invalid("attempt %s is not a Run-level evaluator Attempt", a.ID)
	case a.State != domain.AttemptStateFailed && a.State != domain.AttemptStateTimedOut:
		return invalid("evaluator attempt %s is %s, not Failed or TimedOut", a.ID, a.State)
	}
	return nil
}

func (s *State) runSuspended(e domain.Event, p domain.RunSuspended) error {
	from := s.Run.State
	if err := s.moveRun(e, p.To); err != nil {
		return err
	}
	if p.ResumeState != from {
		return invalid("the resume state %s is not the state %s that the Run leaves", p.ResumeState, from)
	}
	s.Run.Suspensions = append(s.Run.Suspensions, Suspension{
		State:       p.To,
		ResumeState: p.ResumeState,
		Reason:      p.Reason,
		Detail:      p.Detail,
		RequestID:   p.RequestID,
		Seq:         e.Seq,
		Since:       e.Time,
	})
	return nil
}

func (s *State) runResumed(e domain.Event, p domain.RunResumed) error {
	// The table accepts the event only when p.To is the recorded resume state,
	// so a non-empty stack is guaranteed once moveRun succeeds.
	if err := s.moveRun(e, p.To); err != nil {
		return err
	}
	n := len(s.Run.Suspensions)
	top := s.Run.Suspensions[n-1]
	if p.RequestID != "" && p.RequestID != top.RequestID {
		return invalid("the Run waited for request %q, not for %q", top.RequestID, p.RequestID)
	}
	s.Run.Suspensions = s.Run.Suspensions[:n-1]
	return nil
}

func (s *State) runCancelled(e domain.Event, p domain.RunCancelled) error {
	from := s.Run.State
	if err := s.moveRun(e, domain.RunStateCancelled); err != nil {
		return err
	}
	if p.Reason == domain.CancelReasonPlanRejected && from != domain.RunStateWaitingApproval {
		return invalid("a plan can be rejected only while the Run is WaitingApproval, not in %s", from)
	}
	s.Run.Suspensions = []Suspension{}
	s.Run.CancelReason = p.Reason
	return nil
}

// ---------------------------------------------------------------------------
// Phase
// ---------------------------------------------------------------------------

// phaseMove applies an event of the Phase table to one Phase. It looks the
// Phase up (every Phase event but PhaseSuperseded must name the current Plan
// Revision), moves it to the given state when the Phase table allows the
// event, and then calls after, if any, with the Phase and the state that it
// left, for the rules that relate the payload to the state. A Phase that is
// not WaitingForUser keeps no resume state.
func (s *State) phaseMove(e domain.Event, revision int, id domain.PhaseID, to domain.PhaseState,
	after func(ph *Phase, from domain.PhaseState) error) error {
	ph, err := s.lookupPhase(revision, id, e.Type != domain.EventPhaseSuperseded)
	if err != nil {
		return err
	}
	from := ph.State
	if err := s.movePhase(e, ph, to); err != nil {
		return err
	}
	if after != nil {
		return after(ph, from)
	}
	return nil
}

// lookupPhase returns the Phase, which must exist; when current is set its
// revision must be the current Plan Revision.
func (s *State) lookupPhase(revision int, id domain.PhaseID, current bool) (*Phase, error) {
	if s.revision(revision) == nil {
		return nil, invalid("unknown plan revision %d", revision)
	}
	if current && revision != s.Run.PlanRevision {
		return nil, invalid("plan revision %d is not the current revision %d", revision, s.Run.PlanRevision)
	}
	ph := s.phase(revision, id)
	if ph == nil {
		return nil, invalid("unknown phase %s in plan revision %d", id, revision)
	}
	return ph, nil
}

// movePhase moves ph to the given state when the Phase table allows it for the
// event, with the recorded resume state of the Phase as the resume argument.
func (s *State) movePhase(e domain.Event, ph *Phase, to domain.PhaseState) error {
	if _, err := fsm.CheckPhase(ph.State, to, e.Type, ph.ResumeState); err != nil {
		return err
	}
	ph.State = to
	if to != domain.PhaseStateWaitingForUser {
		ph.ResumeState = ""
	}
	ph.UpdatedAt = e.Time
	return nil
}

// requireAttempt checks that the Attempt exists, has the role and is in one of
// the states.
func (s *State) requireAttempt(id domain.AttemptID, role domain.Role, states ...domain.AttemptState) error {
	a := s.attempt(id)
	switch {
	case a == nil:
		return invalid("unknown attempt %s", id)
	case a.Role != role:
		return invalid("attempt %s is a %s Attempt, not a %s Attempt", id, a.Role, role)
	case !slices.Contains(states, a.State):
		return invalid("attempt %s is %s, want one of %v", id, a.State, states)
	}
	return nil
}

// requirePhaseAttempt is requireAttempt for an Attempt that must also belong to
// the given Phase of the given revision.
func (s *State) requirePhaseAttempt(id domain.AttemptID, revision int, phase domain.PhaseID, role domain.Role, states ...domain.AttemptState) error {
	if err := s.requireAttempt(id, role, states...); err != nil {
		return err
	}
	if a := s.attempt(id); a.PhaseID != phase || a.Revision != revision {
		return invalid("attempt %s does not belong to phase %s of plan revision %d", id, phase, revision)
	}
	return nil
}

// attemptScheduled creates an Attempt in Scheduled. An Implementer Attempt
// also moves its Phase to Running (row phase-02 of the Phase table).
func (s *State) attemptScheduled(e domain.Event, p domain.AttemptScheduled) error {
	switch {
	case s.attemptIndex(p.AttemptID) >= 0:
		return invalid("attempt %s already exists", p.AttemptID)
	case s.Run.State == domain.RunStateCreated || s.Run.State.Suspended():
		return invalid("no Attempt can be scheduled while the Run is %s", s.Run.State)
	case p.Revision != 0 && p.Revision != s.Run.PlanRevision:
		return invalid("plan revision %d is not the current revision %d", p.Revision, s.Run.PlanRevision)
	}
	var ph *Phase
	if p.PhaseID != "" {
		var err error
		if ph, err = s.lookupPhase(p.Revision, p.PhaseID, true); err != nil {
			return err
		}
	}
	switch p.Role {
	case domain.RoleImplementer:
		if err := s.movePhase(e, ph, domain.PhaseStateRunning); err != nil {
			return err
		}
		ph.Attempts = append(ph.Attempts, p.AttemptID)
	case domain.RoleEvaluator:
		switch {
		case ph != nil && ph.State != domain.PhaseStateEvaluating:
			return invalid("an evaluator Attempt reviews a Phase in Evaluating, but phase %s is %s", ph.ID, ph.State)
		case ph != nil:
			ph.Attempts = append(ph.Attempts, p.AttemptID)
			ph.UpdatedAt = e.Time
		case s.Run.State != domain.RunStateFinalEvaluation:
			return invalid("a Run-level evaluator Attempt needs a Run in FinalEvaluation, not in %s", s.Run.State)
		}
	}
	s.Attempts = append(s.Attempts, Attempt{
		ID:          p.AttemptID,
		Role:        p.Role,
		Revision:    p.Revision,
		PhaseID:     p.PhaseID,
		SessionID:   p.SessionID,
		State:       domain.AttemptStateScheduled,
		ScheduledAt: e.Time,
		UpdatedAt:   e.Time,
	})
	return nil
}

// ---------------------------------------------------------------------------
// Attempt
// ---------------------------------------------------------------------------

// attemptMove applies an event of the Attempt table to one Attempt: the
// Attempt must exist and the Attempt table must allow the event to move it
// from its state to the given state. Then after, if any, runs with the Attempt
// and the state that it left.
func (s *State) attemptMove(e domain.Event, id domain.AttemptID, to domain.AttemptState,
	after func(a *Attempt, from domain.AttemptState) error) error {
	a := s.attempt(id)
	if a == nil {
		return invalid("unknown attempt %s", id)
	}
	from := a.State
	if _, err := fsm.CheckAttempt(from, to, e.Type); err != nil {
		return err
	}
	a.State = to
	a.UpdatedAt = e.Time
	if after != nil {
		return after(a, from)
	}
	return nil
}
