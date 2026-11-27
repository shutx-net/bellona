package storage

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/shutx-net/bellona/internal/domain"
)

func mustRef(t *testing.T, rel string, data []byte) domain.ArtifactRef {
	t.Helper()
	ref, err := domain.NewArtifactRef(rel, data)
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func TestArtifactRoundTrip(t *testing.T) {
	s := newStore(t, Options{})
	data := []byte(`{"revision":1}` + "\n")
	ref, err := s.WriteArtifact("plans/rev-0001/plan.json", data)
	if err != nil {
		t.Fatal(err)
	}
	if want := mustRef(t, "plans/rev-0001/plan.json", data); ref != want {
		t.Errorf("reference %+v, want %+v", ref, want)
	}
	if ref.Size != int64(len(data)) || len(ref.SHA256) != 64 || ref.Path != "plans/rev-0001/plan.json" {
		t.Errorf("reference fields: %+v", ref)
	}
	got, err := s.ReadArtifact(ref)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("ReadArtifact = %q, %v", got, err)
	}
	if err := s.VerifyArtifacts([]domain.ArtifactRef{ref}); err != nil {
		t.Errorf("VerifyArtifacts: %v", err)
	}
	// No temporary file is left behind, in any directory.
	assertNoTempFiles(t, s.Dir())
	// The bytes on disk are the data, unchanged.
	if disk := mustRead(t, filepath.Join(s.Dir(), "plans/rev-0001/plan.json")); !bytes.Equal(disk, data) {
		t.Errorf("file holds %q", disk)
	}
}

func TestArtifactEmptyAndLimitBoundary(t *testing.T) {
	s := newStore(t, Options{MaxArtifactSize: 8})
	ref, err := s.WriteArtifact("empty.json", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.ReadArtifact(ref); err != nil || len(got) != 0 {
		t.Fatalf("ReadArtifact(empty) = %q, %v", got, err)
	}
	ref, err = s.WriteArtifact("exact.json", []byte("12345678"))
	if err != nil {
		t.Fatalf("a file of exactly the limit: %v", err)
	}
	if _, err := s.ReadArtifact(ref); err != nil {
		t.Fatal(err)
	}
}

func TestArtifactIdenticalRewrite(t *testing.T) {
	s := newStore(t, Options{})
	data := []byte("same bytes")
	ref, err := s.WriteArtifact("a/b.json", data)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(s.Dir(), "a/b.json")
	before, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		again, err := s.WriteArtifact("a/b.json", data)
		if err != nil || again != ref {
			t.Fatalf("second write = %+v, %v; want %+v", again, err, ref)
		}
	}
	after, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
		t.Error("an identical rewrite replaced the file")
	}
}

func TestArtifactDifferentContentIsRefused(t *testing.T) {
	s := newStore(t, Options{})
	if _, err := s.WriteArtifact("a.json", []byte("first")); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(s.Dir(), "a.json")
	for _, other := range []string{"second", "firsT", "fir", "first!", ""} {
		_, err := s.WriteArtifact("a.json", []byte(other))
		if !errors.Is(err, ErrArtifactExists) {
			t.Errorf("rewrite with %q: %v, want ErrArtifactExists", other, err)
		}
	}
	if got := mustRead(t, p); string(got) != "first" {
		t.Errorf("the artifact was rewritten: %q", got)
	}
	// A directory at the path is not an artifact either.
	if err := os.Mkdir(filepath.Join(s.Dir(), "d.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteArtifact("d.json", []byte("x")); !errors.Is(err, ErrArtifactExists) {
		t.Errorf("write over a directory: %v, want ErrArtifactExists", err)
	}
	// The failed writes left nothing but the directories and files above.
	assertNoTempFiles(t, s.Dir())
}

func TestArtifactReservedPathsAreRefused(t *testing.T) {
	s := newStore(t, Options{})
	for _, p := range []string{"events.jsonl", "state.json", "run.lock", "logs/x", "logs/deep/x.json", "worktrees/x", "worktrees/a/b"} {
		if _, err := s.WriteArtifact(p, []byte("x")); !errors.Is(err, ErrReservedPath) {
			t.Errorf("WriteArtifact(%q): %v, want ErrReservedPath", p, err)
		}
		if _, err := s.ReadArtifact(mustRef(t, p, []byte("x"))); !errors.Is(err, ErrReservedPath) {
			t.Errorf("ReadArtifact(%q): %v, want ErrReservedPath", p, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(s.Dir(), "logs")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a refused write created logs/: %v", err)
	}
	// Names that merely look similar are fine.
	for _, p := range []string{"logs.json", "events.json", "worktrees-x/a", "state.json.bak"} {
		if _, err := s.WriteArtifact(p, []byte("x")); err != nil {
			t.Errorf("WriteArtifact(%q): %v", p, err)
		}
	}
}

func TestArtifactInvalidPathsAreRefused(t *testing.T) {
	s := newStore(t, Options{})
	before := fileSet(t, s.Dir())
	for _, p := range []string{"../x", "a/../../x", "/etc/passwd", "a//b", "a/./b", ".hidden", "a/.hidden", "a/", "", "a\\b", "a/..", "x\x00y", ".x.tmp-0011223344556677"} {
		if _, err := s.WriteArtifact(p, []byte("x")); !errors.Is(err, domain.ErrInvalidPath) {
			t.Errorf("WriteArtifact(%q): %v, want ErrInvalidPath", p, err)
		}
		// A reference cannot name such a path either.
		ref := domain.ArtifactRef{Path: p, Size: 1, SHA256: strings.Repeat("0", 64)}
		if _, err := s.ReadArtifact(ref); !errors.Is(err, domain.ErrInvalidPath) {
			t.Errorf("ReadArtifact(%q): %v, want ErrInvalidPath", p, err)
		}
	}
	if after := fileSet(t, s.Dir()); len(after) != len(before) {
		t.Errorf("refused writes changed the Run directory: %v -> %v", before, after)
	}
	// An invalid reference is refused before any file access.
	if _, err := s.ReadArtifact(domain.ArtifactRef{Path: "a.json", Size: -1, SHA256: strings.Repeat("0", 64)}); !errors.Is(err, domain.ErrInvalidArtifactRef) {
		t.Errorf("negative size: %v", err)
	}
	if _, err := s.ReadArtifact(domain.ArtifactRef{Path: "a.json", Size: 1, SHA256: "ABC"}); !errors.Is(err, domain.ErrInvalidArtifactRef) {
		t.Errorf("bad digest: %v", err)
	}
}

func TestArtifactOversizeWriteIsRefused(t *testing.T) {
	s := newStore(t, Options{MaxArtifactSize: 16})
	before := fileSet(t, s.Dir())
	_, err := s.WriteArtifact("big/a.json", bytes.Repeat([]byte("x"), 17))
	if !errors.Is(err, ErrArtifactTooLarge) {
		t.Fatalf("WriteArtifact: %v, want ErrArtifactTooLarge", err)
	}
	if after := fileSet(t, s.Dir()); len(after) != len(before) {
		t.Errorf("a refused write changed the Run directory: %v", after)
	}
	// The default limit applies without the option.
	d := newStore(t, Options{})
	if _, err := d.WriteArtifact("a.json", make([]byte, DefaultMaxArtifactSize+1)); !errors.Is(err, ErrArtifactTooLarge) {
		t.Errorf("default limit: %v, want ErrArtifactTooLarge", err)
	}
}

func TestArtifactReadMissing(t *testing.T) {
	s := newStore(t, Options{})
	if _, err := s.WriteArtifact("file.json", []byte("x")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"nope.json", "no/such/dir.json", "file.json/below.json"} {
		_, err := s.ReadArtifact(mustRef(t, p, []byte("x")))
		if !errors.Is(err, ErrArtifactMissing) || !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("ReadArtifact(%q): %v, want ErrArtifactMissing and fs.ErrNotExist", p, err)
		}
	}
	var pe *PathError
	_, err := s.ReadArtifact(mustRef(t, "nope.json", []byte("x")))
	if !errors.As(err, &pe) || pe.Path != "nope.json" {
		t.Errorf("error %v is not a *PathError for nope.json", err)
	}
	// VerifyArtifacts reports the first failure.
	ok := mustRef(t, "file.json", []byte("x"))
	if err := s.VerifyArtifacts([]domain.ArtifactRef{ok, mustRef(t, "nope.json", nil), mustRef(t, "other.json", nil)}); !errors.Is(err, ErrArtifactMissing) || !strings.Contains(err.Error(), "nope.json") {
		t.Errorf("VerifyArtifacts: %v", err)
	}
}

func TestArtifactReadSymlinkIsRejected(t *testing.T) {
	data := []byte("artifact body")

	t.Run("final component points at another artifact inside the Run", func(t *testing.T) {
		s := newStore(t, Options{})
		if _, err := s.WriteArtifact("a.json", data); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("a.json", filepath.Join(s.Dir(), "b.json")); err != nil {
			t.Fatal(err)
		}
		// The reference is right for the bytes behind the link.
		if _, err := s.ReadArtifact(mustRef(t, "b.json", data)); !errors.Is(err, ErrSymlink) {
			t.Fatalf("ReadArtifact: %v, want ErrSymlink", err)
		}
		if _, err := s.WriteArtifact("b.json", data); !errors.Is(err, ErrSymlink) {
			t.Fatalf("WriteArtifact over a link: %v, want ErrSymlink", err)
		}
	})
	t.Run("final component points outside the Run", func(t *testing.T) {
		s := newStore(t, Options{})
		outside := filepath.Join(t.TempDir(), "outside.json")
		mustWrite(t, outside, data)
		if err := os.Symlink(outside, filepath.Join(s.Dir(), "out.json")); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ReadArtifact(mustRef(t, "out.json", data)); !errors.Is(err, ErrSymlink) {
			t.Fatalf("ReadArtifact: %v, want ErrSymlink", err)
		}
	})
	t.Run("dangling link", func(t *testing.T) {
		s := newStore(t, Options{})
		if err := os.Symlink("missing.json", filepath.Join(s.Dir(), "dangling.json")); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ReadArtifact(mustRef(t, "dangling.json", data)); !errors.Is(err, ErrSymlink) {
			t.Fatalf("ReadArtifact: %v, want ErrSymlink", err)
		}
		if _, err := s.WriteArtifact("dangling.json", data); !errors.Is(err, ErrSymlink) {
			t.Fatalf("WriteArtifact: %v, want ErrSymlink", err)
		}
	})
	t.Run("an intermediate directory is a link", func(t *testing.T) {
		s := newStore(t, Options{})
		if _, err := s.WriteArtifact("d/x.json", data); err != nil {
			t.Fatal(err)
		}
		// Replace d with a link to a real directory inside the Run.
		if err := os.Rename(filepath.Join(s.Dir(), "d"), filepath.Join(s.Dir(), "real")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("real", filepath.Join(s.Dir(), "d")); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ReadArtifact(mustRef(t, "d/x.json", data)); !errors.Is(err, ErrSymlink) {
			t.Fatalf("ReadArtifact: %v, want ErrSymlink", err)
		}
		if _, err := s.WriteArtifact("d/x.json", data); !errors.Is(err, ErrSymlink) {
			t.Fatalf("WriteArtifact of the same bytes: %v, want ErrSymlink", err)
		}
		if _, err := s.WriteArtifact("d/new.json", data); !errors.Is(err, ErrSymlink) {
			t.Fatalf("WriteArtifact of a new file: %v, want ErrSymlink", err)
		}
		if _, err := s.WriteArtifact("d/sub/new.json", data); !errors.Is(err, ErrSymlink) {
			t.Fatalf("WriteArtifact below a new directory: %v, want ErrSymlink", err)
		}
		if _, err := os.Lstat(filepath.Join(s.Dir(), "real", "new.json")); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("a write went through the link: %v", err)
		}
	})
	t.Run("an intermediate directory is a link outside the Run", func(t *testing.T) {
		s := newStore(t, Options{})
		outside := t.TempDir()
		mustWrite(t, filepath.Join(outside, "x.json"), data)
		if err := os.Symlink(outside, filepath.Join(s.Dir(), "ext")); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ReadArtifact(mustRef(t, "ext/x.json", data)); !errors.Is(err, ErrSymlink) {
			t.Fatalf("ReadArtifact: %v, want ErrSymlink", err)
		}
		if _, err := s.WriteArtifact("ext/y.json", data); !errors.Is(err, ErrSymlink) {
			t.Fatalf("WriteArtifact: %v, want ErrSymlink", err)
		}
		if _, err := os.Lstat(filepath.Join(outside, "y.json")); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("a write escaped the Run directory: %v", err)
		}
	})
}

func TestArtifactReadNotRegular(t *testing.T) {
	s := newStore(t, Options{})
	if err := os.Mkdir(filepath.Join(s.Dir(), "adir.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(s.Dir(), "fifo.json"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"adir.json", "fifo.json"} {
		// The call must return, not block on the FIFO.
		if _, err := s.ReadArtifact(mustRef(t, p, nil)); !errors.Is(err, ErrNotRegular) {
			t.Errorf("ReadArtifact(%q): %v, want ErrNotRegular", p, err)
		}
	}
	// Writing the same path does not open the FIFO either.
	if _, err := s.WriteArtifact("fifo.json", nil); !errors.Is(err, ErrArtifactExists) {
		t.Errorf("WriteArtifact over a FIFO: %v, want ErrArtifactExists", err)
	}
	// A file where a directory is needed.
	if _, err := s.WriteArtifact("file.json", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteArtifact("file.json/below.json", []byte("x")); !errors.Is(err, syscall.ENOTDIR) {
		t.Errorf("WriteArtifact below a file: %v, want ENOTDIR", err)
	}
}

func TestArtifactReadMismatch(t *testing.T) {
	data := []byte("0123456789")

	t.Run("one byte changed, same size", func(t *testing.T) {
		s := newStore(t, Options{})
		ref, err := s.WriteArtifact("a.json", data)
		if err != nil {
			t.Fatal(err)
		}
		changed := bytes.Clone(data)
		changed[3] = 'X'
		mustWrite(t, filepath.Join(s.Dir(), "a.json"), changed)
		_, err = s.ReadArtifact(ref)
		if !errors.Is(err, ErrArtifactMismatch) || !strings.Contains(err.Error(), "sha256") {
			t.Fatalf("ReadArtifact: %v, want ErrArtifactMismatch on the hash", err)
		}
		if err := s.VerifyArtifacts([]domain.ArtifactRef{ref}); !errors.Is(err, ErrArtifactMismatch) {
			t.Errorf("VerifyArtifacts: %v", err)
		}
	})
	t.Run("file extended", func(t *testing.T) {
		s := newStore(t, Options{})
		ref, err := s.WriteArtifact("a.json", data)
		if err != nil {
			t.Fatal(err)
		}
		mustWrite(t, filepath.Join(s.Dir(), "a.json"), append(bytes.Clone(data), '!'))
		_, err = s.ReadArtifact(ref)
		if !errors.Is(err, ErrArtifactMismatch) || !strings.Contains(err.Error(), "bytes") {
			t.Fatalf("ReadArtifact: %v, want ErrArtifactMismatch on the size", err)
		}
	})
	t.Run("file truncated", func(t *testing.T) {
		s := newStore(t, Options{})
		ref, err := s.WriteArtifact("a.json", data)
		if err != nil {
			t.Fatal(err)
		}
		mustWrite(t, filepath.Join(s.Dir(), "a.json"), data[:4])
		if _, err := s.ReadArtifact(ref); !errors.Is(err, ErrArtifactMismatch) {
			t.Fatalf("ReadArtifact: %v, want ErrArtifactMismatch", err)
		}
	})
	t.Run("reference of other content", func(t *testing.T) {
		s := newStore(t, Options{})
		if _, err := s.WriteArtifact("a.json", data); err != nil {
			t.Fatal(err)
		}
		wrong := mustRef(t, "a.json", []byte("9876543210"))
		if _, err := s.ReadArtifact(wrong); !errors.Is(err, ErrArtifactMismatch) {
			t.Fatalf("ReadArtifact: %v, want ErrArtifactMismatch", err)
		}
	})
}

func TestArtifactReadTooLarge(t *testing.T) {
	big := bytes.Repeat([]byte("b"), 100)
	s := newStore(t, Options{})
	ref, err := s.WriteArtifact("big.json", big)
	if err != nil {
		t.Fatal(err)
	}
	dir := s.Dir()
	s.Close()
	small := reopen(t, dir, Options{MaxArtifactSize: 99})
	if _, err := small.ReadArtifact(ref); !errors.Is(err, ErrArtifactTooLarge) {
		t.Fatalf("ReadArtifact above the limit: %v, want ErrArtifactTooLarge", err)
	}
	// A file that grew past the limit is too large, not merely a mismatch.
	mustWrite(t, filepath.Join(dir, "big.json"), append(bytes.Clone(big), big...))
	if _, err := small.ReadArtifact(ref); !errors.Is(err, ErrArtifactTooLarge) {
		t.Fatalf("ReadArtifact of a grown file: %v, want ErrArtifactTooLarge", err)
	}
}

func TestArtifactTemporaryFilesAreNeverArtifacts(t *testing.T) {
	var crash bool
	s := newStore(t, Options{Faults: func(p CrashPoint) bool { return crash && p == CrashArtifactTempSynced }})
	crash = true
	data := []byte("half-made")
	if _, err := s.WriteArtifact("a/x.json", data); !errors.Is(err, ErrInjectedCrash) {
		t.Fatalf("WriteArtifact: %v, want ErrInjectedCrash", err)
	}
	dir := s.Dir()
	s.Abandon()
	s = reopen(t, dir, Options{})

	entries, err := os.ReadDir(filepath.Join(dir, "a"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries of a/: %v, %v", entries, err)
	}
	tmp := entries[0].Name()
	if !strings.HasPrefix(tmp, ".x.json.tmp-") {
		t.Fatalf("temporary file name %q", tmp)
	}
	// Neither a reference to the temporary file nor one to the final path
	// is accepted.
	if _, err := s.ReadArtifact(domain.ArtifactRef{Path: "a/" + tmp, Size: int64(len(data)), SHA256: mustRef(t, "a/x.json", data).SHA256}); !errors.Is(err, domain.ErrInvalidPath) {
		t.Errorf("ReadArtifact(temporary): %v, want ErrInvalidPath", err)
	}
	if _, err := s.ReadArtifact(mustRef(t, "a/x.json", data)); !errors.Is(err, ErrArtifactMissing) {
		t.Errorf("ReadArtifact(final path): %v, want ErrArtifactMissing", err)
	}
	// Writing the artifact again works and leaves the orphan alone.
	if _, err := s.WriteArtifact("a/x.json", data); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "a", tmp)); err != nil {
		t.Errorf("the orphaned temporary file was touched: %v", err)
	}
}

// TestWriteNewNeverReplacesButReplaceDoes pins the primitives that artifacts
// and the snapshot are built on: rename would silently replace the target, so
// writeNew must refuse an existing one itself.
func TestWriteNewNeverReplacesButReplaceDoes(t *testing.T) {
	s := newStore(t, Options{})
	p := filepath.Join(s.Dir(), "x", "a")
	if err := s.writeNew("x/a", []byte("one"), finalizePoints{}); err != nil {
		t.Fatal(err)
	}
	if err := s.writeNew("x/a", []byte("two"), finalizePoints{}); !errors.Is(err, ErrArtifactExists) {
		t.Fatalf("writeNew over an existing file: %v, want ErrArtifactExists", err)
	}
	if got := mustRead(t, p); string(got) != "one" {
		t.Errorf("the file holds %q", got)
	}
	if err := s.replace("x/a", []byte("two"), finalizePoints{}); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, p); string(got) != "two" {
		t.Errorf("the file holds %q", got)
	}
	assertNoTempFiles(t, s.Dir())
}
