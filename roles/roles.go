// Package roles embeds the role definitions of Bellona. A role definition is
// the text that defines one agent role (SPEC §4). The definitions are
// compiled into the binary, and each Run freezes a copy of them in its
// manifest (SPEC §12.4), so a Run keeps using the definitions it started
// with. The four definitions in this package are M0 placeholders: they exist
// to be frozen and change-detected, and the real definitions arrive with the
// milestones that launch the roles.
package roles

import (
	"embed"
	"fmt"

	"github.com/shutx-net/bellona/internal/domain"
)

//go:embed coordinator.md planner.md implementer.md evaluator.md
var files embed.FS

// Definitions returns the embedded definition of every role, in the order of
// domain.Roles. Every call returns fresh copies, so a caller may modify the
// contents it receives. It panics when an embedded file is missing, which
// cannot happen in a program that compiled.
func Definitions() []domain.RoleDefinition {
	defs := make([]domain.RoleDefinition, 0, len(domain.Roles()))
	for _, r := range domain.Roles() {
		content, err := files.ReadFile(string(r) + ".md")
		if err != nil {
			panic(fmt.Sprintf("roles: the definition of %s is not embedded: %v", r, err))
		}
		defs = append(defs, domain.RoleDefinition{Role: r, Content: content})
	}
	return defs
}
