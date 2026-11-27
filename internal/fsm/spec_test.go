package fsm_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/shutx-net/bellona/internal/domain"
	"github.com/shutx-net/bellona/internal/fsm"
)

// SPEC.md holds the normative transition tables. This file parses them out of
// the document, so that the tables in tables.go cannot drift from the SPEC:
// TestSpecParity compares them row by row. The parser recognises exactly the
// cell forms that SPEC.md uses and fails on any other, so a new notation in
// the SPEC forces a deliberate code update.

const (
	tableHeader        = "| From | Condition | To |"
	terminalSentence   = "`Completed` and `Cancelled` are the only terminal Run states."
	resumeCell         = "Recorded resume state"
	reconciliationCell = "State determined by reconciliation"
	anyNonterminalCell = "Any nonterminal state"
	anyExceptPrefix    = "Any state except "
)

// specRow is one parsed row of a SPEC table.
type specRow struct {
	line      int // 1-based line number in SPEC.md
	from      fsm.Selector
	condition string
	to        fsm.Target
}

// specTable is one parsed SPEC table.
type specTable struct {
	machine fsm.Machine
	heading string
	rows    []specRow
	// after is the first non-blank line after the table.
	after string
}

var specTables = []struct {
	machine fsm.Machine
	heading string
}{
	{fsm.MachineRun, "### Run states"},
	{fsm.MachinePhase, "### Phase states"},
	{fsm.MachineAttempt, "### Attempt states"},
}

func machineStates(m fsm.Machine) []string {
	var out []string
	switch m {
	case fsm.MachineRun:
		for _, s := range domain.RunStates() {
			out = append(out, string(s))
		}
	case fsm.MachinePhase:
		for _, s := range domain.PhaseStates() {
			out = append(out, string(s))
		}
	case fsm.MachineAttempt:
		for _, s := range domain.AttemptStates() {
			out = append(out, string(s))
		}
	}
	return out
}

// readSpec reads SPEC.md from the repository root. go test runs in the
// package directory, so the root is two levels up.
func readSpec(tb testing.TB) []string {
	tb.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "SPEC.md"))
	if err != nil {
		tb.Fatalf("read SPEC.md: %v", err)
	}
	return strings.Split(string(data), "\n")
}

// parseSpecTables parses the three tables, or fails the test naming every cell
// or structure that it does not recognise.
func parseSpecTables(tb testing.TB) []specTable {
	tb.Helper()
	lines := readSpec(tb)
	var out []specTable
	for _, st := range specTables {
		tbl, errs := parseTable(lines, st.machine, st.heading)
		for _, err := range errs {
			tb.Errorf("SPEC.md, %s: %v", st.heading, err)
		}
		out = append(out, tbl)
	}
	if tb.Failed() {
		tb.FailNow()
	}
	return out
}

// parseTable finds the heading, then the first table header after it, and
// parses the rows. Problems are returned, not fatal, so that a test of the
// parser can feed it synthetic documents.
func parseTable(lines []string, m fsm.Machine, heading string) (specTable, []error) {
	tbl := specTable{machine: m, heading: heading}
	h := -1
	for i, l := range lines {
		if l == heading {
			if h >= 0 {
				return tbl, []error{fmt.Errorf("the heading appears twice (lines %d and %d)", h+1, i+1)}
			}
			h = i
		}
	}
	if h < 0 {
		return tbl, []error{fmt.Errorf("the heading is missing")}
	}
	hdr := -1
	for i := h + 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "### ") || strings.HasPrefix(lines[i], "## ") {
			break
		}
		if strings.HasPrefix(lines[i], tableHeader) {
			hdr = i
			break
		}
	}
	if hdr < 0 {
		return tbl, []error{fmt.Errorf("no %q table follows the heading", tableHeader)}
	}
	if hdr+1 >= len(lines) {
		return tbl, []error{fmt.Errorf("the table ends after its header")}
	}
	if cells, err := splitRow(lines[hdr+1]); err != nil || cells[0] != "---" || cells[1] != "---" || cells[2] != "---" {
		return tbl, []error{fmt.Errorf("line %d is not the separator `| --- | --- | --- |`: %q", hdr+2, lines[hdr+1])}
	}
	valid := machineStates(m)
	var errs []error
	i := hdr + 2
	for ; i < len(lines) && strings.HasPrefix(lines[i], "|"); i++ {
		cells, err := splitRow(lines[i])
		if err != nil {
			errs = append(errs, fmt.Errorf("line %d: %w", i+1, err))
			continue
		}
		row := specRow{line: i + 1, condition: cells[1]}
		var ferr, terr error
		row.from, ferr = parseSource(cells[0], valid)
		row.to, terr = parseTarget(cells[2], valid)
		if ferr != nil {
			errs = append(errs, fmt.Errorf("line %d, From cell %q: %w", i+1, cells[0], ferr))
		}
		if terr != nil {
			errs = append(errs, fmt.Errorf("line %d, To cell %q: %w", i+1, cells[2], terr))
		}
		if cells[1] == "" {
			errs = append(errs, fmt.Errorf("line %d: empty Condition cell", i+1))
		}
		if ferr == nil && terr == nil {
			tbl.rows = append(tbl.rows, row)
		}
	}
	for ; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != "" {
			tbl.after = lines[i]
			break
		}
	}
	return tbl, errs
}

// splitRow trims the outer pipes of a table row and splits it into exactly
// three cells on " | ".
func splitRow(line string) ([]string, error) {
	if !strings.HasPrefix(line, "| ") || !strings.HasSuffix(line, " |") {
		return nil, fmt.Errorf("row %q does not start with \"| \" and end with \" |\"", line)
	}
	cells := strings.Split(line[2:len(line)-2], " | ")
	if len(cells) != 3 {
		return nil, fmt.Errorf("row has %d cells, want 3: %q", len(cells), line)
	}
	return cells, nil
}

var stateNamePattern = regexp.MustCompile(`^[A-Za-z]+$`)

// parseStateList parses `A`, `A` and `B` or `A`, `B`, or `C`: backticked
// state names joined by ", ", " and " or ", or ". The last two separators may
// join only the last name. Every name must be one of valid and appear once.
// The cell must be reproduced exactly by re-joining the names with the
// separators that were found.
func parseStateList(cell string, valid []string) ([]string, error) {
	var names, seps []string
	pos := 0
	final := false
	for {
		if !strings.HasPrefix(cell[pos:], "`") {
			return nil, fmt.Errorf("expected a backticked state name at offset %d", pos)
		}
		end := strings.Index(cell[pos+1:], "`")
		if end < 0 {
			return nil, fmt.Errorf("unterminated backtick at offset %d", pos)
		}
		name := cell[pos+1 : pos+1+end]
		if !stateNamePattern.MatchString(name) || !slices.Contains(valid, name) {
			return nil, fmt.Errorf("%q is not a state of this machine", name)
		}
		if slices.Contains(names, name) {
			return nil, fmt.Errorf("state %q is listed twice", name)
		}
		names = append(names, name)
		pos += end + 2
		if pos == len(cell) {
			break
		}
		if final {
			return nil, fmt.Errorf("unexpected text after the last name at offset %d", pos)
		}
		switch rest := cell[pos:]; {
		case strings.HasPrefix(rest, ", or "):
			seps, final = append(seps, ", or "), true
			pos += len(", or ")
		case strings.HasPrefix(rest, " and "):
			seps, final = append(seps, " and "), true
			pos += len(" and ")
		case strings.HasPrefix(rest, ", "):
			seps = append(seps, ", ")
			pos += len(", ")
		default:
			return nil, fmt.Errorf("unknown separator at offset %d: %q", pos, rest)
		}
	}
	var b strings.Builder
	for i, n := range names {
		if i > 0 {
			b.WriteString(seps[i-1])
		}
		b.WriteString("`" + n + "`")
	}
	if b.String() != cell {
		return nil, fmt.Errorf("the cell is not reproduced by its parsed names (got %q)", b.String())
	}
	return names, nil
}

func parseSource(cell string, valid []string) (fsm.Selector, error) {
	switch {
	case cell == anyNonterminalCell:
		return fsm.Selector{Kind: fsm.SelectAnyNonterminal}, nil
	case strings.HasPrefix(cell, anyExceptPrefix):
		names, err := parseStateList(cell[len(anyExceptPrefix):], valid)
		return fsm.Selector{Kind: fsm.SelectAnyExcept, States: names}, err
	}
	names, err := parseStateList(cell, valid)
	return fsm.Selector{Kind: fsm.SelectStates, States: names}, err
}

func parseTarget(cell string, valid []string) (fsm.Target, error) {
	var t fsm.Target
	if strings.HasSuffix(cell, ")") {
		i := strings.LastIndex(cell, " (")
		if i < 0 {
			return t, fmt.Errorf("a trailing ')' without ' ('")
		}
		t.Note = cell[i+2 : len(cell)-1]
		if t.Note == "" || strings.ContainsAny(t.Note, "()`") {
			return t, fmt.Errorf("unsupported note %q", t.Note)
		}
		cell = cell[:i]
	}
	switch cell {
	case resumeCell:
		t.Kind = fsm.TargetResume
		return t, nil
	case reconciliationCell:
		t.Kind = fsm.TargetReconciliation
		return t, nil
	}
	names, err := parseStateList(cell, valid)
	t.Kind, t.States = fsm.TargetStates, names
	return t, err
}

// ---------------------------------------------------------------------------
// Tests of the parser
// ---------------------------------------------------------------------------

func TestSpecParserCellForms(t *testing.T) {
	valid := machineStates(fsm.MachineRun)
	okLists := map[string][]string{
		"`Created`":                              {"Created"},
		"`Created` and `Blocked`":                {"Created", "Blocked"},
		"`Created`, `Blocked`":                   {"Created", "Blocked"},
		"`Created`, `Planning`, `Executing`":     {"Created", "Planning", "Executing"},
		"`Created`, `Planning`, or `Executing`":  {"Created", "Planning", "Executing"},
		"`Created`, `Planning`, and `Executing`": nil, // ", and " is not a SPEC form
	}
	for cell, want := range okLists {
		got, err := parseStateList(cell, valid)
		if want == nil {
			if err == nil {
				t.Errorf("%q: accepted, got %v", cell, got)
			}
			continue
		}
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("%q: got %v, %v; want %v", cell, got, err, want)
		}
	}
	for _, cell := range []string{
		"", "Created", "`Created", "`Created`.", "`Created` `Blocked`", "`Created`;`Blocked`", "`Created`,`Blocked`",
		"`Created` or `Blocked`", "`Created` and `Blocked` and `Planning`", "`Created`, or `Blocked`, `Planning`",
		"`Created` and `Blocked`, `Planning`", "`Created`, `Created`", "`Nowhere`", "`created`", "` Created`",
		"`Created` and", "`Created`, ", " `Created`", "`Created` ", "`Pending`", "`Created` and `Pending`", "``",
		"`Created`  and `Blocked`",
	} {
		if got, err := parseStateList(cell, valid); err == nil {
			t.Errorf("%q: accepted, got %v", cell, got)
		}
	}

	sel := map[string]fsm.SelectorKind{
		"Any nonterminal state":                    fsm.SelectAnyNonterminal,
		"Any state except `Created` and `Blocked`": fsm.SelectAnyExcept,
		"`Created`, `Blocked`":                     fsm.SelectStates,
	}
	for cell, kind := range sel {
		got, err := parseSource(cell, valid)
		if err != nil || got.Kind != kind {
			t.Errorf("%q: got %+v, %v; want kind %v", cell, got, err, kind)
		}
	}
	for _, cell := range []string{
		"Any nonterminal states", "any nonterminal state", "Any terminal state", "Any state", "Any state except ",
		"Any state except Created", "Any state except `Nowhere`", "Every state", "Any nonterminal state (not Blocked)",
	} {
		if got, err := parseSource(cell, valid); err == nil {
			t.Errorf("source %q: accepted, got %+v", cell, got)
		}
	}

	tgt := map[string]fsm.Target{
		"`Created`":                           {Kind: fsm.TargetStates, States: []string{"Created"}},
		"`Planning` (repair replanning)":      {Kind: fsm.TargetStates, States: []string{"Planning"}, Note: "repair replanning"},
		"`Created` (a new one, §6.1)":         {Kind: fsm.TargetStates, States: []string{"Created"}, Note: "a new one, §6.1"},
		"`Created`, `Blocked`, or `Planning`": {Kind: fsm.TargetStates, States: []string{"Created", "Blocked", "Planning"}},
		"Recorded resume state":               {Kind: fsm.TargetResume},
		"State determined by reconciliation":  {Kind: fsm.TargetReconciliation},
	}
	for cell, want := range tgt {
		got, err := parseTarget(cell, valid)
		if err != nil || got.Kind != want.Kind || !slices.Equal(got.States, want.States) || got.Note != want.Note {
			t.Errorf("%q: got %+v, %v; want %+v", cell, got, err, want)
		}
	}
	for _, cell := range []string{
		"Recorded resume states", "recorded resume state", "State determined by reconciliation.", "`Created` (",
		"`Created` ()", "`Created` (a (b))", "`Created`)", "`Created` (note) (note)", "Created", "`Nowhere` (note)",
		"Recorded resume state (a note)x", "`Created` (`Blocked`)", "Resume",
	} {
		if got, err := parseTarget(cell, valid); err == nil {
			t.Errorf("target %q: accepted, got %+v", cell, got)
		}
	}
}

func TestSpecParserStructure(t *testing.T) {
	doc := func(rows ...string) []string {
		lines := []string{"## 6. Workflow", "", "### Run states", "", "text", "", "| From | Condition | To |", "| --- | --- | --- |"}
		lines = append(lines, rows...)
		return append(lines, "", "After the table.", "", "### Phase states")
	}
	good := doc("| `Created` | cond one | `Blocked` |", "| Any nonterminal state | cond two | `Planning` (note) |")
	tbl, errs := parseTable(good, fsm.MachineRun, "### Run states")
	if len(errs) != 0 || len(tbl.rows) != 2 || tbl.after != "After the table." {
		t.Fatalf("good document: %d rows, after %q, errors %v", len(tbl.rows), tbl.after, errs)
	}
	if tbl.rows[1].to.Note != "note" || tbl.rows[0].condition != "cond one" || tbl.rows[0].line != 9 {
		t.Errorf("unexpected rows: %+v", tbl.rows)
	}

	bad := map[string][]string{
		"missing heading":           {"## x", "| From | Condition | To |", "| --- | --- | --- |"},
		"duplicate heading":         append(slices.Clone(good), "### Run states"),
		"table of the next heading": {"### Run states", "", "text", "### Phase states", "| From | Condition | To |", "| --- | --- | --- |"},
		"no table":                  {"### Run states", "", "text"},
		"missing separator":         {"### Run states", "| From | Condition | To |", "| `Created` | c | `Blocked` |"},
		"header only":               {"### Run states", "| From | Condition | To |"},
		"two cells":                 doc("| `Created` | `Blocked` |"),
		"four cells":                doc("| `Created` | a | b | `Blocked` |"),
		"unknown source":            doc("| Anything | c | `Blocked` |"),
		"unknown target":            doc("| `Created` | c | Somewhere |"),
		"empty condition":           doc("| `Created` |  | `Blocked` |"),
		"state of another machine":  doc("| `Pending` | c | `Blocked` |"),
		"no spaces around pipes":    doc("|`Created`|c|`Blocked`|"),
	}
	for name, lines := range bad {
		if _, errs := parseTable(lines, fsm.MachineRun, "### Run states"); len(errs) == 0 {
			t.Errorf("%s: no error", name)
		}
	}
}

// ---------------------------------------------------------------------------
// Parity
// ---------------------------------------------------------------------------

func TestSpecRowCounts(t *testing.T) {
	want := map[fsm.Machine]int{fsm.MachineRun: 26, fsm.MachinePhase: 22, fsm.MachineAttempt: 11}
	for m, n := range want {
		if got := len(fsm.Rows(m)); got != n {
			t.Errorf("the %s table has %d rows, want %d", m, got, n)
		}
	}
	for _, tbl := range parseSpecTables(t) {
		if got := len(tbl.rows); got != want[tbl.machine] {
			t.Errorf("SPEC.md has %d rows in the %s table, this test expects %d", got, tbl.machine, want[tbl.machine])
		}
	}
}

func TestSpecParity(t *testing.T) {
	for _, tbl := range parseSpecTables(t) {
		t.Run(string(tbl.machine), func(t *testing.T) {
			goRows := fsm.Rows(tbl.machine)
			for _, d := range diffRows(goRows, tbl.rows) {
				t.Error(d)
			}
		})
	}
	// The first sentence after the Run table names the terminal states, which
	// the expansion rule for "Any nonterminal state" relies on.
	run := parseSpecTables(t)[0]
	if !strings.HasPrefix(run.after, terminalSentence) {
		t.Errorf("the text after the Run table starts with %q, want %q", run.after, terminalSentence)
	}
}

// diffRows compares the Go rows with the SPEC rows and reports every
// difference, naming the Go row.
func diffRows(goRows []fsm.Row, spec []specRow) []string {
	var out []string
	if len(goRows) != len(spec) {
		out = append(out, fmt.Sprintf("row count: Go has %d rows, SPEC.md has %d", len(goRows), len(spec)))
	}
	for i := range min(len(goRows), len(spec)) {
		g, s := goRows[i], spec[i]
		where := fmt.Sprintf("%s (row %d, SPEC.md line %d)", g.ID, i+1, s.line)
		if g.From.Kind != s.from.Kind {
			out = append(out, fmt.Sprintf("%s: source kind is %v in Go, %v in SPEC.md", where, g.From.Kind, s.from.Kind))
		}
		if !slices.Equal(g.From.States, s.from.States) {
			out = append(out, fmt.Sprintf("%s: source states are %q in Go, %q in SPEC.md", where, g.From.States, s.from.States))
		}
		if g.Condition != s.condition {
			out = append(out, fmt.Sprintf("%s: condition is %q in Go, %q in SPEC.md", where, g.Condition, s.condition))
		}
		if g.To.Kind != s.to.Kind {
			out = append(out, fmt.Sprintf("%s: target kind is %v in Go, %v in SPEC.md", where, g.To.Kind, s.to.Kind))
		}
		if !slices.Equal(g.To.States, s.to.States) {
			out = append(out, fmt.Sprintf("%s: target states are %q in Go, %q in SPEC.md", where, g.To.States, s.to.States))
		}
		if g.To.Note != s.to.Note {
			out = append(out, fmt.Sprintf("%s: target note is %q in Go, %q in SPEC.md", where, g.To.Note, s.to.Note))
		}
	}
	for i := len(goRows); i < len(spec); i++ {
		out = append(out, fmt.Sprintf("SPEC.md line %d has a row that Go lacks: %q", spec[i].line, spec[i].condition))
	}
	for i := len(spec); i < len(goRows); i++ {
		out = append(out, fmt.Sprintf("%s has no row in SPEC.md", goRows[i].ID))
	}
	return out
}

func TestSpecParityReportsDifferences(t *testing.T) {
	// The comparison itself must notice every kind of difference. Start from
	// the real rows and change one thing at a time.
	tables := parseSpecTables(t)
	run := tables[0]
	goRows := fsm.Rows(fsm.MachineRun)
	if d := diffRows(goRows, run.rows); len(d) != 0 {
		t.Fatalf("the unmodified tables differ: %v", d)
	}
	mutations := map[string]func(g []fsm.Row, s []specRow) ([]fsm.Row, []specRow){
		"condition": func(g []fsm.Row, s []specRow) ([]fsm.Row, []specRow) { g[4].Condition += "x"; return g, s },
		"source states": func(g []fsm.Row, s []specRow) ([]fsm.Row, []specRow) {
			g[19].From.States = g[19].From.States[1:]
			return g, s
		},
		"source kind": func(g []fsm.Row, s []specRow) ([]fsm.Row, []specRow) {
			g[21].From.Kind = fsm.SelectAnyExcept
			return g, s
		},
		"target states": func(g []fsm.Row, s []specRow) ([]fsm.Row, []specRow) {
			g[0].To.States = []string{"Planning"}
			return g, s
		},
		"target kind": func(g []fsm.Row, s []specRow) ([]fsm.Row, []specRow) { g[20].To.Kind = fsm.TargetStates; return g, s },
		"target note": func(g []fsm.Row, s []specRow) ([]fsm.Row, []specRow) { g[15].To.Note = ""; return g, s },
		"missing Go row": func(g []fsm.Row, s []specRow) ([]fsm.Row, []specRow) {
			return slices.Delete(g, 3, 4), s
		},
		"extra Go row":     func(g []fsm.Row, s []specRow) ([]fsm.Row, []specRow) { return append(g, g[0]), s },
		"missing SPEC row": func(g []fsm.Row, s []specRow) ([]fsm.Row, []specRow) { return g, s[:len(s)-1] },
		"swapped rows":     func(g []fsm.Row, s []specRow) ([]fsm.Row, []specRow) { g[0], g[1] = g[1], g[0]; return g, s },
	}
	for name, mutate := range mutations {
		g, s := mutate(fsm.Rows(fsm.MachineRun), slices.Clone(run.rows))
		if d := diffRows(g, s); len(d) == 0 {
			t.Errorf("a difference in the %s is not reported", name)
		}
	}
}
