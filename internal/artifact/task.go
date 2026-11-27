// Package artifact holds the typed artifact contracts of Bellona (SPEC §8).
// Each contract is a versioned Go struct that follows the convention of
// package strictjson and validates its own constraints; the Decode functions
// apply the strict rules of R-ART-01 to untrusted input.
package artifact

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/shutx-net/bellona/internal/strictjson"
)

const (
	// TaskSchemaVersion is the schema version of task.json.
	TaskSchemaVersion = 1
	// MaxRequestBytes is the largest accepted user request. A longer request
	// is rejected, never truncated.
	MaxRequestBytes = 65536
)

// invalid returns a strictjson error that names the JSON Pointer of the
// offending field, so that Decode reports where a constraint failed.
func invalid(pointer, format string, args ...any) error {
	return &strictjson.Error{
		Pointer: pointer,
		Err:     strictjson.ErrInvalidValue,
		Detail:  fmt.Sprintf(format, args...),
	}
}

// Task is the original user request of a Run, stored as task.json.
type Task struct {
	SchemaVersion int    `json:"schemaVersion"`
	Request       string `json:"request"`
}

// Validate checks the schema version and the request: valid UTF-8, not empty
// after trimming white space, and at most MaxRequestBytes bytes.
func (t Task) Validate() error {
	if t.SchemaVersion != TaskSchemaVersion {
		return invalid("/schemaVersion", "schema version %d is not supported, want %d", t.SchemaVersion, TaskSchemaVersion)
	}
	switch {
	case !utf8.ValidString(t.Request):
		return invalid("/request", "not valid UTF-8")
	case strings.TrimSpace(t.Request) == "":
		return invalid("/request", "the request is empty")
	case len(t.Request) > MaxRequestBytes:
		return invalid("/request", "%d bytes exceed the limit of %d", len(t.Request), MaxRequestBytes)
	}
	return nil
}

// DecodeTask strictly decodes and validates a task.json document.
func DecodeTask(data []byte) (Task, error) {
	return strictjson.Decode[Task](data)
}
