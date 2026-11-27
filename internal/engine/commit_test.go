package engine

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shutx-net/bellona/internal/domain"
	"github.com/shutx-net/bellona/internal/fsm"
	"github.com/shutx-net/bellona/internal/projection"
)

// TestCommitRejectionLeavesEverythingUnchanged is R-FSM-02 at the commit: an
// event that the reducer rejects, or that does not validate, or whose
// artifacts do not add up, is refused before anything is written. The journal,
// the snapshot, the set of files and the in-memory state stay as they were,
// and the Run goes on working.
func TestCommitRejectionLeavesEverythingUnchanged(t *testing.T) {
	v := newEnv(t, completing(t))
	run := v.create()
	runDir := v.runDir(run.ID())
	driveToExecuting(t, run, phase1, phase2)
	readyPhase(t, run, phase1)

	// A planner Attempt that is Running, to refer to; and a file whose content
	// differs from the registered task.json.
	const running domain.AttemptID = "attempt-ffffffffffffff02"
	mustPropose(t, run, domain.AttemptScheduled{AttemptID: running, Role: domain.RolePlanner})
	mustPropose(t, run, domain.AttemptStarted{AttemptID: running})

	unbacked := refOf(t, newFile{Path: domain.AttemptOutputPath(running, 1), Data: []byte("{}\n")})
	other := newFile{Path: domain.TaskPath, Data: []byte("{\"another\":\"task\"}\n")}
	output := newFile{Path: domain.AttemptOutputPath(running, 1), Data: []byte("{}\n")}

	cases := []struct {
		name    string
		payload domain.Payload
		files   []newFile
		want    error   // the error must match this
		not     []error // and none of these
	}{
		{
			name:    "an unlisted Phase transition (Ready to Accepted)",
			payload: domain.PhaseAccepted{Revision: 1, PhaseID: phase1, AttemptID: plannerAttempt},
			want:    fsm.ErrIllegalTransition,
			not:     []error{projection.ErrInvalidEvent},
		},
		{
			name:    "an unlisted Run transition (Executing to Completed)",
			payload: domain.RunCompleted{},
			want:    fsm.ErrIllegalTransition,
			not:     []error{projection.ErrInvalidEvent},
		},
		{
			name:    "an unlisted Attempt transition (Running to Abandoned)",
			payload: domain.AttemptAbandoned{AttemptID: running, Reason: domain.AbandonReasonLaunchNotOccurred},
			want:    fsm.ErrIllegalTransition,
			not:     []error{projection.ErrInvalidEvent},
		},
		{
			name:    "an Attempt that does not exist",
			payload: domain.AttemptFinished{AttemptID: "attempt-00000000000000bd", Output: refOf(t, output)},
			files:   []newFile{output},
			want:    projection.ErrInvalidEvent,
			not:     []error{fsm.ErrIllegalTransition},
		},
		{
			name:    "a Phase that does not exist",
			payload: domain.PhaseReady{Revision: 1, PhaseID: "phase-99"},
			want:    projection.ErrInvalidEvent,
		},
		{
			name:    "a reference without a backing file",
			payload: domain.AttemptFinished{AttemptID: running, Output: unbacked},
			want:    ErrArtifactRefMismatch,
		},
		{
			name:    "a file whose bytes are not the referenced ones",
			payload: domain.AttemptFinished{AttemptID: running, Output: unbacked},
			files:   []newFile{{Path: unbacked.Path, Data: []byte("{ }\n")}},
			want:    ErrArtifactRefMismatch,
		},
		{
			name:    "a path that is already registered with other content",
			payload: domain.AttemptFinished{AttemptID: running, Output: refOf(t, other)},
			files:   []newFile{other},
			want:    projection.ErrInvalidEvent,
		},
		{
			name:    "a file that overwrites a registered artifact",
			payload: domain.PhaseReady{Revision: 1, PhaseID: phase2},
			files:   []newFile{other},
			want:    ErrArtifactRefMismatch,
		},
		{
			name:    "a file at a reserved path",
			payload: domain.PhaseReady{Revision: 1, PhaseID: phase2},
			files:   []newFile{{Path: domain.SnapshotPath, Data: []byte("{}")}},
			want:    projection.ErrInvalidEvent,
		},
		{
			name:    "a file at an invalid path",
			payload: domain.PhaseReady{Revision: 1, PhaseID: phase2},
			files:   []newFile{{Path: "../escape.json", Data: []byte("{}")}},
			want:    projection.ErrInvalidEvent,
		},
		{
			name:    "a file listed twice",
			payload: domain.PhaseReady{Revision: 1, PhaseID: phase2},
			files:   []newFile{{Path: "extra.json", Data: []byte("1")}, {Path: "extra.json", Data: []byte("1")}},
			want:    projection.ErrInvalidEvent,
		},
		{
			// The payload does not validate, so the event is refused by the
			// codec; the error still belongs to the reducer's class.
			name:    "a payload that does not validate (an Implementer without a revision)",
			payload: domain.AttemptScheduled{AttemptID: "attempt-00000000000000be", Role: domain.RoleImplementer, PhaseID: phase1},
			want:    projection.ErrInvalidEvent,
			not:     []error{fsm.ErrIllegalTransition},
		},
		{
			name:    "a payload that is a pointer",
			payload: &domain.PhaseReady{Revision: 1, PhaseID: phase2},
			want:    projection.ErrInvalidEvent,
		},
		{
			name:    "no payload",
			payload: nil,
			want:    projection.ErrInvalidEvent,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := fileSet(t, runDir)
			state := stateOf(t, run)
			events := len(readEvents(t, runDir))

			ev, err := run.propose(testCtx(t), tc.payload, tc.files...)
			if err == nil {
				t.Fatalf("the proposal was accepted as %+v", ev)
			}
			requireIs(t, err, tc.want)
			for _, not := range tc.not {
				if errors.Is(err, not) {
					t.Errorf("the error %v matches %v as well", err, not)
				}
			}
			if errors.Is(err, ErrRunFailed) {
				t.Errorf("a rejection stopped the Run handle: %v", err)
			}
			sameFiles(t, "a rejected proposal", files, fileSet(t, runDir))
			equalStates(t, "the state after a rejected proposal", state, stateOf(t, run))
			if got := len(readEvents(t, runDir)); got != events {
				t.Errorf("the journal has %d events, was %d", got, events)
			}
			equalStates(t, "state.json after a rejected proposal", state, snapshotOf(t, runDir))
		})
	}

	// The Run still works: the same Phase event that was refused above for
	// a bad file is accepted without it.
	mustPropose(t, run, domain.PhaseReady{Revision: 1, PhaseID: phase2})
	if ph, _ := stateOf(t, run).Phase(1, phase2); ph.State != domain.PhaseStateReady {
		t.Errorf("the Phase is %s, want Ready", ph.State)
	}
}

// TestCommitStateReturnsCopies: nothing a caller does to the value of State
// reaches the Run (R-FSM-03).
func TestCommitStateReturnsCopies(t *testing.T) {
	v := newEnv(t, completing(t))
	run := v.create()
	driveToExecuting(t, run, phase1, phase2)
	readyPhase(t, run, phase1)

	first := stateOf(t, run)
	want := stateOf(t, run) // a second, untouched copy
	first.RunID = "run-0000000000000000"
	first.LastSeq = 999
	first.Run.State = domain.RunStateCancelled
	first.Limits.MaxReplans = 99
	first.Run.Suspensions = append(first.Run.Suspensions, projection.Suspension{})
	first.Revisions[0].Phases[0].State = domain.PhaseStateFailed
	first.Revisions[0].Phases[1].DependsOn = append(first.Revisions[0].Phases[1].DependsOn, "phase-xx")
	first.Revisions[0].Phases[0].Attempts = append(first.Revisions[0].Phases[0].Attempts, "attempt-0000000000000000")
	first.Attempts[0].State = domain.AttemptStateCancelled
	first.Artifacts[0].Size = -1
	first.Artifacts = append(first.Artifacts[:1], first.Artifacts[2:]...)

	equalStates(t, "State after the caller changed a copy", want, stateOf(t, run))
	// A proposal still applies to the real state, not to the mutated copy.
	mustPropose(t, run, domain.PhaseReady{Revision: 1, PhaseID: phase2})
}

// TestCommitAppliesTheDecodedEvent: what the engine applies in memory is the
// event as it was encoded and decoded, so the live state cannot differ from a
// replay. The default clock reads time.Now, which carries a monotonic reading
// and a local zone; the event time is normalized to UTC without it.
func TestCommitAppliesTheDecodedEvent(t *testing.T) {
	v := newEnv(t, completing(t), func(o *Options) { o.Clock = nil })
	run := v.create()
	runDir := v.runDir(run.ID())
	driveToExecuting(t, run, phase1)
	ev := mustPropose(t, run, domain.PhaseReady{Revision: 1, PhaseID: phase1})

	events := readEvents(t, runDir)
	journaled := events[len(events)-1]
	if ev.Time.Location() != time.UTC || !ev.Time.Equal(ev.Time.Round(0)) {
		t.Errorf("the event time %v is not UTC without a monotonic reading", ev.Time)
	}
	got, err := domain.EncodeEvent(ev)
	if err != nil {
		t.Fatal(err)
	}
	want, err := domain.EncodeEvent(journaled)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) || ev.Seq != journaled.Seq || ev.Type != journaled.Type {
		t.Errorf("the committed event %s differs from the journaled one %s", got, want)
	}
	s := stateOf(t, run)
	if !s.Run.UpdatedAt.Equal(journaled.Time) {
		t.Errorf("the state was updated at %v, the event is from %v", s.Run.UpdatedAt, journaled.Time)
	}
	if ph, _ := s.Phase(1, phase1); !ph.UpdatedAt.Equal(journaled.Time) {
		t.Errorf("the Phase was updated at %v, the event is from %v", ph.UpdatedAt, journaled.Time)
	}
	equalStates(t, "the live state against Replay", s, replayed(t, runDir))
}

// TestCommitPolicyAmendment is R-CFG-01 and invariant 12: after the freeze the
// limits change only by a journaled PolicyAmended, and the frozen policy and
// manifest are never rewritten.
func TestCommitPolicyAmendment(t *testing.T) {
	v := newEnv(t, completing(t))
	ctx := testCtx(t)
	run := v.create()
	runDir := v.runDir(run.ID())
	driveToExecuting(t, run, phase1)

	amend := domain.PolicyAmended{Changes: []domain.LimitChange{{Limit: domain.LimitMaxReplans, Previous: "2", New: "5"}}}
	// Only a Blocked Run can be amended.
	before := fileSet(t, runDir)
	_, err := run.propose(ctx, amend)
	requireIs(t, err, projection.ErrInvalidEvent)
	sameFiles(t, "a refused amendment", before, fileSet(t, runDir))

	policy, manifestJSON := before[domain.PolicyPath], before[domain.ManifestPath]
	mustPropose(t, run, domain.RunSuspended{To: domain.RunStateBlocked, Reason: domain.SuspendReasonLimitReached, ResumeState: domain.RunStateExecuting})
	mustPropose(t, run, amend)
	s := stateOf(t, run)
	if s.Limits.MaxReplans != 5 || len(s.Amendments) != 1 {
		t.Fatalf("MaxReplans is %d with %d amendments", s.Limits.MaxReplans, len(s.Amendments))
	}
	files := fileSet(t, runDir)
	if !bytes.Equal(files[domain.PolicyPath], policy) || !bytes.Equal(files[domain.ManifestPath], manifestJSON) {
		t.Error("an amendment rewrote policy.json or manifest.json")
	}
	if frozen := readEvents(t, runDir)[1].Payload.(domain.ManifestFrozen); frozen.Limits.MaxReplans != 2 {
		t.Errorf("ManifestFrozen records MaxReplans %d, want the frozen 2", frozen.Limits.MaxReplans)
	}

	// The amended limit survives closing and opening the Run.
	if err := run.Close(ctx); err != nil {
		t.Fatal(err)
	}
	reopened, rep := v.open(v.eng, run.ID(), OpenParams{})
	if rep.SnapshotRebuilt || rep.RecoveredTail != nil {
		t.Errorf("Open repaired %+v", rep)
	}
	if got := stateOf(t, reopened).Limits.MaxReplans; got != 5 {
		t.Errorf("after Open MaxReplans is %d, want 5", got)
	}
	mustPropose(t, reopened, domain.RunResumed{To: domain.RunStateExecuting})
	if got := stateOf(t, reopened).Run.State; got != domain.RunStateExecuting {
		t.Errorf("the Run is %s, want Executing", got)
	}
	_, err = reopened.propose(ctx, domain.PolicyAmended{Changes: []domain.LimitChange{{Limit: domain.LimitMaxReplans, Previous: "5", New: "6"}}})
	requireIs(t, err, projection.ErrInvalidEvent)
	if got := stateOf(t, reopened).Limits.MaxReplans; got != 5 {
		t.Errorf("a refused amendment changed MaxReplans to %d", got)
	}
	final := fileSet(t, runDir)
	if !bytes.Equal(final[domain.PolicyPath], policy) || !bytes.Equal(final[domain.ManifestPath], manifestJSON) {
		t.Error("policy.json or manifest.json changed")
	}
	if _, err := os.Stat(filepath.Join(runDir, domain.PolicyPath)); err != nil {
		t.Error(err)
	}
}
