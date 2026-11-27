// Package storage is the file-based persistence of a Run (SPEC §12): the
// per-Run OS file lock, immutable artifacts, the append-only event journal,
// the replaceable snapshot, and the fault-injection points that let tests
// stop at every step of the persistence order.
//
// The state root holds plain files and directories only; there is no
// database (R-PER-01). Linux only: it relies on flock(2), on fsync of
// directories and on os.Root for every access below the Run directory.
//
// The package never removes or truncates anything on its own (R-PER-10). The
// single exception is the recovery of an incomplete trailing journal record
// (journal.go): the torn bytes are preserved under logs/ before the journal
// is cut back. Temporary files are created with O_EXCL and, if a crash or an
// error orphans one, left in place; their names start with a dot, so no
// artifact reference can ever name them.
//
// Only internal/engine imports this package, so that only the engine can
// commit events and write artifacts (R-FSM-03).
package storage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/shutx-net/bellona/internal/domain"
)

// ErrNoStateRoot is returned by StateRoot when neither XDG_STATE_HOME nor
// HOME holds an absolute path.
var ErrNoStateRoot = errors.New("cannot determine the state directory")

// StateRoot returns the directory that holds Bellona's per-Run files:
// $XDG_STATE_HOME/bellona. XDG_STATE_HOME is used when it is set to an
// absolute path (the XDG Base Directory specification says that a relative
// value is invalid and must be ignored). Otherwise the base is
// $HOME/.local/state, which needs an absolute HOME. Without either, the
// error wraps ErrNoStateRoot. getenv looks up an environment variable; a nil
// getenv means os.Getenv.
func StateRoot(getenv func(string) string) (string, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	if xdg := getenv("XDG_STATE_HOME"); xdg != "" && filepath.IsAbs(xdg) {
		return filepath.Join(xdg, "bellona"), nil
	}
	if home := getenv("HOME"); home != "" && filepath.IsAbs(home) {
		return filepath.Join(home, ".local", "state", "bellona"), nil
	}
	return "", fmt.Errorf("%w: set XDG_STATE_HOME or HOME to an absolute path", ErrNoStateRoot)
}

// RunsDir returns the directory that holds one subdirectory per Run.
func RunsDir(stateRoot string) string { return filepath.Join(stateRoot, "runs") }

// RunDir returns the directory of the Run id: <stateRoot>/runs/<id>.
func RunDir(stateRoot string, id domain.RunID) string {
	return filepath.Join(RunsDir(stateRoot), string(id))
}
