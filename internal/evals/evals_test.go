package evals

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/shutx-net/bellona/internal/strictjson"
	"github.com/shutx-net/bellona/internal/testutil/gitrepo"
)

// sampleTasksDir is the directory of the sample tasks, relative to this
// package.
var sampleTasksDir = filepath.Join("..", "..", "evals", "tasks")

func sampleTaskDirs(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(sampleTasksDir)
	if err != nil {
		t.Fatal(err)
	}
	var dirs []string
	for _, e := range entries {
		if !e.IsDir() {
			t.Errorf("%s: only task directories belong in evals/tasks", e.Name())
			continue
		}
		dirs = append(dirs, filepath.Join(sampleTasksDir, e.Name()))
	}
	if len(dirs) < 2 {
		t.Fatalf("evals/tasks has %d sample tasks, want at least 2", len(dirs))
	}
	return dirs
}

func checkError(t *testing.T, err, want error, pointer string) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
	var se *strictjson.Error
	if !errors.As(err, &se) {
		t.Fatalf("error %T is not a *strictjson.Error", err)
	}
	if se.Pointer != pointer {
		t.Errorf("pointer = %q, want %q (%v)", se.Pointer, pointer, err)
	}
}

// ---------------------------------------------------------------------------
// Tasks
// ---------------------------------------------------------------------------

func TestSampleTasks(t *testing.T) {
	for _, dir := range sampleTaskDirs(t) {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			task, err := LoadTask(dir)
			if err != nil {
				t.Fatal(err)
			}
			if task.ID != filepath.Base(dir) {
				t.Errorf("id %q is not the directory name %q", task.ID, filepath.Base(dir))
			}
			if task.Repository.Kind != RepositoryDirectory {
				t.Errorf("repository kind = %q; the M0 samples start from a directory", task.Repository.Kind)
			}
			if task.Acceptance.Files == "" {
				t.Error("the sample has no hidden acceptance files")
			}
			if len(task.Acceptance.RegressionChecks) == 0 {
				t.Error("the sample has no regression check")
			}
			if len(task.ExpectedChangeScope) == 0 {
				t.Error("the sample has no expected change scope")
			}
		})
	}
}

func validTask() Task {
	return Task{
		SchemaVersion: TaskSchemaVersion,
		ID:            "sample-task",
		Title:         "A sample task",
		Repository:    Repository{Kind: RepositoryDirectory, Path: "testdata/repo"},
		Request:       "Do the thing.",
		Setup:         []Command{{ID: "prepare", Argv: []string{"go", "mod", "download"}}},
		Acceptance: Acceptance{
			Files:            "testdata/acceptance",
			Checks:           []Command{{ID: "acceptance-tests", Argv: []string{"go", "test", "./..."}, Timeout: "2m"}},
			RegressionChecks: []Command{{ID: "existing-tests", Argv: []string{"go", "test", "./..."}}},
		},
		ExpectedChangeScope: []string{"*.go", "cmd/*/main.go", "docs/**"},
		Tags:                []string{"go", "small"},
	}
}

func TestTaskRoundTrip(t *testing.T) {
	want := validTask()
	data, err := strictjson.Encode(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeTask(data)
	if err != nil {
		t.Fatalf("DecodeTask(%s): %v", data, err)
	}
	again, err := strictjson.Encode(got)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(data) {
		t.Errorf("round trip changed the task:\n got %s\nwant %s", again, data)
	}
	// Optional members are absent, not null, when they are not set.
	minimal := validTask()
	minimal.Setup, minimal.ExpectedChangeScope, minimal.Tags = nil, nil, nil
	minimal.Acceptance = Acceptance{Checks: []Command{{ID: "check", Argv: []string{"true"}}}}
	data, err = strictjson.Encode(minimal)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{`"setup"`, `"expectedChangeScope"`, `"tags"`, `"files"`, `"regressionChecks"`, `"timeout"`} {
		if strings.Contains(string(data), name) {
			t.Errorf("minimal task encodes %s: %s", name, data)
		}
	}
	if _, err := DecodeTask(data); err != nil {
		t.Errorf("minimal task: %v", err)
	}
}

func TestTaskValidation(t *testing.T) {
	gitTask := func(mod func(*Repository)) func(*Task) {
		return func(task *Task) {
			task.Repository = Repository{Kind: RepositoryGit, URL: "https://example.com/repo.git", Commit: strings.Repeat("a1", 20)}
			task.Acceptance.Files = "acceptance"
			mod(&task.Repository)
		}
	}
	tests := []struct {
		name    string
		mod     func(*Task)
		pointer string // empty: the task must be accepted
	}{
		{"valid", func(*Task) {}, ""},
		{"valid git repository", gitTask(func(*Repository) {}), ""},
		{"valid scope patterns", func(t *Task) { t.ExpectedChangeScope = []string{"**", "**/*.go", "a/**/b.go", "cmd/*"} }, ""},
		{"no acceptance files", func(t *Task) { t.Acceptance.Files = "" }, ""},

		{"unsupported schema version", func(t *Task) { t.SchemaVersion = 2 }, "/schemaVersion"},
		{"zero schema version", func(t *Task) { t.SchemaVersion = 0 }, "/schemaVersion"},
		{"upper-case id", func(t *Task) { t.ID = "Sample" }, "/id"},
		{"id starting with a dash", func(t *Task) { t.ID = "-sample" }, "/id"},
		{"id with a slash", func(t *Task) { t.ID = "a/b" }, "/id"},
		{"empty id", func(t *Task) { t.ID = "" }, "/id"},
		{"overlong id", func(t *Task) { t.ID = strings.Repeat("a", 65) }, "/id"},
		{"empty title", func(t *Task) { t.Title = " " }, "/title"},
		{"overlong title", func(t *Task) { t.Title = strings.Repeat("t", maxTitleBytes+1) }, "/title"},
		{"empty request", func(t *Task) { t.Request = "\n" }, "/request"},
		{"overlong request", func(t *Task) { t.Request = strings.Repeat("r", MaxRequestBytes+1) }, "/request"},
		{"request at the limit", func(t *Task) { t.Request = strings.Repeat("r", MaxRequestBytes) }, ""},

		{"unknown repository kind", func(t *Task) { t.Repository.Kind = "svn" }, "/repository/kind"},
		{"directory without a path", func(t *Task) { t.Repository.Path = "" }, "/repository/path"},
		{"directory with a url", func(t *Task) { t.Repository.URL = "https://example.com/r" }, "/repository"},
		{"directory with a commit", func(t *Task) { t.Repository.Commit = strings.Repeat("a", 40) }, "/repository"},
		{"absolute path", func(t *Task) { t.Repository.Path = "/etc" }, "/repository/path"},
		{"path with ..", func(t *Task) { t.Repository.Path = "../repo" }, "/repository/path"},
		{"path with .. inside", func(t *Task) { t.Repository.Path = "a/../repo" }, "/repository/path"},
		{"path with a dot segment", func(t *Task) { t.Repository.Path = "./repo" }, "/repository/path"},
		{"path with an empty segment", func(t *Task) { t.Repository.Path = "a//b" }, "/repository/path"},
		{"path with a trailing slash", func(t *Task) { t.Repository.Path = "repo/" }, "/repository/path"},
		{"path with a backslash", func(t *Task) { t.Repository.Path = `a\b` }, "/repository/path"},
		{"git without a url", gitTask(func(r *Repository) { r.URL = "" }), "/repository/url"},
		{"git url with a space", gitTask(func(r *Repository) { r.URL = "https://example.com/a b" }), "/repository/url"},
		{"git url that reads as an option", gitTask(func(r *Repository) { r.URL = "--upload-pack=x" }), "/repository/url"},
		{"git without a commit", gitTask(func(r *Repository) { r.Commit = "" }), "/repository/commit"},
		{"git with a short commit", gitTask(func(r *Repository) { r.Commit = "abc123" }), "/repository/commit"},
		{"git with an upper-case commit", gitTask(func(r *Repository) { r.Commit = strings.Repeat("A", 40) }), "/repository/commit"},
		{"git with a path", gitTask(func(r *Repository) { r.Path = "repo" }), "/repository/path"},

		{"duplicate setup id", func(t *Task) {
			t.Setup = []Command{{ID: "a", Argv: []string{"x"}}, {ID: "a", Argv: []string{"y"}}}
		}, "/setup/1/id"},
		{"setup id with a capital", func(t *Task) { t.Setup[0].ID = "Prepare" }, "/setup/0/id"},
		{"empty argv", func(t *Task) { t.Setup[0].Argv = nil }, "/setup/0/argv"},
		{"empty program", func(t *Task) { t.Setup[0].Argv = []string{""} }, "/setup/0/argv"},
		{"NUL in argv", func(t *Task) { t.Setup[0].Argv = []string{"go", "a\x00b"} }, "/setup/0/argv/1"},
		{"empty later argument is legitimate", func(t *Task) { t.Setup[0].Argv = []string{"go", ""} }, ""},
		{"unparsable timeout", func(t *Task) { t.Setup[0].Timeout = "soon" }, "/setup/0/timeout"},
		{"zero timeout", func(t *Task) { t.Setup[0].Timeout = "0s" }, "/setup/0/timeout"},
		{"negative timeout", func(t *Task) { t.Setup[0].Timeout = "-1m" }, "/setup/0/timeout"},
		{"timeout without a unit", func(t *Task) { t.Setup[0].Timeout = "30" }, "/setup/0/timeout"},

		{"no acceptance check", func(t *Task) { t.Acceptance.Checks = nil }, "/acceptance/checks"},
		{"duplicate check id", func(t *Task) {
			t.Acceptance.Checks = append(t.Acceptance.Checks, t.Acceptance.Checks[0])
		}, "/acceptance/checks/1/id"},
		{"duplicate regression check id", func(t *Task) {
			t.Acceptance.RegressionChecks = append(t.Acceptance.RegressionChecks, t.Acceptance.RegressionChecks[0])
		}, "/acceptance/regressionChecks/1/id"},
		{"the same id in both lists is allowed", func(t *Task) { t.Acceptance.RegressionChecks[0].ID = t.Acceptance.Checks[0].ID }, ""},
		{"absolute acceptance files", func(t *Task) { t.Acceptance.Files = "/tmp/a" }, "/acceptance/files"},
		{"acceptance files with ..", func(t *Task) { t.Acceptance.Files = "../a" }, "/acceptance/files"},
		{"acceptance files equal to the repository", func(t *Task) { t.Acceptance.Files = "testdata/repo" }, "/acceptance/files"},
		{"acceptance files inside the repository", func(t *Task) { t.Acceptance.Files = "testdata/repo/accept" }, "/acceptance/files"},
		{"repository inside the acceptance files", func(t *Task) { t.Acceptance.Files = "testdata" }, "/acceptance/files"},
		{"a name that only starts like the repository path", func(t *Task) { t.Acceptance.Files = "testdata/repository-hidden" }, ""},

		{"scope with ..", func(t *Task) { t.ExpectedChangeScope = []string{"*.go", "../x"} }, "/expectedChangeScope/1"},
		{"absolute scope", func(t *Task) { t.ExpectedChangeScope = []string{"/x"} }, "/expectedChangeScope/0"},
		{"negated scope", func(t *Task) { t.ExpectedChangeScope = []string{"!x"} }, "/expectedChangeScope/0"},
		{"empty scope", func(t *Task) { t.ExpectedChangeScope = []string{""} }, "/expectedChangeScope/0"},
		{"scope with a partial globstar", func(t *Task) { t.ExpectedChangeScope = []string{"a**"} }, "/expectedChangeScope/0"},
		{"scope with an empty segment", func(t *Task) { t.ExpectedChangeScope = []string{"a//b"} }, "/expectedChangeScope/0"},
		{"duplicate tag", func(t *Task) { t.Tags = []string{"go", "go"} }, "/tags/1"},
		{"invalid tag", func(t *Task) { t.Tags = []string{"Go"} }, "/tags/0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			task := validTask()
			tt.mod(&task)
			data, err := strictjson.Encode(task)
			if err != nil {
				t.Fatal(err)
			}
			_, err = DecodeTask(data)
			if tt.pointer == "" {
				if err != nil {
					t.Fatalf("DecodeTask error = %v", err)
				}
				return
			}
			checkError(t, err, strictjson.ErrInvalidValue, tt.pointer)
		})
	}
}

func TestTaskStrictDecoding(t *testing.T) {
	const valid = `{"schemaVersion":1,"id":"sample-task","title":"A sample","repository":{"kind":"directory","path":"repo"},` +
		`"request":"Do it.","acceptance":{"checks":[{"id":"check","argv":["true"]}]}}`
	if _, err := DecodeTask([]byte(valid)); err != nil {
		t.Fatalf("the base document must decode: %v", err)
	}
	tests := []struct {
		name     string
		old, new string
		want     error
		pointer  string
	}{
		{"unknown member", `"request":"Do it.",`, `"request":"Do it.","extra":1,`, strictjson.ErrUnknownMember, "/extra"},
		{"unknown nested member", `"kind":"directory",`, `"kind":"directory","extra":1,`, strictjson.ErrUnknownMember, "/repository/extra"},
		{"wrong case", `"request"`, `"Request"`, strictjson.ErrUnknownMember, "/Request"},
		{"missing id", `"id":"sample-task",`, ``, strictjson.ErrMissingMember, "/id"},
		{"missing title", `"title":"A sample",`, ``, strictjson.ErrMissingMember, "/title"},
		{"missing repository", `"repository":{"kind":"directory","path":"repo"},`, ``, strictjson.ErrMissingMember, "/repository"},
		{"missing kind", `"kind":"directory",`, ``, strictjson.ErrMissingMember, "/repository/kind"},
		{"missing request", `"request":"Do it.",`, ``, strictjson.ErrMissingMember, "/request"},
		{"missing acceptance", `,"acceptance":{"checks":[{"id":"check","argv":["true"]}]}`, ``, strictjson.ErrMissingMember, "/acceptance"},
		{"missing checks", `"checks":[{"id":"check","argv":["true"]}]`, ``, strictjson.ErrMissingMember, "/acceptance/checks"},
		{"missing argv", `"id":"check","argv":["true"]`, `"id":"check"`, strictjson.ErrMissingMember, "/acceptance/checks/0/argv"},
		{"null title", `"title":"A sample"`, `"title":null`, strictjson.ErrNull, "/title"},
		{"null repository", `"repository":{"kind":"directory","path":"repo"}`, `"repository":null`, strictjson.ErrNull, "/repository"},
		{"null path", `"path":"repo"`, `"path":null`, strictjson.ErrNull, "/repository/path"},
		{"null checks", `"checks":[{"id":"check","argv":["true"]}]`, `"checks":null`, strictjson.ErrNull, "/acceptance/checks"},
		{"null argument", `"argv":["true"]`, `"argv":["true",null]`, strictjson.ErrNull, "/acceptance/checks/0/argv/1"},
		{"invalid UTF-8", `"request":"Do it."`, "\"request\":\"a\xffb\"", strictjson.ErrInvalidJSON, "/request"},
		{"wrong type", `"schemaVersion":1`, `"schemaVersion":"1"`, strictjson.ErrInvalidValue, "/schemaVersion"},
		{"duplicate member", `"id":"sample-task",`, `"id":"sample-task","id":"sample-task",`, strictjson.ErrDuplicateMember, "/id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !strings.Contains(valid, tt.old) {
				t.Fatalf("test bug: %q is not in the base document", tt.old)
			}
			_, err := DecodeTask([]byte(strings.Replace(valid, tt.old, tt.new, 1)))
			checkError(t, err, tt.want, tt.pointer)
		})
	}
}

// writeTaskDir creates root/id with a task.json that holds content, and
// returns the task directory.
func writeTaskDir(t *testing.T, id, content string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), id)
	if err := os.MkdirAll(filepath.Join(dir, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "repo", "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "hidden"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hidden", "a_test.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "task.json"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func taskJSON(id, repoPath, filesPath string) string {
	files := ""
	if filesPath != "" {
		files = fmt.Sprintf(`"files":%q,`, filesPath)
	}
	return fmt.Sprintf(`{"schemaVersion":1,"id":%q,"title":"T","repository":{"kind":"directory","path":%q},"request":"R",`+
		`"acceptance":{%s"checks":[{"id":"check","argv":["true"]}]}}`, id, repoPath, files)
}

func TestLoadTaskLayout(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		dir := writeTaskDir(t, "layout", taskJSON("layout", "repo", "hidden"))
		task, err := LoadTask(dir)
		if err != nil {
			t.Fatal(err)
		}
		if task.ID != "layout" {
			t.Errorf("id = %q", task.ID)
		}
	})
	t.Run("relative directory argument", func(t *testing.T) {
		dir := writeTaskDir(t, "layout", taskJSON("layout", "repo", "hidden"))
		t.Chdir(dir)
		if _, err := LoadTask("."); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("the id is not the directory name", func(t *testing.T) {
		dir := writeTaskDir(t, "other-name", taskJSON("layout", "repo", "hidden"))
		_, err := LoadTask(dir)
		if !errors.Is(err, ErrInvalidTask) {
			t.Fatalf("error = %v, want ErrInvalidTask", err)
		}
	})
	t.Run("no task.json", func(t *testing.T) {
		dir := writeTaskDir(t, "layout", taskJSON("layout", "repo", "hidden"))
		if err := os.Remove(filepath.Join(dir, "task.json")); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadTask(dir); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("error = %v, want fs.ErrNotExist", err)
		}
	})
	t.Run("a document that does not decode", func(t *testing.T) {
		dir := writeTaskDir(t, "layout", `{"schemaVersion":1}`)
		_, err := LoadTask(dir)
		if !errors.Is(err, strictjson.ErrMissingMember) {
			t.Fatalf("error = %v, want ErrMissingMember", err)
		}
	})
	t.Run("missing repository directory", func(t *testing.T) {
		dir := writeTaskDir(t, "layout", taskJSON("layout", "missing", "hidden"))
		_, err := LoadTask(dir)
		if !errors.Is(err, ErrInvalidTask) || !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("error = %v, want ErrInvalidTask and fs.ErrNotExist", err)
		}
	})
	t.Run("missing acceptance directory", func(t *testing.T) {
		dir := writeTaskDir(t, "layout", taskJSON("layout", "repo", "missing"))
		if _, err := LoadTask(dir); !errors.Is(err, ErrInvalidTask) {
			t.Fatalf("error = %v, want ErrInvalidTask", err)
		}
	})
	t.Run("the repository path is a file", func(t *testing.T) {
		dir := writeTaskDir(t, "layout", taskJSON("layout", "repo/main.go", "hidden"))
		if _, err := LoadTask(dir); !errors.Is(err, ErrInvalidTask) {
			t.Fatalf("error = %v, want ErrInvalidTask", err)
		}
	})
	t.Run("a path that leaves the task directory", func(t *testing.T) {
		dir := writeTaskDir(t, "layout", taskJSON("layout", "../layout/repo", "hidden"))
		_, err := LoadTask(dir)
		checkError(t, err, strictjson.ErrInvalidValue, "/repository/path")
	})
	t.Run("oversized task.json", func(t *testing.T) {
		pad := strings.Repeat(" ", MaxTaskFileBytes)
		dir := writeTaskDir(t, "layout", taskJSON("layout", "repo", "hidden")+pad)
		if _, err := LoadTask(dir); !errors.Is(err, ErrInvalidTask) {
			t.Fatalf("error = %v, want ErrInvalidTask", err)
		}
	})
	t.Run("task.json at the size limit", func(t *testing.T) {
		doc := taskJSON("layout", "repo", "hidden")
		dir := writeTaskDir(t, "layout", doc+strings.Repeat(" ", MaxTaskFileBytes-len(doc)))
		if _, err := LoadTask(dir); err != nil {
			t.Fatal(err)
		}
	})

	symlink := func(t *testing.T, target, link string) {
		t.Helper()
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("symbolic links are not available: %v", err)
		}
	}
	t.Run("the repository directory is a symbolic link", func(t *testing.T) {
		dir := writeTaskDir(t, "layout", taskJSON("layout", "linked", "hidden"))
		symlink(t, "repo", filepath.Join(dir, "linked"))
		if _, err := LoadTask(dir); !errors.Is(err, ErrInvalidTask) {
			t.Fatalf("error = %v, want ErrInvalidTask", err)
		}
	})
	t.Run("a symbolic link on the way to the directory", func(t *testing.T) {
		dir := writeTaskDir(t, "layout", taskJSON("layout", "up/repo", "hidden"))
		symlink(t, ".", filepath.Join(dir, "up"))
		if _, err := LoadTask(dir); !errors.Is(err, ErrInvalidTask) {
			t.Fatalf("error = %v, want ErrInvalidTask", err)
		}
	})
	t.Run("a symbolic link inside the acceptance directory", func(t *testing.T) {
		dir := writeTaskDir(t, "layout", taskJSON("layout", "repo", "hidden"))
		symlink(t, "a_test.go", filepath.Join(dir, "hidden", "b_test.go"))
		if _, err := LoadTask(dir); !errors.Is(err, ErrInvalidTask) {
			t.Fatalf("error = %v, want ErrInvalidTask", err)
		}
	})
	t.Run("task.json is a symbolic link", func(t *testing.T) {
		dir := writeTaskDir(t, "layout", taskJSON("layout", "repo", "hidden"))
		if err := os.Rename(filepath.Join(dir, "task.json"), filepath.Join(dir, "real.json")); err != nil {
			t.Fatal(err)
		}
		symlink(t, "real.json", filepath.Join(dir, "task.json"))
		if _, err := LoadTask(dir); !errors.Is(err, ErrInvalidTask) {
			t.Fatalf("error = %v, want ErrInvalidTask", err)
		}
	})
}

// ---------------------------------------------------------------------------
// Result records
// ---------------------------------------------------------------------------

// syntheticResult is a record that exists only to exercise the format. Its
// numbers and model names are made up and measure nothing.
const syntheticResult = `{"schemaVersion":1,"taskId":"calc-divide","variant":"variant-d","trial":2,"bellonaVersion":"0.1.0-dev",` +
	`"models":[{"role":"planner","requested":"model-a","confirmed":"model-a"},{"role":"implementer","requested":"model-b"}],` +
	`"startedAt":"2026-03-01T10:00:00Z","finishedAt":"2026-03-01T10:12:30.5Z",` +
	`"acceptance":{"passed":false,"checks":[{"id":"acceptance-tests","exitCode":1}]},` +
	`"regressions":0,"unnecessaryChangedFiles":2,"tokens":{"input":1200,"output":340,"cacheRead":0},` +
	`"apiEquivalentCostUsd":0.42,"billedCostUsd":0,"retryCount":1,"planRevisionCount":0,"notes":"synthetic record for the format tests"}`

func ptr[T any](v T) *T { return &v }

// validResult returns a fresh valid record of variant-d.
func validResult(t *testing.T) Result {
	t.Helper()
	r, err := DecodeResult([]byte(syntheticResult))
	if err != nil {
		t.Fatalf("the synthetic record must decode: %v", err)
	}
	return r
}

func TestResultRecord(t *testing.T) {
	t.Run("round trip", func(t *testing.T) {
		r := validResult(t)
		data, err := strictjson.Encode(r)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != syntheticResult {
			t.Errorf("Encode(Decode(x)) changed the record:\n got %s\nwant %s", data, syntheticResult)
		}
		if r.StartedAt != time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC) || r.FinishedAt.Sub(r.StartedAt) != 12*time.Minute+30*time.Second+500*time.Millisecond {
			t.Errorf("times: %v .. %v", r.StartedAt, r.FinishedAt)
		}
	})

	t.Run("a measured zero stays a measured zero", func(t *testing.T) {
		r := validResult(t)
		if r.Regressions == nil || *r.Regressions != 0 ||
			r.BilledCostUSD == nil || *r.BilledCostUSD != 0 ||
			r.PlanRevisionCount == nil || *r.PlanRevisionCount != 0 ||
			r.Tokens == nil || r.Tokens.CacheRead == nil || *r.Tokens.CacheRead != 0 {
			t.Fatalf("a zero in the record was lost: %+v", r)
		}
		data, err := strictjson.Encode(r)
		if err != nil {
			t.Fatal(err)
		}
		for _, member := range []string{`"regressions":0`, `"billedCostUsd":0`, `"planRevisionCount":0`, `"cacheRead":0`} {
			if !strings.Contains(string(data), member) {
				t.Errorf("a measured zero is not encoded: want %s in %s", member, data)
			}
		}
	})

	t.Run("an unmeasured value is an absent member", func(t *testing.T) {
		r := validResult(t)
		r.Regressions, r.UnnecessaryChangedFiles, r.Tokens = nil, nil, nil
		r.APIEquivalentCostUSD, r.BilledCostUSD, r.RetryCount, r.PlanRevisionCount = nil, nil, nil, nil
		r.Models, r.Notes = nil, ""
		data, err := strictjson.Encode(r)
		if err != nil {
			t.Fatal(err)
		}
		for _, member := range []string{"regressions", "unnecessaryChangedFiles", "tokens", "apiEquivalentCostUsd", "billedCostUsd",
			"retryCount", "planRevisionCount", "models", "notes", "fixedEffort"} {
			if strings.Contains(string(data), `"`+member+`"`) {
				t.Errorf("member %q is encoded although it is not measured: %s", member, data)
			}
		}
		if strings.Contains(string(data), "null") {
			t.Errorf("the record contains null: %s", data)
		}
		got, err := DecodeResult(data)
		if err != nil {
			t.Fatal(err)
		}
		if got.Regressions != nil || got.Tokens != nil || got.APIEquivalentCostUSD != nil || got.BilledCostUSD != nil ||
			got.RetryCount != nil || got.PlanRevisionCount != nil || got.UnnecessaryChangedFiles != nil {
			t.Errorf("an absent member decoded as a value: %+v", got)
		}
	})

	t.Run("partial tokens", func(t *testing.T) {
		r := validResult(t)
		r.Tokens = &Tokens{Output: ptr(int64(7))}
		data, err := strictjson.Encode(r)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), `"tokens":{"output":7}`) {
			t.Errorf("partial tokens: %s", data)
		}
		if _, err := DecodeResult(data); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("every variant", func(t *testing.T) {
		for _, v := range Variants() {
			r := validResult(t)
			r.Variant = v
			switch v {
			case VariantBaselineA:
				r.BellonaVersion = ""
			case VariantBaselineB:
				r.FixedEffort = "high"
			}
			data, err := strictjson.Encode(r)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := DecodeResult(data); err != nil {
				t.Errorf("%s: %v (%s)", v, err, data)
			}
		}
	})

	t.Run("strict rejections", func(t *testing.T) {
		tests := []struct {
			name     string
			old, new string
			want     error
			pointer  string
		}{
			{"unknown member", `"trial":2,`, `"trial":2,"selfEvaluation":9,`, strictjson.ErrUnknownMember, "/selfEvaluation"},
			{"unknown nested member", `"cacheRead":0}`, `"cacheRead":0,"extra":1}`, strictjson.ErrUnknownMember, "/tokens/extra"},
			{"wrong case", `"taskId"`, `"TaskId"`, strictjson.ErrUnknownMember, "/TaskId"},
			{"duplicate member", `"trial":2,`, `"trial":2,"trial":2,`, strictjson.ErrDuplicateMember, "/trial"},
			{"missing schemaVersion", `"schemaVersion":1,`, ``, strictjson.ErrMissingMember, "/schemaVersion"},
			{"missing taskId", `"taskId":"calc-divide",`, ``, strictjson.ErrMissingMember, "/taskId"},
			{"missing variant", `"variant":"variant-d",`, ``, strictjson.ErrMissingMember, "/variant"},
			{"missing trial", `"trial":2,`, ``, strictjson.ErrMissingMember, "/trial"},
			{"missing startedAt", `"startedAt":"2026-03-01T10:00:00Z",`, ``, strictjson.ErrMissingMember, "/startedAt"},
			{"missing finishedAt", `"finishedAt":"2026-03-01T10:12:30.5Z",`, ``, strictjson.ErrMissingMember, "/finishedAt"},
			{"missing acceptance", `"acceptance":{"passed":false,"checks":[{"id":"acceptance-tests","exitCode":1}]},`, ``, strictjson.ErrMissingMember, "/acceptance"},
			{"missing passed", `"passed":false,`, ``, strictjson.ErrMissingMember, "/acceptance/passed"},
			{"missing checks", `,"checks":[{"id":"acceptance-tests","exitCode":1}]`, ``, strictjson.ErrMissingMember, "/acceptance/checks"},
			{"missing check id", `"id":"acceptance-tests",`, ``, strictjson.ErrMissingMember, "/acceptance/checks/0/id"},
			{"missing exitCode", `,"exitCode":1`, ``, strictjson.ErrMissingMember, "/acceptance/checks/0/exitCode"},
			{"missing role", `"role":"planner",`, ``, strictjson.ErrMissingMember, "/models/0/role"},
			{"missing requested", `,"requested":"model-b"`, ``, strictjson.ErrMissingMember, "/models/1/requested"},
			{"null trial", `"trial":2`, `"trial":null`, strictjson.ErrNull, "/trial"},
			{"null variant", `"variant":"variant-d"`, `"variant":null`, strictjson.ErrNull, "/variant"},
			{"null startedAt", `"startedAt":"2026-03-01T10:00:00Z"`, `"startedAt":null`, strictjson.ErrNull, "/startedAt"},
			{"null regressions", `"regressions":0`, `"regressions":null`, strictjson.ErrNull, "/regressions"},
			{"null cost", `"billedCostUsd":0`, `"billedCostUsd":null`, strictjson.ErrNull, "/billedCostUsd"},
			{"null tokens", `"tokens":{"input":1200,"output":340,"cacheRead":0}`, `"tokens":null`, strictjson.ErrNull, "/tokens"},
			{"null token", `"input":1200`, `"input":null`, strictjson.ErrNull, "/tokens/input"},
			{"null confirmed", `"confirmed":"model-a"`, `"confirmed":null`, strictjson.ErrNull, "/models/0/confirmed"},
			{"null notes", `"notes":"synthetic record for the format tests"`, `"notes":null`, strictjson.ErrNull, "/notes"},
			{"null models", `"models":[{"role":"planner","requested":"model-a","confirmed":"model-a"},{"role":"implementer","requested":"model-b"}]`, `"models":null`, strictjson.ErrNull, "/models"},
			{"trial as a string", `"trial":2`, `"trial":"2"`, strictjson.ErrInvalidValue, "/trial"},
			{"fractional count", `"unnecessaryChangedFiles":2`, `"unnecessaryChangedFiles":2.5`, strictjson.ErrInvalidValue, "/unnecessaryChangedFiles"},
			{"time with a local offset", `"startedAt":"2026-03-01T10:00:00Z"`, `"startedAt":"2026-03-01T19:00:00+09:00"`, strictjson.ErrInvalidValue, "/startedAt"},
			{"time with a zero offset", `"finishedAt":"2026-03-01T10:12:30.5Z"`, `"finishedAt":"2026-03-01T10:12:30.5+00:00"`, strictjson.ErrInvalidValue, "/finishedAt"},
			{"time that is not a time", `"startedAt":"2026-03-01T10:00:00Z"`, `"startedAt":"yesterday"`, strictjson.ErrInvalidValue, "/startedAt"},
			{"unsupported schemaVersion", `"schemaVersion":1`, `"schemaVersion":2`, strictjson.ErrInvalidValue, "/schemaVersion"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				if !strings.Contains(syntheticResult, tt.old) {
					t.Fatalf("test bug: %q is not in the synthetic record", tt.old)
				}
				_, err := DecodeResult([]byte(strings.Replace(syntheticResult, tt.old, tt.new, 1)))
				checkError(t, err, tt.want, tt.pointer)
			})
		}
	})
}

func TestResultValidation(t *testing.T) {
	zone := time.FixedZone("JST", 9*3600)
	tests := []struct {
		name    string
		mod     func(*Result)
		pointer string // empty: the record must be valid
	}{
		{"valid", func(*Result) {}, ""},
		{"baseline-b with a fixed effort", func(r *Result) { r.Variant, r.FixedEffort = VariantBaselineB, "xhigh" }, ""},
		{"baseline-a outside Bellona", func(r *Result) { r.Variant, r.BellonaVersion = VariantBaselineA, "" }, ""},
		{"no models", func(r *Result) { r.Models = nil }, ""},
		{"equal start and finish", func(r *Result) { r.FinishedAt = r.StartedAt }, ""},

		{"unsupported schema version", func(r *Result) { r.SchemaVersion = 2 }, "/schemaVersion"},
		{"invalid task id", func(r *Result) { r.TaskID = "Calc Divide" }, "/taskId"},
		{"unknown variant", func(r *Result) { r.Variant = "variant-f" }, "/variant"},
		{"empty variant", func(r *Result) { r.Variant = "" }, "/variant"},
		{"trial zero", func(r *Result) { r.Trial = 0 }, "/trial"},
		{"negative trial", func(r *Result) { r.Trial = -1 }, "/trial"},

		{"baseline-b without a fixed effort", func(r *Result) { r.Variant = VariantBaselineB }, "/fixedEffort"},
		{"variant-c with a fixed effort", func(r *Result) { r.Variant, r.FixedEffort = VariantC, "high" }, "/fixedEffort"},
		{"baseline-a with a fixed effort", func(r *Result) { r.Variant, r.BellonaVersion, r.FixedEffort = VariantBaselineA, "", "high" }, "/fixedEffort"},
		{"fixed effort with a space", func(r *Result) { r.Variant, r.FixedEffort = VariantBaselineB, "very high" }, "/fixedEffort"},
		{"baseline-a with a Bellona version", func(r *Result) { r.Variant = VariantBaselineA }, "/bellonaVersion"},
		{"variant-c without a Bellona version", func(r *Result) { r.Variant, r.BellonaVersion = VariantC, "" }, "/bellonaVersion"},
		{"Bellona version with a newline", func(r *Result) { r.BellonaVersion = "0.1\n" }, "/bellonaVersion"},

		{"empty role", func(r *Result) { r.Models[0].Role = "" }, "/models/0/role"},
		{"duplicate role", func(r *Result) { r.Models[1].Role = r.Models[0].Role }, "/models/1/role"},
		{"empty requested model", func(r *Result) { r.Models[1].Requested = "" }, "/models/1/requested"},
		{"confirmed model with a newline", func(r *Result) { r.Models[0].Confirmed = "a\nb" }, "/models/0/confirmed"},

		{"zero start", func(r *Result) { r.StartedAt = time.Time{} }, "/startedAt"},
		{"zero finish", func(r *Result) { r.FinishedAt = time.Time{} }, "/finishedAt"},
		{"start not in UTC", func(r *Result) { r.StartedAt = r.StartedAt.In(zone) }, "/startedAt"},
		{"finish not in UTC", func(r *Result) { r.FinishedAt = r.FinishedAt.In(zone) }, "/finishedAt"},
		{"finish before start", func(r *Result) { r.FinishedAt = r.StartedAt.Add(-time.Nanosecond) }, "/finishedAt"},

		{"passed with a non-zero exit code", func(r *Result) {
			r.Acceptance.Passed = true
		}, "/acceptance/passed"},
		{"not passed with every exit code zero", func(r *Result) {
			r.Acceptance.Checks[0].ExitCode = 0
		}, "/acceptance/passed"},
		{"passed with every exit code zero", func(r *Result) {
			r.Acceptance = CheckSummary{Passed: true, Checks: []CheckOutcome{{ID: "a", ExitCode: 0}, {ID: "b", ExitCode: 0}}}
		}, ""},
		{"a negative exit code is a failure", func(r *Result) {
			r.Acceptance = CheckSummary{Passed: false, Checks: []CheckOutcome{{ID: "a", ExitCode: -1}}}
		}, ""},
		{"no checks", func(r *Result) { r.Acceptance = CheckSummary{Passed: true} }, "/acceptance/checks"},
		{"duplicate check id", func(r *Result) {
			r.Acceptance = CheckSummary{Checks: []CheckOutcome{{ID: "a", ExitCode: 1}, {ID: "a", ExitCode: 1}}}
		}, "/acceptance/checks/1/id"},
		{"invalid check id", func(r *Result) { r.Acceptance.Checks[0].ID = "Bad Id" }, "/acceptance/checks/0/id"},

		{"negative regressions", func(r *Result) { r.Regressions = ptr(-1) }, "/regressions"},
		{"negative unnecessary files", func(r *Result) { r.UnnecessaryChangedFiles = ptr(-1) }, "/unnecessaryChangedFiles"},
		{"negative retry count", func(r *Result) { r.RetryCount = ptr(-1) }, "/retryCount"},
		{"negative plan revision count", func(r *Result) { r.PlanRevisionCount = ptr(-1) }, "/planRevisionCount"},
		{"empty tokens", func(r *Result) { r.Tokens = &Tokens{} }, "/tokens"},
		{"negative input tokens", func(r *Result) { r.Tokens.Input = ptr(int64(-1)) }, "/tokens/input"},
		{"negative output tokens", func(r *Result) { r.Tokens.Output = ptr(int64(-1)) }, "/tokens/output"},
		{"negative cache read", func(r *Result) { r.Tokens.CacheRead = ptr(int64(-1)) }, "/tokens/cacheRead"},
		{"negative cache creation", func(r *Result) { r.Tokens.CacheCreation = ptr(int64(-1)) }, "/tokens/cacheCreation"},
		{"negative cost", func(r *Result) { r.APIEquivalentCostUSD = ptr(-0.01) }, "/apiEquivalentCostUsd"},
		{"negative billed cost", func(r *Result) { r.BilledCostUSD = ptr(-1.0) }, "/billedCostUsd"},
		{"NaN cost", func(r *Result) { r.APIEquivalentCostUSD = ptr(math.NaN()) }, "/apiEquivalentCostUsd"},
		{"infinite billed cost", func(r *Result) { r.BilledCostUSD = ptr(math.Inf(1)) }, "/billedCostUsd"},
		{"blank notes are not recorded", func(r *Result) { r.Notes = "  " }, "/notes"},
		{"overlong notes", func(r *Result) { r.Notes = strings.Repeat("n", maxTextBytes+1) }, "/notes"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := validResult(t)
			tt.mod(&r)
			err := r.Validate()
			if tt.pointer == "" {
				if err != nil {
					t.Fatalf("Validate error = %v", err)
				}
				return
			}
			checkError(t, err, strictjson.ErrInvalidValue, tt.pointer)
		})
	}
}

// TestResultValidationThroughDecode checks that Decode applies Validate, so
// that a document the validation rejects never becomes a Result.
func TestResultValidationThroughDecode(t *testing.T) {
	r := validResult(t)
	r.Acceptance.Passed = true // an exit code is 1
	data, err := strictjson.Encode(r)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeResult(data)
	checkError(t, err, strictjson.ErrInvalidValue, "/acceptance/passed")
	if !reflect.DeepEqual(got, Result{}) {
		t.Errorf("DecodeResult returned a non-zero value on error: %+v", got)
	}
}

func TestVariants(t *testing.T) {
	want := []string{"baseline-a", "baseline-b", "variant-c", "variant-d", "variant-e"}
	var got []string
	for _, v := range Variants() {
		got = append(got, string(v))
		if !v.Valid() {
			t.Errorf("%s is not valid", v)
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("Variants() = %v, want %v", got, want)
	}
	if Variant("").Valid() || Variant("Baseline-A").Valid() {
		t.Error("an unknown variant is valid")
	}
}

func TestContractTypes(t *testing.T) {
	for _, typ := range []reflect.Type{reflect.TypeFor[Task](), reflect.TypeFor[Result]()} {
		if err := strictjson.CheckContractType(typ); err != nil {
			t.Errorf("%s: %v", typ, err)
		}
	}
}

// jsonNames lists the json member names of the struct types reachable from
// t, keyed by the Go type that declares them.
func jsonNames(t reflect.Type, out map[reflect.Type][]string) {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || t.PkgPath() != reflect.TypeFor[Result]().PkgPath() || out[t] != nil {
		return
	}
	out[t] = []string{}
	for i := range t.NumField() {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		out[t] = append(out[t], name)
		jsonNames(f.Type, out)
	}
}

// TestResultMembers pins the members of a result record. A new metric is a
// deliberate change of this list, and there is no member for a model's
// opinion of its own work (SPEC §19: model self-evaluation is not a success
// metric).
func TestResultMembers(t *testing.T) {
	want := []string{
		"schemaVersion", "taskId", "variant", "trial", "fixedEffort", "bellonaVersion", "models", "startedAt", "finishedAt",
		"acceptance", "regressions", "unnecessaryChangedFiles", "tokens", "apiEquivalentCostUsd", "billedCostUsd", "retryCount",
		"planRevisionCount", "notes",
	}
	all := map[reflect.Type][]string{}
	jsonNames(reflect.TypeFor[Result](), all)
	if got := all[reflect.TypeFor[Result]()]; !slices.Equal(got, want) {
		t.Errorf("members of Result:\n got %v\nwant %v", got, want)
	}
	for typ, names := range all {
		for _, name := range names {
			lower := strings.ToLower(name)
			for _, banned := range []string{"self", "score", "confidence", "rating", "opinion", "judg", "grade", "quality"} {
				if strings.Contains(lower, banned) {
					t.Errorf("%s has the member %q, which looks like a model's own judgement", typ, name)
				}
			}
		}
	}
}

// ---------------------------------------------------------------------------
// The sample tasks start unsolved
// ---------------------------------------------------------------------------

// runCommand runs c in dir with the local Go toolchain and returns its exit
// status and combined output. Failing to start the program, or running out
// of time, fails the test: it is not an exit status.
func runCommand(t *testing.T, dir string, c Command) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Argv[0], c.Argv[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local")
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0, string(out)
	case errors.As(err, &exit) && ctx.Err() == nil:
		return exit.ExitCode(), string(out)
	default:
		t.Fatalf("%s: %v\n%s", c.ID, err, out)
		return 0, ""
	}
}

// copyAcceptanceFiles copies the regular files of src into dst. It fails if a
// file already exists in dst: the acceptance files are hidden, so the initial
// repository must not contain them.
func copyAcceptanceFiles(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o755)
		case !d.Type().IsRegular():
			return fmt.Errorf("%s is not a regular file", p)
		}
		if _, err := os.Lstat(target); err == nil {
			return fmt.Errorf("%s already exists in the initial repository", rel)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatalf("copy the acceptance files: %v", err)
	}
}

func TestSampleTasksStartUnsolved(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the Go toolchain in temporary repositories; skipped under -short")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("the go binary is not on PATH")
	}
	for _, dir := range sampleTaskDirs(t) {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			task, err := LoadTask(dir)
			if err != nil {
				t.Fatal(err)
			}
			if task.Repository.Kind != RepositoryDirectory {
				t.Skipf("repository kind %q is not prepared here", task.Repository.Kind)
			}
			repo := gitrepo.FromDir(t, filepath.Join(dir, filepath.FromSlash(task.Repository.Path)))

			for _, c := range task.Setup {
				if exit, out := runCommand(t, repo, c); exit != 0 {
					t.Fatalf("setup %s exited with %d:\n%s", c.ID, exit, out)
				}
			}
			for _, c := range task.Acceptance.RegressionChecks {
				if exit, out := runCommand(t, repo, c); exit != 0 {
					t.Errorf("regression check %s fails on the initial repository (exit %d):\n%s", c.ID, exit, out)
				}
			}

			copyAcceptanceFiles(t, filepath.Join(dir, filepath.FromSlash(task.Acceptance.Files)), repo)
			failed := 0
			for _, c := range task.Acceptance.Checks {
				exit, out := runCommand(t, repo, c)
				t.Logf("acceptance check %s on the initial repository: exit %d", c.ID, exit)
				if exit != 0 {
					failed++
				} else {
					t.Logf("output:\n%s", out)
				}
			}
			if failed == 0 {
				t.Error("every acceptance check passes on the initial repository: the task is already solved")
			}
		})
	}
}
