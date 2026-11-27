// Package domain holds the pure domain model of Bellona: identifiers, the
// states of the three state machines, roles, artifact references and the
// Run-relative file layout. It performs no I/O, reads no clock and draws no
// randomness of its own, so that replay stays free of side effects (R-PER-06).
package domain

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
)

// ErrInvalidID is wrapped by every identifier validation error.
var ErrInvalidID = errors.New("invalid identifier")

var (
	runIDPattern     = regexp.MustCompile(`^run-[0-9a-f]{16}$`)
	attemptIDPattern = regexp.MustCompile(`^attempt-[0-9a-f]{16}$`)
	requestIDPattern = regexp.MustCompile(`^req-[0-9a-f]{16}$`)
	sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	phaseIDPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

// checkID returns an error wrapping ErrInvalidID unless s matches pattern.
func checkID(kind, s string, pattern *regexp.Regexp) error {
	if pattern.MatchString(s) {
		return nil
	}
	if len(s) > 80 {
		s = s[:80] + "..."
	}
	return fmt.Errorf("%w: %s %q does not match %s", ErrInvalidID, kind, s, pattern)
}

// randomHex reads n random bytes from r and returns them as lowercase hex.
func randomHex(r io.Reader, n int) (string, error) {
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// RunID identifies a Run: "run-" and 16 lowercase hex digits.
type RunID string

// NewRunID returns a RunID built from 8 bytes read from r. The engine passes
// crypto/rand.Reader; tests pass a deterministic reader.
func NewRunID(r io.Reader) (RunID, error) {
	h, err := randomHex(r, 8)
	if err != nil {
		return "", err
	}
	return RunID("run-" + h), nil
}

// ParseRunID returns s as a RunID, or an error wrapping ErrInvalidID.
func ParseRunID(s string) (RunID, error) {
	id := RunID(s)
	return id, id.Validate()
}

// Validate reports whether id is a well-formed RunID.
func (id RunID) Validate() error { return checkID("run ID", string(id), runIDPattern) }

// String returns the identifier.
func (id RunID) String() string { return string(id) }

// MarshalText implements encoding.TextMarshaler and refuses an invalid ID.
func (id RunID) MarshalText() ([]byte, error) { return marshalID(id) }

// UnmarshalText implements encoding.TextUnmarshaler and refuses an invalid ID.
func (id *RunID) UnmarshalText(b []byte) error { return unmarshalID(id, b) }

// AttemptID identifies one Attempt: "attempt-" and 16 lowercase hex digits.
type AttemptID string

// NewAttemptID returns an AttemptID built from 8 bytes read from r.
func NewAttemptID(r io.Reader) (AttemptID, error) {
	h, err := randomHex(r, 8)
	if err != nil {
		return "", err
	}
	return AttemptID("attempt-" + h), nil
}

// ParseAttemptID returns s as an AttemptID, or an error wrapping ErrInvalidID.
func ParseAttemptID(s string) (AttemptID, error) {
	id := AttemptID(s)
	return id, id.Validate()
}

// Validate reports whether id is a well-formed AttemptID.
func (id AttemptID) Validate() error { return checkID("attempt ID", string(id), attemptIDPattern) }

// String returns the identifier.
func (id AttemptID) String() string { return string(id) }

// MarshalText implements encoding.TextMarshaler and refuses an invalid ID.
func (id AttemptID) MarshalText() ([]byte, error) { return marshalID(id) }

// UnmarshalText implements encoding.TextUnmarshaler and refuses an invalid ID.
func (id *AttemptID) UnmarshalText(b []byte) error { return unmarshalID(id, b) }

// RequestID identifies an interaction request: "req-" and 16 lowercase hex
// digits. Interaction requests arrive in M2; suspension payloads use the type
// from M0 on.
type RequestID string

// NewRequestID returns a RequestID built from 8 bytes read from r.
func NewRequestID(r io.Reader) (RequestID, error) {
	h, err := randomHex(r, 8)
	if err != nil {
		return "", err
	}
	return RequestID("req-" + h), nil
}

// ParseRequestID returns s as a RequestID, or an error wrapping ErrInvalidID.
func ParseRequestID(s string) (RequestID, error) {
	id := RequestID(s)
	return id, id.Validate()
}

// Validate reports whether id is a well-formed RequestID.
func (id RequestID) Validate() error { return checkID("request ID", string(id), requestIDPattern) }

// String returns the identifier.
func (id RequestID) String() string { return string(id) }

// MarshalText implements encoding.TextMarshaler and refuses an invalid ID.
func (id RequestID) MarshalText() ([]byte, error) { return marshalID(id) }

// UnmarshalText implements encoding.TextUnmarshaler and refuses an invalid ID.
func (id *RequestID) UnmarshalText(b []byte) error { return unmarshalID(id, b) }

// SessionID identifies an Agent Session. The core treats it as opaque because
// every host reports its own format: it must start with a letter or digit,
// continue with letters, digits, '.', '_', ':' or '-', and be at most 128
// bytes long.
type SessionID string

// NewSessionID returns a lowercase RFC 4122 version 4 UUID built from 16
// bytes read from r, because the first host takes a UUID as its session ID.
func NewSessionID(r io.Reader) (SessionID, error) {
	var b [16]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	b[6] = b[6]&0x0f | 0x40 // version 4
	b[8] = b[8]&0x3f | 0x80 // variant 10
	h := hex.EncodeToString(b[:])
	return SessionID(h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]), nil
}

// ParseSessionID returns s as a SessionID, or an error wrapping ErrInvalidID.
func ParseSessionID(s string) (SessionID, error) {
	id := SessionID(s)
	return id, id.Validate()
}

// Validate reports whether id is a well-formed SessionID.
func (id SessionID) Validate() error { return checkID("session ID", string(id), sessionIDPattern) }

// String returns the identifier.
func (id SessionID) String() string { return string(id) }

// MarshalText implements encoding.TextMarshaler and refuses an invalid ID.
func (id SessionID) MarshalText() ([]byte, error) { return marshalID(id) }

// UnmarshalText implements encoding.TextUnmarshaler and refuses an invalid ID.
func (id *SessionID) UnmarshalText(b []byte) error { return unmarshalID(id, b) }

// PhaseID identifies a Phase within a plan. The Planner chooses it (for
// example "phase-01"): it must start with a letter or digit, continue with
// letters, digits, '.', '_' or '-', and be at most 64 bytes long.
type PhaseID string

// ParsePhaseID returns s as a PhaseID, or an error wrapping ErrInvalidID.
func ParsePhaseID(s string) (PhaseID, error) {
	id := PhaseID(s)
	return id, id.Validate()
}

// Validate reports whether id is a well-formed PhaseID.
func (id PhaseID) Validate() error { return checkID("phase ID", string(id), phaseIDPattern) }

// String returns the identifier.
func (id PhaseID) String() string { return string(id) }

// MarshalText implements encoding.TextMarshaler and refuses an invalid ID.
func (id PhaseID) MarshalText() ([]byte, error) { return marshalID(id) }

// UnmarshalText implements encoding.TextUnmarshaler and refuses an invalid ID.
func (id *PhaseID) UnmarshalText(b []byte) error { return unmarshalID(id, b) }

// identifier is the method set shared by every identifier type.
type identifier interface {
	~string
	Validate() error
}

func marshalID[T identifier](id T) ([]byte, error) {
	if err := id.Validate(); err != nil {
		return nil, err
	}
	return []byte(id), nil
}

func unmarshalID[T identifier](dst *T, b []byte) error {
	id := T(b)
	if err := id.Validate(); err != nil {
		return err
	}
	*dst = id
	return nil
}
