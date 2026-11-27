package evals

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/shutx-net/bellona/internal/strictjson"
)

// ResultSchemaVersion is the schema version of a result record.
const ResultSchemaVersion = 1

// Variant names one of the variants of the comparative evaluation of SPEC §19.
type Variant string

// The variants.
const (
	// VariantBaselineA is a single agent plus the mandatory checks, run
	// outside Bellona until the Direct strategy exists.
	VariantBaselineA Variant = "baseline-a"
	// VariantBaselineB is a fixed-effort Planner and Implementer with
	// independentReview none, run once for each permitted effort level.
	VariantBaselineB Variant = "baseline-b"
	// VariantC is an adaptive-effort Planner and Implementer with
	// independentReview none.
	VariantC Variant = "variant-c"
	// VariantD is Variant C plus an independent Evaluator.
	VariantD Variant = "variant-d"
	// VariantE is Variant D plus session reuse and parallel scheduling.
	VariantE Variant = "variant-e"
)

// Variants returns the variants in the order of SPEC §19.
func Variants() []Variant {
	return []Variant{VariantBaselineA, VariantBaselineB, VariantC, VariantD, VariantE}
}

// Valid reports whether v is one of the variants.
func (v Variant) Valid() bool { return slices.Contains(Variants(), v) }

// Result is the record of one trial: one variant working on one task once.
// It carries the metrics of SPEC §19 "Comparative evaluations": the
// independent acceptance outcome, regressions, unnecessary changes, execution
// time, measurable tokens and costs, the retry count and the plan revision
// count.
//
// Every value that a trial could not measure is an absent member, never zero
// (SPEC §14.2): a pointer member that is nil is not encoded, and a pointer to
// zero is a measured zero. Token use and cost as a multiple of Baseline A are
// computed when results are reported and are not stored.
//
// The record has no field for a model's own opinion of its work: a model's
// self-evaluation is never a success metric (SPEC §19).
type Result struct {
	SchemaVersion int `json:"schemaVersion"`
	// TaskID is the id of the task of the trial.
	TaskID  string  `json:"taskId"`
	Variant Variant `json:"variant"`
	// Trial numbers the repetitions of one variant on one task, from 1.
	Trial int `json:"trial"`
	// FixedEffort is the effort level of a Baseline B trial, which runs once
	// for each permitted level. It is present for Baseline B and absent for
	// every other variant.
	FixedEffort string `json:"fixedEffort,omitzero"`
	// BellonaVersion is the engine version (version.Engine) of the trial. It
	// is absent for Baseline A, which runs outside Bellona, and present for
	// every other variant.
	BellonaVersion string `json:"bellonaVersion,omitzero"`
	// Models lists the model of each role that took part, as requested and as
	// confirmed by the host (SPEC §7).
	Models     []RoleModel `json:"models,omitzero"`
	StartedAt  time.Time   `json:"startedAt"`
	FinishedAt time.Time   `json:"finishedAt"`
	// Acceptance is the outcome of the acceptance checks of the task, run
	// after the hidden acceptance files were copied in.
	Acceptance CheckSummary `json:"acceptance"`
	// Regressions counts the regression checks of the task that failed after
	// the trial.
	Regressions *int `json:"regressions,omitzero"`
	// UnnecessaryChangedFiles counts the files changed outside the
	// expectedChangeScope of the task.
	UnnecessaryChangedFiles *int `json:"unnecessaryChangedFiles,omitzero"`
	// Tokens is the measured token use, summed over the whole trial.
	Tokens *Tokens `json:"tokens,omitzero"`
	// APIEquivalentCostUSD is the cost at API prices, which differs from what
	// a subscription bills.
	APIEquivalentCostUSD *float64 `json:"apiEquivalentCostUsd,omitzero"`
	// BilledCostUSD is the amount that was actually billed.
	BilledCostUSD *float64 `json:"billedCostUsd,omitzero"`
	// RetryCount counts the Attempts that retried a Phase.
	RetryCount *int `json:"retryCount,omitzero"`
	// PlanRevisionCount counts the Plan Revisions after the first.
	PlanRevisionCount *int   `json:"planRevisionCount,omitzero"`
	Notes             string `json:"notes,omitzero"`
}

// RoleModel records the model of one role: the model that was requested, and
// the model that the host confirmed having used when it reports it.
type RoleModel struct {
	// Role is the Bellona role name, or "agent" for the single agent of
	// Baseline A.
	Role      string `json:"role"`
	Requested string `json:"requested"`
	Confirmed string `json:"confirmed,omitzero"`
}

// CheckSummary is the outcome of a set of checks.
type CheckSummary struct {
	// Passed is true if and only if every check exited with status 0.
	Passed bool           `json:"passed"`
	Checks []CheckOutcome `json:"checks"`
}

// CheckOutcome is the outcome of one check of the task.
type CheckOutcome struct {
	ID       string `json:"id"`
	ExitCode int    `json:"exitCode"`
}

// Tokens is the measured token use. A member that the host did not report is
// absent. At least one member must be present: a trial without measured
// tokens has no Tokens at all.
type Tokens struct {
	Input         *int64 `json:"input,omitzero"`
	Output        *int64 `json:"output,omitzero"`
	CacheRead     *int64 `json:"cacheRead,omitzero"`
	CacheCreation *int64 `json:"cacheCreation,omitzero"`
}

// Validate checks the record: versions, ids, the variant, the trial number,
// the fixed effort and the Bellona version for the variant, the times, the
// acceptance summary and every measured value. Decode calls Validate on the
// top-level value only, so it also validates every nested value.
func (r Result) Validate() error {
	if r.SchemaVersion != ResultSchemaVersion {
		return invalid("/schemaVersion", "schema version %d is not supported, want %d", r.SchemaVersion, ResultSchemaVersion)
	}
	if !idPattern.MatchString(r.TaskID) {
		return invalid("/taskId", "%q does not match %s", r.TaskID, idPattern)
	}
	if !r.Variant.Valid() {
		return invalid("/variant", "%q is not one of %v", string(r.Variant), Variants())
	}
	if r.Trial < 1 {
		return invalid("/trial", "trial %d is below 1", r.Trial)
	}
	switch {
	case r.Variant == VariantBaselineB && r.FixedEffort == "":
		return invalid("/fixedEffort", "%s runs at a fixed effort level, so fixedEffort is required", VariantBaselineB)
	case r.Variant != VariantBaselineB && r.FixedEffort != "":
		return invalid("/fixedEffort", "only %s has a fixed effort level", VariantBaselineB)
	case r.FixedEffort != "" && !tokenPattern.MatchString(r.FixedEffort):
		return invalid("/fixedEffort", "%q does not match %s", r.FixedEffort, tokenPattern)
	}
	switch {
	case r.Variant == VariantBaselineA && r.BellonaVersion != "":
		return invalid("/bellonaVersion", "%s runs outside Bellona, so bellonaVersion must be absent", VariantBaselineA)
	case r.Variant != VariantBaselineA && r.BellonaVersion == "":
		return invalid("/bellonaVersion", "%s runs in Bellona, so bellonaVersion is required", r.Variant)
	case r.BellonaVersion != "":
		if err := checkLine("/bellonaVersion", r.BellonaVersion, 128); err != nil {
			return err
		}
	}
	if err := validateModels(r.Models); err != nil {
		return err
	}
	if err := r.validateTimes(); err != nil {
		return err
	}
	if err := r.Acceptance.validate("/acceptance"); err != nil {
		return err
	}
	if err := validateMeasures(r); err != nil {
		return err
	}
	if r.Notes != "" {
		if err := checkText("/notes", r.Notes, maxTextBytes); err != nil {
			return err
		}
	}
	return nil
}

// checkLine requires a non-empty single line of valid UTF-8 without control
// characters, of at most limit bytes.
func checkLine(pointer, s string, limit int) error {
	switch {
	case !utf8.ValidString(s):
		return invalid(pointer, "not valid UTF-8")
	case strings.ContainsFunc(s, func(r rune) bool { return r < ' ' || r == 0x7f }):
		return invalid(pointer, "contains a control character")
	case len(s) > limit:
		return invalid(pointer, "%d bytes exceed the limit of %d", len(s), limit)
	}
	return nil
}

func validateModels(models []RoleModel) error {
	seen := make(map[string]bool, len(models))
	for i, m := range models {
		at := fmt.Sprintf("/models/%d", i)
		for _, f := range []struct{ name, value string }{{"role", m.Role}, {"requested", m.Requested}} {
			if f.value == "" {
				return invalid(at+"/"+f.name, "must not be empty")
			}
			if err := checkLine(at+"/"+f.name, f.value, 128); err != nil {
				return err
			}
		}
		if m.Confirmed != "" {
			if err := checkLine(at+"/confirmed", m.Confirmed, 128); err != nil {
				return err
			}
		}
		if seen[m.Role] {
			return invalid(at+"/role", "duplicate role %q", m.Role)
		}
		seen[m.Role] = true
	}
	return nil
}

func (r Result) validateTimes() error {
	for _, f := range []struct {
		pointer string
		t       time.Time
	}{{"/startedAt", r.StartedAt}, {"/finishedAt", r.FinishedAt}} {
		switch {
		case f.t.IsZero():
			return invalid(f.pointer, "the time is not set")
		case f.t.Location() != time.UTC:
			return invalid(f.pointer, "the time %s is not in UTC", f.t.Format(time.RFC3339Nano))
		}
	}
	if r.FinishedAt.Before(r.StartedAt) {
		return invalid("/finishedAt", "%s is before startedAt %s",
			r.FinishedAt.Format(time.RFC3339Nano), r.StartedAt.Format(time.RFC3339Nano))
	}
	return nil
}

// validate checks the checks and the Passed flag.
func (s CheckSummary) validate(pointer string) error {
	if len(s.Checks) == 0 {
		return invalid(pointer+"/checks", "at least one check is required")
	}
	seen := make(map[string]bool, len(s.Checks))
	all := true
	for i, c := range s.Checks {
		at := fmt.Sprintf("%s/checks/%d", pointer, i)
		switch {
		case !idPattern.MatchString(c.ID):
			return invalid(at+"/id", "%q does not match %s", c.ID, idPattern)
		case seen[c.ID]:
			return invalid(at+"/id", "duplicate check id %q", c.ID)
		}
		seen[c.ID] = true
		all = all && c.ExitCode == 0
	}
	if s.Passed != all {
		return invalid(pointer+"/passed", "passed is %t, but the exit codes say %t: passed is true if and only if every check exited with status 0", s.Passed, all)
	}
	return nil
}

// validateMeasures checks the counts, the tokens and the costs: each is
// absent or non-negative and finite.
func validateMeasures(r Result) error {
	for _, c := range []struct {
		pointer string
		value   *int
	}{
		{"/regressions", r.Regressions},
		{"/unnecessaryChangedFiles", r.UnnecessaryChangedFiles},
		{"/retryCount", r.RetryCount},
		{"/planRevisionCount", r.PlanRevisionCount},
	} {
		if c.value != nil && *c.value < 0 {
			return invalid(c.pointer, "%d is negative; leave an unmeasured count out instead", *c.value)
		}
	}
	if t := r.Tokens; t != nil {
		if t.Input == nil && t.Output == nil && t.CacheRead == nil && t.CacheCreation == nil {
			return invalid("/tokens", "no member is measured; leave tokens out instead of recording an empty object")
		}
		for _, c := range []struct {
			pointer string
			value   *int64
		}{
			{"/tokens/input", t.Input},
			{"/tokens/output", t.Output},
			{"/tokens/cacheRead", t.CacheRead},
			{"/tokens/cacheCreation", t.CacheCreation},
		} {
			if c.value != nil && *c.value < 0 {
				return invalid(c.pointer, "%d is negative; leave an unmeasured value out instead", *c.value)
			}
		}
	}
	for _, c := range []struct {
		pointer string
		value   *float64
	}{
		{"/apiEquivalentCostUsd", r.APIEquivalentCostUSD},
		{"/billedCostUsd", r.BilledCostUSD},
	} {
		if c.value == nil {
			continue
		}
		if math.IsNaN(*c.value) || math.IsInf(*c.value, 0) || *c.value < 0 {
			return invalid(c.pointer, "%v is not a finite, non-negative cost; leave an unmeasured cost out instead", *c.value)
		}
	}
	return nil
}

// DecodeResult strictly decodes and validates a result record.
func DecodeResult(data []byte) (Result, error) {
	return strictjson.Decode[Result](data)
}
