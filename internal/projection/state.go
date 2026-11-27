// Package projection derives the state of a Run from its committed events. It
// holds the pure reducer (Apply and Replay), the projected State, and the
// shape of the state.json snapshot (SPEC §12).
//
// The reducer is the single place where "the engine rejects every transition
// that is not listed" (R-FSM-02) is decided for committed events: the engine
// calls Apply before it commits an event, and recovery calls Replay on the
// journal, so both use the same checks and the same fsm tables. The package
// reads no clock, draws no randomness, touches no file, Git repository or
// network, and has no logger (R-PER-06): every timestamp in a State is the
// time of the event that caused the change, and the same sequence of events
// always yields the same State.
//
// A State holds the three machines side by side and never folds one into
// another (R-FSM-01): Run.State is the state of the Run, Phase.State the state
// of one Phase of one Plan Revision, and Attempt.State the state of one
// Attempt. The State is also the content of state.json: it is derived from
// events.jsonl, which stays the authority for the Run (R-PER-02).
package projection

import (
	"bytes"
	"slices"
	"time"

	"github.com/shutx-net/bellona/internal/domain"
	"github.com/shutx-net/bellona/internal/strictjson"
)

// State is the state of one Run as projected from its journal. It is also the
// content of state.json (see Snapshot). Every type in it follows the contract
// convention of package strictjson. Slices keep commit order, so the encoding
// is deterministic.
type State struct {
	RunID domain.RunID `json:"runId"`
	// LastSeq is the sequence number of the last applied event; it is 0 only
	// in the empty State before RunCreated.
	LastSeq uint64 `json:"lastSeq"`
	Run     Run    `json:"run"`
	// Limits are the execution limits in force: those of ManifestFrozen with
	// every PolicyAmended applied (SPEC §6.3).
	Limits     domain.Limits `json:"limits"`
	Amendments []Amendment   `json:"amendments"`
	Revisions  []Revision    `json:"revisions"`
	Attempts   []Attempt     `json:"attempts"`
	// Artifacts lists every artifact that a committed event refers to, in
	// commit order and unique by path. A path is bound to one size and one
	// hash for the life of the Run (R-PER-07).
	Artifacts []domain.ArtifactRef `json:"artifacts"`
}

// Run is the state of the Run machine and the data around it.
type Run struct {
	State domain.RunState `json:"state"`
	// Suspensions is a stack. Entering a suspended state pushes the state that
	// the Run left, which may itself be suspended; resuming pops it, so the
	// recorded resume state is always the ResumeState of the top entry.
	Suspensions []Suspension       `json:"suspensions"`
	CreatedAt   time.Time          `json:"createdAt"`
	UpdatedAt   time.Time          `json:"updatedAt"`
	Task        domain.ArtifactRef `json:"task"`
	Manifest    domain.ArtifactRef `json:"manifest,omitzero"`
	Policy      domain.ArtifactRef `json:"policy,omitzero"`
	// PlanRevision is the number of the current Plan Revision; 0 until the
	// first plan is accepted.
	PlanRevision int `json:"planRevision"`
	// PendingPlan is the plan candidate under validation or awaiting approval.
	PendingPlan domain.ArtifactRef `json:"pendingPlan,omitzero"`
	// ReplanCount counts returns to Planning for a new Plan Revision.
	ReplanCount int `json:"replanCount"`
	// ChangeRequestCount counts plan approval responses that asked for
	// changes; they do not consume the replan counter (SPEC §6).
	ChangeRequestCount int                 `json:"changeRequestCount"`
	CancelReason       domain.CancelReason `json:"cancelReason,omitzero"`
}

// Suspension records one entry into a suspended Run state (SPEC §6).
type Suspension struct {
	// State is the suspended state: WaitingForUser, Blocked or
	// RecoveryRequired.
	State domain.RunState `json:"state"`
	// ResumeState is the state that a RunResumed returns to.
	ResumeState domain.RunState      `json:"resumeState"`
	Reason      domain.SuspendReason `json:"reason"`
	Detail      string               `json:"detail,omitzero"`
	RequestID   domain.RequestID     `json:"requestId,omitzero"`
	// Seq and Since are the sequence number and the time of the RunSuspended
	// event.
	Seq   uint64    `json:"seq"`
	Since time.Time `json:"since"`
}

// Amendment records one PolicyAmended event.
type Amendment struct {
	Seq     uint64               `json:"seq"`
	Time    time.Time            `json:"time"`
	Changes []domain.LimitChange `json:"changes"`
}

// Revision is one accepted Plan Revision with the Phases that it defines.
type Revision struct {
	Number     int                `json:"number"`
	Plan       domain.ArtifactRef `json:"plan"`
	AcceptedAt time.Time          `json:"acceptedAt"`
	Phases     []Phase            `json:"phases"`
}

// Phase is the state of the Phase machine for one Phase of one revision.
type Phase struct {
	ID        domain.PhaseID    `json:"id"`
	DependsOn []domain.PhaseID  `json:"dependsOn"`
	State     domain.PhaseState `json:"state"`
	// ResumeState is the state to return to while the Phase is WaitingForUser;
	// it is empty in every other state.
	ResumeState domain.PhaseState `json:"resumeState,omitzero"`
	// RecoveryCause is the cause of the latest entry into Recovering.
	RecoveryCause domain.PhaseRecoveryCause `json:"recoveryCause,omitzero"`
	// Attempts lists the Attempts scheduled for the Phase (Implementer
	// Attempts and Evaluator Attempts of its review) in commit order.
	Attempts  []domain.AttemptID `json:"attempts"`
	UpdatedAt time.Time          `json:"updatedAt"`
}

// Attempt is the state of the Attempt machine for one Attempt.
type Attempt struct {
	ID   domain.AttemptID `json:"id"`
	Role domain.Role      `json:"role"`
	// Revision and PhaseID are set for Attempts that work for a Phase.
	Revision  int                 `json:"revision,omitzero"`
	PhaseID   domain.PhaseID      `json:"phaseId,omitzero"`
	SessionID domain.SessionID    `json:"sessionId,omitzero"`
	State     domain.AttemptState `json:"state"`
	// Output is the validated output of a Finished Attempt.
	Output     domain.ArtifactRef      `json:"output,omitzero"`
	Outcome    domain.ExecutionOutcome `json:"outcome,omitzero"`
	ErrorClass domain.ErrorClass       `json:"errorClass,omitzero"`
	Detail     string                  `json:"detail,omitzero"`
	// RequestID is the request that the Attempt last waited for.
	RequestID     domain.RequestID     `json:"requestId,omitzero"`
	AbandonReason domain.AbandonReason `json:"abandonReason,omitzero"`
	ScheduledAt   time.Time            `json:"scheduledAt"`
	StartedAt     time.Time            `json:"startedAt,omitzero"`
	EndedAt       time.Time            `json:"endedAt,omitzero"`
	UpdatedAt     time.Time            `json:"updatedAt"`
}

// empty reports whether s is the empty State, the state before RunCreated.
func (s State) empty() bool {
	return s.LastSeq == 0 && s.RunID == "" && s.Run.State == "" &&
		len(s.Run.Suspensions) == 0 && len(s.Amendments) == 0 && len(s.Revisions) == 0 &&
		len(s.Attempts) == 0 && len(s.Artifacts) == 0
}

// Clone returns a deep copy of s that shares no memory with it. Nil and empty
// slices stay what they are. The copy is written out by hand, so a field added
// to a type must be added here as well; a test fails when a slice is shared.
func (s State) Clone() State {
	c := s
	c.Run.Suspensions = slices.Clone(s.Run.Suspensions)
	if s.Amendments != nil {
		c.Amendments = make([]Amendment, len(s.Amendments))
		for i, a := range s.Amendments {
			a.Changes = slices.Clone(a.Changes)
			c.Amendments[i] = a
		}
	}
	if s.Revisions != nil {
		c.Revisions = make([]Revision, len(s.Revisions))
		for i, r := range s.Revisions {
			r.Phases = clonePhases(r.Phases)
			c.Revisions[i] = r
		}
	}
	c.Attempts = slices.Clone(s.Attempts)
	c.Artifacts = slices.Clone(s.Artifacts)
	return c
}

func clonePhases(phases []Phase) []Phase {
	if phases == nil {
		return nil
	}
	out := make([]Phase, len(phases))
	for i, p := range phases {
		p.DependsOn = slices.Clone(p.DependsOn)
		p.Attempts = slices.Clone(p.Attempts)
		out[i] = p
	}
	return out
}

// Attempt returns a copy of the Attempt with the given ID.
func (s State) Attempt(id domain.AttemptID) (Attempt, bool) {
	for _, a := range s.Attempts {
		if a.ID == id {
			return a, true
		}
	}
	return Attempt{}, false
}

// Phase returns a copy of the Phase with the given ID in the given Plan
// Revision. The copy does not share its slices with s.
func (s State) Phase(revision int, id domain.PhaseID) (Phase, bool) {
	for _, r := range s.Revisions {
		if r.Number != revision {
			continue
		}
		for _, p := range r.Phases {
			if p.ID == id {
				return clonePhases([]Phase{p})[0], true
			}
		}
	}
	return Phase{}, false
}

// CurrentRevision returns a copy of the latest accepted Plan Revision, which is
// the one that Run.PlanRevision names. The copy does not share its slices with
// s.
func (s State) CurrentRevision() (Revision, bool) {
	if len(s.Revisions) == 0 {
		return Revision{}, false
	}
	r := s.Revisions[len(s.Revisions)-1]
	r.Phases = clonePhases(r.Phases)
	return r, true
}

// UncertainAttempts returns the IDs of the Attempts whose external execution
// may be in progress or whose outcome is not known: those in Scheduled, Running
// or Unknown, in commit order. Recovery must reconcile them before it reruns
// anything (SPEC §12.1).
func (s State) UncertainAttempts() []domain.AttemptID {
	var out []domain.AttemptID
	for _, a := range s.Attempts {
		switch a.State {
		case domain.AttemptStateScheduled, domain.AttemptStateRunning, domain.AttemptStateUnknown:
			out = append(out, a.ID)
		}
	}
	return out
}

// Canonical returns the canonical encoding of s: the deterministic JSON of
// package strictjson. It is the definition of state equality (see Equal),
// because a live State and a decoded one can differ in nil versus empty
// slices, which have the same encoding. The empty State, which is the State
// before RunCreated and is not a valid snapshot content, encodes as {}.
// Any other State that cannot be encoded, for example one with an invalid
// enumeration value, yields an error.
func Canonical(s State) ([]byte, error) {
	if s.empty() {
		return []byte("{}"), nil
	}
	return strictjson.Encode(s)
}

// Equal reports whether a and b have the same canonical encoding. It reports
// false when either cannot be encoded: two broken States are not equal.
func Equal(a, b State) bool {
	ab, err := Canonical(a)
	if err != nil {
		return false
	}
	bb, err := Canonical(b)
	if err != nil {
		return false
	}
	return bytes.Equal(ab, bb)
}

// attemptIndex returns the index of the Attempt in s.Attempts, or -1.
func (s *State) attemptIndex(id domain.AttemptID) int {
	return slices.IndexFunc(s.Attempts, func(a Attempt) bool { return a.ID == id })
}

// attempt returns a pointer to the Attempt in s.Attempts, or nil. The pointer
// is valid until the slice grows.
func (s *State) attempt(id domain.AttemptID) *Attempt {
	if i := s.attemptIndex(id); i >= 0 {
		return &s.Attempts[i]
	}
	return nil
}

// revision returns a pointer to the revision with the given number, or nil.
func (s *State) revision(number int) *Revision {
	for i := range s.Revisions {
		if s.Revisions[i].Number == number {
			return &s.Revisions[i]
		}
	}
	return nil
}

// phase returns a pointer to the Phase in the given revision, or nil.
func (s *State) phase(revision int, id domain.PhaseID) *Phase {
	r := s.revision(revision)
	if r == nil {
		return nil
	}
	for i := range r.Phases {
		if r.Phases[i].ID == id {
			return &r.Phases[i]
		}
	}
	return nil
}
