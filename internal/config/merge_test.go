package config

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/shutx-net/bellona/internal/domain"
	"github.com/shutx-net/bellona/internal/strictjson"
)

// kv is one setting of a test layer: the dotted JSON path of a field and its
// value. A pointer field takes the plain value (an int for a *int).
type kv struct {
	path  string
	value any
}

// set stores v in the field of c at path, allocating what is on the way.
func set(t *testing.T, c *Config, path string, v any) {
	t.Helper()
	f, ok := lookup(reflect.ValueOf(c).Elem(), path, true)
	if !ok {
		t.Fatalf("no field %q", path)
	}
	rv := reflect.ValueOf(v)
	if f.Kind() == reflect.Pointer && rv.Type() != f.Type() {
		p := reflect.New(f.Type().Elem())
		p.Elem().Set(rv.Convert(f.Type().Elem()))
		rv = p
	}
	f.Set(rv)
}

// layer builds a layer from settings. It is never nil.
func layer(t *testing.T, settings ...kv) *Config {
	t.Helper()
	c := &Config{SchemaVersion: SchemaVersion}
	for _, s := range settings {
		set(t, c, s.path, s.value)
	}
	return c
}

// leaf returns the value of the field at path, with a pointer dereferenced,
// or nil when the field is not set.
func leaf(t *testing.T, c Config, path string) any {
	t.Helper()
	f, ok := lookup(reflect.ValueOf(c), path, false)
	if !ok || !isSet(f) {
		return nil
	}
	if f.Kind() == reflect.Pointer && f.Type() != reflect.TypeFor[*Effort]() {
		f = f.Elem()
	}
	return f.Interface()
}

func resolve(t *testing.T, in Inputs) Config {
	t.Helper()
	in.ProjectTrusted = in.Project != nil
	c, err := Resolve(in)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return c
}

// restrictionPolicies are the entries of the table that are restrictions.
func restrictionPolicies() []policy {
	var out []policy
	for _, p := range policies {
		if p.Kind.restriction() {
			out = append(out, p)
		}
	}
	return out
}

// roleOf returns the role named by an agents path, or "".
func roleOf(path string) domain.Role {
	parts := strings.Split(path, ".")
	if len(parts) == 3 && parts[0] == "agents" {
		return domain.Role(parts[1])
	}
	return ""
}

// leafPaths lists the dotted JSON path of every setting of a configuration
// type, the way the policy table names them: struct members are descended
// into, and Effort, a map and a scalar or list are one setting.
func leafPaths(t reflect.Type, prefix string) []string {
	var out []string
	for i := range t.NumField() {
		f := t.Field(i)
		path := jsonName(f)
		if prefix != "" {
			path = prefix + "." + path
		}
		ft := f.Type
		if ft.Kind() == reflect.Pointer && ft.Elem().Kind() == reflect.Struct && ft != reflect.TypeFor[*Effort]() {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct {
			out = append(out, leafPaths(ft, path)...)
		} else {
			out = append(out, path)
		}
	}
	return out
}

func TestPolicyTableCoversEveryField(t *testing.T) {
	var want []string
	for _, p := range leafPaths(reflect.TypeFor[Config](), "") {
		if strings.HasPrefix(p, "agents.") {
			// agents.<role>.<field>: expand the role.
			_, field, _ := strings.Cut(strings.TrimPrefix(p, "agents."), ".")
			for _, r := range domain.Roles() {
				want = append(want, "agents."+string(r)+"."+field)
			}
			continue
		}
		want = append(want, p)
	}
	var got []string
	for _, p := range policies {
		got = append(got, p.Path)
	}
	slices.Sort(want)
	slices.Sort(got)
	want = slices.Compact(want)
	if !slices.Equal(want, got) {
		t.Errorf("the policy table and the fields of Config differ:\n fields: %v\n table:  %v", want, got)
	}
	// Every path resolves to a field, and no path is listed twice.
	seen := make(map[string]bool)
	zero := reflect.ValueOf(&Config{}).Elem()
	for _, p := range policies {
		if seen[p.Path] {
			t.Errorf("%s is listed twice", p.Path)
		}
		seen[p.Path] = true
		if _, ok := lookup(zero, p.Path, true); !ok {
			t.Errorf("%s names no field of Config", p.Path)
		}
	}
}

func TestPolicyTableRestrictionsArePinned(t *testing.T) {
	want := []string{
		"execution.maxConcurrency", "execution.maxPhaseRetries", "execution.maxReplans",
		"execution.maxAgentSteps", "execution.maxTurnCostUsd", "execution.timeout",
		"execution.requirePlanApproval", "workspace.requireCleanRepository",
		"verification.independentReview", "verification.requiredChecks", "verification.protectedPaths",
	}
	for _, r := range domain.Roles() {
		want = append(want, "agents."+string(r)+".subagents")
	}
	var got []string
	for _, p := range restrictionPolicies() {
		got = append(got, p.Path)
	}
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(want, got) {
		t.Errorf("restriction fields:\n got  %v\n want %v", got, want)
	}
	wantKinds := map[string]kind{
		"execution.maxConcurrency": stricterMin, "execution.maxPhaseRetries": stricterMin,
		"execution.maxReplans": stricterMin, "execution.maxAgentSteps": stricterMin,
		"execution.maxTurnCostUsd": stricterMin, "execution.timeout": stricterShortest,
		"execution.requirePlanApproval": stricterTrue, "workspace.requireCleanRepository": stricterTrue,
		"verification.independentReview": stricterReview, "verification.requiredChecks": union,
		"verification.protectedPaths": union, "agents.planner.subagents": stricterSubagents,
	}
	for _, p := range policies {
		if k, ok := wantKinds[p.Path]; ok && p.Kind != k {
			t.Errorf("%s has kind %s, want %s", p.Path, p.Kind, k)
		}
	}
	// The fields that the user did not name as restrictions stay ordinary.
	for _, path := range []string{
		"agents.implementer.hostCustomization", "agents.implementer.scopeEnforcement",
		"agents.implementer.allowedCommands", "agents.planner.args", "agents.planner.env",
		"workspace.setup", "workspace.root", "execution.strategy", "commands",
	} {
		for _, p := range policies {
			if p.Path == path && p.Kind.restriction() {
				t.Errorf("%s must not be a restriction", path)
			}
		}
	}
	if kind(99).String() != "kind(99)" || ordinary.String() != "ordinary" || union.String() != "union" {
		t.Error("kind.String")
	}
}

// tiers returns sample values of a restriction field ordered from the
// strictest to the loosest. Every value is valid for the field.
func tiers(t *testing.T, p policy) []any {
	t.Helper()
	switch p.Kind {
	case stricterMin:
		if p.Path == "execution.maxTurnCostUsd" {
			return []any{0.5, 2.5, 7.5}
		}
		return []any{1, 4, 9}
	case stricterShortest:
		// Compared as durations: as text "10h" sorts before "9m".
		return []any{"9m", "45m", "10h"}
	case stricterTrue:
		return []any{true, false}
	case stricterReview:
		return []any{"phase", "final", "none"}
	case stricterSubagents:
		if r := roleOf(p.Path); r == domain.RolePlanner || r == domain.RoleEvaluator {
			return []any{"none", "readOnly"}
		}
		return []any{"none"} // readOnly is invalid for the coordinator and the implementer
	}
	t.Fatalf("no sample values for %s of kind %s", p.Path, p.Kind)
	return nil
}

func TestRestrictionFields(t *testing.T) {
	for _, p := range restrictionPolicies() {
		if p.Kind == union {
			continue
		}
		tr := tiers(t, p)
		strictest, loosest := tr[0], tr[len(tr)-1]
		looser := len(tr) > 1
		check := func(t *testing.T, in Inputs, want any) {
			t.Helper()
			got := leaf(t, resolve(t, in), p.Path)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("effective %s = %v (%T), want %v (%T)", p.Path, got, got, want, want)
			}
		}
		t.Run(p.Path, func(t *testing.T) {
			if looser {
				t.Run("a user cannot loosen a project restriction", func(t *testing.T) {
					check(t, Inputs{User: layer(t, kv{p.Path, loosest}), Project: layer(t, kv{p.Path, strictest})}, strictest)
				})
				t.Run("a project cannot loosen a user restriction", func(t *testing.T) {
					check(t, Inputs{User: layer(t, kv{p.Path, strictest}), Project: layer(t, kv{p.Path, loosest})}, strictest)
				})
				t.Run("the CLI can loosen both", func(t *testing.T) {
					check(t, Inputs{
						CLI:     layer(t, kv{p.Path, loosest}),
						User:    layer(t, kv{p.Path, strictest}),
						Project: layer(t, kv{p.Path, strictest}),
					}, loosest)
				})
				t.Run("the CLI can loosen the user", func(t *testing.T) {
					check(t, Inputs{CLI: layer(t, kv{p.Path, loosest}), User: layer(t, kv{p.Path, strictest})}, loosest)
				})
				t.Run("the CLI can loosen the project", func(t *testing.T) {
					check(t, Inputs{CLI: layer(t, kv{p.Path, loosest}), Project: layer(t, kv{p.Path, strictest})}, loosest)
				})
				t.Run("the CLI can tighten both", func(t *testing.T) {
					check(t, Inputs{
						CLI:     layer(t, kv{p.Path, strictest}),
						User:    layer(t, kv{p.Path, loosest}),
						Project: layer(t, kv{p.Path, loosest}),
					}, strictest)
				})
			}
			if len(tr) > 2 {
				t.Run("the stricter of two set values, whatever the order", func(t *testing.T) {
					mid := tr[1]
					check(t, Inputs{User: layer(t, kv{p.Path, mid}), Project: layer(t, kv{p.Path, loosest})}, mid)
					check(t, Inputs{User: layer(t, kv{p.Path, loosest}), Project: layer(t, kv{p.Path, mid})}, mid)
					check(t, Inputs{User: layer(t, kv{p.Path, strictest}), Project: layer(t, kv{p.Path, mid})}, strictest)
					check(t, Inputs{User: layer(t, kv{p.Path, mid}), Project: layer(t, kv{p.Path, strictest})}, strictest)
				})
			}
			t.Run("a value set by one layer only is used as it is", func(t *testing.T) {
				// Even the loosest value: a default is never compared with a layer.
				check(t, Inputs{User: layer(t, kv{p.Path, loosest})}, loosest)
				check(t, Inputs{Project: layer(t, kv{p.Path, loosest})}, loosest)
				check(t, Inputs{CLI: layer(t, kv{p.Path, loosest})}, loosest)
				check(t, Inputs{User: layer(t, kv{p.Path, strictest})}, strictest)
				check(t, Inputs{Project: layer(t, kv{p.Path, strictest})}, strictest)
			})
			t.Run("a field that no layer sets takes the default", func(t *testing.T) {
				want := leaf(t, Defaults(), p.Path)
				check(t, Inputs{}, want)
				// Layers that set other things do not change that. The strategy is
				// not "strict": Strict raises the review (TestStrictRaisesTheReviewMinimum).
				check(t, Inputs{
					CLI:     layer(t, kv{"workspace.root", "/cli"}),
					User:    layer(t, kv{"workspace.root", "/user"}),
					Project: layer(t, kv{"execution.strategy", "standard"}),
				}, want)
			})
		})
	}
}

func TestRestrictionsComparedAsDurationsAndNumbers(t *testing.T) {
	timeout := func(user, project string) any {
		in := Inputs{User: layer(t, kv{"execution.timeout", user}), Project: layer(t, kv{"execution.timeout", project})}
		return resolve(t, in).Execution.Timeout
	}
	if got := timeout("10h", "9m"); got != "9m" {
		t.Errorf("timeout of 10h and 9m = %v", got)
	}
	if got := timeout("9m", "10h"); got != "9m" {
		t.Errorf("timeout of 9m and 10h = %v", got)
	}
	if got := timeout("90m", "1h"); got != "1h" {
		t.Errorf("timeout of 90m and 1h = %v", got)
	}
	if got := timeout("1.5h", "100m"); got != "1.5h" {
		t.Errorf("timeout of 1.5h and 100m = %v", got)
	}
	if got := timeout("100m", "1.5h"); got != "1.5h" {
		t.Errorf("timeout of 100m and 1.5h = %v", got)
	}
	// The effective value keeps the text of the layer it comes from.
	if got := timeout("2h30m", "3h"); got != "2h30m" {
		t.Errorf("timeout = %v", got)
	}
	conc := func(user, project int) int {
		in := Inputs{User: layer(t, kv{"execution.maxConcurrency", user}), Project: layer(t, kv{"execution.maxConcurrency", project})}
		return *resolve(t, in).Execution.MaxConcurrency
	}
	if got := conc(10, 9); got != 9 {
		t.Errorf("maxConcurrency of 10 and 9 = %d", got)
	}
	if got := conc(3, 3); got != 3 {
		t.Errorf("maxConcurrency of 3 and 3 = %d", got)
	}
	// A lone value above the default is not lowered to the default.
	if got := *resolve(t, Inputs{User: layer(t, kv{"execution.maxConcurrency", 8})}).Execution.MaxConcurrency; got != 8 {
		t.Errorf("a lone user maxConcurrency of 8 = %d", got)
	}
	if got := resolve(t, Inputs{User: layer(t, kv{"execution.timeout", "10h"})}).Execution.Timeout; got != "10h" {
		t.Errorf("a lone user timeout of 10h = %s", got)
	}
	// A step or cost cap that no layer sets is not set: it is not a loose
	// value that a stricter one would replace.
	eff := resolve(t, Inputs{User: layer(t, kv{"execution.maxAgentSteps", 50}), Project: layer(t)})
	if *eff.Execution.MaxAgentSteps != 50 || eff.Execution.MaxTurnCostUSD != nil {
		t.Errorf("caps = %v, %v", eff.Execution.MaxAgentSteps, eff.Execution.MaxTurnCostUSD)
	}
	if got := resolve(t, Inputs{}).Execution; got.MaxAgentSteps != nil || got.MaxTurnCostUSD != nil {
		t.Errorf("the defaults set a cap: %+v", got)
	}
}

func TestUnionRestrictions(t *testing.T) {
	commands := kv{"commands", map[string]Command{
		"a": {Argv: []string{"a"}}, "b": {Argv: []string{"b"}}, "c": {Argv: []string{"c"}}, "z": {Argv: []string{"z"}},
	}}
	checks := func(in Inputs) any {
		t.Helper()
		return leaf(t, resolve(t, in), "verification.requiredChecks")
	}
	const path = "verification.requiredChecks"
	t.Run("user then project, first appearance, no duplicates", func(t *testing.T) {
		in := Inputs{
			User:    layer(t, commands, kv{path, []string{"b", "a"}}),
			Project: layer(t, kv{path, []string{"a", "c", "b"}}),
		}
		if got := checks(in); !reflect.DeepEqual(got, []string{"b", "a", "c"}) {
			t.Errorf("requiredChecks = %v, want [b a c]", got)
		}
	})
	t.Run("a project cannot remove a user check and a user cannot remove a project check", func(t *testing.T) {
		in := Inputs{User: layer(t, commands, kv{path, []string{"a"}}), Project: layer(t, kv{path, []string{"b"}})}
		if got := checks(in); !reflect.DeepEqual(got, []string{"a", "b"}) {
			t.Errorf("requiredChecks = %v", got)
		}
		in = Inputs{User: layer(t, commands, kv{path, []string{}}), Project: layer(t, kv{path, []string{"b"}})}
		if got := checks(in); !reflect.DeepEqual(got, []string{"b"}) {
			t.Errorf("an empty user list removed the project check: %v", got)
		}
		in = Inputs{User: layer(t, commands, kv{path, []string{"a"}}), Project: layer(t, kv{path, []string{}})}
		if got := checks(in); !reflect.DeepEqual(got, []string{"a"}) {
			t.Errorf("an empty project list removed the user check: %v", got)
		}
	})
	t.Run("one set list is used as it is", func(t *testing.T) {
		if got := checks(Inputs{User: layer(t, commands, kv{path, []string{"c", "a"}})}); !reflect.DeepEqual(got, []string{"c", "a"}) {
			t.Errorf("user only: %v", got)
		}
		in := Inputs{User: layer(t, commands), Project: layer(t, kv{path, []string{"c", "a"}})}
		if got := checks(in); !reflect.DeepEqual(got, []string{"c", "a"}) {
			t.Errorf("project only: %v", got)
		}
	})
	t.Run("the CLI list replaces both", func(t *testing.T) {
		in := Inputs{
			CLI:     layer(t, kv{path, []string{"z"}}),
			User:    layer(t, commands, kv{path, []string{"a"}}),
			Project: layer(t, kv{path, []string{"b"}}),
		}
		if got := checks(in); !reflect.DeepEqual(got, []string{"z"}) {
			t.Errorf("requiredChecks = %v, want [z]", got)
		}
		in.CLI = layer(t, kv{path, []string{}})
		if got := checks(in); !reflect.DeepEqual(got, []string{}) {
			t.Errorf("an empty CLI list: requiredChecks = %#v, want an empty list", got)
		}
	})
	t.Run("no layer sets it", func(t *testing.T) {
		if got := checks(Inputs{}); !reflect.DeepEqual(got, []string{}) {
			t.Errorf("requiredChecks = %#v, want the default empty list", got)
		}
	})
	t.Run("protected paths", func(t *testing.T) {
		const pp = "verification.protectedPaths"
		in := Inputs{
			User:    layer(t, kv{pp, []string{"**/*_test.go", "Makefile"}}),
			Project: layer(t, kv{pp, []string{"Makefile", "ci/**"}}),
		}
		got := leaf(t, resolve(t, in), pp)
		if !reflect.DeepEqual(got, []string{"**/*_test.go", "Makefile", "ci/**"}) {
			t.Errorf("protectedPaths = %v", got)
		}
		in.CLI = layer(t, kv{pp, []string{"only"}})
		if got := leaf(t, resolve(t, in), pp); !reflect.DeepEqual(got, []string{"only"}) {
			t.Errorf("a CLI list did not replace both: %v", got)
		}
		in = Inputs{User: layer(t, kv{pp, []string{"x", "x"}})}
		if got := leaf(t, resolve(t, in), pp); !reflect.DeepEqual(got, []string{"x", "x"}) {
			t.Errorf("a single list was changed: %v", got)
		}
	})
}

// ordinarySamples holds, for each ordinary field (without the role of an
// agents path), values that are valid for the field.
var ordinarySamples = map[string][]any{
	"agents.runtime":           {"rt-a", "rt-b", "rt-c"},
	"agents.model":             {"model-a", "model-b", "model-c"},
	"agents.effort":            {&Effort{Mode: "fixed", Value: "low"}, &Effort{Mode: "fixed", Value: "medium"}, &Effort{Mode: "fixed", Value: "high", Allowed: []string{"high"}}},
	"agents.hostCustomization": {"inherit", "isolated"},
	"agents.args":              {[]string{"--a"}, []string{"--b", "x"}, []string{}},
	"agents.env":               {[]string{"E_A"}, []string{"E_B", "E_C"}, []string{}},
	"agents.scopeEnforcement":  {"detect", "host"},
	"agents.allowedCommands":   {[]string{"x1"}, []string{"x2", "x3"}, []string{}},
	"execution.strategy":       {"standard", "phased", "strict"},
	"workspace.setup":          {[]string{"x1"}, []string{"x2", "x3"}, []string{}},
	"workspace.root":           {"/r/a", "/r/b", "/r/c"},
}

func sampleKey(path string) string {
	if r := roleOf(path); r != "" {
		return "agents." + strings.TrimPrefix(path, "agents."+string(r)+".")
	}
	return path
}

func TestOrdinaryFieldsFollowPlainPrecedence(t *testing.T) {
	commands := map[string]Command{"x1": {Argv: []string{"x1"}}, "x2": {Argv: []string{"x2"}}, "x3": {Argv: []string{"x3"}}}
	for _, p := range policies {
		if p.Kind != ordinary || p.Path == "schemaVersion" {
			continue
		}
		samples, ok := ordinarySamples[sampleKey(p.Path)]
		if !ok {
			t.Errorf("no sample values for the ordinary field %s: add them to ordinarySamples", p.Path)
			continue
		}
		if r := roleOf(p.Path); r != domain.RoleImplementer && (strings.HasSuffix(p.Path, ".scopeEnforcement") || strings.HasSuffix(p.Path, ".allowedCommands")) {
			continue // exists for the implementer only
		}
		t.Run(p.Path, func(t *testing.T) {
			a, b := samples[0], samples[1]
			// The project always defines the commands that lists may name.
			project := func(settings ...kv) *Config {
				return layer(t, append([]kv{{"commands", commands}}, settings...)...)
			}
			check := func(name string, in Inputs, want any) {
				t.Helper()
				got := leaf(t, resolve(t, in), p.Path)
				if !reflect.DeepEqual(got, want) {
					t.Errorf("%s: effective %s = %v, want %v", name, p.Path, got, want)
				}
			}
			check("project only", Inputs{Project: project(kv{p.Path, a})}, a)
			check("user only", Inputs{User: layer(t, kv{p.Path, a}), Project: project()}, a)
			check("CLI only", Inputs{CLI: layer(t, kv{p.Path, a}), Project: project()}, a)
			check("user over project", Inputs{User: layer(t, kv{p.Path, a}), Project: project(kv{p.Path, b})}, a)
			check("CLI over project", Inputs{CLI: layer(t, kv{p.Path, a}), Project: project(kv{p.Path, b})}, a)
			check("CLI over user", Inputs{CLI: layer(t, kv{p.Path, a}), User: layer(t, kv{p.Path, b}), Project: project()}, a)
			if len(samples) > 2 {
				c := samples[2]
				check("CLI over user over project",
					Inputs{CLI: layer(t, kv{p.Path, c}), User: layer(t, kv{p.Path, b}), Project: project(kv{p.Path, a})}, c)
			}
			check("default", Inputs{Project: project()}, leaf(t, Defaults(), p.Path))
			// A list or an effort object is one value: a higher layer
			// replaces it, it is not merged with the lower one.
			if _, isList := a.([]string); isList {
				check("lists are not merged", Inputs{User: layer(t, kv{p.Path, a}), Project: project(kv{p.Path, b})}, a)
			}
		})
	}
}

func TestOrdinaryPrecedenceOfNamedExamples(t *testing.T) {
	// agents.<role>.<field> merges per role and per field.
	user := layer(t,
		kv{"agents.planner.model", "user-model"},
		kv{"agents.planner.runtime", "user-rt"},
		kv{"agents.evaluator.model", "user-eval"},
	)
	project := layer(t,
		kv{"agents.planner.model", "project-model"},
		kv{"agents.planner.hostCustomization", "isolated"},
		kv{"agents.implementer.model", "project-impl"},
	)
	cli := layer(t, kv{"agents.evaluator.model", "cli-eval"})
	eff := resolve(t, Inputs{CLI: cli, User: user, Project: project})
	pl := eff.Agents.Planner
	if pl == nil || pl.Model != "user-model" || pl.Runtime != "user-rt" || pl.HostCustomization != "isolated" {
		t.Errorf("planner = %+v", pl)
	}
	if eff.Agents.Evaluator == nil || eff.Agents.Evaluator.Model != "cli-eval" {
		t.Errorf("evaluator = %+v", eff.Agents.Evaluator)
	}
	if eff.Agents.Implementer == nil || eff.Agents.Implementer.Model != "project-impl" {
		t.Errorf("implementer = %+v", eff.Agents.Implementer)
	}
	if eff.Agents.Coordinator != nil {
		t.Errorf("coordinator = %+v, want none: no layer sets anything for it", eff.Agents.Coordinator)
	}

	// execution.strategy: CLI over user over project over default.
	strategy := func(cli, user, project string) string {
		in := Inputs{}
		if cli != "" {
			in.CLI = layer(t, kv{"execution.strategy", cli})
		}
		if user != "" {
			in.User = layer(t, kv{"execution.strategy", user})
		}
		if project != "" {
			in.Project = layer(t, kv{"execution.strategy", project})
		}
		return resolve(t, in).Execution.Strategy
	}
	for _, tt := range []struct{ cli, user, project, want string }{
		{"strict", "standard", "phased", "strict"},
		{"", "standard", "strict", "standard"},
		{"", "", "strict", "strict"},
		{"", "", "", "phased"},
		{"standard", "", "strict", "standard"},
	} {
		if got := strategy(tt.cli, tt.user, tt.project); got != tt.want {
			t.Errorf("strategy(cli=%q, user=%q, project=%q) = %q, want %q", tt.cli, tt.user, tt.project, got, tt.want)
		}
	}
}

func TestCommandsMergePerName(t *testing.T) {
	cmd := func(prog string) Command { return Command{Argv: []string{prog, "x"}} }
	cli := layer(t, kv{"commands", map[string]Command{"lint": cmd("cli-lint")}})
	user := layer(t, kv{"commands", map[string]Command{"lint": cmd("user-lint"), "fmt": cmd("user-fmt")}})
	project := layer(t, kv{"commands", map[string]Command{
		"lint": cmd("project-lint"), "fmt": cmd("project-fmt"), "test": cmd("project-test"),
	}})
	eff := resolve(t, Inputs{CLI: cli, User: user, Project: project})
	want := map[string]Command{"lint": cmd("cli-lint"), "fmt": cmd("user-fmt"), "test": cmd("project-test")}
	if !reflect.DeepEqual(eff.Commands, want) {
		t.Errorf("commands = %+v, want %+v", eff.Commands, want)
	}
	if got := resolve(t, Inputs{}).Commands; got != nil {
		t.Errorf("commands without any layer = %+v", got)
	}
	// An empty map in a layer sets nothing to merge but is not dropped.
	if got := resolve(t, Inputs{User: layer(t, kv{"commands", map[string]Command{}})}).Commands; got == nil || len(got) != 0 {
		t.Errorf("an empty commands map became %#v", got)
	}
}

func TestUntrustedProjectIsRefused(t *testing.T) {
	project := layer(t, kv{"execution.maxConcurrency", 1})
	for _, in := range []Inputs{
		{Project: project},
		{Project: project, ProjectTrusted: false, User: layer(t), CLI: layer(t)},
	} {
		got, err := Resolve(in)
		if !errors.Is(err, ErrUntrustedProject) {
			t.Fatalf("Resolve with an untrusted project: %v", err)
		}
		if !reflect.DeepEqual(got, Config{}) {
			t.Errorf("Resolve returned %+v with the error", got)
		}
	}
	// An untrusted project is refused even when it is invalid; nothing else is examined first.
	if _, err := Resolve(Inputs{Project: &Config{}}); !errors.Is(err, ErrUntrustedProject) {
		t.Errorf("an invalid untrusted project: %v", err)
	}
	// The same layer is applied once it is trusted.
	eff, err := Resolve(Inputs{Project: project, ProjectTrusted: true})
	if err != nil {
		t.Fatal(err)
	}
	if *eff.Execution.MaxConcurrency != 1 {
		t.Errorf("maxConcurrency = %d, want the trusted project's 1", *eff.Execution.MaxConcurrency)
	}
	// A trusted flag without a project is harmless.
	if _, err := Resolve(Inputs{ProjectTrusted: true}); err != nil {
		t.Errorf("Resolve(Inputs{ProjectTrusted: true}): %v", err)
	}
}

func TestCommandDefinedOnlyInAnUntrustedProjectCannotBeResolved(t *testing.T) {
	user := layer(t, kv{"verification.requiredChecks", []string{"unit-tests"}})
	project := layer(t, kv{"commands", map[string]Command{"unit-tests": {Argv: []string{"go", "test"}}}})

	// Untrusted: the whole project is refused, not silently dropped.
	if _, err := Resolve(Inputs{User: user, Project: project}); !errors.Is(err, ErrUntrustedProject) {
		t.Errorf("untrusted project: %v", err)
	}
	// Without the project the reference does not resolve.
	_, err := Resolve(Inputs{User: user})
	var se *strictjson.Error
	if !errors.As(err, &se) || se.Pointer != "/verification/requiredChecks/0" || !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("a reference without a command: %v", err)
	}
	// Trusted: it resolves.
	eff, err := Resolve(Inputs{User: user, Project: project, ProjectTrusted: true})
	if err != nil {
		t.Fatalf("trusted project: %v", err)
	}
	if err := eff.ValidateEffective(); err != nil {
		t.Error(err)
	}
}

func TestResolveRejectsInvalidLayers(t *testing.T) {
	bad := &Config{} // no schema version
	for name, in := range map[string]Inputs{
		"CLI":     {CLI: bad},
		"user":    {User: bad},
		"project": {Project: bad, ProjectTrusted: true},
	} {
		_, err := Resolve(in)
		if !errors.Is(err, ErrInvalidConfig) || !strings.Contains(err.Error(), name+" layer") {
			t.Errorf("%s: Resolve = %v", name, err)
		}
	}
	// The merge can combine layers that are valid on their own into one that
	// is not: the effective configuration is validated as a whole.
	user := layer(t, kv{"workspace.setup", []string{"deps"}})
	if _, err := Resolve(Inputs{User: user}); !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("a setup command without a definition: %v", err)
	}
}

func TestResolveOutputValidatesAndDecodesAgain(t *testing.T) {
	spec := mustDecode(t, string(specExample(t)))
	project := layer(t,
		kv{"execution.maxConcurrency", 2},
		kv{"verification.requiredChecks", []string{"deps"}},
		kv{"verification.protectedPaths", []string{"Makefile"}},
		kv{"agents.evaluator.subagents", "none"},
		kv{"commands", map[string]Command{"deps": {Argv: []string{"other"}}}},
	)
	cli := layer(t, kv{"execution.timeout", "30m"}, kv{"agents.planner.model", "cli-model"})
	for name, in := range map[string]Inputs{
		"defaults":     {},
		"SPEC user":    {User: &spec},
		"all layers":   {CLI: cli, User: &spec, Project: project},
		"project only": {Project: project},
	} {
		t.Run(name, func(t *testing.T) {
			in.ProjectTrusted = in.Project != nil
			eff, err := Resolve(in)
			if err != nil {
				t.Fatal(err)
			}
			if err := eff.ValidateEffective(); err != nil {
				t.Fatalf("ValidateEffective: %v", err)
			}
			if err := eff.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			data, err := strictjson.EncodeIndent(eff)
			if err != nil {
				t.Fatal(err)
			}
			again, err := Decode(data)
			if err != nil {
				t.Fatalf("Decode of the effective configuration: %v\n%s", err, data)
			}
			data2, err := strictjson.EncodeIndent(again)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != string(data2) {
				t.Errorf("the effective configuration changed in a round trip:\n%s\n%s", data, data2)
			}
			if _, err := eff.Limits(); err != nil {
				t.Errorf("Limits: %v", err)
			}
		})
	}
}

func TestResolveIsDeterministicAndStartsFromTheDefaults(t *testing.T) {
	enc := func(c Config) string {
		data, err := strictjson.EncodeIndent(c)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	if got, want := enc(resolve(t, Inputs{})), enc(Defaults()); got != want {
		t.Errorf("Resolve(Inputs{}) differs from Defaults:\n%s\n%s", got, want)
	}
	spec := mustDecode(t, string(specExample(t)))
	first := enc(resolve(t, Inputs{User: &spec, Project: layer(t, kv{"verification.protectedPaths", []string{"a"}})}))
	for range 5 {
		if again := enc(resolve(t, Inputs{User: &spec, Project: layer(t, kv{"verification.protectedPaths", []string{"a"}})})); again != first {
			t.Fatalf("Resolve is not deterministic:\n%s\n%s", first, again)
		}
	}
	// The built-in defaults have non-nil lists, so that the frozen policy has
	// the same bytes whether a union was computed or defaulted.
	if got := resolve(t, Inputs{}); got.Verification.RequiredChecks == nil || got.Workspace.Setup == nil || got.Verification.ProtectedPaths == nil {
		t.Errorf("a defaulted list is nil: %+v", got)
	}
}

func TestResolveSharesNoMemoryWithItsInputs(t *testing.T) {
	spec := mustDecode(t, string(specExample(t)))
	spec.Agents.Implementer.AllowedCommands = []string{"deps"}
	project := layer(t,
		kv{"verification.protectedPaths", []string{"a"}},
		kv{"commands", map[string]Command{"extra": {Argv: []string{"x", "y"}}}},
	)
	cli := layer(t, kv{"agents.planner.effort", &Effort{Mode: "fixed", Value: "low", Allowed: []string{"low"}}})
	before := [3]Config{spec.Clone(), project.Clone(), cli.Clone()}

	eff := resolve(t, Inputs{CLI: cli, User: &spec, Project: project})
	// Change everything reachable from the result.
	*eff.Execution.MaxConcurrency = 77
	eff.Agents.Coordinator.Args[0] = "changed"
	eff.Agents.Implementer.Effort.Allowed[0] = "changed"
	eff.Agents.Implementer.AllowedCommands[0] = "changed"
	eff.Agents.Planner.Effort.Allowed[0] = "changed"
	eff.Commands["lint"].Argv[0] = "changed"
	eff.Commands["extra"].Argv[0] = "changed"
	eff.Verification.ProtectedPaths[0] = "changed"
	eff.Verification.RequiredChecks[0] = "changed"
	eff.Workspace.Setup[0] = "changed"

	after := [3]Config{spec, *project, *cli}
	for i, name := range []string{"user", "project", "CLI"} {
		if !reflect.DeepEqual(before[i], after[i]) {
			t.Errorf("changing the effective configuration changed the %s layer:\n%+v\n%+v", name, before[i], after[i])
		}
	}
}

func TestSchemaVersionOfTheResult(t *testing.T) {
	if got := resolve(t, Inputs{User: layer(t)}).SchemaVersion; got != SchemaVersion {
		t.Errorf("schemaVersion = %d", got)
	}
}

func TestMixedLayers(t *testing.T) {
	user := layer(t,
		kv{"execution.maxConcurrency", 4}, kv{"execution.timeout", "3h"},
		kv{"execution.requirePlanApproval", false}, kv{"verification.independentReview", "final"},
		kv{"workspace.requireCleanRepository", false},
	)
	project := layer(t,
		kv{"execution.maxConcurrency", 2}, kv{"execution.timeout", "5h"},
		kv{"execution.requirePlanApproval", true}, kv{"verification.independentReview", "phase"},
		kv{"workspace.requireCleanRepository", true}, kv{"execution.maxReplans", 1},
	)
	cli := layer(t, kv{"execution.timeout", "10h"}, kv{"workspace.requireCleanRepository", false})
	eff := resolve(t, Inputs{CLI: cli, User: user, Project: project})
	e := eff.Execution
	switch {
	case *e.MaxConcurrency != 2: // the stricter of 4 and 2
		t.Errorf("maxConcurrency = %d", *e.MaxConcurrency)
	case e.Timeout != "10h": // the CLI loosens both 3h and 5h
		t.Errorf("timeout = %s", e.Timeout)
	case !*e.RequirePlanApproval: // true if either sets true
		t.Error("requirePlanApproval is false")
	case *e.MaxReplans != 1: // only the project sets it
		t.Errorf("maxReplans = %d", *e.MaxReplans)
	case eff.Verification.IndependentReview != "phase":
		t.Errorf("independentReview = %s", eff.Verification.IndependentReview)
	case *eff.Workspace.RequireCleanRepository: // the CLI loosens true to false
		t.Error("requireCleanRepository is true")
	}
	if got := fmt.Sprint(*e.MaxPhaseRetries); got != "3" {
		t.Errorf("maxPhaseRetries = %s, want the default 3", got)
	}
}

// TestStrictRaisesTheReviewMinimum is R-CFG-01 with SPEC §14 and §10: Strict is
// Phased with `phase` as the minimum of verification.independentReview. The
// minimum is applied to the merged configuration, so it holds whichever layer
// sets the strategy and whichever layer sets the weaker review, a CLI value
// included. The other strategies have no minimum.
func TestStrictRaisesTheReviewMinimum(t *testing.T) {
	const unset = "(unset)"
	// place puts the strategy in the layer strategyAt and the review in the
	// layer reviewAt (unset: no layer sets it).
	place := func(strategyAt, strategy, reviewAt, review string) Inputs {
		layers := map[string]*Config{"CLI": layer(t), "user": layer(t), "project": layer(t)}
		set(t, layers[strategyAt], "execution.strategy", strategy)
		if reviewAt != unset {
			set(t, layers[reviewAt], "verification.independentReview", review)
		}
		return Inputs{CLI: layers["CLI"], User: layers["user"], Project: layers["project"]}
	}
	places := []string{"CLI", "user", "project"}
	reviews := []string{"none", "final", "phase"}

	for _, strategyAt := range places {
		for _, reviewAt := range append(slices.Clone(places), unset) {
			for _, review := range reviews {
				if reviewAt == unset && review != reviews[0] {
					continue // one case: there is no value to vary
				}
				name := fmt.Sprintf("strict from the %s layer, review %s from the %s layer", strategyAt, review, reviewAt)
				if reviewAt == unset {
					name = fmt.Sprintf("strict from the %s layer, no layer sets the review", strategyAt)
				}
				t.Run(name, func(t *testing.T) {
					// Strict: none and final are raised to phase, phase is kept, and
					// the built-in default (final) is raised as well.
					if got := resolve(t, place(strategyAt, "strict", reviewAt, review)).Verification.IndependentReview; got != "phase" {
						t.Errorf("strict: independentReview = %s, want phase", got)
					}
					// Any other strategy leaves the value that the merge gave.
					want := review
					if reviewAt == unset {
						want = leaf(t, Defaults(), "verification.independentReview").(string)
					}
					for _, strategy := range []string{"standard", "phased"} {
						if got := resolve(t, place(strategyAt, strategy, reviewAt, review)).Verification.IndependentReview; got != want {
							t.Errorf("%s: independentReview = %s, want %s", strategy, got, want)
						}
					}
				})
			}
		}
	}

	t.Run("the minimum applies to the merged value", func(t *testing.T) {
		for _, tt := range []struct {
			name string
			in   Inputs
			want string
		}{
			{"the user's none and the project's final merge to final, which is raised",
				Inputs{User: layer(t, kv{"execution.strategy", "strict"}, kv{"verification.independentReview", "none"}),
					Project: layer(t, kv{"verification.independentReview", "final"})}, "phase"},
			{"a CLI none beats a stricter user review, and Strict raises it again",
				Inputs{CLI: layer(t, kv{"verification.independentReview", "none"}),
					User: layer(t, kv{"execution.strategy", "strict"}, kv{"verification.independentReview", "phase"})}, "phase"},
			{"the strategy that decides is the effective one: a CLI phased beats a project strict",
				Inputs{CLI: layer(t, kv{"execution.strategy", "phased"}, kv{"verification.independentReview", "none"}),
					Project: layer(t, kv{"execution.strategy", "strict"})}, "none"},
			{"the user's strict beats the project's standard",
				Inputs{User: layer(t, kv{"execution.strategy", "strict"}, kv{"verification.independentReview", "final"}),
					Project: layer(t, kv{"execution.strategy", "standard"})}, "phase"},
			{"no layer at all is Phased with the final review",
				Inputs{}, "final"},
		} {
			if got := resolve(t, tt.in).Verification.IndependentReview; got != tt.want {
				t.Errorf("%s: independentReview = %s, want %s", tt.name, got, tt.want)
			}
		}
	})

	t.Run("nothing but the review changes", func(t *testing.T) {
		enc := func(c Config) string {
			data, err := strictjson.EncodeIndent(c)
			if err != nil {
				t.Fatal(err)
			}
			return string(data)
		}
		with := func(review string) Inputs {
			return Inputs{
				User:    layer(t, kv{"execution.strategy", "strict"}, kv{"verification.independentReview", review}, kv{"execution.timeout", "45m"}),
				Project: layer(t, kv{"execution.maxReplans", 1}, kv{"verification.protectedPaths", []string{"Makefile"}}),
			}
		}
		if got, want := enc(resolve(t, with("none"))), enc(resolve(t, with("phase"))); got != want {
			t.Errorf("raising the review changed more than the review:\n%s\n%s", got, want)
		}
	})
}
