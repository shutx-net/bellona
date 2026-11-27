package domain

import (
	"errors"
	"fmt"
	"reflect"
	"time"
	"unicode/utf8"

	"encoding/json/jsontext"
	json "encoding/json/v2"

	"github.com/shutx-net/bellona/internal/strictjson"
)

const (
	// EventSchemaVersion is the schema version of the event envelope and of
	// every payload in the catalogue (SPEC §12.2). A record with another
	// version is reported as incompatible, never as corrupt.
	EventSchemaVersion = 1
	// MaxEventSize is the largest encoded record in bytes, the newline that
	// ends a journal line excluded (R-PER-07). Events carry small data and
	// artifact references; plans, diffs and logs are artifacts.
	MaxEventSize = 65536
	// MaxTextBytes is the longest summary or detail text in a payload.
	MaxTextBytes = 1024
)

// Sentinel errors of the event codec.
var (
	// ErrUnsupportedEventSchema is wrapped by the error that DecodeEvent
	// returns for a record whose schemaVersion differs from
	// EventSchemaVersion. It does not match ErrInvalidEvent: a record of a
	// newer contract is incompatible, not corrupt.
	ErrUnsupportedEventSchema = errors.New("unsupported event schema version")
	// ErrUnknownEventType is wrapped by the error for an event type that is
	// not in the catalogue. It also matches ErrInvalidEvent.
	ErrUnknownEventType = errors.New("unknown event type")
	// ErrEventTooLarge is wrapped by the error for a record above
	// MaxEventSize. It also matches ErrInvalidEvent.
	ErrEventTooLarge = errors.New("event record too large")
	// ErrInvalidEvent is wrapped by every other error of EncodeEvent and
	// DecodeEvent: an event that breaks a rule of the envelope or of its
	// payload, or a record that is not strictly valid JSON for its type. The
	// strictjson sentinel that classifies the failure (ErrUnknownMember,
	// ErrMissingMember, ErrNull and so on) matches as well.
	ErrInvalidEvent = errors.New("invalid event")
)

// EventType names an event type of the catalogue. The value is the bare Go
// name of the payload type, for example "RunCreated".
type EventType string

// String returns the event type.
func (t EventType) String() string { return string(t) }

// Payload is the data of one event. Every payload type of the catalogue is a
// struct that follows the contract convention of package strictjson, is used
// as a value (never as a pointer) and validates its own rules. A payload
// embeds no plan, diff, log or agent output: such content is an artifact that
// the payload refers to by ArtifactRef (R-PER-07).
type Payload interface {
	// EventType returns the event type of the payload.
	EventType() EventType
	// Validate checks the rules of the payload that its shape cannot
	// express, including the identifiers, artifact references and nested
	// values that the payload holds (strictjson does not validate nested
	// values on its own). Errors are *strictjson.Error values whose JSON
	// Pointer is relative to the payload.
	Validate() error
	// ArtifactRefs returns every non-zero artifact reference of the payload
	// in field order, or nil.
	ArtifactRefs() []ArtifactRef
}

// Event is one committed event: the envelope and its payload.
type Event struct {
	SchemaVersion int
	Seq           uint64
	Type          EventType
	RunID         RunID
	// Time is the commit time. It must be non-zero and in UTC.
	Time    time.Time
	Payload Payload
}

// wireEvent is the JSON form of an event record, one object per journal
// line. The members appear in this order:
//
//	{"schemaVersion":1,"seq":1,"type":"RunCreated","runId":"run-0123456789abcdef",
//	 "time":"2026-11-28T00:00:00.123456789Z","payload":{...}}
//
// The time is RFC 3339 with the nanoseconds that it has, in UTC with the
// suffix Z.
type wireEvent struct {
	SchemaVersion int            `json:"schemaVersion"`
	Seq           uint64         `json:"seq"`
	Type          EventType      `json:"type"`
	RunID         RunID          `json:"runId"`
	Time          time.Time      `json:"time"`
	Payload       jsontext.Value `json:"payload"`
}

// invalidEvent returns an error that wraps ErrInvalidEvent.
func invalidEvent(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidEvent, fmt.Sprintf(format, args...))
}

// checkEnvelope checks the members of the envelope that do not depend on the
// payload.
func checkEnvelope(seq uint64, runID RunID, t time.Time) error {
	if seq < 1 {
		return invalidEvent("seq is 0, want 1 or more")
	}
	if err := runID.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidEvent, err)
	}
	if t.IsZero() {
		return invalidEvent("time is not set")
	}
	if t.Location() != time.UTC {
		return invalidEvent("time %s is not in UTC", t.Format(time.RFC3339Nano))
	}
	return nil
}

// EncodeEvent returns the journal record of e: one line of JSON without the
// terminating newline, and without any newline inside. It requires the
// current schema version, a sequence number of 1 or more, a valid Run ID, a
// non-zero UTC time, a payload of the concrete type that the catalogue
// registers for Type, and a payload that validates. A record above
// MaxEventSize fails with ErrEventTooLarge. Every error wraps ErrInvalidEvent.
func EncodeEvent(e Event) ([]byte, error) {
	if e.SchemaVersion != EventSchemaVersion {
		return nil, invalidEvent("schema version %d, want %d", e.SchemaVersion, EventSchemaVersion)
	}
	if err := checkEnvelope(e.Seq, e.RunID, e.Time); err != nil {
		return nil, err
	}
	if e.Payload == nil {
		return nil, invalidEvent("%s event has no payload", e.Type)
	}
	entry, ok := registry[e.Type]
	if !ok {
		return nil, fmt.Errorf("%w: %w: %q", ErrInvalidEvent, ErrUnknownEventType, string(e.Type))
	}
	if got := reflect.TypeOf(e.Payload); got != entry.payloadType {
		return nil, invalidEvent("payload of type %s does not match event type %s, which takes %s",
			got, e.Type, entry.payloadType)
	}
	if err := e.Payload.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %s payload: %w", ErrInvalidEvent, e.Type, err)
	}
	payload, err := strictjson.Encode(e.Payload)
	if err != nil {
		return nil, fmt.Errorf("%w: %s payload: %w", ErrInvalidEvent, e.Type, err)
	}
	record, err := strictjson.Encode(wireEvent{
		SchemaVersion: e.SchemaVersion,
		Seq:           e.Seq,
		Type:          e.Type,
		RunID:         e.RunID,
		Time:          e.Time.Round(0), // drop the monotonic clock reading
		Payload:       payload,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidEvent, err)
	}
	if len(record) > MaxEventSize {
		return nil, fmt.Errorf("%w: %w: a %s record is %d bytes, the limit is %d",
			ErrInvalidEvent, ErrEventTooLarge, e.Type, len(record), MaxEventSize)
	}
	return record, nil
}

// DecodeEvent decodes one journal record, which is the line without its
// terminating newline. It rejects a record above MaxEventSize. It then reads
// the schema version alone, so that a record of another version is reported
// as ErrUnsupportedEventSchema even when the rest of it does not parse under
// this version's contract. Everything else is strict (package strictjson): no
// unknown, missing, duplicate or null member, in the envelope or in the
// payload. The event type must be in the catalogue (ErrUnknownEventType), the
// sequence number at least 1, the Run ID valid, the time non-zero and in UTC,
// and the payload must validate. Errors other than ErrUnsupportedEventSchema
// wrap ErrInvalidEvent and, where the failure comes from strictjson, its
// sentinel; the JSON Pointer of a payload error starts with /payload.
func DecodeEvent(record []byte) (Event, error) {
	if len(record) > MaxEventSize {
		return Event{}, fmt.Errorf("%w: %w: the record is %d bytes, the limit is %d",
			ErrInvalidEvent, ErrEventTooLarge, len(record), MaxEventSize)
	}
	// A lenient peek. When it fails or finds no schema version, the strict
	// decoding below reports the precise reason.
	var peek struct {
		SchemaVersion *int `json:"schemaVersion"`
	}
	if err := json.Unmarshal(record, &peek); err == nil && peek.SchemaVersion != nil &&
		*peek.SchemaVersion != EventSchemaVersion {
		return Event{}, fmt.Errorf("%w: the record has schema version %d, this version supports %d",
			ErrUnsupportedEventSchema, *peek.SchemaVersion, EventSchemaVersion)
	}
	wire, err := strictjson.Decode[wireEvent](record)
	if err != nil {
		return Event{}, fmt.Errorf("%w: %w", ErrInvalidEvent, err)
	}
	if wire.SchemaVersion != EventSchemaVersion {
		return Event{}, fmt.Errorf("%w: the record has schema version %d, this version supports %d",
			ErrUnsupportedEventSchema, wire.SchemaVersion, EventSchemaVersion)
	}
	if err := checkEnvelope(wire.Seq, wire.RunID, wire.Time); err != nil {
		return Event{}, err
	}
	entry, ok := registry[wire.Type]
	if !ok {
		return Event{}, fmt.Errorf("%w: %w: %q", ErrInvalidEvent, ErrUnknownEventType, string(wire.Type))
	}
	payload, err := entry.decode(wire.Payload)
	if err != nil {
		return Event{}, fmt.Errorf("%w: %s payload: %w", ErrInvalidEvent, wire.Type, withPointerPrefix(err, "/payload"))
	}
	return Event{
		SchemaVersion: wire.SchemaVersion,
		Seq:           wire.Seq,
		Type:          wire.Type,
		RunID:         wire.RunID,
		Time:          wire.Time,
		Payload:       payload,
	}, nil
}

// withPointerPrefix returns err with prefix in front of the JSON Pointer of
// the *strictjson.Error that it is, or err itself.
func withPointerPrefix(err error, prefix string) error {
	var se *strictjson.Error
	if !errors.As(err, &se) {
		return err
	}
	c := *se
	c.Pointer = prefix + se.Pointer
	return &c
}

// payloadEntry registers one payload type of the catalogue.
type payloadEntry struct {
	eventType   EventType
	payloadType reflect.Type
	decode      func(data []byte) (Payload, error)
}

// entryOf registers the payload type T. The event type is the one that T
// reports for itself, so the catalogue cannot disagree with the type.
func entryOf[T Payload]() payloadEntry {
	var zero T
	return payloadEntry{
		eventType:   zero.EventType(),
		payloadType: reflect.TypeFor[T](),
		decode: func(data []byte) (Payload, error) {
			v, err := strictjson.Decode[T](data)
			if err != nil {
				return nil, err
			}
			return v, nil
		},
	}
}

// registry maps every event type of the catalogue to its payload type.
var registry = func() map[EventType]payloadEntry {
	m := make(map[EventType]payloadEntry, len(catalogue))
	for _, e := range catalogue {
		if _, dup := m[e.eventType]; dup {
			panic("domain: event type " + string(e.eventType) + " is registered twice")
		}
		m[e.eventType] = e
	}
	return m
}()

// EventTypes returns every event type of the catalogue in the order of
// payloads.go. The caller may modify the returned slice.
func EventTypes() []EventType {
	out := make([]EventType, len(catalogue))
	for i, e := range catalogue {
		out[i] = e.eventType
	}
	return out
}

// checkText validates a summary or detail text: valid UTF-8 and at most
// MaxTextBytes bytes. The pointer locates the field within the payload.
func checkText(pointer, s string) error {
	switch {
	case !utf8.ValidString(s):
		return invalidField(pointer, "not valid UTF-8")
	case len(s) > MaxTextBytes:
		return invalidField(pointer, "%d bytes exceed the limit of %d", len(s), MaxTextBytes)
	}
	return nil
}

// invalidField returns a strictjson error that names the field at pointer.
func invalidField(pointer, format string, args ...any) error {
	return &strictjson.Error{
		Pointer: pointer,
		Err:     strictjson.ErrInvalidValue,
		Detail:  fmt.Sprintf(format, args...),
	}
}

// causedSentinel is a sentinel that also matches the error that caused it,
// while its text stays the text of the sentinel.
type causedSentinel struct{ sentinel, cause error }

func (c causedSentinel) Error() string   { return c.sentinel.Error() }
func (c causedSentinel) Unwrap() []error { return []error{c.sentinel, c.cause} }

// fieldError returns a strictjson error that names the field at pointer and
// stays matchable with the error that caused it, for example ErrInvalidID.
func fieldError(pointer string, err error) error {
	return &strictjson.Error{
		Pointer: pointer,
		Err:     causedSentinel{strictjson.ErrInvalidValue, err},
		Detail:  err.Error(),
	}
}
