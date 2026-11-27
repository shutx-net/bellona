package domain

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/shutx-net/bellona/internal/strictjson"
)

// ---------------------------------------------------------------------------
// Samples
// ---------------------------------------------------------------------------

const (
	sampleRunID    RunID     = "run-0123456789abcdef"
	sampleAttempt  AttemptID = "attempt-00000000000000a1"
	sampleAttempt2 AttemptID = "attempt-00000000000000a2"
	sampleRequest  RequestID = "req-00000000000000b1"
	sampleSession  SessionID = "0b0f1f0e-1d2c-4b3a-8f4e-5d6c7b8a9f01"
	samplePhase    PhaseID   = "phase-01"
	samplePhase2   PhaseID   = "phase-02"
)

var sampleTime = time.Date(2026, 11, 28, 0, 0, 0, 123456789, time.UTC)

// sampleRef returns a valid reference to a file at p.
func sampleRef(t testing.TB, p string) ArtifactRef {
	t.Helper()
	r, err := NewArtifactRef(p, []byte("content of "+p))
	if err != nil {
		t.Fatalf("NewArtifactRef(%q): %v", p, err)
	}
	return r
}

// payloadSample is a valid payload with the artifact references that it must
// report. Phase 3 and later phases copy this table to build their fixtures.
type payloadSample struct {
	payload Payload
	refs    []ArtifactRef
}

// samplePayloads returns one valid payload for every event type of the
// catalogue, in the order of EventTypes.
func samplePayloads(t testing.TB) []payloadSample {
	t.Helper()
	ref := func(p string) ArtifactRef { return sampleRef(t, p) }
	task := ref(TaskPath)
	manifest := ref(ManifestPath)
	policy := ref(PolicyPath)
	candidate := ref(AttemptOutputPath(sampleAttempt, 1))
	report := ref("plans/rev-0001/validation.json")
	plan := ref(PlanPath(1))
	feedback := ref("plans/rev-0001/feedback.json")
	decision := ref(AttemptOutputPath(sampleAttempt2, 1))
	output := ref(AttemptOutputPath(sampleAttempt, 1))
	reconciled := ref(AttemptOutputPath(sampleAttempt2, 2))

	return []payloadSample{
		{RunCreated{Task: task}, []ArtifactRef{task}},
		{ManifestFrozen{
			Manifest: manifest,
			Policy:   policy,
			Limits:   Limits{MaxPhaseRetries: 2, MaxReplans: 1, Timeout: "1h30m0s", MaxAgentSteps: 50, MaxTurnCostUSD: 1.5},
		}, []ArtifactRef{manifest, policy}},
		{PolicyAmended{Changes: []LimitChange{
			{Limit: LimitMaxReplans, Previous: "1", New: "3"},
			{Limit: LimitTimeout, Previous: "unlimited", New: "2h0m0s"},
		}}, nil},
		{AttemptScheduled{AttemptID: sampleAttempt, Role: RoleImplementer, Revision: 1, PhaseID: samplePhase, SessionID: sampleSession}, nil},

		{RunPrepared{}, nil},
		{StrategySelected{Decision: decision}, []ArtifactRef{decision}},
		{PlanGenerated{AttemptID: sampleAttempt, Artifact: candidate}, []ArtifactRef{candidate}},
		{PlanValidationFailed{Summary: "phase-02 depends on an unknown phase", Report: report}, []ArtifactRef{report}},
		{PlanApprovalRequested{Artifact: candidate}, []ArtifactRef{candidate}},
		{PlanAccepted{Revision: 1, Plan: plan, Phases: []PhaseDefinition{
			{ID: samplePhase, DependsOn: []PhaseID{}},
			{ID: samplePhase2, DependsOn: []PhaseID{samplePhase}},
		}}, []ArtifactRef{plan}},
		{PlanChangesRequested{Feedback: feedback}, []ArtifactRef{feedback}},
		{FinalVerificationStarted{}, nil},
		{ReplanningStarted{Cause: ReplanCauseFinalVerificationFailed}, nil},
		{FinalEvaluationStarted{}, nil},
		{RunCompleted{}, nil},
		{FinalEvaluationRetried{FailedAttemptID: sampleAttempt}, nil},
		{RunSuspended{To: RunStateWaitingForUser, Reason: SuspendReasonUserInput, ResumeState: RunStatePlanning, Detail: "one question", RequestID: sampleRequest}, nil},
		{RunResumed{To: RunStatePlanning, RequestID: sampleRequest}, nil},
		{RunCancelled{Reason: CancelReasonUserRequest}, nil},

		{PhaseReady{Revision: 1, PhaseID: samplePhase}, nil},
		{PhaseVerificationStarted{Revision: 1, PhaseID: samplePhase, AttemptID: sampleAttempt}, nil},
		{PhaseRecovering{Revision: 1, PhaseID: samplePhase, Cause: PhaseRecoveryCauseChecksFailed}, nil},
		{PhaseEvaluationStarted{Revision: 1, PhaseID: samplePhase}, nil},
		{PhaseIntegrationQueued{Revision: 1, PhaseID: samplePhase}, nil},
		{PhaseEvaluationRetried{Revision: 1, PhaseID: samplePhase, FailedAttemptID: sampleAttempt2}, nil},
		{PhaseIntegrationStarted{Revision: 1, PhaseID: samplePhase}, nil},
		{PhaseAccepted{Revision: 1, PhaseID: samplePhase, AttemptID: sampleAttempt}, nil},
		{PhaseRetryAuthorized{Revision: 1, PhaseID: samplePhase}, nil},
		{PhaseFailed{Revision: 1, PhaseID: samplePhase}, nil},
		{PhaseSuspended{Revision: 1, PhaseID: samplePhase, To: PhaseStateWaitingForUser, ResumeState: PhaseStateRunning, RequestID: sampleRequest}, nil},
		{PhaseResumed{Revision: 1, PhaseID: samplePhase, To: PhaseStateRunning}, nil},
		{PhaseReconciled{Revision: 1, PhaseID: samplePhase, To: PhaseStateVerifying}, nil},
		{PhaseSuperseded{Revision: 1, PhaseID: samplePhase}, nil},

		{AttemptStarted{AttemptID: sampleAttempt, SessionID: sampleSession}, nil},
		{AttemptAbandoned{AttemptID: sampleAttempt, Reason: AbandonReasonLaunchNotOccurred}, nil},
		{AttemptFinished{AttemptID: sampleAttempt, Output: output}, []ArtifactRef{output}},
		{AttemptFailed{AttemptID: sampleAttempt, Outcome: OutcomeFailed, ErrorClass: ErrorClassTransientError, Detail: "the host exited with status 1"}, nil},
		{AttemptTimedOut{AttemptID: sampleAttempt}, nil},
		{AttemptCancelled{AttemptID: sampleAttempt}, nil},
		{AttemptAwaitingInput{AttemptID: sampleAttempt, RequestID: sampleRequest}, nil},
		{AttemptResumed{AttemptID: sampleAttempt}, nil},
		{AttemptOutcomeUnknown{AttemptID: sampleAttempt}, nil},
		{AttemptReconciled{AttemptID: sampleAttempt2, To: AttemptStateFinished, Output: reconciled}, []ArtifactRef{reconciled}},
	}
}

// sampleEvent wraps a payload in an event with fixed envelope values.
func sampleEvent(p Payload) Event {
	return Event{SchemaVersion: EventSchemaVersion, Seq: 7, Type: p.EventType(), RunID: sampleRunID, Time: sampleTime, Payload: p}
}

func mustEncode(t testing.TB, e Event) string {
	t.Helper()
	rec, err := EncodeEvent(e)
	if err != nil {
		t.Fatalf("EncodeEvent(%s): %v", e.Type, err)
	}
	return string(rec)
}

// edit replaces the only occurrence of old in s with replacement.
func edit(t testing.TB, s, old, replacement string) string {
	t.Helper()
	if n := strings.Count(s, old); n != 1 {
		t.Fatalf("%q occurs %d times in %s, want once", old, n, s)
	}
	return strings.Replace(s, old, replacement, 1)
}

// wantIs checks that err matches every target under errors.Is.
func wantIs(t testing.TB, err error, targets ...error) {
	t.Helper()
	if err == nil {
		t.Fatalf("no error, want one matching %v", targets)
	}
	for _, target := range targets {
		if !errors.Is(err, target) {
			t.Errorf("error %q does not match %q", err, target)
		}
	}
}

// wantNotIs checks that err matches none of the targets.
func wantNotIs(t testing.TB, err error, targets ...error) {
	t.Helper()
	for _, target := range targets {
		if errors.Is(err, target) {
			t.Errorf("error %q matches %q, want no match", err, target)
		}
	}
}

// pointerOf returns the JSON Pointer of the strictjson error in err.
func pointerOf(err error) (string, bool) {
	var se *strictjson.Error
	if errors.As(err, &se) {
		return se.Pointer, true
	}
	return "", false
}

// ---------------------------------------------------------------------------
// Catalogue
// ---------------------------------------------------------------------------

func TestEventCatalogue(t *testing.T) {
	types := EventTypes()
	if len(types) != 43 {
		t.Errorf("the catalogue has %d event types, want 43", len(types))
	}
	samples := samplePayloads(t)
	if len(samples) != len(types) {
		t.Fatalf("%d samples for %d event types", len(samples), len(types))
	}
	seen := make(map[EventType]bool)
	for i, typ := range types {
		s := samples[i].payload
		if typ != s.EventType() {
			t.Errorf("sample %d is a %s, the catalogue has %s at that position", i, s.EventType(), typ)
		}
		if got := reflect.TypeOf(s).Name(); got != string(typ) {
			t.Errorf("event type %s is implemented by a type named %s", typ, got)
		}
		if seen[typ] {
			t.Errorf("event type %s is listed twice", typ)
		}
		seen[typ] = true
	}
	// The returned slice is a copy.
	types[0] = "Changed"
	if EventTypes()[0] == "Changed" {
		t.Error("EventTypes returns its internal slice")
	}
	for _, typ := range []EventType{EventRunCreated, EventManifestFrozen, EventPolicyAmended} {
		if !seen[typ] {
			t.Errorf("%s is missing from the catalogue", typ)
		}
	}
	if seen["UserInputRequested"] || seen["UserInputReceived"] {
		t.Error("UserInputRequested and UserInputReceived are reserved for M2")
	}
}

func TestPayloadsFollowTheContractConvention(t *testing.T) {
	for _, s := range samplePayloads(t) {
		typ := reflect.TypeOf(s.payload)
		t.Run(typ.Name(), func(t *testing.T) {
			if err := strictjson.CheckContractType(typ); err != nil {
				t.Error(err)
			}
		})
	}
	for _, typ := range []reflect.Type{reflect.TypeFor[wireEvent](), reflect.TypeFor[Limits](), reflect.TypeFor[LimitChange]()} {
		if err := strictjson.CheckContractType(typ); err != nil {
			t.Errorf("%s: %v", typ, err)
		}
	}
}

func TestPayloadsAreSmall(t *testing.T) {
	// R-PER-07: a payload holds small data and artifact references. No
	// payload field is a free-form blob: text fields are bounded by
	// MaxTextBytes, and everything else is an identifier, an enumeration, a
	// number or an ArtifactRef.
	for _, s := range samplePayloads(t) {
		typ := reflect.TypeOf(s.payload)
		for f := range typ.Fields() {
			switch f.Type.Kind() {
			case reflect.String, reflect.Int, reflect.Uint64, reflect.Slice, reflect.Struct:
			default:
				t.Errorf("%s.%s has the unexpected kind %s", typ.Name(), f.Name, f.Type.Kind())
			}
			if f.Type.Kind() == reflect.Slice && f.Type.Elem().Kind() == reflect.Uint8 {
				t.Errorf("%s.%s is a byte slice", typ.Name(), f.Name)
			}
		}
	}
}

func TestPayloadArtifactRefs(t *testing.T) {
	for _, s := range samplePayloads(t) {
		t.Run(string(s.payload.EventType()), func(t *testing.T) {
			if got := s.payload.ArtifactRefs(); !slices.Equal(got, s.refs) {
				t.Errorf("ArtifactRefs() = %v, want %v", got, s.refs)
			}
			for _, r := range s.payload.ArtifactRefs() {
				if err := r.Validate(); err != nil {
					t.Errorf("reference %v: %v", r, err)
				}
			}
		})
	}
	// An absent optional reference is not reported.
	if got := (PlanValidationFailed{Summary: "x"}).ArtifactRefs(); got != nil {
		t.Errorf("an absent report is reported: %v", got)
	}
	if got := (AttemptReconciled{AttemptID: sampleAttempt, To: AttemptStateFailed}).ArtifactRefs(); got != nil {
		t.Errorf("an absent output is reported: %v", got)
	}
}

// ---------------------------------------------------------------------------
// Round trip
// ---------------------------------------------------------------------------

func TestEventRoundTrip(t *testing.T) {
	for _, s := range samplePayloads(t) {
		t.Run(string(s.payload.EventType()), func(t *testing.T) {
			e := sampleEvent(s.payload)
			rec, err := EncodeEvent(e)
			if err != nil {
				t.Fatal(err)
			}
			if strings.ContainsAny(string(rec), "\n\r") {
				t.Errorf("the record contains a line break: %s", rec)
			}
			if len(rec) > MaxEventSize {
				t.Errorf("the record has %d bytes", len(rec))
			}
			prefix := `{"schemaVersion":1,"seq":7,"type":"` + string(e.Type) + `","runId":"run-0123456789abcdef","time":"2026-11-28T00:00:00.123456789Z","payload":`
			if !strings.HasPrefix(string(rec), prefix) || !strings.HasSuffix(string(rec), "}") {
				t.Errorf("the record does not have the documented wire form:\n%s", rec)
			}
			got, err := DecodeEvent(rec)
			if err != nil {
				t.Fatalf("DecodeEvent: %v\n%s", err, rec)
			}
			if got.SchemaVersion != e.SchemaVersion || got.Seq != e.Seq || got.Type != e.Type || got.RunID != e.RunID {
				t.Errorf("envelope %+v, want %+v", got, e)
			}
			if !got.Time.Equal(e.Time) || got.Time.Location() != time.UTC {
				t.Errorf("time %v (%v), want %v in UTC", got.Time, got.Time.Location(), e.Time)
			}
			if !reflect.DeepEqual(got.Payload, e.Payload) {
				t.Errorf("payload %#v, want %#v", got.Payload, e.Payload)
			}
			again, err := EncodeEvent(got)
			if err != nil {
				t.Fatal(err)
			}
			if string(again) != string(rec) {
				t.Errorf("re-encoding differs:\n%s\n%s", again, rec)
			}
		})
	}
}

func TestEventTimeKeepsNanosecondsAndIsUTC(t *testing.T) {
	for _, tm := range []time.Time{
		time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 11, 28, 0, 0, 0, 1, time.UTC),
		time.Date(2026, 11, 28, 23, 59, 59, 999999999, time.UTC),
		time.Date(2026, 11, 28, 9, 0, 0, 5, time.FixedZone("JST", 9*3600)).UTC(),
	} {
		e := sampleEvent(RunPrepared{})
		e.Time = tm
		got, err := DecodeEvent([]byte(mustEncode(t, e)))
		if err != nil {
			t.Fatal(err)
		}
		if !got.Time.Equal(tm) || got.Time.Location() != time.UTC {
			t.Errorf("time %v, want %v in UTC", got.Time, tm)
		}
	}
}

// ---------------------------------------------------------------------------
// Decoding rejects corrupt records
// ---------------------------------------------------------------------------

func TestDecodeEventRejects(t *testing.T) {
	base := sampleEvent(AttemptFinished{AttemptID: sampleAttempt, Output: sampleRef(t, AttemptOutputPath(sampleAttempt, 1))})
	rec := mustEncode(t, base)
	const (
		attemptMember = `"attemptId":"attempt-00000000000000a1"`
		timeMember    = `"time":"2026-11-28T00:00:00.123456789Z"`
	)
	sha := func() string {
		i := strings.Index(rec, `"sha256":"`)
		return rec[i : i+len(`"sha256":"`)+64+1]
	}()

	tests := []struct {
		name   string
		record string
		is     []error
		not    []error
		ptr    string // expected JSON Pointer, when non-empty
	}{
		{"unknown envelope member", edit(t, rec, `{"schemaVersion":1,`, `{"schemaVersion":1,"extra":1,`),
			[]error{ErrInvalidEvent, strictjson.ErrUnknownMember}, nil, "/extra"},
		{"unknown payload member", edit(t, rec, attemptMember, attemptMember+`,"extra":true`),
			[]error{ErrInvalidEvent, strictjson.ErrUnknownMember}, nil, "/payload/extra"},
		{"unknown member inside a reference", edit(t, rec, sha, sha+`,"extra":1`),
			[]error{ErrInvalidEvent, strictjson.ErrUnknownMember}, nil, "/payload/output/extra"},
		{"member names are case sensitive", edit(t, rec, `"seq":7`, `"Seq":7`),
			[]error{ErrInvalidEvent}, nil, ""},
		{"null payload member", edit(t, rec, `"attemptId":"attempt-00000000000000a1"`, `"attemptId":null`),
			[]error{ErrInvalidEvent, strictjson.ErrNull}, nil, "/payload/attemptId"},
		{"null envelope member", edit(t, rec, `"seq":7`, `"seq":null`),
			[]error{ErrInvalidEvent, strictjson.ErrNull}, nil, "/seq"},
		{"null payload", edit(t, rec, `"payload":{"attemptId"`, `"payload":null,"x":{"attemptId"`),
			[]error{ErrInvalidEvent, strictjson.ErrNull}, nil, "/payload"},
		{"missing payload member", edit(t, rec, attemptMember+",", ""),
			[]error{ErrInvalidEvent, strictjson.ErrMissingMember}, nil, "/payload/attemptId"},
		{"missing member inside a reference", edit(t, rec, `,`+sha, ""),
			[]error{ErrInvalidEvent, strictjson.ErrMissingMember}, nil, "/payload/output/sha256"},
		{"missing envelope member", edit(t, rec, `"runId":"run-0123456789abcdef",`, ""),
			[]error{ErrInvalidEvent, strictjson.ErrMissingMember}, nil, "/runId"},
		{"missing payload", edit(t, rec, `,"payload":`, `,"payloa":`),
			[]error{ErrInvalidEvent}, nil, ""},
		{"duplicate envelope member", edit(t, rec, `"seq":7,`, `"seq":7,"seq":8,`),
			[]error{ErrInvalidEvent, strictjson.ErrDuplicateMember}, nil, ""},
		{"duplicate payload member", edit(t, rec, attemptMember, attemptMember+`,`+attemptMember),
			[]error{ErrInvalidEvent, strictjson.ErrDuplicateMember}, nil, ""},
		{"payload does not match the type", edit(t, rec, `"type":"AttemptFinished"`, `"type":"PlanGenerated"`),
			[]error{ErrInvalidEvent}, []error{ErrUnknownEventType, ErrUnsupportedEventSchema}, ""},
		{"payload of another empty-looking type", edit(t, rec, `"type":"AttemptFinished"`, `"type":"AttemptTimedOut"`),
			[]error{ErrInvalidEvent, strictjson.ErrUnknownMember}, nil, "/payload/output"},
		{"unknown event type", edit(t, rec, `"type":"AttemptFinished"`, `"type":"AttemptFinishedX"`),
			[]error{ErrInvalidEvent, ErrUnknownEventType}, []error{ErrUnsupportedEventSchema}, ""},
		{"event type with other case", edit(t, rec, `"type":"AttemptFinished"`, `"type":"attemptfinished"`),
			[]error{ErrInvalidEvent, ErrUnknownEventType}, nil, ""},
		{"empty event type", edit(t, rec, `"type":"AttemptFinished"`, `"type":""`),
			[]error{ErrInvalidEvent, ErrUnknownEventType}, nil, ""},
		{"schema version 2", edit(t, rec, `"schemaVersion":1`, `"schemaVersion":2`),
			[]error{ErrUnsupportedEventSchema}, []error{ErrInvalidEvent}, ""},
		{"schema version 0", edit(t, rec, `"schemaVersion":1`, `"schemaVersion":0`),
			[]error{ErrUnsupportedEventSchema}, []error{ErrInvalidEvent}, ""},
		{"schema version 2 with an unknown member", edit(t, rec, `"schemaVersion":1,`, `"schemaVersion":2,"newMember":{"a":[1]},`),
			[]error{ErrUnsupportedEventSchema}, []error{ErrInvalidEvent, strictjson.ErrUnknownMember}, ""},
		{"schema version 2 with an unknown type and payload", edit(t, edit(t, rec, `"schemaVersion":1`, `"schemaVersion":2`), `"type":"AttemptFinished"`, `"type":"Future"`),
			[]error{ErrUnsupportedEventSchema}, []error{ErrInvalidEvent, ErrUnknownEventType}, ""},
		{"missing schema version", edit(t, rec, `"schemaVersion":1,`, ""),
			[]error{ErrInvalidEvent, strictjson.ErrMissingMember}, []error{ErrUnsupportedEventSchema}, "/schemaVersion"},
		{"null schema version", edit(t, rec, `"schemaVersion":1`, `"schemaVersion":null`),
			[]error{ErrInvalidEvent, strictjson.ErrNull}, []error{ErrUnsupportedEventSchema}, "/schemaVersion"},
		{"schema version as a string", edit(t, rec, `"schemaVersion":1`, `"schemaVersion":"1"`),
			[]error{ErrInvalidEvent, strictjson.ErrInvalidValue}, []error{ErrUnsupportedEventSchema}, ""},
		{"seq 0", edit(t, rec, `"seq":7`, `"seq":0`),
			[]error{ErrInvalidEvent}, nil, ""},
		{"negative seq", edit(t, rec, `"seq":7`, `"seq":-1`),
			[]error{ErrInvalidEvent, strictjson.ErrInvalidValue}, nil, ""},
		{"fractional seq", edit(t, rec, `"seq":7`, `"seq":7.5`),
			[]error{ErrInvalidEvent, strictjson.ErrInvalidValue}, nil, ""},
		{"invalid run ID", edit(t, rec, `"runId":"run-0123456789abcdef"`, `"runId":"run-1"`),
			[]error{ErrInvalidEvent, ErrInvalidID}, nil, "/runId"},
		{"empty run ID", edit(t, rec, `"runId":"run-0123456789abcdef"`, `"runId":""`),
			[]error{ErrInvalidEvent, ErrInvalidID}, nil, "/runId"},
		{"time with an offset", edit(t, rec, timeMember, `"time":"2026-11-28T09:00:00.123456789+09:00"`),
			[]error{ErrInvalidEvent}, nil, ""},
		{"time with a zero offset", edit(t, rec, timeMember, `"time":"2026-11-28T00:00:00.123456789+00:00"`),
			[]error{ErrInvalidEvent}, nil, ""},
		{"zero time", edit(t, rec, timeMember, `"time":"0001-01-01T00:00:00Z"`),
			[]error{ErrInvalidEvent}, nil, ""},
		{"time that is not a timestamp", edit(t, rec, timeMember, `"time":"yesterday"`),
			[]error{ErrInvalidEvent, strictjson.ErrInvalidValue}, nil, "/time"},
		{"time that is a number", edit(t, rec, timeMember, `"time":1795910400`),
			[]error{ErrInvalidEvent, strictjson.ErrInvalidValue}, nil, ""},
		{"invalid reference path", edit(t, rec, `"path":"attempts/`, `"path":"../attempts/`),
			[]error{ErrInvalidEvent, ErrInvalidPath}, nil, "/payload/output"},
		{"invalid reference digest", edit(t, rec, sha, `"sha256":"ABC"`),
			[]error{ErrInvalidEvent, ErrInvalidArtifactRef}, nil, "/payload/output"},
		{"invalid attempt ID", edit(t, rec, `"attemptId":"attempt-00000000000000a1"`, `"attemptId":"a1"`),
			[]error{ErrInvalidEvent, ErrInvalidID}, nil, "/payload/attemptId"},
		{"trailing data", rec + ` {}`,
			[]error{ErrInvalidEvent, strictjson.ErrTrailingData}, nil, ""},
		{"two records", rec + "\n" + rec,
			[]error{ErrInvalidEvent, strictjson.ErrTrailingData}, nil, ""},
		{"empty record", "",
			[]error{ErrInvalidEvent, strictjson.ErrInvalidJSON}, nil, ""},
		{"not JSON", "events.jsonl is not JSON",
			[]error{ErrInvalidEvent, strictjson.ErrInvalidJSON}, nil, ""},
		{"a JSON array", "[]",
			[]error{ErrInvalidEvent}, nil, ""},
		{"a JSON string", `"RunCreated"`,
			[]error{ErrInvalidEvent}, nil, ""},
		{"a NUL region", "\x00\x00\x00\x00",
			[]error{ErrInvalidEvent, strictjson.ErrInvalidJSON}, nil, ""},
		{"record above the size limit", rec + strings.Repeat(" ", MaxEventSize),
			[]error{ErrInvalidEvent, ErrEventTooLarge}, nil, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DecodeEvent([]byte(tc.record))
			if err == nil {
				t.Fatalf("DecodeEvent accepted %s", tc.record)
			}
			if got.Payload != nil || got.Type != "" {
				t.Errorf("DecodeEvent returned a partial event with its error: %+v", got)
			}
			wantIs(t, err, tc.is...)
			wantNotIs(t, err, tc.not...)
			if tc.ptr != "" {
				if p, ok := pointerOf(err); !ok || p != tc.ptr {
					t.Errorf("JSON Pointer %q (found %v), want %q; error: %v", p, ok, tc.ptr, err)
				}
			}
		})
	}
	if _, err := DecodeEvent([]byte(rec)); err != nil {
		t.Errorf("the unmodified record does not decode: %v", err)
	}
	// The size limit is inclusive: a record of exactly MaxEventSize bytes is
	// not rejected for its size, one byte more is.
	padded := rec + strings.Repeat(" ", MaxEventSize-len(rec))
	if _, err := DecodeEvent([]byte(padded)); err != nil {
		t.Errorf("a record of %d bytes (the limit) is rejected: %v", len(padded), err)
	}
	if _, err := DecodeEvent([]byte(padded + " ")); !errors.Is(err, ErrEventTooLarge) {
		t.Errorf("a record of %d bytes is not rejected for its size: %v", len(padded)+1, err)
	}
}

func TestDecodeEventPayloadRules(t *testing.T) {
	// A payload that parses strictly but breaks a rule of its type is
	// rejected with the pointer of the field.
	suspended := mustEncode(t, sampleEvent(RunSuspended{To: RunStateWaitingForUser, Reason: SuspendReasonUserInput, ResumeState: RunStatePlanning, RequestID: sampleRequest}))
	reconciled := mustEncode(t, sampleEvent(AttemptReconciled{AttemptID: sampleAttempt, To: AttemptStateFailed}))
	plan := mustEncode(t, sampleEvent(PlanAccepted{Revision: 1, Plan: sampleRef(t, PlanPath(1)), Phases: []PhaseDefinition{{ID: samplePhase, DependsOn: []PhaseID{}}}}))
	tests := []struct {
		name   string
		record string
		ptr    string
	}{
		{"waiting without a request", edit(t, suspended, `,"requestId":"req-00000000000000b1"`, ""), "/payload/requestId"},
		{"reason that does not lead to the state", edit(t, suspended, `"reason":"user_input"`, `"reason":"needs_user"`), "/payload/reason"},
		{"unknown reason", edit(t, suspended, `"reason":"user_input"`, `"reason":"tired"`), "/payload/reason"},
		{"unknown state", edit(t, suspended, `"to":"WaitingForUser"`, `"to":"Sleeping"`), "/payload/to"},
		{"finished without output", edit(t, reconciled, `"to":"Failed"`, `"to":"Finished"`), "/payload/output"},
		{"cyclic plan", edit(t, plan, `"dependsOn":[]`, `"dependsOn":["phase-01"]`), "/payload/phases/0/dependsOn/0"},
		{"plan path of another revision", edit(t, plan, `"revision":1`, `"revision":2`), "/payload/plan/path"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeEvent([]byte(tc.record))
			wantIs(t, err, ErrInvalidEvent, strictjson.ErrInvalidValue)
			if p, ok := pointerOf(err); !ok || p != tc.ptr {
				t.Errorf("JSON Pointer %q (found %v), want %q; error: %v", p, ok, tc.ptr, err)
			}
		})
	}
}

func TestDecodeEventRejectsEveryTruncation(t *testing.T) {
	// A record cut short anywhere is detected (T-13).
	for _, s := range samplePayloads(t) {
		rec := mustEncode(t, sampleEvent(s.payload))
		for n := range len(rec) {
			if _, err := DecodeEvent([]byte(rec[:n])); !errors.Is(err, ErrInvalidEvent) {
				t.Fatalf("%s: the first %d of %d bytes decode or fail with %v", s.payload.EventType(), n, len(rec), err)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Encoding rejects invalid events
// ---------------------------------------------------------------------------

func TestEncodeEventRejects(t *testing.T) {
	valid := sampleEvent(RunCreated{Task: sampleRef(t, TaskPath)})
	tests := []struct {
		name   string
		mutate func(e *Event)
		is     []error
	}{
		{"schema version 0", func(e *Event) { e.SchemaVersion = 0 }, []error{ErrInvalidEvent}},
		{"schema version 2", func(e *Event) { e.SchemaVersion = 2 }, []error{ErrInvalidEvent}},
		{"seq 0", func(e *Event) { e.Seq = 0 }, []error{ErrInvalidEvent}},
		{"empty run ID", func(e *Event) { e.RunID = "" }, []error{ErrInvalidEvent, ErrInvalidID}},
		{"invalid run ID", func(e *Event) { e.RunID = "run-xyz" }, []error{ErrInvalidEvent, ErrInvalidID}},
		{"zero time", func(e *Event) { e.Time = time.Time{} }, []error{ErrInvalidEvent}},
		{"time in a fixed zone", func(e *Event) { e.Time = e.Time.In(time.FixedZone("JST", 9*3600)) }, []error{ErrInvalidEvent}},
		{"time in a zero-offset fixed zone", func(e *Event) { e.Time = e.Time.In(time.FixedZone("UTC", 0)) }, []error{ErrInvalidEvent}},
		{"nil payload", func(e *Event) { e.Payload = nil }, []error{ErrInvalidEvent}},
		{"unknown event type", func(e *Event) { e.Type = "Bogus" }, []error{ErrInvalidEvent, ErrUnknownEventType}},
		{"empty event type", func(e *Event) { e.Type = "" }, []error{ErrInvalidEvent, ErrUnknownEventType}},
		{"type and payload differ", func(e *Event) { e.Type = EventRunPrepared }, []error{ErrInvalidEvent}},
		{"pointer payload", func(e *Event) { p := e.Payload.(RunCreated); e.Payload = &p }, []error{ErrInvalidEvent}},
		{"invalid payload", func(e *Event) { e.Payload = RunCreated{Task: sampleRef(t, "other.json")} }, []error{ErrInvalidEvent, strictjson.ErrInvalidValue}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := valid
			tc.mutate(&e)
			rec, err := EncodeEvent(e)
			if err == nil {
				t.Fatalf("EncodeEvent accepted %+v: %s", e, rec)
			}
			if rec != nil {
				t.Errorf("EncodeEvent returned bytes with its error")
			}
			wantIs(t, err, tc.is...)
		})
	}
}

func TestEncodeEventRejectsAnOversizedRecord(t *testing.T) {
	// 256 phases, each depending on all that precede it: valid, but far above
	// MaxEventSize.
	phases := make([]PhaseDefinition, 256)
	for i := range phases {
		phases[i].ID = PhaseID(fmt.Sprintf("p%03d", i))
		phases[i].DependsOn = []PhaseID{}
		for j := range i {
			phases[i].DependsOn = append(phases[i].DependsOn, phases[j].ID)
		}
	}
	p := PlanAccepted{Revision: 1, Plan: sampleRef(t, PlanPath(1)), Phases: phases}
	if err := p.Validate(); err != nil {
		t.Fatalf("the oversized plan must be valid, got: %v", err)
	}
	_, err := EncodeEvent(sampleEvent(p))
	wantIs(t, err, ErrEventTooLarge, ErrInvalidEvent)
}

// ---------------------------------------------------------------------------
// Payload rules
// ---------------------------------------------------------------------------

func TestPayloadRules(t *testing.T) {
	ref := func(p string) ArtifactRef { return sampleRef(t, p) }
	long := strings.Repeat("x", MaxTextBytes+1)
	phases := func(defs ...string) []PhaseDefinition {
		// each def is "id:dep,dep"
		var out []PhaseDefinition
		for _, d := range defs {
			id, deps, _ := strings.Cut(d, ":")
			pd := PhaseDefinition{ID: PhaseID(id), DependsOn: []PhaseID{}}
			for dep := range strings.SplitSeq(deps, ",") {
				if dep != "" {
					pd.DependsOn = append(pd.DependsOn, PhaseID(dep))
				}
			}
			out = append(out, pd)
		}
		return out
	}
	acceptedWith := func(rev int, plan ArtifactRef, defs []PhaseDefinition) PlanAccepted {
		return PlanAccepted{Revision: rev, Plan: plan, Phases: defs}
	}
	manyPhases := func(n int) []PhaseDefinition {
		out := make([]PhaseDefinition, n)
		for i := range out {
			out[i] = PhaseDefinition{ID: PhaseID("q" + strconv.Itoa(i)), DependsOn: []PhaseID{}}
		}
		return out
	}

	tests := []struct {
		name    string
		payload Payload
		ptr     string
	}{
		{"RunCreated with another path", RunCreated{Task: ref("other.json")}, "/task/path"},
		{"RunCreated without a task", RunCreated{}, "/task"},
		{"ManifestFrozen with another manifest path", ManifestFrozen{Manifest: ref("other.json"), Policy: ref(PolicyPath)}, "/manifest/path"},
		{"ManifestFrozen with another policy path", ManifestFrozen{Manifest: ref(ManifestPath), Policy: ref("other.json")}, "/policy/path"},
		{"ManifestFrozen with invalid limits", ManifestFrozen{Manifest: ref(ManifestPath), Policy: ref(PolicyPath), Limits: Limits{MaxReplans: -1}}, "/limits"},

		{"PolicyAmended without changes", PolicyAmended{Changes: []LimitChange{}}, "/changes"},
		{"PolicyAmended with six changes", PolicyAmended{Changes: []LimitChange{
			{LimitMaxReplans, "1", "2"}, {LimitMaxPhaseRetries, "1", "2"}, {LimitTimeout, "unlimited", "1m0s"},
			{LimitMaxAgentSteps, "unlimited", "5"}, {LimitMaxTurnCostUSD, "unlimited", "2"}, {LimitMaxReplans, "2", "3"},
		}}, "/changes"},
		{"PolicyAmended with a duplicate limit", PolicyAmended{Changes: []LimitChange{
			{LimitMaxReplans, "1", "2"}, {LimitMaxReplans, "2", "3"},
		}}, "/changes/1/limit"},
		{"PolicyAmended with previous equal to new", PolicyAmended{Changes: []LimitChange{{LimitMaxReplans, "2", "2"}}}, "/changes/0/new"},
		{"PolicyAmended unlimited to unlimited", PolicyAmended{Changes: []LimitChange{{LimitTimeout, "unlimited", "unlimited"}}}, "/changes/0/new"},
		{"PolicyAmended with an unknown limit", PolicyAmended{Changes: []LimitChange{{"maxSpeed", "1", "2"}}}, "/changes/0/limit"},
		{"PolicyAmended with a value that is not canonical", PolicyAmended{Changes: []LimitChange{{LimitMaxReplans, "1", "03"}}}, "/changes/0/new"},
		{"PolicyAmended with a previous value that is not canonical", PolicyAmended{Changes: []LimitChange{{LimitTimeout, "90m", "2h0m0s"}}}, "/changes/0/previous"},
		{"PolicyAmended with unlimited retries", PolicyAmended{Changes: []LimitChange{{LimitMaxPhaseRetries, "1", "unlimited"}}}, "/changes/0/new"},
		{"PolicyAmended with a negative value", PolicyAmended{Changes: []LimitChange{{LimitMaxReplans, "1", "-1"}}}, "/changes/0/new"},
		{"PolicyAmended with zero for an optional limit", PolicyAmended{Changes: []LimitChange{{LimitMaxAgentSteps, "5", "0"}}}, "/changes/0/new"},

		{"AttemptScheduled implementer without a phase", AttemptScheduled{AttemptID: sampleAttempt, Role: RoleImplementer, Revision: 1}, ""},
		{"AttemptScheduled implementer without a revision", AttemptScheduled{AttemptID: sampleAttempt, Role: RoleImplementer, PhaseID: samplePhase}, ""},
		{"AttemptScheduled planner with a phase", AttemptScheduled{AttemptID: sampleAttempt, Role: RolePlanner, PhaseID: samplePhase}, "/phaseId"},
		{"AttemptScheduled coordinator with a phase", AttemptScheduled{AttemptID: sampleAttempt, Role: RoleCoordinator, Revision: 1, PhaseID: samplePhase}, "/phaseId"},
		{"AttemptScheduled evaluator with a phase only", AttemptScheduled{AttemptID: sampleAttempt, Role: RoleEvaluator, PhaseID: samplePhase}, ""},
		{"AttemptScheduled evaluator with a revision only", AttemptScheduled{AttemptID: sampleAttempt, Role: RoleEvaluator, Revision: 1}, ""},
		{"AttemptScheduled with an unknown role", AttemptScheduled{AttemptID: sampleAttempt, Role: "judge"}, "/role"},
		{"AttemptScheduled with a negative revision", AttemptScheduled{AttemptID: sampleAttempt, Role: RolePlanner, Revision: -1}, "/revision"},
		{"AttemptScheduled with an invalid session", AttemptScheduled{AttemptID: sampleAttempt, Role: RolePlanner, SessionID: "bad session"}, "/sessionId"},
		{"AttemptScheduled without an attempt", AttemptScheduled{Role: RolePlanner}, "/attemptId"},

		{"PlanGenerated without an artifact", PlanGenerated{AttemptID: sampleAttempt}, "/artifact"},
		{"PlanValidationFailed with an empty summary", PlanValidationFailed{}, "/summary"},
		{"PlanValidationFailed with a blank summary", PlanValidationFailed{Summary: " \n\t"}, "/summary"},
		{"PlanValidationFailed with a long summary", PlanValidationFailed{Summary: long}, "/summary"},
		{"PlanValidationFailed with invalid UTF-8", PlanValidationFailed{Summary: "a\xffb"}, "/summary"},
		{"PlanValidationFailed with an invalid report", PlanValidationFailed{Summary: "x", Report: ArtifactRef{Path: "a", Size: -1}}, "/report"},
		{"PlanApprovalRequested without an artifact", PlanApprovalRequested{}, "/artifact"},
		{"PlanChangesRequested without feedback", PlanChangesRequested{}, "/feedback"},
		{"StrategySelected with an invalid decision", StrategySelected{Decision: ArtifactRef{Path: "a"}}, "/decision"},

		{"PlanAccepted with revision 0", acceptedWith(0, ref(PlanPath(1)), phases("a")), "/revision"},
		{"PlanAccepted with the path of another revision", acceptedWith(2, ref(PlanPath(1)), phases("a")), "/plan/path"},
		{"PlanAccepted without phases", acceptedWith(1, ref(PlanPath(1)), nil), "/phases"},
		{"PlanAccepted with 257 phases", acceptedWith(1, ref(PlanPath(1)), manyPhases(257)), "/phases"},
		{"PlanAccepted with a duplicate phase", acceptedWith(1, ref(PlanPath(1)), phases("a", "b", "a")), "/phases/2/id"},
		{"PlanAccepted with an unknown dependency", acceptedWith(1, ref(PlanPath(1)), phases("a:zz")), "/phases/0/dependsOn/0"},
		{"PlanAccepted with a self dependency", acceptedWith(1, ref(PlanPath(1)), phases("a:a")), "/phases/0/dependsOn/0"},
		{"PlanAccepted with a repeated dependency", acceptedWith(1, ref(PlanPath(1)), phases("a", "b:a,a")), "/phases/1/dependsOn/1"},
		{"PlanAccepted with a two-phase cycle", acceptedWith(1, ref(PlanPath(1)), phases("a:b", "b:a")), "/phases/0"},
		{"PlanAccepted with a longer cycle", acceptedWith(1, ref(PlanPath(1)), phases("a", "b:a,d", "c:b", "d:c")), "/phases/1"},
		{"PlanAccepted with an invalid phase ID", acceptedWith(1, ref(PlanPath(1)), phases("a b")), "/phases/0/id"},
		{"PlanAccepted with an invalid dependency ID", acceptedWith(1, ref(PlanPath(1)), phases("a", "b:a b")), "/phases/1/dependsOn/0"},

		{"ReplanningStarted with an unknown cause", ReplanningStarted{Cause: "whim"}, "/cause"},
		{"FinalEvaluationRetried without an attempt", FinalEvaluationRetried{}, "/failedAttemptId"},
		{"RunCancelled with an unknown reason", RunCancelled{Reason: "boredom"}, "/reason"},

		{"RunSuspended to a state that is not suspended", RunSuspended{To: RunStatePlanning, Reason: SuspendReasonNeedsUser, ResumeState: RunStateExecuting}, "/to"},
		{"RunSuspended to Completed", RunSuspended{To: RunStateCompleted, Reason: SuspendReasonNeedsUser, ResumeState: RunStateExecuting}, "/to"},
		{"RunSuspended with an unknown reason", RunSuspended{To: RunStateBlocked, Reason: "x", ResumeState: RunStateExecuting}, "/reason"},
		{"RunSuspended with a reason for another state", RunSuspended{To: RunStateBlocked, Reason: SuspendReasonUserInput, ResumeState: RunStateExecuting, RequestID: sampleRequest}, "/reason"},
		{"RunSuspended unknown_outcome to Blocked", RunSuspended{To: RunStateBlocked, Reason: SuspendReasonUnknownOutcome, ResumeState: RunStateExecuting}, "/reason"},
		{"RunSuspended to WaitingForUser without a request", RunSuspended{To: RunStateWaitingForUser, Reason: SuspendReasonUserInput, ResumeState: RunStatePlanning}, "/requestId"},
		{"RunSuspended to Blocked with a request", RunSuspended{To: RunStateBlocked, Reason: SuspendReasonLimitReached, ResumeState: RunStateExecuting, RequestID: sampleRequest}, "/requestId"},
		{"RunSuspended with an invalid request", RunSuspended{To: RunStateWaitingForUser, Reason: SuspendReasonUserInput, ResumeState: RunStatePlanning, RequestID: "r1"}, "/requestId"},
		{"RunSuspended with an unknown resume state", RunSuspended{To: RunStateBlocked, Reason: SuspendReasonLimitReached, ResumeState: "Lost"}, "/resumeState"},
		{"RunSuspended without a resume state", RunSuspended{To: RunStateBlocked, Reason: SuspendReasonLimitReached}, "/resumeState"},
		{"RunSuspended with a terminal resume state", RunSuspended{To: RunStateBlocked, Reason: SuspendReasonLimitReached, ResumeState: RunStateCompleted}, "/resumeState"},
		{"RunSuspended with itself as the resume state", RunSuspended{To: RunStateBlocked, Reason: SuspendReasonLimitReached, ResumeState: RunStateBlocked}, "/resumeState"},
		{"RunSuspended with a long detail", RunSuspended{To: RunStateBlocked, Reason: SuspendReasonLimitReached, ResumeState: RunStateExecuting, Detail: long}, "/detail"},
		{"RunResumed to an unknown state", RunResumed{To: "Nowhere"}, "/to"},
		{"RunResumed to a terminal state", RunResumed{To: RunStateCancelled}, "/to"},
		{"RunResumed with an invalid request", RunResumed{To: RunStatePlanning, RequestID: "r1"}, "/requestId"},

		{"PhaseReady with revision 0", PhaseReady{PhaseID: samplePhase}, "/revision"},
		{"PhaseReady without a phase", PhaseReady{Revision: 1}, "/phaseId"},
		{"PhaseReady with an invalid phase", PhaseReady{Revision: 1, PhaseID: "x y"}, "/phaseId"},
		{"PhaseVerificationStarted without an attempt", PhaseVerificationStarted{Revision: 1, PhaseID: samplePhase}, "/attemptId"},
		{"PhaseRecovering with an unknown cause", PhaseRecovering{Revision: 1, PhaseID: samplePhase, Cause: "bad luck"}, "/cause"},
		{"PhaseRecovering without a cause", PhaseRecovering{Revision: 1, PhaseID: samplePhase}, "/cause"},
		{"PhaseEvaluationRetried without an attempt", PhaseEvaluationRetried{Revision: 1, PhaseID: samplePhase}, "/failedAttemptId"},
		{"PhaseAccepted without an attempt", PhaseAccepted{Revision: 1, PhaseID: samplePhase}, "/attemptId"},
		{"PhaseSuspended to a state that is not suspended", PhaseSuspended{Revision: 1, PhaseID: samplePhase, To: PhaseStateRunning, ResumeState: PhaseStateReady}, "/to"},
		{"PhaseSuspended to WaitingForUser without a request", PhaseSuspended{Revision: 1, PhaseID: samplePhase, To: PhaseStateWaitingForUser, ResumeState: PhaseStateRunning}, "/requestId"},
		{"PhaseSuspended to RecoveryRequired with a request", PhaseSuspended{Revision: 1, PhaseID: samplePhase, To: PhaseStateRecoveryRequired, ResumeState: PhaseStateRunning, RequestID: sampleRequest}, "/requestId"},
		{"PhaseSuspended with an unknown resume state", PhaseSuspended{Revision: 1, PhaseID: samplePhase, To: PhaseStateRecoveryRequired, ResumeState: "Lost"}, "/resumeState"},
		{"PhaseSuspended with itself as the resume state", PhaseSuspended{Revision: 1, PhaseID: samplePhase, To: PhaseStateRecoveryRequired, ResumeState: PhaseStateRecoveryRequired}, "/resumeState"},
		{"PhaseResumed to an unknown state", PhaseResumed{Revision: 1, PhaseID: samplePhase, To: "Nowhere"}, "/to"},
		{"PhaseReconciled to an unknown state", PhaseReconciled{Revision: 1, PhaseID: samplePhase, To: ""}, "/to"},

		{"AttemptStarted with an invalid session", AttemptStarted{AttemptID: sampleAttempt, SessionID: "-x"}, "/sessionId"},
		{"AttemptStarted with an invalid attempt", AttemptStarted{AttemptID: "attempt-1"}, "/attemptId"},
		{"AttemptAbandoned with an unknown reason", AttemptAbandoned{AttemptID: sampleAttempt, Reason: "gone"}, "/reason"},
		{"AttemptFinished without output", AttemptFinished{AttemptID: sampleAttempt}, "/output"},
		{"AttemptFailed with the outcome completed", AttemptFailed{AttemptID: sampleAttempt, Outcome: OutcomeCompleted}, "/outcome"},
		{"AttemptFailed with an unknown outcome", AttemptFailed{AttemptID: sampleAttempt, Outcome: "exploded"}, "/outcome"},
		{"AttemptFailed without an outcome", AttemptFailed{AttemptID: sampleAttempt}, "/outcome"},
		{"AttemptFailed with an unknown error class", AttemptFailed{AttemptID: sampleAttempt, Outcome: OutcomeFailed, ErrorClass: "Oops"}, "/errorClass"},
		{"AttemptFailed with a long detail", AttemptFailed{AttemptID: sampleAttempt, Outcome: OutcomeFailed, Detail: long}, "/detail"},
		{"AttemptAwaitingInput without a request", AttemptAwaitingInput{AttemptID: sampleAttempt}, "/requestId"},
		{"AttemptReconciled to Finished without output", AttemptReconciled{AttemptID: sampleAttempt, To: AttemptStateFinished}, "/output"},
		{"AttemptReconciled to Failed with output", AttemptReconciled{AttemptID: sampleAttempt, To: AttemptStateFailed, Output: ref("attempts/a/turns/0001/output.json")}, "/output"},
		{"AttemptReconciled to Abandoned with output", AttemptReconciled{AttemptID: sampleAttempt, To: AttemptStateAbandoned, Output: ref("attempts/a/turns/0001/output.json")}, "/output"},
		{"AttemptReconciled to Running", AttemptReconciled{AttemptID: sampleAttempt, To: AttemptStateRunning}, "/to"},
		{"AttemptReconciled to an unknown state", AttemptReconciled{AttemptID: sampleAttempt, To: "Gone"}, "/to"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.payload.Validate()
			if err == nil {
				t.Fatalf("Validate accepted %#v", tc.payload)
			}
			wantIs(t, err, strictjson.ErrInvalidValue)
			if p, ok := pointerOf(err); !ok || p != tc.ptr {
				t.Errorf("JSON Pointer %q (found %v), want %q; error: %v", p, ok, tc.ptr, err)
			}
			if _, encErr := EncodeEvent(sampleEvent(tc.payload)); !errors.Is(encErr, ErrInvalidEvent) {
				t.Errorf("EncodeEvent error %v does not match ErrInvalidEvent", encErr)
			}
		})
	}
}

func TestPayloadRulesAcceptEdgeCases(t *testing.T) {
	ref := sampleRef(t, PlanPath(1))
	diamond := []PhaseDefinition{
		{ID: "a", DependsOn: []PhaseID{}},
		{ID: "b", DependsOn: []PhaseID{"a"}},
		{ID: "c", DependsOn: []PhaseID{"a"}},
		{ID: "d", DependsOn: []PhaseID{"b", "c"}},
	}
	// A dependency may be listed before the phase it names.
	reversed := []PhaseDefinition{
		{ID: "b", DependsOn: []PhaseID{"a"}},
		{ID: "a", DependsOn: []PhaseID{}},
	}
	many := make([]PhaseDefinition, 256)
	for i := range many {
		many[i] = PhaseDefinition{ID: PhaseID("q" + strconv.Itoa(i)), DependsOn: []PhaseID{}}
	}
	valid := []Payload{
		PlanAccepted{Revision: 1, Plan: ref, Phases: diamond},
		PlanAccepted{Revision: 1, Plan: ref, Phases: reversed},
		PlanAccepted{Revision: 1, Plan: ref, Phases: many},
		PlanAccepted{Revision: 1, Plan: ref, Phases: []PhaseDefinition{{ID: "a"}}}, // nil dependsOn
		PlanValidationFailed{Summary: strings.Repeat("é", MaxTextBytes/2)},
		RunSuspended{To: RunStateBlocked, Reason: SuspendReasonBaselineFailed, ResumeState: RunStateCreated},
		RunSuspended{To: RunStateRecoveryRequired, Reason: SuspendReasonUnknownOutcome, ResumeState: RunStateBlocked},
		RunSuspended{To: RunStateBlocked, Reason: SuspendReasonNoProgress, ResumeState: RunStateWaitingForUser},
		RunResumed{To: RunStateRecoveryRequired},
		PhaseSuspended{Revision: 1, PhaseID: samplePhase, To: PhaseStateRecoveryRequired, ResumeState: PhaseStateIntegrating},
		AttemptScheduled{AttemptID: sampleAttempt, Role: RoleEvaluator},
		AttemptScheduled{AttemptID: sampleAttempt, Role: RoleEvaluator, Revision: 2, PhaseID: samplePhase},
		AttemptScheduled{AttemptID: sampleAttempt, Role: RolePlanner},
		AttemptScheduled{AttemptID: sampleAttempt, Role: RoleCoordinator, Revision: 3},
		AttemptFailed{AttemptID: sampleAttempt, Outcome: OutcomeInterrupted},
		AttemptReconciled{AttemptID: sampleAttempt, To: AttemptStateAbandoned},
		PhaseReconciled{Revision: 1, PhaseID: samplePhase, To: PhaseStateAccepted},
	}
	for i, p := range valid {
		if err := p.Validate(); err != nil {
			t.Errorf("case %d (%T): %v", i, p, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Limits and the enumerations
// ---------------------------------------------------------------------------

func TestLimitsValueAndWith(t *testing.T) {
	l := Limits{MaxPhaseRetries: 2, MaxReplans: 1}
	want := map[LimitName]string{
		LimitMaxPhaseRetries: "2",
		LimitMaxReplans:      "1",
		LimitTimeout:         "unlimited",
		LimitMaxAgentSteps:   "unlimited",
		LimitMaxTurnCostUSD:  "unlimited",
	}
	if got := len(LimitNames()); got != len(want) {
		t.Fatalf("%d limit names, want %d", got, len(want))
	}
	for _, n := range LimitNames() {
		if got := l.Value(n); got != want[n] {
			t.Errorf("Value(%s) = %q, want %q", n, got, want[n])
		}
	}
	if got := l.Value("nope"); got != "" {
		t.Errorf("Value of an unknown limit = %q", got)
	}

	steps := []struct {
		name  LimitName
		value string
		check func(Limits) bool
	}{
		{LimitMaxPhaseRetries, "0", func(l Limits) bool { return l.MaxPhaseRetries == 0 }},
		{LimitMaxPhaseRetries, "7", func(l Limits) bool { return l.MaxPhaseRetries == 7 }},
		{LimitMaxReplans, "0", func(l Limits) bool { return l.MaxReplans == 0 }},
		{LimitMaxReplans, "12", func(l Limits) bool { return l.MaxReplans == 12 }},
		{LimitTimeout, "1h30m0s", func(l Limits) bool { return l.Timeout == "1h30m0s" }},
		{LimitTimeout, "5m0s", func(l Limits) bool { return l.Timeout == "5m0s" }},
		{LimitTimeout, "1.5s", func(l Limits) bool { return l.Timeout == "1.5s" }},
		{LimitTimeout, "100ms", func(l Limits) bool { return l.Timeout == "100ms" }},
		{LimitTimeout, "unlimited", func(l Limits) bool { return l.Timeout == "" }},
		{LimitMaxAgentSteps, "40", func(l Limits) bool { return l.MaxAgentSteps == 40 }},
		{LimitMaxAgentSteps, "unlimited", func(l Limits) bool { return l.MaxAgentSteps == 0 }},
		{LimitMaxTurnCostUSD, "2.5", func(l Limits) bool { return l.MaxTurnCostUSD == 2.5 }},
		{LimitMaxTurnCostUSD, "0.001", func(l Limits) bool { return l.MaxTurnCostUSD == 0.001 }},
		{LimitMaxTurnCostUSD, "100", func(l Limits) bool { return l.MaxTurnCostUSD == 100 }},
		{LimitMaxTurnCostUSD, "unlimited", func(l Limits) bool { return l.MaxTurnCostUSD == 0 }},
	}
	for _, s := range steps {
		got, err := l.With(s.name, s.value)
		if err != nil {
			t.Errorf("With(%s, %q): %v", s.name, s.value, err)
			continue
		}
		if !s.check(got) {
			t.Errorf("With(%s, %q) = %+v", s.name, s.value, got)
		}
		if v := got.Value(s.name); v != s.value {
			t.Errorf("Value(%s) after With(%q) = %q", s.name, s.value, v)
		}
		if err := got.Validate(); err != nil {
			t.Errorf("With(%s, %q) is invalid: %v", s.name, s.value, err)
		}
		// The receiver is not modified.
		if l != (Limits{MaxPhaseRetries: 2, MaxReplans: 1}) {
			t.Fatalf("With modified its receiver: %+v", l)
		}
	}

	bad := []struct {
		name  LimitName
		value string
	}{
		{LimitMaxPhaseRetries, ""}, {LimitMaxPhaseRetries, "-1"}, {LimitMaxPhaseRetries, "007"}, {LimitMaxPhaseRetries, "+5"},
		{LimitMaxPhaseRetries, "-0"}, {LimitMaxPhaseRetries, "1.5"}, {LimitMaxPhaseRetries, "abc"}, {LimitMaxPhaseRetries, "unlimited"},
		{LimitMaxPhaseRetries, "99999999999999999999"}, {LimitMaxPhaseRetries, " 1"}, {LimitMaxPhaseRetries, "1e2"},
		{LimitMaxReplans, "unlimited"}, {LimitMaxReplans, "-3"}, {LimitMaxReplans, "0x10"},
		{LimitTimeout, ""}, {LimitTimeout, "0s"}, {LimitTimeout, "-1m0s"}, {LimitTimeout, "90m"}, {LimitTimeout, "1h"},
		{LimitTimeout, "1h30m"}, {LimitTimeout, "soon"}, {LimitTimeout, "0"}, {LimitTimeout, "Unlimited"}, {LimitTimeout, "1m0s "},
		{LimitMaxAgentSteps, "0"}, {LimitMaxAgentSteps, "-1"}, {LimitMaxAgentSteps, "05"}, {LimitMaxAgentSteps, "2.0"}, {LimitMaxAgentSteps, ""},
		{LimitMaxTurnCostUSD, "0"}, {LimitMaxTurnCostUSD, "-1"}, {LimitMaxTurnCostUSD, "-0"}, {LimitMaxTurnCostUSD, "1e2"},
		{LimitMaxTurnCostUSD, "2.50"}, {LimitMaxTurnCostUSD, "NaN"}, {LimitMaxTurnCostUSD, "Inf"}, {LimitMaxTurnCostUSD, "+Inf"},
		{LimitMaxTurnCostUSD, "1e999"}, {LimitMaxTurnCostUSD, "$5"}, {LimitMaxTurnCostUSD, ".5"}, {LimitMaxTurnCostUSD, ""},
		{"maxSpeed", "1"}, {"", "1"},
	}
	for _, b := range bad {
		got, err := l.With(b.name, b.value)
		if err == nil {
			t.Errorf("With(%s, %q) = %+v, want an error", b.name, b.value, got)
			continue
		}
		wantIs(t, err, ErrInvalidLimit)
		if got != l {
			t.Errorf("With(%s, %q) returned %+v with its error, want the receiver", b.name, b.value, got)
		}
	}
}

func TestLimitsValidate(t *testing.T) {
	valid := []Limits{
		{},
		{MaxPhaseRetries: 3, MaxReplans: 2},
		{Timeout: "1ns"},
		{Timeout: "1h0m0s", MaxAgentSteps: 1, MaxTurnCostUSD: 0.01},
	}
	for _, l := range valid {
		if err := l.Validate(); err != nil {
			t.Errorf("%+v: %v", l, err)
		}
	}
	negZero := 0.0
	negZero = -negZero
	invalid := []Limits{
		{MaxPhaseRetries: -1}, {MaxReplans: -1}, {MaxAgentSteps: -1},
		{MaxTurnCostUSD: -0.5}, {MaxTurnCostUSD: negZero},
		{Timeout: "abc"}, {Timeout: "0s"}, {Timeout: "-5m0s"}, {Timeout: "90m"}, {Timeout: "1h"}, {Timeout: " 1m0s"},
	}
	for _, l := range invalid {
		wantIs(t, l.Validate(), ErrInvalidLimit)
	}
	nan := Limits{}
	nan.MaxTurnCostUSD = nanValue()
	wantIs(t, nan.Validate(), ErrInvalidLimit)
	inf := Limits{}
	inf.MaxTurnCostUSD = infValue()
	wantIs(t, inf.Validate(), ErrInvalidLimit)
}

func nanValue() float64 { z := 0.0; return z / z }
func infValue() float64 { z := 0.0; return 1 / z }

func TestLimitsJSON(t *testing.T) {
	l := Limits{MaxPhaseRetries: 2, MaxReplans: 1}
	b, err := strictjson.Encode(l)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"maxPhaseRetries":2,"maxReplans":1}` {
		t.Errorf("optional limits are not omitted: %s", b)
	}
	full := Limits{MaxPhaseRetries: 2, MaxReplans: 1, Timeout: "1h0m0s", MaxAgentSteps: 9, MaxTurnCostUSD: 0.25}
	b, err = strictjson.Encode(full)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"maxPhaseRetries":2,"maxReplans":1,"timeout":"1h0m0s","maxAgentSteps":9,"maxTurnCostUsd":0.25}` {
		t.Errorf("unexpected encoding: %s", b)
	}
	got, err := strictjson.Decode[Limits](b)
	if err != nil || got != full {
		t.Errorf("Decode = %+v, %v", got, err)
	}
	for name, doc := range map[string]string{
		"missing maxReplans": `{"maxPhaseRetries":2}`,
		"unknown member":     `{"maxPhaseRetries":2,"maxReplans":1,"speed":1}`,
		"null timeout":       `{"maxPhaseRetries":2,"maxReplans":1,"timeout":null}`,
		"negative":           `{"maxPhaseRetries":-2,"maxReplans":1}`,
		"non-canonical":      `{"maxPhaseRetries":2,"maxReplans":1,"timeout":"90m"}`,
	} {
		if _, err := strictjson.Decode[Limits]([]byte(doc)); err == nil {
			t.Errorf("%s: Decode accepted %s", name, doc)
		}
	}
}

func TestLimitNameText(t *testing.T) {
	for _, n := range LimitNames() {
		b, err := n.MarshalText()
		if err != nil || string(b) != string(n) {
			t.Errorf("MarshalText(%s) = %q, %v", n, b, err)
		}
		var back LimitName
		if err := back.UnmarshalText(b); err != nil || back != n {
			t.Errorf("UnmarshalText(%q) = %q, %v", b, back, err)
		}
	}
	var n LimitName
	for _, s := range []string{"", "maxreplans", "MaxReplans", "retries"} {
		wantIs(t, n.UnmarshalText([]byte(s)), ErrInvalidLimit)
		if n != "" {
			t.Errorf("a failed UnmarshalText set the value to %q", n)
		}
	}
	_, err := LimitName("x").MarshalText()
	wantIs(t, err, ErrInvalidLimit)
	want := []string{"maxPhaseRetries", "maxReplans", "timeout", "maxAgentSteps", "maxTurnCostUsd"}
	for i, n := range LimitNames() {
		if string(n) != want[i] {
			t.Errorf("limit name %d is %q, want %q", i, n, want[i])
		}
	}
}

// enumCase describes one closed enumeration for the table tests.
type enumCase struct {
	name   string
	values []string
	valid  func(string) bool
	// round trips a value through MarshalText and UnmarshalText.
	roundTrip func(string) (string, error)
	// unmarshal decodes text into a new value.
	unmarshal func(string) error
	marshal   func(string) error
}

func enumOf[T ~string, PT interface {
	*T
	UnmarshalText([]byte) error
}](name string, all []T, valid func(T) bool, marshal func(T) ([]byte, error)) enumCase {
	values := make([]string, len(all))
	for i, v := range all {
		values[i] = string(v)
	}
	return enumCase{
		name:   name,
		values: values,
		valid:  func(s string) bool { return valid(T(s)) },
		roundTrip: func(s string) (string, error) {
			b, err := marshal(T(s))
			if err != nil {
				return "", err
			}
			var v T
			if err := PT(&v).UnmarshalText(b); err != nil {
				return "", err
			}
			return string(v), nil
		},
		unmarshal: func(s string) error { var v T; return PT(&v).UnmarshalText([]byte(s)) },
		marshal:   func(s string) error { _, err := marshal(T(s)); return err },
	}
}

func TestEnumerations(t *testing.T) {
	cases := []enumCase{
		enumOf("SuspendReason", SuspendReasons(), SuspendReason.Valid, SuspendReason.MarshalText),
		enumOf("ReplanCause", ReplanCauses(), ReplanCause.Valid, ReplanCause.MarshalText),
		enumOf("CancelReason", CancelReasons(), CancelReason.Valid, CancelReason.MarshalText),
		enumOf("PhaseRecoveryCause", PhaseRecoveryCauses(), PhaseRecoveryCause.Valid, PhaseRecoveryCause.MarshalText),
		enumOf("AbandonReason", AbandonReasons(), AbandonReason.Valid, AbandonReason.MarshalText),
		enumOf("ExecutionOutcome", ExecutionOutcomes(), ExecutionOutcome.Valid, ExecutionOutcome.MarshalText),
		enumOf("ErrorClass", ErrorClasses(), ErrorClass.Valid, ErrorClass.MarshalText),
	}
	wantCounts := map[string]int{
		"SuspendReason": 8, "ReplanCause": 3, "CancelReason": 2, "PhaseRecoveryCause": 9,
		"AbandonReason": 2, "ExecutionOutcome": 5, "ErrorClass": 16,
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if len(c.values) != wantCounts[c.name] {
				t.Errorf("%d values, want %d", len(c.values), wantCounts[c.name])
			}
			seen := make(map[string]bool)
			for _, v := range c.values {
				if seen[v] {
					t.Errorf("value %q is listed twice", v)
				}
				seen[v] = true
				if !c.valid(v) {
					t.Errorf("%q is not valid", v)
				}
				if got, err := c.roundTrip(v); err != nil || got != v {
					t.Errorf("round trip of %q = %q, %v", v, got, err)
				}
			}
			for _, v := range []string{"", "unknown", strings.ToUpper(c.values[0]), c.values[0] + " ", " " + c.values[0]} {
				if c.valid(v) {
					t.Errorf("%q is valid", v)
				}
				wantIs(t, c.unmarshal(v), ErrInvalidEnum)
				wantIs(t, c.marshal(v), ErrInvalidEnum)
			}
		})
	}
}

func TestEnumerationSemantics(t *testing.T) {
	suspend := map[SuspendReason]RunState{
		SuspendReasonUserInput:            RunStateWaitingForUser,
		SuspendReasonUnknownOutcome:       RunStateRecoveryRequired,
		SuspendReasonBaselineFailed:       RunStateBlocked,
		SuspendReasonMissingEnvironment:   RunStateBlocked,
		SuspendReasonAuthenticationFailed: RunStateBlocked,
		SuspendReasonNoProgress:           RunStateBlocked,
		SuspendReasonLimitReached:         RunStateBlocked,
		SuspendReasonNeedsUser:            RunStateBlocked,
	}
	for _, r := range SuspendReasons() {
		if got := r.Target(); got != suspend[r] {
			t.Errorf("%s.Target() = %q, want %q", r, got, suspend[r])
		}
		if !r.Target().Suspended() {
			t.Errorf("%s leads to %s, which is not a suspended state", r, r.Target())
		}
	}
	if got := SuspendReason("x").Target(); got != "" {
		t.Errorf("an invalid reason leads to %q", got)
	}

	replan := map[ReplanCause]RunState{
		ReplanCauseReplanAuthorized:        RunStateExecuting,
		ReplanCauseFinalVerificationFailed: RunStateFinalVerification,
		ReplanCauseFinalEvaluationRejected: RunStateFinalEvaluation,
	}
	for _, c := range ReplanCauses() {
		if got := c.Source(); got != replan[c] {
			t.Errorf("%s.Source() = %q, want %q", c, got, replan[c])
		}
	}
	recovery := map[PhaseRecoveryCause]PhaseState{
		PhaseRecoveryCauseAttemptFailed:             PhaseStateRunning,
		PhaseRecoveryCauseOutputInvalid:             PhaseStateRunning,
		PhaseRecoveryCauseStatusReported:            PhaseStateRunning,
		PhaseRecoveryCauseScopeViolation:            PhaseStateVerifying,
		PhaseRecoveryCauseChecksFailed:              PhaseStateVerifying,
		PhaseRecoveryCauseRejected:                  PhaseStateEvaluating,
		PhaseRecoveryCauseEvaluatorRetriesExhausted: PhaseStateEvaluating,
		PhaseRecoveryCauseIntegrationConflict:       PhaseStateIntegrating,
		PhaseRecoveryCauseIntegrationChecksFailed:   PhaseStateIntegrating,
	}
	for _, c := range PhaseRecoveryCauses() {
		if got := c.Source(); got != recovery[c] {
			t.Errorf("%s.Source() = %q, want %q", c, got, recovery[c])
		}
	}
	abandon := map[AbandonReason]AttemptState{
		AbandonReasonLaunchNotOccurred:  AttemptStateScheduled,
		AbandonReasonSessionUnavailable: AttemptStateWaitingForUser,
	}
	for _, r := range AbandonReasons() {
		if got := r.Source(); got != abandon[r] {
			t.Errorf("%s.Source() = %q, want %q", r, got, abandon[r])
		}
	}
	if ReplanCause("x").Source() != "" || PhaseRecoveryCause("x").Source() != "" || AbandonReason("x").Source() != "" {
		t.Error("an invalid value has a source state")
	}
	// The values are the wire values of the specification.
	wire := map[string]bool{}
	for _, c := range ErrorClasses() {
		wire[string(c)] = true
	}
	for _, name := range []string{
		"TransientError", "RateLimitExceeded", "InvalidAgentOutput", "VerificationFailed", "SemanticRejection",
		"PlanInvalid", "SessionUnavailable", "AuthenticationFailed", "MissingEnvironment", "WorkspaceConflict",
		"ScopeViolation", "RoleContractViolation", "NeedsUserInput", "TurnLimitReached", "BudgetExceeded",
		"UnknownExecutionOutcome",
	} {
		if !wire[name] {
			t.Errorf("error class %s is missing", name)
		}
	}
}
