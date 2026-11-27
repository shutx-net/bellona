package projection

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/shutx-net/bellona/internal/domain"
	"github.com/shutx-net/bellona/internal/fsm"
)

// A deterministic randomized test of the reducer (T-01, T-04, T-13, R-FSM-02,
// R-PER-06). A generator proposes events
// from the current state, some well formed for it and some not; every proposal
// goes through Apply. For an accepted event the test checks, against the
// oracle that is built from fsm.Edges, that each machine instance moved only
// along an edge for that event; for a rejected one it checks that nothing
// changed. Whatever was accepted is replayed at the end of each episode and must
// give the same state, also after a round trip through the journal encoding.

const (
	propertySeeds    = 5
	propertySteps    = 4100 // per seed; more than 20000 events in all
	episodeMaxSteps  = 300
	episodeMaxEvents = 160
)

func TestPropertyRandomEvents(t *testing.T) {
	o := newOracle()
	totalSteps, totalAccepted, episodes := 0, 0, 0
	acceptedTypes := make(map[domain.EventType]int)
	edgesSeen := make(map[oracleKey]bool)
	for seed := uint64(1); seed <= propertySeeds; seed++ {
		w := &walker{
			t:             t,
			o:             o,
			g:             &gen{rng: rand.New(rand.NewPCG(seed, 0x5eed0000+seed)), o: o, t: t, chaos: 0.08},
			acceptedTypes: acceptedTypes,
			edgesSeen:     edgesSeen,
		}
		for w.steps < propertySteps {
			w.startEpisode()
			episodes++
			for w.steps < propertySteps && w.episodeSteps < episodeMaxSteps && len(w.events) < episodeMaxEvents &&
				!w.state.Run.State.Terminal() {
				w.step()
			}
			w.finishEpisode()
		}
		if share := float64(w.accepted) / float64(w.steps); share < 0.10 {
			t.Errorf("seed %d: only %d of %d events were accepted (%.1f%%); the generator is too wild", seed, w.accepted, w.steps, 100*share)
		}
		t.Logf("seed %d: %d events, %d accepted, %d rejected", seed, w.steps, w.accepted, w.steps-w.accepted)
		totalSteps += w.steps
		totalAccepted += w.accepted
	}
	if totalSteps <= 20000 {
		t.Errorf("%d events, want more than 20000", totalSteps)
	}
	if share := float64(totalAccepted) / float64(totalSteps); share < 0.10 {
		t.Errorf("only %.1f%% of the events were accepted", 100*share)
	}

	// The test is not vacuous: every event type that can follow the prefix was
	// accepted at least once, and a large part of the edges was walked.
	for _, typ := range domain.EventTypes() {
		if typ == domain.EventRunCreated || typ == domain.EventManifestFrozen {
			continue // only ever the first and second event, which the prefix holds
		}
		if acceptedTypes[typ] == 0 {
			t.Errorf("no %s event was accepted in %d steps", typ, totalSteps)
		}
	}
	walked := 0
	var unwalked []oracleKey
	for k := range o.fixed {
		if edgesSeen[k] {
			walked++
		} else {
			unwalked = append(unwalked, k)
		}
	}
	slices.SortFunc(unwalked, func(a, b oracleKey) int { return cmpKeys(a, b) })
	if share := float64(walked) / float64(len(o.fixed)); share < 0.85 {
		t.Errorf("only %d of %d (event, from, to) edges were walked; missing %v", walked, len(o.fixed), unwalked)
	}
	t.Logf("edges not walked by the random events (they are applied by the edge tests): %v", unwalked)
	t.Logf("%d steps in %d episodes, %d accepted (%.0f%%); %d of %d (event, from, to) edges walked",
		totalSteps, episodes, totalAccepted, 100*float64(totalAccepted)/float64(totalSteps), walked, len(o.fixed))
}

func cmpKeys(a, b oracleKey) int {
	return strings.Compare(fmt.Sprint(a), fmt.Sprint(b))
}

// ---------------------------------------------------------------------------
// The walk
// ---------------------------------------------------------------------------

type walker struct {
	t *testing.T
	o *oracle
	g *gen

	state  State
	events []domain.Event

	steps, episodeSteps, accepted int
	acceptedTypes                 map[domain.EventType]int
	edgesSeen                     map[oracleKey]bool
}

// episodeStarts are the states that an episode starts in. Most episodes start
// from the scripted prefix, a Run in Executing with revision 1 and the phases
// phase-01 and phase-02; the others from an earlier point of the lifecycle, so
// that the events of the first states are walked as well.
var episodeStarts = []domain.RunState{
	domain.RunStateCreated, domain.RunStateStrategySelection, domain.RunStatePlanning,
	domain.RunStatePlanValidation, domain.RunStateWaitingApproval,
	domain.RunStateExecuting, domain.RunStateExecuting, domain.RunStateExecuting, domain.RunStateExecuting,
	domain.RunStateExecuting, domain.RunStateExecuting, domain.RunStateExecuting, domain.RunStateExecuting,
}

func (w *walker) startEpisode() {
	w.t.Helper()
	r := reachRun(w.t, pick(w.g, episodeStarts))
	w.state, w.events, w.episodeSteps = r.state, r.events, 0
	w.g.counter += r.attempts // keep generated IDs away from the prefix's
}

func (w *walker) step() {
	t := w.t
	t.Helper()
	w.steps++
	w.episodeSteps++

	e := nextEvent(w.state, w.g.event(w.state))
	w.g.damageEnvelope(&e, w.state)

	before := w.state.Clone()
	got, err := Apply(w.state, e)
	// The argument is never modified, whether the event is accepted or not.
	// The comparison is reflect.DeepEqual, which is sound here because before
	// is a Clone of the argument (TestCloneSharesNothing: Clone keeps nil and
	// empty slices as they are) and it is much cheaper than encoding the State
	// twice per step. Every 16th step also compares the canonical encodings.
	if !reflect.DeepEqual(before, w.state) || (w.steps%16 == 0 && !Equal(before, w.state)) {
		t.Fatalf("step %d: Apply(%s) modified its argument:\nbefore %s\nafter  %s", w.steps, e.Type, show(before), show(w.state))
	}
	if err != nil {
		var ae *ApplyError
		if !errors.As(err, &ae) || ae.Seq != e.Seq || ae.Type != e.Type {
			t.Fatalf("step %d: the error of %s is %#v, want an *ApplyError for it", w.steps, e.Type, err)
		}
		inv, ill := errors.Is(err, ErrInvalidEvent), errors.Is(err, fsm.ErrIllegalTransition)
		if inv == ill {
			t.Fatalf("step %d: %s: the error must match exactly one of the two classes (invalid %t, illegal %t): %v", w.steps, e.Type, inv, ill, err)
		}
		if !reflect.DeepEqual(before, got) || (w.steps%16 == 0 && !Equal(before, got)) {
			t.Fatalf("step %d: a rejected %s returned a different state", w.steps, e.Type)
		}
		return
	}

	w.checkAccepted(before, got, e)
	w.accepted++
	w.acceptedTypes[e.Type]++
	w.events = append(w.events, e)
	w.state = got

	// Now and then continue from a decoded snapshot, as the engine does after a
	// restart: the decoded state must behave like the live one.
	if w.g.rng.IntN(150) == 0 {
		data, err := EncodeSnapshot(w.state)
		if err != nil {
			t.Fatalf("step %d: EncodeSnapshot: %v", w.steps, err)
		}
		dec, err := DecodeSnapshot(data)
		if err != nil {
			t.Fatalf("step %d: DecodeSnapshot: %v", w.steps, err)
		}
		if !Equal(dec, w.state) {
			t.Fatalf("step %d: the decoded snapshot differs from the state", w.steps)
		}
		w.state = dec
	}
}

// finishEpisode replays what was accepted, directly and through the journal
// encoding, and compares with the state that the walk reached.
func (w *walker) finishEpisode() {
	t := w.t
	t.Helper()
	replayed, err := Replay(w.events)
	if err != nil {
		t.Fatalf("Replay of the %d accepted events failed: %v", len(w.events), err)
	}
	if !Equal(replayed, w.state) {
		t.Fatalf("Replay of the accepted events differs from the state of the walk:\nreplay %s\nwalk   %s", canon(t, replayed), canon(t, w.state))
	}
	decoded := make([]domain.Event, len(w.events))
	for i, e := range w.events {
		rec, err := domain.EncodeEvent(e)
		if err != nil {
			t.Fatalf("EncodeEvent(%s, seq %d): %v", e.Type, e.Seq, err)
		}
		if decoded[i], err = domain.DecodeEvent(rec); err != nil {
			t.Fatalf("DecodeEvent(%s): %v\n%s", e.Type, err, rec)
		}
	}
	fromJournal, err := Replay(decoded)
	if err != nil {
		t.Fatalf("Replay of the decoded journal failed: %v", err)
	}
	if !Equal(fromJournal, w.state) {
		t.Fatalf("Replay of the decoded journal differs from the state of the walk")
	}
}

// phaseKey identifies a Phase across revisions.
type phaseKey struct {
	revision int
	id       domain.PhaseID
}

// checkAccepted verifies everything that an accepted event may and may not
// change. The expectation comes from the oracle, not from the check functions
// that Apply uses.
func (w *walker) checkAccepted(before, after State, e domain.Event) {
	t, o := w.t, w.o
	t.Helper()
	fail := func(format string, args ...any) {
		t.Helper()
		t.Fatalf("step %d, %s at seq %d: %s\nbefore %s\nafter  %s", w.steps, e.Type, e.Seq, fmt.Sprintf(format, args...), show(before), show(after))
	}
	typ := e.Type

	// Sequence and time. Nothing but the event time enters the state.
	if after.LastSeq != before.LastSeq+1 || after.LastSeq != e.Seq || after.RunID != before.RunID {
		fail("LastSeq %d (was %d), RunID %s", after.LastSeq, before.LastSeq, after.RunID)
	}
	if !after.Run.UpdatedAt.Equal(e.Time) || !after.Run.CreatedAt.Equal(before.Run.CreatedAt) {
		fail("Run.UpdatedAt %s, Run.CreatedAt %s", after.Run.UpdatedAt, after.Run.CreatedAt)
	}
	// The Run machine.
	from, to := before.Run.State, after.Run.State
	resume := ""
	if n := len(before.Run.Suspensions); n > 0 {
		resume = string(before.Run.Suspensions[n-1].ResumeState)
	}
	if o.events[fsm.MachineRun][typ] {
		if !o.allowed(fsm.MachineRun, typ, string(from), string(to), resume) {
			fail("the Run moved from %s to %s, which no edge of %s allows (resume state %q)", from, to, typ, resume)
		}
		w.edgesSeen[oracleKey{fsm.MachineRun, typ, string(from), string(to)}] = true
	} else if from != to {
		fail("the Run moved from %s to %s, but %s is no Run event", from, to, typ)
	}
	checkStack(fail, typ, before.Run.Suspensions, after.Run.Suspensions)
	runCounters(fail, typ, before, after)

	// Limits and amendments change only through PolicyAmended.
	if typ == domain.EventPolicyAmended {
		if len(after.Amendments) != len(before.Amendments)+1 || after.Limits == before.Limits {
			fail("%d amendments (was %d), limits %+v (were %+v)", len(after.Amendments), len(before.Amendments), after.Limits, before.Limits)
		}
		if from != domain.RunStateBlocked {
			fail("a policy amendment was applied in %s", from)
		}
	} else if len(after.Amendments) != len(before.Amendments) || after.Limits != before.Limits {
		fail("limits or amendments changed")
	}

	// Artifacts: the old list is a prefix, the new entries are the event's
	// references, and paths are unique.
	if len(after.Artifacts) < len(before.Artifacts) || !slices.Equal(before.Artifacts, after.Artifacts[:len(before.Artifacts)]) {
		fail("the artifact list is not a prefix of the new one")
	}
	refs := e.Payload.ArtifactRefs()
	for _, r := range after.Artifacts[len(before.Artifacts):] {
		if !slices.Contains(refs, r) {
			fail("artifact %s was registered but the event does not refer to it", r.Path)
		}
	}
	paths := make(map[string]bool)
	for _, r := range after.Artifacts {
		if paths[r.Path] {
			fail("artifact path %s is listed twice", r.Path)
		}
		paths[r.Path] = true
	}
	for _, r := range refs {
		if !slices.Contains(after.Artifacts, r) {
			fail("the reference %s of the event is not registered", r.Path)
		}
	}

	// Times that other parts of the state record come from the event as well.
	if n := len(after.Run.Suspensions); typ == domain.EventRunSuspended && !after.Run.Suspensions[n-1].Since.Equal(e.Time) {
		fail("the pushed suspension has the time %s", after.Run.Suspensions[n-1].Since)
	}
	if typ == domain.EventPolicyAmended && !after.Amendments[len(after.Amendments)-1].Time.Equal(e.Time) {
		fail("the amendment has the time %s", after.Amendments[len(after.Amendments)-1].Time)
	}
	if typ == domain.EventPlanAccepted && !after.Revisions[len(after.Revisions)-1].AcceptedAt.Equal(e.Time) {
		fail("the revision was accepted at %s", after.Revisions[len(after.Revisions)-1].AcceptedAt)
	}

	// Revisions and the Phase machine.
	if typ == domain.EventPlanAccepted {
		if len(after.Revisions) != len(before.Revisions)+1 {
			fail("%d revisions (was %d)", len(after.Revisions), len(before.Revisions))
		}
	} else if len(after.Revisions) != len(before.Revisions) {
		fail("the number of revisions changed")
	}
	for i, r := range after.Revisions {
		if r.Number != i+1 {
			fail("revision %d has the number %d", i+1, r.Number)
		}
	}
	if after.Run.PlanRevision != len(after.Revisions) {
		fail("PlanRevision %d with %d revisions", after.Run.PlanRevision, len(after.Revisions))
	}
	old := make(map[phaseKey]Phase)
	for _, r := range before.Revisions {
		for _, ph := range r.Phases {
			old[phaseKey{r.Number, ph.ID}] = ph
		}
	}
	var changed []phaseKey
	created := 0
	for _, r := range after.Revisions {
		for _, ph := range r.Phases {
			k := phaseKey{r.Number, ph.ID}
			prev, ok := old[k]
			switch {
			case !ok:
				created++
				if ph.State != domain.PhaseStatePending || !ph.UpdatedAt.Equal(e.Time) || len(ph.Attempts) != 0 || ph.ResumeState != "" {
					fail("the new Phase %v is %+v", k, ph)
				}
			case phaseChanged(t, prev, ph):
				changed = append(changed, k)
				if prev.State != ph.State {
					if !o.allowed(fsm.MachinePhase, typ, string(prev.State), string(ph.State), string(prev.ResumeState)) {
						fail("Phase %v moved from %s to %s, which no edge of %s allows (resume state %q)", k, prev.State, ph.State, typ, prev.ResumeState)
					}
					w.edgesSeen[oracleKey{fsm.MachinePhase, typ, string(prev.State), string(ph.State)}] = true
				} else if typ != domain.EventAttemptScheduled {
					if !o.allowed(fsm.MachinePhase, typ, string(prev.State), string(ph.State), string(prev.ResumeState)) {
						fail("Phase %v changed in %s without a transition that %s allows", k, prev.State, typ)
					}
					w.edgesSeen[oracleKey{fsm.MachinePhase, typ, string(prev.State), string(ph.State)}] = true
				}
				if !ph.UpdatedAt.Equal(e.Time) {
					fail("Phase %v changed but UpdatedAt is %s", k, ph.UpdatedAt)
				}
				if ph.State != domain.PhaseStateWaitingForUser && ph.ResumeState != "" {
					fail("Phase %v in %s has the resume state %s", k, ph.State, ph.ResumeState)
				}
			}
		}
	}
	if len(changed) > 1 {
		fail("one event changed %d Phases: %v", len(changed), changed)
	}
	if (created > 0) != (typ == domain.EventPlanAccepted) {
		fail("%d Phases were created by %s", created, typ)
	}
	wantChanged := 0
	switch p := e.Payload.(type) {
	case domain.AttemptScheduled:
		if p.PhaseID != "" {
			wantChanged = 1
		}
	default:
		if o.events[fsm.MachinePhase][typ] {
			wantChanged = 1
		}
	}
	if len(changed) != wantChanged {
		fail("%d Phases changed, want %d", len(changed), wantChanged)
	}

	// The Attempt machine.
	oldAttempts := make(map[domain.AttemptID]Attempt, len(before.Attempts))
	for _, a := range before.Attempts {
		oldAttempts[a.ID] = a
	}
	wantAttempts := len(before.Attempts)
	if typ == domain.EventAttemptScheduled {
		wantAttempts++
	}
	if len(after.Attempts) != wantAttempts {
		fail("%d Attempts, want %d", len(after.Attempts), wantAttempts)
	}
	changedAttempts := 0
	for i, a := range after.Attempts {
		prev, ok := oldAttempts[a.ID]
		if !ok {
			p, isSched := e.Payload.(domain.AttemptScheduled)
			if !isSched || i != len(after.Attempts)-1 || a.ID != p.AttemptID || a.State != domain.AttemptStateScheduled ||
				!a.ScheduledAt.Equal(e.Time) || !a.UpdatedAt.Equal(e.Time) || a.Role != p.Role {
				fail("the new Attempt %+v does not come from an AttemptScheduled event", a)
			}
			continue
		}
		if !attemptChanged(t, prev, a) {
			continue
		}
		changedAttempts++
		if prev.State == a.State || !o.allowed(fsm.MachineAttempt, typ, string(prev.State), string(a.State), "") {
			fail("Attempt %s moved from %s to %s, which no edge of %s allows", a.ID, prev.State, a.State, typ)
		}
		w.edgesSeen[oracleKey{fsm.MachineAttempt, typ, string(prev.State), string(a.State)}] = true
		if !a.UpdatedAt.Equal(e.Time) {
			fail("Attempt %s changed but UpdatedAt is %s", a.ID, a.UpdatedAt)
		}
		// Every time that appears in a changed Attempt is the event time.
		if !a.ScheduledAt.Equal(prev.ScheduledAt) ||
			(!a.StartedAt.Equal(prev.StartedAt) && !a.StartedAt.Equal(e.Time)) ||
			(!a.EndedAt.Equal(prev.EndedAt) && !a.EndedAt.Equal(e.Time)) {
			fail("Attempt %s has the times %s, %s, %s (was %s, %s, %s)", a.ID, a.ScheduledAt, a.StartedAt, a.EndedAt,
				prev.ScheduledAt, prev.StartedAt, prev.EndedAt)
		}
	}
	if want := b2i(o.events[fsm.MachineAttempt][typ]); changedAttempts != want {
		fail("%d Attempts changed, want %d", changedAttempts, want)
	}
}

// show returns the canonical encoding of a state for a failure message.
func show(s State) string {
	b, err := Canonical(s)
	if err != nil {
		return fmt.Sprintf("(cannot be encoded: %v) %+v", err, s)
	}
	return string(b)
}

// phaseChanged reports whether two versions of a Phase differ. The cheap
// comparison comes first; a difference is confirmed through the encoding.
func phaseChanged(tb testing.TB, a, b Phase) bool {
	tb.Helper()
	return !reflect.DeepEqual(a, b) && encoded(tb, a) != encoded(tb, b)
}

// attemptChanged is phaseChanged for an Attempt.
func attemptChanged(tb testing.TB, a, b Attempt) bool {
	tb.Helper()
	return a != b && encoded(tb, a) != encoded(tb, b)
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// checkStack verifies how the suspension stack may change.
func checkStack(fail func(string, ...any), typ domain.EventType, before, after []Suspension) {
	switch typ {
	case domain.EventRunSuspended:
		if len(after) != len(before)+1 || !slices.Equal(before, after[:len(before)]) {
			fail("RunSuspended did not push exactly one suspension")
		}
	case domain.EventRunResumed:
		if len(after) != len(before)-1 || !slices.Equal(before[:len(after)], after) {
			fail("RunResumed did not pop exactly one suspension")
		}
	case domain.EventRunCancelled:
		if len(after) != 0 {
			fail("RunCancelled left %d suspensions", len(after))
		}
	default:
		if !slices.Equal(before, after) {
			fail("the suspensions changed")
		}
	}
}

// runCounters verifies the counters and plans of the Run.
func runCounters(fail func(string, ...any), typ domain.EventType, before, after State) {
	b, a := before.Run, after.Run
	step := func(name string, was, now int, event domain.EventType) {
		want := was
		if typ == event {
			want++
		}
		if now != want {
			fail("%s is %d, want %d", name, now, want)
		}
	}
	step("ReplanCount", b.ReplanCount, a.ReplanCount, domain.EventReplanningStarted)
	step("ChangeRequestCount", b.ChangeRequestCount, a.ChangeRequestCount, domain.EventPlanChangesRequested)
	step("PlanRevision", b.PlanRevision, a.PlanRevision, domain.EventPlanAccepted)
	switch typ {
	case domain.EventPlanGenerated:
		if a.PendingPlan.IsZero() {
			fail("PlanGenerated left no pending plan")
		}
	case domain.EventPlanValidationFailed, domain.EventPlanAccepted, domain.EventPlanChangesRequested:
		if !a.PendingPlan.IsZero() {
			fail("the pending plan was not cleared")
		}
	default:
		if a.PendingPlan != b.PendingPlan {
			fail("the pending plan changed")
		}
	}
	if (typ == domain.EventRunCancelled) != (a.CancelReason != "") {
		fail("CancelReason is %q", a.CancelReason)
	}
	if b.Task != a.Task || b.Manifest != a.Manifest || b.Policy != a.Policy {
		fail("a frozen artifact reference changed")
	}
}

// ---------------------------------------------------------------------------
// The generator
// ---------------------------------------------------------------------------

// gen proposes events. Roughly half of the proposals name an event type that
// has an edge from some entity of the current state and fill the payload from
// the state; the others are drawn from the whole catalogue. In both cases a
// field is replaced by a random value with probability chaos, so that both
// accepted and rejected events are frequent.
type gen struct {
	rng     *rand.Rand
	o       *oracle
	t       testing.TB
	chaos   float64
	counter int
	st      State
}

func (g *gen) p(prob float64) bool { return g.rng.Float64() < prob }
func (g *gen) n(k int) int         { return g.rng.IntN(k) }

func pick[T any](g *gen, xs []T) T { return xs[g.rng.IntN(len(xs))] }

func (g *gen) fresh() int { g.counter++; return 1000 + g.counter }

func (g *gen) session() domain.SessionID {
	return pick(g, []domain.SessionID{session1, "s-two", "s-three"})
}

// ref returns a reference to the path; with probability chaos its content
// differs from the usual one, which makes it a conflicting reference.
func (g *gen) ref(path string) domain.ArtifactRef {
	if g.p(g.chaos) {
		return refWith(g.t, path, "other content of "+path)
	}
	return ref(g.t, path)
}

// event proposes the payload of the next event.
func (g *gen) event(st State) domain.Payload {
	g.st = st
	for {
		typ := g.pickType()
		// Ending the Run too often would make the episodes short.
		if (typ == domain.EventRunCancelled || typ == domain.EventRunCompleted) && !g.p(0.12) {
			continue
		}
		return g.fill(typ)
	}
}

// damageEnvelope sometimes spoils the envelope of a proposal.
func (g *gen) damageEnvelope(e *domain.Event, st State) {
	if !g.p(0.02) {
		return
	}
	switch g.n(3) {
	case 0:
		e.Seq += uint64(1 + g.n(3))
	case 1:
		e.RunID = "run-fedcba9876543210"
	case 2:
		e.Seq = st.LastSeq
	}
}

// diverting lists the event types that lead away from the happy path of the
// lifecycle: failures, suspensions, cancellations and supersession.
var diverting = []domain.EventType{
	domain.EventPhaseRecovering, domain.EventPhaseSuspended, domain.EventPhaseSuperseded, domain.EventPhaseFailed,
	domain.EventRunSuspended, domain.EventRunCancelled, domain.EventReplanningStarted, domain.EventPolicyAmended,
	domain.EventAttemptCancelled, domain.EventAttemptAbandoned, domain.EventAttemptOutcomeUnknown,
	domain.EventAttemptAwaitingInput, domain.EventPlanValidationFailed, domain.EventPlanChangesRequested,
}

// followUp returns the event type that obviously follows from the state: a
// retry after a failed Evaluator Attempt, or the verification after a finished
// Implementer Attempt. Chains like these are too long to happen by chance.
func (g *gen) followUp() (domain.EventType, bool) {
	failed := func(a Attempt) bool {
		return a.State == domain.AttemptStateFailed || a.State == domain.AttemptStateTimedOut
	}
	if rev, ok := g.st.CurrentRevision(); ok {
		for _, ph := range rev.Phases {
			if len(ph.Attempts) == 0 {
				continue
			}
			last, _ := g.st.Attempt(ph.Attempts[len(ph.Attempts)-1])
			switch {
			case ph.State == domain.PhaseStateEvaluating && last.Role == domain.RoleEvaluator && failed(last):
				return domain.EventPhaseEvaluationRetried, true
			case ph.State == domain.PhaseStateRunning && last.Role == domain.RoleImplementer && last.State == domain.AttemptStateFinished:
				return domain.EventPhaseVerificationStarted, true
			}
		}
	}
	if g.st.Run.State == domain.RunStateFinalEvaluation {
		for i := len(g.st.Attempts) - 1; i >= 0; i-- {
			if a := g.st.Attempts[i]; a.Role == domain.RoleEvaluator && a.PhaseID == "" {
				if failed(a) {
					return domain.EventFinalEvaluationRetried, true
				}
				break
			}
		}
	}
	return "", false
}

func (g *gen) pickType() domain.EventType {
	if g.p(0.3) {
		if typ, ok := g.followUp(); ok {
			return typ
		}
	}
	if g.p(0.6) {
		feasible := g.feasible()
		if g.p(0.6) {
			// Mostly follow the happy path, so that the long chains of events
			// (a Phase from Ready to Accepted, the final evaluation) happen.
			feasible = slices.DeleteFunc(feasible, func(t domain.EventType) bool { return slices.Contains(diverting, t) })
		}
		if len(feasible) > 0 {
			return pick(g, feasible)
		}
	}
	return pick(g, domain.EventTypes())
}

// feasible lists the event types that have an edge from the state of the Run,
// of some Phase of the current revision or of some Attempt, plus the two events
// that no table commits and that can follow.
func (g *gen) feasible() []domain.EventType {
	var out []domain.EventType
	add := func(types []domain.EventType) {
		for _, t := range types {
			if !slices.Contains(out, t) {
				out = append(out, t)
			}
		}
	}
	add(g.o.from[fsm.MachineRun][string(g.st.Run.State)])
	if rev, ok := g.st.CurrentRevision(); ok {
		for _, ph := range rev.Phases {
			add(g.o.from[fsm.MachinePhase][string(ph.State)])
		}
	}
	for _, a := range g.st.Attempts {
		add(g.o.from[fsm.MachineAttempt][string(a.State)])
	}
	add([]domain.EventType{domain.EventAttemptScheduled})
	if g.st.Run.State == domain.RunStateBlocked {
		add([]domain.EventType{domain.EventPolicyAmended})
	}
	return out
}

// attempt picks an Attempt that satisfies pred, or, with probability chaos or
// when there is none, any Attempt or an unknown ID.
func (g *gen) attempt(pred func(Attempt) bool) domain.AttemptID {
	var ids []domain.AttemptID
	for _, a := range g.st.Attempts {
		if pred == nil || pred(a) {
			ids = append(ids, a.ID)
		}
	}
	switch {
	case len(ids) > 0 && !g.p(g.chaos):
		if g.p(0.6) {
			return ids[len(ids)-1] // mostly the latest one, so that an Attempt makes progress
		}
		return pick(g, ids)
	case len(g.st.Attempts) > 0 && g.p(0.5):
		return pick(g, g.st.Attempts).ID
	}
	return attemptID(g.fresh())
}

// attemptFor picks an Attempt whose state has an edge for the event type.
func (g *gen) attemptFor(typ domain.EventType) domain.AttemptID {
	return g.attempt(func(a Attempt) bool { return slices.Contains(g.o.from[fsm.MachineAttempt][string(a.State)], typ) })
}

// phase picks a Phase of the current revision that satisfies pred, or, with
// probability chaos or when there is none, any Phase or an unknown one.
func (g *gen) phase(pred func(Phase) bool) (int, domain.PhaseID) {
	type ref struct {
		revision int
		id       domain.PhaseID
	}
	var matching, all []ref
	for _, r := range g.st.Revisions {
		for _, ph := range r.Phases {
			all = append(all, ref{r.Number, ph.ID})
			if r.Number == g.st.Run.PlanRevision && (pred == nil || pred(ph)) {
				matching = append(matching, ref{r.Number, ph.ID})
			}
		}
	}
	var chosen ref
	switch {
	case len(matching) > 0 && !g.p(g.chaos):
		chosen = pick(g, matching)
	case len(all) > 0 && g.p(0.5):
		chosen = pick(g, all)
	default:
		chosen = ref{max(g.st.Run.PlanRevision, 1) + g.n(2), "phase-77"}
	}
	return chosen.revision, chosen.id
}

// phaseFor picks a Phase whose state has an edge for the event type.
func (g *gen) phaseFor(typ domain.EventType) (int, domain.PhaseID) {
	return g.phase(func(ph Phase) bool { return slices.Contains(g.o.from[fsm.MachinePhase][string(ph.State)], typ) })
}

// phaseData returns the Phase, or the zero Phase when it does not exist.
func (g *gen) phaseData(revision int, id domain.PhaseID) Phase {
	ph, _ := g.st.Phase(revision, id)
	return ph
}

func (g *gen) limitValue(name domain.LimitName) string {
	switch name {
	case domain.LimitTimeout:
		return pick(g, []string{"unlimited", "30m0s", "1h0m0s", "1h30m0s", "2h30m0s"})
	case domain.LimitMaxAgentSteps:
		return pick(g, []string{"unlimited", "10", "50"})
	case domain.LimitMaxTurnCostUSD:
		return pick(g, []string{"unlimited", "0.5", "1.5", "2.25"})
	}
	return strconv.Itoa(g.n(6))
}

func (g *gen) policyAmended() domain.Payload {
	names := domain.LimitNames()
	limits := g.st.Limits
	var changes []domain.LimitChange
	for _, i := range g.rng.Perm(len(names))[:1+g.n(3)] {
		name := names[i]
		prev := limits.Value(name)
		if g.p(g.chaos) {
			prev = g.limitValue(name)
		}
		var next string
		for range 20 {
			cand := g.limitValue(name)
			if cand != prev && cand != limits.Value(name) {
				next = cand
				break
			}
		}
		if next == "" {
			continue
		}
		if updated, err := limits.With(name, next); err == nil {
			limits = updated
			changes = append(changes, domain.LimitChange{Limit: name, Previous: prev, New: next})
		}
	}
	if len(changes) == 0 {
		changes = []domain.LimitChange{{Limit: domain.LimitMaxReplans, Previous: g.st.Limits.Value(domain.LimitMaxReplans), New: "9"}}
	}
	return domain.PolicyAmended{Changes: changes}
}

func (g *gen) attemptScheduled() domain.Payload {
	st := g.st
	p := domain.AttemptScheduled{AttemptID: attemptID(g.fresh()), Role: pick(g, domain.Roles())}
	if g.p(0.6) {
		// A role that the state can use: an Implementer for a Ready Phase, an
		// Evaluator for an Evaluating Phase or the final evaluation, a Planner
		// while Planning.
		var roles []domain.Role
		if rev, ok := st.CurrentRevision(); ok {
			for _, ph := range rev.Phases {
				switch ph.State {
				case domain.PhaseStateReady:
					roles = append(roles, domain.RoleImplementer)
				case domain.PhaseStateEvaluating:
					roles = append(roles, domain.RoleEvaluator)
				}
			}
		}
		switch st.Run.State {
		case domain.RunStateFinalEvaluation:
			roles = append(roles, domain.RoleEvaluator)
		case domain.RunStatePlanning:
			roles = append(roles, domain.RolePlanner)
		}
		if len(roles) > 0 {
			p.Role = pick(g, roles)
		}
	}
	if g.p(g.chaos) && len(st.Attempts) > 0 {
		p.AttemptID = pick(g, st.Attempts).ID
	}
	switch p.Role {
	case domain.RoleImplementer:
		p.Revision, p.PhaseID = g.phase(func(ph Phase) bool { return ph.State == domain.PhaseStateReady })
	case domain.RoleEvaluator:
		if st.Run.State != domain.RunStateFinalEvaluation && !g.p(g.chaos) || g.p(0.3) {
			p.Revision, p.PhaseID = g.phase(func(ph Phase) bool { return ph.State == domain.PhaseStateEvaluating })
		}
	default:
		if st.Run.PlanRevision > 0 && g.p(0.5) {
			p.Revision = st.Run.PlanRevision
		}
	}
	if g.p(0.5) {
		p.SessionID = g.session()
	}
	return p
}

func (g *gen) planAccepted() domain.Payload {
	rev := g.st.Run.PlanRevision + 1
	if g.p(g.chaos) {
		rev = 1 + g.n(4)
	}
	phases := make([]domain.PhaseDefinition, 1+g.n(4))
	for i := range phases {
		phases[i] = domain.PhaseDefinition{ID: domain.PhaseID(fmt.Sprintf("phase-%02d", i+1)), DependsOn: []domain.PhaseID{}}
		for j := range i {
			if g.p(0.4) {
				phases[i].DependsOn = append(phases[i].DependsOn, phases[j].ID)
			}
		}
	}
	if len(phases) > 1 && g.p(g.chaos) {
		phases[0].DependsOn = []domain.PhaseID{phases[len(phases)-1].ID} // a cycle
	}
	return domain.PlanAccepted{Revision: rev, Plan: g.ref(domain.PlanPath(rev)), Phases: phases}
}

func (g *gen) runSuspended() domain.Payload {
	st := g.st
	to := pick(g, []domain.RunState{domain.RunStateWaitingForUser, domain.RunStateBlocked, domain.RunStateRecoveryRequired})
	var reasons []domain.SuspendReason
	for _, r := range domain.SuspendReasons() {
		if r.Target() == to {
			reasons = append(reasons, r)
		}
	}
	p := domain.RunSuspended{To: to, Reason: pick(g, reasons), ResumeState: st.Run.State}
	if p.ResumeState == to || p.ResumeState.Terminal() || g.p(g.chaos) {
		for {
			p.ResumeState = pick(g, domain.RunStates())
			if p.ResumeState != to && !p.ResumeState.Terminal() {
				break
			}
		}
	}
	if to == domain.RunStateWaitingForUser {
		p.RequestID = requestID(g.fresh())
	}
	if g.p(0.3) {
		p.Detail = "detail " + strconv.Itoa(g.n(100))
	}
	return p
}

func (g *gen) runResumed() domain.Payload {
	st := g.st
	p := domain.RunResumed{To: domain.RunStateExecuting}
	if n := len(st.Run.Suspensions); n > 0 && !g.p(g.chaos) {
		top := st.Run.Suspensions[n-1]
		p.To = top.ResumeState
		if top.RequestID != "" && g.p(0.5) {
			p.RequestID = top.RequestID
		}
	} else {
		for {
			p.To = pick(g, domain.RunStates())
			if !p.To.Terminal() {
				break
			}
		}
		if g.p(0.2) {
			p.RequestID = requestID(g.fresh())
		}
	}
	return p
}

func (g *gen) phaseSuspended(rev int, id domain.PhaseID) domain.Payload {
	to := pick(g, []domain.PhaseState{domain.PhaseStateWaitingForUser, domain.PhaseStateRecoveryRequired})
	p := domain.PhaseSuspended{Revision: rev, PhaseID: id, To: to, ResumeState: g.phaseData(rev, id).State}
	if p.ResumeState == "" || p.ResumeState == to || g.p(g.chaos) {
		for {
			p.ResumeState = pick(g, domain.PhaseStates())
			if p.ResumeState != to {
				break
			}
		}
	}
	if to == domain.PhaseStateWaitingForUser {
		p.RequestID = requestID(g.fresh())
	}
	return p
}

// fill builds a payload of the given type from the current state.
func (g *gen) fill(typ domain.EventType) domain.Payload {
	st := g.st
	switch typ {
	case domain.EventRunCreated:
		return domain.RunCreated{Task: g.ref(domain.TaskPath)}
	case domain.EventManifestFrozen:
		return domain.ManifestFrozen{Manifest: g.ref(domain.ManifestPath), Policy: g.ref(domain.PolicyPath),
			Limits: domain.Limits{MaxPhaseRetries: g.n(4), MaxReplans: g.n(4)}}
	case domain.EventPolicyAmended:
		return g.policyAmended()
	case domain.EventAttemptScheduled:
		return g.attemptScheduled()

	case domain.EventRunPrepared:
		return domain.RunPrepared{}
	case domain.EventStrategySelected:
		if g.p(0.5) {
			return domain.StrategySelected{Decision: g.ref("attempts/" + string(attemptID(g.fresh())) + "/turns/0001/output.json")}
		}
		return domain.StrategySelected{}
	case domain.EventPlanGenerated:
		id := g.attempt(func(a Attempt) bool { return a.Role == domain.RolePlanner && a.State == domain.AttemptStateFinished })
		out := domain.ArtifactRef{}
		if a, ok := st.Attempt(id); ok {
			out = a.Output
		}
		if out.IsZero() || g.p(g.chaos) {
			out = g.ref(domain.AttemptOutputPath(id, 1))
		}
		return domain.PlanGenerated{AttemptID: id, Artifact: out}
	case domain.EventPlanValidationFailed:
		p := domain.PlanValidationFailed{Summary: "the plan is invalid"}
		if g.p(0.5) {
			p.Report = g.ref(fmt.Sprintf("plans/rev-%04d/validation.json", 1+g.n(3)))
		}
		return p
	case domain.EventPlanApprovalRequested:
		art := st.Run.PendingPlan
		if art.IsZero() || g.p(g.chaos) {
			art = g.ref(fmt.Sprintf("plans/rev-%04d/candidate.json", 1+g.n(3)))
		}
		return domain.PlanApprovalRequested{Artifact: art}
	case domain.EventPlanAccepted:
		return g.planAccepted()
	case domain.EventPlanChangesRequested:
		return domain.PlanChangesRequested{Feedback: g.ref(fmt.Sprintf("plans/rev-%04d/feedback-%d.json", 1+g.n(3), g.n(3)))}
	case domain.EventFinalVerificationStarted:
		return domain.FinalVerificationStarted{}
	case domain.EventReplanningStarted:
		cause := pick(g, domain.ReplanCauses())
		for _, c := range domain.ReplanCauses() {
			if c.Source() == st.Run.State && !g.p(g.chaos) {
				cause = c
			}
		}
		return domain.ReplanningStarted{Cause: cause}
	case domain.EventFinalEvaluationStarted:
		return domain.FinalEvaluationStarted{}
	case domain.EventRunCompleted:
		return domain.RunCompleted{}
	case domain.EventFinalEvaluationRetried:
		return domain.FinalEvaluationRetried{FailedAttemptID: g.attempt(func(a Attempt) bool {
			return a.Role == domain.RoleEvaluator && a.PhaseID == "" && (a.State == domain.AttemptStateFailed || a.State == domain.AttemptStateTimedOut)
		})}
	case domain.EventRunSuspended:
		return g.runSuspended()
	case domain.EventRunResumed:
		return g.runResumed()
	case domain.EventRunCancelled:
		reason := domain.CancelReasonUserRequest
		if st.Run.State == domain.RunStateWaitingApproval {
			reason = domain.CancelReasonPlanRejected
		}
		if g.p(g.chaos) {
			reason = pick(g, domain.CancelReasons())
		}
		return domain.RunCancelled{Reason: reason}

	case domain.EventPhaseReady:
		rev, id := g.phaseFor(typ)
		return domain.PhaseReady{Revision: rev, PhaseID: id}
	case domain.EventPhaseVerificationStarted:
		rev, id := g.phaseFor(typ)
		attempt := g.attempt(func(a Attempt) bool {
			return a.Role == domain.RoleImplementer && a.State == domain.AttemptStateFinished
		})
		if ph := g.phaseData(rev, id); len(ph.Attempts) > 0 && !g.p(g.chaos) {
			attempt = ph.Attempts[len(ph.Attempts)-1]
		}
		return domain.PhaseVerificationStarted{Revision: rev, PhaseID: id, AttemptID: attempt}
	case domain.EventPhaseRecovering:
		rev, id := g.phaseFor(typ)
		cause := pick(g, domain.PhaseRecoveryCauses())
		for _, c := range domain.PhaseRecoveryCauses() {
			if c.Source() == g.phaseData(rev, id).State && g.p(0.6) {
				cause = c
			}
		}
		return domain.PhaseRecovering{Revision: rev, PhaseID: id, Cause: cause}
	case domain.EventPhaseEvaluationStarted:
		rev, id := g.phaseFor(typ)
		return domain.PhaseEvaluationStarted{Revision: rev, PhaseID: id}
	case domain.EventPhaseIntegrationQueued:
		rev, id := g.phaseFor(typ)
		return domain.PhaseIntegrationQueued{Revision: rev, PhaseID: id}
	case domain.EventPhaseEvaluationRetried:
		rev, id := g.phaseFor(typ)
		return domain.PhaseEvaluationRetried{Revision: rev, PhaseID: id, FailedAttemptID: g.attempt(func(a Attempt) bool {
			return a.Role == domain.RoleEvaluator && a.PhaseID == id && (a.State == domain.AttemptStateFailed || a.State == domain.AttemptStateTimedOut)
		})}
	case domain.EventPhaseIntegrationStarted:
		rev, id := g.phaseFor(typ)
		return domain.PhaseIntegrationStarted{Revision: rev, PhaseID: id}
	case domain.EventPhaseAccepted:
		rev, id := g.phaseFor(typ)
		return domain.PhaseAccepted{Revision: rev, PhaseID: id, AttemptID: g.attempt(func(a Attempt) bool {
			return a.Role == domain.RoleImplementer && a.PhaseID == id && a.State == domain.AttemptStateFinished
		})}
	case domain.EventPhaseRetryAuthorized:
		rev, id := g.phaseFor(typ)
		return domain.PhaseRetryAuthorized{Revision: rev, PhaseID: id}
	case domain.EventPhaseFailed:
		rev, id := g.phaseFor(typ)
		return domain.PhaseFailed{Revision: rev, PhaseID: id}
	case domain.EventPhaseSuspended:
		rev, id := g.phaseFor(typ)
		return g.phaseSuspended(rev, id)
	case domain.EventPhaseResumed:
		rev, id := g.phaseFor(typ)
		to := g.phaseData(rev, id).ResumeState
		if to == "" || g.p(g.chaos) {
			to = pick(g, domain.PhaseStates())
		}
		return domain.PhaseResumed{Revision: rev, PhaseID: id, To: to}
	case domain.EventPhaseReconciled:
		rev, id := g.phaseFor(typ)
		to := pick(g, fsm.ReconciliationTargets())
		if g.p(g.chaos) {
			to = pick(g, domain.PhaseStates())
		}
		return domain.PhaseReconciled{Revision: rev, PhaseID: id, To: to}
	case domain.EventPhaseSuperseded:
		rev, id := g.phaseFor(typ)
		if g.p(0.3) && len(st.Revisions) > 1 {
			rev = 1 + g.n(len(st.Revisions)) // an old revision is allowed
		}
		return domain.PhaseSuperseded{Revision: rev, PhaseID: id}

	case domain.EventAttemptStarted:
		p := domain.AttemptStarted{AttemptID: g.attemptFor(typ)}
		if a, ok := st.Attempt(p.AttemptID); ok && g.p(0.5) {
			p.SessionID = a.SessionID
		}
		if g.p(0.15) {
			p.SessionID = g.session()
		}
		return p
	case domain.EventAttemptAbandoned:
		id := g.attemptFor(typ)
		reason := pick(g, domain.AbandonReasons())
		if a, ok := st.Attempt(id); ok && !g.p(g.chaos) {
			for _, r := range domain.AbandonReasons() {
				if r.Source() == a.State {
					reason = r
				}
			}
		}
		return domain.AttemptAbandoned{AttemptID: id, Reason: reason}
	case domain.EventAttemptFinished:
		id := g.attemptFor(typ)
		return domain.AttemptFinished{AttemptID: id, Output: g.ref(domain.AttemptOutputPath(id, 1+g.n(2)))}
	case domain.EventAttemptFailed:
		p := domain.AttemptFailed{AttemptID: g.attemptFor(typ), Outcome: pick(g, domain.ExecutionOutcomes()[1:])}
		if g.p(0.5) {
			p.ErrorClass = pick(g, domain.ErrorClasses())
		}
		if g.p(0.3) {
			p.Detail = "the host reported an error"
		}
		return p
	case domain.EventAttemptTimedOut:
		return domain.AttemptTimedOut{AttemptID: g.attemptFor(typ)}
	case domain.EventAttemptCancelled:
		return domain.AttemptCancelled{AttemptID: g.attemptFor(typ)}
	case domain.EventAttemptAwaitingInput:
		return domain.AttemptAwaitingInput{AttemptID: g.attemptFor(typ), RequestID: requestID(g.fresh())}
	case domain.EventAttemptResumed:
		return domain.AttemptResumed{AttemptID: g.attemptFor(typ)}
	case domain.EventAttemptOutcomeUnknown:
		return domain.AttemptOutcomeUnknown{AttemptID: g.attemptFor(typ)}
	case domain.EventAttemptReconciled:
		id := g.attemptFor(typ)
		p := domain.AttemptReconciled{AttemptID: id, To: pick(g, []domain.AttemptState{domain.AttemptStateFinished, domain.AttemptStateFailed, domain.AttemptStateAbandoned})}
		if p.To == domain.AttemptStateFinished {
			p.Output = g.ref(domain.AttemptOutputPath(id, 3))
		}
		return p
	}
	g.t.Fatalf("the generator has no case for %s", typ)
	return nil
}
