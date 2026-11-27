package domain

import (
	"errors"
	"fmt"
	"unicode/utf8"
)

// ErrInvalidRoleDefinition is wrapped by every role definition validation
// error.
var ErrInvalidRoleDefinition = errors.New("invalid role definition")

// MaxRoleDefinitionBytes is the largest accepted role definition.
const MaxRoleDefinitionBytes = 1 << 20

// RoleDefinition is the text that defines one Role, for example the
// instructions that an agent of that role receives. The Run manifest freezes
// the contents of the four definitions by copying them (SPEC §12.4), so the
// definition is carried as bytes, not as a path. The type lives here so that
// the roles package, the manifest and the engine share it without importing
// one another.
type RoleDefinition struct {
	Role    Role
	Content []byte
}

// ValidateRoleDefinitions checks that defs holds exactly one definition per
// role, in the order of Roles, and that every definition is non-empty, valid
// UTF-8 and at most MaxRoleDefinitionBytes bytes. Every error wraps
// ErrInvalidRoleDefinition.
func ValidateRoleDefinitions(defs []RoleDefinition) error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidRoleDefinition, fmt.Sprintf(format, args...))
	}
	want := Roles()
	if len(defs) != len(want) {
		return bad("%d definitions, want one for each of the %d roles", len(defs), len(want))
	}
	for i, d := range defs {
		switch {
		case d.Role != want[i]:
			return bad("definition %d is for role %q, want %q (one per role, in the order of Roles)", i, string(d.Role), string(want[i]))
		case len(d.Content) == 0:
			return bad("the definition of %s is empty", d.Role)
		case len(d.Content) > MaxRoleDefinitionBytes:
			return bad("the definition of %s has %d bytes, the limit is %d", d.Role, len(d.Content), MaxRoleDefinitionBytes)
		case !utf8.Valid(d.Content):
			return bad("the definition of %s is not valid UTF-8", d.Role)
		}
	}
	return nil
}
