package domain

import (
	"fmt"
	"strings"
)

// Run-relative layout of SPEC §12. Storage, the manifest and the engine share
// it so that no package other than storage needs to import storage.
const (
	TaskPath       = "task.json"
	ManifestPath   = "manifest.json"
	PolicyPath     = "policy.json"
	EventsPath     = "events.jsonl"
	SnapshotPath   = "state.json"
	LockPath       = "run.lock"
	DefinitionsDir = "definitions"
	LogsDir        = "logs"
	WorktreesDir   = "worktrees"
)

// RoleDefinitionPath returns the path of the frozen definition of role r:
// definitions/roles/<role>.md.
func RoleDefinitionPath(r Role) string {
	return DefinitionsDir + "/roles/" + string(r) + ".md"
}

// PlanPath returns the path of the plan of the given revision:
// plans/rev-<revision, four digits>/plan.json.
func PlanPath(revision int) string {
	return fmt.Sprintf("plans/rev-%04d/plan.json", revision)
}

// AttemptOutputPath returns the path of the validated output of one Turn:
// attempts/<attempt-id>/turns/<turn, four digits>/output.json.
func AttemptOutputPath(id AttemptID, turn int) string {
	return fmt.Sprintf("attempts/%s/turns/%04d/output.json", id, turn)
}

// IsReservedPath reports whether p names a file or directory that the engine
// owns and that is never an artifact: the journal, the snapshot, the lock
// file, and everything under logs and worktrees.
func IsReservedPath(p string) bool {
	switch p {
	case EventsPath, SnapshotPath, LockPath:
		return true
	}
	first, _, _ := strings.Cut(p, "/")
	return first == LogsDir || first == WorktreesDir
}
