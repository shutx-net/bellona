package storage

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
)

// finalizePoints names the crash point to check after the temporary file is
// synced, after the rename and after the directory fsync. A zero value means
// that no point is checked there.
type finalizePoints struct {
	tempSynced CrashPoint
	renamed    CrashPoint
	dirSynced  CrashPoint
}

// writeNew writes data to the Run-relative path rel as a file that did not
// exist: it creates missing parent directories, writes a temporary file in
// the target directory, fsyncs it, renames it into place and fsyncs the
// directory. It refuses an existing target, because rename would silently
// replace it. The caller holds the write lock and has checked the path.
func (s *Store) writeNew(rel string, data []byte, pts finalizePoints) error {
	return s.publish(rel, data, pts, false)
}

// replace is writeNew for a file that may exist, which the rename then
// replaces atomically. Only state.json is ever replaced.
func (s *Store) replace(rel string, data []byte, pts finalizePoints) error {
	return s.publish(rel, data, pts, true)
}

func (s *Store) publish(rel string, data []byte, pts finalizePoints, replaceOK bool) error {
	dir := path.Dir(rel)
	if err := s.ensureDirs(dir); err != nil {
		return err
	}
	if !replaceOK {
		if _, err := s.root.Lstat(rel); err == nil {
			return &PathError{Path: rel, Err: ErrArtifactExists}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return &PathError{Path: rel, Err: err}
		}
	}
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		return &PathError{Path: rel, Err: fmt.Errorf("draw a temporary file name: %w", err)}
	}
	tmp := path.Join(dir, "."+path.Base(rel)+".tmp-"+hex.EncodeToString(suffix))
	f, err := s.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return &PathError{Path: rel, Err: err}
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return &PathError{Path: rel, Err: err}
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return &PathError{Path: rel, Err: err}
	}
	if err := f.Close(); err != nil {
		return &PathError{Path: rel, Err: err}
	}
	if err := s.fire(pts.tempSynced); err != nil {
		return err
	}
	if err := s.root.Rename(tmp, rel); err != nil {
		return &PathError{Path: rel, Err: err}
	}
	if err := s.fire(pts.renamed); err != nil {
		return err
	}
	if err := s.fsyncDir(dir); err != nil {
		return &PathError{Path: rel, Err: fmt.Errorf("sync directory: %w", err)}
	}
	return s.fire(pts.dirSynced)
}

// ensureDirs makes sure that every component of the Run-relative directory
// dir is a real directory, creating the missing ones with mode 0700 and
// fsyncing the parent of each directory that it created. An existing
// component that is a symbolic link fails with ErrSymlink, even when it
// points inside the Run directory; one that is not a directory fails with
// ENOTDIR. "." is the Run directory itself.
func (s *Store) ensureDirs(dir string) error {
	if dir == "." {
		return nil
	}
	cur := ""
	missing := false
	for seg := range strings.SplitSeq(dir, "/") {
		parent := cur
		if parent == "" {
			parent = "."
		}
		cur = path.Join(cur, seg)
		if !missing {
			fi, err := s.root.Lstat(cur)
			switch {
			case err == nil && fi.Mode()&fs.ModeSymlink != 0:
				return &PathError{Path: cur, Err: ErrSymlink}
			case err == nil && !fi.IsDir():
				return &PathError{Path: cur, Err: syscall.ENOTDIR}
			case err == nil:
				continue
			case !errors.Is(err, fs.ErrNotExist):
				return &PathError{Path: cur, Err: err}
			}
			missing = true
		}
		if err := s.root.Mkdir(cur, 0o700); err != nil {
			return &PathError{Path: cur, Err: err}
		}
		if err := s.fsyncDir(parent); err != nil {
			return &PathError{Path: parent, Err: fmt.Errorf("sync directory: %w", err)}
		}
	}
	return nil
}

// fsyncDir fsyncs the Run-relative directory dir ("." is the Run directory).
func (s *Store) fsyncDir(dir string) error {
	d, err := s.root.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	return errors.Join(err, d.Close())
}

// fsyncAncestors fsyncs the directory of rel and every directory above it up
// to the Run directory. It makes the directory entries of an artifact that a
// crashed earlier call finished renaming durable.
func (s *Store) fsyncAncestors(rel string) error {
	for dir := path.Dir(rel); ; dir = path.Dir(dir) {
		if err := s.fsyncDir(dir); err != nil {
			return &PathError{Path: dir, Err: fmt.Errorf("sync directory: %w", err)}
		}
		if dir == "." {
			return nil
		}
	}
}

// fsyncPath fsyncs a directory that lies outside any Run (the parents of the
// Run directory).
func fsyncPath(p string) error {
	d, err := os.Open(p)
	if err != nil {
		return err
	}
	err = d.Sync()
	return errors.Join(err, d.Close())
}

// mkdirAllSync creates dir and its missing ancestors with mode 0700 and
// fsyncs the parent of each directory that it created.
func mkdirAllSync(dir string) error {
	if fi, err := os.Stat(dir); err == nil {
		if !fi.IsDir() {
			return fmt.Errorf("%s: %w", dir, syscall.ENOTDIR)
		}
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	parent := filepath.Dir(dir)
	if parent != dir {
		if err := mkdirAllSync(parent); err != nil {
			return err
		}
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return fsyncPath(parent)
}
