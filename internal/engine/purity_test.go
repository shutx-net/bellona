package engine

import (
	"bytes"
	"testing"

	"github.com/shutx-net/bellona/internal/domain"
	"github.com/shutx-net/bellona/internal/projection"
	"github.com/shutx-net/bellona/internal/testutil/gitrepo"
)

// completeRun creates a Run, takes it to Completed through the engine and
// closes it.
func completeRun(t *testing.T, v *env) domain.RunID {
	t.Helper()
	ctx := testCtx(t)
	run := v.create()
	driveToExecuting(t, run, phase1)
	readyPhase(t, run, phase1)
	id, err := run.StartAttempt(ctx, AttemptSpec{Role: domain.RoleImplementer, PhaseID: phase1})
	if err != nil {
		t.Fatal(err)
	}
	if a, err := run.AwaitAttempt(ctx, id); err != nil || a.State != domain.AttemptStateFinished {
		t.Fatalf("the Attempt: %v, %+v", err, a)
	}
	for _, p := range []domain.Payload{
		domain.PhaseVerificationStarted{Revision: 1, PhaseID: phase1, AttemptID: id},
		domain.PhaseIntegrationQueued{Revision: 1, PhaseID: phase1},
		domain.PhaseIntegrationStarted{Revision: 1, PhaseID: phase1},
		domain.PhaseAccepted{Revision: 1, PhaseID: phase1, AttemptID: id},
		domain.FinalVerificationStarted{},
		domain.RunCompleted{},
	} {
		mustPropose(t, run, p)
	}
	if err := run.Close(ctx); err != nil {
		t.Fatal(err)
	}
	return run.ID()
}

// TestPurityOfOpenAndReplay is R-PER-06 and T-13: opening and replaying a
// Completed Run has no side effects. The Run directory stays byte-identical,
// since there is nothing to recover; the runtime is never called; and a Git
// repository that is the working directory is untouched.
func TestPurityOfOpenAndReplay(t *testing.T) {
	rt := completing(t)
	v := newEnv(t, rt)
	ctx := testCtx(t)
	id := completeRun(t, v)
	runDir := v.runDir(id)
	closedState := replayed(t, runDir)

	repo := gitrepo.New(t)
	t.Chdir(repo)
	gitState := func() []string {
		return []string{
			gitrepo.Git(t, repo, "rev-parse", "HEAD"),
			gitrepo.Git(t, repo, "status", "--porcelain"),
			gitrepo.Git(t, repo, "for-each-ref"),
		}
	}
	headBefore := gitState()
	repoFiles := fileSet(t, repo) // after the Git commands above, which may refresh the index
	runFiles := fileSet(t, runDir)
	starts, resumes, calls := rt.Starts(), rt.Resumes(), len(rt.Calls())

	run, rep, err := v.eng.Open(ctx, id, OpenParams{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.RecoveredTail != nil || rep.SnapshotRebuilt || len(rep.UncertainAttempts) != 0 {
		t.Errorf("Open of a Completed Run reports %+v", rep)
	}
	state := stateOf(t, run)
	if state.Run.State != domain.RunStateCompleted {
		t.Errorf("the Run is %s", state.Run.State)
	}
	equalStates(t, "the opened state against Replay", closedState, state)
	// Replaying the same events again gives the same bytes.
	again := replayed(t, runDir)
	first, err1 := projection.Canonical(closedState)
	second, err2 := projection.Canonical(again)
	if err1 != nil || err2 != nil || !bytes.Equal(first, second) {
		t.Errorf("two replays differ: %v %v", err1, err2)
	}
	if err := run.Close(ctx); err != nil {
		t.Fatal(err)
	}

	sameFiles(t, "Open, State and Close of a Completed Run", runFiles, fileSet(t, runDir))
	sameFiles(t, "the Git repository, the working directory", repoFiles, fileSet(t, repo))
	after := gitState()
	for i := range headBefore {
		if headBefore[i] != after[i] {
			t.Errorf("the Git state changed: %q became %q", headBefore[i], after[i])
		}
	}
	if rt.Starts() != starts || rt.Resumes() != resumes || len(rt.Calls()) != calls || rt.Active() != 0 {
		t.Errorf("the runtime was used: starts %d to %d, resumes %d to %d, calls %d to %d, active %d",
			starts, rt.Starts(), resumes, rt.Resumes(), calls, len(rt.Calls()), rt.Active())
	}
}
