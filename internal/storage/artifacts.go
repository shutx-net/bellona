package storage

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"syscall"

	"github.com/shutx-net/bellona/internal/domain"
)

// WriteArtifact finalizes data as the artifact at the Run-relative path rel
// (SPEC §12.2): a temporary file in the target directory, fsync, atomic
// rename, and an fsync of the directory (and of the parents of directories
// that it created). The caller appends the event that refers to the returned
// reference only after WriteArtifact returns (R-PER-04).
//
// The path must pass domain.ValidateRunPath (an error wrapping
// domain.ErrInvalidPath otherwise), must not be reserved (ErrReservedPath),
// and data must not exceed the size limit (ErrArtifactTooLarge). Finalized
// artifacts are immutable: when rel already holds identical bytes the
// existing reference is returned without writing (the file and its
// directories are fsynced again, in case an earlier call crashed before it
// did), and when it holds anything else the error wraps ErrArtifactExists. A
// symbolic link at any component fails with ErrSymlink.
func (s *Store) WriteArtifact(rel string, data []byte) (domain.ArtifactRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.usable(); err != nil {
		return domain.ArtifactRef{}, err
	}
	if err := domain.ValidateRunPath(rel); err != nil {
		return domain.ArtifactRef{}, err
	}
	if domain.IsReservedPath(rel) {
		return domain.ArtifactRef{}, &PathError{Path: rel, Err: ErrReservedPath}
	}
	if int64(len(data)) > s.maxArtifactSize() {
		return domain.ArtifactRef{}, &PathError{Path: rel, Err: fmt.Errorf("%w: %d bytes exceed the limit of %d",
			ErrArtifactTooLarge, len(data), s.maxArtifactSize())}
	}
	ref, err := domain.NewArtifactRef(rel, data)
	if err != nil {
		return domain.ArtifactRef{}, err
	}
	fi, err := s.lstatNoFollow(rel)
	switch {
	case err == nil:
		if err := s.confirmIdentical(rel, fi, data); err != nil {
			return domain.ArtifactRef{}, err
		}
		return ref, nil
	case !errors.Is(err, fs.ErrNotExist):
		return domain.ArtifactRef{}, err
	}
	err = s.writeNew(rel, data, finalizePoints{
		tempSynced: CrashArtifactTempSynced,
		renamed:    CrashArtifactRenamed,
		dirSynced:  CrashArtifactDirSynced,
	})
	if err != nil {
		return domain.ArtifactRef{}, err
	}
	return ref, nil
}

// confirmIdentical checks that the existing file rel, described by fi, is a
// regular file with exactly the bytes of data, and syncs it and its
// directories. Any other content is ErrArtifactExists. The caller holds the
// write lock.
func (s *Store) confirmIdentical(rel string, fi fs.FileInfo, data []byte) error {
	differs := func(why string) error {
		return &PathError{Path: rel, Err: fmt.Errorf("%w: %s", ErrArtifactExists, why)}
	}
	if !fi.Mode().IsRegular() {
		return differs("it is not a regular file")
	}
	if fi.Size() != int64(len(data)) {
		return differs("it has a different size")
	}
	f, err := s.root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return &PathError{Path: rel, Err: err}
	}
	defer f.Close()
	if fi2, err := f.Stat(); err != nil {
		return &PathError{Path: rel, Err: err}
	} else if !fi2.Mode().IsRegular() || fi2.Size() != int64(len(data)) {
		return differs("it changed while it was read")
	}
	existing, err := io.ReadAll(io.LimitReader(f, int64(len(data))+1))
	if err != nil {
		return &PathError{Path: rel, Err: err}
	}
	if !bytes.Equal(existing, data) {
		return differs("it has different content")
	}
	if err := f.Sync(); err != nil {
		return &PathError{Path: rel, Err: err}
	}
	return s.fsyncAncestors(rel)
}

// ReadArtifact loads the artifact that ref refers to and verifies it. It
// rejects an invalid reference (errors from ref.Validate), a reserved path
// (ErrReservedPath), a symbolic link at any component of the path
// (ErrSymlink), a file that is not a regular file (ErrNotRegular), a missing
// file (ErrArtifactMissing, which also matches fs.ErrNotExist), a size above
// the limit (ErrArtifactTooLarge), and a size or SHA-256 that differs from
// the reference (ErrArtifactMismatch).
func (s *Store) ReadArtifact(ref domain.ArtifactRef) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.usable(); err != nil {
		return nil, err
	}
	return s.readArtifact(ref)
}

// VerifyArtifacts reads every referenced artifact as ReadArtifact does and
// returns the first failure, or nil.
func (s *Store) VerifyArtifacts(refs []domain.ArtifactRef) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.usable(); err != nil {
		return err
	}
	for _, ref := range refs {
		if _, err := s.readArtifact(ref); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) readArtifact(ref domain.ArtifactRef) ([]byte, error) {
	if err := ref.Validate(); err != nil {
		return nil, err
	}
	if domain.IsReservedPath(ref.Path) {
		return nil, &PathError{Path: ref.Path, Err: ErrReservedPath}
	}
	data, err := s.readRegular(ref.Path, s.maxArtifactSize(), ref.Size)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return nil, &PathError{Path: ref.Path, Err: fmt.Errorf("%w: %w", ErrArtifactMissing, fs.ErrNotExist)}
		}
		return nil, err
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != ref.SHA256 {
		return nil, &PathError{Path: ref.Path, Err: fmt.Errorf("%w: sha256 is %s, the reference says %s",
			ErrArtifactMismatch, got, ref.SHA256)}
	}
	return data, nil
}

// lstatNoFollow describes the file at the Run-relative path rel without
// following a symbolic link at any component: every prefix of the path is
// examined with Lstat, so a link that points inside the Run directory is
// rejected as well. A missing component yields an error that matches
// fs.ErrNotExist, a link ErrSymlink, and a component above the last one that
// is not a directory syscall.ENOTDIR.
func (s *Store) lstatNoFollow(rel string) (fs.FileInfo, error) {
	segs := strings.Split(rel, "/")
	cur := ""
	var fi fs.FileInfo
	for i, seg := range segs {
		cur = path.Join(cur, seg)
		var err error
		fi, err = s.root.Lstat(cur)
		switch {
		case err != nil:
			return nil, &PathError{Path: cur, Err: err}
		case fi.Mode()&fs.ModeSymlink != 0:
			return nil, &PathError{Path: cur, Err: ErrSymlink}
		case i < len(segs)-1 && !fi.IsDir():
			return nil, &PathError{Path: cur, Err: syscall.ENOTDIR}
		}
	}
	return fi, nil
}

// readRegular reads the whole file at rel. It requires a regular file no
// larger than limit and, when wantSize is not negative, exactly wantSize
// bytes. Nothing is opened before Lstat has shown a regular file, so a FIFO
// or a device cannot block it. The descriptor is opened non-blocking and
// examined again, to catch a file that was swapped in after the check.
func (s *Store) readRegular(rel string, limit, wantSize int64) ([]byte, error) {
	fi, err := s.lstatNoFollow(rel)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, &PathError{Path: rel, Err: fmt.Errorf("%w: mode %s", ErrNotRegular, fi.Mode().Type())}
	}
	if fi.Size() > limit {
		return nil, &PathError{Path: rel, Err: fmt.Errorf("%w: %d bytes exceed the limit of %d",
			ErrArtifactTooLarge, fi.Size(), limit)}
	}
	if wantSize >= 0 && fi.Size() != wantSize {
		return nil, &PathError{Path: rel, Err: fmt.Errorf("%w: the file has %d bytes, the reference says %d",
			ErrArtifactMismatch, fi.Size(), wantSize)}
	}
	f, err := s.root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, &PathError{Path: rel, Err: err}
	}
	defer f.Close()
	fi2, err := f.Stat()
	if err != nil {
		return nil, &PathError{Path: rel, Err: err}
	}
	if !fi2.Mode().IsRegular() {
		return nil, &PathError{Path: rel, Err: fmt.Errorf("%w: mode %s", ErrNotRegular, fi2.Mode().Type())}
	}
	if fi2.Size() != fi.Size() {
		return nil, &PathError{Path: rel, Err: fmt.Errorf("%w: the file changed while it was read", ErrArtifactMismatch)}
	}
	data, err := io.ReadAll(io.LimitReader(f, fi2.Size()+1))
	if err != nil {
		return nil, &PathError{Path: rel, Err: err}
	}
	if int64(len(data)) != fi2.Size() {
		return nil, &PathError{Path: rel, Err: fmt.Errorf("%w: the file changed while it was read", ErrArtifactMismatch)}
	}
	return data, nil
}
