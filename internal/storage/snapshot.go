package storage

import (
	"fmt"

	"github.com/shutx-net/bellona/internal/domain"
)

// WriteSnapshot replaces state.json atomically: a temporary file, fsync,
// rename over state.json, and an fsync of the Run directory. The snapshot is
// a cache of the journal, so replacing it is the one rewrite that the Store
// allows. data must be at most MaxSnapshotSize bytes (ErrArtifactTooLarge).
// The Store does not interpret the bytes; projection.EncodeSnapshot makes
// them and projection.DecodeSnapshot reads them.
func (s *Store) WriteSnapshot(data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return err
	}
	if len(data) > MaxSnapshotSize {
		return &PathError{Path: domain.SnapshotPath, Err: fmt.Errorf("%w: %d bytes exceed the limit of %d",
			ErrArtifactTooLarge, len(data), MaxSnapshotSize)}
	}
	return s.replace(domain.SnapshotPath, data, finalizePoints{
		tempSynced: CrashSnapshotTempSynced,
		renamed:    CrashSnapshotRenamed,
	})
}

// ReadSnapshot returns the bytes of state.json. An absent file yields an
// error that matches fs.ErrNotExist; a symbolic link yields ErrSymlink, a
// file that is not a regular file ErrNotRegular, and a file above
// MaxSnapshotSize ErrArtifactTooLarge.
func (s *Store) ReadSnapshot() ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.usable(); err != nil {
		return nil, err
	}
	return s.readRegular(domain.SnapshotPath, MaxSnapshotSize, -1)
}
