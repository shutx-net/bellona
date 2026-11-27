package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/shutx-net/bellona/internal/artifact"
	"github.com/shutx-net/bellona/internal/config"
	"github.com/shutx-net/bellona/internal/domain"
	"github.com/shutx-net/bellona/internal/fsm"
	"github.com/shutx-net/bellona/internal/manifest"
	"github.com/shutx-net/bellona/internal/projection"
	"github.com/shutx-net/bellona/internal/runtime/fake"
	"github.com/shutx-net/bellona/internal/storage"
	"github.com/shutx-net/bellona/internal/strictjson"
	"github.com/shutx-net/bellona/roles"
)

// TestLifecycleCompletes drives a Run through the engine to Completed with the
// fake runtime (M0 acceptance, SPEC §18: reconstruct the same state from the
// events). The live state, the replay of events.jsonl, the decoded state.json
// and the state after reopening the Run must all be equal.
func TestLifecycleCompletes(t *testing.T) {
	rt := completing(t)
	v := newEnv(t, rt)
	ctx := testCtx(t)
	run := v.create()
	runDir := v.runDir(run.ID())

	check := func(what string) {
		t.Helper()
		equalStates(t, what+": state.json against State", stateOf(t, run), snapshotOf(t, runDir))
	}
	check("after Create")
	if s := stateOf(t, run); s.Run.State != domain.RunStateCreated || s.LastSeq != 2 {
		t.Fatalf("after Create the Run is %s at event %d", s.Run.State, s.LastSeq)
	}

	driveToExecuting(t, run, phase1)
	check("after the plan was accepted")
	readyPhase(t, run, phase1)
	check("after PhaseReady")

	id, err := run.StartAttempt(ctx, AttemptSpec{Role: domain.RoleImplementer, PhaseID: phase1, Instructions: "do it", Model: "m-1", Effort: "high"})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := run.AwaitAttempt(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.State != domain.AttemptStateFinished {
		t.Fatalf("the Attempt is %s", attempt.State)
	}
	check("after the Attempt finished")
	if ph, ok := stateOf(t, run).Phase(1, phase1); !ok || ph.State != domain.PhaseStateRunning {
		t.Fatalf("the Phase is %v, want Running", ph.State)
	}

	// The output is stored at its path in canonical form and referenced.
	outputPath := domain.AttemptOutputPath(id, 1)
	stored, err := os.ReadFile(filepath.Join(runDir, filepath.FromSlash(outputPath)))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := artifact.DecodeImplementationResult(validOutput(t))
	if err != nil {
		t.Fatal(err)
	}
	want, err := strictjson.EncodeIndent(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, want) {
		t.Errorf("output.json is not the canonical encoding:\n%s\nwant\n%s", stored, want)
	}
	if ref := refOf(t, newFile{Path: outputPath, Data: stored}); attempt.Output != ref {
		t.Errorf("AttemptFinished refers to %+v, the file is %+v", attempt.Output, ref)
	}

	// The runtime saw what StartAttempt was given and the session ID that the
	// engine assigned before launch.
	calls := rt.Calls()
	if len(calls) != 1 || calls[0].Resume {
		t.Fatalf("calls: %+v", calls)
	}
	if got := calls[0].Request; got.Role != domain.RoleImplementer || got.Instructions != "do it" ||
		got.Model != "m-1" || got.Effort != "high" || got.SessionID == "" || got.SessionID != attempt.SessionID {
		t.Errorf("request %+v for the Attempt %+v", got, attempt)
	}

	// The tail of the lifecycle goes through the same commit path.
	for _, p := range []domain.Payload{
		domain.PhaseVerificationStarted{Revision: 1, PhaseID: phase1, AttemptID: id},
		domain.PhaseIntegrationQueued{Revision: 1, PhaseID: phase1},
		domain.PhaseIntegrationStarted{Revision: 1, PhaseID: phase1},
		domain.PhaseAccepted{Revision: 1, PhaseID: phase1, AttemptID: id},
		domain.FinalVerificationStarted{},
		domain.RunCompleted{},
	} {
		mustPropose(t, run, p)
		check("after " + string(p.EventType()))
	}
	live := stateOf(t, run)
	if live.Run.State != domain.RunStateCompleted {
		t.Fatalf("the Run is %s, want Completed", live.Run.State)
	}
	if ph, _ := live.Phase(1, phase1); ph.State != domain.PhaseStateAccepted {
		t.Errorf("the Phase is %s, want Accepted", ph.State)
	}

	// Nothing more is accepted once the Run is Completed.
	before := fileSet(t, runDir)
	if _, err := run.propose(ctx, domain.PhaseReady{Revision: 1, PhaseID: phase1}); !errors.Is(err, projection.ErrInvalidEvent) {
		t.Errorf("a proposal after Completed: %v", err)
	}
	if _, err := run.StartAttempt(ctx, AttemptSpec{Role: domain.RoleImplementer, PhaseID: phase1}); !errors.Is(err, projection.ErrInvalidEvent) {
		t.Errorf("StartAttempt after Completed: %v", err)
	}
	sameFiles(t, "rejected proposals after Completed", before, fileSet(t, runDir))

	// The journal has the Attempt's events in a row: scheduled before the
	// launch, then started, then finished.
	events := readEvents(t, runDir)
	at := slices.IndexFunc(events, func(e domain.Event) bool {
		p, ok := e.Payload.(domain.AttemptScheduled)
		return ok && p.AttemptID == id
	})
	if at < 0 || at+2 >= len(events) || events[at+1].Type != domain.EventAttemptStarted || events[at+2].Type != domain.EventAttemptFinished {
		t.Errorf("the Attempt %s is not scheduled, started and finished in a row: %v", id, eventTypes(t, runDir))
	}

	if err := run.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(ctx); err != nil {
		t.Errorf("a second Close: %v", err)
	}
	if _, err := run.State(ctx); !errors.Is(err, ErrClosed) {
		t.Errorf("State after Close: %v", err)
	}
	if _, err := run.StartAttempt(ctx, AttemptSpec{Role: domain.RoleImplementer, PhaseID: phase1}); !errors.Is(err, ErrClosed) {
		t.Errorf("StartAttempt after Close: %v", err)
	}
	if _, err := run.propose(ctx, domain.PhaseReady{Revision: 1, PhaseID: phase1}); !errors.Is(err, ErrClosed) {
		t.Errorf("propose after Close: %v", err)
	}

	// Three ways to the same state: the live one, the replay of the journal,
	// and the snapshot.
	equalStates(t, "Replay(events.jsonl) against the live state", live, replayed(t, runDir))
	equalStates(t, "state.json against the live state", live, snapshotOf(t, runDir))

	// And the state after reopening the Run.
	before = fileSet(t, runDir)
	reopened, rep := v.open(v.eng, run.ID(), OpenParams{})
	if rep.RecoveredTail != nil || rep.SnapshotRebuilt || rep.SnapshotIssue != "" || len(rep.UncertainAttempts) != 0 ||
		rep.ConfigChanged || len(rep.ChangedRoles) != 0 {
		t.Errorf("Open of a Completed Run reports %+v", rep)
	}
	equalStates(t, "the reopened state against the live state", live, stateOf(t, reopened))
	if err := reopened.Close(ctx); err != nil {
		t.Fatal(err)
	}
	sameFiles(t, "Open and Close of a Completed Run", before, fileSet(t, runDir))
	if rt.Starts() != 1 || rt.Resumes() != 0 {
		t.Errorf("the runtime was started %d times and resumed %d times, want 1 and 0", rt.Starts(), rt.Resumes())
	}
}

// TestAttemptOutcomes covers the failure share of T-05: every way in which an
// execution can end gives the event and the fields that the control loop
// specifies, stores no output for a failure, never retries, and leaves the
// Phase Running (the engine decides nothing about the Phase in M0).
func TestAttemptOutcomes(t *testing.T) {
	notJSON := []byte("this is not JSON")
	unknownStatus := []byte(`{"schemaVersion":1,"status":"done","summary":"s","reportedChangedFiles":[],"issues":[]}`)
	missingMember := []byte(`{"schemaVersion":1,"status":"implemented","summary":"s","issues":[]}`)
	oversized := bytes.Repeat([]byte(" "), maxOutputBytes+1)

	type want struct {
		state      domain.AttemptState
		events     []domain.EventType // after AttemptScheduled
		outcome    domain.ExecutionOutcome
		errorClass domain.ErrorClass
		detail     bool
	}
	invalidOutput := want{
		state:      domain.AttemptStateFailed,
		events:     []domain.EventType{domain.EventAttemptStarted, domain.EventAttemptFailed},
		outcome:    domain.OutcomeOutputInvalid,
		errorClass: domain.ErrorClassInvalidAgentOutput,
		detail:     true,
	}
	unknown := func(events ...domain.EventType) want {
		return want{state: domain.AttemptStateUnknown, events: append(events, domain.EventAttemptOutcomeUnknown)}
	}
	cases := []struct {
		name     string
		behavior fake.Behavior
		want     want
	}{
		{"invalid JSON", fake.Behavior{Output: notJSON}, invalidOutput},
		{"unknown status", fake.Behavior{Output: unknownStatus}, invalidOutput},
		{"missing member", fake.Behavior{Output: missingMember}, invalidOutput},
		{"no output", fake.Behavior{}, invalidOutput},
		{"oversized output", fake.Behavior{Output: oversized}, invalidOutput},
		{"host reports output_invalid", fake.Behavior{Outcome: domain.OutcomeOutputInvalid, Output: notJSON}, want{
			state:      domain.AttemptStateFailed,
			events:     []domain.EventType{domain.EventAttemptStarted, domain.EventAttemptFailed},
			outcome:    domain.OutcomeOutputInvalid,
			errorClass: domain.ErrorClassInvalidAgentOutput,
		}},
		{"host reports failed", fake.Behavior{Outcome: domain.OutcomeFailed, ExitCode: 1}, want{
			state:   domain.AttemptStateFailed,
			events:  []domain.EventType{domain.EventAttemptStarted, domain.EventAttemptFailed},
			outcome: domain.OutcomeFailed,
		}},
		{"host reports limit_reached", fake.Behavior{Outcome: domain.OutcomeLimitReached}, want{
			state:      domain.AttemptStateFailed,
			events:     []domain.EventType{domain.EventAttemptStarted, domain.EventAttemptFailed},
			outcome:    domain.OutcomeLimitReached,
			errorClass: domain.ErrorClassTurnLimitReached,
		}},
		{"host reports interrupted", fake.Behavior{Outcome: domain.OutcomeInterrupted, ExitCode: 130}, want{
			state:   domain.AttemptStateFailed,
			events:  []domain.EventType{domain.EventAttemptStarted, domain.EventAttemptFailed},
			outcome: domain.OutcomeInterrupted,
		}},
		{"host reports an unknown outcome", fake.Behavior{Outcome: "mystery"}, unknown(domain.EventAttemptStarted)},
		{"Start fails", fake.Behavior{StartErr: errors.New("the host cannot be launched")}, unknown()},
		{"Wait fails", fake.Behavior{WaitErr: errors.New("the host vanished")}, unknown(domain.EventAttemptStarted)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := fakeRuntime(func(fake.Call) fake.Behavior { return tc.behavior })
			v := newEnv(t, rt)
			ctx := testCtx(t)
			run := v.create()
			runDir := v.runDir(run.ID())
			driveToExecuting(t, run, phase1)
			readyPhase(t, run, phase1)
			scheduledAt := len(readEvents(t, runDir))

			id, err := run.StartAttempt(ctx, AttemptSpec{Role: domain.RoleImplementer, PhaseID: phase1})
			if err != nil {
				t.Fatal(err)
			}
			a, err := run.AwaitAttempt(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if a.State != tc.want.state || a.Outcome != tc.want.outcome || a.ErrorClass != tc.want.errorClass {
				t.Errorf("the Attempt is %s with outcome %q and class %q, want %s, %q, %q",
					a.State, a.Outcome, a.ErrorClass, tc.want.state, tc.want.outcome, tc.want.errorClass)
			}
			if tc.want.detail != (a.Detail != "") {
				t.Errorf("detail %q", a.Detail)
			}
			if !a.Output.IsZero() {
				t.Errorf("a failed Attempt has the output %+v", a.Output)
			}

			// The journal: AttemptScheduled first, then exactly the expected events.
			types := eventTypes(t, runDir)[scheduledAt:]
			wantTypes := append([]domain.EventType{domain.EventAttemptScheduled}, tc.want.events...)
			if !slices.Equal(types, wantTypes) {
				t.Errorf("events %v, want %v", types, wantTypes)
			}
			// No output file, and no orphan either.
			if _, err := os.Stat(filepath.Join(runDir, "attempts", string(id))); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the attempt directory exists for a failed Attempt: %v", err)
			}
			if ph, _ := stateOf(t, run).Phase(1, phase1); ph.State != domain.PhaseStateRunning {
				t.Errorf("the Phase is %s, want Running", ph.State)
			}

			// No retry, also after the Run is closed and opened again.
			if err := run.Close(ctx); err != nil {
				t.Fatal(err)
			}
			_, rep := v.open(v.eng, run.ID(), OpenParams{})
			if rt.Starts() != 1 || rt.Resumes() != 0 {
				t.Errorf("the runtime was started %d times and resumed %d times, want 1 and 0", rt.Starts(), rt.Resumes())
			}
			wantUncertain := tc.want.state == domain.AttemptStateUnknown
			if got := slices.Contains(rep.UncertainAttempts, id); got != wantUncertain {
				t.Errorf("the Attempt is reported as uncertain: %v, want %v", got, wantUncertain)
			}
		})
	}
}

// TestStartAttemptGuards covers the refusals of StartAttempt. None of them
// calls the runtime.
func TestStartAttemptGuards(t *testing.T) {
	t.Run("a role without an output contract", func(t *testing.T) {
		rt := completing(t)
		v := newEnv(t, rt)
		run := v.create()
		driveToExecuting(t, run, phase1)
		before := fileSet(t, v.runDir(run.ID()))
		for _, role := range []domain.Role{domain.RolePlanner, domain.RoleEvaluator, domain.RoleCoordinator, "reviewer"} {
			_, err := run.StartAttempt(testCtx(t), AttemptSpec{Role: role, PhaseID: phase1})
			requireIs(t, err, ErrNoOutputContract)
		}
		sameFiles(t, "refused StartAttempt", before, fileSet(t, v.runDir(run.ID())))
		if rt.Starts() != 0 {
			t.Errorf("the runtime was started %d times", rt.Starts())
		}
	})

	t.Run("a Phase that is not Ready", func(t *testing.T) {
		rt := completing(t)
		v := newEnv(t, rt)
		run := v.create()
		driveToExecuting(t, run, phase1)
		before := fileSet(t, v.runDir(run.ID()))
		_, err := run.StartAttempt(testCtx(t), AttemptSpec{Role: domain.RoleImplementer, PhaseID: phase1})
		requireIs(t, err, fsm.ErrIllegalTransition)
		sameFiles(t, "StartAttempt for a Pending Phase", before, fileSet(t, v.runDir(run.ID())))
		if rt.Starts() != 0 {
			t.Errorf("the runtime was started %d times", rt.Starts())
		}
	})

	t.Run("an unknown Phase and a missing Phase", func(t *testing.T) {
		rt := completing(t)
		v := newEnv(t, rt)
		run := v.create()
		driveToExecuting(t, run, phase1)
		readyPhase(t, run, phase1)
		for _, phase := range []domain.PhaseID{"phase-99", ""} {
			_, err := run.StartAttempt(testCtx(t), AttemptSpec{Role: domain.RoleImplementer, PhaseID: phase})
			requireIs(t, err, projection.ErrInvalidEvent)
		}
		if rt.Starts() != 0 {
			t.Errorf("the runtime was started %d times", rt.Starts())
		}
	})

	t.Run("before a plan is accepted", func(t *testing.T) {
		rt := completing(t)
		v := newEnv(t, rt)
		run := v.create()
		_, err := run.StartAttempt(testCtx(t), AttemptSpec{Role: domain.RoleImplementer, PhaseID: phase1})
		requireIs(t, err, projection.ErrInvalidEvent)
		if rt.Starts() != 0 {
			t.Errorf("the runtime was started %d times", rt.Starts())
		}
	})

	t.Run("no runtime", func(t *testing.T) {
		v := newEnv(t, nil)
		run := v.create()
		driveToExecuting(t, run, phase1)
		readyPhase(t, run, phase1)
		before := fileSet(t, v.runDir(run.ID()))
		_, err := run.StartAttempt(testCtx(t), AttemptSpec{Role: domain.RoleImplementer, PhaseID: phase1})
		requireIs(t, err, ErrNoRuntime)
		sameFiles(t, "StartAttempt without a runtime", before, fileSet(t, v.runDir(run.ID())))
	})

	t.Run("a context that is done", func(t *testing.T) {
		rt := completing(t)
		v := newEnv(t, rt)
		run := v.create()
		driveToExecuting(t, run, phase1)
		readyPhase(t, run, phase1)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := run.StartAttempt(ctx, AttemptSpec{Role: domain.RoleImplementer, PhaseID: phase1}); !errors.Is(err, context.Canceled) {
			t.Fatalf("StartAttempt with a cancelled context: %v", err)
		}
		if s := stateOf(t, run); len(attemptsOfRole(s, domain.RoleImplementer)) != 0 || rt.Starts() != 0 {
			t.Error("an Attempt was scheduled for a caller that had gone")
		}
	})
}

// TestAwaitAttemptGuards covers AwaitAttempt for an Attempt that does not exist.
func TestAwaitAttemptGuards(t *testing.T) {
	v := newEnv(t, completing(t))
	run := v.create()
	_, err := run.AwaitAttempt(testCtx(t), "attempt-0000000000000000")
	requireIs(t, err, ErrUnknownAttempt)
}

// TestCreateManifestFreezesSuppliedDefinitionsAndPolicy is the engine-level
// share of R-MAN-01 and invariant 12: Create freezes the role definitions that
// it is given, byte for byte, and the effective configuration that Resolve
// produced, not the embedded definitions or the defaults.
func TestCreateManifestFreezesSuppliedDefinitionsAndPolicy(t *testing.T) {
	v := newEnv(t, nil)
	layer, err := config.Decode([]byte(`{
  "schemaVersion": 1,
  "agents": {"planner": {"model": "planner-model", "effort": {"mode": "fixed", "value": "high"}}},
  "execution": {"strategy": "strict", "maxPhaseRetries": 5, "timeout": "45m"},
  "commands": {"build": {"argv": ["go", "build", "./..."]}},
  "verification": {"independentReview": "none", "requiredChecks": ["build"], "protectedPaths": ["Makefile"]}
}`))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Resolve(config.Inputs{User: &layer})
	if err != nil {
		t.Fatal(err)
	}
	encode := func(c config.Config) []byte {
		t.Helper()
		b, err := strictjson.EncodeIndent(c)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	if bytes.Equal(encode(cfg), encode(v.cfg)) {
		t.Fatal("the test configuration equals the defaults")
	}

	// Definitions that differ from the embedded ones in every role: CRLF, text
	// outside ASCII and no trailing newline, all of which must survive.
	embedded := roles.Definitions()
	defs := make([]domain.RoleDefinition, len(embedded))
	for i, r := range domain.Roles() {
		defs[i] = domain.RoleDefinition{Role: r, Content: fmt.Appendf(nil, "# custom %s\r\n\u5f79\u5272 %d\nno trailing newline", r, i)}
		if bytes.Equal(defs[i].Content, embedded[i].Content) {
			t.Fatalf("the custom definition of %s equals the embedded one", r)
		}
	}

	run, err := v.eng.Create(testCtx(t), CreateParams{Request: testRequest, Config: cfg, Roles: defs})
	if err != nil {
		t.Fatal(err)
	}
	closeAtEnd(t, run)
	runDir := v.runDir(run.ID())

	// definitions/roles/<role>.md holds the supplied bytes for all four roles.
	for _, d := range defs {
		got, err := os.ReadFile(filepath.Join(runDir, filepath.FromSlash(domain.RoleDefinitionPath(d.Role))))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, d.Content) {
			t.Errorf("the frozen definition of %s is %q, want the supplied %q", d.Role, got, d.Content)
		}
	}

	// policy.json decodes to the effective configuration that Resolve produced.
	policyData, err := os.ReadFile(filepath.Join(runDir, domain.PolicyPath))
	if err != nil {
		t.Fatal(err)
	}
	policy, err := config.Decode(policyData)
	if err != nil {
		t.Fatalf("policy.json does not decode: %v", err)
	}
	if !bytes.Equal(encode(policy), encode(cfg)) || !bytes.Equal(policyData, encode(cfg)) {
		t.Errorf("policy.json differs from the effective configuration:\n%s\nwant\n%s", policyData, encode(cfg))
	}
	// A few settings, so that the comparison cannot pass for the defaults.
	if policy.Execution.Strategy != "strict" || *policy.Execution.MaxPhaseRetries != 5 || policy.Execution.Timeout != "45m" ||
		policy.Agents.Planner == nil || policy.Agents.Planner.Model != "planner-model" ||
		!slices.Equal(policy.Verification.ProtectedPaths, []string{"Makefile"}) {
		t.Errorf("policy.json lost a setting: %s", policyData)
	}
	if policy.Verification.IndependentReview != "phase" { // Strict raised the review that the layer set to none
		t.Errorf("independentReview is %q, want phase", policy.Verification.IndependentReview)
	}

	// manifest.json refers to exactly these files.
	manifestData, err := os.ReadFile(filepath.Join(runDir, domain.ManifestPath))
	if err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Decode(manifestData)
	if err != nil {
		t.Fatal(err)
	}
	if m.Policy != refOf(t, newFile{Path: domain.PolicyPath, Data: policyData}) {
		t.Errorf("the manifest refers to the policy as %+v", m.Policy)
	}
	if len(m.RoleDefinitions) != len(defs) {
		t.Fatalf("the manifest lists %d role definitions", len(m.RoleDefinitions))
	}
	for i, rd := range m.RoleDefinitions {
		path := domain.RoleDefinitionPath(defs[i].Role)
		if rd.Role != defs[i].Role || rd.Definition != refOf(t, newFile{Path: path, Data: defs[i].Content}) {
			t.Errorf("the manifest refers to the definition of %s as %+v", defs[i].Role, rd)
		}
	}

	// Open compares the frozen copies with what the caller has now: the
	// supplied inputs are the frozen ones, the embedded ones and the defaults
	// are not.
	if err := run.Close(testCtx(t)); err != nil {
		t.Fatal(err)
	}
	reopened, rep := v.open(v.eng, run.ID(), OpenParams{CurrentConfig: &cfg, CurrentRoles: defs})
	if rep.ConfigChanged || len(rep.ChangedRoles) != 0 {
		t.Errorf("Open reports changes for the inputs that were frozen: %+v", rep)
	}
	if err := reopened.Close(testCtx(t)); err != nil {
		t.Fatal(err)
	}
	_, rep = v.open(v.eng, run.ID(), OpenParams{CurrentConfig: &v.cfg, CurrentRoles: embedded})
	if !rep.ConfigChanged || !slices.Equal(rep.ChangedRoles, domain.Roles()) {
		t.Errorf("Open does not report the defaults and the embedded definitions as changed: %+v", rep)
	}
}

// TestCreateIgnoresALateCancel: once Create has written anything, a
// cancellation of its context cannot make it fail for a Run whose RunCreated
// and ManifestFrozen events were both committed. The hook cancels the context
// from inside the control loop, at a crash point, so the order is fixed
// without sleeping; before the fix Create returned the context's error for a
// Run that was committed in full.
func TestCreateIgnoresALateCancel(t *testing.T) {
	cases := []struct {
		name  string
		point storage.CrashPoint
		nth   int32 // cancel at the nth time that the point is reached
	}{
		{"RunCreated is synced", storage.CrashEventSynced, 1},
		{"RunCreated is committed in full", storage.CrashSnapshotRenamed, 1},
		{"ManifestFrozen is synced", storage.CrashEventSynced, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(testCtx(t))
			defer cancel()
			var reached atomic.Int32
			v := newEnv(t, nil, func(o *Options) {
				o.Faults = func(p storage.CrashPoint) bool {
					if p == tc.point && reached.Add(1) == tc.nth {
						cancel()
					}
					return false // no crash: the hook only cancels
				}
			})
			run, err := v.eng.Create(ctx, CreateParams{Request: testRequest, Config: v.cfg})
			if err != nil {
				t.Fatalf("Create after a cancellation that came after its first write: %v", err)
			}
			closeAtEnd(t, run)
			if ctx.Err() == nil {
				t.Fatal("the hook never cancelled the context, so the test checked nothing")
			}

			// The Run is complete and usable.
			runDir := v.runDir(run.ID())
			if got := eventTypes(t, runDir); !slices.Equal(got, []domain.EventType{domain.EventRunCreated, domain.EventManifestFrozen}) {
				t.Fatalf("the journal holds %v", got)
			}
			if s := stateOf(t, run); s.Run.State != domain.RunStateCreated || s.LastSeq != 2 {
				t.Fatalf("the Run is %s at event %d", s.Run.State, s.LastSeq)
			}
			mustPropose(t, run, domain.RunPrepared{})

			// And it opens again.
			if err := run.Close(testCtx(t)); err != nil {
				t.Fatal(err)
			}
			reopened, rep := v.open(v.eng, run.ID(), OpenParams{})
			if rep.RecoveredTail != nil || rep.SnapshotRebuilt {
				t.Errorf("Open reports %+v", rep)
			}
			equalStates(t, "the reopened Run", replayed(t, runDir), stateOf(t, reopened))
		})
	}

	t.Run("a context that is done before Create writes nothing", func(t *testing.T) {
		v := newEnv(t, nil)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		run, err := v.eng.Create(ctx, CreateParams{Request: testRequest, Config: v.cfg})
		if !errors.Is(err, context.Canceled) {
			if err == nil {
				_ = run.Close(testCtx(t))
			}
			t.Fatalf("Create with a cancelled context: %v, want context.Canceled", err)
		}
		// Not even the state root was created.
		if _, err := os.Lstat(v.root); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Create with a cancelled context left %s behind: %v", v.root, err)
		}
	})
}

// TestXDGStateRoot is R-PER-01: the Run lives in plain files under
// $XDG_STATE_HOME/bellona/runs/<run-id>.
func TestXDGStateRoot(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	t.Setenv("HOME", t.TempDir())
	root, err := storage.StateRoot(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "bellona"); root != want {
		t.Fatalf("StateRoot is %s, want %s", root, want)
	}
	eng, err := New(Options{StateRoot: root, Rand: detRand(7), Clock: newTestClock().Now})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Resolve(config.Inputs{})
	if err != nil {
		t.Fatal(err)
	}
	run, err := eng.Create(testCtx(t), CreateParams{Request: testRequest, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	closeAtEnd(t, run)
	runDir := filepath.Join(dir, "bellona", "runs", string(run.ID()))
	for _, name := range []string{"run.lock", "events.jsonl", "state.json", "task.json", "policy.json", "manifest.json",
		filepath.Join("definitions", "roles", "planner.md")} {
		if fi, err := os.Stat(filepath.Join(runDir, name)); err != nil || !fi.Mode().IsRegular() {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := New(Options{StateRoot: "relative/state"}); err == nil {
		t.Error("New accepted a relative state root")
	}
}

// TestLifecycleCloseWaitsForRunningAttempts: Close stops accepting work,
// keeps committing the result of a running Attempt, and returns only when
// no Attempt is running.
func TestLifecycleCloseWaitsForRunningAttempts(t *testing.T) {
	gate := make(chan struct{})
	started := make(chan fake.Call, 1)
	out := validOutput(t)
	rt := fakeRuntime(func(fake.Call) fake.Behavior { return fake.Behavior{Gate: gate, Started: started, Output: out} })
	v := newEnv(t, rt)
	ctx := testCtx(t)
	run := v.create()
	runDir := v.runDir(run.ID())
	driveToExecuting(t, run, phase1)
	readyPhase(t, run, phase1)
	id, err := run.StartAttempt(ctx, AttemptSpec{Role: domain.RoleImplementer, PhaseID: phase1})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	waitForAttempt(t, run, id, domain.AttemptStateRunning)

	closed := make(chan error, 1)
	go func() { closed <- run.Close(ctx) }()

	// Once Close is under way, new work is refused. An illegal proposal tells
	// when: before that the reducer rejects it, afterwards ErrClosed does.
	waitFor(t, "Close to be accepted", func() bool {
		_, err := run.propose(ctx, domain.RunCompleted{})
		return errors.Is(err, ErrClosed)
	})
	if _, err := run.StartAttempt(ctx, AttemptSpec{Role: domain.RoleImplementer, PhaseID: phase1}); !errors.Is(err, ErrClosed) {
		t.Errorf("StartAttempt while closing: %v", err)
	}
	select {
	case err := <-closed:
		t.Fatalf("Close returned (%v) while an Attempt was running", err)
	default:
	}
	// The lock is still held.
	if _, _, err := v.eng.Open(ctx, run.ID(), OpenParams{}); !errors.Is(err, storage.ErrLocked) {
		t.Errorf("Open while Close waits: %v", err)
	}

	close(gate)
	if err := <-closed; err != nil {
		t.Fatalf("Close: %v", err)
	}
	// The result was committed before the lock was released.
	types := eventTypes(t, runDir)
	if got := types[len(types)-1]; got != domain.EventAttemptFinished {
		t.Errorf("the last event is %s, want AttemptFinished", got)
	}
	equalStates(t, "the snapshot after Close", replayed(t, runDir), snapshotOf(t, runDir))
	if rt.Active() != 0 {
		t.Errorf("%d executions are still active", rt.Active())
	}
}

// TestLifecycleCloseGivesUp: when the context of Close ends before the running
// Attempts do, Close stops at once, returns the context error, and the Attempt
// stays Running in the journal. The next Open reports it as uncertain and
// starts nothing.
func TestLifecycleCloseGivesUp(t *testing.T) {
	gate := make(chan struct{}) // never opened
	started := make(chan fake.Call, 1)
	rt := fakeRuntime(func(fake.Call) fake.Behavior { return fake.Behavior{Gate: gate, Started: started} })
	v := newEnv(t, rt)
	ctx := testCtx(t)
	run := v.create()
	runDir := v.runDir(run.ID())
	driveToExecuting(t, run, phase1)
	readyPhase(t, run, phase1)
	id, err := run.StartAttempt(ctx, AttemptSpec{Role: domain.RoleImplementer, PhaseID: phase1})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	waitForAttempt(t, run, id, domain.AttemptStateRunning)
	live := stateOf(t, run)

	gone, cancel := context.WithCancel(context.Background())
	cancel()
	if err := run.Close(gone); !errors.Is(err, context.Canceled) {
		t.Fatalf("Close with a context that is done: %v", err)
	}
	if _, err := run.State(ctx); !errors.Is(err, ErrClosed) {
		t.Errorf("State after Close: %v", err)
	}
	if err := run.Close(ctx); err != nil {
		t.Errorf("a second Close: %v", err)
	}
	// The dropped result of the interrupted execution was not committed.
	types := eventTypes(t, runDir)
	if got := types[len(types)-1]; got != domain.EventAttemptStarted {
		t.Errorf("the last event is %s, want AttemptStarted", got)
	}

	before := fileSet(t, runDir)
	reopened, rep := v.open(v.eng, run.ID(), OpenParams{})
	if !slices.Equal(rep.UncertainAttempts, []domain.AttemptID{id}) {
		t.Errorf("uncertain Attempts: %v, want [%s]", rep.UncertainAttempts, id)
	}
	if rep.SnapshotRebuilt || rep.RecoveredTail != nil {
		t.Errorf("Open repaired %+v", rep)
	}
	equalStates(t, "the reopened state", live, stateOf(t, reopened))
	sameFiles(t, "Open after Close gave up", before, fileSet(t, runDir))
	if rt.Starts() != 1 || rt.Resumes() != 0 {
		t.Errorf("the runtime was started %d times and resumed %d times", rt.Starts(), rt.Resumes())
	}
}
