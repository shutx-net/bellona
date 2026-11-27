package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/shutx-net/bellona/internal/domain"
	"github.com/shutx-net/bellona/internal/strictjson"
)

// specExample returns the JSON example of SPEC §14: the first json block
// after the heading "## 14. Configuration".
func specExample(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "SPEC.md"))
	if err != nil {
		t.Fatalf("read SPEC.md: %v", err)
	}
	_, after, ok := strings.Cut(string(raw), "\n## 14. Configuration\n")
	if !ok {
		t.Fatal(`SPEC.md has no heading "## 14. Configuration"`)
	}
	_, rest, ok := strings.Cut(after, "```json\n")
	if !ok {
		t.Fatal("SPEC.md has no json block after the configuration heading")
	}
	block, _, ok := strings.Cut(rest, "\n```")
	if !ok {
		t.Fatal("the json block of SPEC §14 is not closed")
	}
	return []byte(block)
}

func mustDecode(t *testing.T, doc string) Config {
	t.Helper()
	cfg, err := Decode([]byte(doc))
	if err != nil {
		t.Fatalf("Decode(%s): %v", doc, err)
	}
	return cfg
}

func TestDecodeSpecExample(t *testing.T) {
	cfg, err := Decode(specExample(t))
	if err != nil {
		t.Fatalf("Decode of the SPEC §14 example: %v", err)
	}
	if cfg.SchemaVersion != 1 {
		t.Errorf("schemaVersion = %d", cfg.SchemaVersion)
	}

	co := cfg.Agents.Coordinator
	if co == nil || co.Runtime != "claude-code" || co.Model != "opus" || co.HostCustomization != "isolated" {
		t.Fatalf("coordinator = %+v", co)
	}
	if !reflect.DeepEqual(co.Effort, &Effort{Mode: "fixed", Value: "high"}) {
		t.Errorf("coordinator effort = %+v", co.Effort)
	}
	wantArgs := []string{"--tools", "Read,Grep,Glob", "--permission-mode", "dontAsk"}
	if !slices.Equal(co.Args, wantArgs) {
		t.Errorf("coordinator args = %q", co.Args)
	}

	pl := cfg.Agents.Planner
	if pl == nil || !reflect.DeepEqual(pl.Effort, &Effort{Mode: "adaptive", DecidedBy: "coordinator"}) {
		t.Errorf("planner = %+v", pl)
	}

	im := cfg.Agents.Implementer
	if im == nil || im.Runtime != "codex" || im.Model != "" {
		t.Fatalf("implementer = %+v", im)
	}
	wantEffort := &Effort{Mode: "adaptive", DecidedBy: "planner", Allowed: []string{"low", "medium", "high"}}
	if !reflect.DeepEqual(im.Effort, wantEffort) {
		t.Errorf("implementer effort = %+v", im.Effort)
	}
	if !slices.Equal(im.Args, []string{"<host arguments that permit edits in the worktree>"}) {
		t.Errorf("implementer args = %q", im.Args)
	}

	ev := cfg.Agents.Evaluator
	if ev == nil || ev.Model != "sonnet" || ev.HostCustomization != "isolated" || len(ev.Args) != 6 {
		t.Fatalf("evaluator = %+v", ev)
	}
	if !reflect.DeepEqual(ev.Effort, &Effort{Mode: "adaptive", DecidedBy: "planner"}) {
		t.Errorf("evaluator effort = %+v", ev.Effort)
	}

	e := cfg.Execution
	if e.Strategy != "phased" || *e.MaxConcurrency != 3 || *e.MaxPhaseRetries != 3 || *e.MaxReplans != 2 ||
		*e.MaxAgentSteps != 200 || *e.MaxTurnCostUSD != 5 || e.Timeout != "2h" || e.RequirePlanApproval != nil {
		t.Errorf("execution = %+v", e)
	}
	wantCommands := map[string]Command{
		"deps":       {Argv: []string{"go", "mod", "download"}},
		"unit-tests": {Argv: []string{"go", "test", "./..."}},
		"lint":       {Argv: []string{"go", "vet", "./..."}},
	}
	if !reflect.DeepEqual(cfg.Commands, wantCommands) {
		t.Errorf("commands = %+v", cfg.Commands)
	}
	wantVerification := Verification{
		IndependentReview: "final",
		RequiredChecks:    []string{"unit-tests", "lint"},
		ProtectedPaths:    []string{"**/*_test.go"},
	}
	if !reflect.DeepEqual(cfg.Verification, wantVerification) {
		t.Errorf("verification = %+v", cfg.Verification)
	}
	if w := cfg.Workspace; w.RequireCleanRepository == nil || !*w.RequireCleanRepository || !slices.Equal(w.Setup, []string{"deps"}) {
		t.Errorf("workspace = %+v", w)
	}
}

func TestSpecExampleResolvesAndRoundTrips(t *testing.T) {
	user := mustDecode(t, string(specExample(t)))
	eff, err := Resolve(Inputs{User: &user})
	if err != nil {
		t.Fatalf("Resolve of the SPEC example: %v", err)
	}
	// What the example leaves out comes from the defaults.
	if eff.Execution.RequirePlanApproval == nil || !*eff.Execution.RequirePlanApproval {
		t.Error("requirePlanApproval is not true by default")
	}
	if *eff.Execution.MaxConcurrency != 3 {
		t.Errorf("maxConcurrency = %d, want the example's 3", *eff.Execution.MaxConcurrency)
	}
	data, err := strictjson.EncodeIndent(eff)
	if err != nil {
		t.Fatal(err)
	}
	again, err := Decode(data)
	if err != nil {
		t.Fatalf("the effective configuration does not decode again: %v", err)
	}
	data2, err := strictjson.EncodeIndent(again)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(data2) {
		t.Errorf("round trip changed the document:\n%s\n%s", data, data2)
	}
	if err := again.ValidateEffective(); err != nil {
		t.Errorf("the decoded effective configuration is invalid: %v", err)
	}
}

// doc builds a layer document from the members after schemaVersion.
func doc(members string) string {
	if members == "" {
		return `{"schemaVersion":1}`
	}
	return `{"schemaVersion":1,` + members + `}`
}

func agent(role, members string) string {
	return doc(`"agents":{"` + role + `":{` + members + `}}`)
}

func exec(members string) string { return doc(`"execution":{` + members + `}`) }

func ver(members string) string { return doc(`"verification":{` + members + `}`) }

func ws(members string) string { return doc(`"workspace":{` + members + `}`) }

func cmds(members string) string { return doc(`"commands":{` + members + `}`) }

type decodeCase struct {
	name    string
	in      string
	want    error
	pointer string
}

func checkDecodeError(t *testing.T, in string, want error, pointer string) {
	t.Helper()
	got, err := Decode([]byte(in))
	if err == nil {
		t.Fatalf("Decode(%s) succeeded with %+v, want an error matching %v", in, got, want)
	}
	if !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("error %v does not match ErrInvalidConfig", err)
	}
	if !errors.Is(err, want) {
		t.Errorf("error %v does not match %v", err, want)
	}
	var se *strictjson.Error
	if !errors.As(err, &se) {
		t.Fatalf("error %T is not a *strictjson.Error", err)
	}
	if se.Pointer != pointer {
		t.Errorf("pointer = %q, want %q (%v)", se.Pointer, pointer, err)
	}
	if !reflect.DeepEqual(got, Config{}) {
		t.Errorf("Decode returned a non-zero value on error: %+v", got)
	}
}

func TestDecodeRejectsMalformedDocuments(t *testing.T) {
	tests := []decodeCase{
		{"empty input", ``, strictjson.ErrInvalidJSON, ""},
		{"invalid JSON", `{"schemaVersion":1,`, strictjson.ErrInvalidJSON, "/schemaVersion"},
		{"not an object", `[]`, strictjson.ErrInvalidValue, ""},
		{"null document", `null`, strictjson.ErrNull, ""},
		{"trailing value", doc("") + doc(""), strictjson.ErrTrailingData, ""},
		{"trailing garbage", doc("") + `x`, strictjson.ErrTrailingData, ""},
		{"duplicate top-level member", `{"schemaVersion":1,"schemaVersion":1}`, strictjson.ErrDuplicateMember, "/schemaVersion"},
		{"duplicate nested member", exec(`"strategy":"phased","strategy":"strict"`), strictjson.ErrDuplicateMember, "/execution/strategy"},
		{"duplicate command name", cmds(`"x":{"argv":["a"]},"x":{"argv":["b"]}`), strictjson.ErrDuplicateMember, "/commands/x"},
		{"unknown top-level member", doc(`"extra":1`), strictjson.ErrUnknownMember, "/extra"},
		{"wrong case member", doc(`"Agents":{}`), strictjson.ErrUnknownMember, "/Agents"},
		{"unknown execution member", exec(`"extra":1`), strictjson.ErrUnknownMember, "/execution/extra"},
		{"unknown agent member", agent("planner", `"extra":1`), strictjson.ErrUnknownMember, "/agents/planner/extra"},
		{"unknown role", doc(`"agents":{"reviewer":{}}`), strictjson.ErrUnknownMember, "/agents/reviewer"},
		{"unknown effort member", agent("planner", `"effort":{"mode":"fixed","value":"low","extra":1}`), strictjson.ErrUnknownMember, "/agents/planner/effort/extra"},
		{"unknown command member", cmds(`"x":{"argv":["a"],"shell":"a"}`), strictjson.ErrUnknownMember, "/commands/x/shell"},
		{"unknown verification member", ver(`"extra":1`), strictjson.ErrUnknownMember, "/verification/extra"},
		{"unknown workspace member", ws(`"extra":1`), strictjson.ErrUnknownMember, "/workspace/extra"},
		{"null object", doc(`"agents":null`), strictjson.ErrNull, "/agents"},
		{"null string", agent("planner", `"model":null`), strictjson.ErrNull, "/agents/planner/model"},
		{"null number", exec(`"maxConcurrency":null`), strictjson.ErrNull, "/execution/maxConcurrency"},
		{"null boolean", exec(`"requirePlanApproval":null`), strictjson.ErrNull, "/execution/requirePlanApproval"},
		{"null list", ver(`"requiredChecks":null`), strictjson.ErrNull, "/verification/requiredChecks"},
		{"null list element", agent("planner", `"args":["a",null]`), strictjson.ErrNull, "/agents/planner/args/1"},
		{"null schemaVersion", `{"schemaVersion":null}`, strictjson.ErrNull, "/schemaVersion"},
		{"missing schemaVersion", `{}`, strictjson.ErrMissingMember, "/schemaVersion"},
		{"missing effort mode", agent("planner", `"effort":{}`), strictjson.ErrMissingMember, "/agents/planner/effort/mode"},
		{"missing argv", cmds(`"x":{}`), strictjson.ErrMissingMember, "/commands/x/argv"},
		{"integer as string", exec(`"maxConcurrency":"2"`), strictjson.ErrInvalidValue, "/execution/maxConcurrency"},
		{"fractional integer", exec(`"maxConcurrency":2.5`), strictjson.ErrInvalidValue, "/execution/maxConcurrency"},
		{"boolean as string", exec(`"requirePlanApproval":"yes"`), strictjson.ErrInvalidValue, "/execution/requirePlanApproval"},
		{"string as list", agent("planner", `"args":"x"`), strictjson.ErrInvalidValue, "/agents/planner/args"},
		{"duration as number", exec(`"timeout":7200`), strictjson.ErrInvalidValue, "/execution/timeout"},
		{"commands as list", doc(`"commands":[]`), strictjson.ErrInvalidValue, "/commands"},
		{"cost out of range", exec(`"maxTurnCostUsd":1e999`), strictjson.ErrInvalidValue, "/execution/maxTurnCostUsd"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { checkDecodeError(t, tt.in, tt.want, tt.pointer) })
	}
}

func TestDecodeRejectsInvalidValues(t *testing.T) {
	tooLongName := strings.Repeat("a", 65)
	tests := []decodeCase{
		{"schemaVersion 0", `{"schemaVersion":0}`, strictjson.ErrInvalidValue, "/schemaVersion"},
		{"schemaVersion 2", `{"schemaVersion":2}`, strictjson.ErrInvalidValue, "/schemaVersion"},

		{"strategy direct", exec(`"strategy":"direct"`), strictjson.ErrInvalidValue, "/execution/strategy"},
		{"unknown strategy", exec(`"strategy":"quick"`), strictjson.ErrInvalidValue, "/execution/strategy"},
		{"strategy case", exec(`"strategy":"Phased"`), strictjson.ErrInvalidValue, "/execution/strategy"},
		{"maxConcurrency 0", exec(`"maxConcurrency":0`), strictjson.ErrInvalidValue, "/execution/maxConcurrency"},
		{"maxConcurrency negative", exec(`"maxConcurrency":-1`), strictjson.ErrInvalidValue, "/execution/maxConcurrency"},
		{"maxPhaseRetries negative", exec(`"maxPhaseRetries":-1`), strictjson.ErrInvalidValue, "/execution/maxPhaseRetries"},
		{"maxReplans negative", exec(`"maxReplans":-1`), strictjson.ErrInvalidValue, "/execution/maxReplans"},
		{"maxAgentSteps 0", exec(`"maxAgentSteps":0`), strictjson.ErrInvalidValue, "/execution/maxAgentSteps"},
		{"maxTurnCostUsd 0", exec(`"maxTurnCostUsd":0`), strictjson.ErrInvalidValue, "/execution/maxTurnCostUsd"},
		{"maxTurnCostUsd negative", exec(`"maxTurnCostUsd":-0.5`), strictjson.ErrInvalidValue, "/execution/maxTurnCostUsd"},
		{"timeout zero", exec(`"timeout":"0s"`), strictjson.ErrInvalidValue, "/execution/timeout"},
		{"timeout negative", exec(`"timeout":"-1h"`), strictjson.ErrInvalidValue, "/execution/timeout"},
		{"timeout without unit", exec(`"timeout":"5"`), strictjson.ErrInvalidValue, "/execution/timeout"},
		{"timeout not a duration", exec(`"timeout":"soon"`), strictjson.ErrInvalidValue, "/execution/timeout"},

		{"runtime upper case", agent("planner", `"runtime":"Claude"`), strictjson.ErrInvalidValue, "/agents/planner/runtime"},
		{"runtime starts with a digit", agent("planner", `"runtime":"1a"`), strictjson.ErrInvalidValue, "/agents/planner/runtime"},
		{"runtime underscore", agent("planner", `"runtime":"claude_code"`), strictjson.ErrInvalidValue, "/agents/planner/runtime"},
		{"blank model", agent("planner", `"model":"  "`), strictjson.ErrInvalidValue, "/agents/planner/model"},

		{"effort unknown mode", agent("planner", `"effort":{"mode":"auto"}`), strictjson.ErrInvalidValue, "/agents/planner/effort/mode"},
		{"effort blank mode", agent("planner", `"effort":{"mode":""}`), strictjson.ErrInvalidValue, "/agents/planner/effort/mode"},
		{"fixed without value", agent("planner", `"effort":{"mode":"fixed"}`), strictjson.ErrInvalidValue, "/agents/planner/effort/value"},
		{"fixed with decidedBy", agent("planner", `"effort":{"mode":"fixed","value":"low","decidedBy":"coordinator"}`), strictjson.ErrInvalidValue, "/agents/planner/effort/decidedBy"},
		{"adaptive without decidedBy", agent("planner", `"effort":{"mode":"adaptive"}`), strictjson.ErrInvalidValue, "/agents/planner/effort/decidedBy"},
		{"adaptive with value", agent("planner", `"effort":{"mode":"adaptive","decidedBy":"coordinator","value":"low"}`), strictjson.ErrInvalidValue, "/agents/planner/effort/value"},
		{"coordinator adaptive", agent("coordinator", `"effort":{"mode":"adaptive","decidedBy":"planner"}`), strictjson.ErrInvalidValue, "/agents/coordinator/effort/mode"},
		{"planner decided by planner", agent("planner", `"effort":{"mode":"adaptive","decidedBy":"planner"}`), strictjson.ErrInvalidValue, "/agents/planner/effort/decidedBy"},
		{"planner decided by user", agent("planner", `"effort":{"mode":"adaptive","decidedBy":"user"}`), strictjson.ErrInvalidValue, "/agents/planner/effort/decidedBy"},
		{"implementer decided by coordinator", agent("implementer", `"effort":{"mode":"adaptive","decidedBy":"coordinator"}`), strictjson.ErrInvalidValue, "/agents/implementer/effort/decidedBy"},
		{"evaluator decided by coordinator", agent("evaluator", `"effort":{"mode":"adaptive","decidedBy":"coordinator"}`), strictjson.ErrInvalidValue, "/agents/evaluator/effort/decidedBy"},
		{"effort value upper case", agent("planner", `"effort":{"mode":"fixed","value":"High"}`), strictjson.ErrInvalidValue, "/agents/planner/effort/value"},
		{"effort allowed empty", agent("planner", `"effort":{"mode":"adaptive","decidedBy":"coordinator","allowed":[]}`), strictjson.ErrInvalidValue, "/agents/planner/effort/allowed"},
		{"effort allowed duplicate", agent("planner", `"effort":{"mode":"adaptive","decidedBy":"coordinator","allowed":["low","low"]}`), strictjson.ErrInvalidValue, "/agents/planner/effort/allowed/1"},
		{"effort allowed token", agent("planner", `"effort":{"mode":"adaptive","decidedBy":"coordinator","allowed":["low","Hi gh"]}`), strictjson.ErrInvalidValue, "/agents/planner/effort/allowed/1"},
		{"fixed value not allowed", agent("planner", `"effort":{"mode":"fixed","value":"max","allowed":["low","high"]}`), strictjson.ErrInvalidValue, "/agents/planner/effort/allowed"},

		{"hostCustomization", agent("planner", `"hostCustomization":"none"`), strictjson.ErrInvalidValue, "/agents/planner/hostCustomization"},
		{"empty argument", agent("planner", `"args":["--a",""]`), strictjson.ErrInvalidValue, "/agents/planner/args/1"},
		{"env name", agent("planner", `"env":["OK","1BAD"]`), strictjson.ErrInvalidValue, "/agents/planner/env/1"},
		{"env name with dash", agent("planner", `"env":["A-B"]`), strictjson.ErrInvalidValue, "/agents/planner/env/0"},
		{"env duplicate", agent("planner", `"env":["A","A"]`), strictjson.ErrInvalidValue, "/agents/planner/env/1"},
		{"subagents value", agent("planner", `"subagents":"all"`), strictjson.ErrInvalidValue, "/agents/planner/subagents"},
		{"subagents readOnly for the implementer", agent("implementer", `"subagents":"readOnly"`), strictjson.ErrInvalidValue, "/agents/implementer/subagents"},
		{"subagents readOnly for the coordinator", agent("coordinator", `"subagents":"readOnly"`), strictjson.ErrInvalidValue, "/agents/coordinator/subagents"},
		{"scopeEnforcement value", agent("implementer", `"scopeEnforcement":"strict"`), strictjson.ErrInvalidValue, "/agents/implementer/scopeEnforcement"},
		{"scopeEnforcement on the planner", agent("planner", `"scopeEnforcement":"detect"`), strictjson.ErrInvalidValue, "/agents/planner/scopeEnforcement"},
		{"allowedCommands on the evaluator", agent("evaluator", `"allowedCommands":[]`), strictjson.ErrInvalidValue, "/agents/evaluator/allowedCommands"},
		{"allowedCommands name", agent("implementer", `"allowedCommands":["fmt","Bad Name"]`), strictjson.ErrInvalidValue, "/agents/implementer/allowedCommands/1"},
		{"allowedCommands duplicate", agent("implementer", `"allowedCommands":["fmt","fmt"]`), strictjson.ErrInvalidValue, "/agents/implementer/allowedCommands/1"},

		{"command name upper case", cmds(`"Lint":{"argv":["a"]}`), strictjson.ErrInvalidValue, "/commands/Lint"},
		{"command name leading dash", cmds(`"-x":{"argv":["a"]}`), strictjson.ErrInvalidValue, "/commands/-x"},
		{"command name too long", cmds(`"` + tooLongName + `":{"argv":["a"]}`), strictjson.ErrInvalidValue, "/commands/" + tooLongName},
		{"command name with a slash", cmds(`"a/b":{"argv":["a"]}`), strictjson.ErrInvalidValue, "/commands/a~1b"},
		{"empty argv", cmds(`"x":{"argv":[]}`), strictjson.ErrInvalidValue, "/commands/x/argv"},
		{"empty program", cmds(`"x":{"argv":["","a"]}`), strictjson.ErrInvalidValue, "/commands/x/argv/0"},

		{"independentReview value", ver(`"independentReview":"always"`), strictjson.ErrInvalidValue, "/verification/independentReview"},
		{"requiredChecks name", ver(`"requiredChecks":["lint","Bad Name"]`), strictjson.ErrInvalidValue, "/verification/requiredChecks/1"},
		{"requiredChecks duplicate", ver(`"requiredChecks":["lint","lint"]`), strictjson.ErrInvalidValue, "/verification/requiredChecks/1"},
		{"protected path empty", ver(`"protectedPaths":["a",""]`), strictjson.ErrInvalidValue, "/verification/protectedPaths/1"},
		{"protected path absolute", ver(`"protectedPaths":["/etc/passwd"]`), strictjson.ErrInvalidValue, "/verification/protectedPaths/0"},
		{"protected path backslash", ver(`"protectedPaths":["a\\b"]`), strictjson.ErrInvalidValue, "/verification/protectedPaths/0"},
		{"protected path dot dot", ver(`"protectedPaths":["../x"]`), strictjson.ErrInvalidValue, "/verification/protectedPaths/0"},
		{"protected path inner dot dot", ver(`"protectedPaths":["a/../b"]`), strictjson.ErrInvalidValue, "/verification/protectedPaths/0"},
		{"protected path trailing dot dot", ver(`"protectedPaths":["a/.."]`), strictjson.ErrInvalidValue, "/verification/protectedPaths/0"},

		{"setup name", ws(`"setup":["deps","Bad Name"]`), strictjson.ErrInvalidValue, "/workspace/setup/1"},
		{"setup duplicate", ws(`"setup":["deps","deps"]`), strictjson.ErrInvalidValue, "/workspace/setup/1"},
		{"root relative", ws(`"root":"worktrees"`), strictjson.ErrInvalidValue, "/workspace/root"},
		{"root not clean", ws(`"root":"/a/../b"`), strictjson.ErrInvalidValue, "/workspace/root"},
		{"root trailing slash", ws(`"root":"/a/"`), strictjson.ErrInvalidValue, "/workspace/root"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { checkDecodeError(t, tt.in, tt.want, tt.pointer) })
	}
}

// An empty string member cannot be told from an absent one, so Decode
// rejects it, while an empty element of a list stays a list element.
func TestDecodeRejectsEmptyStringMembers(t *testing.T) {
	for _, tt := range []decodeCase{
		{"model", agent("planner", `"model":""`), strictjson.ErrInvalidValue, "/agents/planner/model"},
		{"runtime", agent("planner", `"runtime":""`), strictjson.ErrInvalidValue, "/agents/planner/runtime"},
		{"effort value", agent("planner", `"effort":{"mode":"fixed","value":""}`), strictjson.ErrInvalidValue, "/agents/planner/effort/value"},
		{"effort decidedBy", agent("planner", `"effort":{"mode":"adaptive","decidedBy":""}`), strictjson.ErrInvalidValue, "/agents/planner/effort/decidedBy"},
		{"hostCustomization", agent("planner", `"hostCustomization":""`), strictjson.ErrInvalidValue, "/agents/planner/hostCustomization"},
		{"strategy", exec(`"strategy":""`), strictjson.ErrInvalidValue, "/execution/strategy"},
		{"timeout", exec(`"timeout":""`), strictjson.ErrInvalidValue, "/execution/timeout"},
		{"independentReview", ver(`"independentReview":""`), strictjson.ErrInvalidValue, "/verification/independentReview"},
		{"root", ws(`"root":""`), strictjson.ErrInvalidValue, "/workspace/root"},
	} {
		t.Run(tt.name, func(t *testing.T) { checkDecodeError(t, tt.in, tt.want, tt.pointer) })
	}
	// An empty element is validated by the rule of its list, not dropped.
	cfg := mustDecode(t, cmds(`"x":{"argv":["git","commit","-m",""]}`))
	if got := cfg.Commands["x"].Argv; !slices.Equal(got, []string{"git", "commit", "-m", ""}) {
		t.Errorf("argv = %q", got)
	}
}

func TestDecodeAccepts(t *testing.T) {
	tests := []struct{ name, in string }{
		{"schema version only", doc("")},
		{"white space around", " \n" + doc("") + "\n"},
		{"empty objects", doc(`"agents":{},"execution":{},"verification":{},"workspace":{}`)},
		{"empty agent", agent("planner", ``)},
		{"empty lists", doc(`"verification":{"requiredChecks":[],"protectedPaths":[]},"workspace":{"setup":[]}`)},
		{"empty commands", doc(`"commands":{}`)},
		{"zero retries", exec(`"maxPhaseRetries":0,"maxReplans":0`)},
		{"fractional cost", exec(`"maxTurnCostUsd":0.25`)},
		{"integer cost", exec(`"maxTurnCostUsd":5`)},
		{"compound timeout", exec(`"timeout":"1h30m"`)},
		{"strategies", exec(`"strategy":"standard"`)},
		{"strict strategy", exec(`"strategy":"strict"`)},
		{"fixed effort with allowed", agent("planner", `"effort":{"mode":"fixed","value":"high","allowed":["low","high"]}`)},
		{"planner subagents readOnly", agent("planner", `"subagents":"readOnly"`)},
		{"evaluator subagents readOnly", agent("evaluator", `"subagents":"readOnly"`)},
		{"implementer extras", agent("implementer", `"scopeEnforcement":"host","allowedCommands":["fmt"],"env":["GOFLAGS","HOME_X"]`)},
		{"protected path pattern", ver(`"protectedPaths":["**/*_test.go","docs/**","a..b/c"]`)},
		{"command name with dots", cmds(`"go.test-1_x":{"argv":["go","test"]}`)},
		{"absolute root", ws(`"root":"/var/lib/bellona"`)},
		{"false boolean", doc(`"execution":{"requirePlanApproval":false},"workspace":{"requireCleanRepository":false}`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { mustDecode(t, tt.in) })
	}
}

func TestDecodeKeepsAbsentAndEmptyListsApart(t *testing.T) {
	absent := mustDecode(t, doc(""))
	if absent.Verification.RequiredChecks != nil || absent.Commands != nil || absent.Agents.Planner != nil {
		t.Errorf("an absent member decoded as set: %+v", absent)
	}
	empty := mustDecode(t, ver(`"requiredChecks":[]`))
	if empty.Verification.RequiredChecks == nil || len(empty.Verification.RequiredChecks) != 0 {
		t.Errorf("an empty list decoded as %#v", empty.Verification.RequiredChecks)
	}
	for _, c := range []Config{absent, empty} {
		data, err := strictjson.Encode(c)
		if err != nil {
			t.Fatal(err)
		}
		again, err := Decode(data)
		if err != nil {
			t.Fatalf("Decode(%s): %v", data, err)
		}
		if (again.Verification.RequiredChecks == nil) != (c.Verification.RequiredChecks == nil) {
			t.Errorf("round trip of %s changed whether requiredChecks is set", data)
		}
	}
}

func TestContractTypes(t *testing.T) {
	for _, v := range []any{
		Config{}, Agents{}, Agent{}, Effort{}, Execution{}, Command{}, Verification{}, Workspace{},
	} {
		typ := reflect.TypeOf(v)
		t.Run(typ.Name(), func(t *testing.T) {
			if err := strictjson.CheckContractType(typ); err != nil {
				t.Error(err)
			}
		})
	}
}

func TestValidateAcceptsAPartialLayerAndRejectsABadOne(t *testing.T) {
	if err := (Config{SchemaVersion: 1}).Validate(); err != nil {
		t.Errorf("a layer with only the schema version is invalid: %v", err)
	}
	if err := (Config{}).Validate(); !errors.Is(err, ErrInvalidConfig) || !errors.Is(err, strictjson.ErrInvalidValue) {
		t.Errorf("a layer without a schema version: %v", err)
	}
	bad := Config{SchemaVersion: 1, Agents: Agents{Planner: &Agent{Model: " \t"}}}
	err := bad.Validate()
	var se *strictjson.Error
	if !errors.As(err, &se) || se.Pointer != "/agents/planner/model" || !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("Validate of a blank model = %v", err)
	}
}

func TestDefaultsAreValidAndFresh(t *testing.T) {
	d := Defaults()
	if err := d.ValidateEffective(); err != nil {
		t.Fatalf("Defaults does not validate as an effective configuration: %v", err)
	}
	if d.Execution.Strategy != "phased" || *d.Execution.MaxConcurrency != 2 || *d.Execution.MaxPhaseRetries != 3 ||
		*d.Execution.MaxReplans != 2 || d.Execution.Timeout != "2h" || !*d.Execution.RequirePlanApproval ||
		d.Verification.IndependentReview != "final" || !*d.Workspace.RequireCleanRepository {
		t.Errorf("Defaults = %+v", d)
	}
	if d.Execution.MaxAgentSteps != nil || d.Execution.MaxTurnCostUSD != nil || d.Agents != (Agents{}) || d.Commands != nil {
		t.Errorf("Defaults sets a step or cost cap, an agent or a command: %+v", d)
	}
	*d.Execution.MaxConcurrency = 99
	if got := *Defaults().Execution.MaxConcurrency; got != 2 {
		t.Errorf("a second call to Defaults sees the change: maxConcurrency = %d", got)
	}
}

func TestValidateEffective(t *testing.T) {
	good := func() Config {
		c := Defaults()
		c.Commands = map[string]Command{"unit-tests": {Argv: []string{"go", "test"}}, "fmt": {Argv: []string{"gofmt"}}}
		c.Verification.RequiredChecks = []string{"unit-tests"}
		c.Workspace.Setup = []string{"fmt"}
		c.Agents.Implementer = &Agent{AllowedCommands: []string{"fmt"}}
		return c
	}
	if err := good().ValidateEffective(); err != nil {
		t.Fatalf("a good effective configuration is invalid: %v", err)
	}
	tests := []struct {
		name    string
		mutate  func(*Config)
		pointer string
	}{
		{"undefined required check", func(c *Config) { c.Verification.RequiredChecks = []string{"unit-tests", "lint"} }, "/verification/requiredChecks/1"},
		{"undefined setup command", func(c *Config) { c.Workspace.Setup = []string{"deps"} }, "/workspace/setup/0"},
		{"undefined allowed command", func(c *Config) { c.Agents.Implementer.AllowedCommands = []string{"gen"} }, "/agents/implementer/allowedCommands/0"},
		{"no commands at all", func(c *Config) { c.Commands = nil }, "/verification/requiredChecks/0"},
		{"strategy unset", func(c *Config) { c.Execution.Strategy = "" }, "/execution/strategy"},
		{"maxConcurrency unset", func(c *Config) { c.Execution.MaxConcurrency = nil }, "/execution/maxConcurrency"},
		{"maxPhaseRetries unset", func(c *Config) { c.Execution.MaxPhaseRetries = nil }, "/execution/maxPhaseRetries"},
		{"maxReplans unset", func(c *Config) { c.Execution.MaxReplans = nil }, "/execution/maxReplans"},
		{"requirePlanApproval unset", func(c *Config) { c.Execution.RequirePlanApproval = nil }, "/execution/requirePlanApproval"},
		{"independentReview unset", func(c *Config) { c.Verification.IndependentReview = "" }, "/verification/independentReview"},
		{"requireCleanRepository unset", func(c *Config) { c.Workspace.RequireCleanRepository = nil }, "/workspace/requireCleanRepository"},
		{"layer rule", func(c *Config) { c.Execution.Strategy = "direct" }, "/execution/strategy"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := good()
			tt.mutate(&c)
			err := c.ValidateEffective()
			var se *strictjson.Error
			if !errors.As(err, &se) || !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("ValidateEffective = %v, want an invalid configuration error", err)
			}
			if se.Pointer != tt.pointer {
				t.Errorf("pointer = %q, want %q (%v)", se.Pointer, tt.pointer, err)
			}
		})
	}
	// Agents and commands are optional.
	if err := Defaults().ValidateEffective(); err != nil {
		t.Errorf("Defaults: %v", err)
	}
}

func TestLimits(t *testing.T) {
	spec := mustDecode(t, string(specExample(t)))
	tests := []struct {
		name   string
		cfg    Config
		want   domain.Limits
		failed bool
	}{
		{"SPEC example", spec, domain.Limits{MaxPhaseRetries: 3, MaxReplans: 2, Timeout: "2h0m0s", MaxAgentSteps: 200, MaxTurnCostUSD: 5}, false},
		{"defaults", Defaults(), domain.Limits{MaxPhaseRetries: 3, MaxReplans: 2, Timeout: "2h0m0s"}, false},
		{"canonical timeout", mustDecode(t, exec(`"maxPhaseRetries":0,"maxReplans":1,"timeout":"90m"`)), domain.Limits{MaxReplans: 1, Timeout: "1h30m0s"}, false},
		{"canonical compound timeout", mustDecode(t, exec(`"maxPhaseRetries":1,"maxReplans":1,"timeout":"1h30m"`)), domain.Limits{MaxPhaseRetries: 1, MaxReplans: 1, Timeout: "1h30m0s"}, false},
		{"fractional cost", mustDecode(t, exec(`"maxPhaseRetries":1,"maxReplans":1,"maxTurnCostUsd":0.25`)), domain.Limits{MaxPhaseRetries: 1, MaxReplans: 1, MaxTurnCostUSD: 0.25}, false},
		{"no timeout", mustDecode(t, exec(`"maxPhaseRetries":1,"maxReplans":1`)), domain.Limits{MaxPhaseRetries: 1, MaxReplans: 1}, false},
		{"unset retries", mustDecode(t, exec(`"maxReplans":1`)), domain.Limits{}, true},
		{"unset replans", mustDecode(t, exec(`"maxPhaseRetries":1`)), domain.Limits{}, true},
		{"empty configuration", Config{}, domain.Limits{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.cfg.Limits()
			if tt.failed {
				if !errors.Is(err, ErrInvalidConfig) {
					t.Fatalf("Limits error = %v, want ErrInvalidConfig", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Limits: %v", err)
			}
			if got != tt.want {
				t.Errorf("Limits = %+v, want %+v", got, tt.want)
			}
			if err := got.Validate(); err != nil {
				t.Errorf("the limits do not validate: %v", err)
			}
		})
	}
	// A timeout that bypassed Validate is still refused.
	c := Defaults()
	c.Execution.Timeout = "soon"
	if _, err := c.Limits(); !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("Limits with a bad timeout = %v", err)
	}
	if got, _ := Defaults().Limits(); got.Value(domain.LimitTimeout) != "2h0m0s" {
		t.Errorf("canonical timeout = %q", got.Value(domain.LimitTimeout))
	}
}

func TestCloneSharesNothing(t *testing.T) {
	orig := mustDecode(t, string(specExample(t)))
	orig.Agents.Implementer.AllowedCommands = []string{"deps"}
	orig.Verification.RequiredChecks = []string{} // an empty list stays empty
	clone := orig.Clone()
	if !reflect.DeepEqual(orig, clone) {
		t.Fatalf("Clone differs from the original:\n%+v\n%+v", orig, clone)
	}
	if clone.Verification.RequiredChecks == nil {
		t.Error("Clone turned an empty list into nil")
	}
	// Change every kind of shared storage of the clone.
	*clone.Execution.MaxConcurrency = 42
	*clone.Execution.MaxTurnCostUSD = 42
	*clone.Workspace.RequireCleanRepository = false
	clone.Agents.Coordinator.Args[0] = "changed"
	clone.Agents.Implementer.Effort.Allowed[0] = "changed"
	clone.Agents.Implementer.AllowedCommands[0] = "changed"
	clone.Commands["lint"].Argv[0] = "changed"
	clone.Commands["added"] = Command{Argv: []string{"x"}}
	clone.Verification.ProtectedPaths[0] = "changed"
	clone.Workspace.Setup[0] = "changed"
	want := mustDecode(t, string(specExample(t)))
	want.Agents.Implementer.AllowedCommands = []string{"deps"}
	want.Verification.RequiredChecks = []string{}
	if !reflect.DeepEqual(orig, want) {
		t.Errorf("changing the clone changed the original:\n%+v", orig)
	}
	if !reflect.DeepEqual(Config{}.Clone(), Config{}) {
		t.Error("Clone of the zero Config is not the zero Config")
	}
}

func TestPointerEscapesTokens(t *testing.T) {
	if got := pointer("commands", "a/b", "c~d"); got != "/commands/a~1b/c~0d" {
		t.Errorf("pointer = %q", got)
	}
}
