package storage

import "strconv"

// CrashPoint names a step of the persistence order at which a test can stop
// an operation, as a crash would. The values are in the order in which one
// Run goes through them (SPEC §12, "Durability and concurrency"): finalize an
// artifact, append and synchronize the event that refers to it, replace the
// snapshot. The zero value is not a crash point.
type CrashPoint int

const (
	// CrashArtifactTempSynced: the temporary file is written and fsynced and
	// is not renamed yet. The artifact does not exist.
	CrashArtifactTempSynced CrashPoint = iota + 1
	// CrashArtifactRenamed: the artifact is renamed into place and its
	// directory is not fsynced yet.
	CrashArtifactRenamed
	// CrashArtifactDirSynced: the artifact is durable and no event refers to
	// it yet.
	CrashArtifactDirSynced
	// CrashEventTorn: only the first half of the record is written, without
	// the newline.
	CrashEventTorn
	// CrashEventWritten: the record and its newline are written and not
	// fsynced. An in-process crash cannot lose page-cache data, so this
	// models a write that happened to survive.
	CrashEventWritten
	// CrashEventSynced: the record is fsynced; the snapshot is not updated.
	CrashEventSynced
	// CrashSnapshotTempSynced: the new snapshot is in a temporary file, not
	// renamed yet; state.json still holds the old snapshot.
	CrashSnapshotTempSynced
	// CrashSnapshotRenamed: the new state.json is in place and its directory
	// is not fsynced yet.
	CrashSnapshotRenamed
)

var crashPointNames = [...]string{
	CrashArtifactTempSynced: "CrashArtifactTempSynced",
	CrashArtifactRenamed:    "CrashArtifactRenamed",
	CrashArtifactDirSynced:  "CrashArtifactDirSynced",
	CrashEventTorn:          "CrashEventTorn",
	CrashEventWritten:       "CrashEventWritten",
	CrashEventSynced:        "CrashEventSynced",
	CrashSnapshotTempSynced: "CrashSnapshotTempSynced",
	CrashSnapshotRenamed:    "CrashSnapshotRenamed",
}

// String returns the name of the constant, or "CrashPoint(n)" for a value
// that is not a crash point.
func (p CrashPoint) String() string {
	if p >= 1 && int(p) < len(crashPointNames) {
		return crashPointNames[p]
	}
	return "CrashPoint(" + strconv.Itoa(int(p)) + ")"
}

// CrashPoints returns every crash point in persistence order.
func CrashPoints() []CrashPoint {
	out := make([]CrashPoint, 0, len(crashPointNames)-1)
	for p := CrashArtifactTempSynced; int(p) < len(crashPointNames); p++ {
		out = append(out, p)
	}
	return out
}

// FaultInjector decides at each crash point whether the operation stops
// there. When it returns true the Store stops the current operation right
// there, leaves the files as a crash would, marks itself dead and returns an
// error that wraps ErrInjectedCrash; every later call returns ErrStoreDead
// until Abandon releases the Run lock. A nil FaultInjector never fires.
// Production code never sets one.
type FaultInjector func(CrashPoint) bool
