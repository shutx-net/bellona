package storage

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/shutx-net/bellona/internal/domain"
)

// crashAt returns an injector that fires at p, once armed.
func crashAt(p CrashPoint, armed *bool) FaultInjector {
	return func(q CrashPoint) bool { return *armed && q == p }
}

// tempFilesIn returns the names of the dot-prefixed files directly in dir.
func tempFilesIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			out = append(out, e.Name())
		}
	}
	return out
}

// wantDeadThenAbandon checks that every call of a dead Store returns
// ErrStoreDead and writes nothing, that the Run stays locked, and then
// abandons the Store so that the caller can reopen the Run.
func wantDeadThenAbandon(t *testing.T, s *Store) {
	t.Helper()
	dir := s.Dir()
	calls := map[string]func() error{
		"WriteArtifact":  func() error { _, err := s.WriteArtifact("after/crash.json", []byte("x")); return err },
		"ReadArtifact":   func() error { _, err := s.ReadArtifact(domain.ArtifactRef{}); return err },
		"VerifyArtifact": func() error { return s.VerifyArtifacts(nil) },
		"AppendEvent":    func() error { return s.AppendEvent(testRecord(t, 99)) },
		"LoadJournal":    func() error { _, err := s.LoadJournal(); return err },
		"WriteSnapshot":  func() error { return s.WriteSnapshot([]byte("x")) },
		"ReadSnapshot":   func() error { _, err := s.ReadSnapshot(); return err },
	}
	before := fileSet(t, dir)
	for name, call := range calls {
		if err := call(); !errors.Is(err, ErrStoreDead) {
			t.Errorf("%s on a dead Store: %v, want ErrStoreDead", name, err)
		}
	}
	if after := fileSet(t, dir); len(after) != len(before) {
		t.Errorf("a dead Store wrote files: %v -> %v", before, after)
	}
	// A dead Store still holds the Run lock until it is abandoned.
	if _, err := OpenRun(dir, Options{}); !errors.Is(err, ErrLocked) {
		t.Errorf("OpenRun while the dead Store holds the Run: %v, want ErrLocked", err)
	}
	s.Abandon()
	if err := s.WriteSnapshot([]byte("x")); err == nil {
		t.Error("an abandoned Store accepted a write")
	}
}

func TestCrashPointNamesAndOrder(t *testing.T) {
	points := CrashPoints()
	if len(points) != 8 {
		t.Fatalf("%d crash points, want 8", len(points))
	}
	want := []string{
		"CrashArtifactTempSynced", "CrashArtifactRenamed", "CrashArtifactDirSynced",
		"CrashEventTorn", "CrashEventWritten", "CrashEventSynced",
		"CrashSnapshotTempSynced", "CrashSnapshotRenamed",
	}
	for i, p := range points {
		if p.String() != want[i] {
			t.Errorf("point %d is %s, want %s", i, p, want[i])
		}
	}
	if CrashPoint(0).String() != "CrashPoint(0)" || CrashPoint(99).String() != "CrashPoint(99)" {
		t.Errorf("unknown points: %s, %s", CrashPoint(0), CrashPoint(99))
	}
}

// TestCrashPointsAreAskedInPersistenceOrder runs one full cycle with an
// injector that never fires and checks the order in which the points come up.
func TestCrashPointsAreAskedInPersistenceOrder(t *testing.T) {
	var seen []CrashPoint
	s := newStore(t, Options{Faults: func(p CrashPoint) bool { seen = append(seen, p); return false }})
	if _, err := s.WriteArtifact("task.json", []byte("task")); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendEvent(testRecord(t, 1)); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteSnapshot([]byte("{}")); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(seen, CrashPoints()) {
		t.Errorf("points asked: %v\nwant: %v", seen, CrashPoints())
	}
	// A nil injector never fires, and the same cycle works.
	s2 := newStore(t, Options{})
	if _, err := s2.WriteArtifact("task.json", []byte("task")); err != nil {
		t.Fatal(err)
	}
}

func TestCrashArtifactTempSynced(t *testing.T) {
	armed := false
	s := newStore(t, Options{Faults: crashAt(CrashArtifactTempSynced, &armed)})
	dir := s.Dir()
	data := []byte("artifact written but never renamed")
	armed = true
	_, err := s.WriteArtifact("plans/rev-0001/plan.json", data)
	if !errors.Is(err, ErrInjectedCrash) || !strings.Contains(err.Error(), "CrashArtifactTempSynced") {
		t.Fatalf("WriteArtifact: %v, want ErrInjectedCrash naming the point", err)
	}
	before := fileSet(t, dir)
	wantDeadThenAbandon(t, s)

	s = reopen(t, dir, Options{})
	j, err := s.LoadJournal()
	if err != nil || len(j.Events) != 0 || j.Recovered != nil {
		t.Fatalf("LoadJournal = %+v, %v", j, err)
	}
	// Only a dot-prefixed temporary file with the full bytes; no artifact.
	tmps := tempFilesIn(t, filepath.Join(dir, "plans/rev-0001"))
	if len(tmps) != 1 || !strings.HasPrefix(tmps[0], ".plan.json.tmp-") {
		t.Fatalf("temporary files: %v", tmps)
	}
	if got := mustRead(t, filepath.Join(dir, "plans/rev-0001", tmps[0])); !bytes.Equal(got, data) {
		t.Errorf("the synced temporary file holds %q", got)
	}
	if _, err := os.Lstat(filepath.Join(dir, "plans/rev-0001/plan.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the artifact exists: %v", err)
	}
	if _, err := s.ReadArtifact(mustRef(t, "plans/rev-0001/plan.json", data)); !errors.Is(err, ErrArtifactMissing) {
		t.Errorf("ReadArtifact: %v, want ErrArtifactMissing", err)
	}
	assertNothingDisappeared(t, before, fileSet(t, dir))
}

func TestCrashArtifactRenamedAndDirSynced(t *testing.T) {
	for _, point := range []CrashPoint{CrashArtifactRenamed, CrashArtifactDirSynced} {
		t.Run(point.String(), func(t *testing.T) {
			armed := false
			s := newStore(t, Options{Faults: crashAt(point, &armed)})
			dir := s.Dir()
			data := []byte("complete artifact, no event yet")
			ref := mustRef(t, "attempts/attempt-0123456789abcdef/turns/0001/output.json", data)
			armed = true
			if _, err := s.WriteArtifact(ref.Path, data); !errors.Is(err, ErrInjectedCrash) {
				t.Fatalf("WriteArtifact: %v, want ErrInjectedCrash", err)
			}
			before := fileSet(t, dir)
			wantDeadThenAbandon(t, s)

			s = reopen(t, dir, Options{})
			// The complete artifact is there and verifies against a
			// reference that the test computed itself; the journal has no
			// event for it.
			got, err := s.ReadArtifact(ref)
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("ReadArtifact = %q, %v", got, err)
			}
			j, err := s.LoadJournal()
			if err != nil || len(j.Events) != 0 {
				t.Fatalf("LoadJournal = %+v, %v", j, err)
			}
			assertNoTempFiles(t, dir)
			assertNothingDisappeared(t, before, fileSet(t, dir))
			// The engine's retry of the same write is idempotent.
			again, err := s.WriteArtifact(ref.Path, data)
			if err != nil || again != ref {
				t.Errorf("repeated WriteArtifact = %+v, %v", again, err)
			}
		})
	}
}

func TestCrashEventTorn(t *testing.T) {
	armed := false
	s := newStore(t, Options{Faults: crashAt(CrashEventTorn, &armed)})
	dir := s.Dir()
	appendAll(t, s, 2)
	r3 := testRecord(t, 3)
	armed = true
	if err := s.AppendEvent(r3); !errors.Is(err, ErrInjectedCrash) {
		t.Fatalf("AppendEvent: %v, want ErrInjectedCrash", err)
	}
	// Exactly the first half of the record, no newline.
	complete := journalBytes(testRecord(t, 1), testRecord(t, 2))
	want := append(bytes.Clone(complete), r3[:len(r3)/2]...)
	if got := mustRead(t, filepath.Join(dir, domain.EventsPath)); !bytes.Equal(got, want) {
		t.Fatalf("the journal holds %q", got)
	}
	before := fileSet(t, dir)
	wantDeadThenAbandon(t, s)

	s = reopen(t, dir, Options{})
	j, err := s.LoadJournal()
	if err != nil {
		t.Fatal(err)
	}
	if len(j.Events) != 2 || j.Recovered == nil || j.Recovered.Offset != int64(len(complete)) {
		t.Fatalf("Journal = %+v (%+v)", j, j.Recovered)
	}
	if got := mustRead(t, filepath.Join(dir, j.Recovered.PreservedAs)); !bytes.Equal(got, r3[:len(r3)/2]) {
		t.Errorf("preserved %q", got)
	}
	assertNothingDisappeared(t, before, fileSet(t, dir))
	// The event goes in again with the same sequence number.
	if err := s.AppendEvent(r3); err != nil {
		t.Fatal(err)
	}
	j, err = s.LoadJournal()
	if err != nil || len(j.Events) != 3 || j.Recovered != nil {
		t.Fatalf("after the append: %+v, %v", j, err)
	}
}

func TestCrashEventWrittenAndSynced(t *testing.T) {
	for _, point := range []CrashPoint{CrashEventWritten, CrashEventSynced} {
		t.Run(point.String(), func(t *testing.T) {
			armed := false
			s := newStore(t, Options{Faults: crashAt(point, &armed)})
			dir := s.Dir()
			appendAll(t, s, 1)
			armed = true
			if err := s.AppendEvent(testRecord(t, 2)); !errors.Is(err, ErrInjectedCrash) {
				t.Fatalf("AppendEvent: %v, want ErrInjectedCrash", err)
			}
			before := fileSet(t, dir)
			wantDeadThenAbandon(t, s)

			s = reopen(t, dir, Options{})
			j, err := s.LoadJournal()
			if err != nil || len(j.Events) != 2 || j.Recovered != nil {
				t.Fatalf("LoadJournal = %+v, %v", j, err)
			}
			if got := mustRead(t, filepath.Join(dir, domain.EventsPath)); !bytes.Equal(got, journalBytes(testRecord(t, 1), testRecord(t, 2))) {
				t.Errorf("the journal holds %q", got)
			}
			// No snapshot was written: the engine rebuilds it from the journal.
			if _, err := s.ReadSnapshot(); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("ReadSnapshot: %v, want not exist", err)
			}
			assertNothingDisappeared(t, before, fileSet(t, dir))
		})
	}
}

func TestCrashSnapshotTempSynced(t *testing.T) {
	armed := false
	s := newStore(t, Options{Faults: crashAt(CrashSnapshotTempSynced, &armed)})
	dir := s.Dir()
	if err := s.WriteSnapshot([]byte("old snapshot")); err != nil {
		t.Fatal(err)
	}
	armed = true
	if err := s.WriteSnapshot([]byte("new snapshot")); !errors.Is(err, ErrInjectedCrash) {
		t.Fatalf("WriteSnapshot: %v, want ErrInjectedCrash", err)
	}
	before := fileSet(t, dir)
	wantDeadThenAbandon(t, s)

	s = reopen(t, dir, Options{})
	got, err := s.ReadSnapshot()
	if err != nil || string(got) != "old snapshot" {
		t.Fatalf("ReadSnapshot = %q, %v; want the old snapshot", got, err)
	}
	tmps := tempFilesIn(t, dir)
	if len(tmps) != 1 || !strings.HasPrefix(tmps[0], ".state.json.tmp-") {
		t.Fatalf("temporary files: %v", tmps)
	}
	if got := mustRead(t, filepath.Join(dir, tmps[0])); string(got) != "new snapshot" {
		t.Errorf("the temporary file holds %q", got)
	}
	assertNothingDisappeared(t, before, fileSet(t, dir))
	// A new attempt succeeds and leaves the orphan alone.
	if err := s.WriteSnapshot([]byte("newer snapshot")); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ReadSnapshot(); string(got) != "newer snapshot" {
		t.Errorf("ReadSnapshot = %q", got)
	}
	if len(tempFilesIn(t, dir)) != 1 {
		t.Error("the orphaned temporary file was removed")
	}
}

func TestCrashSnapshotRenamed(t *testing.T) {
	armed := false
	s := newStore(t, Options{Faults: crashAt(CrashSnapshotRenamed, &armed)})
	dir := s.Dir()
	if err := s.WriteSnapshot([]byte("old snapshot")); err != nil {
		t.Fatal(err)
	}
	armed = true
	if err := s.WriteSnapshot([]byte("new snapshot")); !errors.Is(err, ErrInjectedCrash) {
		t.Fatalf("WriteSnapshot: %v, want ErrInjectedCrash", err)
	}
	before := fileSet(t, dir)
	wantDeadThenAbandon(t, s)

	s = reopen(t, dir, Options{})
	got, err := s.ReadSnapshot()
	if err != nil || string(got) != "new snapshot" {
		t.Fatalf("ReadSnapshot = %q, %v; want the new snapshot", got, err)
	}
	assertNoTempFiles(t, dir)
	assertNothingDisappeared(t, before, fileSet(t, dir))
}

// TestCrashInjectorOnlyFiresAtItsPoint checks that an operation is not
// affected by an injector for a point that it does not pass.
func TestCrashInjectorOnlyFiresAtItsPoint(t *testing.T) {
	for _, point := range CrashPoints() {
		t.Run(point.String(), func(t *testing.T) {
			armed := true
			s := newStore(t, Options{Faults: crashAt(point, &armed)})
			artifactPoint := point <= CrashArtifactDirSynced
			eventPoint := point >= CrashEventTorn && point <= CrashEventSynced
			snapshotPoint := point >= CrashSnapshotTempSynced

			_, err := s.WriteArtifact("a.json", []byte("x"))
			if got := errors.Is(err, ErrInjectedCrash); got != artifactPoint {
				t.Fatalf("WriteArtifact crashed = %v, want %v (%v)", got, artifactPoint, err)
			}
			if artifactPoint {
				return
			}
			err = s.AppendEvent(testRecord(t, 1))
			if got := errors.Is(err, ErrInjectedCrash); got != eventPoint {
				t.Fatalf("AppendEvent crashed = %v, want %v (%v)", got, eventPoint, err)
			}
			if eventPoint {
				return
			}
			err = s.WriteSnapshot([]byte("x"))
			if got := errors.Is(err, ErrInjectedCrash); got != snapshotPoint {
				t.Fatalf("WriteSnapshot crashed = %v, want %v (%v)", got, snapshotPoint, err)
			}
		})
	}
}

// TestCrashSequenceNeverLosesAFile walks the whole persistence order, crashing
// at every point in turn on top of a Run that already holds files, and checks
// after each crash and each recovery that no earlier file has disappeared and
// that the journal is intact.
func TestCrashSequenceNeverLosesAFile(t *testing.T) {
	for _, point := range CrashPoints() {
		t.Run(point.String(), func(t *testing.T) {
			armed := false
			s := newStore(t, Options{Faults: crashAt(point, &armed)})
			dir := s.Dir()
			// A Run with history: artifacts, events, a snapshot.
			for i := range 3 {
				if _, err := s.WriteArtifact(fmt.Sprintf("plans/rev-%04d/plan.json", i+1), []byte(fmt.Sprint("plan ", i))); err != nil {
					t.Fatal(err)
				}
			}
			appendAll(t, s, 3)
			if err := s.WriteSnapshot([]byte("snapshot after 3")); err != nil {
				t.Fatal(err)
			}
			history := fileSet(t, dir)

			armed = true
			var err error
			switch {
			case point <= CrashArtifactDirSynced:
				_, err = s.WriteArtifact("plans/rev-0004/plan.json", []byte("plan 3"))
			case point <= CrashEventSynced:
				err = s.AppendEvent(testRecord(t, 4))
			default:
				err = s.WriteSnapshot([]byte("snapshot after 4"))
			}
			if !errors.Is(err, ErrInjectedCrash) {
				t.Fatalf("operation: %v, want ErrInjectedCrash", err)
			}
			afterCrash := fileSet(t, dir)
			assertNothingDisappeared(t, history, afterCrash)
			s.Abandon()

			s = reopen(t, dir, Options{})
			j, err := s.LoadJournal()
			if err != nil {
				t.Fatalf("LoadJournal: %v", err)
			}
			if n := len(j.Events); n < 3 || n > 4 {
				t.Errorf("%d events after recovery", n)
			}
			assertNothingDisappeared(t, history, fileSet(t, dir))
			assertNothingDisappeared(t, afterCrash, fileSet(t, dir))
			// Every history artifact still verifies.
			for i := range 3 {
				ref := mustRef(t, fmt.Sprintf("plans/rev-%04d/plan.json", i+1), []byte(fmt.Sprint("plan ", i)))
				if _, err := s.ReadArtifact(ref); err != nil {
					t.Errorf("artifact %d: %v", i+1, err)
				}
			}
		})
	}
}
