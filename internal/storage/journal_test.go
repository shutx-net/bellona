package storage

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/shutx-net/bellona/internal/domain"
)

var baseTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// recordFor returns the encoded event number seq of the Run runID: RunCreated,
// then ManifestFrozen, then RunPrepared for everything after (the journal
// layer checks the shape and the sequence, not the lifecycle).
func recordFor(t *testing.T, runID domain.RunID, seq uint64) []byte {
	t.Helper()
	var p domain.Payload
	switch seq {
	case 1:
		p = domain.RunCreated{Task: mustRef(t, domain.TaskPath, []byte("task"))}
	case 2:
		p = domain.ManifestFrozen{
			Manifest: mustRef(t, domain.ManifestPath, []byte("manifest")),
			Policy:   mustRef(t, domain.PolicyPath, []byte("policy")),
			Limits:   domain.Limits{MaxPhaseRetries: 2, MaxReplans: 1},
		}
	default:
		p = domain.RunPrepared{}
	}
	rec, err := domain.EncodeEvent(domain.Event{
		SchemaVersion: domain.EventSchemaVersion,
		Seq:           seq,
		Type:          p.EventType(),
		RunID:         runID,
		Time:          baseTime.Add(time.Duration(seq) * time.Second),
		Payload:       p,
	})
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func testRecord(t *testing.T, seq uint64) []byte { return recordFor(t, testRunID, seq) }

// journalBytes joins records, each followed by a newline.
func journalBytes(records ...[]byte) []byte {
	var b []byte
	for _, r := range records {
		b = append(append(b, r...), '\n')
	}
	return b
}

// journalWith creates a Run, closes it, puts raw into events.jsonl and opens
// the Run again.
func journalWith(t *testing.T, raw []byte) (*Store, string) {
	t.Helper()
	s := newStore(t, Options{})
	dir := s.Dir()
	s.Close()
	mustWrite(t, filepath.Join(dir, domain.EventsPath), raw)
	return reopen(t, dir, Options{}), dir
}

func appendAll(t *testing.T, s *Store, n uint64) {
	t.Helper()
	for seq := uint64(1); seq <= n; seq++ {
		if err := s.AppendEvent(testRecord(t, seq)); err != nil {
			t.Fatalf("AppendEvent(%d): %v", seq, err)
		}
	}
}

func TestJournalAppendAndLoad(t *testing.T) {
	s := newStore(t, Options{})
	appendAll(t, s, 5)
	j, err := s.LoadJournal()
	if err != nil {
		t.Fatal(err)
	}
	if j.Recovered != nil || len(j.Events) != 5 {
		t.Fatalf("Journal = %+v", j)
	}
	for i, ev := range j.Events {
		seq := uint64(i + 1)
		if ev.Seq != seq || ev.RunID != testRunID || ev.SchemaVersion != domain.EventSchemaVersion {
			t.Errorf("event %d: %+v", i, ev)
		}
		again, err := domain.EncodeEvent(ev)
		if err != nil || !bytes.Equal(again, testRecord(t, seq)) {
			t.Errorf("event %d does not re-encode to its record: %v", i, err)
		}
	}
	if j.Events[0].Type != domain.EventRunCreated || j.Events[1].Type != domain.EventManifestFrozen {
		t.Errorf("types: %s, %s", j.Events[0].Type, j.Events[1].Type)
	}
	var want []byte
	for seq := uint64(1); seq <= 5; seq++ {
		want = append(append(want, testRecord(t, seq)...), '\n')
	}
	if got := mustRead(t, filepath.Join(s.Dir(), domain.EventsPath)); !bytes.Equal(got, want) {
		t.Errorf("journal bytes:\n%s\nwant:\n%s", got, want)
	}
}

func TestJournalSurvivesReopen(t *testing.T) {
	s := newStore(t, Options{})
	dir := s.Dir()
	appendAll(t, s, 3)
	s.Close()

	s = reopen(t, dir, Options{})
	if err := s.AppendEvent(testRecord(t, 4)); !errors.Is(err, ErrJournalNotLoaded) {
		t.Fatalf("AppendEvent before LoadJournal: %v, want ErrJournalNotLoaded", err)
	}
	j, err := s.LoadJournal()
	if err != nil || len(j.Events) != 3 || j.Recovered != nil {
		t.Fatalf("LoadJournal = %+v, %v", j, err)
	}
	if err := s.AppendEvent(testRecord(t, 4)); err != nil {
		t.Fatal(err)
	}
	j, err = s.LoadJournal()
	if err != nil || len(j.Events) != 4 {
		t.Fatalf("LoadJournal = %+v, %v", j, err)
	}
	// Loading again changes nothing.
	j2, err := s.LoadJournal()
	if err != nil || len(j2.Events) != 4 {
		t.Fatalf("second LoadJournal = %+v, %v", j2, err)
	}
}

func TestJournalEmpty(t *testing.T) {
	s := newStore(t, Options{})
	dir := s.Dir()
	s.Close()
	s = reopen(t, dir, Options{})
	j, err := s.LoadJournal()
	if err != nil || len(j.Events) != 0 || j.Recovered != nil {
		t.Fatalf("LoadJournal = %+v, %v", j, err)
	}
}

func TestJournalLoadWithoutTailLeavesFileByteIdentical(t *testing.T) {
	raw := journalBytes(testRecord(t, 1), testRecord(t, 2), testRecord(t, 3))
	s, dir := journalWith(t, raw)
	before := fileSet(t, dir)
	for range 2 {
		j, err := s.LoadJournal()
		if err != nil || len(j.Events) != 3 || j.Recovered != nil {
			t.Fatalf("LoadJournal = %+v, %v", j, err)
		}
	}
	if got := mustRead(t, filepath.Join(dir, domain.EventsPath)); !bytes.Equal(got, raw) {
		t.Error("the journal changed")
	}
	if after := fileSet(t, dir); len(after) != len(before) {
		t.Errorf("files appeared: %v", after)
	}
}

func TestJournalAppendRefusals(t *testing.T) {
	s := newStore(t, Options{})
	appendAll(t, s, 2)
	p := filepath.Join(s.Dir(), domain.EventsPath)
	before := mustRead(t, p)

	cases := []struct {
		name   string
		record []byte
		also   error
	}{
		{"empty", nil, nil},
		{"newline inside", []byte("{\"a\":1}\n{\"b\":2}"), nil},
		{"only a newline", []byte("\n"), nil},
		{"above the limit", bytes.Repeat([]byte("x"), domain.MaxEventSize+1), domain.ErrEventTooLarge},
	}
	for _, tt := range cases {
		err := s.AppendEvent(tt.record)
		if !errors.Is(err, ErrInvalidRecord) {
			t.Errorf("%s: %v, want ErrInvalidRecord", tt.name, err)
		}
		if tt.also != nil && !errors.Is(err, tt.also) {
			t.Errorf("%s: %v, want %v as well", tt.name, err, tt.also)
		}
	}
	if got := mustRead(t, p); !bytes.Equal(got, before) {
		t.Error("a refused record reached the journal")
	}
	// A refusal does not kill the Store.
	if err := s.AppendEvent(testRecord(t, 3)); err != nil {
		t.Fatalf("AppendEvent after refusals: %v", err)
	}
}

func TestJournalNotLoadedAfterCorruptLoad(t *testing.T) {
	s, _ := journalWith(t, journalBytes(testRecord(t, 1), []byte("garbage")))
	if _, err := s.LoadJournal(); !errors.Is(err, ErrJournalCorrupt) {
		t.Fatalf("LoadJournal: %v", err)
	}
	if err := s.AppendEvent(testRecord(t, 2)); !errors.Is(err, ErrJournalNotLoaded) {
		t.Fatalf("AppendEvent after a failed load: %v, want ErrJournalNotLoaded", err)
	}
}

func TestJournalTornTailIsPreservedAndCut(t *testing.T) {
	r1, r2, r3 := testRecord(t, 1), testRecord(t, 2), testRecord(t, 3)
	half := r3[:len(r3)/2]
	complete := journalBytes(r1, r2)
	s, dir := journalWith(t, append(bytes.Clone(complete), half...))

	j, err := s.LoadJournal()
	if err != nil {
		t.Fatal(err)
	}
	if len(j.Events) != 2 {
		t.Fatalf("%d events, want 2", len(j.Events))
	}
	wantName := fmt.Sprintf("logs/events.jsonl.torn-%d", len(complete))
	if j.Recovered == nil || *j.Recovered != (TailRecovery{Offset: int64(len(complete)), Length: int64(len(half)), PreservedAs: wantName}) {
		t.Fatalf("Recovered = %+v, want offset %d length %d as %s", j.Recovered, len(complete), len(half), wantName)
	}
	// The torn bytes are kept byte for byte, the journal is cut to the last
	// complete record.
	if got := mustRead(t, filepath.Join(dir, wantName)); !bytes.Equal(got, half) {
		t.Errorf("preserved %q, want %q", got, half)
	}
	if got := mustRead(t, filepath.Join(dir, domain.EventsPath)); !bytes.Equal(got, complete) {
		t.Errorf("journal holds %q", got)
	}
	wantMode(t, filepath.Join(dir, wantName), 0o600)
	wantMode(t, filepath.Join(dir, domain.LogsDir), 0o700)
	// Appending goes on with the next sequence number.
	if err := s.AppendEvent(r3); err != nil {
		t.Fatal(err)
	}
	j, err = s.LoadJournal()
	if err != nil || len(j.Events) != 3 || j.Recovered != nil || j.Events[2].Seq != 3 {
		t.Fatalf("after the append: %+v, %v", j, err)
	}
	// The preserved file is still there.
	if _, err := os.Lstat(filepath.Join(dir, wantName)); err != nil {
		t.Error(err)
	}
}

func TestJournalTornTailVariants(t *testing.T) {
	r1, r2 := testRecord(t, 1), testRecord(t, 2)
	complete := journalBytes(r1)
	tests := []struct {
		name string
		tail []byte
		keep int // events that survive
	}{
		{"one byte", r2[:1], 1},
		{"complete JSON without its newline", r2, 1},
		{"record minus the last byte", r2[:len(r2)-1], 1},
		{"garbage without a newline", []byte("not json at all"), 1},
		{"NULs without a newline", []byte("\x00\x00\x00\x00"), 1},
		{"NULs then a newline", []byte("\x00\x00\x00\n"), 1},
		{"NULs, a newline, more NULs", []byte("\x00\x00\n\x00\x00\x00"), 1},
		{"NULs and several newlines", []byte("\x00\n\n\x00\n"), 1},
		{"several NUL records", []byte("\x00\x00\n\x00\x00\n"), 1},
		{"NULs then a partial record", []byte("\x00\x00\x00" + string(r2[:5])), 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, dir := journalWith(t, append(bytes.Clone(complete), tt.tail...))
			j, err := s.LoadJournal()
			if err != nil {
				t.Fatal(err)
			}
			if len(j.Events) != tt.keep || j.Recovered == nil {
				t.Fatalf("Journal = %+v", j)
			}
			if j.Recovered.Offset != int64(len(complete)) || j.Recovered.Length != int64(len(tt.tail)) {
				t.Errorf("Recovered = %+v", *j.Recovered)
			}
			if got := mustRead(t, filepath.Join(dir, j.Recovered.PreservedAs)); !bytes.Equal(got, tt.tail) {
				t.Errorf("preserved %q, want %q", got, tt.tail)
			}
			if got := mustRead(t, filepath.Join(dir, domain.EventsPath)); !bytes.Equal(got, complete) {
				t.Errorf("journal holds %q", got)
			}
		})
	}
}

func TestJournalWhollyTornJournal(t *testing.T) {
	for _, raw := range []string{"{\"schemaVersion\":1,\"se", "\x00\x00\x00\x00", "\x00\x00\n"} {
		s, dir := journalWith(t, []byte(raw))
		j, err := s.LoadJournal()
		if err != nil || len(j.Events) != 0 || j.Recovered == nil || j.Recovered.Offset != 0 {
			t.Fatalf("raw %q: %+v, %v", raw, j, err)
		}
		if got := mustRead(t, filepath.Join(dir, j.Recovered.PreservedAs)); string(got) != raw {
			t.Errorf("preserved %q", got)
		}
		if got := mustRead(t, filepath.Join(dir, domain.EventsPath)); len(got) != 0 {
			t.Errorf("journal holds %q", got)
		}
		s.Close()
	}
}

func TestJournalTornTailNameCollision(t *testing.T) {
	r1, r2 := testRecord(t, 1), testRecord(t, 2)
	complete := journalBytes(r1)
	s, dir := journalWith(t, append(bytes.Clone(complete), r2[:10]...))
	base := fmt.Sprintf("logs/events.jsonl.torn-%d", len(complete))
	if err := os.MkdirAll(filepath.Join(dir, "logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(dir, base), []byte("earlier torn bytes"))
	mustWrite(t, filepath.Join(dir, base+"-2"), []byte("even earlier"))
	// A dangling link also holds the name.
	if err := os.Symlink("nowhere", filepath.Join(dir, base+"-3")); err != nil {
		t.Fatal(err)
	}

	j, err := s.LoadJournal()
	if err != nil {
		t.Fatal(err)
	}
	if j.Recovered == nil || j.Recovered.PreservedAs != base+"-4" {
		t.Fatalf("Recovered = %+v, want %s-4", j.Recovered, base)
	}
	if got := mustRead(t, filepath.Join(dir, base)); string(got) != "earlier torn bytes" {
		t.Errorf("an earlier file was overwritten: %q", got)
	}
	if got := mustRead(t, filepath.Join(dir, base+"-2")); string(got) != "even earlier" {
		t.Errorf("an earlier file was overwritten: %q", got)
	}
	if got := mustRead(t, filepath.Join(dir, base+"-4")); !bytes.Equal(got, r2[:10]) {
		t.Errorf("preserved %q", got)
	}
}

func TestJournalTornTailWithSymlinkedLogsDirIsAnError(t *testing.T) {
	r1, r2 := testRecord(t, 1), testRecord(t, 2)
	raw := append(journalBytes(r1), r2[:7]...)
	s, dir := journalWith(t, raw)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "logs")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadJournal(); !errors.Is(err, ErrSymlink) {
		t.Fatalf("LoadJournal: %v, want ErrSymlink", err)
	}
	// Nothing was preserved outside and the journal was not cut.
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("files were written through the link: %v", entries)
	}
	if got := mustRead(t, filepath.Join(dir, domain.EventsPath)); !bytes.Equal(got, raw) {
		t.Error("the journal was cut although the torn bytes could not be preserved")
	}
}

func TestJournalCorruptNewlineTerminatedLastRecord(t *testing.T) {
	r1, r2 := testRecord(t, 1), testRecord(t, 2)
	for name, last := range map[string][]byte{
		"garbage":        []byte("this is not json"),
		"truncated JSON": r2[:len(r2)/2],
		"empty":          nil,
		"wrong seq":      testRecord(t, 5),
	} {
		t.Run(name, func(t *testing.T) {
			raw := journalBytes(r1, last)
			s, dir := journalWith(t, raw)
			before := fileSet(t, dir)
			_, err := s.LoadJournal()
			var je *JournalError
			if !errors.Is(err, ErrJournalCorrupt) || !errors.As(err, &je) {
				t.Fatalf("LoadJournal: %v, want a *JournalError matching ErrJournalCorrupt", err)
			}
			if je.Line != 2 || je.Offset != int64(len(r1)+1) {
				t.Errorf("JournalError line %d offset %d, want line 2 offset %d", je.Line, je.Offset, len(r1)+1)
			}
			if !strings.Contains(err.Error(), "line 2") || !strings.Contains(err.Error(), fmt.Sprint(len(r1)+1)) {
				t.Errorf("the message does not name the location: %v", err)
			}
			// The file is byte-identical and nothing was preserved.
			if got := mustRead(t, filepath.Join(dir, domain.EventsPath)); !bytes.Equal(got, raw) {
				t.Error("the journal was modified")
			}
			if after := fileSet(t, dir); len(after) != len(before) {
				t.Errorf("files appeared: %v", after)
			}
		})
	}
}

func TestJournalCorruptMiddle(t *testing.T) {
	r := func(seq uint64) []byte { return testRecord(t, seq) }
	other := domain.RunID("run-fedcba9876543210")
	tests := []struct {
		name   string
		raw    []byte
		line   int
		offset int
		also   error
	}{
		{"garbage in the middle", journalBytes(r(1), []byte("garbage"), r(3)), 2, len(r(1)) + 1, nil},
		{"sequence gap", journalBytes(r(1), r(2), r(4)), 3, len(r(1)) + len(r(2)) + 2, nil},
		{"repeated sequence number", journalBytes(r(1), r(2), r(2)), 3, len(r(1)) + len(r(2)) + 2, nil},
		{"journal starts at 2", journalBytes(r(2), r(3)), 1, 0, nil},
		{"sequence goes backwards", journalBytes(r(1), r(2), r(3), r(2)), 4, len(r(1)) + len(r(2)) + len(r(3)) + 3, nil},
		{"record of another Run", journalBytes(r(1), recordFor(t, other, 2), r(3)), 2, len(r(1)) + 1, nil},
		{"empty line in the middle", journalBytes(r(1), nil, r(2)), 2, len(r(1)) + 1, nil},
		{"empty line at the end", journalBytes(r(1), nil), 2, len(r(1)) + 1, nil},
		{"blank line", journalBytes(r(1), []byte("  "), r(2)), 2, len(r(1)) + 1, nil},
		{"NUL record followed by data", journalBytes(r(1), []byte("\x00\x00"), r(2)), 2, len(r(1)) + 1, nil},
		{"oversize line in the middle", journalBytes(r(1), bytes.Repeat([]byte("x"), domain.MaxEventSize+10), r(2)), 2, len(r(1)) + 1, domain.ErrEventTooLarge},
		{"oversize line at the end", journalBytes(r(1), bytes.Repeat([]byte("x"), domain.MaxEventSize+10)), 2, len(r(1)) + 1, domain.ErrEventTooLarge},
		{"unknown event type", journalBytes(r(1), bytes.Replace(r(2), []byte(`"ManifestFrozen"`), []byte(`"NoSuchEvent"`), 1)), 2, len(r(1)) + 1, domain.ErrUnknownEventType},
		{"unknown member", journalBytes(r(1), bytes.Replace(r(2), []byte(`"payload"`), []byte(`"extra":1,"payload"`), 1)), 2, len(r(1)) + 1, domain.ErrInvalidEvent},
		{"two records on one line", journalBytes(append(append(bytes.Clone(r(1)), ' '), r(2)...)), 1, 0, domain.ErrInvalidEvent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, dir := journalWith(t, tt.raw)
			before := fileSet(t, dir)
			_, err := s.LoadJournal()
			var je *JournalError
			if !errors.Is(err, ErrJournalCorrupt) || !errors.As(err, &je) {
				t.Fatalf("LoadJournal: %v, want ErrJournalCorrupt", err)
			}
			if je.Line != tt.line || je.Offset != int64(tt.offset) {
				t.Errorf("line %d offset %d, want line %d offset %d (%v)", je.Line, je.Offset, tt.line, tt.offset, err)
			}
			if tt.also != nil && !errors.Is(err, tt.also) {
				t.Errorf("%v does not match %v", err, tt.also)
			}
			if errors.Is(err, domain.ErrUnsupportedEventSchema) {
				t.Errorf("%v matches ErrUnsupportedEventSchema", err)
			}
			if got := mustRead(t, filepath.Join(dir, domain.EventsPath)); !bytes.Equal(got, tt.raw) {
				t.Error("the journal was modified")
			}
			if after := fileSet(t, dir); len(after) != len(before) {
				t.Errorf("files appeared: %v", after)
			}
		})
	}
}

// A corrupt record in the middle is an error even when the file also ends in
// a torn record, and nothing is recovered then.
func TestJournalCorruptMiddleWithTornTail(t *testing.T) {
	r1, r3 := testRecord(t, 1), testRecord(t, 3)
	raw := append(journalBytes(r1, []byte("garbage")), r3[:9]...)
	s, dir := journalWith(t, raw)
	if _, err := s.LoadJournal(); !errors.Is(err, ErrJournalCorrupt) {
		t.Fatalf("LoadJournal: %v", err)
	}
	if got := mustRead(t, filepath.Join(dir, domain.EventsPath)); !bytes.Equal(got, raw) {
		t.Error("the journal was modified")
	}
	if _, err := os.Lstat(filepath.Join(dir, domain.LogsDir)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("logs/ appeared: %v", err)
	}
}

func TestJournalUnsupportedSchemaIsIncompatibleNotCorrupt(t *testing.T) {
	r1, r2 := testRecord(t, 1), testRecord(t, 2)
	newer := bytes.Replace(r2, []byte(`"schemaVersion":1`), []byte(`"schemaVersion":2`), 1)
	if bytes.Equal(newer, r2) {
		t.Fatal("the test record does not carry schemaVersion 1 as expected")
	}
	for name, raw := range map[string][]byte{
		"last record":     journalBytes(r1, newer),
		"middle record":   journalBytes(r1, newer, testRecord(t, 3)),
		"unparseable too": journalBytes(r1, []byte(`{"schemaVersion":2,"whatever":true}`)),
	} {
		t.Run(name, func(t *testing.T) {
			s, dir := journalWith(t, raw)
			_, err := s.LoadJournal()
			if !errors.Is(err, domain.ErrUnsupportedEventSchema) {
				t.Fatalf("LoadJournal: %v, want ErrUnsupportedEventSchema", err)
			}
			if errors.Is(err, ErrJournalCorrupt) {
				t.Errorf("%v matches ErrJournalCorrupt", err)
			}
			if !strings.Contains(err.Error(), "line 2") {
				t.Errorf("the message does not name the line: %v", err)
			}
			if got := mustRead(t, filepath.Join(dir, domain.EventsPath)); !bytes.Equal(got, raw) {
				t.Error("the journal was modified")
			}
		})
	}
}

// requireJournalUntouched checks that a refused load changed nothing: the
// journal holds exactly raw and nothing was copied under logs/.
func requireJournalUntouched(t *testing.T, dir string, raw []byte) {
	t.Helper()
	if got := mustRead(t, filepath.Join(dir, domain.EventsPath)); !bytes.Equal(got, raw) {
		t.Errorf("the journal changed: %d bytes, want %d", len(got), len(raw))
	}
	if _, err := os.Lstat(filepath.Join(dir, domain.LogsDir)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("logs/ exists after the refusal: %v", err)
	}
}

// TestJournalOversizeRecordIsCorrupt is R-PER-05 and R-WS-06: a record that
// ends with a newline is at most domain.MaxEventSize bytes long, the newline
// excluded, and a longer one is corruption, in the middle of the journal or at
// its end. So is a trailing region without a newline of more than
// domain.MaxEventSize+1 bytes (one maximum record and its newline; see
// TestJournalOversizeTailIsCorrupt for the region that is recovered). Nothing
// is cut or copied. A record of exactly domain.MaxEventSize bytes is still read
// as a record.
func TestJournalOversizeRecordIsCorrupt(t *testing.T) {
	r1, r2 := testRecord(t, 1), testRecord(t, 2)
	over := bytes.Repeat([]byte("x"), domain.MaxEventSize+1)     // too long for a record
	nuls := bytes.Repeat([]byte{0}, domain.MaxEventSize+1)       // too long for a record
	overTorn := bytes.Repeat([]byte("x"), domain.MaxEventSize+2) // too long for a tail
	nulsTorn := bytes.Repeat([]byte{0}, domain.MaxEventSize+2)   // too long for a tail
	tests := []struct {
		name string
		raw  []byte
		line int
	}{
		{"oversize record in the middle", journalBytes(r1, over, r2), 2},
		{"oversize record at the end, newline-terminated", journalBytes(r1, over), 2},
		{"oversize record at the end, torn", append(journalBytes(r1), overTorn...), 2},
		{"oversize first record", journalBytes(over, r1), 1},
		{"oversize torn journal", overTorn, 1},
		{"oversize NUL record in the middle", journalBytes(r1, nuls, r2), 2},
		{"oversize NUL record at the end, newline-terminated", journalBytes(r1, nuls), 2},
		{"oversize NUL torn tail", append(journalBytes(r1), nulsTorn...), 2},
		{"much more than the limit", append(journalBytes(r1), bytes.Repeat([]byte("x"), 5*domain.MaxEventSize+3)...), 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, dir := journalWith(t, tt.raw)
			_, err := s.LoadJournal()
			var je *JournalError
			if !errors.Is(err, ErrJournalCorrupt) || !errors.As(err, &je) {
				t.Fatalf("LoadJournal: %v, want a *JournalError matching ErrJournalCorrupt", err)
			}
			if !errors.Is(err, domain.ErrEventTooLarge) {
				t.Errorf("%v does not match domain.ErrEventTooLarge", err)
			}
			wantOffset := int64(0)
			if tt.line == 2 {
				wantOffset = int64(len(r1) + 1)
			}
			if je.Line != tt.line || je.Offset != wantOffset {
				t.Errorf("line %d offset %d, want line %d offset %d", je.Line, je.Offset, tt.line, wantOffset)
			}
			requireJournalUntouched(t, dir, tt.raw)
			if err := s.AppendEvent(testRecord(t, 3)); !errors.Is(err, ErrJournalNotLoaded) {
				t.Errorf("AppendEvent after a refused load: %v, want ErrJournalNotLoaded", err)
			}
		})
	}

	t.Run("a record of exactly the limit is read as a record", func(t *testing.T) {
		raw := journalBytes(r1, bytes.Repeat([]byte("x"), domain.MaxEventSize), r2)
		s, dir := journalWith(t, raw)
		_, err := s.LoadJournal()
		var je *JournalError
		if !errors.Is(err, ErrJournalCorrupt) || !errors.As(err, &je) || je.Line != 2 {
			t.Fatalf("LoadJournal: %v, want corruption at line 2", err)
		}
		// Not for its size: the record is invalid JSON.
		if errors.Is(err, domain.ErrEventTooLarge) {
			t.Errorf("a record of exactly %d bytes is reported as too large: %v", domain.MaxEventSize, err)
		}
		requireJournalUntouched(t, dir, raw)
	})
}

// TestJournalOversizeTailIsCorrupt: an incomplete trailing region (a torn
// record, or NUL bytes and newlines) is recovered only up to
// domain.MaxEventSize+1 bytes in total, one maximum record and its newline,
// which is all that a crash during an append can leave. So the bytes that
// LoadJournal copies under logs/ never exceed that. One byte more is
// corruption and changes nothing.
func TestJournalOversizeTailIsCorrupt(t *testing.T) {
	r1 := testRecord(t, 1)
	complete := journalBytes(r1)
	const limit = domain.MaxEventSize + 1
	repeat := func(b byte, n int) []byte { return bytes.Repeat([]byte{b}, n) }
	join := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

	tails := []struct {
		name string
		at   []byte // exactly the limit
		over []byte // one byte more
	}{
		{"torn record", repeat('x', limit), repeat('x', limit+1)},
		{"NUL bytes without a newline", repeat(0, limit), repeat(0, limit+1)},
		{"NUL bytes with a newline in the middle",
			join(repeat(0, limit/2), []byte("\n"), repeat(0, limit-limit/2-1)),
			join(repeat(0, limit/2), []byte("\n"), repeat(0, limit-limit/2))},
		{"NUL bytes ending in a newline", join(repeat(0, limit-1), []byte("\n")), join(repeat(0, limit), []byte("\n"))},
		{"newlines among NUL bytes", join(repeat(0, 3), repeat('\n', limit-6), repeat(0, 3)), join(repeat(0, 3), repeat('\n', limit-5), repeat(0, 3))},
	}
	for _, tt := range tails {
		t.Run(tt.name+" at the limit is recovered", func(t *testing.T) {
			if len(tt.at) != limit {
				t.Fatalf("the test tail has %d bytes, want %d", len(tt.at), limit)
			}
			s, dir := journalWith(t, append(bytes.Clone(complete), tt.at...))
			j, err := s.LoadJournal()
			if err != nil {
				t.Fatal(err)
			}
			if len(j.Events) != 1 || j.Recovered == nil || j.Recovered.Offset != int64(len(complete)) || j.Recovered.Length != limit {
				t.Fatalf("Journal = %+v (recovered %+v)", j, j.Recovered)
			}
			if got := mustRead(t, filepath.Join(dir, j.Recovered.PreservedAs)); !bytes.Equal(got, tt.at) {
				t.Errorf("the preserved file differs from the tail (%d bytes)", len(got))
			}
			if got := mustRead(t, filepath.Join(dir, domain.EventsPath)); !bytes.Equal(got, complete) {
				t.Errorf("the journal holds %d bytes, want %d", len(got), len(complete))
			}
		})
		t.Run(tt.name+" above the limit is corrupt", func(t *testing.T) {
			if len(tt.over) != limit+1 {
				t.Fatalf("the test tail has %d bytes, want %d", len(tt.over), limit+1)
			}
			raw := append(bytes.Clone(complete), tt.over...)
			s, dir := journalWith(t, raw)
			_, err := s.LoadJournal()
			var je *JournalError
			if !errors.Is(err, ErrJournalCorrupt) || !errors.As(err, &je) {
				t.Fatalf("LoadJournal: %v, want a *JournalError matching ErrJournalCorrupt", err)
			}
			if je.Line != 2 || je.Offset != int64(len(complete)) {
				t.Errorf("line %d offset %d, want line 2 offset %d", je.Line, je.Offset, len(complete))
			}
			requireJournalUntouched(t, dir, raw)
		})
	}

	t.Run("a torn real record is still recovered", func(t *testing.T) {
		r2 := testRecord(t, 2)
		s, _ := journalWith(t, append(bytes.Clone(complete), r2[:len(r2)-1]...))
		if j, err := s.LoadJournal(); err != nil || len(j.Events) != 1 || j.Recovered == nil {
			t.Fatalf("LoadJournal = %+v, %v", j, err)
		}
	})
}

// TestJournalOversizeRegionIsNotReadWhole is R-WS-06: LoadJournal reads the
// journal incrementally, so a journal that ends in a huge damaged region costs
// a bounded amount of memory, not the size of the file.
func TestJournalOversizeRegionIsNotReadWhole(t *testing.T) {
	r1 := testRecord(t, 1)
	s := newStore(t, Options{})
	dir := s.Dir()
	s.Close()
	const regionSize = 16 << 20
	f, err := os.OpenFile(filepath.Join(dir, domain.EventsPath), os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(journalBytes(r1)); err != nil {
		t.Fatal(err)
	}
	chunk := bytes.Repeat([]byte("x"), 1<<20)
	for range regionSize >> 20 {
		if _, err := f.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	s = reopen(t, dir, Options{})

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err = s.LoadJournal()
	runtime.ReadMemStats(&after)
	if !errors.Is(err, ErrJournalCorrupt) {
		t.Fatalf("LoadJournal: %v, want ErrJournalCorrupt", err)
	}
	// The read buffer is about 64 KiB; reading the whole region would allocate
	// at least regionSize.
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > regionSize/4 {
		t.Errorf("LoadJournal allocated %d bytes for a journal that ends in %d damaged bytes", allocated, regionSize)
	}
}

// TestJournalEventsDoNotAliasTheReadBuffer: the decoded events stay valid
// after the read buffer is refilled, which happens for a journal longer than
// the buffer.
func TestJournalEventsDoNotAliasTheReadBuffer(t *testing.T) {
	s := newStore(t, Options{})
	const n = 600
	appendAll(t, s, n)
	if size := len(mustRead(t, filepath.Join(s.Dir(), domain.EventsPath))); size <= domain.MaxEventSize+1 {
		t.Fatalf("the journal has %d bytes; the test needs more than one read buffer (%d bytes)", size, domain.MaxEventSize+1)
	}
	j, err := s.LoadJournal()
	if err != nil || len(j.Events) != n {
		t.Fatalf("LoadJournal: %d events, %v", len(j.Events), err)
	}
	for i, ev := range j.Events {
		again, err := domain.EncodeEvent(ev)
		if err != nil || !bytes.Equal(again, testRecord(t, uint64(i+1))) {
			t.Fatalf("event %d does not re-encode to its record: %v", i+1, err)
		}
	}
}

func TestJournalLargeJournal(t *testing.T) {
	s := newStore(t, Options{})
	const n = 300
	appendAll(t, s, n)
	j, err := s.LoadJournal()
	if err != nil || len(j.Events) != n || j.Events[n-1].Seq != n {
		t.Fatalf("LoadJournal: %d events, %v", len(j.Events), err)
	}
}

// A failed write of the journal leaves it in an unknown state, so the Store
// stops.
func TestJournalWriteFailureKillsTheStore(t *testing.T) {
	s := newStore(t, Options{})
	appendAll(t, s, 1)
	if err := s.journal.Close(); err != nil { // the next write fails
		t.Fatal(err)
	}
	if err := s.AppendEvent(testRecord(t, 2)); err == nil || errors.Is(err, ErrStoreDead) {
		t.Fatalf("AppendEvent on a closed descriptor: %v, want the write error", err)
	}
	if err := s.AppendEvent(testRecord(t, 2)); !errors.Is(err, ErrStoreDead) {
		t.Fatalf("second AppendEvent: %v, want ErrStoreDead", err)
	}
	if _, err := s.WriteArtifact("a.json", nil); !errors.Is(err, ErrStoreDead) {
		t.Fatalf("WriteArtifact: %v, want ErrStoreDead", err)
	}
	s.Abandon()
}
