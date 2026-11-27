package engine

import (
	"errors"
	"fmt"
	"runtime"
	"slices"
	"sync"
	"testing"

	"github.com/shutx-net/bellona/internal/domain"
	"github.com/shutx-net/bellona/internal/fsm"
	"github.com/shutx-net/bellona/internal/runtime/fake"
)

// readState starts goroutines that call State in a loop until the returned
// function is called. Each also changes the copy it gets, which must reach
// nothing. The function waits for the goroutines and returns their errors.
func readState(t *testing.T, run *Run, readers int) (stop func() []error) {
	t.Helper()
	ctx := testCtx(t)
	quit := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error
	for range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var last uint64
			for {
				select {
				case <-quit:
					return
				default:
				}
				s, err := run.State(ctx)
				if err != nil {
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
					return
				}
				if s.LastSeq < last {
					mu.Lock()
					errs = append(errs, fmt.Errorf("State went back from event %d to %d", last, s.LastSeq))
					mu.Unlock()
					return
				}
				last = s.LastSeq
				s.Run.State = ""
				s.Attempts = nil
				runtime.Gosched()
			}
		}()
	}
	return func() []error {
		close(quit)
		wg.Wait()
		return errs
	}
}

// assertContiguous checks that the journal's sequence numbers run 1, 2, 3 and so
// on, and that Replay, the live state and state.json agree.
func assertContiguous(t *testing.T, run *Run, runDir string) {
	t.Helper()
	events := readEvents(t, runDir)
	for i, ev := range events {
		if ev.Seq != uint64(i+1) {
			t.Fatalf("event %d has the sequence number %d", i+1, ev.Seq)
		}
	}
	live := stateOf(t, run)
	if live.LastSeq != uint64(len(events)) {
		t.Errorf("the state is at event %d, the journal has %d", live.LastSeq, len(events))
	}
	equalStates(t, "Replay against the live state", live, replayed(t, runDir))
	equalStates(t, "state.json against the live state", live, snapshotOf(t, runDir))
}

// TestConcurrentAttempts is the M0 share of T-20 and R-SCH-07: four
// Attempts for four independent Phases run at the same time, their workers
// report through channels, and one control loop commits the events, while
// other goroutines read the state. Run it under the race detector.
func TestConcurrentAttempts(t *testing.T) {
	const n = 4
	phases := []domain.PhaseID{"phase-01", "phase-02", "phase-03", "phase-04"}
	gate := make(chan struct{}) // holds every execution until the test opens it
	started := make(chan fake.Call, n)
	output := validOutput(t)
	rt := fakeRuntime(func(fake.Call) fake.Behavior {
		return fake.Behavior{Gate: gate, Started: started, Output: output}
	})
	v := newEnv(t, rt)
	ctx := testCtx(t)
	run := v.create()
	runDir := v.runDir(run.ID())
	driveToExecuting(t, run, phases...)
	for _, p := range phases {
		readyPhase(t, run, p)
	}
	stopReaders := readState(t, run, 3)

	// Four goroutines start an Attempt each at the same time.
	type started4 struct {
		phase domain.PhaseID
		id    domain.AttemptID
		err   error
	}
	results := make(chan started4, n)
	release := make(chan struct{})
	for _, p := range phases {
		go func() {
			<-release
			id, err := run.StartAttempt(ctx, AttemptSpec{Role: domain.RoleImplementer, PhaseID: p})
			results <- started4{p, id, err}
		}()
	}
	close(release)

	// All four executions are running at the same time: the fake signals each
	// one once it exists, and none ends before the gate opens.
	for range n {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("timed out waiting for the executions to start")
		}
	}
	if got := rt.Active(); got != n {
		t.Errorf("%d executions are running at the same time, want %d", got, n)
	}
	ids := make(map[domain.PhaseID]domain.AttemptID, n)
	for range n {
		r := <-results
		if r.err != nil {
			t.Fatalf("StartAttempt for %s: %v", r.phase, r.err)
		}
		ids[r.phase] = r.id
	}
	for _, p := range phases {
		waitForAttempt(t, run, ids[p], domain.AttemptStateRunning)
	}

	close(gate)
	sessions := make(map[domain.SessionID]bool, n)
	for _, p := range phases {
		a, err := run.AwaitAttempt(ctx, ids[p])
		if err != nil {
			t.Fatal(err)
		}
		if a.State != domain.AttemptStateFinished || a.PhaseID != p {
			t.Errorf("the Attempt for %s is %s for phase %q", p, a.State, a.PhaseID)
		}
		sessions[a.SessionID] = true
	}
	if len(sessions) != n {
		t.Errorf("%d distinct session IDs for %d Attempts", len(sessions), n)
	}
	if errs := stopReaders(); len(errs) != 0 {
		t.Errorf("readers: %v", errs)
	}
	if rt.Starts() != n || rt.Resumes() != 0 {
		t.Errorf("the runtime was started %d times and resumed %d times, want %d and 0", rt.Starts(), rt.Resumes(), n)
	}
	assertContiguous(t, run, runDir)

	// Every Attempt has its events in order: scheduled, started, finished.
	position := map[domain.AttemptID][]domain.EventType{}
	for _, ev := range readEvents(t, runDir) {
		switch p := ev.Payload.(type) {
		case domain.AttemptScheduled:
			position[p.AttemptID] = append(position[p.AttemptID], ev.Type)
		case domain.AttemptStarted:
			position[p.AttemptID] = append(position[p.AttemptID], ev.Type)
		case domain.AttemptFinished:
			position[p.AttemptID] = append(position[p.AttemptID], ev.Type)
		}
	}
	for _, p := range phases {
		want := []domain.EventType{domain.EventAttemptScheduled, domain.EventAttemptStarted, domain.EventAttemptFinished}
		if got := position[ids[p]]; !slices.Equal(got, want) {
			t.Errorf("events of the Attempt for %s: %v, want %v", p, got, want)
		}
	}
	if err := run.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if rt.Active() != 0 {
		t.Errorf("%d executions are still active after Close", rt.Active())
	}
}

// TestConcurrentProposals: many goroutines propose events at once, some
// legal and some not. Every legal one is committed, every illegal one is
// rejected without a trace, the sequence numbers are contiguous and the
// journal replays to the live state.
func TestConcurrentProposals(t *testing.T) {
	const legal, illegal = 32, 8
	phases := make([]domain.PhaseID, legal)
	for i := range phases {
		phases[i] = domain.PhaseID(fmt.Sprintf("phase-%02d", i+1))
	}
	v := newEnv(t, completing(t))
	ctx := testCtx(t)
	run := v.create()
	runDir := v.runDir(run.ID())
	driveToExecuting(t, run, phases...)
	base := stateOf(t, run).LastSeq
	stopReaders := readState(t, run, 2)

	release := make(chan struct{})
	seqs := make(chan uint64, legal)
	failures := make(chan error, legal+illegal)
	var wg sync.WaitGroup
	for _, p := range phases {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-release
			ev, err := run.propose(ctx, domain.PhaseReady{Revision: 1, PhaseID: p})
			if err != nil {
				failures <- fmt.Errorf("PhaseReady for %s: %w", p, err)
				return
			}
			seqs <- ev.Seq
		}()
	}
	for range illegal {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-release
			// RunCompleted is not allowed in Executing.
			if _, err := run.propose(ctx, domain.RunCompleted{}); !errors.Is(err, fsm.ErrIllegalTransition) {
				failures <- fmt.Errorf("RunCompleted: %v", err)
			}
		}()
	}
	close(release)
	wg.Wait()
	close(seqs)
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	var got []uint64
	for s := range seqs {
		got = append(got, s)
	}
	slices.Sort(got)
	for i, s := range got {
		if s != base+uint64(i)+1 {
			t.Fatalf("the committed sequence numbers are %v, want %d to %d without a gap", got, base+1, base+legal)
		}
	}
	if len(got) != legal {
		t.Fatalf("%d proposals were committed, want %d", len(got), legal)
	}
	if errs := stopReaders(); len(errs) != 0 {
		t.Errorf("readers: %v", errs)
	}
	assertContiguous(t, run, runDir)
	s := stateOf(t, run)
	for _, p := range phases {
		if ph, _ := s.Phase(1, p); ph.State != domain.PhaseStateReady {
			t.Errorf("%s is %s, want Ready", p, ph.State)
		}
	}
}

// TestConcurrentStartAttemptForOnePhase is invariant 14: concurrent Attempts
// for the same Phase are prohibited. Many goroutines start an Attempt for the
// same Ready Phase; exactly one succeeds, the others are rejected by the
// reducer, and the runtime is called once.
func TestConcurrentStartAttemptForOnePhase(t *testing.T) {
	const callers = 8
	gate := make(chan struct{})
	started := make(chan fake.Call, callers)
	output := validOutput(t)
	rt := fakeRuntime(func(fake.Call) fake.Behavior {
		return fake.Behavior{Gate: gate, Started: started, Output: output}
	})
	v := newEnv(t, rt)
	ctx := testCtx(t)
	run := v.create()
	runDir := v.runDir(run.ID())
	driveToExecuting(t, run, phase1)
	readyPhase(t, run, phase1)

	release := make(chan struct{})
	type outcome struct {
		id  domain.AttemptID
		err error
	}
	outcomes := make(chan outcome, callers)
	for range callers {
		go func() {
			<-release
			id, err := run.StartAttempt(ctx, AttemptSpec{Role: domain.RoleImplementer, PhaseID: phase1})
			outcomes <- outcome{id, err}
		}()
	}
	close(release)
	var winners []domain.AttemptID
	for range callers {
		o := <-outcomes
		switch {
		case o.err == nil:
			winners = append(winners, o.id)
		case !errors.Is(o.err, fsm.ErrIllegalTransition):
			t.Errorf("a losing StartAttempt: %v", o.err)
		}
	}
	if len(winners) != 1 {
		t.Fatalf("%d Attempts were started for one Phase, want 1", len(winners))
	}
	<-started
	close(gate)
	if a, err := run.AwaitAttempt(ctx, winners[0]); err != nil || a.State != domain.AttemptStateFinished {
		t.Fatalf("the Attempt: %v, %+v", err, a)
	}
	if rt.Starts() != 1 {
		t.Errorf("the runtime was started %d times, want 1", rt.Starts())
	}
	assertContiguous(t, run, runDir)
}
