// Package engine is the engine core of Bellona: the one component that commits
// events (SPEC §6, invariant 9). It is a library with no terminal I/O and no
// daemon (R-CLI-04, SPEC §15.1): a caller creates a Run or opens an existing
// one and gets a *Run handle; the handle holds the Run lock until it is
// closed.
//
// # Single writer
//
// Every open Run has one control loop, a goroutine that owns the store, the
// projected state and the bookkeeping of running Attempts (invariants 9 and
// 14, SPEC §10). Callers and workers reach it only through channels. A commit
// goes through one path:
//
//  1. the proposed event is built, encoded, decoded again and applied to the
//     projected state by the reducer; an event that the reducer rejects
//     (fsm.ErrIllegalTransition or projection.ErrInvalidEvent) is returned
//     as an error and nothing is written (R-FSM-02);
//  2. the artifacts that the event refers to are finalized (temporary file,
//     fsync, rename, directory fsync);
//  3. the event is appended and fsynced;
//  4. the snapshot is replaced.
//
// That is the order of R-PER-04. The event that is applied in memory is the
// event as it was encoded and decoded, so the live state and a replay of the
// journal cannot differ. A storage failure in steps 2 to 4 stops the Run
// handle (ErrRunFailed): the files are left as the failure left them and the
// next Open recovers.
//
// # Workers
//
// An Attempt runs in a worker goroutine. A worker never touches the store or
// the state: it reports the launch and the result of the execution to the
// control loop through a channel, and the loop commits AttemptStarted,
// AttemptFinished, AttemptFailed or AttemptOutcomeUnknown (SPEC §10). The
// loop commits AttemptScheduled before it calls the runtime, so the intent to
// launch is durable before the launch (SPEC §12, Crash recovery).
//
// # Resume
//
// Open takes the Run lock, recovers an incomplete journal tail, checks the
// recorded contract versions, replays the journal, verifies every artifact
// reference and repairs the snapshot. It never calls the runtime: replay has
// no side effects (invariant 13), and Attempts whose outcome is uncertain are
// reported, not rerun (invariant 11). Reconciliation is M3.
//
// # Scope of M0
//
// The engine decides no transition on its own. It commits what a caller
// proposes in package tests and the outcome of the Attempts that it started.
// The workflow driver, condition guards, verification, worktrees, timeouts,
// staged cancellation, retries and reconciliation belong to M1a and later.
package engine

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/shutx-net/bellona/internal/agent"
	"github.com/shutx-net/bellona/internal/manifest"
	"github.com/shutx-net/bellona/internal/storage"
)

// Sentinel errors. Errors from the reducer match fsm.ErrIllegalTransition or
// projection.ErrInvalidEvent, and errors from storage match the sentinels of
// package storage; the engine adds the following.
var (
	// ErrClosed: the Run handle is closed or closing.
	ErrClosed = errors.New("engine: the Run is closed")
	// ErrRunFailed: a storage failure stopped the Run handle. Every later
	// call returns an error that wraps ErrRunFailed and the cause. Close the
	// handle and open the Run again to recover.
	ErrRunFailed = errors.New("engine: the Run handle stopped after a storage failure; close it and open the Run again")
	// ErrNoRuntime: StartAttempt was called on an engine without a Runtime.
	ErrNoRuntime = errors.New("engine: no agent runtime is configured")
	// ErrNoOutputContract: the role has no output contract in M0. Only the
	// Implementer has one; plans, reviews and decisions arrive in M2 and M3.
	ErrNoOutputContract = errors.New("engine: the role has no output contract")
	// ErrIncompleteRun: the Run directory has fewer than the two events that
	// every Run starts with (RunCreated and ManifestFrozen), for example
	// after a failure during Create. A Run directory without events.jsonl is
	// reported this way too.
	ErrIncompleteRun = errors.New("engine: the Run is incomplete")
	// ErrInconsistentRun: the frozen files of the Run disagree with the
	// journal, for example the policy limits with ManifestFrozen.
	ErrInconsistentRun = errors.New("engine: the Run files are inconsistent")
	// ErrSnapshotAhead: state.json records an event that the journal does
	// not hold, so committed records are missing from the journal.
	ErrSnapshotAhead = errors.New("engine: the snapshot is ahead of the journal")
	// ErrArtifactRefMismatch: a payload refers to an artifact that no file
	// of the commit provides and that no earlier event registered, or a file
	// of the commit conflicts with a registered artifact.
	ErrArtifactRefMismatch = errors.New("engine: the event refers to an artifact that is not backed by a file")
	// ErrUnknownAttempt: AwaitAttempt named an Attempt that the Run does not
	// have.
	ErrUnknownAttempt = errors.New("engine: unknown Attempt")
	// ErrIncompatible: the Run was recorded under contracts that this engine
	// does not support (manifest.ErrIncompatible). Open refuses it.
	ErrIncompatible = manifest.ErrIncompatible
)

// Options configures an Engine.
type Options struct {
	// StateRoot is the directory that holds the per-Run directories, the
	// value of storage.StateRoot: a Run lives in <StateRoot>/runs/<run-id>.
	// The caller resolves XDG_STATE_HOME. It must be an absolute path.
	StateRoot string
	// Runtime starts the agents. It may be nil when no Attempt is started.
	Runtime agent.Runtime
	// Clock gives the time of events. The default is time.Now. It is called
	// from the control loop of each open Run, so it must be safe for
	// concurrent use when several Runs are open.
	Clock func() time.Time
	// Rand is the source of Run, Attempt and session IDs. The default is
	// crypto/rand.Reader. The engine serializes the reads, so a source that is
	// not safe for concurrent use is fine.
	Rand io.Reader
	// Logger receives the engine's log records. The default discards them.
	Logger *slog.Logger
	// Faults stops storage operations at crash points. Tests only.
	Faults storage.FaultInjector

	// recordVersions, when set, replaces the contract versions that Create
	// records in the manifest. Tests only.
	recordVersions *manifest.Versions
	// supportedVersions, when set, replaces the contract versions that Open
	// supports. Tests only.
	supportedVersions *manifest.Versions
}

// Engine creates Runs and opens them again. It holds no per-Run state, so one
// Engine may serve many Runs, from several goroutines.
type Engine struct {
	stateRoot string
	runtime   agent.Runtime
	clock     func() time.Time
	rand      io.Reader
	logger    *slog.Logger
	faults    storage.FaultInjector

	recordVersions    *manifest.Versions
	supportedVersions *manifest.Versions
}

// New returns an Engine for the options. StateRoot must be absolute.
func New(opts Options) (*Engine, error) {
	if !filepath.IsAbs(opts.StateRoot) {
		return nil, fmt.Errorf("engine: the state root %q is not an absolute path", opts.StateRoot)
	}
	e := &Engine{
		stateRoot:         filepath.Clean(opts.StateRoot),
		runtime:           opts.Runtime,
		clock:             opts.Clock,
		rand:              opts.Rand,
		logger:            opts.Logger,
		faults:            opts.Faults,
		recordVersions:    opts.recordVersions,
		supportedVersions: opts.supportedVersions,
	}
	if e.clock == nil {
		e.clock = time.Now
	}
	if e.rand == nil {
		e.rand = rand.Reader
	}
	e.rand = &lockedReader{r: e.rand}
	if e.logger == nil {
		e.logger = slog.New(slog.DiscardHandler)
	}
	return e, nil
}

// lockedReader serializes reads from a source that may not be safe for
// concurrent use.
type lockedReader struct {
	mu sync.Mutex
	r  io.Reader
}

func (l *lockedReader) Read(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.r.Read(p)
}
