package storage

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/shutx-net/bellona/internal/domain"
)

// testRunID is the Run ID of every test Run.
const testRunID domain.RunID = "run-0123456789abcdef"

// newRunDir returns the Run directory path of a Run that does not exist yet,
// below a fresh temporary state root. The tests never touch the real state
// directory.
func newRunDir(t *testing.T) string {
	t.Helper()
	return RunDir(t.TempDir(), testRunID)
}

// newStore creates a Run in a temporary state root.
func newStore(t *testing.T, opts Options) *Store {
	t.Helper()
	s, err := CreateRun(newRunDir(t), opts)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	t.Cleanup(s.Abandon)
	return s
}

// reopen opens the Run directory again. The caller has closed or abandoned
// the earlier Store.
func reopen(t *testing.T, dir string, opts Options) *Store {
	t.Helper()
	s, err := OpenRun(dir, opts)
	if err != nil {
		t.Fatalf("OpenRun: %v", err)
	}
	t.Cleanup(s.Abandon)
	return s
}

// fileSet describes every path below root: "dir", "link" or "file:<size>".
func fileSet(t *testing.T, root string) map[string]string {
	t.Helper()
	set := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			set[rel] = "dir"
		case d.Type()&fs.ModeSymlink != 0:
			set[rel] = "link"
		case d.Type().IsRegular():
			fi, err := d.Info()
			if err != nil {
				return err
			}
			set[rel] = fmt.Sprintf("file:%d", fi.Size())
		default:
			set[rel] = "other"
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return set
}

// assertNothingDisappeared fails when a path of before is absent from after.
func assertNothingDisappeared(t *testing.T, before, after map[string]string) {
	t.Helper()
	for p := range before {
		if _, ok := after[p]; !ok {
			t.Errorf("path %q disappeared", p)
		}
	}
}

// assertNoTempFiles fails when a temporary file (a name that starts with a
// dot) exists anywhere below dir.
func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	for p := range fileSet(t, dir) {
		if p != "." && strings.HasPrefix(filepath.Base(p), ".") {
			t.Errorf("leftover temporary file %q", p)
		}
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustWrite(t *testing.T, p string, b []byte) {
	t.Helper()
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func wantMode(t *testing.T, p string, want fs.FileMode) {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != want {
		t.Errorf("%s has mode %o, want %o", p, got, want)
	}
}

func TestStateRoot(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	tests := []struct {
		name string
		env  map[string]string
		want string
		fail bool
	}{
		{"absolute XDG_STATE_HOME", map[string]string{"XDG_STATE_HOME": "/xdg/state", "HOME": "/home/u"}, "/xdg/state/bellona", false},
		{"XDG_STATE_HOME with trailing slash", map[string]string{"XDG_STATE_HOME": "/xdg/state/"}, "/xdg/state/bellona", false},
		{"relative XDG_STATE_HOME is ignored", map[string]string{"XDG_STATE_HOME": "rel/state", "HOME": "/home/u"}, "/home/u/.local/state/bellona", false},
		{"empty XDG_STATE_HOME falls back to HOME", map[string]string{"XDG_STATE_HOME": "", "HOME": "/home/u"}, "/home/u/.local/state/bellona", false},
		{"XDG_STATE_HOME unset", map[string]string{"HOME": "/home/u"}, "/home/u/.local/state/bellona", false},
		{"relative XDG_STATE_HOME and no HOME", map[string]string{"XDG_STATE_HOME": "rel"}, "", true},
		{"neither is set", map[string]string{}, "", true},
		{"relative HOME", map[string]string{"HOME": "home/u"}, "", true},
		{"empty HOME", map[string]string{"HOME": ""}, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := StateRoot(env(tt.env))
			if tt.fail {
				if !errors.Is(err, ErrNoStateRoot) {
					t.Fatalf("StateRoot = %q, %v; want an error wrapping ErrNoStateRoot", got, err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("StateRoot = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

func TestStateRootFromProcessEnvironment(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_STATE_HOME", xdg)
	want := filepath.Join(xdg, "bellona")
	for _, getenv := range []func(string) string{os.Getenv, nil} {
		got, err := StateRoot(getenv)
		if err != nil || got != want {
			t.Errorf("StateRoot = %q, %v; want %q", got, err, want)
		}
	}
}

func TestRunDirLayout(t *testing.T) {
	if got, want := RunsDir("/s"), "/s/runs"; got != want {
		t.Errorf("RunsDir = %q, want %q", got, want)
	}
	if got, want := RunDir("/s", testRunID), "/s/runs/"+string(testRunID); got != want {
		t.Errorf("RunDir = %q, want %q", got, want)
	}
}

func TestCreateRunLayoutAndPermissions(t *testing.T) {
	stateRoot := filepath.Join(t.TempDir(), "bellona")
	dir := RunDir(stateRoot, testRunID)
	s, err := CreateRun(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.RunID() != testRunID || s.Dir() != dir {
		t.Errorf("RunID %q, Dir %q", s.RunID(), s.Dir())
	}
	// Directories 0700, files 0600, parents that CreateRun made included.
	wantMode(t, stateRoot, 0o700)
	wantMode(t, RunsDir(stateRoot), 0o700)
	wantMode(t, dir, 0o700)
	wantMode(t, filepath.Join(dir, domain.LockPath), 0o600)
	wantMode(t, filepath.Join(dir, domain.EventsPath), 0o600)
	if got := fileSet(t, dir); len(got) != 3 {
		t.Errorf("a new Run holds %v, want only run.lock and events.jsonl", got)
	}
	if b := mustRead(t, filepath.Join(dir, domain.EventsPath)); len(b) != 0 {
		t.Errorf("the new journal holds %d bytes", len(b))
	}
	// The journal counts as loaded, empty.
	j, err := s.LoadJournal()
	if err != nil || len(j.Events) != 0 || j.Recovered != nil {
		t.Errorf("LoadJournal = %+v, %v", j, err)
	}

	// Files made later get the same modes.
	if _, err := s.WriteArtifact("plans/rev-0001/plan.json", []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteSnapshot([]byte("{}")); err != nil {
		t.Fatal(err)
	}
	wantMode(t, filepath.Join(dir, "plans"), 0o700)
	wantMode(t, filepath.Join(dir, "plans/rev-0001"), 0o700)
	wantMode(t, filepath.Join(dir, "plans/rev-0001/plan.json"), 0o600)
	wantMode(t, filepath.Join(dir, domain.SnapshotPath), 0o600)
}

func TestCreateRunErrors(t *testing.T) {
	t.Run("exists", func(t *testing.T) {
		s := newStore(t, Options{})
		_, err := CreateRun(s.Dir(), Options{})
		if !errors.Is(err, ErrRunExists) {
			t.Fatalf("CreateRun on an existing Run: %v, want ErrRunExists", err)
		}
		// The failed call did not disturb the Store that holds the Run.
		if _, err := s.WriteArtifact("a.json", []byte("x")); err != nil {
			t.Errorf("the first Store broke: %v", err)
		}
		if _, err := OpenRun(s.Dir(), Options{}); !errors.Is(err, ErrLocked) {
			t.Errorf("OpenRun: %v, want ErrLocked", err)
		}
	})
	t.Run("invalid directory name", func(t *testing.T) {
		for _, name := range []string{"run-1", "RUN-0123456789abcdef", "run-0123456789abcdeF", "plain", "run-0123456789abcdef0"} {
			dir := filepath.Join(t.TempDir(), "runs", name)
			if _, err := CreateRun(dir, Options{}); !errors.Is(err, domain.ErrInvalidID) {
				t.Errorf("CreateRun(%q): %v, want ErrInvalidID", name, err)
			}
			if _, err := os.Lstat(dir); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("CreateRun(%q) left %s behind: %v", name, dir, err)
			}
		}
	})
	t.Run("parent is a file", func(t *testing.T) {
		base := t.TempDir()
		mustWrite(t, filepath.Join(base, "runs"), []byte("x"))
		if _, err := CreateRun(filepath.Join(base, "runs", string(testRunID)), Options{}); !errors.Is(err, syscall.ENOTDIR) {
			t.Errorf("CreateRun below a file: %v, want ENOTDIR", err)
		}
	})
}

func TestOpenRunErrors(t *testing.T) {
	t.Run("not found", func(t *testing.T) {
		_, err := OpenRun(newRunDir(t), Options{})
		if !errors.Is(err, ErrRunNotFound) {
			t.Fatalf("OpenRun: %v, want ErrRunNotFound", err)
		}
	})
	t.Run("a file is not a Run", func(t *testing.T) {
		dir := newRunDir(t)
		if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
			t.Fatal(err)
		}
		mustWrite(t, dir, []byte("x"))
		if _, err := OpenRun(dir, Options{}); !errors.Is(err, ErrRunNotFound) {
			t.Fatalf("OpenRun on a file: %v, want ErrRunNotFound", err)
		}
	})
	t.Run("invalid directory name", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "not-a-run")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenRun(dir, Options{}); !errors.Is(err, domain.ErrInvalidID) {
			t.Fatalf("OpenRun: %v, want ErrInvalidID", err)
		}
	})
	t.Run("trailing slash", func(t *testing.T) {
		s := newStore(t, Options{})
		dir := s.Dir()
		s.Close()
		s2 := reopen(t, dir+"/", Options{})
		if s2.RunID() != testRunID {
			t.Errorf("RunID %q", s2.RunID())
		}
	})
}

// TestOpenRunMissingJournalIsNotExist: a Run directory without events.jsonl was
// not made by CreateRun (or lost its journal), so OpenRun refuses it and
// changes nothing: no journal is created, not even the lock file, and the lock
// is free afterwards.
func TestOpenRunMissingJournalIsNotExist(t *testing.T) {
	// contents maps every regular file below dir to its bytes.
	contents := func(t *testing.T, dir string) map[string]string {
		t.Helper()
		out := map[string]string{}
		for p, kind := range fileSet(t, dir) {
			if strings.HasPrefix(kind, "file:") {
				out[p] = string(mustRead(t, filepath.Join(dir, p)))
			}
		}
		return out
	}
	check := func(t *testing.T, dir string) {
		t.Helper()
		before, beforeBytes := fileSet(t, dir), contents(t, dir)
		for range 2 { // the second open fails the same way, not with ErrLocked
			_, err := OpenRun(dir, Options{})
			var pe *PathError
			switch {
			case err == nil:
				t.Fatal("OpenRun of a Run directory without a journal succeeded")
			case !errors.Is(err, fs.ErrNotExist):
				t.Fatalf("OpenRun: %v, want an error that matches fs.ErrNotExist", err)
			case !errors.As(err, &pe) || pe.Path != domain.EventsPath:
				t.Fatalf("OpenRun: %v, want a *PathError for %s", err, domain.EventsPath)
			case errors.Is(err, ErrLocked) || errors.Is(err, ErrRunNotFound):
				t.Fatalf("OpenRun: %v is a lock or a Run-not-found error", err)
			}
		}
		if after := fileSet(t, dir); !reflect.DeepEqual(before, after) {
			t.Errorf("a refused OpenRun changed the Run directory:\n before %v\n after  %v", before, after)
		}
		if after := contents(t, dir); !reflect.DeepEqual(beforeBytes, after) {
			t.Errorf("a refused OpenRun changed the bytes of a file:\n before %q\n after  %q", beforeBytes, after)
		}
		if _, err := os.Lstat(filepath.Join(dir, domain.EventsPath)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("events.jsonl exists after the refusal: %v", err)
		}
	}

	t.Run("a Run whose journal was removed", func(t *testing.T) {
		s := newStore(t, Options{})
		dir := s.Dir()
		appendAll(t, s, 2)
		s.Close()
		if err := os.Remove(filepath.Join(dir, domain.EventsPath)); err != nil {
			t.Fatal(err)
		}
		check(t, dir)
	})
	t.Run("an empty Run directory", func(t *testing.T) {
		// No lock file either: nothing may be created for a refused Open.
		dir := newRunDir(t)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		check(t, dir)
		if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
			t.Errorf("the empty Run directory holds %v (%v) after the refusal", entries, err)
		}
	})
}

func TestOpenRunSymlinkedOwnedFiles(t *testing.T) {
	for _, name := range []string{domain.LockPath, domain.EventsPath} {
		t.Run(name, func(t *testing.T) {
			s := newStore(t, Options{})
			dir := s.Dir()
			s.Close()
			other := filepath.Join(t.TempDir(), "other")
			mustWrite(t, other, nil)
			if err := os.Remove(filepath.Join(dir, name)); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(other, filepath.Join(dir, name)); err != nil {
				t.Fatal(err)
			}
			if _, err := OpenRun(dir, Options{}); !errors.Is(err, ErrSymlink) {
				t.Fatalf("OpenRun with a symlinked %s: %v, want ErrSymlink", name, err)
			}
			// The failed open released everything it took.
			if err := os.Remove(filepath.Join(dir, name)); err != nil {
				t.Fatal(err)
			}
			if name == domain.EventsPath {
				// OpenRun creates no journal: put a regular file where the link was.
				// A missing lock file, in contrast, is created by the reopen.
				mustWrite(t, filepath.Join(dir, name), nil)
			}
			reopen(t, dir, Options{})
		})
	}
}

func TestOpenRunNonRegularOwnedFile(t *testing.T) {
	s := newStore(t, Options{})
	dir := s.Dir()
	s.Close()
	p := filepath.Join(dir, domain.EventsPath)
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenRun(dir, Options{}); !errors.Is(err, ErrNotRegular) {
		t.Fatalf("OpenRun with a FIFO as journal: %v, want ErrNotRegular", err)
	}
}

func TestLockSecondOpenInSameProcess(t *testing.T) {
	s := newStore(t, Options{})
	dir := s.Dir()
	for range 3 {
		if _, err := OpenRun(dir, Options{}); !errors.Is(err, ErrLocked) {
			t.Fatalf("second OpenRun: %v, want ErrLocked", err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := OpenRun(dir, Options{})
	if err != nil {
		t.Fatalf("OpenRun after Close: %v", err)
	}
	if _, err := OpenRun(dir, Options{}); !errors.Is(err, ErrLocked) {
		t.Fatalf("OpenRun while the second Store is open: %v, want ErrLocked", err)
	}
	s2.Abandon()
	s3, err := OpenRun(dir, Options{})
	if err != nil {
		t.Fatalf("OpenRun after Abandon: %v", err)
	}
	s3.Close()
}

// lockHelperEnv carries the Run directory to the child process of
// TestLockAcrossProcesses.
const lockHelperEnv = "BELLONA_STORAGE_LOCK_HELPER_DIR"

// TestLockHelperProcess is the child of TestLockAcrossProcesses. Run on its
// own it does nothing.
func TestLockHelperProcess(t *testing.T) {
	dir := os.Getenv(lockHelperEnv)
	if dir == "" {
		return
	}
	s, err := OpenRun(dir, Options{})
	switch {
	case err == nil:
		fmt.Println("LOCK-HELPER-RESULT: acquired")
		s.Close()
	case errors.Is(err, ErrLocked):
		fmt.Println("LOCK-HELPER-RESULT: locked")
	default:
		fmt.Println("LOCK-HELPER-RESULT: error:", err)
	}
}

// helperOpens runs the test binary as a child process that tries to open the
// Run and returns what it reports.
func helperOpens(t *testing.T, dir string) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestLockHelperProcess$", "-test.count=1")
	cmd.Env = append(os.Environ(), lockHelperEnv+"="+dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the helper process failed: %v\n%s", err, out)
	}
	for line := range strings.Lines(string(out)) {
		if rest, ok := strings.CutPrefix(line, "LOCK-HELPER-RESULT: "); ok {
			return strings.TrimSpace(rest)
		}
	}
	t.Fatalf("the helper process reported nothing:\n%s", out)
	return ""
}

func TestLockAcrossProcesses(t *testing.T) {
	s := newStore(t, Options{})
	dir := s.Dir()
	if got := helperOpens(t, dir); got != "locked" {
		t.Fatalf("while the parent holds the Store, the child reported %q, want locked", got)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if got := helperOpens(t, dir); got != "acquired" {
		t.Fatalf("after Close the child reported %q, want acquired", got)
	}
	// And the other way around: the child has exited, so the parent can
	// lock again.
	reopen(t, dir, Options{})
}

func TestStoreClosedAndIdempotentClose(t *testing.T) {
	s := newStore(t, Options{})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	s.Abandon() // no effect
	if _, err := s.WriteArtifact("a.json", nil); !errors.Is(err, ErrStoreClosed) {
		t.Errorf("WriteArtifact: %v, want ErrStoreClosed", err)
	}
	if _, err := s.ReadArtifact(domain.ArtifactRef{}); !errors.Is(err, ErrStoreClosed) {
		t.Errorf("ReadArtifact: %v, want ErrStoreClosed", err)
	}
	if err := s.AppendEvent([]byte("{}")); !errors.Is(err, ErrStoreClosed) {
		t.Errorf("AppendEvent: %v, want ErrStoreClosed", err)
	}
	if _, err := s.LoadJournal(); !errors.Is(err, ErrStoreClosed) {
		t.Errorf("LoadJournal: %v, want ErrStoreClosed", err)
	}
	if err := s.WriteSnapshot(nil); !errors.Is(err, ErrStoreClosed) {
		t.Errorf("WriteSnapshot: %v, want ErrStoreClosed", err)
	}
	if _, err := s.ReadSnapshot(); !errors.Is(err, ErrStoreClosed) {
		t.Errorf("ReadSnapshot: %v, want ErrStoreClosed", err)
	}
}

func TestSnapshotRoundTripAndReplacement(t *testing.T) {
	s := newStore(t, Options{})
	dir := s.Dir()
	if _, err := s.ReadSnapshot(); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("ReadSnapshot without a snapshot: %v, want fs.ErrNotExist", err)
	}
	for _, body := range []string{"first\n", "second snapshot, longer\n", ""} {
		if err := s.WriteSnapshot([]byte(body)); err != nil {
			t.Fatal(err)
		}
		got, err := s.ReadSnapshot()
		if err != nil || string(got) != body {
			t.Fatalf("ReadSnapshot = %q, %v; want %q", got, err, body)
		}
	}
	// The replacement leaves no temporary file behind.
	assertNoTempFiles(t, dir)
	// The snapshot is no artifact: its path is reserved.
	if _, err := s.WriteArtifact(domain.SnapshotPath, []byte("x")); !errors.Is(err, ErrReservedPath) {
		t.Errorf("WriteArtifact(state.json): %v, want ErrReservedPath", err)
	}
}

func TestSnapshotReadChecks(t *testing.T) {
	s := newStore(t, Options{})
	p := filepath.Join(s.Dir(), domain.SnapshotPath)

	t.Run("symlink", func(t *testing.T) {
		other := filepath.Join(s.Dir(), "other")
		mustWrite(t, other, []byte("{}"))
		if err := os.Symlink("other", p); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ReadSnapshot(); !errors.Is(err, ErrSymlink) {
			t.Fatalf("ReadSnapshot: %v, want ErrSymlink", err)
		}
		// A snapshot is a cache: writing replaces the link, not its target.
		if err := s.WriteSnapshot([]byte("new")); err != nil {
			t.Fatal(err)
		}
		if got := mustRead(t, other); string(got) != "{}" {
			t.Errorf("the link target was modified: %q", got)
		}
		if got, err := s.ReadSnapshot(); err != nil || string(got) != "new" {
			t.Errorf("ReadSnapshot = %q, %v", got, err)
		}
	})
	t.Run("directory", func(t *testing.T) {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ReadSnapshot(); !errors.Is(err, ErrNotRegular) {
			t.Fatalf("ReadSnapshot: %v, want ErrNotRegular", err)
		}
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("too large", func(t *testing.T) {
		mustWrite(t, p, nil)
		if err := os.Truncate(p, MaxSnapshotSize+1); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ReadSnapshot(); !errors.Is(err, ErrArtifactTooLarge) {
			t.Fatalf("ReadSnapshot: %v, want ErrArtifactTooLarge", err)
		}
		if err := s.WriteSnapshot(make([]byte, MaxSnapshotSize+1)); !errors.Is(err, ErrArtifactTooLarge) {
			t.Fatalf("WriteSnapshot: %v, want ErrArtifactTooLarge", err)
		}
	})
}

// TestStoreConcurrentUse is meant for the race detector: reads and writes
// from several goroutines.
func TestStoreConcurrentUse(t *testing.T) {
	s := newStore(t, Options{})
	ref, err := s.WriteArtifact("shared.json", []byte("shared"))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				if _, err := s.ReadArtifact(ref); err != nil {
					t.Error(err)
					return
				}
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 10 {
				if _, err := s.WriteArtifact(fmt.Sprintf("w/%d-%d.json", i, j), []byte("x")); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if n := len(fileSet(t, filepath.Join(s.Dir(), "w"))); n != 41 { // the directory and 40 files
		t.Errorf("%d entries below w, want 41", n)
	}
}
