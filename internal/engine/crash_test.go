package engine

import (
	"bytes"
	"errors"
	"path"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/shutx-net/bellona/internal/domain"
	"github.com/shutx-net/bellona/internal/fsm"
	"github.com/shutx-net/bellona/internal/projection"
	"github.com/shutx-net/bellona/internal/runtime/fake"
	"github.com/shutx-net/bellona/internal/storage"
)

// This file is T-12 and the engine share of T-04: a crash during a commit, at
// every storage crash point, followed by Open. A crash is injected through the
// storage fault injector, which stops the operation where a crash would and
// leaves the files as it would; the Run handle then fails stop, Close writes
// nothing, and a fresh engine opens the Run. Three scenarios cover the
// commits that matter:
//
//	A: AttemptFinished, which finalizes an artifact (after the implementation
//	   completed),
//	B: AttemptScheduled, the launch intent (before the runtime is called),
//	C: AttemptStarted, the confirmation of the launch (after the runtime was
//	   called).

// outcome is what a crash must leave behind.
type outcome struct {
	attempt   domain.AttemptState // state of the Attempt after Open; "" when the Attempt does not exist
	tail      bool                // an incomplete trailing record is recovered
	rebuilt   string              // the snapshot issue, "" when no rebuild is needed
	orphan    bool                // an artifact file may exist that no event refers to
	committed bool                // the crashing event survived in the journal
}

// crashFixture is a Run on an engine whose storage crashes on demand.
type crashFixture struct {
	t      *testing.T
	v      *env
	rt     *fake.Runtime
	run    *Run
	runDir string
	armed  *atomic.Bool
	live   projection.State // the state before the crashing operation
	id     domain.AttemptID // the Implementer Attempt, when there is one
}

// newCrashFixture creates a Run in which phase-01 is Ready. The injector
// fires at point once armed.
func newCrashFixture(t *testing.T, point storage.CrashPoint, script func(fake.Call) fake.Behavior) *crashFixture {
	t.Helper()
	armed := new(atomic.Bool)
	rt := fakeRuntime(script)
	v := newEnv(t, rt, func(o *Options) {
		o.Faults = func(p storage.CrashPoint) bool { return armed.Load() && p == point }
	})
	run := v.create()
	driveToExecuting(t, run, phase1)
	readyPhase(t, run, phase1)
	return &crashFixture{t: t, v: v, rt: rt, run: run, runDir: v.runDir(run.ID()), armed: armed}
}

// phaseState is the state of phase-01.
func phaseState(t *testing.T, s projection.State) domain.PhaseState {
	t.Helper()
	ph, ok := s.Phase(1, phase1)
	if !ok {
		t.Fatal("no phase-01")
	}
	return ph.State
}

// crashed checks that the Run handle failed stop with the injected crash and
// that Close writes nothing and releases the lock. It returns the file set
// that the crash left.
func (f *crashFixture) crashed(err error) map[string][]byte {
	t := f.t
	t.Helper()
	requireIs(t, err, ErrRunFailed)
	requireIs(t, err, storage.ErrInjectedCrash)
	ctx := testCtx(t)
	if _, err := f.run.State(ctx); !isRunFailed(err) {
		t.Errorf("State after the crash: %v", err)
	}
	if _, err := f.run.propose(ctx, domain.PhaseReady{Revision: 1, PhaseID: "phase-02"}); !isRunFailed(err) {
		t.Errorf("propose after the crash: %v", err)
	}
	if _, err := f.run.StartAttempt(ctx, AttemptSpec{Role: domain.RoleImplementer, PhaseID: phase1}); !isRunFailed(err) {
		t.Errorf("StartAttempt after the crash: %v", err)
	}
	if _, err := f.run.AwaitAttempt(ctx, plannerAttempt); !isRunFailed(err) {
		t.Errorf("AwaitAttempt after the crash: %v", err)
	}
	before := fileSet(t, f.runDir)
	if err := f.run.Close(ctx); err != nil {
		t.Fatalf("Close after the crash: %v", err)
	}
	sameFiles(t, "Close after a crash", before, fileSet(t, f.runDir))
	return before
}

func isRunFailed(err error) bool { return err != nil && errors.Is(err, ErrRunFailed) }

// recover opens the Run with a fresh engine that has no faults and checks what
// is common to every crash: Open writes nothing but the repairs that it
// reports, deletes nothing, never calls the runtime, and reconstructs the state
// that the journal describes. It returns the reopened Run and the report.
func (f *crashFixture) recoverRun(before map[string][]byte) (*Run, OpenReport) {
	t := f.t
	t.Helper()
	starts, resumes := f.rt.Starts(), f.rt.Resumes()
	run, rep := f.v.open(f.v.another(), f.run.ID(), OpenParams{})
	after := fileSet(t, f.runDir)
	checkRecoveryFiles(t, before, after, rep)

	state := stateOf(t, run)
	equalStates(t, "Replay(events.jsonl) against the reopened state", replayed(t, f.runDir), state)
	equalStates(t, "state.json after Open", state, snapshotOf(t, f.runDir))
	if got := state.UncertainAttempts(); !slices.Equal(got, rep.UncertainAttempts) {
		t.Errorf("the report lists the uncertain Attempts %v, the state has %v", rep.UncertainAttempts, got)
	}
	if f.rt.Starts() != starts || f.rt.Resumes() != resumes {
		t.Errorf("Open called the runtime: starts %d to %d, resumes %d to %d", starts, f.rt.Starts(), resumes, f.rt.Resumes())
	}

	// Recovery is complete: closing and opening again finds nothing to repair
	// and writes nothing.
	if err := run.Close(testCtx(t)); err != nil {
		t.Fatal(err)
	}
	settled := fileSet(t, f.runDir)
	reopened, again := f.v.open(f.v.another(), f.run.ID(), OpenParams{})
	if again.RecoveredTail != nil || again.SnapshotRebuilt {
		t.Errorf("a second Open repairs again: %+v", again)
	}
	sameFiles(t, "a second Open", settled, fileSet(t, f.runDir))
	equalStates(t, "the state after a second Open", state, stateOf(t, reopened))
	return reopened, rep
}

// checkRecoveryFiles checks R-PER-10 for one Open: every file that existed
// still exists; nothing but events.jsonl (truncated, with the cut bytes
// preserved under logs/) and state.json (replaced, when the report says so)
// changed; the only new files are what the tail recovery reports.
func checkRecoveryFiles(t *testing.T, before, after map[string][]byte, rep OpenReport) {
	t.Helper()
	for p, b := range before {
		a, ok := after[p]
		switch {
		case !ok:
			t.Errorf("Open removed %s", p)
		case p == domain.EventsPath:
			if !bytes.HasPrefix(b, a) {
				t.Errorf("events.jsonl after Open is not a prefix of the file before")
			}
			if rep.RecoveredTail == nil && !bytes.Equal(a, b) {
				t.Errorf("events.jsonl changed without a recovered tail")
			}
		case p == domain.SnapshotPath:
			if !rep.SnapshotRebuilt && !bytes.Equal(a, b) {
				t.Errorf("state.json changed without a rebuild")
			}
		case !bytes.Equal(a, b):
			t.Errorf("Open changed %s", p)
		}
	}
	for p := range after {
		if _, ok := before[p]; ok {
			continue
		}
		if rep.RecoveredTail == nil || (p != "logs/" && p != rep.RecoveredTail.PreservedAs) {
			t.Errorf("Open created %s", p)
		}
	}
	if tail := rep.RecoveredTail; tail != nil {
		journalBefore, journalAfter := before[domain.EventsPath], after[domain.EventsPath]
		cut := journalBefore[len(journalAfter):]
		if int64(len(journalAfter)) != tail.Offset || int64(len(cut)) != tail.Length || len(cut) == 0 {
			t.Errorf("the tail recovery says offset %d, length %d; the journal was cut from %d to %d bytes",
				tail.Offset, tail.Length, len(journalBefore), len(journalAfter))
		}
		if preserved := after[tail.PreservedAs]; !bytes.Equal(preserved, cut) {
			t.Errorf("%s holds %q, the bytes cut off were %q", tail.PreservedAs, preserved, cut)
		}
		if _, err := domain.DecodeEvent(bytes.TrimSuffix(cut, []byte("\n"))); err == nil {
			t.Errorf("the preserved tail %q is a complete event", cut)
		}
	}
}

// checkOutcome compares the reopened Run with what the crash point must leave.
func checkOutcome(t *testing.T, f *crashFixture, run *Run, rep OpenReport, want outcome, journalBefore int) {
	t.Helper()
	s := stateOf(t, run)
	if (rep.RecoveredTail != nil) != want.tail {
		t.Errorf("recovered tail: %+v, want one: %v", rep.RecoveredTail, want.tail)
	}
	if rep.SnapshotIssue != want.rebuilt || rep.SnapshotRebuilt != (want.rebuilt != "") {
		t.Errorf("snapshot rebuilt %v for %q, want %q", rep.SnapshotRebuilt, rep.SnapshotIssue, want.rebuilt)
	}
	var a projection.Attempt
	var found bool
	if f.id != "" {
		a, found = s.Attempt(f.id)
	} else if ids := attemptsOfRole(s, domain.RoleImplementer); len(ids) == 1 {
		a, found = s.Attempt(ids[0])
	}
	if want.attempt == "" {
		if found {
			t.Errorf("the Attempt exists in state %s, want none", a.State)
		}
	} else {
		if !found || a.State != want.attempt {
			t.Fatalf("the Attempt is %q (found: %v), want %s", a.State, found, want.attempt)
		}
		uncertain := slices.Contains(rep.UncertainAttempts, a.ID)
		if wantUncertain := a.State != domain.AttemptStateFinished; uncertain != wantUncertain {
			t.Errorf("the Attempt is reported as uncertain: %v, want %v", uncertain, wantUncertain)
		}
	}
	if want.attempt == "" && len(rep.UncertainAttempts) != 0 {
		t.Errorf("uncertain Attempts %v, want none", rep.UncertainAttempts)
	}
	got := len(readEvents(t, f.runDir))
	if got != journalBefore && got != journalBefore+1 {
		t.Errorf("the journal has %d events, it had %d before the crashing commit", got, journalBefore)
	}
	if survived := got == journalBefore+1; survived != want.committed {
		t.Errorf("the crashing event survived in the journal: %v, want %v", survived, want.committed)
	}
}

// TestCrashDuringArtifactCommit is scenario A: AttemptFinished crashes while
// it finalizes the output, appends the event or updates the snapshot.
func TestCrashDuringArtifactCommit(t *testing.T) {
	for _, point := range storage.CrashPoints() {
		t.Run(point.String(), func(t *testing.T) {
			var want outcome
			switch point {
			case storage.CrashArtifactTempSynced, storage.CrashArtifactRenamed, storage.CrashArtifactDirSynced:
				want = outcome{attempt: domain.AttemptStateRunning, orphan: true}
			case storage.CrashEventTorn:
				want = outcome{attempt: domain.AttemptStateRunning, tail: true}
			case storage.CrashEventWritten, storage.CrashEventSynced, storage.CrashSnapshotTempSynced:
				want = outcome{attempt: domain.AttemptStateFinished, rebuilt: snapshotStale, committed: true}
			case storage.CrashSnapshotRenamed:
				want = outcome{attempt: domain.AttemptStateFinished, committed: true}
			default:
				t.Fatalf("no expectation for %s", point)
			}

			gate := make(chan struct{})
			started := make(chan fake.Call, 1)
			output := validOutput(t)
			f := newCrashFixture(t, point, func(fake.Call) fake.Behavior {
				return fake.Behavior{Gate: gate, Started: started, Output: output}
			})
			ctx := testCtx(t)
			id, err := f.run.StartAttempt(ctx, AttemptSpec{Role: domain.RoleImplementer, PhaseID: phase1})
			if err != nil {
				t.Fatal(err)
			}
			f.id = id
			<-started
			waitForAttempt(t, f.run, id, domain.AttemptStateRunning)
			f.live = stateOf(t, f.run)
			journalBefore := len(readEvents(t, f.runDir))

			// The implementation completes: the commit of AttemptFinished crashes.
			f.armed.Store(true)
			close(gate)
			_, err = f.run.AwaitAttempt(ctx, id)
			before := f.crashed(err)

			run, rep := f.recoverRun(before)
			checkOutcome(t, f, run, rep, want, journalBefore)
			s := stateOf(t, run)
			outputPath := domain.AttemptOutputPath(id, 1)
			registered := slices.ContainsFunc(s.Artifacts, func(r domain.ArtifactRef) bool { return r.Path == outputPath })
			if registered != want.committed {
				t.Errorf("the output is registered in the state: %v, want %v", registered, want.committed)
			}
			if !want.committed {
				// An orphan, if there is one, is not an accepted artifact and
				// no event refers to it.
				for _, ev := range readEvents(t, f.runDir) {
					if ev.Type == domain.EventAttemptFinished && ev.Payload.(domain.AttemptFinished).AttemptID == id {
						t.Error("AttemptFinished is in the journal")
					}
				}
				equalStates(t, "the state before the crash", f.live, s)
			}
			_, onDisk := before[outputPath]
			if want.committed && !onDisk {
				t.Error("the output of a committed AttemptFinished is not on disk")
			}
			if want.orphan {
				// The crash left a file that no event refers to: the temporary
				// file before the rename, the finished file after it.
				temp := false
				for p := range before {
					temp = temp || strings.HasPrefix(p, path.Dir(outputPath)+"/.output.json.tmp-")
				}
				if point == storage.CrashArtifactTempSynced && (onDisk || !temp) {
					t.Errorf("after a crash before the rename: output on disk %v, temporary file %v", onDisk, temp)
				}
				if point != storage.CrashArtifactTempSynced && !onDisk {
					t.Error("after a crash after the rename the output is not on disk")
				}
			}
			if f.rt.Starts() != 1 || f.rt.Resumes() != 0 {
				t.Errorf("the runtime was started %d times and resumed %d times, want 1 and 0", f.rt.Starts(), f.rt.Resumes())
			}
		})
	}
}

// TestCrashDuringLaunchIntent is scenario B: AttemptScheduled, the launch
// intent, crashes while the event is appended or the snapshot updated. This is
// T-12 "immediately after synchronizing AttemptScheduled" and "after event
// synchronization but before snapshot update": the Attempt is Scheduled, the
// snapshot is rebuilt, and the runtime is never called, before or after Open.
func TestCrashDuringLaunchIntent(t *testing.T) {
	cases := []struct {
		point storage.CrashPoint
		want  outcome
	}{
		{storage.CrashEventTorn, outcome{tail: true}},
		{storage.CrashEventWritten, outcome{attempt: domain.AttemptStateScheduled, rebuilt: snapshotStale, committed: true}},
		{storage.CrashEventSynced, outcome{attempt: domain.AttemptStateScheduled, rebuilt: snapshotStale, committed: true}},
		{storage.CrashSnapshotTempSynced, outcome{attempt: domain.AttemptStateScheduled, rebuilt: snapshotStale, committed: true}},
		{storage.CrashSnapshotRenamed, outcome{attempt: domain.AttemptStateScheduled, committed: true}},
	}
	for _, tc := range cases {
		t.Run(tc.point.String(), func(t *testing.T) {
			f := newCrashFixture(t, tc.point, nil)
			f.live = stateOf(t, f.run)
			journalBefore := len(readEvents(t, f.runDir))

			f.armed.Store(true)
			_, err := f.run.StartAttempt(testCtx(t), AttemptSpec{Role: domain.RoleImplementer, PhaseID: phase1})
			if err == nil {
				t.Fatal("StartAttempt succeeded although the commit of AttemptScheduled crashed")
			}
			if f.rt.Starts() != 0 {
				t.Fatalf("the runtime was started %d times although AttemptScheduled was not committed", f.rt.Starts())
			}
			before := f.crashed(err)

			run, rep := f.recoverRun(before)
			checkOutcome(t, f, run, rep, tc.want, journalBefore)
			s := stateOf(t, run)
			if tc.want.committed {
				if phaseState(t, s) != domain.PhaseStateRunning {
					t.Errorf("the Phase is %s, want Running", phaseState(t, s))
				}
			} else {
				if phaseState(t, s) != domain.PhaseStateReady {
					t.Errorf("the Phase is %s, want Ready", phaseState(t, s))
				}
				equalStates(t, "the state before the crash", f.live, s)
			}
			if f.rt.Starts() != 0 || f.rt.Resumes() != 0 {
				t.Errorf("the runtime was called: %d starts, %d resumes", f.rt.Starts(), f.rt.Resumes())
			}

			// T-04, duplicate launches. When the intent is in the journal the
			// Phase is Running, so a second Attempt for it is refused and
			// nothing is launched. When the intent is not in the journal, no
			// launch can have happened, and the Phase can be started once.
			ctx := testCtx(t)
			spec := AttemptSpec{Role: domain.RoleImplementer, PhaseID: phase1}
			if tc.want.committed {
				_, err := run.StartAttempt(ctx, spec)
				requireIs(t, err, fsm.ErrIllegalTransition)
				if f.rt.Starts() != 0 {
					t.Errorf("a duplicate launch: the runtime was started %d times", f.rt.Starts())
				}
				return
			}
			id, err := run.StartAttempt(ctx, spec)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := run.AwaitAttempt(ctx, id); err != nil {
				t.Fatal(err)
			}
			if f.rt.Starts() != 1 {
				t.Errorf("the runtime was started %d times, want 1", f.rt.Starts())
			}
		})
	}
}

// TestCrashAfterLaunch is scenario C: the agent was launched, and the commit of
// AttemptStarted crashes. The Attempt is Scheduled or Running after Open, is
// reported as uncertain, and is not launched again.
func TestCrashAfterLaunch(t *testing.T) {
	cases := []struct {
		point storage.CrashPoint
		want  outcome
	}{
		{storage.CrashEventTorn, outcome{attempt: domain.AttemptStateScheduled, tail: true}},
		{storage.CrashEventWritten, outcome{attempt: domain.AttemptStateRunning, rebuilt: snapshotStale, committed: true}},
		{storage.CrashEventSynced, outcome{attempt: domain.AttemptStateRunning, rebuilt: snapshotStale, committed: true}},
		{storage.CrashSnapshotTempSynced, outcome{attempt: domain.AttemptStateRunning, rebuilt: snapshotStale, committed: true}},
		{storage.CrashSnapshotRenamed, outcome{attempt: domain.AttemptStateRunning, committed: true}},
	}
	for _, tc := range cases {
		t.Run(tc.point.String(), func(t *testing.T) {
			started := make(chan fake.Call) // unbuffered: the worker waits here until the test receives
			output := validOutput(t)
			f := newCrashFixture(t, tc.point, func(fake.Call) fake.Behavior {
				return fake.Behavior{Started: started, Output: output}
			})
			ctx := testCtx(t)

			// AttemptScheduled is committed when StartAttempt returns. The worker
			// is held inside Start, so arming now makes the next commit,
			// AttemptStarted, the one that crashes.
			id, err := f.run.StartAttempt(ctx, AttemptSpec{Role: domain.RoleImplementer, PhaseID: phase1})
			if err != nil {
				t.Fatal(err)
			}
			f.id = id
			f.live = stateOf(t, f.run)
			journalBefore := len(readEvents(t, f.runDir))
			f.armed.Store(true)
			<-started
			_, err = f.run.AwaitAttempt(ctx, id)
			before := f.crashed(err)

			run, rep := f.recoverRun(before)
			checkOutcome(t, f, run, rep, tc.want, journalBefore)
			if !tc.want.committed {
				equalStates(t, "the state before the crash", f.live, stateOf(t, run))
			}
			if f.rt.Starts() != 1 || f.rt.Resumes() != 0 {
				t.Errorf("the runtime was started %d times and resumed %d times, want 1 and 0", f.rt.Starts(), f.rt.Resumes())
			}
		})
	}
}
