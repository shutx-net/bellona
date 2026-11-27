// Package config decodes and validates the user-managed configuration of
// Bellona, bellona.json (SPEC §14), and merges the configuration layers into
// the effective configuration that a Run freezes (R-CFG-01).
//
// One Go type, Config, serves as a layer and as the effective configuration.
// A layer is a partial document: every member except schemaVersion is
// optional, and a member that a layer leaves out is a setting that this layer
// does not make. Scalars whose zero value is meaningful are pointers, and a
// list is set when it is not nil, so an explicit empty list in a layer
// differs from an absent one. A string member is set when it is not empty;
// Decode rejects a string member that is present but empty, because an empty
// string could not be told from an absent member.
//
// Decode is strict (R-ART-01): it rejects unknown members, duplicate members,
// trailing values, nulls and every invalid value. Resolve merges decoded
// layers by the precedence of SPEC §14; see merge.go for the rule and for the
// table that holds the policy of every field.
package config

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/shutx-net/bellona/internal/domain"
	"github.com/shutx-net/bellona/internal/strictjson"
)

// SchemaVersion is the schema version of bellona.json and of the effective
// configuration that a Run freezes in policy.json.
const SchemaVersion = 1

// ErrInvalidConfig is wrapped by every error about the content of a
// configuration: a decode error, a validation error and a failed merge.
var ErrInvalidConfig = errors.New("invalid configuration")

// Config is one configuration layer, or the effective configuration. Its
// JSON form is the document of SPEC §14. The JSON names are the keys that the
// SPEC lists.
type Config struct {
	SchemaVersion int                `json:"schemaVersion"`
	Agents        Agents             `json:"agents,omitzero"`
	Execution     Execution          `json:"execution,omitzero"`
	Commands      map[string]Command `json:"commands,omitzero"`
	Verification  Verification       `json:"verification,omitzero"`
	Workspace     Workspace          `json:"workspace,omitzero"`
}

// Agents holds the settings of the four roles. A role that is nil has no
// settings in this layer.
type Agents struct {
	Coordinator *Agent `json:"coordinator,omitzero"`
	Planner     *Agent `json:"planner,omitzero"`
	Implementer *Agent `json:"implementer,omitzero"`
	Evaluator   *Agent `json:"evaluator,omitzero"`
}

// byRole returns the settings of role r, or nil.
func (a Agents) byRole(r domain.Role) *Agent {
	switch r {
	case domain.RoleCoordinator:
		return a.Coordinator
	case domain.RolePlanner:
		return a.Planner
	case domain.RoleImplementer:
		return a.Implementer
	case domain.RoleEvaluator:
		return a.Evaluator
	}
	return nil
}

// Agent holds the settings of one role (SPEC §14). Runtime names an adapter;
// it is checked against the adapters that exist only when a Run starts (M1a).
// Args are extra command-line arguments for the harness, in the order given.
// Env names additional environment variables to pass to the agent process.
// Subagents, ScopeEnforcement and AllowedCommands are described in SPEC §11.1
// and §14; the last two exist for the Implementer only.
type Agent struct {
	Runtime           string   `json:"runtime,omitzero"`
	Model             string   `json:"model,omitzero"`
	Effort            *Effort  `json:"effort,omitzero"`
	HostCustomization string   `json:"hostCustomization,omitzero"`
	Args              []string `json:"args,omitzero"`
	Env               []string `json:"env,omitzero"`
	Subagents         string   `json:"subagents,omitzero"`
	ScopeEnforcement  string   `json:"scopeEnforcement,omitzero"`
	AllowedCommands   []string `json:"allowedCommands,omitzero"`
}

// Effort states how the reasoning effort of a role is chosen (SPEC §7). A
// fixed effort has a Value. An adaptive effort has DecidedBy, the role that
// chooses it, and may restrict the choice with Allowed. A layer sets Effort as
// a whole object.
type Effort struct {
	Mode      string   `json:"mode"`
	Value     string   `json:"value,omitzero"`
	DecidedBy string   `json:"decidedBy,omitzero"`
	Allowed   []string `json:"allowed,omitzero"`
}

// Execution holds the execution strategy and the execution limits (SPEC §10,
// §14.2). MaxAgentSteps and MaxTurnCostUSD cap one Turn where the host
// supports it. Timeout is a Go duration such as "2h".
type Execution struct {
	Strategy            string   `json:"strategy,omitzero"`
	MaxConcurrency      *int     `json:"maxConcurrency,omitzero"`
	MaxPhaseRetries     *int     `json:"maxPhaseRetries,omitzero"`
	MaxReplans          *int     `json:"maxReplans,omitzero"`
	MaxAgentSteps       *int     `json:"maxAgentSteps,omitzero"`
	MaxTurnCostUSD      *float64 `json:"maxTurnCostUsd,omitzero"`
	Timeout             string   `json:"timeout,omitzero"`
	RequirePlanApproval *bool    `json:"requirePlanApproval,omitzero"`
}

// Command is a named command: an argument array, never a shell string.
type Command struct {
	Argv []string `json:"argv"`
}

// Verification holds the verification settings (SPEC §9). RequiredChecks
// names commands. ProtectedPaths are path patterns; the full pattern grammar
// arrives with M1b.
type Verification struct {
	IndependentReview string   `json:"independentReview,omitzero"`
	RequiredChecks    []string `json:"requiredChecks,omitzero"`
	ProtectedPaths    []string `json:"protectedPaths,omitzero"`
}

// Workspace holds the workspace settings (SPEC §13). Setup names commands.
// Root is the absolute directory under which worktrees are created.
type Workspace struct {
	RequireCleanRepository *bool    `json:"requireCleanRepository,omitzero"`
	Setup                  []string `json:"setup,omitzero"`
	Root                   string   `json:"root,omitzero"`
}

// The settings that SPEC §14 lists, as accepted by Validate.
const (
	strategyStandard = "standard"
	strategyPhased   = "phased"
	strategyStrict   = "strict"

	reviewNone  = "none"
	reviewFinal = "final"
	reviewPhase = "phase"

	effortFixed    = "fixed"
	effortAdaptive = "adaptive"

	customizationInherit  = "inherit"
	customizationIsolated = "isolated"

	subagentsNone     = "none"
	subagentsReadOnly = "readOnly"

	scopeDetect = "detect"
	scopeHost   = "host"
)

var (
	runtimePattern     = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	effortTokenPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
	envNamePattern     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	commandNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
)

// valueError is the Err of every validation error: it matches
// ErrInvalidConfig and strictjson.ErrInvalidValue, while its text stays the
// text of ErrInvalidConfig.
type valueError struct{}

func (valueError) Error() string   { return ErrInvalidConfig.Error() }
func (valueError) Unwrap() []error { return []error{ErrInvalidConfig, strictjson.ErrInvalidValue} }

// invalid returns a validation error for the member at the given JSON
// Pointer.
func invalid(pointer, format string, args ...any) error {
	return &strictjson.Error{Pointer: pointer, Err: valueError{}, Detail: fmt.Sprintf(format, args...)}
}

// pointer builds an RFC 6901 JSON Pointer from reference tokens.
func pointer(tokens ...string) string {
	var b strings.Builder
	for _, t := range tokens {
		b.WriteByte('/')
		b.WriteString(strings.NewReplacer("~", "~0", "/", "~1").Replace(t))
	}
	return b.String()
}

// Decode strictly decodes and validates one configuration layer, for example
// the contents of a bellona.json. Every error wraps ErrInvalidConfig and,
// where the error concerns one member, is a *strictjson.Error that carries
// the JSON Pointer of that member. A string member that is present but empty
// is rejected, because it could not be told from an absent member.
func Decode(data []byte) (Config, error) {
	cfg, err := strictjson.Decode[Config](data)
	if err == nil {
		err = rejectEmptyStrings(data)
	}
	if err != nil {
		if !errors.Is(err, ErrInvalidConfig) {
			err = fmt.Errorf("%w: %w", ErrInvalidConfig, err)
		}
		return Config{}, err
	}
	return cfg, nil
}

// rejectEmptyStrings reports the first object member whose value is the empty
// string. Elements of arrays are not examined: Validate judges them, and an
// empty element is a real element, for example an empty argument in argv.
func rejectEmptyStrings(data []byte) error {
	doc, err := strictjson.Decode[any](data)
	if err != nil {
		return err
	}
	return walkEmpty(doc, nil)
}

func walkEmpty(v any, path []string) error {
	obj, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	for _, name := range slices.Sorted(maps.Keys(obj)) {
		at := append(slices.Clone(path), name)
		if s, ok := obj[name].(string); ok && s == "" {
			return invalid(pointer(at...), "the member is an empty string; leave it out to leave the setting unset")
		}
		if err := walkEmpty(obj[name], at); err != nil {
			return err
		}
	}
	return nil
}

// Validate checks one configuration layer: every value that is set must be
// valid. A member that is not set is not checked, and nothing is required
// besides the schema version, so a partial layer is valid. It does not check
// that the commands that lists name are defined; ValidateEffective does. The
// first violation is returned as a *strictjson.Error that names the member by
// its JSON Pointer and wraps ErrInvalidConfig and strictjson.ErrInvalidValue.
func (c Config) Validate() error {
	if c.SchemaVersion != SchemaVersion {
		return invalid(pointer("schemaVersion"), "schema version %d is not supported, want %d", c.SchemaVersion, SchemaVersion)
	}
	for _, r := range domain.Roles() {
		if a := c.Agents.byRole(r); a != nil {
			if err := a.validate(r, "agents", string(r)); err != nil {
				return err
			}
		}
	}
	if err := c.Execution.validate(); err != nil {
		return err
	}
	for _, name := range slices.Sorted(maps.Keys(c.Commands)) {
		if err := c.Commands[name].validate(name); err != nil {
			return err
		}
	}
	if err := c.Verification.validate(); err != nil {
		return err
	}
	return c.Workspace.validate()
}

func (a Agent) validate(role domain.Role, path ...string) error {
	at := func(member string, more ...string) string {
		return pointer(append(append(slices.Clone(path), member), more...)...)
	}
	if a.Runtime != "" && !runtimePattern.MatchString(a.Runtime) {
		return invalid(at("runtime"), "runtime %q does not match %s", a.Runtime, runtimePattern)
	}
	if a.Model != "" && strings.TrimSpace(a.Model) == "" {
		return invalid(at("model"), "the model name is blank")
	}
	if a.Effort != nil {
		if err := a.Effort.validate(role, at("effort")); err != nil {
			return err
		}
	}
	switch a.HostCustomization {
	case "", customizationInherit, customizationIsolated:
	default:
		return invalid(at("hostCustomization"), "%q is not %s or %s", a.HostCustomization, customizationInherit, customizationIsolated)
	}
	for i, arg := range a.Args {
		if arg == "" {
			return invalid(at("args", strconv.Itoa(i)), "an argument is empty")
		}
	}
	seen := make(map[string]bool)
	for i, name := range a.Env {
		switch {
		case !envNamePattern.MatchString(name):
			return invalid(at("env", strconv.Itoa(i)), "%q is not an environment variable name (%s)", name, envNamePattern)
		case seen[name]:
			return invalid(at("env", strconv.Itoa(i)), "environment variable %q is listed twice", name)
		}
		seen[name] = true
	}
	switch a.Subagents {
	case "", subagentsNone:
	case subagentsReadOnly:
		if role == domain.RoleImplementer || role == domain.RoleCoordinator {
			return invalid(at("subagents"), "the %s may not delegate to subagents: %s is only for the planner and the evaluator", role, subagentsReadOnly)
		}
	default:
		return invalid(at("subagents"), "%q is not %s or %s", a.Subagents, subagentsNone, subagentsReadOnly)
	}
	if a.ScopeEnforcement != "" && role != domain.RoleImplementer {
		return invalid(at("scopeEnforcement"), "scopeEnforcement exists for the implementer only")
	}
	switch a.ScopeEnforcement {
	case "", scopeDetect, scopeHost:
	default:
		return invalid(at("scopeEnforcement"), "%q is not %s or %s", a.ScopeEnforcement, scopeDetect, scopeHost)
	}
	if a.AllowedCommands != nil && role != domain.RoleImplementer {
		return invalid(at("allowedCommands"), "allowedCommands exists for the implementer only")
	}
	return validateNames(a.AllowedCommands, pointer(append(slices.Clone(path), "allowedCommands")...))
}

// deciders names the role that chooses the adaptive effort of a role
// (SPEC §7). The coordinator has none: its effort cannot be adaptive.
var deciders = map[domain.Role]domain.Role{
	domain.RolePlanner:     domain.RoleCoordinator,
	domain.RoleImplementer: domain.RolePlanner,
	domain.RoleEvaluator:   domain.RolePlanner,
}

func (e Effort) validate(role domain.Role, at string) error {
	switch e.Mode {
	case effortFixed:
		if e.Value == "" {
			return invalid(at+"/value", "a fixed effort needs a value")
		}
		if e.DecidedBy != "" {
			return invalid(at+"/decidedBy", "a fixed effort is not decided by a role")
		}
	case effortAdaptive:
		decider, ok := deciders[role]
		if !ok {
			return invalid(at+"/mode", "the effort of the %s cannot be adaptive: no role decides it", role)
		}
		if e.DecidedBy == "" {
			return invalid(at+"/decidedBy", "an adaptive effort needs decidedBy")
		}
		if e.DecidedBy != string(decider) {
			return invalid(at+"/decidedBy", "the effort of the %s is decided by the %s, not by %q", role, decider, e.DecidedBy)
		}
		if e.Value != "" {
			return invalid(at+"/value", "an adaptive effort has no fixed value")
		}
	case "":
		return invalid(at+"/mode", "the mode is required: %s or %s", effortFixed, effortAdaptive)
	default:
		return invalid(at+"/mode", "%q is not %s or %s", e.Mode, effortFixed, effortAdaptive)
	}
	if e.Value != "" && !effortTokenPattern.MatchString(e.Value) {
		return invalid(at+"/value", "%q does not match %s", e.Value, effortTokenPattern)
	}
	if e.Allowed == nil {
		return nil
	}
	if len(e.Allowed) == 0 {
		return invalid(at+"/allowed", "the list of allowed values is empty")
	}
	seen := make(map[string]bool)
	for i, v := range e.Allowed {
		switch {
		case !effortTokenPattern.MatchString(v):
			return invalid(at+"/allowed/"+strconv.Itoa(i), "%q does not match %s", v, effortTokenPattern)
		case seen[v]:
			return invalid(at+"/allowed/"+strconv.Itoa(i), "%q is listed twice", v)
		}
		seen[v] = true
	}
	if e.Value != "" && !seen[e.Value] {
		return invalid(at+"/allowed", "the fixed value %q is not in the allowed values", e.Value)
	}
	return nil
}

func (e Execution) validate() error {
	at := func(member string) string { return pointer("execution", member) }
	switch e.Strategy {
	case "", strategyStandard, strategyPhased, strategyStrict:
	case "direct":
		return invalid(at("strategy"), "the direct strategy is not available before M7")
	default:
		return invalid(at("strategy"), "%q is not %s, %s or %s", e.Strategy, strategyStandard, strategyPhased, strategyStrict)
	}
	if v := e.MaxConcurrency; v != nil && *v < 1 {
		return invalid(at("maxConcurrency"), "maxConcurrency is %d, want 1 or more", *v)
	}
	if v := e.MaxPhaseRetries; v != nil && *v < 0 {
		return invalid(at("maxPhaseRetries"), "maxPhaseRetries is %d, want 0 or more", *v)
	}
	if v := e.MaxReplans; v != nil && *v < 0 {
		return invalid(at("maxReplans"), "maxReplans is %d, want 0 or more", *v)
	}
	if v := e.MaxAgentSteps; v != nil && *v < 1 {
		return invalid(at("maxAgentSteps"), "maxAgentSteps is %d, want 1 or more", *v)
	}
	if v := e.MaxTurnCostUSD; v != nil && (math.IsNaN(*v) || math.IsInf(*v, 0) || *v <= 0) {
		return invalid(at("maxTurnCostUsd"), "maxTurnCostUsd is %v, want a finite number above 0", *v)
	}
	if e.Timeout != "" {
		if _, err := parseTimeout(e.Timeout); err != nil {
			return invalid(at("timeout"), "%v", err)
		}
	}
	return nil
}

// parseTimeout parses a positive Go duration.
func parseTimeout(s string) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	switch {
	case err != nil:
		return 0, fmt.Errorf("timeout %q is not a duration such as \"2h\" or \"90m\": %w", s, err)
	case d <= 0:
		return 0, fmt.Errorf("timeout %q is not positive", s)
	}
	return d, nil
}

func (c Command) validate(name string) error {
	if !commandNamePattern.MatchString(name) {
		return invalid(pointer("commands", name), "command name %q does not match %s", name, commandNamePattern)
	}
	switch {
	case len(c.Argv) == 0:
		return invalid(pointer("commands", name, "argv"), "argv is empty")
	case c.Argv[0] == "":
		return invalid(pointer("commands", name, "argv", "0"), "argv[0], the program, is empty")
	}
	return nil
}

func (v Verification) validate() error {
	switch v.IndependentReview {
	case "", reviewNone, reviewFinal, reviewPhase:
	default:
		return invalid(pointer("verification", "independentReview"), "%q is not %s, %s or %s", v.IndependentReview, reviewNone, reviewFinal, reviewPhase)
	}
	if err := validateNames(v.RequiredChecks, pointer("verification", "requiredChecks")); err != nil {
		return err
	}
	for i, p := range v.ProtectedPaths {
		at := pointer("verification", "protectedPaths", strconv.Itoa(i))
		switch {
		case p == "":
			return invalid(at, "a protected path is empty")
		case !utf8.ValidString(p) || strings.ContainsRune(p, 0):
			return invalid(at, "a protected path must be valid UTF-8 without NUL")
		case strings.HasPrefix(p, "/"):
			return invalid(at, "%q is absolute; protected paths are relative to the repository root", p)
		case strings.Contains(p, `\`):
			return invalid(at, "%q contains a backslash; use / as the separator", p)
		case slices.Contains(strings.Split(p, "/"), ".."):
			return invalid(at, "%q has a .. segment", p)
		}
	}
	return nil
}

func (w Workspace) validate() error {
	if err := validateNames(w.Setup, pointer("workspace", "setup")); err != nil {
		return err
	}
	if w.Root != "" && (!filepath.IsAbs(w.Root) || filepath.Clean(w.Root) != w.Root) {
		return invalid(pointer("workspace", "root"), "%q is not an absolute, clean path", w.Root)
	}
	return nil
}

// validateNames checks a list of command names: valid names, no duplicates.
func validateNames(names []string, at string) error {
	seen := make(map[string]bool)
	for i, name := range names {
		switch {
		case !commandNamePattern.MatchString(name):
			return invalid(at+"/"+strconv.Itoa(i), "%q is not a command name (%s)", name, commandNamePattern)
		case seen[name]:
			return invalid(at+"/"+strconv.Itoa(i), "command %q is listed twice", name)
		}
		seen[name] = true
	}
	return nil
}

// Defaults returns the built-in defaults, the lowest-priority layer. They are
// provisional until SPEC §21 settles the formal defaults: strategy phased,
// maxConcurrency 2, maxPhaseRetries 3, maxReplans 2, timeout 2h, plan
// approval required, independentReview final, a clean repository required,
// no required checks, no protected paths, no setup, no agents, no commands,
// and no step or cost cap. Every call returns a fresh value.
func Defaults() Config {
	return Config{
		SchemaVersion: SchemaVersion,
		Execution: Execution{
			Strategy:            strategyPhased,
			MaxConcurrency:      ptr(2),
			MaxPhaseRetries:     ptr(3),
			MaxReplans:          ptr(2),
			Timeout:             "2h",
			RequirePlanApproval: ptr(true),
		},
		Verification: Verification{
			IndependentReview: reviewFinal,
			RequiredChecks:    []string{},
			ProtectedPaths:    []string{},
		},
		Workspace: Workspace{
			RequireCleanRepository: ptr(true),
			Setup:                  []string{},
		},
	}
}

func ptr[T any](v T) *T { return &v }

// ValidateEffective checks an effective configuration, the result of
// Resolve: it passes Validate, the settings that have a built-in default are
// all set, and every command that requiredChecks, setup and the
// Implementer's allowedCommands name is defined in commands. The agents and
// the commands themselves are optional.
func (c Config) ValidateEffective() error {
	if err := c.Validate(); err != nil {
		return err
	}
	e := c.Execution
	for _, s := range []struct {
		member string
		set    bool
	}{
		{"strategy", e.Strategy != ""},
		{"maxConcurrency", e.MaxConcurrency != nil},
		{"maxPhaseRetries", e.MaxPhaseRetries != nil},
		{"maxReplans", e.MaxReplans != nil},
		{"requirePlanApproval", e.RequirePlanApproval != nil},
	} {
		if !s.set {
			return invalid(pointer("execution", s.member), "the effective configuration does not set %s", s.member)
		}
	}
	if c.Verification.IndependentReview == "" {
		return invalid(pointer("verification", "independentReview"), "the effective configuration does not set independentReview")
	}
	if c.Workspace.RequireCleanRepository == nil {
		return invalid(pointer("workspace", "requireCleanRepository"), "the effective configuration does not set requireCleanRepository")
	}
	refs := []struct {
		at    string
		names []string
	}{
		{pointer("verification", "requiredChecks"), c.Verification.RequiredChecks},
		{pointer("workspace", "setup"), c.Workspace.Setup},
	}
	if a := c.Agents.Implementer; a != nil {
		refs = append(refs, struct {
			at    string
			names []string
		}{pointer("agents", "implementer", "allowedCommands"), a.AllowedCommands})
	}
	for _, r := range refs {
		for i, name := range r.names {
			if _, ok := c.Commands[name]; !ok {
				return invalid(r.at+"/"+strconv.Itoa(i), "command %q is not defined in commands", name)
			}
		}
	}
	return nil
}

// Limits returns the execution limits of the configuration in the form that
// the Run records (domain.Limits). The retry and replan limits must be set.
// The timeout is in the canonical form of time.Duration.String, so "2h"
// becomes "2h0m0s"; an unset timeout, step cap or cost cap stays unset.
func (c Config) Limits() (domain.Limits, error) {
	e := c.Execution
	if e.MaxPhaseRetries == nil || e.MaxReplans == nil {
		return domain.Limits{}, fmt.Errorf("%w: maxPhaseRetries and maxReplans must be set", ErrInvalidConfig)
	}
	l := domain.Limits{MaxPhaseRetries: *e.MaxPhaseRetries, MaxReplans: *e.MaxReplans}
	if e.Timeout != "" {
		d, err := parseTimeout(e.Timeout)
		if err != nil {
			return domain.Limits{}, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
		}
		l.Timeout = d.String()
	}
	if e.MaxAgentSteps != nil {
		l.MaxAgentSteps = *e.MaxAgentSteps
	}
	if e.MaxTurnCostUSD != nil {
		l.MaxTurnCostUSD = *e.MaxTurnCostUSD
	}
	if err := l.Validate(); err != nil {
		return domain.Limits{}, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}
	return l, nil
}

// Clone returns a deep copy of c: it shares no pointer, slice or map with c.
// A nil slice or map stays nil, an empty one stays empty.
func (c Config) Clone() Config {
	return deepCopy(reflect.ValueOf(c)).Interface().(Config)
}

// deepCopy copies v, a value of a configuration type: pointers, slices, maps
// and structs are copied recursively, everything else is a plain value.
func deepCopy(v reflect.Value) reflect.Value {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return v
		}
		p := reflect.New(v.Type().Elem())
		p.Elem().Set(deepCopy(v.Elem()))
		return p
	case reflect.Slice:
		if v.IsNil() {
			return v
		}
		s := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := range v.Len() {
			s.Index(i).Set(deepCopy(v.Index(i)))
		}
		return s
	case reflect.Map:
		if v.IsNil() {
			return v
		}
		m := reflect.MakeMapWithSize(v.Type(), v.Len())
		for it := v.MapRange(); it.Next(); {
			m.SetMapIndex(it.Key(), deepCopy(it.Value()))
		}
		return m
	case reflect.Struct:
		s := reflect.New(v.Type()).Elem()
		for i := range v.NumField() {
			s.Field(i).Set(deepCopy(v.Field(i)))
		}
		return s
	}
	return v
}
