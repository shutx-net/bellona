package manifest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/shutx-net/bellona/internal/config"
	"github.com/shutx-net/bellona/internal/domain"
	"github.com/shutx-net/bellona/internal/fsm"
	"github.com/shutx-net/bellona/internal/strictjson"
	"github.com/shutx-net/bellona/internal/version"
)

const testRunID = domain.RunID("run-0123456789abcdef")

func effective(t *testing.T) config.Config {
	t.Helper()
	cfg, err := config.Resolve(config.Inputs{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return cfg
}

// roleDefs returns definitions that are independent of the embedded
// placeholders.
func roleDefs() []domain.RoleDefinition {
	var defs []domain.RoleDefinition
	for _, r := range domain.Roles() {
		defs = append(defs, domain.RoleDefinition{Role: r, Content: []byte("# " + string(r) + "\nDefinition text of the " + string(r) + ".\n")})
	}
	return defs
}

func input(t *testing.T) BuildInput {
	t.Helper()
	return BuildInput{RunID: testRunID, Effective: effective(t), Roles: roleDefs()}
}

func build(t *testing.T, in BuildInput) Frozen {
	t.Helper()
	f, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return f
}

func sum(data []byte) string {
	s := sha256.Sum256(data)
	return hex.EncodeToString(s[:])
}

func fileMap(f Frozen) map[string][]byte {
	m := make(map[string][]byte)
	for _, file := range f.Files {
		m[file.Path] = file.Data
	}
	return m
}

func TestBuildIsDeterministic(t *testing.T) {
	in := input(t)
	a, b := build(t, in), build(t, in)
	if !reflect.DeepEqual(a, b) {
		t.Fatal("two builds of the same input differ")
	}
	// A freshly resolved configuration and fresh definitions give the same bytes.
	c := build(t, input(t))
	for i := range a.Files {
		if a.Files[i].Path != c.Files[i].Path || !bytes.Equal(a.Files[i].Data, c.Files[i].Data) {
			t.Errorf("file %s differs between independent builds", a.Files[i].Path)
		}
	}
}

func TestBuildFilesAndOrder(t *testing.T) {
	in := input(t)
	f := build(t, in)
	wantPaths := []string{
		"policy.json",
		"definitions/roles/coordinator.md", "definitions/roles/planner.md",
		"definitions/roles/implementer.md", "definitions/roles/evaluator.md",
		"manifest.json",
	}
	var gotPaths []string
	for _, file := range f.Files {
		gotPaths = append(gotPaths, file.Path)
	}
	if !slices.Equal(gotPaths, wantPaths) {
		t.Fatalf("file paths = %v, want %v", gotPaths, wantPaths)
	}
	files := fileMap(f)
	for _, d := range in.Roles {
		if got := files[domain.RoleDefinitionPath(d.Role)]; !bytes.Equal(got, d.Content) {
			t.Errorf("the file of %s is not a verbatim copy of the definition:\n%q", d.Role, got)
		}
	}
	// policy.json is the effective configuration: it decodes with
	// config.Decode and re-encodes to the frozen bytes.
	policy := files["policy.json"]
	decoded, err := config.Decode(policy)
	if err != nil {
		t.Fatalf("config.Decode(policy.json): %v", err)
	}
	again, err := strictjson.EncodeIndent(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again, policy) {
		t.Errorf("policy.json does not survive a decode and encode:\n%s\n%s", policy, again)
	}
	direct, err := strictjson.EncodeIndent(in.Effective)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(direct, policy) {
		t.Errorf("policy.json is not the encoding of the effective configuration:\n%s", policy)
	}
	if !bytes.HasSuffix(policy, []byte("}\n")) || !bytes.Contains(policy, []byte("\n  \"")) {
		t.Errorf("policy.json is not indented with a trailing newline:\n%s", policy)
	}
	// manifest.json is the encoding of the manifest, and decodes to it.
	data := f.Files[len(f.Files)-1].Data
	want, err := strictjson.EncodeIndent(f.Manifest)
	if err != nil || !bytes.Equal(want, data) {
		t.Errorf("manifest.json is not the encoding of the manifest (%v)", err)
	}
	m, err := Decode(data)
	if err != nil {
		t.Fatalf("Decode(manifest.json): %v", err)
	}
	if !reflect.DeepEqual(m, f.Manifest) {
		t.Errorf("the decoded manifest differs:\n%+v\n%+v", m, f.Manifest)
	}
}

func TestBuildRecordsTheContracts(t *testing.T) {
	in := input(t)
	f := build(t, in)
	m := f.Manifest
	files := fileMap(f)

	if m.SchemaVersion != 1 || m.RunID != testRunID || m.EngineVersion != version.Engine {
		t.Errorf("manifest header = %+v", m)
	}
	if m.WorkflowID != fsm.WorkflowID || m.WorkflowVersion != fsm.WorkflowVersion || m.WorkflowDigest != "sha256:"+fsm.Digest() {
		t.Errorf("workflow = %q %d %q", m.WorkflowID, m.WorkflowVersion, m.WorkflowDigest)
	}
	if m.EventSchemaVersion != domain.EventSchemaVersion || m.ConfigSchemaVersion != config.SchemaVersion {
		t.Errorf("schema versions = %d, %d", m.EventSchemaVersion, m.ConfigSchemaVersion)
	}
	if got := m.Versions(); got != Current() {
		t.Errorf("Versions() = %+v, want Current() = %+v", got, Current())
	}

	policyRef, err := domain.NewArtifactRef("policy.json", files["policy.json"])
	if err != nil || m.Policy != policyRef {
		t.Errorf("policy reference = %+v, want %+v (%v)", m.Policy, policyRef, err)
	}
	if len(m.RoleDefinitions) != 4 {
		t.Fatalf("%d role definition references", len(m.RoleDefinitions))
	}
	var digestInput strings.Builder
	for i, r := range domain.Roles() {
		ref := m.RoleDefinitions[i]
		want, err := domain.NewArtifactRef(domain.RoleDefinitionPath(r), in.Roles[i].Content)
		if err != nil || ref.Role != r || ref.Definition != want {
			t.Errorf("reference %d = %+v, want %s %+v (%v)", i, ref, r, want, err)
		}
		fmt.Fprintf(&digestInput, "%s %s\n", r, sum(in.Roles[i].Content))
	}
	if want := "sha256:" + sum([]byte(digestInput.String())); m.RoleDefinitionsDigest != want {
		t.Errorf("roleDefinitionsDigest = %s, want %s", m.RoleDefinitionsDigest, want)
	}
	if err := m.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
	if err := CheckCompatible(m, Current()); err != nil {
		t.Errorf("CheckCompatible of a fresh manifest: %v", err)
	}
}

func TestBuildCopiesItsInputs(t *testing.T) {
	// Changing the input afterwards does not change the result.
	in := input(t)
	f := build(t, in)
	want := build(t, input(t))
	for i := range in.Roles {
		for j := range in.Roles[i].Content {
			in.Roles[i].Content[j] = 'X'
		}
	}
	if !reflect.DeepEqual(f, want) {
		t.Error("the result changed with the input")
	}

	// Changing the result does not change the input.
	in = input(t)
	f = build(t, in)
	for _, file := range f.Files {
		for j := range file.Data {
			file.Data[j] = 'X'
		}
	}
	if !reflect.DeepEqual(in.Roles, roleDefs()) {
		t.Error("the role definitions of the input changed with the result")
	}
	// A later build does not see the change either.
	if again := build(t, in); !reflect.DeepEqual(again, want) {
		t.Error("a later build is affected by changes to an earlier result")
	}
}

func TestChangingOneRoleChangesOnlyItsReferenceAndTheDigest(t *testing.T) {
	in := input(t)
	base := build(t, in)
	in.Roles[1].Content = append(in.Roles[1].Content, []byte("An added line.\n")...)
	changed := build(t, in)

	if changed.Manifest.Policy != base.Manifest.Policy {
		t.Error("the policy reference changed")
	}
	if changed.Manifest.RoleDefinitionsDigest == base.Manifest.RoleDefinitionsDigest {
		t.Error("the role definitions digest did not change")
	}
	for i := range base.Manifest.RoleDefinitions {
		same := changed.Manifest.RoleDefinitions[i] == base.Manifest.RoleDefinitions[i]
		if i == 1 && same {
			t.Error("the reference of the planner did not change")
		}
		if i != 1 && !same {
			t.Errorf("the reference of %s changed", domain.Roles()[i])
		}
	}
	if !bytes.Equal(changed.Files[0].Data, base.Files[0].Data) {
		t.Error("policy.json changed")
	}
	for i := 1; i <= 4; i++ {
		same := bytes.Equal(changed.Files[i].Data, base.Files[i].Data)
		if wantSame := i != 2; same != wantSame { // index 2 is the planner
			t.Errorf("file %s: unchanged = %v, want %v", changed.Files[i].Path, same, wantSame)
		}
	}
}

func TestChangingTheConfigurationChangesOnlyThePolicyReference(t *testing.T) {
	in := input(t)
	base := build(t, in)
	in.Effective.Execution.MaxReplans = new(int)
	changed := build(t, in)
	if changed.Manifest.Policy == base.Manifest.Policy {
		t.Error("the policy reference did not change")
	}
	if !reflect.DeepEqual(changed.Manifest.RoleDefinitions, base.Manifest.RoleDefinitions) ||
		changed.Manifest.RoleDefinitionsDigest != base.Manifest.RoleDefinitionsDigest {
		t.Error("the role definitions changed")
	}
}

func TestBuildRecordsGivenVersionsVerbatim(t *testing.T) {
	in := input(t)
	in.EngineVersion = "9.9.9-test"
	v := Current()
	v.EventSchema = 2
	v.WorkflowVersion = 7
	v.WorkflowDigest = "sha256:" + strings.Repeat("ab", 32)
	v.ConfigSchema = 5
	v.WorkflowID = "other-flow"
	in.Versions = v
	f := build(t, in)
	if f.Manifest.EngineVersion != "9.9.9-test" || f.Manifest.Versions() != v {
		t.Errorf("manifest = %+v", f.Manifest)
	}
	// It is a valid manifest that decodes, and this engine refuses to resume it.
	m, err := Decode(f.Files[len(f.Files)-1].Data)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	err = CheckCompatible(m, Current())
	var ie *IncompatibleError
	if !errors.Is(err, ErrIncompatible) || !errors.As(err, &ie) {
		t.Fatalf("CheckCompatible = %v", err)
	}
	var fields []string
	for _, mm := range ie.Mismatches {
		fields = append(fields, mm.Field)
	}
	want := []string{"workflowId", "workflowVersion", "workflowDigest", "eventSchemaVersion", "configSchemaVersion"}
	if !slices.Equal(fields, want) {
		t.Errorf("mismatched fields = %v, want %v", fields, want)
	}
	// The same manifest is compatible with an engine that supports exactly these versions.
	if err := CheckCompatible(m, v); err != nil {
		t.Errorf("CheckCompatible with the recorded versions: %v", err)
	}
}

func TestBuildRejects(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*BuildInput)
		want   error
	}{
		{"empty Run ID", func(in *BuildInput) { in.RunID = "" }, domain.ErrInvalidID},
		{"malformed Run ID", func(in *BuildInput) { in.RunID = "run-xyz" }, domain.ErrInvalidID},
		{"zero configuration", func(in *BuildInput) { in.Effective = config.Config{} }, config.ErrInvalidConfig},
		{"layer, not effective", func(in *BuildInput) { in.Effective = config.Config{SchemaVersion: 1} }, config.ErrInvalidConfig},
		{"undefined command", func(in *BuildInput) { in.Effective.Verification.RequiredChecks = []string{"missing"} }, config.ErrInvalidConfig},
		{"no definitions", func(in *BuildInput) { in.Roles = nil }, domain.ErrInvalidRoleDefinition},
		{"three definitions", func(in *BuildInput) { in.Roles = in.Roles[:3] }, domain.ErrInvalidRoleDefinition},
		{"wrong order", func(in *BuildInput) { in.Roles[0], in.Roles[1] = in.Roles[1], in.Roles[0] }, domain.ErrInvalidRoleDefinition},
		{"empty definition", func(in *BuildInput) { in.Roles[2].Content = nil }, domain.ErrInvalidRoleDefinition},
		{"invalid UTF-8", func(in *BuildInput) { in.Roles[3].Content = []byte("\xff") }, domain.ErrInvalidRoleDefinition},
		{"oversize definition", func(in *BuildInput) {
			in.Roles[0].Content = bytes.Repeat([]byte("a"), domain.MaxRoleDefinitionBytes+1)
		}, domain.ErrInvalidRoleDefinition},
		{"malformed workflow digest", func(in *BuildInput) {
			in.Versions = Current()
			in.Versions.WorkflowDigest = "not-a-digest"
		}, ErrInvalidManifest},
		{"zero event schema", func(in *BuildInput) {
			in.Versions = Current()
			in.Versions.EventSchema = 0
		}, ErrInvalidManifest},
		{"control character in the engine version", func(in *BuildInput) { in.EngineVersion = "1.0\n" }, ErrInvalidManifest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := input(t)
			tt.mutate(&in)
			got, err := Build(in)
			if !errors.Is(err, tt.want) {
				t.Fatalf("Build error = %v, want %v", err, tt.want)
			}
			if got.Files != nil || got.Manifest.RunID != "" {
				t.Errorf("Build returned %+v with an error", got)
			}
		})
	}
}

func manifestBytes(t *testing.T) []byte {
	t.Helper()
	f := build(t, input(t))
	return f.Files[len(f.Files)-1].Data
}

// edit decodes manifest.json into generic values, applies fn and encodes it
// again.
func edit(t *testing.T, data []byte, fn func(map[string]any)) []byte {
	t.Helper()
	m, err := strictjson.Decode[map[string]any](data)
	if err != nil {
		t.Fatal(err)
	}
	fn(m)
	out, err := strictjson.Encode(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// nullMark stands for a JSON null in a document built with edit, which
// cannot hold one; withNull replaces it.
const nullMark = "NULL-PLACEHOLDER"

func withNull(data []byte) []byte {
	return bytes.Replace(data, []byte(`"`+nullMark+`"`), []byte("null"), 1)
}

func TestDecodeRoundTripAndStrictRejections(t *testing.T) {
	valid := manifestBytes(t)
	if _, err := Decode(valid); err != nil {
		t.Fatalf("Decode of a valid manifest: %v", err)
	}
	members := []string{
		"schemaVersion", "runId", "engineVersion", "workflowId", "workflowVersion", "workflowDigest",
		"eventSchemaVersion", "configSchemaVersion", "policy", "roleDefinitions", "roleDefinitionsDigest",
	}
	type tc struct {
		name    string
		data    []byte
		want    error
		pointer string
	}
	var tests []tc
	for _, member := range members {
		tests = append(tests,
			tc{"missing " + member, edit(t, valid, func(m map[string]any) { delete(m, member) }), strictjson.ErrMissingMember, "/" + member},
			tc{"null " + member, withNull(edit(t, valid, func(m map[string]any) { m[member] = nullMark })), strictjson.ErrNull, "/" + member},
		)
		if member != "schemaVersion" { // a different schema version is reported as an incompatibility
			tests = append(tests, tc{"wrong type of " + member, edit(t, valid, func(m map[string]any) { m[member] = true }), strictjson.ErrInvalidValue, "/" + member})
		}
	}
	tests = append(tests,
		tc{"empty input", nil, strictjson.ErrInvalidJSON, ""},
		tc{"not an object", []byte(`[]`), strictjson.ErrInvalidValue, ""},
		tc{"trailing value", append(slices.Clone(valid), valid...), strictjson.ErrTrailingData, ""},
		tc{"trailing garbage", append(slices.Clone(valid), 'x'), strictjson.ErrTrailingData, ""},
		tc{"another schema version with a trailing value", append(edit(t, valid, func(m map[string]any) { m["schemaVersion"] = 2 }), '{', '}'), strictjson.ErrTrailingData, ""},
		tc{"unknown member", edit(t, valid, func(m map[string]any) { m["extra"] = 1 }), strictjson.ErrUnknownMember, "/extra"},
		tc{"unknown nested member", edit(t, valid, func(m map[string]any) { m["policy"].(map[string]any)["extra"] = 1 }), strictjson.ErrUnknownMember, "/policy/extra"},
		tc{"unknown reference member", edit(t, valid, func(m map[string]any) {
			m["roleDefinitions"].([]any)[0].(map[string]any)["extra"] = 1
		}), strictjson.ErrUnknownMember, "/roleDefinitions/0/extra"},
		tc{"missing nested member", edit(t, valid, func(m map[string]any) { delete(m["policy"].(map[string]any), "sha256") }), strictjson.ErrMissingMember, "/policy/sha256"},
		tc{"duplicate member", []byte(strings.Replace(string(valid), `"runId":`, `"runId":"x","runId":`, 1)), strictjson.ErrDuplicateMember, "/runId"},
		tc{"unknown role", edit(t, valid, func(m map[string]any) {
			m["roleDefinitions"].([]any)[0].(map[string]any)["role"] = "reviewer"
		}), strictjson.ErrInvalidValue, "/roleDefinitions/0/role"},
		tc{"schemaVersion as a string", edit(t, valid, func(m map[string]any) { m["schemaVersion"] = "1" }), strictjson.ErrInvalidValue, "/schemaVersion"},
		tc{"fractional schemaVersion", edit(t, valid, func(m map[string]any) { m["schemaVersion"] = 1.5 }), strictjson.ErrInvalidValue, "/schemaVersion"},
	)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Decode(tt.data)
			if !errors.Is(err, tt.want) {
				t.Fatalf("Decode error = %v, want %v", err, tt.want)
			}
			if !errors.Is(err, ErrInvalidManifest) {
				t.Errorf("error %v does not match ErrInvalidManifest", err)
			}
			if errors.Is(err, ErrIncompatible) {
				t.Errorf("error %v matches ErrIncompatible", err)
			}
			var se *strictjson.Error
			if !errors.As(err, &se) || se.Pointer != tt.pointer {
				t.Errorf("error %v: pointer = %q, want %q", err, pointerOf(err), tt.pointer)
			}
			if !reflect.DeepEqual(got, Manifest{}) {
				t.Errorf("Decode returned a non-zero value with an error: %+v", got)
			}
		})
	}
}

func pointerOf(err error) string {
	var se *strictjson.Error
	if errors.As(err, &se) {
		return se.Pointer
	}
	return "(none)"
}

func TestDecodeReportsAnotherSchemaVersionAsIncompatible(t *testing.T) {
	valid := manifestBytes(t)
	for name, data := range map[string][]byte{
		"version 2":                edit(t, valid, func(m map[string]any) { m["schemaVersion"] = 2 }),
		"version 0":                edit(t, valid, func(m map[string]any) { m["schemaVersion"] = 0 }),
		"negative version":         edit(t, valid, func(m map[string]any) { m["schemaVersion"] = -4 }),
		"members of another shape": []byte(`{"schemaVersion":3,"runId":"run-0123456789abcdef","somethingNew":{"x":[1,2]}}`),
	} {
		t.Run(name, func(t *testing.T) {
			got, err := Decode(data)
			var ie *IncompatibleError
			if !errors.Is(err, ErrIncompatible) || !errors.As(err, &ie) {
				t.Fatalf("Decode error = %v, want an incompatibility", err)
			}
			if errors.Is(err, ErrInvalidManifest) {
				t.Errorf("an incompatibility must not match ErrInvalidManifest: %v", err)
			}
			if len(ie.Mismatches) != 1 || ie.Mismatches[0].Field != "schemaVersion" || ie.Mismatches[0].Supported != "1" {
				t.Errorf("mismatches = %+v", ie.Mismatches)
			}
			if !reflect.DeepEqual(got, Manifest{}) {
				t.Errorf("Decode returned a non-zero value: %+v", got)
			}
		})
	}
	// A Run built with another manifest schema version is incompatible, too.
	in := input(t)
	in.Versions = Current()
	in.Versions.ManifestSchema = 2
	f := build(t, in)
	_, err := Decode(f.Files[len(f.Files)-1].Data)
	var ie *IncompatibleError
	if !errors.As(err, &ie) || ie.Mismatches[0].Recorded != "2" {
		t.Errorf("Decode of a schemaVersion 2 manifest = %v", err)
	}
	if err := CheckCompatible(f.Manifest, Current()); !errors.Is(err, ErrIncompatible) {
		t.Errorf("CheckCompatible of a schemaVersion 2 manifest = %v", err)
	}
}

func clone(m Manifest) Manifest {
	m.RoleDefinitions = slices.Clone(m.RoleDefinitions)
	return m
}

func TestValidateRejects(t *testing.T) {
	base := build(t, input(t)).Manifest
	other := strings.Repeat("0", 64)
	tests := []struct {
		name    string
		mutate  func(*Manifest)
		pointer string
	}{
		{"schemaVersion 0", func(m *Manifest) { m.SchemaVersion = 0 }, "/schemaVersion"},
		{"negative schemaVersion", func(m *Manifest) { m.SchemaVersion = -1 }, "/schemaVersion"},
		{"empty Run ID", func(m *Manifest) { m.RunID = "" }, "/runId"},
		{"malformed Run ID", func(m *Manifest) { m.RunID = "run-1" }, "/runId"},
		{"empty engine version", func(m *Manifest) { m.EngineVersion = "" }, "/engineVersion"},
		{"engine version with a newline", func(m *Manifest) { m.EngineVersion = "1\n2" }, "/engineVersion"},
		{"engine version too long", func(m *Manifest) { m.EngineVersion = strings.Repeat("v", 129) }, "/engineVersion"},
		{"empty workflow ID", func(m *Manifest) { m.WorkflowID = "" }, "/workflowId"},
		{"workflow ID with a space", func(m *Manifest) { m.WorkflowID = "a b" }, "/workflowId"},
		{"workflow version 0", func(m *Manifest) { m.WorkflowVersion = 0 }, "/workflowVersion"},
		{"workflow digest without prefix", func(m *Manifest) { m.WorkflowDigest = other }, "/workflowDigest"},
		{"workflow digest wrong prefix", func(m *Manifest) { m.WorkflowDigest = "sha1:" + other }, "/workflowDigest"},
		{"workflow digest upper case", func(m *Manifest) { m.WorkflowDigest = "sha256:" + strings.ToUpper(strings.Repeat("ab", 32)) }, "/workflowDigest"},
		{"workflow digest short", func(m *Manifest) { m.WorkflowDigest = "sha256:abcd" }, "/workflowDigest"},
		{"workflow digest long", func(m *Manifest) { m.WorkflowDigest = "sha256:" + other + "0" }, "/workflowDigest"},
		{"event schema 0", func(m *Manifest) { m.EventSchemaVersion = 0 }, "/eventSchemaVersion"},
		{"configuration schema 0", func(m *Manifest) { m.ConfigSchemaVersion = 0 }, "/configSchemaVersion"},
		{"policy path", func(m *Manifest) { m.Policy.Path = "policy2.json" }, "/policy/path"},
		{"policy path in a directory", func(m *Manifest) { m.Policy.Path = "definitions/policy.json" }, "/policy/path"},
		{"policy digest", func(m *Manifest) { m.Policy.SHA256 = "XYZ" }, "/policy"},
		{"policy size", func(m *Manifest) { m.Policy.Size = -1 }, "/policy"},
		{"no role definitions", func(m *Manifest) { m.RoleDefinitions = nil }, "/roleDefinitions"},
		{"three role definitions", func(m *Manifest) { m.RoleDefinitions = m.RoleDefinitions[:3] }, "/roleDefinitions"},
		{"five role definitions", func(m *Manifest) { m.RoleDefinitions = append(m.RoleDefinitions, m.RoleDefinitions[0]) }, "/roleDefinitions"},
		{"role definitions in the wrong order", func(m *Manifest) {
			m.RoleDefinitions[0], m.RoleDefinitions[1] = m.RoleDefinitions[1], m.RoleDefinitions[0]
		}, "/roleDefinitions/0/role"},
		{"duplicate role", func(m *Manifest) { m.RoleDefinitions[3].Role = domain.RoleCoordinator }, "/roleDefinitions/3/role"},
		{"definition path of another role", func(m *Manifest) {
			m.RoleDefinitions[1].Definition.Path = domain.RoleDefinitionPath(domain.RoleEvaluator)
		}, "/roleDefinitions/1/definition/path"},
		{"definition path outside definitions", func(m *Manifest) { m.RoleDefinitions[2].Definition.Path = "implementer.md" }, "/roleDefinitions/2/definition/path"},
		{"definition path invalid", func(m *Manifest) { m.RoleDefinitions[2].Definition.Path = "../implementer.md" }, "/roleDefinitions/2/definition"},
		{"definition digest malformed", func(m *Manifest) { m.RoleDefinitions[0].Definition.SHA256 = "abc" }, "/roleDefinitions/0/definition"},
		{"role definitions digest without prefix", func(m *Manifest) { m.RoleDefinitionsDigest = other }, "/roleDefinitionsDigest"},
		{"role definitions digest malformed", func(m *Manifest) { m.RoleDefinitionsDigest = "sha256:xyz" }, "/roleDefinitionsDigest"},
		{"role definitions digest inconsistent", func(m *Manifest) { m.RoleDefinitionsDigest = "sha256:" + other }, "/roleDefinitionsDigest"},
		{"a reference changed without the digest", func(m *Manifest) { m.RoleDefinitions[2].Definition.SHA256 = other }, "/roleDefinitionsDigest"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := clone(base)
			tt.mutate(&m)
			err := m.Validate()
			if !errors.Is(err, ErrInvalidManifest) || !errors.Is(err, strictjson.ErrInvalidValue) {
				t.Fatalf("Validate = %v, want an invalid manifest error", err)
			}
			if got := pointerOf(err); got != tt.pointer {
				t.Errorf("pointer = %q, want %q (%v)", got, tt.pointer, err)
			}
			// The same defect is reported by Decode of the encoded form.
			if data, encErr := strictjson.Encode(m); encErr == nil && tt.name != "no role definitions" {
				// A schema version other than 1 is an incompatibility, not a
				// malformed manifest.
				if _, err := Decode(data); !errors.Is(err, ErrInvalidManifest) && !(tt.pointer == "/schemaVersion" && errors.Is(err, ErrIncompatible)) {
					t.Errorf("Decode of the invalid manifest = %v", err)
				}
			}
		})
	}
	if err := clone(base).Validate(); err != nil {
		t.Errorf("the unchanged manifest is invalid: %v", err)
	}
}

func TestValidateNeverComparesWithTheSupportedVersions(t *testing.T) {
	m := clone(build(t, input(t)).Manifest)
	m.SchemaVersion = 9
	m.WorkflowID = "elsewhere"
	m.WorkflowVersion = 42
	m.WorkflowDigest = "sha256:" + strings.Repeat("c", 64)
	m.EventSchemaVersion = 8
	m.ConfigSchemaVersion = 7
	m.EngineVersion = "far-future"
	if err := m.Validate(); err != nil {
		t.Errorf("Validate compared a version with the supported one: %v", err)
	}
}

func TestCheckCompatible(t *testing.T) {
	m := build(t, input(t)).Manifest
	if err := CheckCompatible(m, Current()); err != nil {
		t.Fatalf("equal versions: %v", err)
	}
	// The engine version is recorded, not compared.
	m2 := clone(m)
	m2.EngineVersion = "a completely different engine"
	if err := CheckCompatible(m2, Current()); err != nil {
		t.Errorf("a different engine version string made the Run incompatible: %v", err)
	}

	changes := []struct {
		field    string
		recorded string
		change   func(*Versions)
	}{
		{"schemaVersion", "1", func(v *Versions) { v.ManifestSchema = 2 }},
		{"workflowId", fsm.WorkflowID, func(v *Versions) { v.WorkflowID = "other" }},
		{"workflowVersion", strconv.Itoa(fsm.WorkflowVersion), func(v *Versions) { v.WorkflowVersion++ }},
		{"workflowDigest", "sha256:" + fsm.Digest(), func(v *Versions) { v.WorkflowDigest = "sha256:" + strings.Repeat("0", 64) }},
		{"eventSchemaVersion", strconv.Itoa(domain.EventSchemaVersion), func(v *Versions) { v.EventSchema++ }},
		{"configSchemaVersion", strconv.Itoa(config.SchemaVersion), func(v *Versions) { v.ConfigSchema++ }},
	}
	for _, c := range changes {
		t.Run(c.field, func(t *testing.T) {
			supported := Current()
			c.change(&supported)
			err := CheckCompatible(m, supported)
			if !errors.Is(err, ErrIncompatible) {
				t.Fatalf("CheckCompatible = %v, want ErrIncompatible", err)
			}
			if errors.Is(err, ErrInvalidManifest) {
				t.Error("an incompatibility matches ErrInvalidManifest")
			}
			var ie *IncompatibleError
			if !errors.As(err, &ie) {
				t.Fatalf("error %T is not an *IncompatibleError", err)
			}
			if len(ie.Mismatches) != 1 || ie.Mismatches[0].Field != c.field || ie.Mismatches[0].Recorded != c.recorded {
				t.Errorf("mismatches = %+v, want only %s recorded as %s", ie.Mismatches, c.field, c.recorded)
			}
			msg := err.Error()
			for _, want := range []string{c.field, "resumption is refused", "no migration procedure"} {
				if !strings.Contains(msg, want) {
					t.Errorf("message %q does not mention %q", msg, want)
				}
			}
		})
	}

	t.Run("every mismatch is listed", func(t *testing.T) {
		supported := Versions{ManifestSchema: 5, WorkflowID: "w", WorkflowVersion: 6, WorkflowDigest: "sha256:" + strings.Repeat("1", 64), EventSchema: 7, ConfigSchema: 8}
		err := CheckCompatible(m, supported)
		var ie *IncompatibleError
		if !errors.As(err, &ie) || len(ie.Mismatches) != 6 {
			t.Fatalf("CheckCompatible = %v", err)
		}
		for _, c := range changes {
			if !strings.Contains(err.Error(), c.field) {
				t.Errorf("message does not list %s: %v", c.field, err)
			}
		}
		if got := ie.Mismatches[0]; got.Supported != "5" || got.Recorded != "1" {
			t.Errorf("first mismatch = %+v", got)
		}
	})
	t.Run("a zero supported value is a mismatch", func(t *testing.T) {
		if err := CheckCompatible(m, Versions{}); !errors.Is(err, ErrIncompatible) {
			t.Errorf("CheckCompatible(m, Versions{}) = %v", err)
		}
	})
}

func TestIncompatibleErrorMatchesOnlyItsSentinel(t *testing.T) {
	err := error(&IncompatibleError{Mismatches: []Mismatch{{Field: "x", Recorded: "1", Supported: "2"}}})
	if !errors.Is(err, ErrIncompatible) {
		t.Error("an *IncompatibleError does not match ErrIncompatible")
	}
	for _, other := range []error{ErrInvalidManifest, config.ErrInvalidConfig, domain.ErrUnsupportedEventSchema, errors.New("x")} {
		if errors.Is(err, other) {
			t.Errorf("an *IncompatibleError matches %v", other)
		}
	}
	wrapped := fmt.Errorf("open the Run: %w", err)
	if !errors.Is(wrapped, ErrIncompatible) {
		t.Error("a wrapped *IncompatibleError does not match ErrIncompatible")
	}
}

func TestCompare(t *testing.T) {
	in := input(t)
	m := build(t, in).Manifest

	t.Run("unchanged", func(t *testing.T) {
		got, err := Compare(m, effective(t), roleDefs())
		if err != nil {
			t.Fatal(err)
		}
		if got.Any() || got.Config || got.Roles != nil {
			t.Errorf("Compare = %+v, want no change", got)
		}
	})
	t.Run("a changed limit", func(t *testing.T) {
		cur := effective(t)
		replans := *cur.Execution.MaxReplans + 1
		cur.Execution.MaxReplans = &replans
		got, err := Compare(m, cur, roleDefs())
		if err != nil {
			t.Fatal(err)
		}
		if !got.Config || len(got.Roles) != 0 || !got.Any() {
			t.Errorf("Compare = %+v, want only the configuration changed", got)
		}
	})
	t.Run("a changed setting that is not a limit", func(t *testing.T) {
		cur := effective(t)
		cur.Agents.Planner = &config.Agent{Model: "other-model"}
		got, err := Compare(m, cur, roleDefs())
		if err != nil || !got.Config {
			t.Errorf("Compare = %+v, %v", got, err)
		}
	})
	t.Run("a changed planner definition", func(t *testing.T) {
		defs := roleDefs()
		defs[1].Content = []byte("a new planner definition")
		got, err := Compare(m, effective(t), defs)
		if err != nil {
			t.Fatal(err)
		}
		if got.Config || !reflect.DeepEqual(got.Roles, []domain.Role{domain.RolePlanner}) || !got.Any() {
			t.Errorf("Compare = %+v, want only the planner", got)
		}
	})
	t.Run("several changes", func(t *testing.T) {
		defs := roleDefs()
		defs[3].Content = []byte("e")
		defs[0].Content = []byte("c")
		cur := effective(t)
		cur.Execution.Timeout = "1h"
		got, err := Compare(m, cur, defs)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Config || !reflect.DeepEqual(got.Roles, []domain.Role{domain.RoleCoordinator, domain.RoleEvaluator}) {
			t.Errorf("Compare = %+v", got)
		}
	})
	t.Run("a definition that changes back is no change", func(t *testing.T) {
		defs := roleDefs()
		got, err := Compare(m, effective(t), defs)
		if err != nil || got.Any() {
			t.Errorf("Compare = %+v, %v", got, err)
		}
	})
	t.Run("nil roles skip the role comparison", func(t *testing.T) {
		got, err := Compare(m, effective(t), nil)
		if err != nil || got.Any() {
			t.Errorf("Compare = %+v, %v", got, err)
		}
		cur := effective(t)
		cur.Execution.Timeout = "1h"
		got, err = Compare(m, cur, nil)
		if err != nil || !got.Config || got.Roles != nil {
			t.Errorf("Compare = %+v, %v", got, err)
		}
	})
	t.Run("the pieces", func(t *testing.T) {
		changed, err := CompareConfig(m, effective(t))
		if err != nil || changed {
			t.Errorf("CompareConfig = %v, %v", changed, err)
		}
		defs := roleDefs()
		defs[2].Content = []byte("different")
		roles, err := CompareRoles(m, defs)
		if err != nil || !reflect.DeepEqual(roles, []domain.Role{domain.RoleImplementer}) {
			t.Errorf("CompareRoles = %v, %v", roles, err)
		}
	})
	t.Run("invalid inputs", func(t *testing.T) {
		defs := roleDefs()
		if _, err := Compare(m, effective(t), defs[:3]); !errors.Is(err, domain.ErrInvalidRoleDefinition) {
			t.Errorf("three definitions: %v", err)
		}
		if _, err := CompareRoles(m, []domain.RoleDefinition{}); !errors.Is(err, domain.ErrInvalidRoleDefinition) {
			t.Errorf("an empty list of definitions: %v", err)
		}
		bad := clone(m)
		bad.RoleDefinitionsDigest = "sha256:" + strings.Repeat("0", 64)
		if _, err := Compare(bad, effective(t), defs); !errors.Is(err, ErrInvalidManifest) {
			t.Errorf("an invalid manifest: %v", err)
		}
		if _, err := CompareConfig(bad, effective(t)); !errors.Is(err, ErrInvalidManifest) {
			t.Errorf("CompareConfig with an invalid manifest: %v", err)
		}
		if _, err := CompareRoles(bad, defs); !errors.Is(err, ErrInvalidManifest) {
			t.Errorf("CompareRoles with an invalid manifest: %v", err)
		}
	})
}

func TestContractTypes(t *testing.T) {
	for _, v := range []any{Manifest{}, RoleDefinitionRef{}} {
		typ := reflect.TypeOf(v)
		t.Run(typ.Name(), func(t *testing.T) {
			if err := strictjson.CheckContractType(typ); err != nil {
				t.Error(err)
			}
		})
	}
}

func TestChangesAny(t *testing.T) {
	for _, tt := range []struct {
		c    Changes
		want bool
	}{
		{Changes{}, false},
		{Changes{Config: true}, true},
		{Changes{Roles: []domain.Role{domain.RolePlanner}}, true},
		{Changes{Roles: []domain.Role{}}, false},
	} {
		if got := tt.c.Any(); got != tt.want {
			t.Errorf("%+v.Any() = %v", tt.c, got)
		}
	}
}
