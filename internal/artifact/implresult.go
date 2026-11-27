package artifact

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/shutx-net/bellona/internal/strictjson"
)

// ImplementationResultSchemaVersion is the schema version of an
// Implementation result.
const ImplementationResultSchemaVersion = 1

// MaxTextBytes is the largest accepted summary or issue text.
const MaxTextBytes = 4096

// ErrInvalidStatus is wrapped by the error for an unknown ResultStatus.
var ErrInvalidStatus = errors.New("invalid result status")

// ResultStatus is the status that an Implementer reports (SPEC §8). An
// Implementer that needs a user decision returns the common needs_user_input
// output instead, which is not part of this contract.
type ResultStatus string

// The candidate statuses of an Implementation result.
const (
	StatusImplemented ResultStatus = "implemented"
	StatusNeedsReplan ResultStatus = "needs_replan"
	StatusBlocked     ResultStatus = "blocked"
	StatusFailed      ResultStatus = "failed"
)

var resultStatuses = []ResultStatus{StatusImplemented, StatusNeedsReplan, StatusBlocked, StatusFailed}

// Valid reports whether s is one of the four statuses.
func (s ResultStatus) Valid() bool { return slices.Contains(resultStatuses, s) }

// String returns the status name.
func (s ResultStatus) String() string { return string(s) }

// MarshalText implements encoding.TextMarshaler and refuses an unknown status.
func (s ResultStatus) MarshalText() ([]byte, error) {
	if !s.Valid() {
		return nil, fmt.Errorf("%w: %q", ErrInvalidStatus, string(s))
	}
	return []byte(s), nil
}

// UnmarshalText implements encoding.TextUnmarshaler and refuses an unknown
// status, including the empty string.
func (s *ResultStatus) UnmarshalText(b []byte) error {
	v := ResultStatus(b)
	if !v.Valid() {
		return fmt.Errorf("%w: %q", ErrInvalidStatus, string(b))
	}
	*s = v
	return nil
}

// ImplementationResult is the output contract of an Implementer (SPEC §8,
// "Implementation result"). It is the one typed artifact that the fake runtime
// returns in M0.
//
// The contract is provisional: M1b (R-ART-07) finalizes the field types that
// the SPEC example leaves open. "implemented" is an agent report, distinct
// from acceptance by Bellona; the engine compares ReportedChangedFiles with
// the actual diff.
//
// Every field is required. The two lists may be empty.
type ImplementationResult struct {
	SchemaVersion        int          `json:"schemaVersion"`
	Status               ResultStatus `json:"status"`
	Summary              string       `json:"summary"`
	ReportedChangedFiles []string     `json:"reportedChangedFiles"`
	Issues               []string     `json:"issues"`
}

// Validate checks the schema version, the status, the summary (valid UTF-8,
// not empty after trimming white space, at most MaxTextBytes bytes), the
// reported files (repository-relative, slash-separated, in clean form and
// unique) and the issues (each non-empty and at most MaxTextBytes bytes).
func (r ImplementationResult) Validate() error {
	if r.SchemaVersion != ImplementationResultSchemaVersion {
		return invalid("/schemaVersion", "schema version %d is not supported, want %d",
			r.SchemaVersion, ImplementationResultSchemaVersion)
	}
	if !r.Status.Valid() {
		return invalid("/status", "unknown status %q", string(r.Status))
	}
	if err := checkText(r.Summary); err != nil {
		return invalid("/summary", "%v", err)
	}
	seen := make(map[string]int, len(r.ReportedChangedFiles))
	for i, f := range r.ReportedChangedFiles {
		pointer := "/reportedChangedFiles/" + strconv.Itoa(i)
		if err := validateRepoPath(f); err != nil {
			return invalid(pointer, "%v", err)
		}
		if first, dup := seen[f]; dup {
			return invalid(pointer, "%q is already reported at index %d", f, first)
		}
		seen[f] = i
	}
	for i, issue := range r.Issues {
		if err := checkText(issue); err != nil {
			return invalid("/issues/"+strconv.Itoa(i), "%v", err)
		}
	}
	return nil
}

// DecodeImplementationResult strictly decodes and validates an Implementation
// result.
func DecodeImplementationResult(data []byte) (ImplementationResult, error) {
	return strictjson.Decode[ImplementationResult](data)
}

// checkText checks a summary or an issue: valid UTF-8, not empty after
// trimming white space, and at most MaxTextBytes bytes.
func checkText(s string) error {
	switch {
	case !utf8.ValidString(s):
		return errors.New("not valid UTF-8")
	case strings.TrimSpace(s) == "":
		return errors.New("the text is empty")
	case len(s) > MaxTextBytes:
		return fmt.Errorf("%d bytes exceed the limit of %d", len(s), MaxTextBytes)
	}
	return nil
}

// validateRepoPath checks a repository-relative, slash-separated file path as
// an Implementer reports it. Unlike a Run-relative path, it may start a
// segment with a dot (.gitignore, .github/...) and has no length limit.
func validateRepoPath(p string) error {
	switch {
	case p == "":
		return errors.New("empty path")
	case !utf8.ValidString(p):
		return errors.New("path is not valid UTF-8")
	case strings.ContainsRune(p, 0):
		return errors.New("path contains a NUL byte")
	case strings.ContainsRune(p, '\\'):
		return fmt.Errorf("%q contains a backslash", p)
	case strings.HasPrefix(p, "/"):
		return fmt.Errorf("%q is absolute", p)
	}
	for seg := range strings.SplitSeq(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("%q has an empty, \".\" or \"..\" segment", p)
		}
	}
	if path.Clean(p) != p {
		return fmt.Errorf("%q is not in clean form", p)
	}
	return nil
}
