package storage

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/shutx-net/bellona/internal/domain"
)

const (
	// DefaultMaxArtifactSize is the largest artifact that WriteArtifact and
	// ReadArtifact accept when Options.MaxArtifactSize is zero.
	DefaultMaxArtifactSize = 16 << 20
	// MaxSnapshotSize is the largest state.json that WriteSnapshot and
	// ReadSnapshot accept.
	MaxSnapshotSize = 64 << 20
)

// Sentinel errors. Errors that concern one file are *PathError values that
// wrap one of them (or an operating-system error); test with errors.Is.
var (
	// ErrLocked: another Store, in this process or in another one, holds the
	// lock of the Run.
	ErrLocked = errors.New("run is locked by another store or process")
	// ErrRunExists: CreateRun found the Run directory already there.
	ErrRunExists = errors.New("run directory already exists")
	// ErrRunNotFound: OpenRun found no Run directory.
	ErrRunNotFound = errors.New("run directory not found")
	// ErrStoreDead: an injected crash or a failed journal write stopped the
	// Store. It refuses every call until Abandon releases the lock.
	ErrStoreDead = errors.New("store is dead after a crash or a failed write; abandon it and reopen the Run")
	// ErrStoreClosed: the Store was closed or abandoned.
	ErrStoreClosed = errors.New("store is closed")
	// ErrJournalNotLoaded: AppendEvent on a Store that was opened with
	// OpenRun and has not loaded the journal successfully yet.
	ErrJournalNotLoaded = errors.New("journal is not loaded; call LoadJournal first")
	// ErrReservedPath: the path names a file or directory that the engine
	// owns (events.jsonl, state.json, run.lock, logs/..., worktrees/...).
	ErrReservedPath = errors.New("reserved path")
	// ErrArtifactExists: a different artifact already exists at the path.
	ErrArtifactExists = errors.New("an artifact already exists at the path")
	// ErrArtifactMissing: the artifact file does not exist. The error also
	// matches fs.ErrNotExist.
	ErrArtifactMissing = errors.New("artifact is missing")
	// ErrArtifactMismatch: the size or the SHA-256 of the file differs from
	// the reference.
	ErrArtifactMismatch = errors.New("artifact does not match its reference")
	// ErrArtifactTooLarge: the data or the file is above the size limit.
	ErrArtifactTooLarge = errors.New("artifact is too large")
	// ErrSymlink: a symbolic link was found where a real file or directory
	// is required, at any component of the path, even when it points inside
	// the Run directory.
	ErrSymlink = errors.New("symbolic link in the path")
	// ErrNotRegular: the file is not a regular file.
	ErrNotRegular = errors.New("not a regular file")
	// ErrJournalCorrupt: events.jsonl has a malformed, out-of-sequence or
	// foreign record that is not the incomplete trailing record.
	ErrJournalCorrupt = errors.New("event journal is corrupt")
	// ErrInjectedCrash: a FaultInjector stopped the operation at a crash
	// point.
	ErrInjectedCrash = errors.New("injected crash")
	// ErrInvalidRecord: AppendEvent was given an empty record, a record with
	// a newline in it, or a record above domain.MaxEventSize (which also
	// matches domain.ErrEventTooLarge).
	ErrInvalidRecord = errors.New("invalid event record")
)

// PathError is the error for a file of the Run. Path is relative to the Run
// directory, with slashes.
type PathError struct {
	Path string
	Err  error
}

func (e *PathError) Error() string { return "storage: " + e.Path + ": " + e.Err.Error() }
func (e *PathError) Unwrap() error { return e.Err }

// Options configures a Store. The zero value is the production setting.
type Options struct {
	// Faults, when non-nil, is asked at every crash point. Tests only.
	Faults FaultInjector
	// MaxArtifactSize is the size limit for one artifact in bytes; zero
	// means DefaultMaxArtifactSize.
	MaxArtifactSize int64
}

// Store is the open, locked directory of one Run. It holds the Run lock from
// CreateRun or OpenRun until Close or Abandon, so at most one Store per Run
// exists across all processes. Its methods may be called from several
// goroutines; writes are serialized.
//
// A Store never deletes anything. After an injected crash or a failed write
// of the journal it is dead: every call returns ErrStoreDead until it is
// closed or abandoned.
type Store struct {
	mu sync.RWMutex // guards the flags below; writes take the write lock

	dir     string
	runID   domain.RunID
	root    *os.Root
	lock    *os.File
	journal *os.File
	opts    Options

	loaded bool // the journal was loaded or created empty in this session
	dead   bool
	closed bool
}

// runIDOf returns the Run ID that the last element of runDir spells.
func runIDOf(runDir string) (domain.RunID, error) {
	id, err := domain.ParseRunID(filepath.Base(runDir))
	if err != nil {
		return "", fmt.Errorf("storage: the name of the Run directory %q is not a Run ID: %w", runDir, err)
	}
	return id, nil
}

// CreateRun makes the directory runDir for a new Run, locks it and creates
// its empty journal. The last element of runDir must be a valid Run ID. The
// parent directories are created as needed (mode 0700). It fails with
// ErrRunExists when runDir is already there. The journal counts as loaded.
func CreateRun(runDir string, opts Options) (*Store, error) {
	runDir = filepath.Clean(runDir)
	id, err := runIDOf(runDir)
	if err != nil {
		return nil, err
	}
	parent := filepath.Dir(runDir)
	if err := mkdirAllSync(parent); err != nil {
		return nil, fmt.Errorf("storage: create %s: %w", parent, err)
	}
	if err := os.Mkdir(runDir, 0o700); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("%w: %s", ErrRunExists, runDir)
		}
		return nil, fmt.Errorf("storage: create %s: %w", runDir, err)
	}
	root, err := os.OpenRoot(runDir)
	if err != nil {
		return nil, fmt.Errorf("storage: open %s: %w", runDir, err)
	}
	s := &Store{dir: runDir, runID: id, root: root, opts: opts}
	if err := s.acquire(true); err != nil {
		s.release()
		return nil, err
	}
	s.loaded = true
	if err := s.fsyncDir("."); err != nil {
		s.release()
		return nil, fmt.Errorf("storage: sync %s: %w", runDir, err)
	}
	if err := fsyncPath(parent); err != nil {
		s.release()
		return nil, fmt.Errorf("storage: sync %s: %w", parent, err)
	}
	return s, nil
}

// OpenRun locks an existing Run directory and opens its journal for
// appending. The last element of runDir must be a valid Run ID. It fails
// with ErrRunNotFound when the directory is missing and with ErrLocked when
// another Store holds the Run. A Run directory without events.jsonl is not a
// Run that CreateRun made: OpenRun fails with a *PathError for events.jsonl
// that matches fs.ErrNotExist and creates nothing, so a refused Open leaves
// the directory as it was. The journal is not read: call LoadJournal before
// AppendEvent.
func OpenRun(runDir string, opts Options) (*Store, error) {
	runDir = filepath.Clean(runDir)
	id, err := runIDOf(runDir)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(runDir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("%w: %s", ErrRunNotFound, runDir)
	case err != nil:
		return nil, fmt.Errorf("storage: open %s: %w", runDir, err)
	case !fi.IsDir():
		return nil, fmt.Errorf("%w: %s is not a directory", ErrRunNotFound, runDir)
	}
	root, err := os.OpenRoot(runDir)
	if err != nil {
		return nil, fmt.Errorf("storage: open %s: %w", runDir, err)
	}
	s := &Store{dir: runDir, runID: id, root: root, opts: opts}
	if err := s.acquire(false); err != nil {
		s.release()
		return nil, err
	}
	return s, nil
}

// acquire takes the Run lock and opens the journal. When create is true (a new
// Run) the journal must not exist and is created; otherwise it must exist and
// is never created. Neither file may be a symbolic link or a non-regular file.
// The caller releases the Store on error.
func (s *Store) acquire(create bool) error {
	journalFlags := os.O_RDWR | os.O_APPEND
	if create {
		journalFlags |= os.O_CREATE | os.O_EXCL
	} else if _, err := s.root.Lstat(domain.EventsPath); errors.Is(err, fs.ErrNotExist) {
		// Look before the lock file is created, which a Run directory without
		// a journal may lack as well: refusing it must not change it.
		return &PathError{Path: domain.EventsPath, Err: err}
	}
	lock, err := s.openOwned(domain.LockPath, os.O_RDWR|os.O_CREATE)
	if err != nil {
		return err
	}
	s.lock = lock
	if err := flockExclusive(lock); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return fmt.Errorf("%w: %s", ErrLocked, s.dir)
		}
		return &PathError{Path: domain.LockPath, Err: err}
	}
	journal, err := s.openOwned(domain.EventsPath, journalFlags)
	if err != nil {
		return err
	}
	s.journal = journal
	return nil
}

// openOwned opens a file that sits directly in the Run directory and that
// the engine owns, after checking that it is not a symbolic link and, when it
// exists, that it is a regular file.
func (s *Store) openOwned(name string, flag int) (*os.File, error) {
	if fi, err := s.root.Lstat(name); err == nil {
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			return nil, &PathError{Path: name, Err: ErrSymlink}
		case !fi.Mode().IsRegular():
			return nil, &PathError{Path: name, Err: ErrNotRegular}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, &PathError{Path: name, Err: err}
	}
	f, err := s.root.OpenFile(name, flag, 0o600)
	if err != nil {
		return nil, &PathError{Path: name, Err: err}
	}
	return f, nil
}

// flockExclusive takes an exclusive, non-blocking flock on f. The lock
// belongs to the open file description, so it is released when f is closed
// and a second open of the same file conflicts even inside this process.
func flockExclusive(f *os.File) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var lockErr error
	if err := rc.Control(func(fd uintptr) {
		for {
			lockErr = syscall.Flock(int(fd), syscall.LOCK_EX|syscall.LOCK_NB)
			if lockErr != syscall.EINTR {
				return
			}
		}
	}); err != nil {
		return err
	}
	return lockErr
}

// RunID returns the Run ID that the Run directory is named after.
func (s *Store) RunID() domain.RunID { return s.runID }

// Dir returns the path of the Run directory as it was given to CreateRun or
// OpenRun, cleaned.
func (s *Store) Dir() string { return s.dir }

// Close closes the journal, the lock file, which releases the Run lock, and
// the Run directory. It writes nothing, so on a dead Store it behaves like
// Abandon. Closing a closed Store does nothing.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	return s.release()
}

// Abandon closes every descriptor without writing anything and releases the
// Run lock. Tests use it to leave a crashed Store behind and reopen the Run.
// Abandoning a closed Store does nothing.
func (s *Store) Abandon() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		_ = s.release()
	}
}

// release closes whatever is open and marks the Store closed. The caller
// holds the write lock or owns the Store exclusively.
func (s *Store) release() error {
	s.closed = true
	var errs []error
	if s.journal != nil {
		errs = append(errs, s.journal.Close())
	}
	if s.lock != nil {
		errs = append(errs, s.lock.Close())
	}
	if s.root != nil {
		errs = append(errs, s.root.Close())
	}
	return errors.Join(errs...)
}

// usable returns the error that a call on an unusable Store reports. The
// caller holds the lock.
func (s *Store) usable() error {
	switch {
	case s.closed:
		return ErrStoreClosed
	case s.dead:
		return ErrStoreDead
	}
	return nil
}

// fire asks the fault injector about the crash point p. When it fires, the
// Store is dead and the error wraps ErrInjectedCrash. The caller holds the
// write lock.
func (s *Store) fire(p CrashPoint) error {
	if p == 0 || s.opts.Faults == nil || !s.opts.Faults(p) {
		return nil
	}
	s.dead = true
	return fmt.Errorf("%w at %s", ErrInjectedCrash, p)
}

// maxArtifactSize returns the effective artifact size limit.
func (s *Store) maxArtifactSize() int64 {
	if s.opts.MaxArtifactSize > 0 {
		return s.opts.MaxArtifactSize
	}
	return DefaultMaxArtifactSize
}
