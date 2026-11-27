package storage

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"

	"github.com/shutx-net/bellona/internal/domain"
)

// JournalError locates a corrupt record of events.jsonl. Line is 1-based and
// Offset is the byte offset of the first byte of the record. It matches
// ErrJournalCorrupt and unwraps to the reason.
type JournalError struct {
	Line   int
	Offset int64
	Err    error
}

func (e *JournalError) Error() string {
	return fmt.Sprintf("storage: %s line %d (byte offset %d): %v", domain.EventsPath, e.Line, e.Offset, e.Err)
}
func (e *JournalError) Unwrap() []error { return []error{ErrJournalCorrupt, e.Err} }

// TailRecovery describes an incomplete trailing journal record that
// LoadJournal cut off. Offset is where the record started (the journal is now
// that long), Length the number of bytes that were removed, and PreservedAs
// the Run-relative path of the file that keeps them.
type TailRecovery struct {
	Offset      int64
	Length      int64
	PreservedAs string
}

// Journal is the result of LoadJournal: every event in order, and the
// recovery that happened, if any.
type Journal struct {
	Events    []domain.Event
	Recovered *TailRecovery
}

// AppendEvent appends one encoded event record (domain.EncodeEvent) to
// events.jsonl and returns after it is fsynced. The record plus a newline is
// written with a single write on the O_APPEND descriptor. The journal must
// have been loaded in this session: CreateRun starts with an empty one, and
// after OpenRun LoadJournal must succeed first (ErrJournalNotLoaded
// otherwise). A record that is empty, contains a newline or exceeds
// domain.MaxEventSize is refused with ErrInvalidRecord. Whether the record is
// the next legal event is the reducer's business: the engine applies the
// event to the state first and appends it only if that succeeds.
//
// A failed or short write or a failed fsync leaves the journal in an unknown
// state, so the Store is dead afterwards (ErrStoreDead); the next OpenRun
// and LoadJournal find out what reached the file.
func (s *Store) AppendEvent(record []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return err
	}
	if !s.loaded {
		return ErrJournalNotLoaded
	}
	switch {
	case len(record) == 0:
		return fmt.Errorf("%w: the record is empty", ErrInvalidRecord)
	case len(record) > domain.MaxEventSize:
		return fmt.Errorf("%w: %w: %d bytes, the limit is %d",
			ErrInvalidRecord, domain.ErrEventTooLarge, len(record), domain.MaxEventSize)
	case bytes.IndexByte(record, '\n') >= 0:
		return fmt.Errorf("%w: the record contains a newline", ErrInvalidRecord)
	}
	if err := s.fire(CrashEventTorn); err != nil {
		// A write cut short: the first half of the record, no newline.
		_, _ = s.journal.Write(record[:len(record)/2])
		return err
	}
	line := make([]byte, 0, len(record)+1)
	line = append(append(line, record...), '\n')
	if n, err := s.journal.Write(line); err != nil || n != len(line) {
		s.dead = true
		if err == nil {
			err = io.ErrShortWrite
		}
		return fmt.Errorf("storage: append to %s (%d of %d bytes written): %w", domain.EventsPath, n, len(line), err)
	}
	if err := s.fire(CrashEventWritten); err != nil {
		return err
	}
	if err := s.journal.Sync(); err != nil {
		s.dead = true
		return fmt.Errorf("storage: sync %s: %w", domain.EventsPath, err)
	}
	return s.fire(CrashEventSynced)
}

// LoadJournal reads events.jsonl from the start and validates it: every
// record must decode with domain.DecodeEvent, the sequence numbers must run
// 1, 2, 3 and so on without a gap or a repeat, and every record must belong
// to the Run that the directory is named after. The decoded events are
// returned in order and the journal counts as loaded, so AppendEvent works.
//
// The file is read record by record. At most one record and a read buffer of
// domain.MaxEventSize+2 bytes are held at a time, besides the events that are
// returned, so a damaged or hostile file cannot make LoadJournal read it whole
// (R-WS-06). A record that ends with a newline is at most domain.MaxEventSize
// bytes long, the newline excluded; a longer one is corruption.
//
// A record that fails is an error that wraps ErrJournalCorrupt (a
// *JournalError with its line and byte offset), and nothing is modified. The
// one exception is a record of an unsupported event schema, whose error wraps
// domain.ErrUnsupportedEventSchema and does not match ErrJournalCorrupt: the
// journal is from another version, not damaged.
//
// An incomplete trailing record is recovered, which is safe because the Store
// holds the Run lock. It is the bytes after the last newline, or a trailing
// region that consists only of NUL bytes (at least one) and newlines, which
// is how a crash can leave a file whose size was updated before its data
// reached the disk. The exact bytes are first preserved in
// logs/events.jsonl.torn-<offset> (with -2, -3 and so on appended when that
// name is taken), then the journal is truncated to the end of the last
// complete record and fsynced. Journal.Recovered reports it. An incomplete
// trailing region is at most domain.MaxEventSize+1 bytes in total, which is
// one record of the largest size and its newline: that is all that an append
// can have left behind, torn or zero-filled. A longer region is corruption, so
// what is copied under logs/ is never larger. A record that ends with a newline
// is complete: if it is invalid, that is corruption, not a torn record.
func (s *Store) LoadJournal() (Journal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return Journal{}, err
	}
	fi, err := s.journal.Stat()
	if err != nil {
		return Journal{}, &PathError{Path: domain.EventsPath, Err: err}
	}
	size := fi.Size()
	// A trailing region of maxTailSize bytes leaves room for one more byte in the
	// buffer, so a line that does not fit is longer than any recoverable tail.
	br := bufio.NewReaderSize(io.NewSectionReader(s.journal, 0, size), maxTailSize+1)

	var (
		events []domain.Event
		pos    int64  // byte offset of the record that is read next
		tail   []byte // the incomplete trailing region, from pos
	)
	for pos < size {
		line := len(events) + 1
		corrupt := func(reason error) error { return &JournalError{Line: line, Offset: pos, Err: reason} }
		// record aliases the buffer of br until the next read.
		record, err := br.ReadSlice('\n')
		switch {
		case err == nil:
			record = record[:len(record)-1] // the record without its newline
			if len(record) > domain.MaxEventSize {
				return Journal{}, corrupt(errRecordTooLong)
			}
		case errors.Is(err, bufio.ErrBufferFull):
			return Journal{}, corrupt(errRecordTooLong)
		case errors.Is(err, io.EOF) && len(record) > 0:
			tail = bytes.Clone(record) // bytes after the last newline
		default:
			if errors.Is(err, io.EOF) { // the file is shorter than its size said
				err = io.ErrUnexpectedEOF
			}
			return Journal{}, &PathError{Path: domain.EventsPath, Err: err}
		}
		if tail != nil {
			break
		}
		next := pos + int64(len(record)) + 1
		if len(record) > 0 && onlyNUL(record) {
			// A record of NUL bytes is the start of a recoverable tail when the
			// journal holds nothing but NUL bytes and newlines from here on.
			if size-pos > maxTailSize {
				return Journal{}, corrupt(fmt.Errorf("a record of NUL bytes with %d bytes of journal after its start; an incomplete trailing region has at most %d",
					size-pos, maxTailSize))
			}
			region := make([]byte, size-pos)
			if _, err := s.journal.ReadAt(region, pos); err != nil {
				return Journal{}, &PathError{Path: domain.EventsPath, Err: err}
			}
			if onlyNULOrNewline(region) {
				tail = region
				break
			}
		}
		ev, err := domain.DecodeEvent(record)
		if err != nil {
			if errors.Is(err, domain.ErrUnsupportedEventSchema) {
				return Journal{}, fmt.Errorf("storage: %s line %d (byte offset %d): %w", domain.EventsPath, line, pos, err)
			}
			return Journal{}, corrupt(err)
		}
		if want := uint64(len(events)) + 1; ev.Seq != want {
			return Journal{}, corrupt(fmt.Errorf("sequence number %d, want %d", ev.Seq, want))
		}
		if ev.RunID != s.runID {
			return Journal{}, corrupt(fmt.Errorf("the record belongs to %s, this Run is %s", ev.RunID, s.runID))
		}
		events = append(events, ev)
		pos = next
	}

	out := Journal{Events: events}
	if tail != nil {
		rec, err := s.recoverTail(tail, pos)
		if err != nil {
			return Journal{}, err
		}
		out.Recovered = rec
	}
	s.loaded = true
	return out, nil
}

// maxTailSize is the size of the largest incomplete trailing region that
// LoadJournal recovers: a record of domain.MaxEventSize bytes and its newline.
const maxTailSize = domain.MaxEventSize + 1

// errRecordTooLong is the reason of a record that is longer than
// domain.MaxEventSize, or of a trailing region that is longer than
// maxTailSize, whether or not a newline follows. It matches
// domain.ErrInvalidEvent and domain.ErrEventTooLarge, like the error of
// domain.DecodeEvent for such a record.
var errRecordTooLong = fmt.Errorf("%w: %w: the record is longer than %d bytes",
	domain.ErrInvalidEvent, domain.ErrEventTooLarge, domain.MaxEventSize)

// onlyNUL reports whether every byte of b is NUL.
func onlyNUL(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// onlyNULOrNewline reports whether every byte of b is NUL or a newline.
func onlyNULOrNewline(b []byte) bool {
	for _, c := range b {
		if c != 0 && c != '\n' {
			return false
		}
	}
	return true
}

// recoverTail preserves the incomplete trailing record tail, which starts at
// offset, and then cuts the journal back to offset. This is the only place
// where the module truncates a file, and it does so only after the exact
// bytes are durable under logs/. LoadJournal passes at most maxTailSize
// bytes. The caller holds the write lock and the Run lock
// is held by the Store.
func (s *Store) recoverTail(tail []byte, offset int64) (*TailRecovery, error) {
	if err := s.ensureDirs(domain.LogsDir); err != nil {
		return nil, err
	}
	base := fmt.Sprintf("%s/%s.torn-%d", domain.LogsDir, domain.EventsPath, offset)
	name := base
	for n := 2; ; n++ {
		_, err := s.root.Lstat(name)
		if errors.Is(err, fs.ErrNotExist) {
			break
		}
		if err != nil {
			return nil, &PathError{Path: name, Err: err}
		}
		name = fmt.Sprintf("%s-%d", base, n)
	}
	if err := s.writeNew(name, tail, finalizePoints{}); err != nil {
		return nil, err
	}
	if err := s.journal.Truncate(offset); err != nil {
		s.dead = true
		return nil, fmt.Errorf("storage: cut %s back to %d bytes: %w", domain.EventsPath, offset, err)
	}
	if err := s.journal.Sync(); err != nil {
		s.dead = true
		return nil, fmt.Errorf("storage: sync %s: %w", domain.EventsPath, err)
	}
	return &TailRecovery{Offset: offset, Length: int64(len(tail)), PreservedAs: name}, nil
}
