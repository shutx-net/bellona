// Package manifest builds and checks the Run manifest (SPEC §12.4, invariant
// 12). At Run start the engine freezes the effective configuration, the
// contents of the role definitions and the workflow identifier and version by
// copying them into the Run directory; the manifest, manifest.json, refers to
// the copies by path, size and SHA-256 and records the contracts that the
// engine supported when the Run started. A digest is recorded in addition to
// the copy, never instead of it.
//
// The package is pure: it builds the bytes of the files and checks bytes that
// were read back, and it writes nothing. The engine writes the files.
//
// On resume, CheckCompatible refuses a Run that was recorded under different
// engine or event contracts. M0 has no migration procedure, so every
// incompatible Run is refused. When a migration is first needed it will be a
// separate, explicitly invoked user procedure that rewrites a Run to the
// current contracts; M0 does not model one. Compare detects that the
// configuration or a role definition now differs from the frozen copy; it
// never applies a change (R-MAN-01, T-14).
package manifest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	json "encoding/json/v2"

	"github.com/shutx-net/bellona/internal/config"
	"github.com/shutx-net/bellona/internal/domain"
	"github.com/shutx-net/bellona/internal/fsm"
	"github.com/shutx-net/bellona/internal/strictjson"
	"github.com/shutx-net/bellona/internal/version"
)

// SchemaVersion is the schema version of manifest.json.
const SchemaVersion = 1

// ErrInvalidManifest is wrapped by every error about the content of a
// manifest that is not an incompatibility: a decode error, a validation error
// and a failed build.
var ErrInvalidManifest = errors.New("invalid Run manifest")

// ErrIncompatible is matched by an *IncompatibleError: the Run was recorded
// under engine or event contracts that this engine does not support.
var ErrIncompatible = errors.New("the Run was recorded under incompatible contracts")

var (
	digestPattern     = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	workflowIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

// maxEngineVersionBytes bounds the recorded engine version.
const maxEngineVersionBytes = 128

// Versions are the contract versions that decide whether a Run can be
// resumed. The engine version string is not among them: it is informative.
type Versions struct {
	ManifestSchema  int
	WorkflowID      string
	WorkflowVersion int
	WorkflowDigest  string // "sha256:" and the lowercase hex digest of the workflow
	EventSchema     int
	ConfigSchema    int
}

// Current returns the versions that this engine supports: the manifest and
// configuration schema versions, the workflow identifier, version and digest
// of the embedded tables, and the event schema version.
func Current() Versions {
	return Versions{
		ManifestSchema:  SchemaVersion,
		WorkflowID:      fsm.WorkflowID,
		WorkflowVersion: fsm.WorkflowVersion,
		WorkflowDigest:  "sha256:" + fsm.Digest(),
		EventSchema:     domain.EventSchemaVersion,
		ConfigSchema:    config.SchemaVersion,
	}
}

// RoleDefinitionRef refers to the frozen copy of the definition of one role.
type RoleDefinitionRef struct {
	Role       domain.Role        `json:"role"`
	Definition domain.ArtifactRef `json:"definition"`
}

// Manifest is the content of manifest.json. The runtimes section of the SPEC
// example joins in M1a.
type Manifest struct {
	SchemaVersion         int                 `json:"schemaVersion"`
	RunID                 domain.RunID        `json:"runId"`
	EngineVersion         string              `json:"engineVersion"`
	WorkflowID            string              `json:"workflowId"`
	WorkflowVersion       int                 `json:"workflowVersion"`
	WorkflowDigest        string              `json:"workflowDigest"`
	EventSchemaVersion    int                 `json:"eventSchemaVersion"`
	ConfigSchemaVersion   int                 `json:"configSchemaVersion"`
	Policy                domain.ArtifactRef  `json:"policy"`
	RoleDefinitions       []RoleDefinitionRef `json:"roleDefinitions"`
	RoleDefinitionsDigest string              `json:"roleDefinitionsDigest"`
}

// Versions returns the contract versions that the manifest records.
func (m Manifest) Versions() Versions {
	return Versions{
		ManifestSchema:  m.SchemaVersion,
		WorkflowID:      m.WorkflowID,
		WorkflowVersion: m.WorkflowVersion,
		WorkflowDigest:  m.WorkflowDigest,
		EventSchema:     m.EventSchemaVersion,
		ConfigSchema:    m.ConfigSchemaVersion,
	}
}

// valueError is the Err of every validation error: it matches
// ErrInvalidManifest and strictjson.ErrInvalidValue.
type valueError struct{}

func (valueError) Error() string   { return ErrInvalidManifest.Error() }
func (valueError) Unwrap() []error { return []error{ErrInvalidManifest, strictjson.ErrInvalidValue} }

func invalid(pointer, format string, args ...any) error {
	return &strictjson.Error{Pointer: pointer, Err: valueError{}, Detail: fmt.Sprintf(format, args...)}
}

// Validate checks the formats of the manifest and its internal consistency:
// a valid Run ID, a non-empty engine version, a valid workflow identifier,
// positive versions, digests of the form "sha256:" and 64 lowercase hex
// characters, the policy at domain.PolicyPath, exactly one definition per
// role in the order of domain.Roles at domain.RoleDefinitionPath, and a role
// definitions digest that agrees with the references. It never compares a
// version with the version that this engine supports: that is
// CheckCompatible. The first violation is returned as a *strictjson.Error
// that names the member by its JSON Pointer.
func (m Manifest) Validate() error {
	if m.SchemaVersion < 1 {
		return invalid("/schemaVersion", "schema version %d is not positive", m.SchemaVersion)
	}
	if err := m.RunID.Validate(); err != nil {
		return invalid("/runId", "%v", err)
	}
	switch {
	case m.EngineVersion == "":
		return invalid("/engineVersion", "the engine version is empty")
	case len(m.EngineVersion) > maxEngineVersionBytes || !utf8.ValidString(m.EngineVersion) ||
		strings.ContainsFunc(m.EngineVersion, func(r rune) bool { return r < ' ' || r == 0x7f }):
		return invalid("/engineVersion", "the engine version is too long or contains control characters")
	case !workflowIDPattern.MatchString(m.WorkflowID):
		return invalid("/workflowId", "%q does not match %s", m.WorkflowID, workflowIDPattern)
	case m.WorkflowVersion < 1:
		return invalid("/workflowVersion", "workflow version %d is not positive", m.WorkflowVersion)
	case !digestPattern.MatchString(m.WorkflowDigest):
		return invalid("/workflowDigest", "the digest must be %q followed by 64 lowercase hex characters", "sha256:")
	case m.EventSchemaVersion < 1:
		return invalid("/eventSchemaVersion", "event schema version %d is not positive", m.EventSchemaVersion)
	case m.ConfigSchemaVersion < 1:
		return invalid("/configSchemaVersion", "configuration schema version %d is not positive", m.ConfigSchemaVersion)
	}
	if err := m.Policy.Validate(); err != nil {
		return invalid("/policy", "%v", err)
	}
	if m.Policy.Path != domain.PolicyPath {
		return invalid("/policy/path", "the policy is at %q, want %q", m.Policy.Path, domain.PolicyPath)
	}
	roles := domain.Roles()
	if len(m.RoleDefinitions) != len(roles) {
		return invalid("/roleDefinitions", "%d role definitions, want one for each of the %d roles", len(m.RoleDefinitions), len(roles))
	}
	for i, r := range m.RoleDefinitions {
		at := "/roleDefinitions/" + strconv.Itoa(i)
		switch {
		case r.Role != roles[i]:
			return invalid(at+"/role", "role %q at position %d, want %q (one per role, in the order of the roles)", string(r.Role), i, string(roles[i]))
		case r.Definition.Validate() != nil:
			return invalid(at+"/definition", "%v", r.Definition.Validate())
		case r.Definition.Path != domain.RoleDefinitionPath(r.Role):
			return invalid(at+"/definition/path", "the definition of %s is at %q, want %q", r.Role, r.Definition.Path, domain.RoleDefinitionPath(r.Role))
		}
	}
	if !digestPattern.MatchString(m.RoleDefinitionsDigest) {
		return invalid("/roleDefinitionsDigest", "the digest must be %q followed by 64 lowercase hex characters", "sha256:")
	}
	if want := roleDefinitionsDigest(m.RoleDefinitions); m.RoleDefinitionsDigest != want {
		return invalid("/roleDefinitionsDigest", "the digest does not agree with the role definitions, want %s", want)
	}
	return nil
}

// roleDefinitionsDigest returns "sha256:" and the hex SHA-256 of the
// concatenation, in the order given, of "<role> <SHA-256 of the definition,
// lowercase hex>\n".
func roleDefinitionsDigest(refs []RoleDefinitionRef) string {
	h := sha256.New()
	for _, r := range refs {
		fmt.Fprintf(h, "%s %s\n", r.Role, r.Definition.SHA256)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// BuildInput is the input of Build.
type BuildInput struct {
	RunID domain.RunID
	// Effective is the effective configuration, the result of
	// config.Resolve.
	Effective config.Config
	// Roles holds the definition of every role, in the order of
	// domain.Roles.
	Roles []domain.RoleDefinition
	// EngineVersion is the engine version to record. Empty means
	// version.Engine.
	EngineVersion string
	// Versions are the contract versions to record. The zero value means
	// Current(). They are recorded as given, so that a test can create a Run
	// whose versions this engine does not support.
	Versions Versions
}

// File is a file that the engine writes into the Run directory.
type File struct {
	// Path is relative to the Run directory.
	Path string
	Data []byte
}

// Frozen is the result of Build.
type Frozen struct {
	Manifest Manifest
	// Files holds policy.json, the role definitions and, last, manifest.json,
	// in the order in which the engine writes them: every file that the
	// manifest refers to exists before the manifest does.
	Files []File
}

// Build freezes a Run start. It validates the effective configuration
// (config.Config.ValidateEffective) and the role definitions
// (domain.ValidateRoleDefinitions), and returns the files to write and the
// manifest that refers to them: policy.json holds the effective configuration
// in the indented form of strictjson.EncodeIndent; definitions/roles/<role>.md
// holds a copy of each definition, byte for byte, in the order of
// domain.Roles; manifest.json comes last. The result is deterministic, and
// shares no memory with the input.
func Build(in BuildInput) (Frozen, error) {
	if err := in.RunID.Validate(); err != nil {
		return Frozen{}, fmt.Errorf("%w: %w", ErrInvalidManifest, err)
	}
	if err := in.Effective.ValidateEffective(); err != nil {
		return Frozen{}, err
	}
	if err := domain.ValidateRoleDefinitions(in.Roles); err != nil {
		return Frozen{}, err
	}
	engine := in.EngineVersion
	if engine == "" {
		engine = version.Engine
	}
	versions := in.Versions
	if versions == (Versions{}) {
		versions = Current()
	}

	policy, err := strictjson.EncodeIndent(in.Effective)
	if err != nil {
		return Frozen{}, fmt.Errorf("%w: encode the effective configuration: %w", ErrInvalidManifest, err)
	}
	policyRef, err := domain.NewArtifactRef(domain.PolicyPath, policy)
	if err != nil {
		return Frozen{}, fmt.Errorf("%w: %w", ErrInvalidManifest, err)
	}
	files := []File{{Path: domain.PolicyPath, Data: policy}}
	refs := make([]RoleDefinitionRef, 0, len(in.Roles))
	for _, d := range in.Roles {
		path := domain.RoleDefinitionPath(d.Role)
		data := bytes.Clone(d.Content)
		ref, err := domain.NewArtifactRef(path, data)
		if err != nil {
			return Frozen{}, fmt.Errorf("%w: %w", ErrInvalidManifest, err)
		}
		refs = append(refs, RoleDefinitionRef{Role: d.Role, Definition: ref})
		files = append(files, File{Path: path, Data: data})
	}
	m := Manifest{
		SchemaVersion:         versions.ManifestSchema,
		RunID:                 in.RunID,
		EngineVersion:         engine,
		WorkflowID:            versions.WorkflowID,
		WorkflowVersion:       versions.WorkflowVersion,
		WorkflowDigest:        versions.WorkflowDigest,
		EventSchemaVersion:    versions.EventSchema,
		ConfigSchemaVersion:   versions.ConfigSchema,
		Policy:                policyRef,
		RoleDefinitions:       refs,
		RoleDefinitionsDigest: roleDefinitionsDigest(refs),
	}
	if err := m.Validate(); err != nil {
		return Frozen{}, err
	}
	data, err := strictjson.EncodeIndent(m)
	if err != nil {
		return Frozen{}, fmt.Errorf("%w: encode the manifest: %w", ErrInvalidManifest, err)
	}
	files = append(files, File{Path: domain.ManifestPath, Data: data})
	return Frozen{Manifest: m, Files: files}, nil
}

// Decode strictly decodes and validates manifest.json. It first reads the
// schema version leniently, and returns an *IncompatibleError for a manifest
// of another schema version, because such a manifest may have other members
// that strict decoding would reject as unknown. Any other error wraps
// ErrInvalidManifest.
func Decode(data []byte) (Manifest, error) {
	// A lenient peek. When it fails or finds no schema version, strict
	// decoding below reports the precise error.
	var peek struct {
		SchemaVersion *int `json:"schemaVersion"`
	}
	if err := json.Unmarshal(data, &peek); err == nil && peek.SchemaVersion != nil && *peek.SchemaVersion != SchemaVersion {
		return Manifest{}, &IncompatibleError{Mismatches: []Mismatch{{
			Field:     "schemaVersion",
			Recorded:  strconv.Itoa(*peek.SchemaVersion),
			Supported: strconv.Itoa(SchemaVersion),
		}}}
	}
	m, err := strictjson.Decode[Manifest](data)
	if err != nil {
		if !errors.Is(err, ErrInvalidManifest) {
			err = fmt.Errorf("%w: %w", ErrInvalidManifest, err)
		}
		return Manifest{}, err
	}
	return m, nil
}

// Mismatch is one contract version that differs from the supported one.
// Field is the name of the manifest member.
type Mismatch struct {
	Field     string
	Recorded  string
	Supported string
}

// IncompatibleError reports that a Run was recorded under contracts that this
// engine does not support, and lists every difference. It matches
// ErrIncompatible under errors.Is.
type IncompatibleError struct {
	Mismatches []Mismatch
}

// Error lists every mismatch and states that resumption is refused.
func (e *IncompatibleError) Error() string {
	parts := make([]string, len(e.Mismatches))
	for i, m := range e.Mismatches {
		parts[i] = fmt.Sprintf("%s was recorded as %s, this engine supports %s", m.Field, m.Recorded, m.Supported)
	}
	return fmt.Sprintf("%s: %s; resumption is refused because no migration procedure exists in M0",
		ErrIncompatible, strings.Join(parts, "; "))
}

// Is reports whether target is ErrIncompatible.
func (e *IncompatibleError) Is(target error) bool { return target == ErrIncompatible }

// CheckCompatible returns nil only when every contract version that the
// manifest records equals the supported one: the manifest schema version, the
// workflow identifier, version and digest, the event schema version and the
// configuration schema version. Otherwise it returns an *IncompatibleError
// that lists every mismatch. The engine version string is recorded but not
// compared.
func CheckCompatible(m Manifest, supported Versions) error {
	rec := m.Versions()
	var mismatches []Mismatch
	check := func(field, recorded, want string) {
		if recorded != want {
			mismatches = append(mismatches, Mismatch{Field: field, Recorded: recorded, Supported: want})
		}
	}
	check("schemaVersion", strconv.Itoa(rec.ManifestSchema), strconv.Itoa(supported.ManifestSchema))
	check("workflowId", rec.WorkflowID, supported.WorkflowID)
	check("workflowVersion", strconv.Itoa(rec.WorkflowVersion), strconv.Itoa(supported.WorkflowVersion))
	check("workflowDigest", rec.WorkflowDigest, supported.WorkflowDigest)
	check("eventSchemaVersion", strconv.Itoa(rec.EventSchema), strconv.Itoa(supported.EventSchema))
	check("configSchemaVersion", strconv.Itoa(rec.ConfigSchema), strconv.Itoa(supported.ConfigSchema))
	if len(mismatches) == 0 {
		return nil
	}
	return &IncompatibleError{Mismatches: mismatches}
}

// Changes reports what differs between a frozen Run and the current
// configuration and role definitions.
type Changes struct {
	// Config is true when the current effective configuration differs from
	// the frozen policy.json.
	Config bool
	// Roles lists, in the order of domain.Roles, every role whose current
	// definition differs from the frozen copy.
	Roles []domain.Role
}

// Any reports whether anything changed.
func (c Changes) Any() bool { return c.Config || len(c.Roles) > 0 }

// Compare detects changes of the configuration and of the role definitions
// since the Run was frozen (T-14). Config is true when the SHA-256 of
// strictjson.EncodeIndent(current) differs from the policy reference of the
// manifest; Roles lists every role whose current content hash differs from
// the frozen reference. A nil roles skips the role comparison; otherwise
// roles must pass domain.ValidateRoleDefinitions. A change is reported, never
// applied: the frozen copies keep governing the Run.
func Compare(m Manifest, current config.Config, roles []domain.RoleDefinition) (Changes, error) {
	changed, err := CompareConfig(m, current)
	if err != nil {
		return Changes{}, err
	}
	out := Changes{Config: changed}
	if roles != nil {
		if out.Roles, err = CompareRoles(m, roles); err != nil {
			return Changes{}, err
		}
	}
	return out, nil
}

// CompareConfig reports whether the SHA-256 of strictjson.EncodeIndent of the
// current effective configuration differs from the frozen policy reference of
// the manifest.
func CompareConfig(m Manifest, current config.Config) (bool, error) {
	if err := m.Validate(); err != nil {
		return false, err
	}
	data, err := strictjson.EncodeIndent(current)
	if err != nil {
		return false, fmt.Errorf("%w: encode the current configuration: %w", ErrInvalidManifest, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]) != m.Policy.SHA256, nil
}

// CompareRoles returns, in the order of domain.Roles, every role whose
// current definition has a content hash that differs from the frozen
// reference of the manifest. roles must pass domain.ValidateRoleDefinitions.
func CompareRoles(m Manifest, roles []domain.RoleDefinition) ([]domain.Role, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	if err := domain.ValidateRoleDefinitions(roles); err != nil {
		return nil, err
	}
	var changed []domain.Role
	for i, d := range roles {
		sum := sha256.Sum256(d.Content)
		if hex.EncodeToString(sum[:]) != m.RoleDefinitions[i].Definition.SHA256 {
			changed = append(changed, d.Role)
		}
	}
	return changed, nil
}
