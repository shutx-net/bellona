// Package evals holds the formats of the evaluation infrastructure of SPEC
// §19: the task that every variant of a comparative evaluation receives, and
// the result record of one trial. M0 begins the infrastructure (R-TECH-03)
// with the two formats, their strict validation and a first set of sample
// tasks under evals/tasks; running trials and comparing variants is R-TECH-04
// and belongs to M4.
//
// Both formats follow the contract convention of package strictjson: members
// are required unless their json tag carries omitzero, JSON null is never
// valid, and a value that was not measured is an absent member, never zero.
// The package reads task files and checks their layout on disk, but it never
// writes, deletes or runs anything.
package evals

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/shutx-net/bellona/internal/strictjson"
)

const (
	// TaskSchemaVersion is the schema version of task.json.
	TaskSchemaVersion = 1

	// MaxTaskFileBytes is the largest accepted task.json. A larger file is
	// rejected, never truncated.
	MaxTaskFileBytes = 1 << 20

	// MaxRequestBytes is the largest accepted request. It equals the limit of
	// the Run's task artifact (artifact.MaxRequestBytes), because Bellona
	// variants receive the request as that artifact.
	MaxRequestBytes = 65536

	maxTitleBytes = 200
	maxTextBytes  = 4096
)

// The kinds of Repository.
const (
	// RepositoryDirectory names a task-relative directory whose content
	// becomes the initial commit of a fresh Git repository.
	RepositoryDirectory = "directory"
	// RepositoryGit names a remote repository at one commit.
	RepositoryGit = "git"
)

// ErrInvalidTask reports a task directory whose layout on disk is wrong: the
// directory name is not the task id, a referenced directory is missing, is
// not a directory, lies outside the task directory or contains a symbolic
// link, or task.json is too large. Problems inside task.json are
// *strictjson.Error values.
var ErrInvalidTask = errors.New("invalid evaluation task")

var (
	idPattern     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
	tokenPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,31}$`)
)

// invalid returns a strictjson error that names the JSON Pointer of the
// offending member, so that Decode reports where a constraint failed.
func invalid(pointer, format string, args ...any) error {
	return &strictjson.Error{
		Pointer: pointer,
		Err:     strictjson.ErrInvalidValue,
		Detail:  fmt.Sprintf(format, args...),
	}
}

// Task is one evaluation task, stored as task.json in a directory that is
// named after the task id. Every variant of a comparative evaluation
// receives the same task, the same initial repository and the same checks
// (SPEC §19).
type Task struct {
	SchemaVersion int `json:"schemaVersion"`
	// ID names the task and its directory.
	ID    string `json:"id"`
	Title string `json:"title"`
	// Repository is the initial code.
	Repository Repository `json:"repository"`
	// Request is the user request that is given to the variant, verbatim.
	Request string `json:"request"`
	// Setup lists commands that run once in the prepared repository before
	// the request is given to the variant.
	Setup []Command `json:"setup,omitzero"`
	// Acceptance defines the independent judgement of a trial.
	Acceptance Acceptance `json:"acceptance"`
	// ExpectedChangeScope lists the change-scope patterns of the files that a
	// correct solution touches (SPEC §8: "/"-separated repository-relative
	// paths, "*" within one segment, "**" for any number of segments). A
	// file that a trial changes outside them counts as an unnecessary change.
	ExpectedChangeScope []string `json:"expectedChangeScope,omitzero"`
	Tags                []string `json:"tags,omitzero"`
}

// Repository is the initial code of a task.
type Repository struct {
	// Kind is RepositoryDirectory or RepositoryGit.
	Kind string `json:"kind"`
	// Path is a task-relative directory (kind directory only).
	Path string `json:"path,omitzero"`
	// URL is the location of the repository (kind git only).
	URL string `json:"url,omitzero"`
	// Commit is a 40-character lowercase hexadecimal commit ID (kind git
	// only).
	Commit string `json:"commit,omitzero"`
}

// Command is a command that a trial runs in the repository. It is an argument
// array, never a shell string (SPEC §14).
type Command struct {
	// ID names the command in a result record.
	ID   string   `json:"id"`
	Argv []string `json:"argv"`
	// Timeout is a positive Go duration such as "2m". Absent means that the
	// runner applies its own limit.
	Timeout string `json:"timeout,omitzero"`
}

// Acceptance defines how a trial is judged without asking the model.
type Acceptance struct {
	// Files is a task-relative directory of hidden acceptance tests. The
	// runner copies its content over the repository after the variant has
	// finished, so the variant never sees it.
	Files string `json:"files,omitzero"`
	// Checks must all exit 0 for the trial to be accepted.
	Checks []Command `json:"checks"`
	// RegressionChecks pass on the initial repository; a failure after the
	// run counts as a regression.
	RegressionChecks []Command `json:"regressionChecks,omitzero"`
}

// Validate checks the task: the schema version, the id, the texts, the
// repository, the commands, the acceptance definition, the change scope and
// the tags. Decode calls Validate on the top-level value only, so it also
// validates every nested value.
func (t Task) Validate() error {
	if t.SchemaVersion != TaskSchemaVersion {
		return invalid("/schemaVersion", "schema version %d is not supported, want %d", t.SchemaVersion, TaskSchemaVersion)
	}
	if !idPattern.MatchString(t.ID) {
		return invalid("/id", "%q does not match %s", t.ID, idPattern)
	}
	if err := checkText("/title", t.Title, maxTitleBytes); err != nil {
		return err
	}
	if err := checkText("/request", t.Request, MaxRequestBytes); err != nil {
		return err
	}
	if err := t.Repository.validate("/repository"); err != nil {
		return err
	}
	if err := validateCommands("/setup", t.Setup, false); err != nil {
		return err
	}
	if err := t.Acceptance.validate("/acceptance"); err != nil {
		return err
	}
	if t.Repository.Kind == RepositoryDirectory && t.Acceptance.Files != "" &&
		overlaps(t.Repository.Path, t.Acceptance.Files) {
		return invalid("/acceptance/files", "%q and the repository directory %q overlap, so the acceptance files would not be hidden",
			t.Acceptance.Files, t.Repository.Path)
	}
	for i, pattern := range t.ExpectedChangeScope {
		if err := checkScopePattern(pattern); err != nil {
			return invalid(fmt.Sprintf("/expectedChangeScope/%d", i), "%v", err)
		}
	}
	seen := make(map[string]bool, len(t.Tags))
	for i, tag := range t.Tags {
		switch {
		case !idPattern.MatchString(tag):
			return invalid(fmt.Sprintf("/tags/%d", i), "%q does not match %s", tag, idPattern)
		case seen[tag]:
			return invalid(fmt.Sprintf("/tags/%d", i), "duplicate tag %q", tag)
		}
		seen[tag] = true
	}
	return nil
}

// checkText requires valid UTF-8 that is not blank and at most limit bytes.
func checkText(pointer, s string, limit int) error {
	switch {
	case !utf8.ValidString(s):
		return invalid(pointer, "not valid UTF-8")
	case strings.TrimSpace(s) == "":
		return invalid(pointer, "must not be empty")
	case len(s) > limit:
		return invalid(pointer, "%d bytes exceed the limit of %d", len(s), limit)
	}
	return nil
}

func (r Repository) validate(pointer string) error {
	switch r.Kind {
	case RepositoryDirectory:
		if r.Path == "" {
			return invalid(pointer+"/path", "a repository of kind %s needs a path", RepositoryDirectory)
		}
		if err := checkRelativePath(r.Path); err != nil {
			return invalid(pointer+"/path", "%v", err)
		}
		if r.URL != "" || r.Commit != "" {
			return invalid(pointer, "a repository of kind %s has no url and no commit", RepositoryDirectory)
		}
	case RepositoryGit:
		if r.Path != "" {
			return invalid(pointer+"/path", "a repository of kind %s has no path", RepositoryGit)
		}
		switch {
		case r.URL == "":
			return invalid(pointer+"/url", "a repository of kind %s needs a url", RepositoryGit)
		case !utf8.ValidString(r.URL) || strings.ContainsFunc(r.URL, isSpaceOrControl):
			return invalid(pointer+"/url", "contains white space, a control character or invalid UTF-8")
		case strings.HasPrefix(r.URL, "-"):
			return invalid(pointer+"/url", "must not start with %q, so that it cannot be read as an option", "-")
		case !commitPattern.MatchString(r.Commit):
			return invalid(pointer+"/commit", "%q is not a 40-character lowercase hexadecimal commit ID", r.Commit)
		}
	default:
		return invalid(pointer+"/kind", "%q is not %q or %q", r.Kind, RepositoryDirectory, RepositoryGit)
	}
	return nil
}

func isSpaceOrControl(r rune) bool { return r <= ' ' || r == 0x7f || r == 0x85 || r == 0xa0 }

func (c Command) validate(pointer string) error {
	if !idPattern.MatchString(c.ID) {
		return invalid(pointer+"/id", "%q does not match %s", c.ID, idPattern)
	}
	if len(c.Argv) == 0 || c.Argv[0] == "" {
		return invalid(pointer+"/argv", "the command needs a program as its first element")
	}
	for i, arg := range c.Argv {
		if !utf8.ValidString(arg) || strings.ContainsRune(arg, 0) {
			return invalid(fmt.Sprintf("%s/argv/%d", pointer, i), "not valid UTF-8 or contains a NUL byte")
		}
	}
	if c.Timeout != "" {
		d, err := time.ParseDuration(c.Timeout)
		if err != nil || d <= 0 {
			return invalid(pointer+"/timeout", "%q is not a positive duration such as \"2m\"", c.Timeout)
		}
	}
	return nil
}

// validateCommands validates a list of commands with unique ids. A list that
// is required must not be empty.
func validateCommands(pointer string, list []Command, required bool) error {
	if required && len(list) == 0 {
		return invalid(pointer, "at least one command is required")
	}
	seen := make(map[string]bool, len(list))
	for i, c := range list {
		at := fmt.Sprintf("%s/%d", pointer, i)
		if err := c.validate(at); err != nil {
			return err
		}
		if seen[c.ID] {
			return invalid(at+"/id", "duplicate command id %q", c.ID)
		}
		seen[c.ID] = true
	}
	return nil
}

func (a Acceptance) validate(pointer string) error {
	if a.Files != "" {
		if err := checkRelativePath(a.Files); err != nil {
			return invalid(pointer+"/files", "%v", err)
		}
	}
	if err := validateCommands(pointer+"/checks", a.Checks, true); err != nil {
		return err
	}
	return validateCommands(pointer+"/regressionChecks", a.RegressionChecks, false)
}

// checkRelativePath checks the syntax of a task-relative or
// repository-relative path: "/"-separated, not absolute, with no empty, "."
// or ".." segment and no backslash or control character.
func checkRelativePath(p string) error {
	switch {
	case p == "":
		return errors.New("the path is empty")
	case !utf8.ValidString(p):
		return errors.New("the path is not valid UTF-8")
	case strings.HasPrefix(p, "/"):
		return fmt.Errorf("%q is absolute; it must be relative", p)
	case strings.ContainsFunc(p, func(r rune) bool { return r < ' ' || r == 0x7f || r == '\\' }):
		return fmt.Errorf("%q contains a backslash or a control character", p)
	}
	for seg := range strings.SplitSeq(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("%q must be a clean path without empty, \".\" or \"..\" segments", p)
		}
	}
	return nil
}

// checkScopePattern checks a change-scope pattern against the grammar of SPEC
// §8: absolute paths, ".." segments and negation are rejected, and "**" is a
// whole segment.
func checkScopePattern(p string) error {
	if strings.HasPrefix(p, "!") {
		return fmt.Errorf("%q: negation is not supported", p)
	}
	if err := checkRelativePath(p); err != nil {
		return err
	}
	for seg := range strings.SplitSeq(p, "/") {
		if strings.Contains(seg, "**") && seg != "**" {
			return fmt.Errorf("%q: \"**\" must be a whole path segment", p)
		}
	}
	return nil
}

// overlaps reports whether the relative paths a and b are equal or one lies
// inside the other.
func overlaps(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

// DecodeTask strictly decodes and validates a task.json document.
func DecodeTask(data []byte) (Task, error) {
	return strictjson.Decode[Task](data)
}

// LoadTask reads and validates the task in dir. It reads dir/task.json (at
// most MaxTaskFileBytes), decodes it strictly, requires the task id to equal
// the name of dir, and requires the repository directory and the acceptance
// directory, when the task uses them, to be directories inside dir that are
// reached and filled without any symbolic link. Layout problems wrap
// ErrInvalidTask.
func LoadTask(dir string) (Task, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Task{}, err
	}
	data, err := readTaskFile(filepath.Join(abs, "task.json"))
	if err != nil {
		return Task{}, err
	}
	task, err := DecodeTask(data)
	if err != nil {
		return Task{}, fmt.Errorf("evals: %s: %w", filepath.Join(dir, "task.json"), err)
	}
	if want := filepath.Base(abs); task.ID != want {
		return Task{}, fmt.Errorf("%w: the id %q of %s is not the directory name %q", ErrInvalidTask, task.ID, filepath.Join(dir, "task.json"), want)
	}
	if task.Repository.Kind == RepositoryDirectory {
		if err := checkTaskDir(abs, task.Repository.Path); err != nil {
			return Task{}, err
		}
	}
	if task.Acceptance.Files != "" {
		if err := checkTaskDir(abs, task.Acceptance.Files); err != nil {
			return Task{}, err
		}
	}
	return task, nil
}

// readTaskFile reads a regular file of at most MaxTaskFileBytes.
func readTaskFile(name string) ([]byte, error) {
	info, err := os.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s is not a regular file", ErrInvalidTask, name)
	}
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxTaskFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxTaskFileBytes {
		return nil, fmt.Errorf("%w: %s is larger than %d bytes", ErrInvalidTask, name, MaxTaskFileBytes)
	}
	return data, nil
}

// checkTaskDir requires rel, a validated task-relative path, to name a
// directory below root, checks every element of the path with Lstat so that
// no symbolic link is followed, and requires every entry below the directory
// to be a regular file or a directory.
func checkTaskDir(root, rel string) error {
	cur := root
	for seg := range strings.SplitSeq(rel, "/") {
		cur = filepath.Join(cur, seg)
		info, err := os.Lstat(cur)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidTask, err)
		}
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			return fmt.Errorf("%w: %s is a symbolic link", ErrInvalidTask, cur)
		case !info.IsDir():
			return fmt.Errorf("%w: %s is not a directory", ErrInvalidTask, cur)
		}
	}
	return filepath.WalkDir(cur, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && !d.Type().IsRegular() {
			return fmt.Errorf("%w: %s is a symbolic link or another irregular file", ErrInvalidTask, p)
		}
		return nil
	})
}
