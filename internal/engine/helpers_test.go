package engine

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shutx-net/bellona/internal/agent"
	"github.com/shutx-net/bellona/internal/artifact"
	"github.com/shutx-net/bellona/internal/config"
	"github.com/shutx-net/bellona/internal/domain"
	"github.com/shutx-net/bellona/internal/projection"
	"github.com/shutx-net/bellona/internal/runtime/fake"
	"github.com/shutx-net/bellona/internal/storage"
	"github.com/shutx-net/bellona/internal/strictjson"
)

const (
	phase1 domain.PhaseID = "phase-01"
	phase2 domain.PhaseID = "phase-02"

	// plannerAttempt is the ID of the planner Attempt that driveToExecuting
	// proposes. The engine draws its own Attempt IDs at random, so no clash.
	plannerAttempt domain.AttemptID = "attempt-ffffffffffffff01"

	testRequest = "implement the requested change"
)

// ---------------------------------------------------------------------------
// Environment
// ---------------------------------------------------------------------------

// testClock returns strictly increasing UTC times and is safe for concurrent
// use, because the control loops of several Runs call it.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock() *testClock {
	return &testClock{now: time.Date(2026, 11, 28, 0, 0, 0, 123456789, time.UTC)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(time.Second)
	return c.now
}

// detRand is a deterministic source of random bytes. The engine serializes the
// reads, so it needs no locking here.
func detRand(seed byte) io.Reader {
	var key [32]byte
	key[0] = seed
	return rand.NewChaCha8(key)
}

// env is an Engine on a temporary state root with a fake runtime.
type env struct {
	t    *testing.T
	root string
	rt   *fake.Runtime
	eng  *Engine
	cfg  config.Config
}

// newEnv makes an engine whose state root is a fresh temporary directory. The
// environment variables that name the state directory are redirected too, so
// that nothing can reach the real one. rt may be nil.
func newEnv(t *testing.T, rt *fake.Runtime, mods ...func(*Options)) *env {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	root := filepath.Join(t.TempDir(), "state")
	opts := Options{StateRoot: root, Clock: newTestClock().Now, Rand: detRand(1)}
	if rt != nil {
		opts.Runtime = rt
	}
	for _, mod := range mods {
		mod(&opts)
	}
	eng, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Resolve(config.Inputs{})
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, root: root, rt: rt, eng: eng, cfg: cfg}
}

// another returns an engine on the same state root with its own options.
func (v *env) another(mods ...func(*Options)) *Engine {
	v.t.Helper()
	opts := Options{StateRoot: v.root, Clock: newTestClock().Now, Rand: detRand(2)}
	if v.rt != nil {
		opts.Runtime = v.rt
	}
	for _, mod := range mods {
		mod(&opts)
	}
	eng, err := New(opts)
	if err != nil {
		v.t.Fatal(err)
	}
	return eng
}

func (v *env) runDir(id domain.RunID) string { return storage.RunDir(v.root, id) }

// create makes a Run and closes it when the test ends.
func (v *env) create() *Run {
	v.t.Helper()
	return v.createWith(v.eng)
}

func (v *env) createWith(eng *Engine) *Run {
	v.t.Helper()
	run, err := eng.Create(testCtx(v.t), CreateParams{Request: testRequest, Config: v.cfg})
	if err != nil {
		v.t.Fatalf("Create: %v", err)
	}
	closeAtEnd(v.t, run)
	return run
}

// open opens a Run and closes it when the test ends.
func (v *env) open(eng *Engine, id domain.RunID, p OpenParams) (*Run, OpenReport) {
	v.t.Helper()
	run, rep, err := eng.Open(testCtx(v.t), id, p)
	if err != nil {
		v.t.Fatalf("Open: %v", err)
	}
	closeAtEnd(v.t, run)
	return run, rep
}

// closeAtEnd releases the Run when the test ends. The context is already done,
// so a Run that still has a running execution is stopped at once.
func closeAtEnd(t testing.TB, run *Run) {
	t.Cleanup(func() {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_ = run.Close(ctx)
	})
}

// testCtx returns a context that bounds the whole test, so that a deadlock
// fails the test instead of hanging it.
func testCtx(t testing.TB) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// ---------------------------------------------------------------------------
// The fake runtime
// ---------------------------------------------------------------------------

// fakeRuntime is a fake runtime that advertises SessionIDBeforeLaunch.
func fakeRuntime(script func(fake.Call) fake.Behavior) *fake.Runtime {
	return fake.New(agent.Capabilities{SessionIDBeforeLaunch: true, Resume: true}, script)
}

// validOutput returns the bytes of a valid Implementation result.
func validOutput(t testing.TB) []byte {
	t.Helper()
	b, err := strictjson.Encode(artifact.ImplementationResult{
		SchemaVersion:        artifact.ImplementationResultSchemaVersion,
		Status:               artifact.StatusImplemented,
		Summary:              "implemented the requested change",
		ReportedChangedFiles: []string{"internal/a.go", "README.md"},
		Issues:               []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// completing returns a fake runtime whose every execution completes at once
// with a valid Implementation result.
func completing(t testing.TB) *fake.Runtime {
	out := validOutput(t)
	return fakeRuntime(func(fake.Call) fake.Behavior { return fake.Behavior{Output: out} })
}

// ---------------------------------------------------------------------------
// Driving a Run
// ---------------------------------------------------------------------------

// refOf returns the reference of a file.
func refOf(t testing.TB, f newFile) domain.ArtifactRef {
	t.Helper()
	ref, err := domain.NewArtifactRef(f.Path, f.Data)
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

// mustPropose commits p and fails the test when it is rejected.
func mustPropose(t testing.TB, run *Run, p domain.Payload, files ...newFile) domain.Event {
	t.Helper()
	ev, err := run.propose(testCtx(t), p, files...)
	if err != nil {
		t.Fatalf("propose %s: %v", p.EventType(), err)
	}
	return ev
}

// stateOf returns the state of the Run.
func stateOf(t testing.TB, run *Run) projection.State {
	t.Helper()
	s, err := run.State(testCtx(t))
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	return s
}

// driveToExecuting takes a Created Run to Executing with plan revision 1 whose
// Phases have no dependencies: RunPrepared, StrategySelected, a planner
// Attempt that is scheduled, started and finished with a plan candidate,
// PlanGenerated, and PlanAccepted. Every event goes through the commit path.
func driveToExecuting(t testing.TB, run *Run, phaseIDs ...domain.PhaseID) {
	t.Helper()
	mustPropose(t, run, domain.RunPrepared{})
	mustPropose(t, run, domain.StrategySelected{})
	mustPropose(t, run, domain.AttemptScheduled{AttemptID: plannerAttempt, Role: domain.RolePlanner})
	mustPropose(t, run, domain.AttemptStarted{AttemptID: plannerAttempt})
	candidate := newFile{Path: domain.AttemptOutputPath(plannerAttempt, 1), Data: []byte("{\"candidate\":true}\n")}
	candidateRef := refOf(t, candidate)
	mustPropose(t, run, domain.AttemptFinished{AttemptID: plannerAttempt, Output: candidateRef}, candidate)
	mustPropose(t, run, domain.PlanGenerated{AttemptID: plannerAttempt, Artifact: candidateRef})
	defs := make([]domain.PhaseDefinition, len(phaseIDs))
	for i, id := range phaseIDs {
		defs[i] = domain.PhaseDefinition{ID: id, DependsOn: []domain.PhaseID{}}
	}
	plan := newFile{Path: domain.PlanPath(1), Data: []byte("{\"plan\":1}\n")}
	mustPropose(t, run, domain.PlanAccepted{Revision: 1, Plan: refOf(t, plan), Phases: defs}, plan)
}

// readyPhase proposes PhaseReady for a Phase of revision 1.
func readyPhase(t testing.TB, run *Run, id domain.PhaseID) {
	t.Helper()
	mustPropose(t, run, domain.PhaseReady{Revision: 1, PhaseID: id})
}

// readEvents decodes events.jsonl of a Run directory line by line. It reads the
// file directly and uses no storage code, so it checks what the engine wrote.
func readEvents(t testing.TB, runDir string) []domain.Event {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(runDir, domain.EventsPath))
	if err != nil {
		t.Fatal(err)
	}
	var events []domain.Event
	for i, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		ev, err := domain.DecodeEvent([]byte(line))
		if err != nil {
			t.Fatalf("events.jsonl line %d: %v", i+1, err)
		}
		events = append(events, ev)
	}
	return events
}

// replayed returns the state that the events of a Run directory produce.
func replayed(t testing.TB, runDir string) projection.State {
	t.Helper()
	s, err := projection.Replay(readEvents(t, runDir))
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	return s
}

// snapshotOf decodes state.json of a Run directory.
func snapshotOf(t testing.TB, runDir string) projection.State {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(runDir, domain.SnapshotPath))
	if err != nil {
		t.Fatal(err)
	}
	s, err := projection.DecodeSnapshot(data)
	if err != nil {
		t.Fatalf("DecodeSnapshot: %v", err)
	}
	return s
}

// fileSet returns every file of dir with its bytes, for the byte-identity and
// no-deletion checks. It does not follow symbolic links: a link is recorded
// with its target, and a directory under a key that ends in a slash.
func fileSet(t testing.TB, dir string) map[string][]byte {
	t.Helper()
	out := make(map[string][]byte)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		switch {
		case rel == ".":
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			out[rel] = []byte("symlink to " + target)
		case d.IsDir():
			out[rel+"/"] = nil
		case d.Type().IsRegular():
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			out[rel] = data
		default:
			out[rel] = []byte("irregular file " + d.Type().String())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// sameFiles fails the test unless the two file sets are identical.
func sameFiles(t testing.TB, what string, before, after map[string][]byte) {
	t.Helper()
	for p, b := range before {
		a, ok := after[p]
		switch {
		case !ok:
			t.Errorf("%s: %s disappeared", what, p)
		case !bytes.Equal(a, b):
			t.Errorf("%s: %s changed", what, p)
		}
	}
	for p := range after {
		if _, ok := before[p]; !ok {
			t.Errorf("%s: %s appeared", what, p)
		}
	}
}

// equalStates fails the test unless the two states are equal under
// projection.Equal.
func equalStates(t testing.TB, what string, want, got projection.State) {
	t.Helper()
	if projection.Equal(want, got) {
		return
	}
	w, _ := projection.Canonical(want)
	g, _ := projection.Canonical(got)
	t.Errorf("%s: the states differ\nwant %s\ngot  %s", what, w, g)
}

// requireIs fails the test unless err matches want.
func requireIs(t testing.TB, err, want error) {
	t.Helper()
	if err == nil || !errors.Is(err, want) {
		t.Fatalf("error %v does not match %v", err, want)
	}
}

// waitFor polls cond, without sleeping, until it holds or the test context
// ends. cond must take part in the synchronization that it waits for, for
// example by asking the control loop.
func waitFor(t testing.TB, what string, cond func() bool) {
	t.Helper()
	ctx := testCtx(t)
	for !cond() {
		if ctx.Err() != nil {
			t.Fatalf("timed out waiting for %s", what)
		}
		runtime.Gosched()
	}
}

// waitForAttempt waits until the Attempt of the Run is in the state.
func waitForAttempt(t testing.TB, run *Run, id domain.AttemptID, state domain.AttemptState) {
	t.Helper()
	ctx := testCtx(t)
	waitFor(t, "attempt "+string(id)+" to be "+string(state), func() bool {
		s, err := run.State(ctx)
		if err != nil {
			t.Fatalf("State: %v", err)
		}
		a, ok := s.Attempt(id)
		return ok && a.State == state
	})
}

// attemptsOfRole returns the IDs of the Attempts with the role, in order.
func attemptsOfRole(s projection.State, role domain.Role) []domain.AttemptID {
	var ids []domain.AttemptID
	for _, a := range s.Attempts {
		if a.Role == role {
			ids = append(ids, a.ID)
		}
	}
	return ids
}

// eventTypes returns the types of the events of a Run directory.
func eventTypes(t testing.TB, runDir string) []domain.EventType {
	t.Helper()
	var types []domain.EventType
	for _, ev := range readEvents(t, runDir) {
		types = append(types, ev.Type)
	}
	return types
}

// copyDir copies a directory tree of regular files and directories.
func copyDir(t testing.TB, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
}
