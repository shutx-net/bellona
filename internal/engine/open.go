package engine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	"github.com/shutx-net/bellona/internal/config"
	"github.com/shutx-net/bellona/internal/domain"
	"github.com/shutx-net/bellona/internal/manifest"
	"github.com/shutx-net/bellona/internal/projection"
	"github.com/shutx-net/bellona/internal/storage"
)

// Snapshot problems that Open repairs, the values of OpenReport.SnapshotIssue.
const (
	snapshotMissing    = "missing"    // there is no state.json
	snapshotUnreadable = "unreadable" // state.json cannot be read or decoded
	snapshotStale      = "stale"      // state.json stops before the last event
	snapshotMismatch   = "mismatch"   // state.json differs from the journal at the same event
)

// OpenParams is the optional input of Open. Both fields only feed the report
// of changes: whatever they say, the Run keeps using its frozen copies.
type OpenParams struct {
	// CurrentConfig is the effective configuration that the caller resolved
	// now. When set, Open reports whether it differs from the frozen policy.
	CurrentConfig *config.Config
	// CurrentRoles are the role definitions that the caller has now. When
	// set, they must be one per role in the order of domain.Roles, and Open
	// reports the roles whose definition differs from the frozen copy.
	CurrentRoles []domain.RoleDefinition
}

// OpenReport tells what Open found and repaired.
type OpenReport struct {
	// RecoveredTail is set when an incomplete trailing journal record was cut
	// off; its bytes are preserved under logs/.
	RecoveredTail *storage.TailRecovery
	// SnapshotRebuilt is true when state.json was written again, and
	// SnapshotIssue says why: "missing", "unreadable", "stale" or "mismatch".
	SnapshotRebuilt bool
	SnapshotIssue   string
	// UncertainAttempts lists the Attempts that were Scheduled, Running or
	// Unknown when the journal ends. Their execution may be in progress or
	// their outcome is not known; Open reports them and never launches,
	// resumes, cancels or repeats anything (invariant 11). Reconciling them is
	// M3.
	UncertainAttempts []domain.AttemptID
	// ConfigChanged is true when CurrentConfig differs from the frozen policy.
	ConfigChanged bool
	// ChangedRoles lists, in the order of domain.Roles, the roles whose
	// CurrentRoles definition differs from the frozen copy.
	ChangedRoles []domain.Role
}

// Open resumes an existing Run: it takes the Run lock, recovers the journal
// tail, checks that the Run was recorded under contracts that this engine
// supports, replays the journal, verifies every artifact reference and repairs
// the snapshot, in this order:
//
//  1. take the Run lock (storage.ErrLocked, storage.ErrRunNotFound); a Run
//     directory without events.jsonl is refused as ErrIncompleteRun, like one
//     whose journal is empty (step 3), and nothing is created for it;
//  2. load and validate the journal: an incomplete trailing record is recovered
//     under the lock, with its bytes preserved under logs/ (the one thing Open
//     writes before it has accepted the Run); corruption in the middle is
//     refused (storage.ErrJournalCorrupt); a record of an unsupported event
//     schema is refused as ErrIncompatible;
//  3. require the first two events, RunCreated and ManifestFrozen
//     (ErrIncompleteRun);
//  4. read the manifest and refuse the Run when its contract versions are not
//     the supported ones (ErrIncompatible), before any event is interpreted;
//  5. replay the events with the reducer, which refuses a record that it
//     rejects (fsm.ErrIllegalTransition, projection.ErrInvalidEvent);
//  6. verify every artifact reference of the state and the role definitions of
//     the manifest: a missing, modified, oversized or symbolic-link file stops
//     the resume (R-PER-07);
//  7. check that the frozen files agree with ManifestFrozen (ErrInconsistentRun);
//  8. compare state.json with the replayed state: a missing, unreadable, stale
//     or mismatching snapshot is rebuilt, a snapshot ahead of the journal is
//     refused (ErrSnapshotAhead);
//  9. when the caller gives the current configuration or role definitions,
//     report what differs from the frozen copies, without applying it
//     (R-CFG-01, T-14);
//  10. report the uncertain Attempts.
//
// Every refusal releases the lock and leaves the Run directory as it was,
// except for the tail recovery of step 2. The snapshot is written last, after
// every check that can refuse the Run. Open never calls the runtime, so a Run
// that is Completed opens without any side effect (invariant 13); the report
// of a refused Open holds what was found until the refusal.
func (e *Engine) Open(ctx context.Context, id domain.RunID, p OpenParams) (*Run, OpenReport, error) {
	var rep OpenReport
	if err := ctx.Err(); err != nil {
		return nil, rep, err
	}
	if err := id.Validate(); err != nil {
		return nil, rep, err
	}
	store, err := storage.OpenRun(storage.RunDir(e.stateRoot, id), storage.Options{Faults: e.faults})
	if err != nil {
		// A Run directory without a journal is as incomplete as one with an empty
		// journal. storage.OpenRun created nothing and changed nothing.
		var pe *storage.PathError
		if errors.As(err, &pe) && pe.Path == domain.EventsPath && errors.Is(err, fs.ErrNotExist) {
			return nil, rep, fmt.Errorf("%w: the Run directory has no %s: %w", ErrIncompleteRun, domain.EventsPath, err)
		}
		return nil, rep, err
	}
	state, err := e.resume(store, id, p, &rep)
	if err != nil {
		if cerr := store.Close(); cerr != nil {
			e.logger.Error("releasing a Run that was refused", "run", string(id), "error", cerr)
		}
		return nil, rep, err
	}
	return e.startRun(id, store, state), rep, nil
}

// resume performs steps 2 to 10 of Open on a locked store and returns the
// replayed state. rep is filled as the steps go.
func (e *Engine) resume(store *storage.Store, id domain.RunID, p OpenParams, rep *OpenReport) (projection.State, error) {
	// 2. The journal.
	journal, err := store.LoadJournal()
	if err != nil {
		if errors.Is(err, domain.ErrUnsupportedEventSchema) {
			return projection.State{}, fmt.Errorf("%w: %w", ErrIncompatible, err)
		}
		return projection.State{}, err
	}
	if journal.Recovered != nil {
		rep.RecoveredTail = journal.Recovered
		e.logger.Warn("recovered an incomplete trailing journal record", "run", string(id),
			"offset", journal.Recovered.Offset, "bytes", journal.Recovered.Length, "preserved_as", journal.Recovered.PreservedAs)
	}
	events := journal.Events

	// 3. Every Run starts with RunCreated and ManifestFrozen.
	if len(events) < 2 {
		return projection.State{}, fmt.Errorf("%w: the journal holds %d event(s), a Run starts with RunCreated and ManifestFrozen",
			ErrIncompleteRun, len(events))
	}
	_, created := events[0].Payload.(domain.RunCreated)
	frozen, isFrozen := events[1].Payload.(domain.ManifestFrozen)
	if !created || !isFrozen {
		return projection.State{}, fmt.Errorf("%w: the journal starts with %s and %s, not with RunCreated and ManifestFrozen",
			ErrIncompleteRun, events[0].Type, events[1].Type)
	}

	// 4. Compatibility comes before the events are interpreted.
	manifestData, err := store.ReadArtifact(frozen.Manifest)
	if err != nil {
		return projection.State{}, err
	}
	m, err := manifest.Decode(manifestData)
	if err != nil {
		return projection.State{}, err
	}
	supported := manifest.Current()
	if e.supportedVersions != nil {
		supported = *e.supportedVersions
	}
	if err := manifest.CheckCompatible(m, supported); err != nil {
		return projection.State{}, err
	}

	// 5. Replay: pure state reconstruction.
	state, err := projection.Replay(events)
	if err != nil {
		return projection.State{}, err
	}

	// 6. Every reference is verified; loss or mismatch stops the resume.
	if err := store.VerifyArtifacts(state.Artifacts); err != nil {
		return projection.State{}, err
	}
	definitions := make([]domain.ArtifactRef, len(m.RoleDefinitions))
	for i, r := range m.RoleDefinitions {
		definitions[i] = r.Definition
	}
	if err := store.VerifyArtifacts(definitions); err != nil {
		return projection.State{}, err
	}

	// 7. The frozen files agree with the event that froze them. The limits of
	// policy.json are compared with the ManifestFrozen payload, not with the
	// replayed state: a PolicyAmended changes the state and never the policy.
	if m.RunID != id {
		return projection.State{}, fmt.Errorf("%w: the manifest belongs to %s, this Run is %s", ErrInconsistentRun, m.RunID, id)
	}
	if m.Policy != frozen.Policy {
		return projection.State{}, fmt.Errorf("%w: the manifest and ManifestFrozen refer to different policies", ErrInconsistentRun)
	}
	policyData, err := store.ReadArtifact(m.Policy)
	if err != nil {
		return projection.State{}, err
	}
	policy, err := config.Decode(policyData)
	if err != nil {
		return projection.State{}, fmt.Errorf("%w: %s: %w", ErrInconsistentRun, domain.PolicyPath, err)
	}
	limits, err := policy.Limits()
	if err != nil {
		return projection.State{}, fmt.Errorf("%w: %s: %w", ErrInconsistentRun, domain.PolicyPath, err)
	}
	if limits != frozen.Limits {
		return projection.State{}, fmt.Errorf("%w: the limits of %s differ from those of ManifestFrozen", ErrInconsistentRun, domain.PolicyPath)
	}

	// 8. The snapshot is a cache of the journal. Decide first, write last.
	issue, err := snapshotIssue(store, state)
	if err != nil {
		return projection.State{}, err
	}

	// 9. Changes since the freeze are reported, never applied.
	if p.CurrentConfig != nil {
		if rep.ConfigChanged, err = manifest.CompareConfig(m, *p.CurrentConfig); err != nil {
			return projection.State{}, err
		}
	}
	if p.CurrentRoles != nil {
		if rep.ChangedRoles, err = manifest.CompareRoles(m, p.CurrentRoles); err != nil {
			return projection.State{}, err
		}
	}

	// 10. Uncertain Attempts are reported and left alone.
	rep.UncertainAttempts = state.UncertainAttempts()

	if issue != "" {
		snapshot, err := projection.EncodeSnapshot(state)
		if err != nil {
			return projection.State{}, fmt.Errorf("engine: encode the snapshot: %w", err)
		}
		if err := store.WriteSnapshot(snapshot); err != nil {
			return projection.State{}, fmt.Errorf("engine: rebuild the snapshot: %w", err)
		}
		rep.SnapshotRebuilt, rep.SnapshotIssue = true, issue
		e.logger.Info("rebuilt the snapshot from the journal", "run", string(id), "issue", issue)
	}
	return state, nil
}

// snapshotIssue compares state.json with the state that the journal produces.
// It returns "" when the snapshot is current, the reason why it must be
// rebuilt otherwise, and ErrSnapshotAhead when it records events that the
// journal does not hold. It writes nothing.
func snapshotIssue(store *storage.Store, state projection.State) (string, error) {
	data, err := store.ReadSnapshot()
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return snapshotMissing, nil
	case err != nil:
		return snapshotUnreadable, nil
	}
	snap, err := projection.DecodeSnapshot(data)
	switch {
	case err != nil:
		return snapshotUnreadable, nil
	case snap.LastSeq > state.LastSeq:
		return "", fmt.Errorf("%w: the snapshot records event %d, the journal ends at event %d; committed records are missing from the journal",
			ErrSnapshotAhead, snap.LastSeq, state.LastSeq)
	case snap.LastSeq < state.LastSeq:
		return snapshotStale, nil
	case !projection.Equal(snap, state):
		return snapshotMismatch, nil
	}
	return "", nil
}
