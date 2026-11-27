package domain

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"time"
)

// ErrInvalidLimit is wrapped by every error about an execution limit: an
// invalid Limits value, an unknown limit name or a value that is not the
// canonical text of a limit.
var ErrInvalidLimit = errors.New("invalid execution limit")

// unlimited is the canonical text of an unset optional limit.
const unlimited = "unlimited"

// Limits are the execution limits of a Run. They are frozen at Run start and
// can change only through a journaled user policy amendment (SPEC §6.3).
//
// Timeout is the Run time limit in the canonical form of time.Duration.String
// (for example "1h30m0s"); the empty string means no Run time limit.
// MaxAgentSteps and MaxTurnCostUSD are per-Turn caps, where the host supports
// them (SPEC §14.2); zero means no cap. The three optional limits are absent
// from the JSON form when unset.
type Limits struct {
	MaxPhaseRetries int     `json:"maxPhaseRetries"`
	MaxReplans      int     `json:"maxReplans"`
	Timeout         string  `json:"timeout,omitzero"`
	MaxAgentSteps   int     `json:"maxAgentSteps,omitzero"`
	MaxTurnCostUSD  float64 `json:"maxTurnCostUsd,omitzero"`
}

// Validate checks that the retry and replan limits and the step cap are not
// negative, that Timeout is empty or a positive duration in its canonical
// form, and that the cost cap is finite, not negative and not negative zero.
// Every error wraps ErrInvalidLimit.
func (l Limits) Validate() error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidLimit, fmt.Sprintf(format, args...))
	}
	switch {
	case l.MaxPhaseRetries < 0:
		return bad("maxPhaseRetries is %d, want 0 or more", l.MaxPhaseRetries)
	case l.MaxReplans < 0:
		return bad("maxReplans is %d, want 0 or more", l.MaxReplans)
	case l.MaxAgentSteps < 0:
		return bad("maxAgentSteps is %d, want 0 or more", l.MaxAgentSteps)
	case math.IsNaN(l.MaxTurnCostUSD) || math.IsInf(l.MaxTurnCostUSD, 0):
		return bad("maxTurnCostUsd is not finite")
	case l.MaxTurnCostUSD < 0 || math.Signbit(l.MaxTurnCostUSD):
		return bad("maxTurnCostUsd is %v, want 0 or more", l.MaxTurnCostUSD)
	}
	if l.Timeout != "" {
		d, err := time.ParseDuration(l.Timeout)
		switch {
		case err != nil:
			return bad("timeout %q is not a duration: %v", l.Timeout, err)
		case d <= 0:
			return bad("timeout %q is not positive", l.Timeout)
		case d.String() != l.Timeout:
			return bad("timeout %q is not in the canonical form %q", l.Timeout, d.String())
		}
	}
	return nil
}

// LimitName names one execution limit. The values are the configuration keys
// under execution in SPEC §14.
type LimitName string

// The execution limits.
const (
	LimitMaxPhaseRetries LimitName = "maxPhaseRetries"
	LimitMaxReplans      LimitName = "maxReplans"
	LimitTimeout         LimitName = "timeout"
	LimitMaxAgentSteps   LimitName = "maxAgentSteps"
	LimitMaxTurnCostUSD  LimitName = "maxTurnCostUsd"
)

var limitNames = []LimitName{
	LimitMaxPhaseRetries, LimitMaxReplans, LimitTimeout, LimitMaxAgentSteps, LimitMaxTurnCostUSD,
}

// LimitNames returns every limit name in the order of the Limits fields. The
// caller may modify the returned slice.
func LimitNames() []LimitName { return slices.Clone(limitNames) }

// Valid reports whether n is a limit name.
func (n LimitName) Valid() bool { return slices.Contains(limitNames, n) }

// String returns the name.
func (n LimitName) String() string { return string(n) }

// MarshalText implements encoding.TextMarshaler and refuses an unknown name.
func (n LimitName) MarshalText() ([]byte, error) {
	return marshalEnum("limit name", ErrInvalidLimit, n)
}

// UnmarshalText implements encoding.TextUnmarshaler and refuses an unknown
// name, including the empty string.
func (n *LimitName) UnmarshalText(b []byte) error {
	return unmarshalEnum("limit name", ErrInvalidLimit, n, b)
}

// Value returns the canonical text of the limit n: a decimal integer for the
// retry and replan limits and the step cap, strconv.FormatFloat(v, 'f', -1,
// 64) for the cost cap, the canonical duration for the timeout, and the
// literal "unlimited" for an unset timeout, step cap or cost cap. It returns
// the empty string for an unknown name.
func (l Limits) Value(n LimitName) string {
	switch n {
	case LimitMaxPhaseRetries:
		return strconv.Itoa(l.MaxPhaseRetries)
	case LimitMaxReplans:
		return strconv.Itoa(l.MaxReplans)
	case LimitTimeout:
		if l.Timeout == "" {
			return unlimited
		}
		return l.Timeout
	case LimitMaxAgentSteps:
		if l.MaxAgentSteps == 0 {
			return unlimited
		}
		return strconv.Itoa(l.MaxAgentSteps)
	case LimitMaxTurnCostUSD:
		if l.MaxTurnCostUSD == 0 {
			return unlimited
		}
		return strconv.FormatFloat(l.MaxTurnCostUSD, 'f', -1, 64)
	}
	return ""
}

// With returns a copy of l in which the limit n has the value given as
// canonical text (see Value). The text must be exactly the text that Value
// returns for the updated copy, so "007", "+5", "1e2", "90m" and, for the
// three optional limits, "0" are rejected; "unlimited" is accepted only for
// the timeout, the step cap and the cost cap. The updated copy must validate.
// Every error wraps ErrInvalidLimit.
func (l Limits) With(n LimitName, value string) (Limits, error) {
	out := l
	var err error
	switch n {
	case LimitMaxPhaseRetries:
		out.MaxPhaseRetries, err = strconv.Atoi(value)
	case LimitMaxReplans:
		out.MaxReplans, err = strconv.Atoi(value)
	case LimitTimeout:
		if value == unlimited {
			out.Timeout = ""
		} else {
			out.Timeout = value
		}
	case LimitMaxAgentSteps:
		if value == unlimited {
			out.MaxAgentSteps = 0
		} else {
			out.MaxAgentSteps, err = strconv.Atoi(value)
		}
	case LimitMaxTurnCostUSD:
		if value == unlimited {
			out.MaxTurnCostUSD = 0
		} else {
			out.MaxTurnCostUSD, err = strconv.ParseFloat(value, 64)
		}
	default:
		return l, fmt.Errorf("%w: unknown limit %q", ErrInvalidLimit, string(n))
	}
	if err != nil {
		return l, fmt.Errorf("%w: %s %q is not a valid value: %v", ErrInvalidLimit, n, value, err)
	}
	if err := out.Validate(); err != nil {
		return l, err
	}
	if canonical := out.Value(n); canonical != value {
		return l, fmt.Errorf("%w: %s %q is not in the canonical form %q", ErrInvalidLimit, n, value, canonical)
	}
	return out, nil
}

// LimitChange is one entry of a policy amendment: the previous and the new
// canonical text of a limit (see Limits.Value).
type LimitChange struct {
	Limit    LimitName `json:"limit"`
	Previous string    `json:"previous"`
	New      string    `json:"new"`
}
