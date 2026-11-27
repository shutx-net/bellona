package fake_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/shutx-net/bellona/internal/agent"
	"github.com/shutx-net/bellona/internal/domain"
	"github.com/shutx-net/bellona/internal/runtime/fake"
)

// safety is how long a test waits for something that must happen. It only
// turns a hang into a failure; no test waits for it on the success path.
const safety = 10 * time.Second

var _ agent.Runtime = (*fake.Runtime)(nil)

// scripted returns a script that gives the same Behavior to every call.
func scripted(b fake.Behavior) func(fake.Call) fake.Behavior {
	return func(fake.Call) fake.Behavior { return b }
}

func start(t *testing.T, rt *fake.Runtime, req agent.Request) agent.Execution {
	t.Helper()
	ex, err := rt.Start(t.Context(), req)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return ex
}

func wait(t *testing.T, ex agent.Execution) agent.Result {
	t.Helper()
	res, err := ex.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	return res
}

// waited is what Wait returned.
type waited struct {
	res agent.Result
	err error
}

// waitAsync runs Wait in a goroutine and returns a channel for its outcome, so
// that a Wait that never returns fails the test instead of hanging it.
func waitAsync(ex agent.Execution) <-chan waited {
	ch := make(chan waited, 1)
	go func() {
		res, err := ex.Wait()
		ch <- waited{res, err}
	}()
	return ch
}

func receive[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(safety):
		t.Fatalf("timed out waiting for %s", what)
		panic("unreachable")
	}
}

// drain returns the events that are buffered on ch and reports whether ch is
// closed. It never blocks.
func drain(ch <-chan agent.Event) (events []agent.Event, closed bool) {
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return events, true
			}
			events = append(events, ev)
		default:
			return events, false
		}
	}
}

func assertResult(t *testing.T, got, want agent.Result) {
	t.Helper()
	if got.SessionID != want.SessionID || got.Outcome != want.Outcome ||
		got.ExitCode != want.ExitCode || string(got.Output) != string(want.Output) {
		t.Errorf("result = %+v, want %+v", got, want)
	}
}

func TestCapabilities(t *testing.T) {
	want := agent.Capabilities{SessionIDBeforeLaunch: true, StructuredOutput: true}
	got, err := fake.New(want, nil).Capabilities(t.Context())
	if err != nil || got != want {
		t.Fatalf("Capabilities = %+v, %v; want %+v, nil", got, err, want)
	}
}

func TestNilScriptCompletes(t *testing.T) {
	rt := fake.New(agent.Capabilities{}, nil)
	ex := start(t, rt, agent.Request{Role: domain.RoleImplementer})
	assertResult(t, wait(t, ex), agent.Result{SessionID: "fake-1", Outcome: domain.OutcomeCompleted})
	if rt.Starts() != 1 || rt.Resumes() != 0 || rt.Active() != 0 {
		t.Errorf("Starts, Resumes, Active = %d, %d, %d; want 1, 0, 0", rt.Starts(), rt.Resumes(), rt.Active())
	}
}

func TestScriptedSuccessWithOutputAndEvents(t *testing.T) {
	at := time.Date(2026, 11, 28, 1, 2, 3, 0, time.UTC)
	events := []agent.Event{
		{Kind: "turn.started", Time: at, Data: []byte(`{"n":1}`)},
		{Kind: "turn.ended", Time: at.Add(time.Second)},
	}
	rt := fake.New(agent.Capabilities{}, scripted(fake.Behavior{
		Output:   []byte(`{"status":"done"}`),
		ExitCode: 0,
		Events:   events,
	}))
	ex := start(t, rt, agent.Request{Role: domain.RolePlanner})

	got, closed := drain(ex.Events())
	if !closed {
		t.Error("the events channel is not closed after it was drained")
	}
	if len(got) != len(events) {
		t.Fatalf("got %d events, want %d", len(got), len(events))
	}
	for i := range events {
		if got[i].Kind != events[i].Kind || !got[i].Time.Equal(events[i].Time) || string(got[i].Data) != string(events[i].Data) {
			t.Errorf("event %d = %+v, want %+v", i, got[i], events[i])
		}
	}
	assertResult(t, wait(t, ex), agent.Result{
		SessionID: "fake-1", Outcome: domain.OutcomeCompleted, Output: []byte(`{"status":"done"}`),
	})
}

func TestStartErr(t *testing.T) {
	boom := errors.New("boom")
	started := make(chan fake.Call, 1)
	rt := fake.New(agent.Capabilities{}, scripted(fake.Behavior{StartErr: boom, Started: started}))
	ex, err := rt.Start(t.Context(), agent.Request{Role: domain.RoleImplementer})
	if !errors.Is(err, boom) {
		t.Fatalf("Start error = %v, want boom", err)
	}
	if ex != nil {
		t.Errorf("Start returned an execution with its error: %v", ex)
	}
	select {
	case c := <-started:
		t.Errorf("Started was signalled for a failed start: %+v", c)
	default:
	}
	if rt.Starts() != 1 || rt.Active() != 0 || len(rt.Calls()) != 1 {
		t.Errorf("Starts, Active, calls = %d, %d, %d; want 1, 0, 1", rt.Starts(), rt.Active(), len(rt.Calls()))
	}

	_, err = rt.Resume(t.Context(), "sess-1", agent.Request{})
	if !errors.Is(err, boom) || rt.Resumes() != 1 || rt.Active() != 0 {
		t.Errorf("Resume error = %v, Resumes = %d, Active = %d; want boom, 1, 0", err, rt.Resumes(), rt.Active())
	}
}

func TestWaitErrComesWithTheScriptedResult(t *testing.T) {
	lost := errors.New("process lost")
	rt := fake.New(agent.Capabilities{}, scripted(fake.Behavior{
		Outcome: domain.OutcomeFailed, ExitCode: 3, Output: []byte("partial"), WaitErr: lost,
	}))
	ex := start(t, rt, agent.Request{})
	res, err := ex.Wait()
	if !errors.Is(err, lost) {
		t.Fatalf("Wait error = %v, want %v", err, lost)
	}
	assertResult(t, res, agent.Result{SessionID: "fake-1", Outcome: domain.OutcomeFailed, ExitCode: 3, Output: []byte("partial")})
	if rt.Active() != 0 {
		t.Errorf("Active = %d after Wait, want 0", rt.Active())
	}
}

func TestOutcomes(t *testing.T) {
	tests := []struct {
		name string
		in   domain.ExecutionOutcome
		want domain.ExecutionOutcome
		exit int
	}{
		{"zero means completed", "", domain.OutcomeCompleted, 0},
		{"completed", domain.OutcomeCompleted, domain.OutcomeCompleted, 0},
		{"failed", domain.OutcomeFailed, domain.OutcomeFailed, 1},
		{"limit_reached", domain.OutcomeLimitReached, domain.OutcomeLimitReached, 1},
		{"output_invalid", domain.OutcomeOutputInvalid, domain.OutcomeOutputInvalid, 0},
		{"scripted interrupted", domain.OutcomeInterrupted, domain.OutcomeInterrupted, 0},
		{"unknown values pass through", "something_new", "something_new", 0},
		{"exit code does not decide", domain.OutcomeCompleted, domain.OutcomeCompleted, 7},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rt := fake.New(agent.Capabilities{}, scripted(fake.Behavior{Outcome: tc.in, ExitCode: tc.exit}))
			res := wait(t, start(t, rt, agent.Request{}))
			if res.Outcome != tc.want || res.ExitCode != tc.exit {
				t.Errorf("outcome, exit code = %q, %d; want %q, %d", res.Outcome, res.ExitCode, tc.want, tc.exit)
			}
		})
	}
}

func TestSessionIDRules(t *testing.T) {
	const chosen domain.SessionID = "11111111-2222-4333-8444-555555555555"
	tests := []struct {
		name       string
		caps       agent.Capabilities
		resume     bool
		given      domain.SessionID // the argument of Resume
		reqSession domain.SessionID
		want       domain.SessionID
	}{
		{"start, engine assigns, capability on", agent.Capabilities{SessionIDBeforeLaunch: true}, false, "", chosen, chosen},
		{"start, nothing assigned, capability on", agent.Capabilities{SessionIDBeforeLaunch: true}, false, "", "", "fake-1"},
		{"start, engine assigns, capability off", agent.Capabilities{}, false, "", chosen, "fake-1"},
		{"start, nothing assigned, capability off", agent.Capabilities{}, false, "", "", "fake-1"},
		{"resume, capability on", agent.Capabilities{SessionIDBeforeLaunch: true, Resume: true}, true, "sess-old", chosen, "sess-old"},
		{"resume, capability off", agent.Capabilities{Resume: true}, true, "sess-old", chosen, "sess-old"},
		{"resume, no session in the request", agent.Capabilities{Resume: true}, true, "sess-old", "", "sess-old"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rt := fake.New(tc.caps, nil)
			req := agent.Request{Role: domain.RoleImplementer, SessionID: tc.reqSession}
			var ex agent.Execution
			var err error
			if tc.resume {
				ex, err = rt.Resume(t.Context(), tc.given, req)
			} else {
				ex, err = rt.Start(t.Context(), req)
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := wait(t, ex).SessionID; got != tc.want {
				t.Errorf("result session ID = %q, want %q", got, tc.want)
			}
			calls := rt.Calls()
			if len(calls) != 1 || calls[0].SessionID != tc.want || calls[0].Resume != tc.resume {
				t.Errorf("recorded call = %+v, want session ID %q and Resume %v", calls, tc.want, tc.resume)
			}
		})
	}
}

func TestCallsAreNumberedAcrossStartAndResume(t *testing.T) {
	rt := fake.New(agent.Capabilities{Resume: true}, nil)
	inputs := []domain.ArtifactRef{{Path: "artifacts/a.json", Size: 2, SHA256: "0000000000000000000000000000000000000000000000000000000000000000"}}
	reqs := []agent.Request{
		{Role: domain.RolePlanner, Model: "m1", Effort: "low", Instructions: "plan", Workspace: "/w/1", OutputSchema: "{}"},
		{Role: domain.RoleImplementer, Inputs: inputs},
		{Role: domain.RoleEvaluator},
	}
	wait(t, start(t, rt, reqs[0]))
	ex, err := rt.Resume(t.Context(), "sess-9", reqs[1])
	if err != nil {
		t.Fatal(err)
	}
	wait(t, ex)
	wait(t, start(t, rt, reqs[2]))

	if rt.Starts() != 2 || rt.Resumes() != 1 {
		t.Errorf("Starts, Resumes = %d, %d; want 2, 1", rt.Starts(), rt.Resumes())
	}
	calls := rt.Calls()
	if len(calls) != 3 {
		t.Fatalf("got %d calls, want 3", len(calls))
	}
	wantSession := []domain.SessionID{"fake-1", "sess-9", "fake-3"}
	for i, c := range calls {
		if c.Seq != i+1 || c.Resume != (i == 1) || c.SessionID != wantSession[i] {
			t.Errorf("call %d = {Seq %d, Resume %v, SessionID %q}", i, c.Seq, c.Resume, c.SessionID)
		}
		if c.Request.Role != reqs[i].Role || c.Request.Model != reqs[i].Model || c.Request.Effort != reqs[i].Effort ||
			c.Request.Instructions != reqs[i].Instructions || c.Request.Workspace != reqs[i].Workspace ||
			c.Request.OutputSchema != reqs[i].OutputSchema || !slices.Equal(c.Request.Inputs, reqs[i].Inputs) {
			t.Errorf("call %d request = %+v, want %+v", i, c.Request, reqs[i])
		}
	}
}

func TestRecordsAreCopies(t *testing.T) {
	rt := fake.New(agent.Capabilities{}, nil)
	ref := domain.ArtifactRef{Path: "artifacts/a.json", Size: 1, SHA256: "0000000000000000000000000000000000000000000000000000000000000000"}
	inputs := []domain.ArtifactRef{ref}
	wait(t, start(t, rt, agent.Request{Inputs: inputs}))

	inputs[0].Path = "artifacts/changed.json" // the caller reuses its slice
	calls := rt.Calls()
	if calls[0].Request.Inputs[0] != ref {
		t.Errorf("recorded input = %+v, want it unaffected by the caller", calls[0].Request.Inputs[0])
	}
	calls[0].Seq = 99 // a test edits what Calls returned
	calls[0].Request.Inputs[0].Path = "artifacts/other.json"
	again := rt.Calls()
	if again[0].Seq != 1 || again[0].Request.Inputs[0] != ref {
		t.Errorf("Calls shares memory with the runtime: %+v", again[0])
	}
}

func TestOutputAndEventsAreNotSharedBetweenExecutions(t *testing.T) {
	shared := fake.Behavior{
		Output: []byte("abc"),
		Events: []agent.Event{{Kind: "k", Data: []byte("xyz")}},
	}
	rt := fake.New(agent.Capabilities{}, scripted(shared))
	first := start(t, rt, agent.Request{})
	second := start(t, rt, agent.Request{})

	res := wait(t, first)
	res.Output[0] = 'Z'
	evs, _ := drain(first.Events())
	evs[0].Data[0] = 'Z'

	res2 := wait(t, second)
	evs2, _ := drain(second.Events())
	if string(res2.Output) != "abc" || string(evs2[0].Data) != "xyz" {
		t.Errorf("second execution saw %q and %q, want abc and xyz", res2.Output, evs2[0].Data)
	}
	if string(shared.Output) != "abc" || string(shared.Events[0].Data) != "xyz" {
		t.Errorf("the script's Behavior was modified: %q, %q", shared.Output, shared.Events[0].Data)
	}
}

func TestStartedIsSignalledWithTheCall(t *testing.T) {
	started := make(chan fake.Call, 1)
	rt := fake.New(agent.Capabilities{}, scripted(fake.Behavior{Started: started}))
	ex := start(t, rt, agent.Request{Role: domain.RoleEvaluator, Model: "m"})
	got := receive(t, started, "Started")
	if got.Seq != 1 || got.Resume || got.SessionID != "fake-1" || got.Request.Role != domain.RoleEvaluator || got.Request.Model != "m" {
		t.Errorf("signalled call = %+v", got)
	}
	if rt.Active() != 1 {
		t.Errorf("Active = %d once Started was signalled, want 1", rt.Active())
	}
	wait(t, ex)
}

func TestScriptSeesTheCall(t *testing.T) {
	var seen []fake.Call
	rt := fake.New(agent.Capabilities{Resume: true}, func(c fake.Call) fake.Behavior {
		seen = append(seen, c)
		if c.Resume {
			return fake.Behavior{Outcome: domain.OutcomeLimitReached}
		}
		return fake.Behavior{Output: []byte("first")}
	})
	if res := wait(t, start(t, rt, agent.Request{Role: domain.RoleImplementer})); string(res.Output) != "first" {
		t.Errorf("first result = %+v", res)
	}
	ex, err := rt.Resume(t.Context(), "sess-1", agent.Request{Role: domain.RoleImplementer})
	if err != nil {
		t.Fatal(err)
	}
	if res := wait(t, ex); res.Outcome != domain.OutcomeLimitReached || res.SessionID != "sess-1" {
		t.Errorf("resumed result = %+v", res)
	}
	if len(seen) != 2 || seen[0].Seq != 1 || seen[0].Resume || seen[1].Seq != 2 || !seen[1].Resume || seen[1].SessionID != "sess-1" {
		t.Errorf("the script saw %+v", seen)
	}
}

func TestCancelInterrupts(t *testing.T) {
	gate := make(chan struct{}) // never opened
	rt := fake.New(agent.Capabilities{}, scripted(fake.Behavior{
		Gate: gate, Output: []byte("ignored"), ExitCode: 0, WaitErr: errors.New("ignored"),
	}))
	ex := start(t, rt, agent.Request{})
	pending := waitAsync(ex)
	if err := ex.Cancel(t.Context()); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if err := ex.Cancel(t.Context()); err != nil {
		t.Fatalf("second Cancel: %v", err)
	}
	w := receive(t, pending, "Wait after Cancel")
	if w.err != nil {
		t.Errorf("Wait error = %v, want nil", w.err)
	}
	assertResult(t, w.res, agent.Result{SessionID: "fake-1", Outcome: domain.OutcomeInterrupted, ExitCode: 130})
	if rt.Active() != 0 {
		t.Errorf("Active = %d, want 0", rt.Active())
	}
	if err := ex.Cancel(t.Context()); err != nil { // after Wait: still fine
		t.Errorf("Cancel after Wait: %v", err)
	}
}

func TestCancelBeforeWaitWinsOverAnOpenGateAndNoGate(t *testing.T) {
	open := make(chan struct{})
	close(open)
	for name, gate := range map[string]<-chan struct{}{"no gate": nil, "open gate": open} {
		t.Run(name, func(t *testing.T) {
			rt := fake.New(agent.Capabilities{}, scripted(fake.Behavior{Gate: gate, Output: []byte("done")}))
			ex := start(t, rt, agent.Request{})
			if err := ex.Cancel(t.Context()); err != nil {
				t.Fatal(err)
			}
			res := wait(t, ex)
			if res.Outcome != domain.OutcomeInterrupted || res.ExitCode != 130 || len(res.Output) != 0 {
				t.Errorf("result = %+v, want an interrupted one without output", res)
			}
		})
	}
}

func TestContextCancellationInterrupts(t *testing.T) {
	gate := make(chan struct{}) // never opened
	rt := fake.New(agent.Capabilities{}, scripted(fake.Behavior{Gate: gate}))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ex, err := rt.Start(ctx, agent.Request{})
	if err != nil {
		t.Fatal(err)
	}
	pending := waitAsync(ex)
	cancel()
	w := receive(t, pending, "Wait after the context was cancelled")
	if w.err != nil {
		t.Errorf("Wait error = %v, want nil", w.err)
	}
	assertResult(t, w.res, agent.Result{SessionID: "fake-1", Outcome: domain.OutcomeInterrupted, ExitCode: 130})
}

func TestResumeContextCancellationInterrupts(t *testing.T) {
	rt := fake.New(agent.Capabilities{Resume: true}, scripted(fake.Behavior{Gate: make(chan struct{})}))
	ctx, cancel := context.WithCancel(t.Context())
	ex, err := rt.Resume(ctx, "sess-2", agent.Request{})
	if err != nil {
		t.Fatal(err)
	}
	pending := waitAsync(ex)
	cancel()
	w := receive(t, pending, "Wait after the context was cancelled")
	assertResult(t, w.res, agent.Result{SessionID: "sess-2", Outcome: domain.OutcomeInterrupted, ExitCode: 130})
}

// A caller that does not receive from Started must not hang Start once its
// context is cancelled, and the execution still exists and is interrupted.
func TestStartDoesNotBlockForeverOnStarted(t *testing.T) {
	rt := fake.New(agent.Capabilities{}, scripted(fake.Behavior{Started: make(chan fake.Call)})) // nobody receives
	ctx, cancel := context.WithCancel(t.Context())
	type started struct {
		ex  agent.Execution
		err error
	}
	done := make(chan started, 1)
	go func() {
		ex, err := rt.Start(ctx, agent.Request{})
		done <- started{ex, err}
	}()
	cancel()
	s := receive(t, done, "Start to return after the context was cancelled")
	if s.err != nil || s.ex == nil {
		t.Fatalf("Start = %v, %v; want an execution", s.ex, s.err)
	}
	if rt.Active() != 1 {
		t.Errorf("Active = %d before Wait, want 1", rt.Active())
	}
	assertResult(t, wait(t, s.ex), agent.Result{SessionID: "fake-1", Outcome: domain.OutcomeInterrupted, ExitCode: 130})
	if rt.Active() != 0 {
		t.Errorf("Active = %d after Wait, want 0", rt.Active())
	}
}

func TestEventsAreClosedBeforeWaitReturns(t *testing.T) {
	gate := make(chan struct{})
	rt := fake.New(agent.Capabilities{}, scripted(fake.Behavior{
		Gate:   gate,
		Events: []agent.Event{{Kind: "a"}, {Kind: "b"}, {Kind: "c"}},
	}))
	ex := start(t, rt, agent.Request{})
	pending := waitAsync(ex)
	close(gate)
	receive(t, pending, "Wait")

	// Nothing was read before Wait returned; the channel must be closed now
	// and still hold the events.
	events, closed := drain(ex.Events())
	if !closed {
		t.Fatal("the events channel was not closed when Wait returned")
	}
	if len(events) != 3 || events[0].Kind != "a" || events[2].Kind != "c" {
		t.Errorf("events = %+v, want a, b, c", events)
	}
}

func TestEventsCanBeIgnoredOrDrainedEarly(t *testing.T) {
	rt := fake.New(agent.Capabilities{}, scripted(fake.Behavior{
		Gate:   make(chan struct{}), // the execution is still held
		Events: []agent.Event{{Kind: "a"}},
	}))
	ex := start(t, rt, agent.Request{})
	// A consumer that ranges over the channel before calling Wait ends at
	// once; it does not depend on Wait running at the same time.
	n := 0
	for range ex.Events() {
		n++
	}
	if n != 1 {
		t.Errorf("read %d events, want 1", n)
	}
	if err := ex.Cancel(t.Context()); err != nil {
		t.Fatal(err)
	}
	wait(t, ex)
}

func TestWaitMayBeCalledOnce(t *testing.T) {
	rt := fake.New(agent.Capabilities{}, nil)
	ex := start(t, rt, agent.Request{})
	wait(t, ex)
	if _, err := ex.Wait(); !errors.Is(err, fake.ErrWaitCalledTwice) {
		t.Errorf("second Wait error = %v, want ErrWaitCalledTwice", err)
	}
	if rt.Active() != 0 {
		t.Errorf("Active = %d, want 0: a second Wait must not uncount again", rt.Active())
	}
}

func TestActiveCountsExecutionsUntilTheirWaitReturns(t *testing.T) {
	rt := fake.New(agent.Capabilities{}, nil)
	a := start(t, rt, agent.Request{})
	b := start(t, rt, agent.Request{})
	if rt.Active() != 2 {
		t.Fatalf("Active = %d, want 2: executions stay active until they are waited for", rt.Active())
	}
	wait(t, a)
	if rt.Active() != 1 {
		t.Errorf("Active = %d after one Wait, want 1", rt.Active())
	}
	wait(t, b)
	if rt.Active() != 0 {
		t.Errorf("Active = %d after both Waits, want 0", rt.Active())
	}
}

// Sixteen executions are held at one gate, all are seen as started and
// active, and one close releases them together. The script keeps its state in
// a plain variable: the runtime must call it one call at a time. Run with
// -race.
func TestConcurrentExecutionsHeldAtAGate(t *testing.T) {
	const n = 16
	gate := make(chan struct{})
	started := make(chan fake.Call) // unbuffered: every Start waits for the test to receive
	scriptCalls := 0                // deliberately unsynchronized
	rt := fake.New(agent.Capabilities{SessionIDBeforeLaunch: true}, func(c fake.Call) fake.Behavior {
		scriptCalls++
		return fake.Behavior{
			Gate:    gate,
			Started: started,
			Output:  []byte(fmt.Sprintf("out-%d", c.Seq)),
			Events:  []agent.Event{{Kind: "step", Data: []byte{byte(c.Seq)}}},
		}
	})

	type finished struct {
		res agent.Result
		err error
	}
	results := make(chan finished, n)
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			ex, err := rt.Start(t.Context(), agent.Request{Role: domain.RoleImplementer})
			if err != nil {
				results <- finished{err: err}
				return
			}
			go func() {
				for range ex.Events() { // a consumer, as the engine might have
				}
			}()
			res, err := ex.Wait()
			results <- finished{res, err}
		})
	}

	seen := map[int]bool{}
	for range n {
		c := receive(t, started, "an execution to start")
		if seen[c.Seq] {
			t.Errorf("Seq %d was signalled twice", c.Seq)
		}
		seen[c.Seq] = true
	}
	if got := rt.Active(); got != n {
		t.Fatalf("Active = %d with every execution held at the gate, want %d", got, n)
	}
	if got := rt.Starts(); got != n {
		t.Errorf("Starts = %d, want %d", got, n)
	}
	select {
	case r := <-results:
		t.Fatalf("an execution finished before the gate was opened: %+v", r)
	default:
	}

	close(gate)
	wg.Wait()
	close(results)

	sessions := map[domain.SessionID]bool{}
	for r := range results {
		if r.err != nil {
			t.Errorf("Wait error: %v", r.err)
			continue
		}
		if r.res.Outcome != domain.OutcomeCompleted || sessions[r.res.SessionID] {
			t.Errorf("result = %+v (duplicate session ID: %v)", r.res, sessions[r.res.SessionID])
		}
		sessions[r.res.SessionID] = true
	}
	if len(sessions) != n {
		t.Errorf("got %d distinct session IDs, want %d", len(sessions), n)
	}
	if rt.Active() != 0 || rt.Starts() != n || rt.Resumes() != 0 {
		t.Errorf("Active, Starts, Resumes = %d, %d, %d; want 0, %d, 0", rt.Active(), rt.Starts(), rt.Resumes(), n)
	}
	if scriptCalls != n {
		t.Errorf("the script ran %d times, want %d", scriptCalls, n)
	}
	calls := rt.Calls()
	if len(calls) != n {
		t.Fatalf("got %d calls, want %d", len(calls), n)
	}
	for i, c := range calls {
		if c.Seq != i+1 {
			t.Errorf("calls[%d].Seq = %d, want %d", i, c.Seq, i+1)
		}
	}
}
