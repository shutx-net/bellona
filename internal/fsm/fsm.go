// Package fsm is the fixed workflow of SPEC §6 as data: the three transition
// tables (Run, Phase and Attempt), the check functions that decide whether a
// transition is listed, and a digest of the tables for the Run manifest. The
// engine and the replay of the journal both use the check functions, so
// "the engine rejects every transition that is not listed" (R-FSM-02) is
// decided in one place. The package is pure and imports only package domain.
//
// The tables are normative in SPEC.md; tables.go holds a copy that a test
// compares with SPEC.md row by row. Each row names the event type that commits
// the transition. Rows keep their own IDs ("run-05"); the event does not carry
// the ID, because several rows can share a (from, to) pair, and the payload
// records which condition applied.
//
// # Expansion rules
//
// The check functions and Edges derive concrete (from, to) combinations from a
// row by these rules, which clarify the notation of the SPEC tables:
//
//   - A SelectStates source is the listed states.
//   - SelectAnyNonterminal ("Any nonterminal state") exists for the Run
//     machine only and means every Run state except Completed and Cancelled.
//     It therefore includes the suspended states WaitingForUser, Blocked and
//     RecoveryRequired.
//   - SelectAnyExcept ("Any state except ...") means every state of the
//     machine except the listed ones.
//   - A wildcard source (either of the two above) never yields a transition
//     whose target equals its source: there is no self-transition through a
//     wildcard. A row that lists a self-transition explicitly (FinalEvaluation
//     to FinalEvaluation, Evaluating to Evaluating) allows it.
//   - TargetStates is the listed states. The note in parentheses in a SPEC
//     target cell is kept in Target.Note and has no effect on matching.
//   - TargetResume ("Recorded resume state") matches only the state that the
//     caller passes as the resume state, and never when that state is empty.
//     For every other kind of target the resume state is ignored. The check
//     does not judge whether a resume state is sensible: the payload of the
//     event that records it and the reducer do.
//   - TargetReconciliation ("State determined by reconciliation") exists for
//     the Phase machine only and means exactly one of Ready, Running,
//     Verifying, Evaluating, IntegrationPending, Integrating, Accepted and
//     Recovering. Failed is not a target: a Phase reaches Failed only from
//     Recovering (row phase-16).
//   - A check accepts a combination when the first row, in table order, has
//     the same event, selects the source and contains the target. Both states
//     must be valid states of the machine.
//
// A change to a table or to a rule changes the behaviour of the engine: bump
// WorkflowVersion, which the Run manifest records, together with the golden
// value of the digest test.
package fsm

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/shutx-net/bellona/internal/domain"
)

const (
	// WorkflowID identifies the fixed workflow of SPEC §6.
	WorkflowID = "bellona-fixed"
	// WorkflowVersion is the version of the tables and of the expansion
	// rules. Bump it whenever either changes.
	WorkflowVersion = 1
)

// ErrIllegalTransition is matched, under errors.Is, by the error that a check
// function returns for a combination that the tables do not list.
var ErrIllegalTransition = errors.New("illegal transition")

// Machine names one of the three state machines.
type Machine string

// The state machines.
const (
	MachineRun     Machine = "run"
	MachinePhase   Machine = "phase"
	MachineAttempt Machine = "attempt"
)

// machines lists the machines in the order of SPEC.
var machines = []Machine{MachineRun, MachinePhase, MachineAttempt}

// Machines returns the machines in the order of SPEC: Run, Phase, Attempt.
func Machines() []Machine { return slices.Clone(machines) }

// SelectorKind says how a row selects its source states.
type SelectorKind int

// The kinds of source selector.
const (
	// SelectStates selects the listed states.
	SelectStates SelectorKind = iota
	// SelectAnyNonterminal selects every nonterminal state (Run machine).
	SelectAnyNonterminal
	// SelectAnyExcept selects every state except the listed ones.
	SelectAnyExcept
)

// String returns a stable name of the kind.
func (k SelectorKind) String() string {
	switch k {
	case SelectStates:
		return "states"
	case SelectAnyNonterminal:
		return "any-nonterminal"
	case SelectAnyExcept:
		return "any-except"
	}
	return "invalid-selector-" + strconv.Itoa(int(k))
}

// Selector selects the source states of a row. States holds the listed states
// for SelectStates and the excluded states for SelectAnyExcept.
type Selector struct {
	Kind   SelectorKind
	States []string
}

// TargetKind says how a row names its target states.
type TargetKind int

// The kinds of target.
const (
	// TargetStates is the listed states.
	TargetStates TargetKind = iota
	// TargetResume is the recorded resume state.
	TargetResume
	// TargetReconciliation is the state determined by reconciliation (Phase
	// machine).
	TargetReconciliation
)

// String returns a stable name of the kind.
func (k TargetKind) String() string {
	switch k {
	case TargetStates:
		return "states"
	case TargetResume:
		return "resume"
	case TargetReconciliation:
		return "reconciliation"
	}
	return "invalid-target-" + strconv.Itoa(int(k))
}

// Target is the target of a row. States holds the listed states for
// TargetStates. Note is the parenthetical text of the SPEC target cell without
// the parentheses, for example "repair replanning", or empty.
type Target struct {
	Kind   TargetKind
	States []string
	Note   string
}

// Row is one row of a transition table.
type Row struct {
	// ID is the identifier of the row: "run-01", "phase-12", "attempt-03".
	// The number is the position of the row in its table.
	ID      string
	Machine Machine
	From    Selector
	// Condition is the text of the Condition cell of SPEC, verbatim.
	Condition string
	To        Target
	// Event is the event type that commits the transition.
	Event domain.EventType
}

func (r Row) clone() Row {
	r.From.States = slices.Clone(r.From.States)
	r.To.States = slices.Clone(r.To.States)
	return r
}

// reconciliationTargets are the Phase states that reconciliation can
// determine, in Phase state order.
var reconciliationTargets = []domain.PhaseState{
	domain.PhaseStateReady, domain.PhaseStateRunning, domain.PhaseStateVerifying, domain.PhaseStateEvaluating,
	domain.PhaseStateIntegrationPending, domain.PhaseStateIntegrating, domain.PhaseStateAccepted,
	domain.PhaseStateRecovering,
}

// ReconciliationTargets returns the Phase states that a TargetReconciliation
// row allows, in Phase state order.
func ReconciliationTargets() []domain.PhaseState { return slices.Clone(reconciliationTargets) }

// tables holds the rows of each machine.
var tables = map[Machine][]Row{
	MachineRun:     runRows,
	MachinePhase:   phaseRows,
	MachineAttempt: attemptRows,
}

// Rows returns a copy of the rows of machine m in table order, or nil for an
// unknown machine.
func Rows(m Machine) []Row {
	src := tables[m]
	if src == nil {
		return nil
	}
	out := make([]Row, len(src))
	for i, r := range src {
		out[i] = r.clone()
	}
	return out
}

// statesOf returns the states of machine m in SPEC order.
func statesOf(m Machine) []string {
	switch m {
	case MachineRun:
		return names(domain.RunStates())
	case MachinePhase:
		return names(domain.PhaseStates())
	case MachineAttempt:
		return names(domain.AttemptStates())
	}
	return nil
}

// nonterminal reports whether state s of machine m is a nonterminal state for
// the purpose of SelectAnyNonterminal. Only the Run machine has the notion.
func nonterminal(m Machine, s string) bool {
	return m == MachineRun && !domain.RunState(s).Terminal()
}

// selects reports whether the selector of a row of machine m selects state s,
// which must be a state of m.
func (sel Selector) selects(m Machine, s string) bool {
	switch sel.Kind {
	case SelectStates:
		return slices.Contains(sel.States, s)
	case SelectAnyNonterminal:
		return nonterminal(m, s)
	case SelectAnyExcept:
		return !slices.Contains(sel.States, s)
	}
	return false
}

// wildcard reports whether the selector is not an explicit list.
func (sel Selector) wildcard() bool { return sel.Kind != SelectStates }

// listed returns the states that a target of kind TargetStates or
// TargetReconciliation names. It returns nil for TargetResume.
func (t Target) listed() []string {
	switch t.Kind {
	case TargetStates:
		return t.States
	case TargetReconciliation:
		return names(reconciliationTargets)
	}
	return nil
}

// matches reports whether the row allows the transition from one state to
// another, both valid states of the row's machine. The event has already been
// compared by the caller.
func (r Row) matches(from, to, resume string) bool {
	if !r.From.selects(r.Machine, from) {
		return false
	}
	if r.From.wildcard() && from == to {
		return false
	}
	if r.To.Kind == TargetResume {
		return resume != "" && to == resume
	}
	return slices.Contains(r.To.listed(), to)
}

// Edge is one concrete transition that a row allows.
type Edge struct {
	// Row is the ID of the row that yields the edge.
	Row string
	// From and To are states of the machine. For a resume target To is empty
	// and ToResume is true: the target is the recorded resume state.
	From, To string
	ToResume bool
	Event    domain.EventType
}

// Edges returns every edge that the rows of machine m yield under the
// expansion rules, rows in table order and, within a row, sources and targets
// in state order. A resume target yields one edge per source with ToResume set
// and To empty. It returns nil for an unknown machine.
func Edges(m Machine) []Edge {
	var out []Edge
	for _, r := range tables[m] {
		for _, from := range statesOf(m) {
			if !r.From.selects(m, from) {
				continue
			}
			if r.To.Kind == TargetResume {
				out = append(out, Edge{Row: r.ID, From: from, ToResume: true, Event: r.Event})
				continue
			}
			for _, to := range statesOf(m) {
				if slices.Contains(r.To.listed(), to) && !(r.From.wildcard() && from == to) {
					out = append(out, Edge{Row: r.ID, From: from, To: to, Event: r.Event})
				}
			}
		}
	}
	return out
}

// TransitionEvents returns the event types that commit transitions: those of
// the rows of the three tables, in the order of their first appearance (Run
// table, then Phase table, then Attempt table). The events of the catalogue
// that no row commits (RunCreated, ManifestFrozen, PolicyAmended) are not
// transition events.
func TransitionEvents() []domain.EventType {
	var out []domain.EventType
	for _, m := range machines {
		for _, r := range tables[m] {
			if !slices.Contains(out, r.Event) {
				out = append(out, r.Event)
			}
		}
	}
	return out
}

// TransitionError is the error that the check functions return for a
// combination that the tables do not list. It matches ErrIllegalTransition
// under errors.Is.
type TransitionError struct {
	Machine Machine
	From    string
	To      string
	Event   domain.EventType
}

// Error implements the error interface.
func (e *TransitionError) Error() string {
	return fmt.Sprintf("fsm: the %s machine has no transition %q to %q through the event %q (%v)",
		e.Machine, e.From, e.To, e.Event, ErrIllegalTransition)
}

// Is reports whether target is ErrIllegalTransition.
func (e *TransitionError) Is(target error) bool { return target == ErrIllegalTransition }

// CheckRun reports whether the Run may move from one state to another through
// the event, and returns the first matching row of the Run table. The resume
// state is the one recorded when the Run was suspended; it matters only for
// the rows whose target is the recorded resume state and is ignored otherwise
// (pass the empty state when nothing is recorded). A combination that is not
// listed yields a *TransitionError.
func CheckRun(from, to domain.RunState, ev domain.EventType, resume domain.RunState) (Row, error) {
	return check(MachineRun, string(from), string(to), ev, string(resume))
}

// CheckPhase is CheckRun for the Phase machine. For a row with a resume target
// the resume state is the Phase's recorded resume state; the rows of
// reconciliation targets ignore it.
func CheckPhase(from, to domain.PhaseState, ev domain.EventType, resume domain.PhaseState) (Row, error) {
	return check(MachinePhase, string(from), string(to), ev, string(resume))
}

// CheckAttempt is CheckRun for the Attempt machine, which has no resume
// state.
func CheckAttempt(from, to domain.AttemptState, ev domain.EventType) (Row, error) {
	return check(MachineAttempt, string(from), string(to), ev, "")
}

func check(m Machine, from, to string, ev domain.EventType, resume string) (Row, error) {
	states := statesOf(m)
	if slices.Contains(states, from) && slices.Contains(states, to) {
		for _, r := range tables[m] {
			if r.Event == ev && r.matches(from, to, resume) {
				return r.clone(), nil
			}
		}
	}
	return Row{}, &TransitionError{Machine: m, From: from, To: to, Event: ev}
}

// Digest returns the lowercase hex SHA-256 of a canonical text of the
// workflow: WorkflowID, WorkflowVersion, the ordered state lists of the three
// machines and one line per row (ID, machine, selector kind and states,
// condition, target kind, states and note, event). Fields are separated by a
// tab and lines by a newline. The Run manifest records it, so a Run is
// refused on resume when the tables of the engine differ from the tables the
// Run started with.
func Digest() string {
	var b strings.Builder
	line := func(fields ...string) {
		b.WriteString(strings.Join(fields, "\t"))
		b.WriteByte('\n')
	}
	line("workflow", WorkflowID, strconv.Itoa(WorkflowVersion))
	for _, m := range machines {
		line("states", string(m), strings.Join(statesOf(m), ","))
	}
	for _, m := range machines {
		for _, r := range tables[m] {
			line("row", r.ID, string(r.Machine),
				r.From.Kind.String(), strings.Join(r.From.States, ","),
				r.Condition,
				r.To.Kind.String(), strings.Join(r.To.States, ","), r.To.Note,
				string(r.Event))
		}
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}
