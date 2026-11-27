package fsm_test

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/shutx-net/bellona/internal/domain"
	"github.com/shutx-net/bellona/internal/fsm"
)

// goldenDigest pins fsm.Digest(). The digest covers the workflow identifier
// and version, the state lists and every row of the three tables.
const goldenDigest = "7b8f264b1a78efe61d9b9716075f5cff97586d0f93883051f7022ad7ae46ffc5"

// ---------------------------------------------------------------------------
// Mapping: rows and committing events
// ---------------------------------------------------------------------------

// rowEvents pins the committing event of every row.
const rowEvents = `
run-01 RunPrepared
run-02 RunSuspended
run-03 StrategySelected
run-04 PlanGenerated
run-05 PlanValidationFailed
run-06 PlanApprovalRequested
run-07 PlanAccepted
run-08 PlanAccepted
run-09 PlanChangesRequested
run-10 RunCancelled
run-11 FinalVerificationStarted
run-12 ReplanningStarted
run-13 RunSuspended
run-14 FinalEvaluationStarted
run-15 RunCompleted
run-16 ReplanningStarted
run-17 RunCompleted
run-18 ReplanningStarted
run-19 FinalEvaluationRetried
run-20 RunSuspended
run-21 RunResumed
run-22 RunSuspended
run-23 RunResumed
run-24 RunSuspended
run-25 RunResumed
run-26 RunCancelled
phase-01 PhaseReady
phase-02 AttemptScheduled
phase-03 PhaseVerificationStarted
phase-04 PhaseRecovering
phase-05 PhaseEvaluationStarted
phase-06 PhaseIntegrationQueued
phase-07 PhaseRecovering
phase-08 PhaseIntegrationQueued
phase-09 PhaseRecovering
phase-10 PhaseEvaluationRetried
phase-11 PhaseRecovering
phase-12 PhaseIntegrationStarted
phase-13 PhaseAccepted
phase-14 PhaseRecovering
phase-15 PhaseRetryAuthorized
phase-16 PhaseFailed
phase-17 PhaseSuspended
phase-18 PhaseResumed
phase-19 PhaseSuspended
phase-20 PhaseReconciled
phase-21 PhaseSuperseded
phase-22 PhaseSuperseded
attempt-01 AttemptStarted
attempt-02 AttemptAbandoned
attempt-03 AttemptFinished
attempt-04 AttemptFailed
attempt-05 AttemptTimedOut
attempt-06 AttemptCancelled
attempt-07 AttemptAwaitingInput
attempt-08 AttemptResumed
attempt-09 AttemptAbandoned
attempt-10 AttemptOutcomeUnknown
attempt-11 AttemptReconciled
`

func TestMappingRowEvents(t *testing.T) {
	want := make(map[string]domain.EventType)
	var order []string
	for line := range strings.SplitSeq(strings.TrimSpace(rowEvents), "\n") {
		id, ev, ok := strings.Cut(line, " ")
		if !ok {
			t.Fatalf("bad mapping line %q", line)
		}
		want[id] = domain.EventType(ev)
		order = append(order, id)
	}
	var got []string
	for _, m := range fsm.Machines() {
		for _, r := range fsm.Rows(m) {
			got = append(got, r.ID)
			if ev, ok := want[r.ID]; !ok {
				t.Errorf("row %s is not in the mapping", r.ID)
			} else if r.Event != ev {
				t.Errorf("row %s is committed by %q, want %q", r.ID, r.Event, ev)
			}
		}
	}
	if !slices.Equal(got, order) {
		t.Errorf("row IDs are %v, want %v", got, order)
	}
}

func TestMappingTransitionEvents(t *testing.T) {
	inRows := make(map[domain.EventType]fsm.Machine)
	for _, m := range fsm.Machines() {
		for _, r := range fsm.Rows(m) {
			if r.Event == "" {
				t.Errorf("row %s has no event", r.ID)
			}
			if other, dup := inRows[r.Event]; dup && other != m {
				t.Errorf("event %s commits rows of the %s and the %s machine", r.Event, other, m)
			}
			inRows[r.Event] = m
		}
	}
	catalogue := make(map[domain.EventType]bool)
	for _, e := range domain.EventTypes() {
		catalogue[e] = true
	}
	for e := range inRows {
		if !catalogue[e] {
			t.Errorf("the event %q of a row is not in the catalogue", e)
		}
	}

	transition := fsm.TransitionEvents()
	seen := make(map[domain.EventType]bool)
	for _, e := range transition {
		if seen[e] {
			t.Errorf("TransitionEvents lists %s twice", e)
		}
		seen[e] = true
		if _, ok := inRows[e]; !ok {
			t.Errorf("TransitionEvents lists %s, which no row commits", e)
		}
	}
	for e := range inRows {
		if !seen[e] {
			t.Errorf("%s appears in a row but not in TransitionEvents", e)
		}
	}
	// Every event of the catalogue is a transition event, except the three
	// that record no transition.
	nonTransition := map[domain.EventType]bool{
		domain.EventRunCreated: true, domain.EventManifestFrozen: true, domain.EventPolicyAmended: true,
	}
	for _, e := range domain.EventTypes() {
		switch {
		case nonTransition[e] && seen[e]:
			t.Errorf("%s is a transition event, want it in no row", e)
		case !nonTransition[e] && !seen[e]:
			t.Errorf("%s is in no row, want it to commit a transition", e)
		}
	}
	if got, want := len(transition), len(domain.EventTypes())-len(nonTransition); got != want {
		t.Errorf("%d transition events, want %d", got, want)
	}
	// Order of first appearance: the first event is RunPrepared (run-01).
	if transition[0] != domain.EventRunPrepared {
		t.Errorf("the first transition event is %s, want RunPrepared", transition[0])
	}
	// The returned slice is a copy.
	transition[0] = "Changed"
	if fsm.TransitionEvents()[0] == "Changed" {
		t.Error("TransitionEvents returns its internal slice")
	}
}

var rowIDPattern = regexp.MustCompile(`^(run|phase|attempt)-(\d\d)$`)

func TestMappingRowsWellFormed(t *testing.T) {
	ids := make(map[string]bool)
	for _, m := range fsm.Machines() {
		valid := machineStates(m)
		for i, r := range fsm.Rows(m) {
			where := fmt.Sprintf("row %s", r.ID)
			if mm := rowIDPattern.FindStringSubmatch(r.ID); mm == nil || mm[1] != string(m) || mm[2] != fmt.Sprintf("%02d", i+1) {
				t.Errorf("%s: the ID does not match position %d of the %s table", where, i+1, m)
			}
			if ids[r.ID] {
				t.Errorf("%s: duplicate ID", where)
			}
			ids[r.ID] = true
			if r.Machine != m {
				t.Errorf("%s: Machine is %q, want %q", where, r.Machine, m)
			}
			if r.Condition == "" || strings.ContainsAny(r.Condition, "\t\n") {
				t.Errorf("%s: condition %q is empty or has a tab or newline", where, r.Condition)
			}
			for _, s := range append(slices.Clone(r.From.States), r.To.States...) {
				if !slices.Contains(valid, s) {
					t.Errorf("%s: %q is not a state of the %s machine", where, s, m)
				}
			}
			switch r.From.Kind {
			case fsm.SelectStates:
				if len(r.From.States) == 0 {
					t.Errorf("%s: no source states", where)
				}
			case fsm.SelectAnyNonterminal:
				if m != fsm.MachineRun || len(r.From.States) != 0 {
					t.Errorf("%s: SelectAnyNonterminal is defined for the Run machine without states", where)
				}
			case fsm.SelectAnyExcept:
				if len(r.From.States) == 0 {
					t.Errorf("%s: SelectAnyExcept without excluded states", where)
				}
			default:
				t.Errorf("%s: unknown selector kind %v", where, r.From.Kind)
			}
			switch r.To.Kind {
			case fsm.TargetStates:
				if len(r.To.States) == 0 {
					t.Errorf("%s: no target states", where)
				}
			case fsm.TargetResume:
				if m == fsm.MachineAttempt || len(r.To.States) != 0 {
					t.Errorf("%s: a resume target needs a Run or Phase row without states", where)
				}
			case fsm.TargetReconciliation:
				if m != fsm.MachinePhase || len(r.To.States) != 0 {
					t.Errorf("%s: a reconciliation target is defined for the Phase machine without states", where)
				}
			default:
				t.Errorf("%s: unknown target kind %v", where, r.To.Kind)
			}
		}
	}
}

func TestRowsReturnsCopies(t *testing.T) {
	before := fsm.Digest()
	for _, m := range fsm.Machines() {
		rows := fsm.Rows(m)
		for i := range rows {
			rows[i].Condition = "changed"
			rows[i].Event = "Changed"
			for j := range rows[i].From.States {
				rows[i].From.States[j] = "Changed"
			}
			for j := range rows[i].To.States {
				rows[i].To.States[j] = "Changed"
			}
		}
		// A row returned by a check is a copy too.
		if row, err := fsm.CheckRun(domain.RunStateCreated, domain.RunStateStrategySelection, domain.EventRunPrepared, ""); err == nil {
			row.From.States[0] = "Changed"
			row.Condition = "changed"
		}
	}
	if fsm.Digest() != before {
		t.Error("modifying the result of Rows changed the tables")
	}
	if got := fsm.Rows("nothing"); got != nil {
		t.Errorf("Rows of an unknown machine = %v", got)
	}
	if got := fsm.Edges("nothing"); got != nil {
		t.Errorf("Edges of an unknown machine = %v", got)
	}
	if got := fsm.Machines(); !slices.Equal(got, []fsm.Machine{fsm.MachineRun, fsm.MachinePhase, fsm.MachineAttempt}) {
		t.Errorf("Machines() = %v", got)
	}
}

// ---------------------------------------------------------------------------
// Exhaustive check against an expectation computed from SPEC.md
// ---------------------------------------------------------------------------

// The expansion rules are implemented a second time here, from the parsed
// SPEC rows and the committing events alone, never through fsm internals.
var (
	terminalRunStates = []string{"Completed", "Cancelled"}
	reconciliationSet = []string{
		"Ready", "Running", "Verifying", "Evaluating", "IntegrationPending", "Integrating", "Accepted", "Recovering",
	}
)

type expectation struct {
	tb      testing.TB
	machine fsm.Machine
	spec    []specRow
	events  []domain.EventType
}

// first returns the index of the first row that allows the combination, or -1.
func (x expectation) first(from, to string, ev domain.EventType, resume string) int {
	states := machineStates(x.machine)
	if !slices.Contains(states, from) || !slices.Contains(states, to) {
		return -1
	}
	for i, r := range x.spec {
		if x.events[i] != ev {
			continue
		}
		var selected bool
		switch r.from.Kind {
		case fsm.SelectStates:
			selected = slices.Contains(r.from.States, from)
		case fsm.SelectAnyNonterminal:
			if x.machine != fsm.MachineRun {
				x.tb.Fatalf("SPEC.md uses %q in the %s table, which the expansion rules do not define", anyNonterminalCell, x.machine)
			}
			selected = !slices.Contains(terminalRunStates, from)
		case fsm.SelectAnyExcept:
			selected = !slices.Contains(r.from.States, from)
		}
		if !selected || (r.from.Kind != fsm.SelectStates && from == to) {
			continue
		}
		var targeted bool
		switch r.to.Kind {
		case fsm.TargetStates:
			targeted = slices.Contains(r.to.States, to)
		case fsm.TargetResume:
			targeted = resume != "" && to == resume
		case fsm.TargetReconciliation:
			if x.machine != fsm.MachinePhase {
				x.tb.Fatalf("SPEC.md uses %q in the %s table, which the expansion rules do not define", reconciliationCell, x.machine)
			}
			targeted = slices.Contains(reconciliationSet, to)
		}
		if targeted {
			return i
		}
	}
	return -1
}

// checkFunc calls the check function of a machine with strings.
func checkFunc(m fsm.Machine) func(from, to string, ev domain.EventType, resume string) (fsm.Row, error) {
	switch m {
	case fsm.MachineRun:
		return func(from, to string, ev domain.EventType, resume string) (fsm.Row, error) {
			return fsm.CheckRun(domain.RunState(from), domain.RunState(to), ev, domain.RunState(resume))
		}
	case fsm.MachinePhase:
		return func(from, to string, ev domain.EventType, resume string) (fsm.Row, error) {
			return fsm.CheckPhase(domain.PhaseState(from), domain.PhaseState(to), ev, domain.PhaseState(resume))
		}
	default:
		return func(from, to string, ev domain.EventType, resume string) (fsm.Row, error) {
			return fsm.CheckAttempt(domain.AttemptState(from), domain.AttemptState(to), ev)
		}
	}
}

// probeStates are the values tried as a source or target: every state of the
// machine, the empty string, an unknown name and a state of another machine.
func probeStates(m fsm.Machine) []string {
	foreign := map[fsm.Machine]string{fsm.MachineRun: "Pending", fsm.MachinePhase: "Blocked", fsm.MachineAttempt: "Planning"}
	return append(machineStates(m), "", "Bogus", foreign[m])
}

func probeEvents() []domain.EventType {
	return append(domain.EventTypes(), "", "Bogus")
}

func probeResumes(m fsm.Machine) []string {
	if m == fsm.MachineAttempt {
		return []string{""} // the Attempt machine has no resume state
	}
	return append([]string{""}, probeStates(m)...)
}

func TestExhaustiveCheck(t *testing.T) {
	parsed := parseSpecTables(t)
	for i, m := range fsm.Machines() {
		spec := parsed[i]
		if spec.machine != m {
			t.Fatalf("parsed table %d is for %s, want %s", i, spec.machine, m)
		}
		t.Run(string(m), func(t *testing.T) {
			goRows := fsm.Rows(m)
			if len(goRows) != len(spec.rows) {
				t.Fatalf("%d Go rows, %d SPEC rows", len(goRows), len(spec.rows))
			}
			events := make([]domain.EventType, len(goRows))
			for i, r := range goRows {
				events[i] = r.Event
			}
			x := expectation{tb: t, machine: m, spec: spec.rows, events: events}
			check := checkFunc(m)
			hit := make(map[string]int)
			accepted, rejected := 0, 0
			for _, from := range probeStates(m) {
				for _, to := range probeStates(m) {
					for _, ev := range probeEvents() {
						for _, resume := range probeResumes(m) {
							want := x.first(from, to, ev, resume)
							row, err := check(from, to, ev, resume)
							combo := fmt.Sprintf("%s: %q -> %q via %q (resume %q)", m, from, to, ev, resume)
							if want >= 0 {
								accepted++
								if err != nil {
									t.Errorf("%s: rejected (%v), want it accepted by %s", combo, err, goRows[want].ID)
									continue
								}
								if row.ID != goRows[want].ID || row.Event != ev || row.Machine != m {
									t.Errorf("%s: matched row %s (%s, %s), want %s", combo, row.ID, row.Machine, row.Event, goRows[want].ID)
								}
								hit[row.ID]++
								continue
							}
							rejected++
							var te *fsm.TransitionError
							switch {
							case err == nil:
								t.Errorf("%s: accepted by %s, want it rejected", combo, row.ID)
							case !errors.Is(err, fsm.ErrIllegalTransition):
								t.Errorf("%s: error %v does not match ErrIllegalTransition", combo, err)
							case !errors.As(err, &te) || te.Machine != m || te.From != from || te.To != to || te.Event != ev:
								t.Errorf("%s: error %v carries the wrong details", combo, err)
							case row.ID != "":
								t.Errorf("%s: a rejected combination returned the row %s", combo, row.ID)
							}
						}
					}
				}
			}
			// The test must not be vacuous, and every row must be the first
			// match of at least one combination (none is fully shadowed).
			if accepted == 0 || rejected == 0 {
				t.Errorf("%d combinations accepted and %d rejected", accepted, rejected)
			}
			for _, r := range goRows {
				// Rows that repeat an earlier (from, to, event) with another
				// condition are shadowed by that row: phase-11 by phase-09.
				if hit[r.ID] == 0 && r.ID != "phase-11" {
					t.Errorf("row %s is never the first match of any combination", r.ID)
				}
				if hit[r.ID] != 0 && r.ID == "phase-11" {
					t.Errorf("row phase-11 is expected to be shadowed by phase-09, but is the first match %d times", hit[r.ID])
				}
			}
			t.Logf("%s: %d combinations accepted, %d rejected", m, accepted, rejected)
		})
	}
}

// ---------------------------------------------------------------------------
// Named examples
// ---------------------------------------------------------------------------

func TestNamedExamples(t *testing.T) {
	run := func(from, to domain.RunState, ev domain.EventType, resume domain.RunState) (string, error) {
		r, err := fsm.CheckRun(from, to, ev, resume)
		return r.ID, err
	}
	phase := func(from, to domain.PhaseState, ev domain.EventType, resume domain.PhaseState) (string, error) {
		r, err := fsm.CheckPhase(from, to, ev, resume)
		return r.ID, err
	}
	attempt := func(from, to domain.AttemptState, ev domain.EventType) (string, error) {
		r, err := fsm.CheckAttempt(from, to, ev)
		return r.ID, err
	}
	accepts := func(t *testing.T, name, wantRow string, id string, err error) {
		t.Helper()
		if err != nil || id != wantRow {
			t.Errorf("%s: got row %q, %v; want row %s", name, id, err, wantRow)
		}
	}
	rejects := func(t *testing.T, name string, _ string, err error) {
		t.Helper()
		if !errors.Is(err, fsm.ErrIllegalTransition) {
			t.Errorf("%s: want ErrIllegalTransition, got %v", name, err)
		}
	}

	t.Run("terminal Run states", func(t *testing.T) {
		for _, from := range []domain.RunState{domain.RunStateCompleted, domain.RunStateCancelled} {
			for _, to := range domain.RunStates() {
				for _, ev := range domain.EventTypes() {
					for _, resume := range append(domain.RunStates(), "") {
						id, err := run(from, to, ev, resume)
						rejects(t, fmt.Sprintf("%s -> %s via %s", from, to, ev), id, err)
					}
				}
			}
		}
	})
	t.Run("Run", func(t *testing.T) {
		id, err := run(domain.RunStateCreated, domain.RunStateExecuting, domain.EventPlanAccepted, "")
		rejects(t, "Created -> Executing", id, err)
		id, err = run(domain.RunStateCreated, domain.RunStateStrategySelection, domain.EventStrategySelected, "")
		rejects(t, "Created -> StrategySelection via StrategySelected (wrong event)", id, err)
		id, err = run(domain.RunStateCreated, domain.RunStateStrategySelection, domain.EventRunPrepared, "")
		accepts(t, "Created -> StrategySelection via RunPrepared", "run-01", id, err)

		id, err = run(domain.RunStateBlocked, domain.RunStateBlocked, domain.EventRunSuspended, "")
		rejects(t, "Blocked -> Blocked via RunSuspended (wildcard self-transition)", id, err)
		id, err = run(domain.RunStateRecoveryRequired, domain.RunStateRecoveryRequired, domain.EventRunSuspended, "")
		rejects(t, "RecoveryRequired -> RecoveryRequired via RunSuspended", id, err)
		id, err = run(domain.RunStateFinalEvaluation, domain.RunStateFinalEvaluation, domain.EventFinalEvaluationRetried, "")
		accepts(t, "FinalEvaluation -> FinalEvaluation via FinalEvaluationRetried", "run-19", id, err)
		id, err = run(domain.RunStatePlanning, domain.RunStatePlanning, domain.EventFinalEvaluationRetried, "")
		rejects(t, "Planning -> Planning via FinalEvaluationRetried", id, err)

		// First match in table order.
		id, err = run(domain.RunStateCreated, domain.RunStateBlocked, domain.EventRunSuspended, "")
		accepts(t, "Created -> Blocked", "run-02", id, err)
		id, err = run(domain.RunStateExecuting, domain.RunStateBlocked, domain.EventRunSuspended, "")
		accepts(t, "Executing -> Blocked", "run-13", id, err)
		id, err = run(domain.RunStatePlanning, domain.RunStateBlocked, domain.EventRunSuspended, "")
		accepts(t, "Planning -> Blocked", "run-22", id, err)
		id, err = run(domain.RunStatePlanning, domain.RunStateWaitingForUser, domain.EventRunSuspended, "")
		accepts(t, "Planning -> WaitingForUser", "run-20", id, err)
		id, err = run(domain.RunStateCreated, domain.RunStateWaitingForUser, domain.EventRunSuspended, "")
		rejects(t, "Created -> WaitingForUser", id, err)
		id, err = run(domain.RunStateWaitingApproval, domain.RunStateWaitingForUser, domain.EventRunSuspended, "")
		rejects(t, "WaitingApproval -> WaitingForUser", id, err)
		id, err = run(domain.RunStateWaitingForUser, domain.RunStateRecoveryRequired, domain.EventRunSuspended, "")
		accepts(t, "WaitingForUser -> RecoveryRequired", "run-24", id, err)
		id, err = run(domain.RunStateBlocked, domain.RunStateCancelled, domain.EventRunCancelled, "")
		accepts(t, "Blocked -> Cancelled", "run-26", id, err)
		id, err = run(domain.RunStateWaitingApproval, domain.RunStateCancelled, domain.EventRunCancelled, "")
		accepts(t, "WaitingApproval -> Cancelled", "run-10", id, err)
		id, err = run(domain.RunStateExecuting, domain.RunStatePlanning, domain.EventReplanningStarted, "")
		accepts(t, "Executing -> Planning via ReplanningStarted", "run-12", id, err)
		id, err = run(domain.RunStatePlanValidation, domain.RunStateExecuting, domain.EventPlanAccepted, "")
		accepts(t, "PlanValidation -> Executing", "run-07", id, err)
		id, err = run(domain.RunStateWaitingApproval, domain.RunStateExecuting, domain.EventPlanAccepted, "")
		accepts(t, "WaitingApproval -> Executing", "run-08", id, err)
	})
	t.Run("Run resume", func(t *testing.T) {
		for _, resume := range domain.RunStates() {
			for _, suspended := range []domain.RunState{domain.RunStateWaitingForUser, domain.RunStateBlocked, domain.RunStateRecoveryRequired} {
				want := map[domain.RunState]string{
					domain.RunStateWaitingForUser: "run-21", domain.RunStateBlocked: "run-23", domain.RunStateRecoveryRequired: "run-25",
				}[suspended]
				for _, to := range domain.RunStates() {
					id, err := run(suspended, to, domain.EventRunResumed, resume)
					if to == resume {
						accepts(t, fmt.Sprintf("%s -> %s (resume %s)", suspended, to, resume), want, id, err)
					} else {
						rejects(t, fmt.Sprintf("%s -> %s (resume %s)", suspended, to, resume), id, err)
					}
				}
				id, err := run(suspended, domain.RunStatePlanning, domain.EventRunResumed, "")
				rejects(t, fmt.Sprintf("%s -> Planning without a resume state", suspended), id, err)
			}
		}
		// Only suspended states resume.
		id, err := run(domain.RunStateExecuting, domain.RunStatePlanning, domain.EventRunResumed, domain.RunStatePlanning)
		rejects(t, "Executing -> Planning via RunResumed", id, err)
		id, err = run(domain.RunStateWaitingForUser, domain.RunStatePlanning, domain.EventRunResumed, domain.RunStatePlanning)
		accepts(t, "WaitingForUser -> Planning via RunResumed (resume Planning)", "run-21", id, err)
		id, err = run(domain.RunStateWaitingForUser, domain.RunStatePlanning, domain.EventRunResumed, domain.RunStateExecuting)
		rejects(t, "WaitingForUser -> Planning via RunResumed (resume Executing)", id, err)
	})

	t.Run("Phase", func(t *testing.T) {
		id, err := phase(domain.PhaseStateAccepted, domain.PhaseStateRecovering, domain.EventPhaseRecovering, "")
		rejects(t, "Accepted -> Recovering", id, err)
		id, err = phase(domain.PhaseStatePending, domain.PhaseStateRunning, domain.EventAttemptScheduled, "")
		rejects(t, "Pending -> Running via AttemptScheduled", id, err)
		id, err = phase(domain.PhaseStateReady, domain.PhaseStateRunning, domain.EventAttemptScheduled, "")
		accepts(t, "Ready -> Running via AttemptScheduled", "phase-02", id, err)
		id, err = phase(domain.PhaseStateEvaluating, domain.PhaseStateEvaluating, domain.EventPhaseEvaluationRetried, "")
		accepts(t, "Evaluating -> Evaluating via PhaseEvaluationRetried", "phase-10", id, err)
		id, err = phase(domain.PhaseStateRunning, domain.PhaseStateRunning, domain.EventPhaseEvaluationRetried, "")
		rejects(t, "Running -> Running via PhaseEvaluationRetried", id, err)
		id, err = phase(domain.PhaseStateEvaluating, domain.PhaseStateRecovering, domain.EventPhaseRecovering, "")
		accepts(t, "Evaluating -> Recovering (first of phase-09 and phase-11)", "phase-09", id, err)
		id, err = phase(domain.PhaseStateReady, domain.PhaseStateRecovering, domain.EventPhaseRecovering, "")
		rejects(t, "Ready -> Recovering", id, err)
		id, err = phase(domain.PhaseStateRecovering, domain.PhaseStateFailed, domain.EventPhaseFailed, "")
		accepts(t, "Recovering -> Failed", "phase-16", id, err)
		id, err = phase(domain.PhaseStateRunning, domain.PhaseStateFailed, domain.EventPhaseFailed, "")
		rejects(t, "Running -> Failed", id, err)
		id, err = phase(domain.PhaseStateRecovering, domain.PhaseStateReady, domain.EventPhaseRetryAuthorized, "")
		accepts(t, "Recovering -> Ready", "phase-15", id, err)

		// Superseded.
		for _, from := range domain.PhaseStates() {
			id, err := phase(from, domain.PhaseStateSuperseded, domain.EventPhaseSuperseded, "")
			switch from {
			case domain.PhaseStateAccepted:
				accepts(t, "Accepted -> Superseded", "phase-21", id, err)
			case domain.PhaseStateSuperseded:
				rejects(t, "Superseded -> Superseded", id, err)
			default:
				accepts(t, fmt.Sprintf("%s -> Superseded", from), "phase-22", id, err)
			}
		}

		// Suspension, resume and reconciliation.
		id, err = phase(domain.PhaseStateRunning, domain.PhaseStateWaitingForUser, domain.EventPhaseSuspended, "")
		accepts(t, "Running -> WaitingForUser", "phase-17", id, err)
		id, err = phase(domain.PhaseStateVerifying, domain.PhaseStateWaitingForUser, domain.EventPhaseSuspended, "")
		rejects(t, "Verifying -> WaitingForUser", id, err)
		id, err = phase(domain.PhaseStateVerifying, domain.PhaseStateRecoveryRequired, domain.EventPhaseSuspended, "")
		accepts(t, "Verifying -> RecoveryRequired", "phase-19", id, err)
		id, err = phase(domain.PhaseStateRecovering, domain.PhaseStateRecoveryRequired, domain.EventPhaseSuspended, "")
		rejects(t, "Recovering -> RecoveryRequired", id, err)
		id, err = phase(domain.PhaseStateWaitingForUser, domain.PhaseStateRunning, domain.EventPhaseResumed, domain.PhaseStateRunning)
		accepts(t, "WaitingForUser -> Running (resume Running)", "phase-18", id, err)
		id, err = phase(domain.PhaseStateWaitingForUser, domain.PhaseStateRunning, domain.EventPhaseResumed, domain.PhaseStateEvaluating)
		rejects(t, "WaitingForUser -> Running (resume Evaluating)", id, err)
		id, err = phase(domain.PhaseStateWaitingForUser, domain.PhaseStateRunning, domain.EventPhaseResumed, "")
		rejects(t, "WaitingForUser -> Running without a resume state", id, err)

		eight := fsm.ReconciliationTargets()
		if len(eight) != 8 {
			t.Errorf("%d reconciliation targets, want 8", len(eight))
		}
		for _, to := range domain.PhaseStates() {
			// The resume state is ignored by a reconciliation row.
			for _, resume := range []domain.PhaseState{"", domain.PhaseStateRunning, domain.PhaseStateFailed} {
				id, err := phase(domain.PhaseStateRecoveryRequired, to, domain.EventPhaseReconciled, resume)
				if slices.Contains(eight, to) {
					accepts(t, fmt.Sprintf("RecoveryRequired -> %s via PhaseReconciled (resume %q)", to, resume), "phase-20", id, err)
				} else {
					rejects(t, fmt.Sprintf("RecoveryRequired -> %s via PhaseReconciled (resume %q)", to, resume), id, err)
				}
			}
		}
		for _, to := range []domain.PhaseState{
			domain.PhaseStateReady, domain.PhaseStateRunning, domain.PhaseStateVerifying, domain.PhaseStateEvaluating,
			domain.PhaseStateIntegrationPending, domain.PhaseStateIntegrating, domain.PhaseStateAccepted, domain.PhaseStateRecovering,
		} {
			if id, err := phase(domain.PhaseStateRecoveryRequired, to, domain.EventPhaseReconciled, ""); err != nil {
				t.Errorf("RecoveryRequired -> %s via PhaseReconciled: %s, %v", to, id, err)
			}
		}
		for _, to := range []domain.PhaseState{domain.PhaseStatePending, domain.PhaseStateFailed, domain.PhaseStateSuperseded,
			domain.PhaseStateWaitingForUser, domain.PhaseStateRecoveryRequired} {
			id, err := phase(domain.PhaseStateRecoveryRequired, to, domain.EventPhaseReconciled, "")
			rejects(t, fmt.Sprintf("RecoveryRequired -> %s via PhaseReconciled", to), id, err)
		}
		id, err = phase(domain.PhaseStateRunning, domain.PhaseStateReady, domain.EventPhaseReconciled, "")
		rejects(t, "Running -> Ready via PhaseReconciled", id, err)
	})

	t.Run("Attempt", func(t *testing.T) {
		id, err := attempt(domain.AttemptStateUnknown, domain.AttemptStateTimedOut, domain.EventAttemptTimedOut)
		rejects(t, "Unknown -> TimedOut", id, err)
		id, err = attempt(domain.AttemptStateRunning, domain.AttemptStateTimedOut, domain.EventAttemptTimedOut)
		accepts(t, "Running -> TimedOut", "attempt-05", id, err)
		id, err = attempt(domain.AttemptStateScheduled, domain.AttemptStateAbandoned, domain.EventAttemptAbandoned)
		accepts(t, "Scheduled -> Abandoned", "attempt-02", id, err)
		id, err = attempt(domain.AttemptStateWaitingForUser, domain.AttemptStateAbandoned, domain.EventAttemptAbandoned)
		accepts(t, "WaitingForUser -> Abandoned", "attempt-09", id, err)
		id, err = attempt(domain.AttemptStateRunning, domain.AttemptStateAbandoned, domain.EventAttemptAbandoned)
		rejects(t, "Running -> Abandoned", id, err)
		for _, from := range []domain.AttemptState{domain.AttemptStateScheduled, domain.AttemptStateRunning} {
			id, err = attempt(from, domain.AttemptStateUnknown, domain.EventAttemptOutcomeUnknown)
			accepts(t, fmt.Sprintf("%s -> Unknown", from), "attempt-10", id, err)
		}
		id, err = attempt(domain.AttemptStateWaitingForUser, domain.AttemptStateUnknown, domain.EventAttemptOutcomeUnknown)
		rejects(t, "WaitingForUser -> Unknown", id, err)
		for _, to := range domain.AttemptStates() {
			id, err = attempt(domain.AttemptStateUnknown, to, domain.EventAttemptReconciled)
			switch to {
			case domain.AttemptStateFinished, domain.AttemptStateFailed, domain.AttemptStateAbandoned:
				accepts(t, fmt.Sprintf("Unknown -> %s via AttemptReconciled", to), "attempt-11", id, err)
			default:
				rejects(t, fmt.Sprintf("Unknown -> %s via AttemptReconciled", to), id, err)
			}
		}
		// Terminal Attempt states have no way out.
		for _, from := range domain.AttemptStates() {
			if !from.Terminal() {
				continue
			}
			for _, to := range domain.AttemptStates() {
				for _, ev := range domain.EventTypes() {
					if id, err := attempt(from, to, ev); err == nil {
						t.Errorf("terminal %s -> %s via %s is accepted by %s", from, to, ev, id)
					}
				}
			}
		}
	})

	t.Run("invalid states", func(t *testing.T) {
		id, err := run("", domain.RunStatePlanning, domain.EventReplanningStarted, "")
		rejects(t, "empty source", id, err)
		id, err = run(domain.RunStateExecuting, "", domain.EventReplanningStarted, "")
		rejects(t, "empty target", id, err)
		id, err = run(domain.RunStateExecuting, "Bogus", domain.EventRunSuspended, "")
		rejects(t, "unknown target through a wildcard", id, err)
		id, err = run("Bogus", domain.RunStateBlocked, domain.EventRunSuspended, "")
		rejects(t, "unknown source through a wildcard", id, err)
		id, err = run(domain.RunStateWaitingForUser, "Bogus", domain.EventRunResumed, "Bogus")
		rejects(t, "unknown resume state", id, err)
		id, err = phase("Pending ", domain.PhaseStateReady, domain.EventPhaseReady, "")
		rejects(t, "state with a space", id, err)
		id, err = phase("Bogus", domain.PhaseStateSuperseded, domain.EventPhaseSuperseded, "")
		rejects(t, "unknown Phase source through a wildcard", id, err)
	})

	t.Run("error text", func(t *testing.T) {
		_, err := run(domain.RunStateCreated, domain.RunStateExecuting, domain.EventPlanAccepted, "")
		var te *fsm.TransitionError
		if !errors.As(err, &te) {
			t.Fatalf("not a *TransitionError: %v", err)
		}
		for _, frag := range []string{"run", "Created", "Executing", "PlanAccepted", "illegal transition"} {
			if !strings.Contains(err.Error(), frag) {
				t.Errorf("error text %q lacks %q", err.Error(), frag)
			}
		}
		if errors.Is(err, errors.New("illegal transition")) {
			t.Error("the error matches a different error with the same text")
		}
	})
}

// ---------------------------------------------------------------------------
// Edges
// ---------------------------------------------------------------------------

func TestEdges(t *testing.T) {
	for _, m := range fsm.Machines() {
		t.Run(string(m), func(t *testing.T) {
			edges := fsm.Edges(m)
			if len(edges) == 0 {
				t.Fatal("no edges")
			}
			rows := make(map[string]fsm.Row)
			for _, r := range fsm.Rows(m) {
				rows[r.ID] = r
			}
			type key struct {
				row, from, to string
				resume        bool
				ev            domain.EventType
			}
			seen := make(map[key]bool)
			check := checkFunc(m)
			states := machineStates(m)
			// the set of combinations that edges allow, ignoring the row
			type combo struct {
				from, to string
				ev       domain.EventType
				resume   bool
			}
			allowed := make(map[combo]bool)
			for _, e := range edges {
				k := key{e.Row, e.From, e.To, e.ToResume, e.Event}
				if seen[k] {
					t.Errorf("duplicate edge %+v", e)
				}
				seen[k] = true
				r, ok := rows[e.Row]
				if !ok || r.Event != e.Event {
					t.Errorf("edge %+v does not belong to a row with its event", e)
					continue
				}
				if !slices.Contains(states, e.From) || (!e.ToResume && !slices.Contains(states, e.To)) {
					t.Errorf("edge %+v has an invalid state", e)
				}
				if e.ToResume != (r.To.Kind == fsm.TargetResume) || (e.ToResume && e.To != "") {
					t.Errorf("edge %+v disagrees with the target kind of %s", e, e.Row)
				}
				allowed[combo{e.From, e.To, e.Event, e.ToResume}] = true
				// The edge is accepted by the check functions.
				if e.ToResume {
					for _, resume := range states {
						if _, err := check(e.From, resume, e.Event, resume); err != nil {
							t.Errorf("edge %+v: resume %s is rejected: %v", e, resume, err)
						}
					}
				} else if _, err := check(e.From, e.To, e.Event, ""); err != nil {
					t.Errorf("edge %+v is rejected: %v", e, err)
				}
			}
			// Conversely, every accepted combination comes from an edge.
			for _, from := range states {
				for _, to := range states {
					for _, ev := range domain.EventTypes() {
						_, errNoResume := check(from, to, ev, "")
						_, errResume := check(from, to, ev, to)
						wantNoResume := allowed[combo{from, to, ev, false}]
						wantResume := wantNoResume || allowed[combo{from, "", ev, true}]
						if (errNoResume == nil) != wantNoResume {
							t.Errorf("%s -> %s via %s without a resume state: accepted=%v, edges say %v", from, to, ev, errNoResume == nil, wantNoResume)
						}
						if (errResume == nil) != wantResume {
							t.Errorf("%s -> %s via %s with resume %s: accepted=%v, edges say %v", from, to, ev, to, errResume == nil, wantResume)
						}
					}
				}
			}
			// The result is a fresh slice.
			edges[0].Row = "changed"
			if fsm.Edges(m)[0].Row == "changed" {
				t.Error("Edges returns its internal slice")
			}
		})
	}
	// Wildcards never yield self-transitions; explicit rows may.
	for _, e := range fsm.Edges(fsm.MachineRun) {
		if e.From == e.To && e.Row != "run-19" {
			t.Errorf("self edge %+v", e)
		}
	}
	for _, e := range fsm.Edges(fsm.MachinePhase) {
		if e.From == e.To && e.Row != "phase-10" {
			t.Errorf("self edge %+v", e)
		}
	}
	// Row run-22 expands to every nonterminal state but Blocked.
	var sources []string
	for _, e := range fsm.Edges(fsm.MachineRun) {
		if e.Row == "run-22" {
			sources = append(sources, e.From)
			if e.To != "Blocked" {
				t.Errorf("run-22 edge %+v", e)
			}
		}
	}
	wantSources := []string{
		"Created", "StrategySelection", "Planning", "PlanValidation", "WaitingApproval", "Executing",
		"FinalVerification", "FinalEvaluation", "WaitingForUser", "RecoveryRequired",
	}
	if !slices.Equal(sources, wantSources) {
		t.Errorf("run-22 sources are %v, want %v", sources, wantSources)
	}
}

// ---------------------------------------------------------------------------
// Digest
// ---------------------------------------------------------------------------

func TestDigestGolden(t *testing.T) {
	got := fsm.Digest()
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(got) {
		t.Fatalf("Digest() = %q, want 64 lowercase hex digits", got)
	}
	if again := fsm.Digest(); again != got {
		t.Errorf("Digest() is not stable: %q then %q", got, again)
	}
	if got != goldenDigest {
		t.Errorf("fsm.Digest() = %s, want %s.\n"+
			"The transition tables, the state lists or the workflow identifier changed. "+
			"Bump fsm.WorkflowVersion (the Run manifest records it, and Runs started with the old tables become "+
			"incompatible) and update goldenDigest in this file together, in the same change.", got, goldenDigest)
	}
	if fsm.WorkflowID != "bellona-fixed" || fsm.WorkflowVersion != 1 {
		t.Errorf("workflow %s version %d", fsm.WorkflowID, fsm.WorkflowVersion)
	}
}

// ---------------------------------------------------------------------------
// Enumerations of the payloads agree with the tables
// ---------------------------------------------------------------------------

func TestMappingEnumerationsMatchTables(t *testing.T) {
	// Replan causes: the row that the cause names is the one that matches.
	replanRow := map[domain.ReplanCause]string{
		domain.ReplanCauseReplanAuthorized:        "run-12",
		domain.ReplanCauseFinalVerificationFailed: "run-16",
		domain.ReplanCauseFinalEvaluationRejected: "run-18",
	}
	for _, c := range domain.ReplanCauses() {
		row, err := fsm.CheckRun(c.Source(), domain.RunStatePlanning, domain.EventReplanningStarted, "")
		if err != nil || row.ID != replanRow[c] {
			t.Errorf("%s: row %q, %v; want %s", c, row.ID, err, replanRow[c])
		}
	}
	// Phase recovery causes: the source state of the cause enters Recovering.
	for _, c := range domain.PhaseRecoveryCauses() {
		if _, err := fsm.CheckPhase(c.Source(), domain.PhaseStateRecovering, domain.EventPhaseRecovering, ""); err != nil {
			t.Errorf("%s (source %s): %v", c, c.Source(), err)
		}
	}
	// Abandon reasons.
	abandonRow := map[domain.AbandonReason]string{
		domain.AbandonReasonLaunchNotOccurred:  "attempt-02",
		domain.AbandonReasonSessionUnavailable: "attempt-09",
	}
	for _, r := range domain.AbandonReasons() {
		row, err := fsm.CheckAttempt(r.Source(), domain.AttemptStateAbandoned, domain.EventAttemptAbandoned)
		if err != nil || row.ID != abandonRow[r] {
			t.Errorf("%s: row %q, %v; want %s", r, row.ID, err, abandonRow[r])
		}
	}
	// Suspend reasons: the target of a reason is a state that RunSuspended
	// reaches, and from the sources that the SPEC lists for it.
	reached := make(map[string][]string)
	for _, e := range fsm.Edges(fsm.MachineRun) {
		if e.Event == domain.EventRunSuspended {
			reached[e.To] = append(reached[e.To], e.From)
		}
	}
	for _, r := range domain.SuspendReasons() {
		if len(reached[string(r.Target())]) == 0 {
			t.Errorf("%s leads to %s, which RunSuspended does not reach", r, r.Target())
		}
	}
	if got := reached["WaitingForUser"]; !slices.Equal(got, []string{"StrategySelection", "Planning", "Executing", "FinalEvaluation"}) {
		t.Errorf("RunSuspended reaches WaitingForUser from %v", got)
	}
	// Cancel reasons: plan rejection is the WaitingApproval row.
	if row, err := fsm.CheckRun(domain.RunStateWaitingApproval, domain.RunStateCancelled, domain.EventRunCancelled, ""); err != nil || row.ID != "run-10" {
		t.Errorf("plan rejection: row %q, %v", row.ID, err)
	}
	for _, c := range domain.CancelReasons() {
		if !c.Valid() {
			t.Errorf("cancel reason %s is not valid", c)
		}
	}
}
