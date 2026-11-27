package domain

import (
	"errors"
	"fmt"
	"slices"
)

// ErrInvalidEnum is wrapped by every error about a value that is not a member
// of one of the closed enumerations of this file.
var ErrInvalidEnum = errors.New("invalid enumeration value")

// enumValue is the method set shared by the closed string enumerations.
type enumValue interface {
	~string
	Valid() bool
}

func marshalEnum[T enumValue](kind string, sentinel error, v T) ([]byte, error) {
	if !v.Valid() {
		return nil, fmt.Errorf("%w: %q is not a %s", sentinel, string(v), kind)
	}
	return []byte(v), nil
}

func unmarshalEnum[T enumValue](kind string, sentinel error, dst *T, b []byte) error {
	v := T(b)
	if !v.Valid() {
		return fmt.Errorf("%w: %q is not a %s", sentinel, string(b), kind)
	}
	*dst = v
	return nil
}

// SuspendReason is the reason a Run enters a suspended state (SPEC §6).
type SuspendReason string

// The suspension reasons. Each one determines the suspended state that the
// Run enters (see Target).
const (
	// SuspendReasonUserInput: a validated question request; the Run waits
	// for the user.
	SuspendReasonUserInput SuspendReason = "user_input"
	// SuspendReasonUnknownOutcome: an unknown external execution outcome;
	// the Run needs reconciliation.
	SuspendReasonUnknownOutcome SuspendReason = "unknown_outcome"
	// SuspendReasonBaselineFailed: the baseline checks failed.
	SuspendReasonBaselineFailed SuspendReason = "baseline_failed"
	// SuspendReasonMissingEnvironment: the environment is missing.
	SuspendReasonMissingEnvironment SuspendReason = "missing_environment"
	// SuspendReasonAuthenticationFailed: authentication failed.
	SuspendReasonAuthenticationFailed SuspendReason = "authentication_failed"
	// SuspendReasonNoProgress: no Phase can make progress and replanning is
	// not authorized.
	SuspendReasonNoProgress SuspendReason = "no_progress"
	// SuspendReasonLimitReached: a Run-wide limit is reached.
	SuspendReasonLimitReached SuspendReason = "limit_reached"
	// SuspendReasonNeedsUser: the issue cannot be resolved without the user.
	SuspendReasonNeedsUser SuspendReason = "needs_user"
)

var suspendReasons = []SuspendReason{
	SuspendReasonUserInput, SuspendReasonUnknownOutcome, SuspendReasonBaselineFailed,
	SuspendReasonMissingEnvironment, SuspendReasonAuthenticationFailed, SuspendReasonNoProgress,
	SuspendReasonLimitReached, SuspendReasonNeedsUser,
}

// SuspendReasons returns every suspension reason in declaration order. The
// caller may modify the returned slice.
func SuspendReasons() []SuspendReason { return slices.Clone(suspendReasons) }

// Valid reports whether r is a suspension reason.
func (r SuspendReason) Valid() bool { return slices.Contains(suspendReasons, r) }

// String returns the reason.
func (r SuspendReason) String() string { return string(r) }

// Target returns the suspended Run state that r leads to: WaitingForUser for
// user_input, RecoveryRequired for unknown_outcome and Blocked for every other
// reason. It returns the empty string for an invalid reason.
func (r SuspendReason) Target() RunState {
	switch r {
	case SuspendReasonUserInput:
		return RunStateWaitingForUser
	case SuspendReasonUnknownOutcome:
		return RunStateRecoveryRequired
	case SuspendReasonBaselineFailed, SuspendReasonMissingEnvironment, SuspendReasonAuthenticationFailed,
		SuspendReasonNoProgress, SuspendReasonLimitReached, SuspendReasonNeedsUser:
		return RunStateBlocked
	}
	return ""
}

// MarshalText implements encoding.TextMarshaler and refuses an invalid reason.
func (r SuspendReason) MarshalText() ([]byte, error) {
	return marshalEnum("suspend reason", ErrInvalidEnum, r)
}

// UnmarshalText implements encoding.TextUnmarshaler and refuses an invalid
// reason, including the empty string.
func (r *SuspendReason) UnmarshalText(b []byte) error {
	return unmarshalEnum("suspend reason", ErrInvalidEnum, r, b)
}

// ReplanCause is the reason a Run returns to Planning for a new Plan
// Revision. Every cause consumes the replan counter.
type ReplanCause string

// The replan causes.
const (
	// ReplanCauseReplanAuthorized: a replan was authorized while Executing.
	ReplanCauseReplanAuthorized ReplanCause = "replan_authorized"
	// ReplanCauseFinalVerificationFailed: the final verification failed.
	ReplanCauseFinalVerificationFailed ReplanCause = "final_verification_failed"
	// ReplanCauseFinalEvaluationRejected: the final evaluation was rejected.
	ReplanCauseFinalEvaluationRejected ReplanCause = "final_evaluation_rejected"
)

var replanCauses = []ReplanCause{
	ReplanCauseReplanAuthorized, ReplanCauseFinalVerificationFailed, ReplanCauseFinalEvaluationRejected,
}

// ReplanCauses returns every replan cause in declaration order. The caller
// may modify the returned slice.
func ReplanCauses() []ReplanCause { return slices.Clone(replanCauses) }

// Valid reports whether c is a replan cause.
func (c ReplanCause) Valid() bool { return slices.Contains(replanCauses, c) }

// String returns the cause.
func (c ReplanCause) String() string { return string(c) }

// Source returns the Run state that c leaves: Executing, FinalVerification or
// FinalEvaluation. It returns the empty string for an invalid cause.
func (c ReplanCause) Source() RunState {
	switch c {
	case ReplanCauseReplanAuthorized:
		return RunStateExecuting
	case ReplanCauseFinalVerificationFailed:
		return RunStateFinalVerification
	case ReplanCauseFinalEvaluationRejected:
		return RunStateFinalEvaluation
	}
	return ""
}

// MarshalText implements encoding.TextMarshaler and refuses an invalid cause.
func (c ReplanCause) MarshalText() ([]byte, error) {
	return marshalEnum("replan cause", ErrInvalidEnum, c)
}

// UnmarshalText implements encoding.TextUnmarshaler and refuses an invalid
// cause, including the empty string.
func (c *ReplanCause) UnmarshalText(b []byte) error {
	return unmarshalEnum("replan cause", ErrInvalidEnum, c, b)
}

// CancelReason is the reason a Run is cancelled.
type CancelReason string

// The cancellation reasons.
const (
	// CancelReasonPlanRejected: the user rejected the plan.
	CancelReasonPlanRejected CancelReason = "plan_rejected"
	// CancelReasonUserRequest: the user requested cancellation.
	CancelReasonUserRequest CancelReason = "user_request"
)

var cancelReasons = []CancelReason{CancelReasonPlanRejected, CancelReasonUserRequest}

// CancelReasons returns every cancellation reason in declaration order. The
// caller may modify the returned slice.
func CancelReasons() []CancelReason { return slices.Clone(cancelReasons) }

// Valid reports whether r is a cancellation reason.
func (r CancelReason) Valid() bool { return slices.Contains(cancelReasons, r) }

// String returns the reason.
func (r CancelReason) String() string { return string(r) }

// MarshalText implements encoding.TextMarshaler and refuses an invalid reason.
func (r CancelReason) MarshalText() ([]byte, error) {
	return marshalEnum("cancel reason", ErrInvalidEnum, r)
}

// UnmarshalText implements encoding.TextUnmarshaler and refuses an invalid
// reason, including the empty string.
func (r *CancelReason) UnmarshalText(b []byte) error {
	return unmarshalEnum("cancel reason", ErrInvalidEnum, r, b)
}

// PhaseRecoveryCause is the condition under which a Phase enters Recovering.
// The causes correspond to the conditions of the Phase table in SPEC §6.
type PhaseRecoveryCause string

// The Phase recovery causes, grouped by the state that they leave.
const (
	// Running: execution failed, the output was invalid, or the Implementer
	// reported a status other than implemented.
	PhaseRecoveryCauseAttemptFailed  PhaseRecoveryCause = "attempt_failed"
	PhaseRecoveryCauseOutputInvalid  PhaseRecoveryCause = "output_invalid"
	PhaseRecoveryCauseStatusReported PhaseRecoveryCause = "status_reported"
	// Verifying: the change scope was violated, or the checks failed on the
	// candidate snapshot.
	PhaseRecoveryCauseScopeViolation PhaseRecoveryCause = "scope_violation"
	PhaseRecoveryCauseChecksFailed   PhaseRecoveryCause = "checks_failed"
	// Evaluating: the verdict was a rejection, or the Evaluator retries are
	// exhausted.
	PhaseRecoveryCauseRejected                  PhaseRecoveryCause = "rejected"
	PhaseRecoveryCauseEvaluatorRetriesExhausted PhaseRecoveryCause = "evaluator_retries_exhausted"
	// Integrating: a Git conflict, or the post-integration checks failed.
	PhaseRecoveryCauseIntegrationConflict     PhaseRecoveryCause = "integration_conflict"
	PhaseRecoveryCauseIntegrationChecksFailed PhaseRecoveryCause = "integration_checks_failed"
)

var phaseRecoveryCauses = []PhaseRecoveryCause{
	PhaseRecoveryCauseAttemptFailed, PhaseRecoveryCauseOutputInvalid, PhaseRecoveryCauseStatusReported,
	PhaseRecoveryCauseScopeViolation, PhaseRecoveryCauseChecksFailed,
	PhaseRecoveryCauseRejected, PhaseRecoveryCauseEvaluatorRetriesExhausted,
	PhaseRecoveryCauseIntegrationConflict, PhaseRecoveryCauseIntegrationChecksFailed,
}

// PhaseRecoveryCauses returns every Phase recovery cause in declaration
// order. The caller may modify the returned slice.
func PhaseRecoveryCauses() []PhaseRecoveryCause { return slices.Clone(phaseRecoveryCauses) }

// Valid reports whether c is a Phase recovery cause.
func (c PhaseRecoveryCause) Valid() bool { return slices.Contains(phaseRecoveryCauses, c) }

// String returns the cause.
func (c PhaseRecoveryCause) String() string { return string(c) }

// Source returns the Phase state that c leaves: Running, Verifying,
// Evaluating or Integrating. It returns the empty string for an invalid
// cause.
func (c PhaseRecoveryCause) Source() PhaseState {
	switch c {
	case PhaseRecoveryCauseAttemptFailed, PhaseRecoveryCauseOutputInvalid, PhaseRecoveryCauseStatusReported:
		return PhaseStateRunning
	case PhaseRecoveryCauseScopeViolation, PhaseRecoveryCauseChecksFailed:
		return PhaseStateVerifying
	case PhaseRecoveryCauseRejected, PhaseRecoveryCauseEvaluatorRetriesExhausted:
		return PhaseStateEvaluating
	case PhaseRecoveryCauseIntegrationConflict, PhaseRecoveryCauseIntegrationChecksFailed:
		return PhaseStateIntegrating
	}
	return ""
}

// MarshalText implements encoding.TextMarshaler and refuses an invalid cause.
func (c PhaseRecoveryCause) MarshalText() ([]byte, error) {
	return marshalEnum("phase recovery cause", ErrInvalidEnum, c)
}

// UnmarshalText implements encoding.TextUnmarshaler and refuses an invalid
// cause, including the empty string.
func (c *PhaseRecoveryCause) UnmarshalText(b []byte) error {
	return unmarshalEnum("phase recovery cause", ErrInvalidEnum, c, b)
}

// AbandonReason is the reason an Attempt is Abandoned. An abandoned Attempt
// does not consume failure or retry counts (SPEC §6).
type AbandonReason string

// The abandonment reasons.
const (
	// AbandonReasonLaunchNotOccurred: reconciliation confirmed that the
	// launch of a Scheduled Attempt did not occur.
	AbandonReasonLaunchNotOccurred AbandonReason = "launch_not_occurred"
	// AbandonReasonSessionUnavailable: the session of a WaitingForUser
	// Attempt cannot be restored.
	AbandonReasonSessionUnavailable AbandonReason = "session_unavailable"
)

var abandonReasons = []AbandonReason{AbandonReasonLaunchNotOccurred, AbandonReasonSessionUnavailable}

// AbandonReasons returns every abandonment reason in declaration order. The
// caller may modify the returned slice.
func AbandonReasons() []AbandonReason { return slices.Clone(abandonReasons) }

// Valid reports whether r is an abandonment reason.
func (r AbandonReason) Valid() bool { return slices.Contains(abandonReasons, r) }

// String returns the reason.
func (r AbandonReason) String() string { return string(r) }

// Source returns the Attempt state that r leaves: Scheduled for
// launch_not_occurred and WaitingForUser for session_unavailable. It returns
// the empty string for an invalid reason.
func (r AbandonReason) Source() AttemptState {
	switch r {
	case AbandonReasonLaunchNotOccurred:
		return AttemptStateScheduled
	case AbandonReasonSessionUnavailable:
		return AttemptStateWaitingForUser
	}
	return ""
}

// MarshalText implements encoding.TextMarshaler and refuses an invalid reason.
func (r AbandonReason) MarshalText() ([]byte, error) {
	return marshalEnum("abandon reason", ErrInvalidEnum, r)
}

// UnmarshalText implements encoding.TextUnmarshaler and refuses an invalid
// reason, including the empty string.
func (r *AbandonReason) UnmarshalText(b []byte) error {
	return unmarshalEnum("abandon reason", ErrInvalidEnum, r, b)
}

// ExecutionOutcome is the outcome of one agent execution as the host reports
// it (SPEC §11, ExecutionResult.Outcome). It is derived from the host's
// result, never from the exit code alone.
type ExecutionOutcome string

// The execution outcomes.
const (
	OutcomeCompleted     ExecutionOutcome = "completed"
	OutcomeLimitReached  ExecutionOutcome = "limit_reached"
	OutcomeOutputInvalid ExecutionOutcome = "output_invalid"
	OutcomeFailed        ExecutionOutcome = "failed"
	OutcomeInterrupted   ExecutionOutcome = "interrupted"
)

var executionOutcomes = []ExecutionOutcome{
	OutcomeCompleted, OutcomeLimitReached, OutcomeOutputInvalid, OutcomeFailed, OutcomeInterrupted,
}

// ExecutionOutcomes returns every execution outcome in declaration order. The
// caller may modify the returned slice.
func ExecutionOutcomes() []ExecutionOutcome { return slices.Clone(executionOutcomes) }

// Valid reports whether o is an execution outcome.
func (o ExecutionOutcome) Valid() bool { return slices.Contains(executionOutcomes, o) }

// String returns the outcome.
func (o ExecutionOutcome) String() string { return string(o) }

// MarshalText implements encoding.TextMarshaler and refuses an invalid outcome.
func (o ExecutionOutcome) MarshalText() ([]byte, error) {
	return marshalEnum("execution outcome", ErrInvalidEnum, o)
}

// UnmarshalText implements encoding.TextUnmarshaler and refuses an invalid
// outcome, including the empty string.
func (o *ExecutionOutcome) UnmarshalText(b []byte) error {
	return unmarshalEnum("execution outcome", ErrInvalidEnum, o, b)
}

// ErrorClass is the class of an error or outcome in the error table of SPEC
// §14.1. The set is provisional until M3 formalizes the error codes.
type ErrorClass string

// The error classes of SPEC §14.1.
const (
	ErrorClassTransientError          ErrorClass = "TransientError"
	ErrorClassRateLimitExceeded       ErrorClass = "RateLimitExceeded"
	ErrorClassInvalidAgentOutput      ErrorClass = "InvalidAgentOutput"
	ErrorClassVerificationFailed      ErrorClass = "VerificationFailed"
	ErrorClassSemanticRejection       ErrorClass = "SemanticRejection"
	ErrorClassPlanInvalid             ErrorClass = "PlanInvalid"
	ErrorClassSessionUnavailable      ErrorClass = "SessionUnavailable"
	ErrorClassAuthenticationFailed    ErrorClass = "AuthenticationFailed"
	ErrorClassMissingEnvironment      ErrorClass = "MissingEnvironment"
	ErrorClassWorkspaceConflict       ErrorClass = "WorkspaceConflict"
	ErrorClassScopeViolation          ErrorClass = "ScopeViolation"
	ErrorClassRoleContractViolation   ErrorClass = "RoleContractViolation"
	ErrorClassNeedsUserInput          ErrorClass = "NeedsUserInput"
	ErrorClassTurnLimitReached        ErrorClass = "TurnLimitReached"
	ErrorClassBudgetExceeded          ErrorClass = "BudgetExceeded"
	ErrorClassUnknownExecutionOutcome ErrorClass = "UnknownExecutionOutcome"
)

var errorClasses = []ErrorClass{
	ErrorClassTransientError, ErrorClassRateLimitExceeded, ErrorClassInvalidAgentOutput,
	ErrorClassVerificationFailed, ErrorClassSemanticRejection, ErrorClassPlanInvalid,
	ErrorClassSessionUnavailable, ErrorClassAuthenticationFailed, ErrorClassMissingEnvironment,
	ErrorClassWorkspaceConflict, ErrorClassScopeViolation, ErrorClassRoleContractViolation,
	ErrorClassNeedsUserInput, ErrorClassTurnLimitReached, ErrorClassBudgetExceeded,
	ErrorClassUnknownExecutionOutcome,
}

// ErrorClasses returns every error class in the order of SPEC §14.1. The
// caller may modify the returned slice.
func ErrorClasses() []ErrorClass { return slices.Clone(errorClasses) }

// Valid reports whether c is an error class.
func (c ErrorClass) Valid() bool { return slices.Contains(errorClasses, c) }

// String returns the class.
func (c ErrorClass) String() string { return string(c) }

// MarshalText implements encoding.TextMarshaler and refuses an invalid class.
func (c ErrorClass) MarshalText() ([]byte, error) {
	return marshalEnum("error class", ErrInvalidEnum, c)
}

// UnmarshalText implements encoding.TextUnmarshaler and refuses an invalid
// class, including the empty string.
func (c *ErrorClass) UnmarshalText(b []byte) error {
	return unmarshalEnum("error class", ErrInvalidEnum, c, b)
}
