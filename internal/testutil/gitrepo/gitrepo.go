// Package gitrepo creates temporary Git repositories for tests (SPEC §17,
// R-TECH-02). Git runs with an isolated configuration, so the user's Git
// settings, hooks and signing keys never influence a test. Only test code may
// import this package.
package gitrepo

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// New creates a temporary directory that holds a Git repository with a README.md
// and exactly one commit on the branch main, and returns its path. The
// directory is removed by the testing package when the test ends. The test is
// skipped when git is not on PATH.
func New(tb testing.TB) string {
	tb.Helper()
	requireGit(tb)
	dir := tb.TempDir()
	Git(tb, dir, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Test repository\n"), 0o644); err != nil {
		tb.Fatalf("gitrepo: %v", err)
	}
	commitAll(tb, dir)
	return dir
}

// FromDir is like New, but copies the files and directories of src into the
// repository first and commits them as the initial commit. A symbolic link or
// any other irregular file in src, or a .git entry, fails the test.
func FromDir(tb testing.TB, src string) string {
	tb.Helper()
	requireGit(tb)
	dir := tb.TempDir()
	Git(tb, dir, "init", "-q", "-b", "main")
	if err := copyTree(src, dir); err != nil {
		tb.Fatalf("gitrepo: %v", err)
	}
	commitAll(tb, dir)
	return dir
}

// Git runs git with the given arguments in dir and returns its standard
// output without the trailing newline. A failure fails the test with git's
// standard error. The environment is explicit: the user's Git configuration is
// never read, and every date, name and e-mail address is fixed. The test is
// skipped when git is not on PATH.
func Git(tb testing.TB, dir string, args ...string) string {
	tb.Helper()
	requireGit(tb)
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnv(tb.TempDir())
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		tb.Fatalf("gitrepo: git %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimRight(stdout.String(), "\r\n")
}

func requireGit(tb testing.TB) {
	tb.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		tb.Skip("git is not on PATH")
	}
}

func gitEnv(home string) []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"LC_ALL=C",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=bellona-test",
		"GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_AUTHOR_DATE=2026-01-01T00:00:00Z",
		"GIT_COMMITTER_NAME=bellona-test",
		"GIT_COMMITTER_EMAIL=test@example.invalid",
		"GIT_COMMITTER_DATE=2026-01-01T00:00:00Z",
	}
}

func commitAll(tb testing.TB, dir string) {
	tb.Helper()
	Git(tb, dir, "add", "-A")
	Git(tb, dir, "-c", "commit.gpgsign=false", "commit", "--no-verify", "-q", "--allow-empty", "-m", "Initial commit")
}

// copyTree copies the regular files and directories under src into the
// existing directory dst, keeping the permission bits. It fails on a symbolic
// link, on any other irregular file and on a .git entry.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if d.Name() == ".git" {
			return fmt.Errorf("%s: a .git entry cannot be copied", rel)
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o755)
		case d.Type().IsRegular():
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			return os.WriteFile(target, data, info.Mode().Perm())
		case d.Type()&fs.ModeSymlink != 0:
			return fmt.Errorf("%s: symbolic links cannot be copied", rel)
		default:
			return errors.New(rel + ": only regular files and directories can be copied")
		}
	})
}
