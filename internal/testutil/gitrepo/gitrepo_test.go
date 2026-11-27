package gitrepo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNew(t *testing.T) {
	dir := New(t)
	if got := Git(t, dir, "rev-list", "--count", "HEAD"); got != "1" {
		t.Errorf("commit count = %q, want 1", got)
	}
	if got := Git(t, dir, "status", "--porcelain"); got != "" {
		t.Errorf("status --porcelain = %q, want empty", got)
	}
	if got := Git(t, dir, "rev-parse", "--abbrev-ref", "HEAD"); got != "main" {
		t.Errorf("branch = %q, want main", got)
	}
	if got := Git(t, dir, "ls-files"); got != "README.md" {
		t.Errorf("tracked files = %q, want README.md", got)
	}
	if got := Git(t, dir, "log", "-1", "--format=%an <%ae> %aI"); got != "bellona-test <test@example.invalid> 2026-01-01T00:00:00Z" {
		t.Errorf("commit identity = %q", got)
	}
}

func TestNewIsIsolatedFromUserConfig(t *testing.T) {
	// A hostile global configuration must not leak into the helper.
	home := t.TempDir()
	cfg := filepath.Join(home, "gitconfig")
	if err := os.WriteFile(cfg, []byte("[commit]\n\tgpgsign = true\n[user]\n\tname = Someone Else\n[init]\n\tdefaultBranch = other\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_AUTHOR_NAME", "Someone Else")
	dir := New(t)
	if got := Git(t, dir, "log", "-1", "--format=%an"); got != "bellona-test" {
		t.Errorf("author = %q, want bellona-test", got)
	}
	if got := Git(t, dir, "rev-parse", "--abbrev-ref", "HEAD"); got != "main" {
		t.Errorf("branch = %q, want main", got)
	}
}

func TestNewReturnsIndependentRepositories(t *testing.T) {
	a, b := New(t), New(t)
	if a == b {
		t.Fatalf("both repositories live in %s", a)
	}
	if err := os.WriteFile(filepath.Join(a, "only-in-a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := Git(t, b, "status", "--porcelain"); got != "" {
		t.Errorf("repository b is dirty: %q", got)
	}
}

func TestFromDir(t *testing.T) {
	src := t.TempDir()
	write := func(rel, content string, perm os.FileMode) {
		t.Helper()
		p := filepath.Join(src, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), perm); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example\n", 0o644)
	write("pkg/inner/a.go", "package inner\n", 0o644)
	write("scripts/run.sh", "#!/bin/sh\n", 0o755)

	dir := FromDir(t, src)
	if got := Git(t, dir, "rev-list", "--count", "HEAD"); got != "1" {
		t.Errorf("commit count = %q, want 1", got)
	}
	if got := Git(t, dir, "status", "--porcelain"); got != "" {
		t.Errorf("status --porcelain = %q, want empty", got)
	}
	if got, want := Git(t, dir, "ls-files"), "go.mod\npkg/inner/a.go\nscripts/run.sh"; got != want {
		t.Errorf("tracked files = %q, want %q", got, want)
	}
	if got := Git(t, dir, "ls-files", "-s", "scripts/run.sh"); !strings.HasPrefix(got, "100755 ") {
		t.Errorf("run.sh mode = %q, want an executable file", got)
	}
	data, err := os.ReadFile(filepath.Join(dir, "pkg", "inner", "a.go"))
	if err != nil || string(data) != "package inner\n" {
		t.Errorf("copied file = %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(src, ".git")); !os.IsNotExist(err) {
		t.Errorf("the source directory must not become a repository: %v", err)
	}
}

func TestFromDirEmptySource(t *testing.T) {
	dir := FromDir(t, t.TempDir())
	if got := Git(t, dir, "rev-list", "--count", "HEAD"); got != "1" {
		t.Errorf("commit count = %q, want 1", got)
	}
}

func TestCopyTreeRejectsIrregularEntries(t *testing.T) {
	t.Run("symlink to a file", func(t *testing.T) {
		src := t.TempDir()
		if err := os.WriteFile(filepath.Join(src, "real.txt"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("real.txt", filepath.Join(src, "link.txt")); err != nil {
			t.Skipf("cannot create a symlink: %v", err)
		}
		err := copyTree(src, t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "link.txt") || !strings.Contains(err.Error(), "symbolic") {
			t.Fatalf("copyTree error = %v, want a symbolic link error naming link.txt", err)
		}
	})
	t.Run("dangling symlink in a subdirectory", func(t *testing.T) {
		src := t.TempDir()
		if err := os.Mkdir(filepath.Join(src, "sub"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("/nonexistent/target", filepath.Join(src, "sub", "dangling")); err != nil {
			t.Skipf("cannot create a symlink: %v", err)
		}
		if err := copyTree(src, t.TempDir()); err == nil {
			t.Fatal("copyTree accepted a dangling symlink")
		}
	})
	t.Run("symlink to a directory", func(t *testing.T) {
		src := t.TempDir()
		if err := os.Mkdir(filepath.Join(src, "real"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("real", filepath.Join(src, "link")); err != nil {
			t.Skipf("cannot create a symlink: %v", err)
		}
		if err := copyTree(src, t.TempDir()); err == nil {
			t.Fatal("copyTree accepted a symlink to a directory")
		}
	})
	t.Run(".git entry", func(t *testing.T) {
		src := t.TempDir()
		if err := os.Mkdir(filepath.Join(src, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := copyTree(src, t.TempDir()); err == nil {
			t.Fatal("copyTree accepted a .git directory")
		}
	})
	t.Run("missing source", func(t *testing.T) {
		if err := copyTree(filepath.Join(t.TempDir(), "absent"), t.TempDir()); err == nil {
			t.Fatal("copyTree accepted a missing source")
		}
	})
}

func TestGitRunsInTheGivenDirectory(t *testing.T) {
	dir := New(t)
	if got := Git(t, dir, "rev-parse", "--show-toplevel"); !strings.HasSuffix(got, filepath.Base(dir)) {
		t.Errorf("toplevel = %q, want it to end with %q", got, filepath.Base(dir))
	}
}

// With git missing from PATH, every entry point skips the test instead of
// failing it. A skipped subtest does not fail its parent, while a helper that
// forgot to skip would fail because it could not run git.
func TestHelpersSkipWithoutGit(t *testing.T) {
	t.Run("New", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		New(t)
		t.Error("New did not skip")
	})
	t.Run("FromDir", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		FromDir(t, t.TempDir())
		t.Error("FromDir did not skip")
	})
	t.Run("Git", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		Git(t, t.TempDir(), "status")
		t.Error("Git did not skip")
	})
}
