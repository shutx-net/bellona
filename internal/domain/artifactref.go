package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"
)

// ErrInvalidPath is wrapped by every Run-relative path validation error.
var ErrInvalidPath = errors.New("invalid Run-relative path")

// ErrInvalidArtifactRef is wrapped by every artifact reference error other
// than an invalid path.
var ErrInvalidArtifactRef = errors.New("invalid artifact reference")

const (
	// MaxPathBytes is the longest Run-relative path.
	MaxPathBytes = 255
	// MaxSegmentBytes is the longest single segment of a Run-relative path.
	MaxSegmentBytes = 128
)

var (
	segmentPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*$`)
	sha256Pattern  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ArtifactRef refers to a finalized, immutable artifact file by its path
// relative to the Run directory, its size and its SHA-256 (SPEC §12.2). Hashes
// detect accidental corruption and mix-ups, not deliberate modification.
type ArtifactRef struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"` // lowercase hex
}

// NewArtifactRef describes data stored at the Run-relative path p. It
// returns an error wrapping ErrInvalidPath for an invalid path.
func NewArtifactRef(p string, data []byte) (ArtifactRef, error) {
	if err := ValidateRunPath(p); err != nil {
		return ArtifactRef{}, err
	}
	sum := sha256.Sum256(data)
	return ArtifactRef{Path: p, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}, nil
}

// Validate checks the path (ErrInvalidPath), the size, which must not be
// negative, and the digest, which must be 64 lowercase hex characters.
func (r ArtifactRef) Validate() error {
	if err := ValidateRunPath(r.Path); err != nil {
		return err
	}
	if r.Size < 0 {
		return fmt.Errorf("%w: negative size %d", ErrInvalidArtifactRef, r.Size)
	}
	if !sha256Pattern.MatchString(r.SHA256) {
		return fmt.Errorf("%w: sha256 must be 64 lowercase hex characters", ErrInvalidArtifactRef)
	}
	return nil
}

// IsZero reports whether r is the zero value, which marks an absent optional
// reference (use it with the omitzero JSON option).
func (r ArtifactRef) IsZero() bool { return r == ArtifactRef{} }

// ValidateRunPath checks a Run-relative, slash-separated path. It is
// non-empty, at most MaxPathBytes bytes of valid UTF-8, without NUL or
// backslash, not absolute, equal to its path.Clean form, and every segment is
// at most MaxSegmentBytes bytes and matches [A-Za-z0-9_][A-Za-z0-9._-]*. A
// segment can therefore never be empty, "." or "..", and never start with a
// dot, so that temporary files can never be referenced. Every error wraps
// ErrInvalidPath.
func ValidateRunPath(p string) error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidPath, fmt.Sprintf(format, args...))
	}
	switch {
	case p == "":
		return bad("empty path")
	case len(p) > MaxPathBytes:
		return bad("%d bytes exceed the limit of %d", len(p), MaxPathBytes)
	case !utf8.ValidString(p):
		return bad("not valid UTF-8")
	case strings.ContainsRune(p, 0):
		return bad("contains a NUL byte")
	case strings.ContainsRune(p, '\\'):
		return bad("contains a backslash")
	case strings.HasPrefix(p, "/"):
		return bad("absolute path %q", p)
	case path.Clean(p) != p:
		return bad("%q is not in clean form", p)
	}
	for seg := range strings.SplitSeq(p, "/") {
		switch {
		case seg == "" || seg == "." || seg == "..":
			return bad("%q has an empty, \".\" or \"..\" segment", p)
		case len(seg) > MaxSegmentBytes:
			return bad("a segment of %q exceeds %d bytes", p, MaxSegmentBytes)
		case !segmentPattern.MatchString(seg):
			return bad("segment %q of %q must match %s", seg, p, segmentPattern)
		}
	}
	return nil
}
