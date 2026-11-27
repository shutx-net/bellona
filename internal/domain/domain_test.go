package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/shutx-net/bellona/internal/strictjson"
)

func newReader(seed byte) *rand.ChaCha8 {
	var s [32]byte
	s[0] = seed
	return rand.NewChaCha8(s)
}

func TestGenerators(t *testing.T) {
	type gen struct {
		name   string
		prefix string
		make   func(r *rand.ChaCha8) (string, error)
		parse  func(s string) error
	}
	gens := []gen{
		{"run", "run-", func(r *rand.ChaCha8) (string, error) { id, err := NewRunID(r); return id.String(), err },
			func(s string) error { _, err := ParseRunID(s); return err }},
		{"attempt", "attempt-", func(r *rand.ChaCha8) (string, error) { id, err := NewAttemptID(r); return id.String(), err },
			func(s string) error { _, err := ParseAttemptID(s); return err }},
		{"request", "req-", func(r *rand.ChaCha8) (string, error) { id, err := NewRequestID(r); return id.String(), err },
			func(s string) error { _, err := ParseRequestID(s); return err }},
		{"session", "", func(r *rand.ChaCha8) (string, error) { id, err := NewSessionID(r); return id.String(), err },
			func(s string) error { _, err := ParseSessionID(s); return err }},
	}
	for _, g := range gens {
		t.Run(g.name, func(t *testing.T) {
			r := newReader(1)
			seen := make(map[string]bool)
			for range 50 {
				s, err := g.make(r)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.HasPrefix(s, g.prefix) {
					t.Errorf("%q lacks prefix %q", s, g.prefix)
				}
				if err := g.parse(s); err != nil {
					t.Errorf("generated %q does not parse: %v", s, err)
				}
				if seen[s] {
					t.Errorf("duplicate id %q", s)
				}
				seen[s] = true
			}
			// The same reader state yields the same ID.
			a, _ := g.make(newReader(7))
			b, _ := g.make(newReader(7))
			if a != b {
				t.Errorf("same randomness gave %q and %q", a, b)
			}
		})
	}
	if _, err := NewRunID(strings.NewReader("short")); err == nil {
		t.Error("NewRunID accepted a short reader")
	}
	if _, err := NewAttemptID(strings.NewReader("")); err == nil {
		t.Error("NewAttemptID accepted an empty reader")
	}
	if _, err := NewRequestID(strings.NewReader("1234567")); err == nil {
		t.Error("NewRequestID accepted a short reader")
	}
	if _, err := NewSessionID(strings.NewReader("0123456789abcde")); err == nil {
		t.Error("NewSessionID accepted a short reader")
	}
}

func TestNewSessionIDIsUUIDv4(t *testing.T) {
	r := newReader(3)
	for range 100 {
		id, err := NewSessionID(r)
		if err != nil {
			t.Fatal(err)
		}
		s := id.String()
		if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
			t.Fatalf("%q is not in 8-4-4-4-12 form", s)
		}
		if s != strings.ToLower(s) {
			t.Fatalf("%q is not lowercase", s)
		}
		if s[14] != '4' {
			t.Errorf("%q: version nibble is %c, want 4", s, s[14])
		}
		if !strings.ContainsRune("89ab", rune(s[19])) {
			t.Errorf("%q: variant nibble is %c, want one of 89ab", s, s[19])
		}
		if _, err := hex.DecodeString(strings.ReplaceAll(s, "-", "")); err != nil {
			t.Errorf("%q is not hex: %v", s, err)
		}
	}
	// All-ones randomness still gets the version and variant bits.
	id, err := NewSessionID(strings.NewReader(strings.Repeat("\xff", 16)))
	if err != nil || id != "ffffffff-ffff-4fff-bfff-ffffffffffff" {
		t.Errorf("NewSessionID(0xff...) = %q, %v", id, err)
	}
	id, err = NewSessionID(strings.NewReader(strings.Repeat("\x00", 16)))
	if err != nil || id != "00000000-0000-4000-8000-000000000000" {
		t.Errorf("NewSessionID(0x00...) = %q, %v", id, err)
	}
}

func TestIDKnownValues(t *testing.T) {
	id, err := NewRunID(strings.NewReader("\x00\x01\x02\x03\x04\x05\x06\xff"))
	if err != nil || id != "run-00010203040506ff" {
		t.Errorf("NewRunID = %q, %v", id, err)
	}
	aid, _ := NewAttemptID(strings.NewReader("\xab\xcd\xef\x01\x23\x45\x67\x89"))
	if aid != "attempt-abcdef0123456789" {
		t.Errorf("NewAttemptID = %q", aid)
	}
	rid, _ := NewRequestID(strings.NewReader("\x10\x20\x30\x40\x50\x60\x70\x80"))
	if rid != "req-1020304050607080" {
		t.Errorf("NewRequestID = %q", rid)
	}
}

// idCase describes one identifier type for the table tests.
type idCase struct {
	name      string
	valid     []string
	invalid   []string
	unmarshal func(b []byte) error
	marshal   func(s string) ([]byte, error)
	validate  func(s string) error
}

func idCases() []idCase {
	return []idCase{
		{
			name:  "RunID",
			valid: []string{"run-0123456789abcdef", "run-ffffffffffffffff"},
			invalid: []string{"", "run-", "run-0123456789ABCDEF", "run-0123456789abcde", "run-0123456789abcdef0",
				"Run-0123456789abcdef", "attempt-0123456789abcdef", "0123456789abcdef", "run-0123456789abcdeg",
				"run-0123456789abcdef\n", " run-0123456789abcdef", "run_0123456789abcdef"},
			unmarshal: func(b []byte) error { var v RunID; return v.UnmarshalText(b) },
			marshal:   func(s string) ([]byte, error) { return RunID(s).MarshalText() },
			validate:  func(s string) error { return RunID(s).Validate() },
		},
		{
			name:  "AttemptID",
			valid: []string{"attempt-0123456789abcdef"},
			invalid: []string{"", "attempt-", "attempt-0123456789ABCDEF", "run-0123456789abcdef", "att-0123456789abcdef",
				"attempt-0123456789abcdef00", "attempt-xyz"},
			unmarshal: func(b []byte) error { var v AttemptID; return v.UnmarshalText(b) },
			marshal:   func(s string) ([]byte, error) { return AttemptID(s).MarshalText() },
			validate:  func(s string) error { return AttemptID(s).Validate() },
		},
		{
			name:      "RequestID",
			valid:     []string{"req-0123456789abcdef"},
			invalid:   []string{"", "req-", "req-0123456789ABCDEF", "request-0123456789abcdef", "req-0123456789abcdef0"},
			unmarshal: func(b []byte) error { var v RequestID; return v.UnmarshalText(b) },
			marshal:   func(s string) ([]byte, error) { return RequestID(s).MarshalText() },
			validate:  func(s string) error { return RequestID(s).Validate() },
		},
		{
			name: "SessionID",
			valid: []string{
				"6ba7b810-9dad-41d1-80b4-00c04fd430c8", "session-01", "a", "0", "abc:def.ghi_jkl-mno", "A",
				strings.Repeat("a", 128),
			},
			invalid: []string{"", "-abc", ".abc", ":abc", "_abc", "has space", "a/b", "a\\b", "über", "a\x00b", "a\n",
				strings.Repeat("a", 129)},
			unmarshal: func(b []byte) error { var v SessionID; return v.UnmarshalText(b) },
			marshal:   func(s string) ([]byte, error) { return SessionID(s).MarshalText() },
			validate:  func(s string) error { return SessionID(s).Validate() },
		},
		{
			name:  "PhaseID",
			valid: []string{"phase-01", "p", "Phase.1_a-b", "1", strings.Repeat("p", 64)},
			invalid: []string{"", "-phase", ".phase", "_phase", "phase 01", "phase/01", "phase:01", "phase\n",
				strings.Repeat("p", 65), "ph\xffase"},
			unmarshal: func(b []byte) error { var v PhaseID; return v.UnmarshalText(b) },
			marshal:   func(s string) ([]byte, error) { return PhaseID(s).MarshalText() },
			validate:  func(s string) error { return PhaseID(s).Validate() },
		},
	}
}

func TestIDValidation(t *testing.T) {
	for _, c := range idCases() {
		t.Run(c.name, func(t *testing.T) {
			for _, s := range c.valid {
				if err := c.validate(s); err != nil {
					t.Errorf("Validate(%q) = %v", s, err)
				}
				if err := c.unmarshal([]byte(s)); err != nil {
					t.Errorf("UnmarshalText(%q) = %v", s, err)
				}
				if b, err := c.marshal(s); err != nil || string(b) != s {
					t.Errorf("MarshalText(%q) = %q, %v", s, b, err)
				}
			}
			for _, s := range c.invalid {
				if err := c.validate(s); !errors.Is(err, ErrInvalidID) {
					t.Errorf("Validate(%q) = %v, want ErrInvalidID", s, err)
				}
				if err := c.unmarshal([]byte(s)); !errors.Is(err, ErrInvalidID) {
					t.Errorf("UnmarshalText(%q) = %v, want ErrInvalidID", s, err)
				}
				if b, err := c.marshal(s); err == nil {
					t.Errorf("MarshalText(%q) = %q, want an error", s, b)
				}
			}
		})
	}
}

func TestUnmarshalTextLeavesTargetOnError(t *testing.T) {
	id := RunID("run-0123456789abcdef")
	if err := id.UnmarshalText([]byte("nope")); err == nil || id != "run-0123456789abcdef" {
		t.Errorf("UnmarshalText changed the target on error: %q, %v", id, err)
	}
	st := RunStateExecuting
	if err := st.UnmarshalText([]byte("nope")); err == nil || st != RunStateExecuting {
		t.Errorf("UnmarshalText changed the target on error: %q, %v", st, err)
	}
}

func TestParseFunctions(t *testing.T) {
	if id, err := ParseRunID("run-0123456789abcdef"); err != nil || id != "run-0123456789abcdef" {
		t.Errorf("ParseRunID = %q, %v", id, err)
	}
	if _, err := ParseRunID(""); !errors.Is(err, ErrInvalidID) {
		t.Errorf("ParseRunID(\"\") = %v", err)
	}
	if id, err := ParsePhaseID("phase-01"); err != nil || id.String() != "phase-01" {
		t.Errorf("ParsePhaseID = %q, %v", id, err)
	}
	if _, err := ParsePhaseID("bad id"); !errors.Is(err, ErrInvalidID) {
		t.Errorf("ParsePhaseID = %v", err)
	}
	if _, err := ParseAttemptID("attempt-0123456789abcdef"); err != nil {
		t.Error(err)
	}
	if _, err := ParseRequestID("req-0123456789abcdef"); err != nil {
		t.Error(err)
	}
	if _, err := ParseSessionID("a b"); !errors.Is(err, ErrInvalidID) {
		t.Errorf("ParseSessionID = %v", err)
	}
}

type idHolder struct {
	Run     RunID                  `json:"run"`
	Attempt AttemptID              `json:"attempt"`
	Phase   PhaseID                `json:"phase"`
	State   RunState               `json:"state"`
	PState  PhaseState             `json:"phaseState"`
	AState  AttemptState           `json:"attemptState"`
	Role    Role                   `json:"role"`
	Ref     ArtifactRef            `json:"ref"`
	Opt     ArtifactRef            `json:"opt,omitzero"`
	ByPhase map[PhaseID]PhaseState `json:"byPhase,omitzero"`
}

func TestJSONRoundTrip(t *testing.T) {
	ref, err := NewArtifactRef("plans/rev-0001/plan.json", []byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	v := idHolder{
		Run: "run-0123456789abcdef", Attempt: "attempt-0123456789abcdef", Phase: "phase-01",
		State: RunStateWaitingForUser, PState: PhaseStateIntegrationPending, AState: AttemptStateTimedOut,
		Role: RoleEvaluator, Ref: ref,
		ByPhase: map[PhaseID]PhaseState{"phase-01": PhaseStateAccepted, "phase-02": PhaseStatePending},
	}
	if err := strictjson.CheckContractType(reflect.TypeFor[idHolder]()); err != nil {
		t.Fatal(err)
	}
	data, err := strictjson.Encode(v)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"run":"run-0123456789abcdef","attempt":"attempt-0123456789abcdef","phase":"phase-01","state":"WaitingForUser",` +
		`"phaseState":"IntegrationPending","attemptState":"TimedOut","role":"evaluator",` +
		`"ref":{"path":"plans/rev-0001/plan.json","size":2,"sha256":"` + ref.SHA256 + `"},` +
		`"byPhase":{"phase-01":"Accepted","phase-02":"Pending"}}`
	if string(data) != want {
		t.Fatalf("Encode =\n%s\nwant\n%s", data, want)
	}
	got, err := strictjson.Decode[idHolder](data)
	if err != nil || !reflect.DeepEqual(got, v) {
		t.Fatalf("round trip = %+v, %v", got, err)
	}
	// Invalid members are rejected while decoding, with a pointer.
	for _, tt := range []struct{ old, new, pointer string }{
		{`"run-0123456789abcdef"`, `"run-0123456789ABCDEF"`, "/run"},
		{`"WaitingForUser"`, `"waitingforuser"`, "/state"},
		{`"evaluator"`, `"Evaluator"`, "/role"},
		{`"phase-01","state"`, `"","state"`, "/phase"},
		{`"phase-02":"Pending"`, `"phase 02":"Pending"`, "/byPhase/phase 02"},
	} {
		_, err := strictjson.Decode[idHolder]([]byte(strings.Replace(string(data), tt.old, tt.new, 1)))
		var se *strictjson.Error
		if !errors.Is(err, strictjson.ErrInvalidValue) || !errors.As(err, &se) || se.Pointer != tt.pointer {
			t.Errorf("replace %s: error = %v, want ErrInvalidValue at %q", tt.new, err, tt.pointer)
		}
	}
	// The zero ArtifactRef is an absent optional member.
	if strings.Contains(string(data), `"opt"`) {
		t.Errorf("zero ArtifactRef was encoded: %s", data)
	}
}

func TestStateLists(t *testing.T) {
	names := func(n int, get func(i int) string) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = get(i)
		}
		return out
	}
	runs := RunStates()
	if got, want := names(len(runs), func(i int) string { return string(runs[i]) }), []string{
		"Created", "StrategySelection", "Planning", "PlanValidation", "WaitingApproval", "Executing",
		"FinalVerification", "FinalEvaluation", "Completed", "Cancelled", "WaitingForUser", "Blocked", "RecoveryRequired",
	}; !slices.Equal(got, want) || len(runs) != 13 {
		t.Errorf("RunStates = %v", got)
	}
	phases := PhaseStates()
	if got, want := names(len(phases), func(i int) string { return string(phases[i]) }), []string{
		"Pending", "Ready", "Running", "Verifying", "Evaluating", "IntegrationPending", "Integrating", "Accepted",
		"Recovering", "Failed", "Superseded", "WaitingForUser", "RecoveryRequired",
	}; !slices.Equal(got, want) || len(phases) != 13 {
		t.Errorf("PhaseStates = %v", got)
	}
	attempts := AttemptStates()
	if got, want := names(len(attempts), func(i int) string { return string(attempts[i]) }), []string{
		"Scheduled", "Running", "Finished", "Failed", "TimedOut", "Cancelled", "WaitingForUser", "Abandoned", "Unknown",
	}; !slices.Equal(got, want) || len(attempts) != 9 {
		t.Errorf("AttemptStates = %v", got)
	}
	// The returned slices are copies.
	runs[0] = "Mutated"
	if RunStates()[0] != RunStateCreated {
		t.Error("RunStates returns the internal slice")
	}
}

func TestStateConstants(t *testing.T) {
	if RunStateCreated != "Created" || RunStateRecoveryRequired != "RecoveryRequired" ||
		PhaseStateIntegrationPending != "IntegrationPending" || PhaseStateRunning != "Running" ||
		AttemptStateTimedOut != "TimedOut" || AttemptStateRunning != "Running" {
		t.Error("state constants do not carry the bare SPEC names")
	}
}

func TestStateSets(t *testing.T) {
	wantTerminal := []RunState{RunStateCompleted, RunStateCancelled}
	wantSuspended := []RunState{RunStateWaitingForUser, RunStateBlocked, RunStateRecoveryRequired}
	for _, s := range RunStates() {
		if got, want := s.Terminal(), slices.Contains(wantTerminal, s); got != want {
			t.Errorf("RunState(%s).Terminal() = %v", s, got)
		}
		if got, want := s.Suspended(), slices.Contains(wantSuspended, s); got != want {
			t.Errorf("RunState(%s).Suspended() = %v", s, got)
		}
		if !s.Valid() {
			t.Errorf("%s is not valid", s)
		}
	}
	wantAttemptTerminal := []AttemptState{
		AttemptStateFinished, AttemptStateFailed, AttemptStateTimedOut, AttemptStateCancelled, AttemptStateAbandoned,
	}
	for _, s := range AttemptStates() {
		if got, want := s.Terminal(), slices.Contains(wantAttemptTerminal, s); got != want {
			t.Errorf("AttemptState(%s).Terminal() = %v", s, got)
		}
		if !s.Valid() {
			t.Errorf("%s is not valid", s)
		}
	}
	for _, s := range PhaseStates() {
		if !s.Valid() {
			t.Errorf("%s is not valid", s)
		}
	}
	if RunState("").Valid() || PhaseState("pending").Valid() || AttemptState("Done").Valid() {
		t.Error("unknown states must not be valid")
	}
}

func TestStateText(t *testing.T) {
	for _, s := range RunStates() {
		b, err := s.MarshalText()
		if err != nil || string(b) != string(s) {
			t.Errorf("RunState %s MarshalText = %q, %v", s, b, err)
		}
		var got RunState
		if err := got.UnmarshalText(b); err != nil || got != s {
			t.Errorf("RunState %s UnmarshalText = %q, %v", s, got, err)
		}
	}
	for _, s := range PhaseStates() {
		var got PhaseState
		b, _ := s.MarshalText()
		if err := got.UnmarshalText(b); err != nil || got != s {
			t.Errorf("PhaseState %s UnmarshalText = %q, %v", s, got, err)
		}
	}
	for _, s := range AttemptStates() {
		var got AttemptState
		b, _ := s.MarshalText()
		if err := got.UnmarshalText(b); err != nil || got != s {
			t.Errorf("AttemptState %s UnmarshalText = %q, %v", s, got, err)
		}
	}
	for _, bad := range []string{"", "created", "CREATED", "Created ", "Done", "Unknown"} {
		var r RunState
		if err := r.UnmarshalText([]byte(bad)); !errors.Is(err, ErrInvalidState) {
			t.Errorf("RunState.UnmarshalText(%q) = %v", bad, err)
		}
	}
	var p PhaseState
	if err := p.UnmarshalText([]byte("Scheduled")); !errors.Is(err, ErrInvalidState) {
		t.Errorf("PhaseState accepted an Attempt state: %v", err)
	}
	var a AttemptState
	if err := a.UnmarshalText([]byte("Pending")); !errors.Is(err, ErrInvalidState) {
		t.Errorf("AttemptState accepted a Phase state: %v", err)
	}
	if _, err := RunState("bogus").MarshalText(); err == nil {
		t.Error("MarshalText accepted an unknown state")
	}
	if _, err := PhaseState("").MarshalText(); err == nil {
		t.Error("MarshalText accepted an empty state")
	}
	if _, err := AttemptState("bogus").MarshalText(); err == nil {
		t.Error("MarshalText accepted an unknown state")
	}
}

func TestRole(t *testing.T) {
	want := []Role{"coordinator", "planner", "implementer", "evaluator"}
	if got := Roles(); !slices.Equal(got, want) {
		t.Errorf("Roles() = %v", got)
	}
	if RoleCoordinator != "coordinator" || RolePlanner != "planner" || RoleImplementer != "implementer" || RoleEvaluator != "evaluator" {
		t.Error("role constants")
	}
	for _, r := range Roles() {
		if !r.Valid() || r.Validate() != nil {
			t.Errorf("%s is not valid", r)
		}
		b, err := r.MarshalText()
		var got Role
		if err != nil || got.UnmarshalText(b) != nil || got != r {
			t.Errorf("round trip of %s: %q %v", r, got, err)
		}
		if got, want := r.PhaseBound(), r == RoleImplementer || r == RoleEvaluator; got != want {
			t.Errorf("%s.PhaseBound() = %v", r, got)
		}
	}
	for _, bad := range []string{"", "Planner", "engine", "planner "} {
		var r Role
		if err := r.UnmarshalText([]byte(bad)); !errors.Is(err, ErrInvalidRole) {
			t.Errorf("UnmarshalText(%q) = %v", bad, err)
		}
		if Role(bad).Valid() {
			t.Errorf("Role(%q).Valid()", bad)
		}
	}
	if _, err := Role("x").MarshalText(); err == nil {
		t.Error("MarshalText accepted an unknown role")
	}
	roles := Roles()
	roles[0] = "mutated"
	if Roles()[0] != RoleCoordinator {
		t.Error("Roles returns the internal slice")
	}
}

const goodSum = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" // sha256 of empty input

func TestValidateRunPathAccepts(t *testing.T) {
	long := strings.Repeat("a", 100) + "/" + strings.Repeat("b", 100) + "/" + strings.Repeat("c", 53)
	if len(long) != MaxPathBytes {
		t.Fatalf("fixture has %d bytes", len(long))
	}
	for _, p := range []string{
		"plans/rev-0001/plan.json",
		"attempts/attempt-0123456789abcdef/turns/0001/output.json",
		"definitions/roles/planner.md",
		"task.json", "a", "_x", "A-b_c.d",
		"interactions/req-0123456789abcdef/answers/Q-001.json",
		strings.Repeat("s", 128),
		long,
	} {
		if err := ValidateRunPath(p); err != nil {
			t.Errorf("ValidateRunPath(%q) = %v", p, err)
		}
	}
}

func TestValidateRunPathRejects(t *testing.T) {
	tests := []struct{ name, path string }{
		{"empty", ""},
		{"absolute", "/etc/passwd"},
		{"absolute root only", "/"},
		{"dot-dot segment", "plans/../task.json"},
		{"leading dot-dot", "../task.json"},
		{"only dot-dot", ".."},
		{"dot segment", "plans/./plan.json"},
		{"only dot", "."},
		{"empty segment", "plans//plan.json"},
		{"trailing slash", "plans/"},
		{"backslash", `plans\plan.json`},
		{"backslash only", `\`},
		{"NUL byte", "plans/pl\x00an.json"},
		{"hidden file", ".plan.json"},
		{"hidden segment", "plans/.tmp/plan.json"},
		{"temporary file", "plans/.plan.json.tmp123"},
		{"hidden directory", ".git/config"},
		{"invalid UTF-8", "plans/\xff.json"},
		{"non-ASCII", "plans/pl\u00e4n.json"},
		{"space", "plans/my plan.json"},
		{"colon", "c:/plan.json"},
		{"256 bytes", strings.Repeat("a", 100) + "/" + strings.Repeat("b", 100) + "/" + strings.Repeat("c", 54)},
		{"129 byte segment", strings.Repeat("s", 129)},
		{"129 byte inner segment", "x/" + strings.Repeat("s", 129) + "/y"},
		{"newline", "plans/plan.json\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateRunPath(tt.path); !errors.Is(err, ErrInvalidPath) {
				t.Errorf("ValidateRunPath(%q) = %v, want ErrInvalidPath", tt.path, err)
			}
		})
	}
}

func TestArtifactRefValidate(t *testing.T) {
	good := ArtifactRef{Path: "plans/rev-0001/plan.json", Size: 0, SHA256: goodSum}
	if err := good.Validate(); err != nil {
		t.Fatalf("Validate = %v", err)
	}
	tests := []struct {
		name   string
		mutate func(r *ArtifactRef)
		want   error
	}{
		{"absolute path", func(r *ArtifactRef) { r.Path = "/plan.json" }, ErrInvalidPath},
		{"dot-dot", func(r *ArtifactRef) { r.Path = "../plan.json" }, ErrInvalidPath},
		{"dot segment", func(r *ArtifactRef) { r.Path = "a/./b" }, ErrInvalidPath},
		{"empty segment", func(r *ArtifactRef) { r.Path = "a//b" }, ErrInvalidPath},
		{"backslash", func(r *ArtifactRef) { r.Path = `a\b` }, ErrInvalidPath},
		{"NUL", func(r *ArtifactRef) { r.Path = "a\x00b" }, ErrInvalidPath},
		{"dot segment start", func(r *ArtifactRef) { r.Path = "a/.b" }, ErrInvalidPath},
		{"invalid UTF-8", func(r *ArtifactRef) { r.Path = "a/\xfe" }, ErrInvalidPath},
		{"overlong path", func(r *ArtifactRef) { r.Path = strings.Repeat("a/", 128) + "a" }, ErrInvalidPath},
		{"empty path", func(r *ArtifactRef) { r.Path = "" }, ErrInvalidPath},
		{"negative size", func(r *ArtifactRef) { r.Size = -1 }, ErrInvalidArtifactRef},
		{"uppercase sha", func(r *ArtifactRef) { r.SHA256 = strings.ToUpper(goodSum) }, ErrInvalidArtifactRef},
		{"short sha", func(r *ArtifactRef) { r.SHA256 = goodSum[:63] }, ErrInvalidArtifactRef},
		{"long sha", func(r *ArtifactRef) { r.SHA256 = goodSum + "0" }, ErrInvalidArtifactRef},
		{"non-hex sha", func(r *ArtifactRef) { r.SHA256 = goodSum[:63] + "g" }, ErrInvalidArtifactRef},
		{"empty sha", func(r *ArtifactRef) { r.SHA256 = "" }, ErrInvalidArtifactRef},
		{"sha prefix", func(r *ArtifactRef) { r.SHA256 = "sha256:" + goodSum }, ErrInvalidArtifactRef},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := good
			tt.mutate(&r)
			if err := r.Validate(); !errors.Is(err, tt.want) {
				t.Errorf("Validate(%+v) = %v, want %v", r, err, tt.want)
			}
		})
	}
}

func TestNewArtifactRef(t *testing.T) {
	data := []byte("hello\n")
	ref, err := NewArtifactRef("attempts/attempt-0123456789abcdef/turns/0001/output.json", data)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if ref.Size != 6 || ref.SHA256 != hex.EncodeToString(sum[:]) || ref.Validate() != nil || ref.IsZero() {
		t.Errorf("NewArtifactRef = %+v", ref)
	}
	if empty, err := NewArtifactRef("task.json", nil); err != nil || empty.Size != 0 || empty.SHA256 != goodSum {
		t.Errorf("NewArtifactRef(nil) = %+v, %v", empty, err)
	}
	if _, err := NewArtifactRef("../x", data); !errors.Is(err, ErrInvalidPath) {
		t.Errorf("NewArtifactRef with a bad path = %v", err)
	}
	if !(ArtifactRef{}).IsZero() {
		t.Error("the zero ArtifactRef must be zero")
	}
	if (ArtifactRef{}).Validate() == nil {
		t.Error("the zero ArtifactRef must not validate")
	}
}

func TestArtifactRefIsAContract(t *testing.T) {
	if err := strictjson.CheckContractType(reflect.TypeFor[ArtifactRef]()); err != nil {
		t.Fatal(err)
	}
	ref, _ := NewArtifactRef("task.json", []byte("x"))
	got, err := strictjson.Decode[ArtifactRef]([]byte(`{"path":"task.json","size":1,"sha256":"` + ref.SHA256 + `"}`))
	if err != nil || got != ref {
		t.Fatalf("Decode = %+v, %v", got, err)
	}
	if _, err := strictjson.Decode[ArtifactRef]([]byte(`{"path":"task.json","size":1}`)); !errors.Is(err, strictjson.ErrMissingMember) {
		t.Errorf("missing sha256: %v", err)
	}
	if _, err := strictjson.Decode[ArtifactRef]([]byte(`{"path":"../task.json","size":1,"sha256":"` + ref.SHA256 + `"}`)); !errors.Is(err, strictjson.ErrInvalidValue) {
		t.Errorf("bad path: %v", err)
	}
}

func TestLayout(t *testing.T) {
	checks := []struct{ got, want string }{
		{RoleDefinitionPath(RolePlanner), "definitions/roles/planner.md"},
		{RoleDefinitionPath(RoleEvaluator), "definitions/roles/evaluator.md"},
		{PlanPath(1), "plans/rev-0001/plan.json"},
		{PlanPath(12), "plans/rev-0012/plan.json"},
		{AttemptOutputPath("attempt-0123456789abcdef", 1), "attempts/attempt-0123456789abcdef/turns/0001/output.json"},
		{AttemptOutputPath("attempt-0123456789abcdef", 23), "attempts/attempt-0123456789abcdef/turns/0023/output.json"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
		if err := ValidateRunPath(c.got); err != nil {
			t.Errorf("layout path %q is not a valid Run path: %v", c.got, err)
		}
		if IsReservedPath(c.got) {
			t.Errorf("layout path %q must not be reserved", c.got)
		}
	}
	for _, p := range []string{TaskPath, ManifestPath, PolicyPath, EventsPath, SnapshotPath, LockPath, DefinitionsDir, LogsDir, WorktreesDir} {
		if err := ValidateRunPath(p); err != nil {
			t.Errorf("layout constant %q is not a valid Run path: %v", p, err)
		}
	}
	if TaskPath != "task.json" || ManifestPath != "manifest.json" || PolicyPath != "policy.json" ||
		EventsPath != "events.jsonl" || SnapshotPath != "state.json" || LockPath != "run.lock" ||
		DefinitionsDir != "definitions" || LogsDir != "logs" || WorktreesDir != "worktrees" {
		t.Error("layout constants differ from SPEC §12")
	}
}

func TestIsReservedPath(t *testing.T) {
	for _, p := range []string{
		"events.jsonl", "state.json", "run.lock", "logs", "logs/a.log", "logs/attempt-1/stdout.log", "worktrees",
		"worktrees/phase-01/README.md",
	} {
		if !IsReservedPath(p) {
			t.Errorf("IsReservedPath(%q) = false", p)
		}
	}
	for _, p := range []string{
		"task.json", "manifest.json", "policy.json", "plans/rev-0001/plan.json", "definitions/roles/planner.md",
		"logs2/a", "mylogs/a", "events.jsonl.bak", "attempts/attempt-0123456789abcdef/logs/x", "", "state.json/x",
		"a/events.jsonl",
	} {
		if IsReservedPath(p) {
			t.Errorf("IsReservedPath(%q) = true", p)
		}
	}
}
