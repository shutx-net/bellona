package config

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/shutx-net/bellona/internal/domain"
)

// ErrUntrustedProject is returned by Resolve when a project layer is given
// that is not trusted. The settings of an untrusted project are never applied
// and never silently dropped.
var ErrUntrustedProject = errors.New("the project configuration is not trusted")

// Inputs are the configuration layers that Resolve merges. The caller decodes
// the layers (see Decode) and decides which project configuration is trusted;
// where the files live and how a project becomes trusted are decided with the
// CLI (M1b). A nil layer is absent. There is no input for proposals made by an
// LLM: they are validated against the frozen policy (M2), never merged.
type Inputs struct {
	// CLI holds the settings given explicitly on the command line.
	CLI *Config
	// User holds the user's settings.
	User *Config
	// Project holds the settings of the project, and is applied only when
	// ProjectTrusted is true.
	Project *Config
	// ProjectTrusted states that the caller trusts Project.
	ProjectTrusted bool
}

// kind says how Resolve combines the values that the layers set for one
// field.
type kind int

// The kinds of field policy. ordinary and perName fields follow the plain
// precedence of SPEC §14. The other kinds are restrictions (see
// kind.restriction).
const (
	// ordinary: the highest-precedence layer that sets the field wins. A
	// list and an object (effort) count as one value.
	ordinary kind = iota
	// perName: like ordinary, for each name of a map (commands).
	perName
	// stricterMin: the smaller number is the stricter value.
	stricterMin
	// stricterShortest: the shorter duration is the stricter value.
	stricterShortest
	// stricterTrue: true is the stricter value.
	stricterTrue
	// stricterReview: phase is stricter than final, final than none.
	stricterReview
	// stricterSubagents: none is stricter than readOnly.
	stricterSubagents
	// union: the stricter value is the union of the lists, in order of first
	// appearance.
	union
)

var kindNames = [...]string{
	ordinary: "ordinary", perName: "per-name", stricterMin: "stricter-min",
	stricterShortest: "stricter-shortest", stricterTrue: "stricter-true",
	stricterReview: "stricter-review", stricterSubagents: "stricter-subagents", union: "union",
}

// String returns the name of the kind.
func (k kind) String() string {
	if k < 0 || int(k) >= len(kindNames) {
		return fmt.Sprintf("kind(%d)", int(k))
	}
	return kindNames[k]
}

// restriction reports whether the kind belongs to a restriction field: an
// operation restriction or a budget ceiling.
func (k kind) restriction() bool { return k >= stricterMin }

// policy is the merge policy of one field: the dotted JSON path of the field
// and the kind of comparison.
type policy struct {
	Path string
	Kind kind
}

// rolePlaceholder stands for each of the four roles in a path of
// policyTemplate.
const rolePlaceholder = "<role>"

// policyTemplate is the one table of per-field policy, used by Resolve and by
// its tests. It lists every setting of Config: a test checks that no field of
// Config is missing and that every path resolves.
//
// Merge rule (decided by the user; SPEC §14): an explicit CLI value always
// wins. For an ordinary or perName field the highest-precedence layer that
// sets it wins: CLI, then user, then trusted project, then the built-in
// default. For a restriction field the CLI value is the effective value even
// when it is looser than the other layers; otherwise the effective value is
// the stricter of the user and trusted-project values that are set, and one
// set value is used as it is. The built-in default applies only when no layer
// sets the field. A default is never compared with a layer.
//
// The table holds the merge only. A strategy's minimum (the review of Strict,
// SPEC §10 and R-SCH-02) is a separate rule that Resolve applies to the merged
// result; see applyStrategyMinimums.
var policyTemplate = []policy{
	{"schemaVersion", ordinary},

	{"agents." + rolePlaceholder + ".runtime", ordinary},
	{"agents." + rolePlaceholder + ".model", ordinary},
	{"agents." + rolePlaceholder + ".effort", ordinary},
	{"agents." + rolePlaceholder + ".hostCustomization", ordinary},
	{"agents." + rolePlaceholder + ".args", ordinary},
	{"agents." + rolePlaceholder + ".env", ordinary},
	{"agents." + rolePlaceholder + ".subagents", stricterSubagents},
	{"agents." + rolePlaceholder + ".scopeEnforcement", ordinary},
	{"agents." + rolePlaceholder + ".allowedCommands", ordinary},

	{"execution.strategy", ordinary},
	{"execution.maxConcurrency", stricterMin},
	{"execution.maxPhaseRetries", stricterMin},
	{"execution.maxReplans", stricterMin},
	{"execution.maxAgentSteps", stricterMin},
	{"execution.maxTurnCostUsd", stricterMin},
	{"execution.timeout", stricterShortest},
	{"execution.requirePlanApproval", stricterTrue},

	{"commands", perName},

	{"verification.independentReview", stricterReview},
	{"verification.requiredChecks", union},
	{"verification.protectedPaths", union},

	{"workspace.requireCleanRepository", stricterTrue},
	{"workspace.setup", ordinary},
	{"workspace.root", ordinary},
}

// policies is policyTemplate with the role placeholder expanded for each
// role, in the order of domain.Roles.
var policies = expandRoles(policyTemplate)

func expandRoles(template []policy) []policy {
	var out []policy
	for _, p := range template {
		if !strings.Contains(p.Path, rolePlaceholder) {
			out = append(out, p)
			continue
		}
		for _, r := range domain.Roles() {
			out = append(out, policy{strings.Replace(p.Path, rolePlaceholder, string(r), 1), p.Kind})
		}
	}
	return out
}

// Resolve merges the layers into the effective configuration that a Run
// freezes. A project layer that is not trusted is refused with
// ErrUntrustedProject before anything else happens. Every layer given must
// pass Validate. The result passes ValidateEffective, so every command that
// the configuration refers to is defined, and it shares no memory with the
// inputs. See policyTemplate for the merge rule.
func Resolve(in Inputs) (Config, error) {
	if in.Project != nil && !in.ProjectTrusted {
		return Config{}, ErrUntrustedProject
	}
	for _, l := range []struct {
		name string
		cfg  *Config
	}{{"CLI", in.CLI}, {"user", in.User}, {"project", in.Project}} {
		if l.cfg == nil {
			continue
		}
		if err := l.cfg.Validate(); err != nil {
			return Config{}, fmt.Errorf("%s layer: %w", l.name, err)
		}
	}
	out := Defaults()
	for _, p := range policies {
		if err := p.apply(&out, in); err != nil {
			return Config{}, fmt.Errorf("%w: merge %s: %w", ErrInvalidConfig, p.Path, err)
		}
	}
	applyStrategyMinimums(&out)
	if err := out.ValidateEffective(); err != nil {
		return Config{}, err
	}
	return out, nil
}

// applyStrategyMinimums raises the settings of the merged configuration that
// the effective execution strategy fixes at a minimum (SPEC §10, R-SCH-02):
// Strict is Phased with `phase` as the minimum of verification.independentReview,
// which configuration cannot lower. It runs after the layers are merged, so it
// holds whichever layer, the CLI included, set the weaker value, and it keeps a
// stronger value. The other strategies have no minimum.
func applyStrategyMinimums(c *Config) {
	if c.Execution.Strategy == strategyStrict && reviewRank(c.Verification.IndependentReview) < reviewRank(reviewPhase) {
		c.Verification.IndependentReview = reviewPhase
	}
}

// apply merges the field of p from the layers into dst.
func (p policy) apply(dst *Config, in Inputs) error {
	// The layers in precedence order: CLI, user, trusted project.
	var set [3]reflect.Value
	for i, l := range [3]*Config{in.CLI, in.User, in.Project} {
		if l == nil {
			continue
		}
		if v, ok := lookup(reflect.ValueOf(l).Elem(), p.Path, false); ok && isSet(v) {
			set[i] = v
		}
	}
	cli, user, project := set[0], set[1], set[2]

	var result reflect.Value
	switch {
	case p.Kind == perName:
		result = mergeNames(set[:])
	case cli.IsValid():
		// An explicit CLI value always wins, for every kind.
		result = cli
	case !p.Kind.restriction():
		for _, v := range set[1:] {
			if v.IsValid() {
				result = v
				break
			}
		}
	case user.IsValid() && project.IsValid():
		var err error
		if result, err = stricter(p.Kind, user, project); err != nil {
			return err
		}
	case user.IsValid():
		result = user
	case project.IsValid():
		result = project
	}
	if !result.IsValid() {
		return nil // no layer sets the field: the built-in default stays
	}
	target, ok := lookup(reflect.ValueOf(dst).Elem(), p.Path, true)
	if !ok {
		return fmt.Errorf("no field for %q", p.Path)
	}
	target.Set(deepCopy(result))
	return nil
}

// mergeNames merges the maps of the layers (in precedence order) name by
// name: for each name the highest-precedence layer that defines it wins. The
// result is the zero Value when no layer sets the map.
func mergeNames(layers []reflect.Value) reflect.Value {
	var merged reflect.Value
	for _, m := range layers {
		if !m.IsValid() {
			continue
		}
		if !merged.IsValid() {
			merged = reflect.MakeMap(m.Type())
		}
		for it := m.MapRange(); it.Next(); {
			if !merged.MapIndex(it.Key()).IsValid() {
				merged.SetMapIndex(it.Key(), it.Value())
			}
		}
	}
	return merged
}

// stricter returns the stricter of two set values of a restriction field of
// kind k. On a tie it returns a.
func stricter(k kind, a, b reflect.Value) (reflect.Value, error) {
	switch k {
	case stricterMin:
		x, y := a.Elem(), b.Elem()
		if x.CanInt() && y.Int() < x.Int() || x.CanFloat() && y.Float() < x.Float() {
			return b, nil
		}
	case stricterShortest:
		x, err := time.ParseDuration(a.String())
		if err != nil {
			return reflect.Value{}, err
		}
		y, err := time.ParseDuration(b.String())
		if err != nil {
			return reflect.Value{}, err
		}
		if y < x {
			return b, nil
		}
	case stricterTrue:
		if !a.Elem().Bool() && b.Elem().Bool() {
			return b, nil
		}
	case stricterReview:
		if reviewRank(b.String()) > reviewRank(a.String()) {
			return b, nil
		}
	case stricterSubagents:
		if a.String() != subagentsNone && b.String() == subagentsNone {
			return b, nil
		}
	case union:
		return reflect.ValueOf(unionOf(a.Interface().([]string), b.Interface().([]string))), nil
	default:
		return reflect.Value{}, fmt.Errorf("%s is not a restriction", k)
	}
	return a, nil
}

// reviewRank orders the values of verification.independentReview: a higher
// rank is stricter.
func reviewRank(s string) int {
	switch s {
	case reviewPhase:
		return 2
	case reviewFinal:
		return 1
	}
	return 0
}

// unionOf returns the entries of a followed by the entries of b that a does
// not contain, without duplicates, as a new non-nil slice.
func unionOf(a, b []string) []string {
	out := make([]string, 0, len(a)+len(b))
	for _, s := range slices.Concat(a, b) {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// lookup returns the field of the struct value v that the dotted JSON path
// names. When a pointer on the way is nil, lookup allocates it if alloc is
// true (v must then be addressable) and reports false otherwise.
func lookup(v reflect.Value, path string, alloc bool) (reflect.Value, bool) {
	for name := range strings.SplitSeq(path, ".") {
		if v.Kind() == reflect.Pointer {
			if v.IsNil() {
				if !alloc {
					return reflect.Value{}, false
				}
				v.Set(reflect.New(v.Type().Elem()))
			}
			v = v.Elem()
		}
		if v.Kind() != reflect.Struct {
			return reflect.Value{}, false
		}
		f, ok := fieldByJSONName(v, name)
		if !ok {
			return reflect.Value{}, false
		}
		v = f
	}
	return v, true
}

// fieldByJSONName returns the field of the struct value v whose JSON name is
// name.
func fieldByJSONName(v reflect.Value, name string) (reflect.Value, bool) {
	t := v.Type()
	for i := range t.NumField() {
		if jsonName(t.Field(i)) == name {
			return v.Field(i), true
		}
	}
	return reflect.Value{}, false
}

func jsonName(f reflect.StructField) string {
	name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
	return name
}

// isSet reports whether a layer sets the field v: a pointer, slice or map is
// set when it is not nil, a string when it is not empty, a number when it is
// not zero.
func isSet(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Map:
		return !v.IsNil()
	case reflect.String:
		return v.Len() > 0
	case reflect.Int:
		return v.Int() != 0
	}
	return false
}
