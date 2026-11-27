package engine

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shutx-net/bellona/internal/artifact"
	"github.com/shutx-net/bellona/internal/config"
	"github.com/shutx-net/bellona/internal/domain"
	"github.com/shutx-net/bellona/internal/fsm"
	"github.com/shutx-net/bellona/internal/manifest"
	"github.com/shutx-net/bellona/internal/projection"
	"github.com/shutx-net/bellona/internal/storage"
	"github.com/shutx-net/bellona/internal/strictjson"
	"github.com/shutx-net/bellona/roles"
)

// fixture is a closed Run that every test of this file copies: a plan with one
// Phase, an Implementer Attempt that finished with a stored output, and the
// Phase in Running. Copying keeps the tests independent and cheap.
type fixture struct {
	root    string
	id      domain.RunID
	attempt domain.AttemptID
	cfg     config.Config
}

func buildFixture(t *testing.T) *fixture {
	t.Helper()
	v := newEnv(t, completing(t))
	ctx := testCtx(t)
	run := v.create()
	driveToExecuting(t, run, phase1)
	readyPhase(t, run, phase1)
	id, err := run.StartAttempt(ctx, AttemptSpec{Role: domain.RoleImplementer, PhaseID: phase1})
	if err != nil {
		t.Fatal(err)
	}
	if a, err := run.AwaitAttempt(ctx, id); err != nil || a.State != domain.AttemptStateFinished {
		t.Fatalf("the fixture Attempt: %v, %+v", err, a)
	}
	if err := run.Close(ctx); err != nil {
		t.Fatal(err)
	}
	return &fixture{root: v.root, id: run.ID(), attempt: id, cfg: v.cfg}
}

// copy returns an environment whose state root holds a copy of the fixture
// Run, with the engine of an ordinary fake runtime.
func (fx *fixture) copy(t *testing.T) *env {
	t.Helper()
	v := newEnv(t, completing(t))
	copyDir(t, storage.RunDir(fx.root, fx.id), v.runDir(fx.id))
	v.cfg = fx.cfg
	return v
}

// requireRefusal checks that Open refuses the Run with an error that matches
// every one of want, that the Run directory is byte-identical afterwards, and
// that the lock was released: a second Open fails the same way instead of with
// ErrLocked.
func requireRefusal(t *testing.T, v *env, id domain.RunID, p OpenParams, want ...error) error {
	t.Helper()
	runDir := v.runDir(id)
	before := fileSet(t, runDir)
	run, _, err := v.eng.Open(testCtx(t), id, p)
	if err == nil {
		_ = run.Close(testCtx(t))
		t.Fatal("Open succeeded, want a refusal")
	}
	for _, w := range want {
		if !errors.Is(err, w) {
			t.Fatalf("Open: %v\n does not match %v", err, w)
		}
	}
	sameFiles(t, "a refused Open", before, fileSet(t, runDir))
	_, _, again := v.eng.Open(testCtx(t), id, p)
	if again == nil || errors.Is(again, storage.ErrLocked) {
		t.Errorf("after the refusal a second Open returns %v; the lock must have been released", again)
	}
	for _, w := range want {
		if !errors.Is(again, w) {
			t.Errorf("the second Open: %v\n does not match %v", again, w)
		}
	}
	return err
}

// ---------------------------------------------------------------------------
// The lock
// ---------------------------------------------------------------------------

// TestOpenLock is R-PER-03: the lock is held for the whole life of the Run
// handle and Open fails while it is held.
func TestOpenLock(t *testing.T) {
	v := newEnv(t, completing(t))
	ctx := testCtx(t)
	run := v.create()
	runDir := v.runDir(run.ID())
	before := fileSet(t, runDir)

	_, _, err := v.eng.Open(ctx, run.ID(), OpenParams{})
	requireIs(t, err, storage.ErrLocked)
	// A refusal because of the lock writes nothing either.
	sameFiles(t, "Open of a locked Run", before, fileSet(t, runDir))
	// Another engine on the same state root meets the same lock.
	_, _, err = v.another().Open(ctx, run.ID(), OpenParams{})
	requireIs(t, err, storage.ErrLocked)

	if err := run.Close(ctx); err != nil {
		t.Fatal(err)
	}
	reopened, _ := v.open(v.eng, run.ID(), OpenParams{})
	if _, _, err := v.another().Open(ctx, run.ID(), OpenParams{}); !errors.Is(err, storage.ErrLocked) {
		t.Errorf("Open while the Run is held again: %v", err)
	}
	if err := reopened.Close(ctx); err != nil {
		t.Fatal(err)
	}

	_, _, err = v.eng.Open(ctx, "run-0000000000000000", OpenParams{})
	requireIs(t, err, storage.ErrRunNotFound)
	if _, _, err = v.eng.Open(ctx, "not-a-run", OpenParams{}); !errors.Is(err, domain.ErrInvalidID) {
		t.Errorf("Open of an invalid Run ID: %v", err)
	}
}

// ---------------------------------------------------------------------------
// The snapshot is a cache
// ---------------------------------------------------------------------------

// TestOpenSnapshot: a missing, unreadable, stale or mismatching snapshot is
// rebuilt from the journal (R-PER-02) and nothing else is written; a
// snapshot that is ahead of the journal is refused.
func TestOpenSnapshot(t *testing.T) {
	fx := buildFixture(t)
	runDirOf := func(v *env) string { return v.runDir(fx.id) }
	write := func(t *testing.T, v *env, data []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(runDirOf(v), domain.SnapshotPath), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	encode := func(t *testing.T, s projection.State) []byte {
		t.Helper()
		b, err := projection.EncodeSnapshot(s)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	repairs := []struct {
		name   string
		issue  string
		damage func(t *testing.T, v *env)
	}{
		{"missing", "missing", func(t *testing.T, v *env) {
			if err := os.Remove(filepath.Join(runDirOf(v), domain.SnapshotPath)); err != nil {
				t.Fatal(err)
			}
		}},
		{"garbage", "unreadable", func(t *testing.T, v *env) { write(t, v, []byte("not json at all")) }},
		{"empty", "unreadable", func(t *testing.T, v *env) { write(t, v, nil) }},
		{"truncated", "unreadable", func(t *testing.T, v *env) {
			good := encode(t, replayed(t, runDirOf(v)))
			write(t, v, good[:len(good)/2])
		}},
		{"another schema version", "unreadable", func(t *testing.T, v *env) {
			write(t, v, []byte(`{"schemaVersion":2,"state":{"future":true}}`))
		}},
		{"a symbolic link", "unreadable", func(t *testing.T, v *env) {
			link := filepath.Join(runDirOf(v), domain.SnapshotPath)
			if err := os.Remove(link); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(domain.TaskPath, link); err != nil {
				t.Fatal(err)
			}
		}},
		{"stale", "stale", func(t *testing.T, v *env) {
			events := readEvents(t, runDirOf(v))
			s, err := projection.Replay(events[:len(events)-1])
			if err != nil {
				t.Fatal(err)
			}
			write(t, v, encode(t, s))
		}},
		{"an earlier event", "stale", func(t *testing.T, v *env) {
			s, err := projection.Replay(readEvents(t, runDirOf(v))[:2])
			if err != nil {
				t.Fatal(err)
			}
			write(t, v, encode(t, s))
		}},
		{"the same event with another state", "mismatch", func(t *testing.T, v *env) {
			s := replayed(t, runDirOf(v))
			s.Run.ReplanCount = 9
			write(t, v, encode(t, s))
		}},
		{"another Run", "mismatch", func(t *testing.T, v *env) {
			s := replayed(t, runDirOf(v))
			s.RunID = "run-0000000000000000"
			write(t, v, encode(t, s))
		}},
	}
	for _, tc := range repairs {
		t.Run("rebuilt when "+tc.name, func(t *testing.T) {
			v := fx.copy(t)
			tc.damage(t, v)
			before := fileSet(t, runDirOf(v))
			want := replayed(t, runDirOf(v))

			run, rep := v.open(v.eng, fx.id, OpenParams{})
			if !rep.SnapshotRebuilt || rep.SnapshotIssue != tc.issue || rep.RecoveredTail != nil {
				t.Fatalf("report %+v, want the snapshot rebuilt as %q", rep, tc.issue)
			}
			equalStates(t, "the opened state", want, stateOf(t, run))
			after := fileSet(t, runDirOf(v))
			wantBytes := encode(t, want)
			if string(after[domain.SnapshotPath]) != string(wantBytes) {
				t.Errorf("state.json is not the snapshot of the replayed state")
			}
			// Only state.json changed.
			delete(before, domain.SnapshotPath)
			delete(after, domain.SnapshotPath)
			sameFiles(t, "the rebuild", before, after)
			if err := run.Close(testCtx(t)); err != nil {
				t.Fatal(err)
			}
			if _, rep := v.open(v.eng, fx.id, OpenParams{}); rep.SnapshotRebuilt {
				t.Errorf("the rebuilt snapshot is rebuilt again: %+v", rep)
			}
		})
	}

	t.Run("not rebuilt when it is current", func(t *testing.T) {
		v := fx.copy(t)
		before := fileSet(t, runDirOf(v))
		_, rep := v.open(v.eng, fx.id, OpenParams{})
		if rep.SnapshotRebuilt || rep.SnapshotIssue != "" {
			t.Errorf("report %+v", rep)
		}
		sameFiles(t, "Open of a current Run", before, fileSet(t, runDirOf(v)))
	})

	t.Run("refused when it is ahead of the journal", func(t *testing.T) {
		v := fx.copy(t)
		s := replayed(t, runDirOf(v))
		s.LastSeq += 3
		write(t, v, encode(t, s))
		requireRefusal(t, v, fx.id, OpenParams{}, ErrSnapshotAhead)
	})

	t.Run("refused when the journal lost its last events", func(t *testing.T) {
		// events.jsonl is cut at a record boundary: the snapshot then records
		// events that the journal does not have.
		v := fx.copy(t)
		path := filepath.Join(runDirOf(v), domain.EventsPath)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		lines := slices.Collect(splitLines(data))
		if err := os.WriteFile(path, joinLines(lines[:len(lines)-2]), 0o600); err != nil {
			t.Fatal(err)
		}
		requireRefusal(t, v, fx.id, OpenParams{}, ErrSnapshotAhead)
	})
}

// ---------------------------------------------------------------------------
// The journal
// ---------------------------------------------------------------------------

func splitLines(data []byte) func(yield func([]byte) bool) {
	return func(yield func([]byte) bool) {
		for len(data) > 0 {
			i := 0
			for i < len(data) && data[i] != '\n' {
				i++
			}
			end := min(i+1, len(data))
			if !yield(data[:end]) {
				return
			}
			data = data[end:]
		}
	}
}

func joinLines(lines [][]byte) []byte {
	var out []byte
	for _, l := range lines {
		out = append(out, l...)
	}
	return out
}

// appendJournal appends raw bytes to events.jsonl of a Run.
func appendJournal(t *testing.T, runDir string, data ...byte) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(runDir, domain.EventsPath), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// nextRecord encodes the event that would follow the last event of the Run
// directory, with the payload. The record ends with a newline, which matters:
// without it the journal treats the record as an incomplete tail and recovers
// instead of refusing.
func nextRecord(t *testing.T, runDir string, id domain.RunID, p domain.Payload, seqSkip uint64) []byte {
	t.Helper()
	events := readEvents(t, runDir)
	last := events[len(events)-1]
	record, err := domain.EncodeEvent(domain.Event{
		SchemaVersion: domain.EventSchemaVersion,
		Seq:           last.Seq + 1 + seqSkip,
		Type:          p.EventType(),
		RunID:         id,
		Time:          last.Time.Add(time.Second),
		Payload:       p,
	})
	if err != nil {
		t.Fatal(err)
	}
	return append(record, '\n')
}

// TestOpenJournal: corruption in the middle, a record that the reducer
// rejects, and a record of another event schema are refused; an incomplete
// trailing record is recovered (R-PER-05).
func TestOpenJournal(t *testing.T) {
	fx := buildFixture(t)

	t.Run("a corrupt record in the middle", func(t *testing.T) {
		v := fx.copy(t)
		path := filepath.Join(v.runDir(fx.id), domain.EventsPath)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		lines := slices.Collect(splitLines(data))
		lines[2] = append([]byte("x"), lines[2][1:]...) // the third record no longer starts with {
		if err := os.WriteFile(path, joinLines(lines), 0o600); err != nil {
			t.Fatal(err)
		}
		requireRefusal(t, v, fx.id, OpenParams{}, storage.ErrJournalCorrupt)
	})

	t.Run("a record with a gap in the sequence", func(t *testing.T) {
		v := fx.copy(t)
		runDir := v.runDir(fx.id)
		appendJournal(t, runDir, nextRecord(t, runDir, fx.id, domain.RunCompleted{}, 1)...)
		requireRefusal(t, v, fx.id, OpenParams{}, storage.ErrJournalCorrupt)
	})

	t.Run("a record of another Run", func(t *testing.T) {
		v := fx.copy(t)
		runDir := v.runDir(fx.id)
		appendJournal(t, runDir, nextRecord(t, runDir, "run-0000000000000000", domain.PhaseReady{Revision: 1, PhaseID: phase1}, 0)...)
		requireRefusal(t, v, fx.id, OpenParams{}, storage.ErrJournalCorrupt)
	})

	t.Run("a well-formed record that the reducer rejects", func(t *testing.T) {
		v := fx.copy(t)
		runDir := v.runDir(fx.id)
		// RunCompleted is not allowed while the Run is Executing.
		appendJournal(t, runDir, nextRecord(t, runDir, fx.id, domain.RunCompleted{}, 0)...)
		err := requireRefusal(t, v, fx.id, OpenParams{}, fsm.ErrIllegalTransition)
		var ae *projection.ApplyError
		if !errors.As(err, &ae) || ae.Type != domain.EventRunCompleted {
			t.Errorf("the error does not name the offending record: %v", err)
		}
		// An invalid (not illegal) record is refused as well.
		w := fx.copy(t)
		appendJournal(t, w.runDir(fx.id), nextRecord(t, w.runDir(fx.id), fx.id, domain.PhaseReady{Revision: 1, PhaseID: "phase-99"}, 0)...)
		requireRefusal(t, w, fx.id, OpenParams{}, projection.ErrInvalidEvent)
	})

	t.Run("a record of another event schema", func(t *testing.T) {
		v := fx.copy(t)
		runDir := v.runDir(fx.id)
		appendJournal(t, runDir, []byte(`{"schemaVersion":2,"seq":99,"type":"FutureEvent","runId":"`+string(fx.id)+`","payload":{}}`+"\n")...)
		requireRefusal(t, v, fx.id, OpenParams{}, ErrIncompatible, domain.ErrUnsupportedEventSchema)
	})

	t.Run("an incomplete trailing record is recovered", func(t *testing.T) {
		v := fx.copy(t)
		runDir := v.runDir(fx.id)
		want := replayed(t, runDir)
		record := nextRecord(t, runDir, fx.id, domain.RunCompleted{}, 0)
		half := record[:len(record)/2]
		appendJournal(t, runDir, half...)
		before := fileSet(t, runDir)

		run, rep := v.open(v.eng, fx.id, OpenParams{})
		if rep.RecoveredTail == nil || rep.SnapshotRebuilt {
			t.Fatalf("report %+v", rep)
		}
		if got := fileSet(t, runDir)[rep.RecoveredTail.PreservedAs]; string(got) != string(half) {
			t.Errorf("the preserved tail is %q, want %q", got, half)
		}
		checkRecoveryFiles(t, before, fileSet(t, runDir), rep)
		equalStates(t, "the recovered state", want, stateOf(t, run))
	})

	t.Run("a trailing run of NUL bytes is recovered", func(t *testing.T) {
		v := fx.copy(t)
		runDir := v.runDir(fx.id)
		want := replayed(t, runDir)
		zeros := make([]byte, 37)
		appendJournal(t, runDir, zeros...)
		before := fileSet(t, runDir)
		run, rep := v.open(v.eng, fx.id, OpenParams{})
		if rep.RecoveredTail == nil || rep.RecoveredTail.Length != int64(len(zeros)) {
			t.Fatalf("report %+v", rep)
		}
		checkRecoveryFiles(t, before, fileSet(t, runDir), rep)
		equalStates(t, "the recovered state", want, stateOf(t, run))
	})
}

// ---------------------------------------------------------------------------
// Artifacts
// ---------------------------------------------------------------------------

// TestOpenArtifacts is R-PER-07, R-WS-06 and R-MAN-01: a referenced file that
// is missing, changed, replaced by a symbolic link or not a regular file stops
// the resume, whether an event or the manifest refers to it.
func TestOpenArtifacts(t *testing.T) {
	fx := buildFixture(t)
	outputPath := domain.AttemptOutputPath(fx.attempt, 1)
	roleDefinition := domain.RoleDefinitionPath(domain.RoleImplementer)

	flip := func(rel string) func(t *testing.T, runDir string) {
		return func(t *testing.T, runDir string) {
			path := filepath.Join(runDir, filepath.FromSlash(rel))
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			data[0] ^= 0x01 // the same size, other content
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	cases := []struct {
		name   string
		mutate func(t *testing.T, runDir string)
		want   error
	}{
		{"task.json changed by one byte", flip(domain.TaskPath), storage.ErrArtifactMismatch},
		{"policy.json changed by one byte", flip(domain.PolicyPath), storage.ErrArtifactMismatch},
		{"manifest.json changed by one byte", flip(domain.ManifestPath), storage.ErrArtifactMismatch},
		{"a frozen role definition changed by one byte", flip(roleDefinition), storage.ErrArtifactMismatch},
		{"the plan changed by one byte", flip(domain.PlanPath(1)), storage.ErrArtifactMismatch},
		{"the Attempt output changed by one byte", flip(outputPath), storage.ErrArtifactMismatch},
		{"a frozen role definition with another size", func(t *testing.T, runDir string) {
			path := filepath.Join(runDir, filepath.FromSlash(roleDefinition))
			f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.WriteString("an extra line\n"); err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
		}, storage.ErrArtifactMismatch},
		{"the plan removed", func(t *testing.T, runDir string) {
			if err := os.Remove(filepath.Join(runDir, filepath.FromSlash(domain.PlanPath(1)))); err != nil {
				t.Fatal(err)
			}
		}, storage.ErrArtifactMissing},
		{"a frozen role definition removed", func(t *testing.T, runDir string) {
			if err := os.Remove(filepath.Join(runDir, filepath.FromSlash(domain.RoleDefinitionPath(domain.RolePlanner)))); err != nil {
				t.Fatal(err)
			}
		}, storage.ErrArtifactMissing},
		{"policy.json removed", func(t *testing.T, runDir string) {
			if err := os.Remove(filepath.Join(runDir, domain.PolicyPath)); err != nil {
				t.Fatal(err)
			}
		}, storage.ErrArtifactMissing},
		{"manifest.json removed", func(t *testing.T, runDir string) {
			if err := os.Remove(filepath.Join(runDir, domain.ManifestPath)); err != nil {
				t.Fatal(err)
			}
		}, storage.ErrArtifactMissing},
		{"the Attempt output replaced by a link to an identical file", func(t *testing.T, runDir string) {
			path := filepath.Join(runDir, filepath.FromSlash(outputPath))
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			real := filepath.Join(filepath.Dir(path), "real.json")
			if err := os.WriteFile(real, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("real.json", path); err != nil {
				t.Fatal(err)
			}
		}, storage.ErrSymlink},
		{"the Attempt output replaced by a link to another artifact", func(t *testing.T, runDir string) {
			path := filepath.Join(runDir, filepath.FromSlash(outputPath))
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			// attempts/<id>/turns/0001/output.json -> task.json of the Run
			if err := os.Symlink("../../../../"+domain.TaskPath, path); err != nil {
				t.Fatal(err)
			}
		}, storage.ErrSymlink},
		{"a directory of the Attempt replaced by a link", func(t *testing.T, runDir string) {
			dir := filepath.Join(runDir, "attempts", string(fx.attempt))
			moved := filepath.Join(runDir, "attempts", "moved")
			if err := os.Rename(dir, moved); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("moved", dir); err != nil {
				t.Fatal(err)
			}
		}, storage.ErrSymlink},
		{"a frozen role definition replaced by a link", func(t *testing.T, runDir string) {
			path := filepath.Join(runDir, filepath.FromSlash(roleDefinition))
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(runDir, "copy.md"), data, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("../../copy.md", path); err != nil {
				t.Fatal(err)
			}
		}, storage.ErrSymlink},
		{"the Attempt output replaced by a directory", func(t *testing.T, runDir string) {
			path := filepath.Join(runDir, filepath.FromSlash(outputPath))
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}, storage.ErrNotRegular},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := fx.copy(t)
			tc.mutate(t, v.runDir(fx.id))
			requireRefusal(t, v, fx.id, OpenParams{}, tc.want)
		})
	}

	t.Run("an orphan file that no event refers to is left alone", func(t *testing.T) {
		v := fx.copy(t)
		orphan := filepath.Join(v.runDir(fx.id), "attempts", "orphan.json")
		if err := os.WriteFile(orphan, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		before := fileSet(t, v.runDir(fx.id))
		run, _ := v.open(v.eng, fx.id, OpenParams{})
		for _, a := range stateOf(t, run).Artifacts {
			if a.Path == "attempts/orphan.json" {
				t.Error("an orphan became an accepted artifact")
			}
		}
		sameFiles(t, "Open with an orphan", before, fileSet(t, v.runDir(fx.id)))
	})
}

// ---------------------------------------------------------------------------
// Compatibility and consistency
// ---------------------------------------------------------------------------

// TestOpenCompatibility is R-MAN-02 and T-14: a Run recorded under other
// contract versions is refused, with the field named, and nothing is written.
func TestOpenCompatibility(t *testing.T) {
	current := manifest.Current()
	variants := []struct {
		field string // the name of the manifest member in the error
		mod   func(v *manifest.Versions)
	}{
		{"workflowVersion", func(v *manifest.Versions) { v.WorkflowVersion++ }},
		{"workflowDigest", func(v *manifest.Versions) { v.WorkflowDigest = "sha256:" + strings.Repeat("a", 64) }},
		{"workflowId", func(v *manifest.Versions) { v.WorkflowID = "another-workflow" }},
		{"eventSchemaVersion", func(v *manifest.Versions) { v.EventSchema++ }},
		{"configSchemaVersion", func(v *manifest.Versions) { v.ConfigSchema++ }},
		{"schemaVersion", func(v *manifest.Versions) { v.ManifestSchema++ }},
	}
	for _, tc := range variants {
		t.Run("recorded "+tc.field, func(t *testing.T) {
			recorded := current
			tc.mod(&recorded)
			v := newEnv(t, completing(t), func(o *Options) { o.recordVersions = &recorded })
			run := v.create()
			id := run.ID()
			if err := run.Close(testCtx(t)); err != nil {
				t.Fatal(err)
			}
			err := requireRefusal(t, v, id, OpenParams{}, ErrIncompatible)
			var ie *manifest.IncompatibleError
			if !errors.As(err, &ie) {
				t.Fatalf("the error is not an *IncompatibleError: %v", err)
			}
			if len(ie.Mismatches) != 1 || ie.Mismatches[0].Field != tc.field {
				t.Errorf("mismatches %+v, want only %s", ie.Mismatches, tc.field)
			}
		})
	}

	t.Run("supported versions that differ from the recorded ones", func(t *testing.T) {
		v := newEnv(t, completing(t))
		run := v.create()
		if err := run.Close(testCtx(t)); err != nil {
			t.Fatal(err)
		}
		supported := current
		supported.WorkflowVersion += 5
		supported.EventSchema += 1
		eng := v.another(func(o *Options) { o.supportedVersions = &supported })
		v.eng = eng
		err := requireRefusal(t, v, run.ID(), OpenParams{}, ErrIncompatible)
		var ie *manifest.IncompatibleError
		if !errors.As(err, &ie) || len(ie.Mismatches) != 2 {
			t.Errorf("the error does not list both mismatches: %v", err)
		}
	})

	t.Run("a compatible Run opens", func(t *testing.T) {
		v := newEnv(t, completing(t))
		run := v.create()
		if err := run.Close(testCtx(t)); err != nil {
			t.Fatal(err)
		}
		v.open(v.eng, run.ID(), OpenParams{})
	})
}

// forgeRun writes the two first events and the frozen files of a Run by hand
// under the state root, so that a test can make files that disagree with each
// other in ways that no engine would. The policy file holds policyCfg, the
// manifest is built for manifestRun, and ManifestFrozen records frozenLimits.
func forgeRun(t *testing.T, root string, id domain.RunID, policyCfg config.Config, frozenLimits domain.Limits, manifestRun domain.RunID) {
	t.Helper()
	runDir := storage.RunDir(root, id)
	put := func(rel string, data []byte) {
		t.Helper()
		path := filepath.Join(runDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	put(domain.LockPath, nil)
	taskData, err := strictjson.EncodeIndent(artifact.Task{SchemaVersion: artifact.TaskSchemaVersion, Request: testRequest})
	if err != nil {
		t.Fatal(err)
	}
	put(domain.TaskPath, taskData)
	frozen, err := manifest.Build(manifest.BuildInput{RunID: manifestRun, Effective: policyCfg, Roles: roles.Definitions()})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range frozen.Files {
		put(f.Path, f.Data)
	}
	manifestFile := frozen.Files[len(frozen.Files)-1]
	at := newTestClock().Now()
	var journal []byte
	for i, p := range []domain.Payload{
		domain.RunCreated{Task: refOf(t, newFile{Path: domain.TaskPath, Data: taskData})},
		domain.ManifestFrozen{Manifest: refOf(t, newFile{Path: manifestFile.Path, Data: manifestFile.Data}), Policy: frozen.Manifest.Policy, Limits: frozenLimits},
	} {
		record, err := domain.EncodeEvent(domain.Event{
			SchemaVersion: domain.EventSchemaVersion, Seq: uint64(i + 1), Type: p.EventType(), RunID: id, Time: at, Payload: p,
		})
		if err != nil {
			t.Fatal(err)
		}
		journal = append(append(journal, record...), '\n')
	}
	put(domain.EventsPath, journal)
}

// TestOpenConsistency: the frozen files must agree with the event that froze
// them (ErrInconsistentRun).
func TestOpenConsistency(t *testing.T) {
	const id domain.RunID = "run-0123456789abcdef"
	cfg, err := config.Resolve(config.Inputs{})
	if err != nil {
		t.Fatal(err)
	}
	limits, err := cfg.Limits()
	if err != nil {
		t.Fatal(err)
	}

	t.Run("a forged Run that is consistent opens", func(t *testing.T) {
		v := newEnv(t, nil)
		forgeRun(t, v.root, id, cfg, limits, id)
		v.open(v.eng, id, OpenParams{})
	})
	t.Run("policy limits that differ from ManifestFrozen", func(t *testing.T) {
		v := newEnv(t, nil)
		other := cfg.Clone()
		replans := *other.Execution.MaxReplans + 4
		other.Execution.MaxReplans = &replans
		forgeRun(t, v.root, id, other, limits, id)
		requireRefusal(t, v, id, OpenParams{}, ErrInconsistentRun)
	})
	t.Run("a manifest of another Run", func(t *testing.T) {
		v := newEnv(t, nil)
		forgeRun(t, v.root, id, cfg, limits, "run-fedcba9876543210")
		requireRefusal(t, v, id, OpenParams{}, ErrInconsistentRun)
	})
}

// ---------------------------------------------------------------------------
// Incomplete Runs
// ---------------------------------------------------------------------------

// TestOpenIncompleteRun: a Run directory with fewer than the two events that
// every Run starts with is refused as incomplete, and Open writes nothing.
func TestOpenIncompleteRun(t *testing.T) {
	t.Run("an empty journal", func(t *testing.T) {
		v := newEnv(t, nil)
		const id domain.RunID = "run-0123456789abcdef"
		runDir := v.runDir(id)
		if err := os.MkdirAll(runDir, 0o700); err != nil {
			t.Fatal(err)
		}
		// Both files exist, so that Open has nothing to create.
		for _, name := range []string{domain.LockPath, domain.EventsPath} {
			if err := os.WriteFile(filepath.Join(runDir, name), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		requireRefusal(t, v, id, OpenParams{}, ErrIncompleteRun)
	})

	// A Run directory without events.jsonl is incomplete, like one with an empty
	// journal. Open must not create the journal (nor anything else) on the way
	// to the refusal: requireRefusal checks that the directory is unchanged and
	// that the lock was released.
	t.Run("a missing journal", func(t *testing.T) {
		v := newEnv(t, nil)
		run := v.create()
		if err := run.Close(testCtx(t)); err != nil {
			t.Fatal(err)
		}
		runDir := v.runDir(run.ID())
		if err := os.Remove(filepath.Join(runDir, domain.EventsPath)); err != nil {
			t.Fatal(err)
		}
		err := requireRefusal(t, v, run.ID(), OpenParams{}, ErrIncompleteRun, fs.ErrNotExist)
		var pe *storage.PathError
		if !errors.As(err, &pe) || pe.Path != domain.EventsPath {
			t.Errorf("the refusal %v does not carry the storage error for %s", err, domain.EventsPath)
		}
		if _, statErr := os.Lstat(filepath.Join(runDir, domain.EventsPath)); !errors.Is(statErr, fs.ErrNotExist) {
			t.Errorf("Open created %s: %v", domain.EventsPath, statErr)
		}
	})

	t.Run("a missing journal of a Run that has a snapshot and artifacts", func(t *testing.T) {
		// The snapshot is a cache of the journal, never a replacement for it.
		fx := buildFixture(t)
		v := fx.copy(t)
		if err := os.Remove(filepath.Join(v.runDir(fx.id), domain.EventsPath)); err != nil {
			t.Fatal(err)
		}
		requireRefusal(t, v, fx.id, OpenParams{}, ErrIncompleteRun, fs.ErrNotExist)
	})

	t.Run("a Run directory that is empty", func(t *testing.T) {
		v := newEnv(t, nil)
		const id domain.RunID = "run-0123456789abcdef"
		if err := os.MkdirAll(v.runDir(id), 0o700); err != nil {
			t.Fatal(err)
		}
		requireRefusal(t, v, id, OpenParams{}, ErrIncompleteRun, fs.ErrNotExist)
		if entries, err := os.ReadDir(v.runDir(id)); err != nil || len(entries) != 0 {
			t.Errorf("the empty Run directory holds %v (%v) after the refusal", entries, err)
		}
	})

	t.Run("a Run whose creation failed after RunCreated", func(t *testing.T) {
		// The second artifact write of Create is the first file of
		// ManifestFrozen: crash there.
		var writes atomic.Int32
		v := newEnv(t, nil, func(o *Options) {
			o.Faults = func(p storage.CrashPoint) bool {
				return p == storage.CrashArtifactTempSynced && writes.Add(1) == 2
			}
		})
		_, err := v.eng.Create(testCtx(t), CreateParams{Request: testRequest, Config: v.cfg})
		requireIs(t, err, storage.ErrInjectedCrash)
		entries, err := os.ReadDir(filepath.Join(v.root, "runs"))
		if err != nil || len(entries) != 1 {
			t.Fatalf("the partial Run directory must stay in place: %v, %d entries", err, len(entries))
		}
		id := domain.RunID(entries[0].Name())
		if got := len(readEvents(t, v.runDir(id))); got != 1 {
			t.Fatalf("the journal has %d events, want only RunCreated", got)
		}
		// The failed Create released the lock; Open reports an incomplete Run.
		v.eng = v.another()
		requireRefusal(t, v, id, OpenParams{}, ErrIncompleteRun)
	})
}

// ---------------------------------------------------------------------------
// Changes since the freeze are reported, never applied
// ---------------------------------------------------------------------------

// TestOpenReportsChanges is R-CFG-01 and T-14: a configuration or role
// definition that differs from the frozen copy is reported, and the Run keeps
// using the frozen one.
func TestOpenReportsChanges(t *testing.T) {
	fx := buildFixture(t)
	changedConfig := func(replans int) *config.Config {
		c := fx.cfg.Clone()
		c.Execution.MaxReplans = &replans
		return &c
	}
	changedRoles := func(changed ...domain.Role) []domain.RoleDefinition {
		defs := roles.Definitions()
		for i := range defs {
			if slices.Contains(changed, defs[i].Role) {
				defs[i].Content = append(defs[i].Content, []byte("\nA changed line.\n")...)
			}
		}
		return defs
	}

	t.Run("the same configuration and role definitions", func(t *testing.T) {
		v := fx.copy(t)
		same := fx.cfg.Clone()
		_, rep := v.open(v.eng, fx.id, OpenParams{CurrentConfig: &same, CurrentRoles: roles.Definitions()})
		if rep.ConfigChanged || len(rep.ChangedRoles) != 0 {
			t.Errorf("report %+v", rep)
		}
	})

	t.Run("a changed configuration", func(t *testing.T) {
		v := fx.copy(t)
		before := fileSet(t, v.runDir(fx.id))
		frozen := replayed(t, v.runDir(fx.id)).Limits
		run, rep := v.open(v.eng, fx.id, OpenParams{CurrentConfig: changedConfig(*fx.cfg.Execution.MaxReplans + 3)})
		if !rep.ConfigChanged || len(rep.ChangedRoles) != 0 {
			t.Fatalf("report %+v", rep)
		}
		if got := stateOf(t, run).Limits; got != frozen {
			t.Errorf("the limits are %+v, the frozen ones are %+v", got, frozen)
		}
		sameFiles(t, "Open with a changed configuration", before, fileSet(t, v.runDir(fx.id)))
	})

	t.Run("a changed role definition", func(t *testing.T) {
		v := fx.copy(t)
		before := fileSet(t, v.runDir(fx.id))
		_, rep := v.open(v.eng, fx.id, OpenParams{CurrentRoles: changedRoles(domain.RolePlanner)})
		if rep.ConfigChanged || !slices.Equal(rep.ChangedRoles, []domain.Role{domain.RolePlanner}) {
			t.Fatalf("report %+v", rep)
		}
		sameFiles(t, "Open with a changed role definition", before, fileSet(t, v.runDir(fx.id)))
	})

	t.Run("both changed", func(t *testing.T) {
		v := fx.copy(t)
		_, rep := v.open(v.eng, fx.id, OpenParams{CurrentConfig: changedConfig(1),
			CurrentRoles: changedRoles(domain.RoleEvaluator, domain.RolePlanner)})
		want := []domain.Role{domain.RolePlanner, domain.RoleEvaluator}
		if !rep.ConfigChanged || !slices.Equal(rep.ChangedRoles, want) {
			t.Fatalf("report %+v, want the configuration and %v", rep, want)
		}
	})

	t.Run("invalid role definitions are refused before the snapshot is written", func(t *testing.T) {
		v := fx.copy(t)
		if err := os.Remove(filepath.Join(v.runDir(fx.id), domain.SnapshotPath)); err != nil {
			t.Fatal(err)
		}
		// The snapshot is missing, so Open would rebuild it; the refusal has to
		// come first, and state.json stays missing.
		requireRefusal(t, v, fx.id, OpenParams{CurrentRoles: roles.Definitions()[:1]}, domain.ErrInvalidRoleDefinition)
		if _, err := os.Stat(filepath.Join(v.runDir(fx.id), domain.SnapshotPath)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a refused Open wrote the snapshot: %v", err)
		}
	})
}
