package domain

import (
	"errors"
	"fmt"
	"slices"
)

// ErrInvalidRole is wrapped by every role validation error.
var ErrInvalidRole = errors.New("invalid role")

// Role is the logical role of an LLM agent (SPEC §4). The Workflow Engine is
// a program, not a Role.
type Role string

// The agent roles.
const (
	RoleCoordinator Role = "coordinator"
	RolePlanner     Role = "planner"
	RoleImplementer Role = "implementer"
	RoleEvaluator   Role = "evaluator"
)

var roles = []Role{RoleCoordinator, RolePlanner, RoleImplementer, RoleEvaluator}

// Roles returns every role in the order coordinator, planner, implementer,
// evaluator. The caller may modify the returned slice.
func Roles() []Role { return slices.Clone(roles) }

// Valid reports whether r is one of the four roles.
func (r Role) Valid() bool { return slices.Contains(roles, r) }

// Validate returns an error wrapping ErrInvalidRole unless r is valid.
func (r Role) Validate() error {
	if r.Valid() {
		return nil
	}
	return fmt.Errorf("%w: %q", ErrInvalidRole, string(r))
}

// String returns the role name.
func (r Role) String() string { return string(r) }

// PhaseBound reports whether an Attempt of this role works for one Phase. An
// Implementer always does. An Evaluator does for a phase review; in the final
// evaluation it works for the Run instead.
func (r Role) PhaseBound() bool { return r == RoleImplementer || r == RoleEvaluator }

// MarshalText implements encoding.TextMarshaler and refuses an invalid role.
func (r Role) MarshalText() ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return []byte(r), nil
}

// UnmarshalText implements encoding.TextUnmarshaler and refuses an invalid
// role, including the empty string.
func (r *Role) UnmarshalText(b []byte) error {
	v := Role(b)
	if err := v.Validate(); err != nil {
		return err
	}
	*r = v
	return nil
}
