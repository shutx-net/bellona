package engine

import (
	"fmt"

	"github.com/shutx-net/bellona/internal/artifact"
	"github.com/shutx-net/bellona/internal/domain"
	"github.com/shutx-net/bellona/internal/strictjson"
)

// maxOutputBytes is the largest agent output that the engine examines. A
// longer output is invalid, never truncated (SPEC §12.2).
const maxOutputBytes = 1 << 20

// outputContract is what the engine knows about the final message of one
// role: how to validate it (R-ART-01) and which bytes to store.
type outputContract struct {
	// validate checks the agent's output strictly and returns the canonical
	// bytes of the stored artifact.
	validate func(output []byte) ([]byte, error)
}

// contracts is the output contract registry, keyed by role. In M0 only the
// Implementer has a contract. The plan, evaluation and decision contracts
// arrive with the milestones that launch those roles (M2 and M3), so a
// StartAttempt for another role is refused with ErrNoOutputContract.
var contracts = map[domain.Role]outputContract{
	domain.RoleImplementer: {validate: validateImplementation},
}

// contractFor returns the output contract of the role.
func contractFor(role domain.Role) (outputContract, bool) {
	c, ok := contracts[role]
	return c, ok
}

// validateImplementation decodes the output with the strict decoder of the
// Implementation result and returns the canonical indented encoding of the
// decoded value, which is what the engine stores as output.json.
func validateImplementation(output []byte) ([]byte, error) {
	if len(output) > maxOutputBytes {
		return nil, fmt.Errorf("the output has %d bytes, the limit is %d", len(output), maxOutputBytes)
	}
	result, err := artifact.DecodeImplementationResult(output)
	if err != nil {
		return nil, err
	}
	stored, err := strictjson.EncodeIndent(result)
	if err != nil {
		return nil, fmt.Errorf("encode the implementation result: %w", err)
	}
	return stored, nil
}
