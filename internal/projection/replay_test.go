package projection

import (
	"bytes"
	"errors"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	json "encoding/json/v2"

	"github.com/shutx-net/bellona/internal/domain"
	"github.com/shutx-net/bellona/internal/fsm"
	"github.com/shutx-net/bellona/internal/strictjson"
)

// The tests of this file cover T-04 and T-13 (event replay, identical state from
// identical sequences, detection of illegal and inconsistent records) and
// R-PER-02 (the snapshot is rebuildable from events).
//
// lifecycle drives a whole Run from RunCreated to RunCompleted through every
// kind of step that the happy path and the common detours have: two phases, an
// Evaluator that fails once, a limit reached while the final evaluation runs,
// a policy amendment, and the resume.
func lifecycle(t *testing.T) *run {
	t.Helper()
	r := reachRun(t, domain.RunStateExecuting) // the scripted prefix, nine events

	phase := func(id domain.PhaseID) {
		r.do(domain.PhaseReady{Revision: 1, PhaseID: id})
		impl := r.implement(id)
		r.finish(impl)
		r.do(domain.PhaseVerificationStarted{Revision: 1, PhaseID: id, AttemptID: impl})
		r.do(domain.PhaseEvaluationStarted{Revision: 1, PhaseID: id})
		eval1 := r.schedule(domain.RoleEvaluator, 1, id)
		r.fail(eval1)
		r.do(domain.PhaseEvaluationRetried{Revision: 1, PhaseID: id, FailedAttemptID: eval1})
		eval2 := r.schedule(domain.RoleEvaluator, 1, id)
		r.finish(eval2)
		r.do(domain.PhaseIntegrationQueued{Revision: 1, PhaseID: id})
		r.do(domain.PhaseIntegrationStarted{Revision: 1, PhaseID: id})
		r.do(domain.PhaseAccepted{Revision: 1, PhaseID: id, AttemptID: impl})
	}
	phase(phase1)
	phase(phase2)
	r.do(domain.FinalVerificationStarted{})
	r.do(domain.FinalEvaluationStarted{})
	r.do(domain.RunSuspended{To: domain.RunStateBlocked, Reason: domain.SuspendReasonLimitReached,
		ResumeState: domain.RunStateFinalEvaluation, Detail: "the evaluator ran out of retries"})
	r.do(domain.PolicyAmended{Changes: []domain.LimitChange{{Limit: domain.LimitMaxPhaseRetries, Previous: "2", New: "4"}}})
	r.do(domain.RunResumed{To: domain.RunStateFinalEvaluation})
	r.do(domain.RunCompleted{})
	return r
}

func TestReplayLifecycleMatchesIncrementalApply(t *testing.T) {
	r := lifecycle(t)
	if r.state.Run.State != domain.RunStateCompleted {
		t.Fatalf("the Run is %s", r.state.Run.State)
	}
	for _, id := range []domain.PhaseID{phase1, phase2} {
		if got := phaseOf(t, r.state, id).State; got != domain.PhaseStateAccepted {
			t.Errorf("%s is %s", id, got)
		}
	}
	if len(r.events) < 45 {
		t.Errorf("the lifecycle has only %d events", len(r.events))
	}

	replayed, err := Replay(r.events)
	if err != nil {
		t.Fatal(err)
	}
	if !Equal(replayed, r.state) {
		t.Fatalf("Replay differs from incremental Apply:\nreplay %s\napply  %s", canon(t, replayed), canon(t, r.state))
	}
	// Replaying twice gives the same bytes.
	again, err := Replay(r.events)
	if err != nil {
		t.Fatal(err)
	}
	if a, b := canon(t, replayed), canon(t, again); !bytes.Equal(a, b) {
		t.Errorf("two replays differ:\n%s\n%s", a, b)
	}
	if replayed.LastSeq != uint64(len(r.events)) {
		t.Errorf("LastSeq is %d for %d events", replayed.LastSeq, len(r.events))
	}
	last := r.events[len(r.events)-1]
	if !replayed.Run.UpdatedAt.Equal(last.Time) {
		t.Errorf("Run.UpdatedAt is %s, want the time of the last event, %s", replayed.Run.UpdatedAt, last.Time)
	}
	if !replayed.Run.CreatedAt.Equal(r.events[0].Time) {
		t.Errorf("Run.CreatedAt is %s, want the time of the first event", replayed.Run.CreatedAt)
	}

	// The facts of the lifecycle.
	if got := replayed.Limits.MaxPhaseRetries; got != 4 || len(replayed.Amendments) != 1 {
		t.Errorf("MaxPhaseRetries %d, %d amendments", got, len(replayed.Amendments))
	}
	if len(replayed.Run.Suspensions) != 0 || replayed.Run.PlanRevision != 1 || len(replayed.Revisions) != 1 {
		t.Errorf("suspensions %d, PlanRevision %d, revisions %d", len(replayed.Run.Suspensions), replayed.Run.PlanRevision, len(replayed.Revisions))
	}
	if got := replayed.UncertainAttempts(); len(got) != 0 {
		t.Errorf("uncertain Attempts after a completed Run: %v", got)
	}
	// phase-01: the implementer, the failed evaluator and the one that finished.
	if ph := phaseOf(t, replayed, phase1); len(ph.Attempts) != 3 {
		t.Errorf("phase-01 has the Attempts %v", ph.Attempts)
	}
	for _, e := range r.events {
		if e.Type == domain.EventPolicyAmended && !replayed.Amendments[0].Time.Equal(e.Time) {
			t.Errorf("the amendment time is %s, want %s", replayed.Amendments[0].Time, e.Time)
		}
	}
}

// Timestamps in the state come only from event times: no clock is read, so
// every time that the state holds is the time of some event (or absent).
func TestReplayTimestampsComeFromEvents(t *testing.T) {
	r := lifecycle(t)
	times := make(map[time.Time]bool)
	for _, e := range r.events {
		times[e.Time] = true
	}
	found := 0
	var walk func(path string, v reflect.Value)
	walk = func(path string, v reflect.Value) {
		switch v.Kind() {
		case reflect.Struct:
			if v.Type() == reflect.TypeFor[time.Time]() {
				ts := v.Interface().(time.Time)
				if ts.IsZero() {
					return
				}
				found++
				if !times[ts] {
					t.Errorf("%s is %s, which is the time of no event", path, ts.Format(time.RFC3339Nano))
				}
				return
			}
			for i := range v.NumField() {
				walk(path+"."+v.Type().Field(i).Name, v.Field(i))
			}
		case reflect.Slice:
			for i := range v.Len() {
				walk(path+"[]", v.Index(i))
			}
		}
	}
	walk("State", reflect.ValueOf(r.state))
	if found < 30 {
		t.Errorf("the walk found only %d timestamps", found)
	}

	// The same events with other times give the same state apart from the
	// times: the reducer does not look at the wall clock.
	shifted := make([]domain.Event, len(r.events))
	for i, e := range r.events {
		e.Time = e.Time.Add(48 * time.Hour)
		shifted[i] = e
	}
	got, err := Replay(shifted)
	if err != nil {
		t.Fatal(err)
	}
	want := r.state.Run.UpdatedAt.Add(48 * time.Hour)
	if !got.Run.UpdatedAt.Equal(want) {
		t.Errorf("Run.UpdatedAt is %s, want %s", got.Run.UpdatedAt, want)
	}
	for _, a := range got.Attempts {
		if a.ScheduledAt.Before(baseTime.Add(48 * time.Hour)) {
			t.Errorf("attempt %s was scheduled at %s", a.ID, a.ScheduledAt)
		}
	}
}

func TestReplayOfNothing(t *testing.T) {
	got, err := Replay(nil)
	if err != nil || !Equal(got, State{}) {
		t.Errorf("Replay(nil) = %+v, %v", got, err)
	}
	got, err = Replay([]domain.Event{})
	if err != nil || !Equal(got, State{}) {
		t.Errorf("Replay([]) = %+v, %v", got, err)
	}
	if _, err := EncodeSnapshot(got); err == nil {
		t.Error("EncodeSnapshot encoded the empty State")
	}
}

// Replay reports the first record that cannot be applied, by sequence number,
// for a gap, a wrong Run ID, an illegal transition and an invalid event.
func TestReplayReportsTheOffendingRecord(t *testing.T) {
	r := lifecycle(t)
	events := r.events
	at := func(i int) uint64 { return events[i].Seq }

	t.Run("a sequence gap", func(t *testing.T) {
		cut := append(append([]domain.Event(nil), events[:20]...), events[21:]...)
		requireReplayError(t, cut, at(21), ErrInvalidEvent)
	})
	t.Run("a repeated record", func(t *testing.T) {
		dup := append(append([]domain.Event(nil), events[:20]...), events[19])
		dup = append(dup, events[20:]...)
		requireReplayError(t, dup, at(19), ErrInvalidEvent)
	})
	t.Run("a wrong Run ID", func(t *testing.T) {
		bad := append([]domain.Event(nil), events...)
		bad[30].RunID = "run-fedcba9876543210"
		requireReplayError(t, bad, at(30), ErrInvalidEvent)
	})
	t.Run("an illegal transition", func(t *testing.T) {
		// RunCompleted while the Run is still Executing.
		bad := append([]domain.Event(nil), events[:12]...)
		bad = append(bad, mkEvent(events[11].Seq+1, domain.RunCompleted{}))
		requireReplayError(t, bad, events[11].Seq+1, fsm.ErrIllegalTransition)
	})
	t.Run("an invalid payload", func(t *testing.T) {
		bad := append([]domain.Event(nil), events...)
		bad[25].Payload = domain.PhaseReady{Revision: 0, PhaseID: phase1}
		bad[25].Type = domain.EventPhaseReady
		requireReplayError(t, bad, at(25), ErrInvalidEvent)
	})
	t.Run("the first event is not RunCreated", func(t *testing.T) {
		requireReplayError(t, events[1:], at(1), ErrInvalidEvent)
	})
	t.Run("an event after the end", func(t *testing.T) {
		bad := append(append([]domain.Event(nil), events...), mkEvent(events[len(events)-1].Seq+1, domain.RunPrepared{}))
		requireReplayError(t, bad, events[len(events)-1].Seq+1, ErrInvalidEvent)
	})
}

func requireReplayError(t *testing.T, events []domain.Event, seq uint64, sentinel error) {
	t.Helper()
	got, err := Replay(events)
	var ae *ApplyError
	if !errors.As(err, &ae) {
		t.Fatalf("Replay returned %v, want an *ApplyError", err)
	}
	if ae.Seq != seq {
		t.Errorf("the error names record %d (%s), want %d: %v", ae.Seq, ae.Type, seq, err)
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("%v does not match %v", err, sentinel)
	}
	if !Equal(got, State{}) {
		t.Errorf("Replay returned a state with its error: %s", canon(t, got))
	}
}

// ---------------------------------------------------------------------------
// Snapshot
// ---------------------------------------------------------------------------

func TestReplaySnapshotRoundTrip(t *testing.T) {
	r := lifecycle(t)
	data, err := EncodeSnapshot(r.state)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(data, []byte("}\n")) || bytes.Count(data, []byte("\n\n")) != 0 {
		t.Errorf("the snapshot does not end in a single newline")
	}
	if !strings.HasPrefix(string(data), "{\n  \"schemaVersion\": 1,\n  \"state\": {\n    \"runId\": \"run-0123456789abcdef\",\n    \"lastSeq\": ") {
		t.Errorf("the snapshot does not have the documented shape:\n%.200s", data)
	}
	again, err := EncodeSnapshot(r.state)
	if err != nil || !bytes.Equal(data, again) {
		t.Errorf("EncodeSnapshot is not deterministic (%v)", err)
	}
	dec, err := DecodeSnapshot(data)
	if err != nil {
		t.Fatal(err)
	}
	if !Equal(dec, r.state) {
		t.Fatalf("the decoded snapshot differs:\nwant %s\ngot  %s", canon(t, r.state), canon(t, dec))
	}
	// Encoding the decoded state gives the same bytes.
	if reenc, err := EncodeSnapshot(dec); err != nil || !bytes.Equal(reenc, data) {
		t.Errorf("re-encoding the decoded snapshot gives other bytes (%v)", err)
	}
	// The last applied sequence number is state.lastSeq.
	var peek struct {
		State struct {
			LastSeq uint64 `json:"lastSeq"`
		} `json:"state"`
	}
	if err := json.Unmarshal(data, &peek); err != nil || peek.State.LastSeq != r.state.LastSeq {
		t.Errorf("state.lastSeq is %d (%v), want %d", peek.State.LastSeq, err, r.state.LastSeq)
	}
}

// A snapshot taken part way plus the events after it give the same state as the
// replay of all events: this is how the engine uses the snapshot.
func TestReplaySnapshotThenTail(t *testing.T) {
	r := lifecycle(t)
	want, err := Replay(r.events)
	if err != nil {
		t.Fatal(err)
	}
	for _, cut := range []int{1, 2, 9, 20, 30, len(r.events) - 1, len(r.events)} {
		prefix, err := Replay(r.events[:cut])
		if err != nil {
			t.Fatal(err)
		}
		data, err := EncodeSnapshot(prefix)
		if err != nil {
			t.Fatal(err)
		}
		state, err := DecodeSnapshot(data)
		if err != nil {
			t.Fatalf("cut %d: %v", cut, err)
		}
		if state.LastSeq != uint64(cut) {
			t.Fatalf("cut %d: LastSeq is %d", cut, state.LastSeq)
		}
		for _, e := range r.events[cut:] {
			if state, err = Apply(state, e); err != nil {
				t.Fatalf("cut %d: %v", cut, err)
			}
		}
		if !Equal(state, want) {
			t.Errorf("cut %d: the snapshot plus the tail differs from the replay", cut)
		}
	}
}

func TestReplaySnapshotRejects(t *testing.T) {
	r := lifecycle(t)
	data, err := EncodeSnapshot(r.state)
	if err != nil {
		t.Fatal(err)
	}
	good := string(data)
	once := func(old, replacement string) string {
		t.Helper()
		if n := strings.Count(good, old); n != 1 {
			t.Fatalf("%q occurs %d times in the snapshot, want once", old, n)
		}
		return strings.Replace(good, old, replacement, 1)
	}
	lastSeq := regexp.MustCompile(`\n\s*"lastSeq": \d+,`)

	tests := []struct {
		name string
		in   string
		want []error
	}{
		{"an unknown member of the snapshot", once("{\n  \"schemaVersion\"", "{\n  \"extra\": 1,\n  \"schemaVersion\""), []error{strictjson.ErrUnknownMember}},
		{"an unknown member of the state", once("\"lastSeq\"", "\"bogus\": true,\n    \"lastSeq\""), []error{strictjson.ErrUnknownMember}},
		{"an unknown member of the Run", once("\"suspensions\"", "\"bogus\": true,\n      \"suspensions\""), []error{strictjson.ErrUnknownMember}},
		{"a null", once("\"suspensions\": []", "\"suspensions\": null"), []error{strictjson.ErrNull}},
		{"a null state", once("\"state\": {", "\"state\": null, \"x\": {"), []error{strictjson.ErrNull}},
		{"a missing member", lastSeq.ReplaceAllString(good, ""), []error{strictjson.ErrMissingMember}},
		{"a missing state", once("\"state\": {", "\"stats\": {"), nil},
		{"a missing schema version", once("\"schemaVersion\": 1,", ""), []error{strictjson.ErrMissingMember}},
		{"a null schema version", once("\"schemaVersion\": 1", "\"schemaVersion\": null"), []error{strictjson.ErrNull}},
		{"a schema version of the wrong type", once("\"schemaVersion\": 1", "\"schemaVersion\": \"1\""), []error{strictjson.ErrInvalidValue}},
		{"an unknown state", once("\"state\": \"Completed\"", "\"state\": \"Nonsense\""), []error{strictjson.ErrInvalidValue, domain.ErrInvalidState}},
		{"a duplicate member", once("\"lastSeq\"", "\"lastSeq\": 1, \"lastSeq\""), []error{strictjson.ErrDuplicateMember}},
		{"trailing data", good + "{}", []error{strictjson.ErrTrailingData}},
		{"an invalid Run ID", once("\"runId\": \"run-0123456789abcdef\"", "\"runId\": \"run-1\""), []error{strictjson.ErrInvalidValue, domain.ErrInvalidID}},
		{"empty input", "", []error{strictjson.ErrInvalidJSON}},
		{"not JSON", "state", []error{strictjson.ErrInvalidJSON}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DecodeSnapshot([]byte(tt.in))
			if err == nil {
				t.Fatal("DecodeSnapshot accepted it")
			}
			if errors.Is(err, ErrSnapshotVersion) {
				t.Errorf("%v matches ErrSnapshotVersion", err)
			}
			for _, w := range tt.want {
				if !errors.Is(err, w) {
					t.Errorf("%v does not match %v", err, w)
				}
			}
			if !Equal(got, State{}) {
				t.Errorf("DecodeSnapshot returned a state with its error")
			}
		})
	}

	t.Run("another schema version", func(t *testing.T) {
		for _, in := range []string{
			once("\"schemaVersion\": 1", "\"schemaVersion\": 2"),
			once("\"schemaVersion\": 1", "\"schemaVersion\": 0"),
			`{"schemaVersion": 2, "state": 5}`,                   // the rest does not follow this version's contract
			`{"schemaVersion": 2, "state": {"newMember": null}}`, // not even strictly valid for this version
			`{"state": {}, "schemaVersion": 99}`,
		} {
			got, err := DecodeSnapshot([]byte(in))
			if !errors.Is(err, ErrSnapshotVersion) {
				t.Errorf("%.60s: error %v does not match ErrSnapshotVersion", in, err)
			}
			if !Equal(got, State{}) {
				t.Errorf("DecodeSnapshot returned a state with its error")
			}
		}
	})
}

func TestReplayContractTypes(t *testing.T) {
	for _, typ := range []reflect.Type{reflect.TypeFor[Snapshot](), reflect.TypeFor[State]()} {
		if err := strictjson.CheckContractType(typ); err != nil {
			t.Errorf("%s: %v", typ, err)
		}
	}
}

// The projected state of a Run is the same whatever route led to it: the
// snapshot, the replay and a replay of the encoded journal agree.
func TestReplayThroughTheJournalEncoding(t *testing.T) {
	r := lifecycle(t)
	decoded := make([]domain.Event, len(r.events))
	for i, e := range r.events {
		rec, err := domain.EncodeEvent(e)
		if err != nil {
			t.Fatalf("EncodeEvent(%s): %v", e.Type, err)
		}
		if decoded[i], err = domain.DecodeEvent(rec); err != nil {
			t.Fatalf("DecodeEvent(%s): %v", e.Type, err)
		}
	}
	got, err := Replay(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if !Equal(got, r.state) {
		t.Errorf("the replay of the decoded journal differs from the incremental state")
	}
}
