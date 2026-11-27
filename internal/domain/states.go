package domain

import (
	"errors"
	"fmt"
	"slices"
)

// ErrInvalidState is wrapped by every state validation error.
var ErrInvalidState = errors.New("invalid state")

// stateName is the method set shared by the three state types.
type stateName interface {
	~string
	Valid() bool
}

func stateError(kind string, s string) error {
	return fmt.Errorf("%w: %q is not a %s state", ErrInvalidState, s, kind)
}

func marshalState[T stateName](kind string, s T) ([]byte, error) {
	if !s.Valid() {
		return nil, stateError(kind, string(s))
	}
	return []byte(s), nil
}

func unmarshalState[T stateName](kind string, dst *T, b []byte) error {
	s := T(b)
	if !s.Valid() {
		return stateError(kind, string(b))
	}
	*dst = s
	return nil
}

// RunState is a state of the Run state machine (SPEC §6). The wire value is
// the bare state name.
type RunState string

// The Run states, in the order of SPEC §6.
const (
	RunStateCreated           RunState = "Created"
	RunStateStrategySelection RunState = "StrategySelection"
	RunStatePlanning          RunState = "Planning"
	RunStatePlanValidation    RunState = "PlanValidation"
	RunStateWaitingApproval   RunState = "WaitingApproval"
	RunStateExecuting         RunState = "Executing"
	RunStateFinalVerification RunState = "FinalVerification"
	RunStateFinalEvaluation   RunState = "FinalEvaluation"
	RunStateCompleted         RunState = "Completed"
	RunStateCancelled         RunState = "Cancelled"
	RunStateWaitingForUser    RunState = "WaitingForUser"
	RunStateBlocked           RunState = "Blocked"
	RunStateRecoveryRequired  RunState = "RecoveryRequired"
)

var runStates = []RunState{
	RunStateCreated, RunStateStrategySelection, RunStatePlanning, RunStatePlanValidation,
	RunStateWaitingApproval, RunStateExecuting, RunStateFinalVerification, RunStateFinalEvaluation,
	RunStateCompleted, RunStateCancelled, RunStateWaitingForUser, RunStateBlocked, RunStateRecoveryRequired,
}

// RunStates returns every Run state in declaration order. The caller may
// modify the returned slice.
func RunStates() []RunState { return slices.Clone(runStates) }

// Valid reports whether s is a Run state.
func (s RunState) Valid() bool { return slices.Contains(runStates, s) }

// String returns the state name.
func (s RunState) String() string { return string(s) }

// Terminal reports whether no further transition leaves s. Completed and
// Cancelled are the only terminal Run states.
func (s RunState) Terminal() bool { return s == RunStateCompleted || s == RunStateCancelled }

// Suspended reports whether the engine has recorded a state to resume and no
// agent process is running: WaitingForUser, Blocked and RecoveryRequired.
func (s RunState) Suspended() bool {
	return s == RunStateWaitingForUser || s == RunStateBlocked || s == RunStateRecoveryRequired
}

// MarshalText implements encoding.TextMarshaler and refuses an unknown state.
func (s RunState) MarshalText() ([]byte, error) { return marshalState("Run", s) }

// UnmarshalText implements encoding.TextUnmarshaler and refuses an unknown
// state, including the empty string.
func (s *RunState) UnmarshalText(b []byte) error { return unmarshalState("Run", s, b) }

// PhaseState is a state of the Phase state machine (SPEC §6).
type PhaseState string

// The Phase states, in the order of SPEC §6.
const (
	PhaseStatePending            PhaseState = "Pending"
	PhaseStateReady              PhaseState = "Ready"
	PhaseStateRunning            PhaseState = "Running"
	PhaseStateVerifying          PhaseState = "Verifying"
	PhaseStateEvaluating         PhaseState = "Evaluating"
	PhaseStateIntegrationPending PhaseState = "IntegrationPending"
	PhaseStateIntegrating        PhaseState = "Integrating"
	PhaseStateAccepted           PhaseState = "Accepted"
	PhaseStateRecovering         PhaseState = "Recovering"
	PhaseStateFailed             PhaseState = "Failed"
	PhaseStateSuperseded         PhaseState = "Superseded"
	PhaseStateWaitingForUser     PhaseState = "WaitingForUser"
	PhaseStateRecoveryRequired   PhaseState = "RecoveryRequired"
)

var phaseStates = []PhaseState{
	PhaseStatePending, PhaseStateReady, PhaseStateRunning, PhaseStateVerifying, PhaseStateEvaluating,
	PhaseStateIntegrationPending, PhaseStateIntegrating, PhaseStateAccepted, PhaseStateRecovering,
	PhaseStateFailed, PhaseStateSuperseded, PhaseStateWaitingForUser, PhaseStateRecoveryRequired,
}

// PhaseStates returns every Phase state in declaration order. The caller may
// modify the returned slice.
func PhaseStates() []PhaseState { return slices.Clone(phaseStates) }

// Valid reports whether s is a Phase state.
func (s PhaseState) Valid() bool { return slices.Contains(phaseStates, s) }

// String returns the state name.
func (s PhaseState) String() string { return string(s) }

// MarshalText implements encoding.TextMarshaler and refuses an unknown state.
func (s PhaseState) MarshalText() ([]byte, error) { return marshalState("Phase", s) }

// UnmarshalText implements encoding.TextUnmarshaler and refuses an unknown
// state, including the empty string.
func (s *PhaseState) UnmarshalText(b []byte) error { return unmarshalState("Phase", s, b) }

// AttemptState is a state of the Attempt state machine (SPEC §6).
type AttemptState string

// The Attempt states, in the order of SPEC §6.
const (
	AttemptStateScheduled      AttemptState = "Scheduled"
	AttemptStateRunning        AttemptState = "Running"
	AttemptStateFinished       AttemptState = "Finished"
	AttemptStateFailed         AttemptState = "Failed"
	AttemptStateTimedOut       AttemptState = "TimedOut"
	AttemptStateCancelled      AttemptState = "Cancelled"
	AttemptStateWaitingForUser AttemptState = "WaitingForUser"
	AttemptStateAbandoned      AttemptState = "Abandoned"
	AttemptStateUnknown        AttemptState = "Unknown"
)

var attemptStates = []AttemptState{
	AttemptStateScheduled, AttemptStateRunning, AttemptStateFinished, AttemptStateFailed, AttemptStateTimedOut,
	AttemptStateCancelled, AttemptStateWaitingForUser, AttemptStateAbandoned, AttemptStateUnknown,
}

// AttemptStates returns every Attempt state in declaration order. The caller
// may modify the returned slice.
func AttemptStates() []AttemptState { return slices.Clone(attemptStates) }

// Valid reports whether s is an Attempt state.
func (s AttemptState) Valid() bool { return slices.Contains(attemptStates, s) }

// String returns the state name.
func (s AttemptState) String() string { return string(s) }

// Terminal reports whether no further transition leaves s: Finished, Failed,
// TimedOut, Cancelled and Abandoned.
func (s AttemptState) Terminal() bool {
	switch s {
	case AttemptStateFinished, AttemptStateFailed, AttemptStateTimedOut, AttemptStateCancelled, AttemptStateAbandoned:
		return true
	}
	return false
}

// MarshalText implements encoding.TextMarshaler and refuses an unknown state.
func (s AttemptState) MarshalText() ([]byte, error) { return marshalState("Attempt", s) }

// UnmarshalText implements encoding.TextUnmarshaler and refuses an unknown
// state, including the empty string.
func (s *AttemptState) UnmarshalText(b []byte) error { return unmarshalState("Attempt", s, b) }
