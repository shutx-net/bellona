package projection

import (
	"errors"
	"fmt"

	json "encoding/json/v2"

	"github.com/shutx-net/bellona/internal/domain"
	"github.com/shutx-net/bellona/internal/strictjson"
)

// SnapshotSchemaVersion is the schema version of state.json.
const SnapshotSchemaVersion = 1

// ErrSnapshotVersion is wrapped by the error of DecodeSnapshot for a snapshot
// whose schemaVersion differs from SnapshotSchemaVersion. A snapshot is only a
// cache of the journal, so the engine reacts by rebuilding it with Replay
// instead of treating the Run as corrupt.
var ErrSnapshotVersion = errors.New("unsupported snapshot schema version")

// Snapshot is the content of state.json. The last applied sequence number is
// state.lastSeq. The snapshot is derived data: it is reconstructible from
// events.jsonl, which stays the authoritative source of the Run state (SPEC
// §12, R-PER-02).
type Snapshot struct {
	SchemaVersion int   `json:"schemaVersion"`
	State         State `json:"state"`
}

// Replay returns the State that the events produce when they are applied in
// order from the empty State, or the *ApplyError of the first event that
// cannot be applied; its Seq identifies the offending record. The State
// returned with an error is the empty State. Replay has no side effects:
// nothing is launched, no clock is read, and the same events always yield the
// same State (SPEC invariant 13). It gives the same result as folding Apply
// over the events, without copying the State for every event.
func Replay(events []domain.Event) (State, error) {
	var s State
	for _, e := range events {
		if err := s.apply(e); err != nil {
			return State{}, &ApplyError{Seq: e.Seq, Type: e.Type, Err: err}
		}
	}
	return s, nil
}

// EncodeSnapshot returns the content of state.json for s: indented
// deterministic JSON with a trailing newline. The empty State cannot be
// encoded, because a snapshot exists only after the first event.
func EncodeSnapshot(s State) ([]byte, error) {
	return strictjson.EncodeIndent(Snapshot{SchemaVersion: SnapshotSchemaVersion, State: s})
}

// DecodeSnapshot decodes the content of state.json. It reads the schema
// version alone first, so that a snapshot of another version yields an error
// that wraps ErrSnapshotVersion even when the rest of it does not parse under
// this version's contract. The rest is strict (package strictjson): no
// unknown, missing, duplicate or null member. DecodeSnapshot checks the shape
// only; the snapshot is a cache, and the engine compares it with the journal.
func DecodeSnapshot(data []byte) (State, error) {
	// A lenient peek. When it fails or finds no schema version, the strict
	// decoding below reports the precise reason.
	var peek struct {
		SchemaVersion *int `json:"schemaVersion"`
	}
	if err := json.Unmarshal(data, &peek); err == nil && peek.SchemaVersion != nil &&
		*peek.SchemaVersion != SnapshotSchemaVersion {
		return State{}, versionError(*peek.SchemaVersion)
	}
	snap, err := strictjson.Decode[Snapshot](data)
	if err != nil {
		return State{}, err
	}
	if snap.SchemaVersion != SnapshotSchemaVersion {
		return State{}, versionError(snap.SchemaVersion)
	}
	return snap.State, nil
}

func versionError(got int) error {
	return fmt.Errorf("%w: the snapshot has schema version %d, this version supports %d",
		ErrSnapshotVersion, got, SnapshotSchemaVersion)
}
