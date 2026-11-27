package roles

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/shutx-net/bellona/internal/domain"
)

func TestDefinitionsAreValidAndInRoleOrder(t *testing.T) {
	defs := Definitions()
	if err := domain.ValidateRoleDefinitions(defs); err != nil {
		t.Fatalf("the embedded definitions are invalid: %v", err)
	}
	if len(defs) != 4 {
		t.Fatalf("%d definitions, want 4", len(defs))
	}
	for i, r := range domain.Roles() {
		if defs[i].Role != r {
			t.Errorf("definition %d is for %s, want %s", i, defs[i].Role, r)
		}
		text := string(defs[i].Content)
		if !strings.HasPrefix(text, "# ") || !strings.Contains(strings.ToLower(text), strings.ToLower(string(r))) {
			t.Errorf("the definition of %s does not look like its placeholder:\n%s", r, text)
		}
		if !strings.Contains(text, "placeholder") {
			t.Errorf("the definition of %s does not say that it is a placeholder:\n%s", r, text)
		}
		if !strings.HasSuffix(text, "\n") {
			t.Errorf("the definition of %s does not end with a newline", r)
		}
	}
	// The four definitions differ, so that a change of one is detectable.
	seen := make(map[string]domain.Role)
	for _, d := range defs {
		if other, ok := seen[string(d.Content)]; ok {
			t.Errorf("the definitions of %s and %s are identical", d.Role, other)
		}
		seen[string(d.Content)] = d.Role
	}
}

func TestDefinitionsReturnFreshCopies(t *testing.T) {
	first := Definitions()
	want := make([][]byte, len(first))
	for i, d := range first {
		want[i] = bytes.Clone(d.Content)
	}
	// Change the content of every definition and the slice itself.
	for i := range first {
		for j := range first[i].Content {
			first[i].Content[j] = 'X'
		}
	}
	first[0], first[1] = first[1], first[0]
	first[2].Content = nil

	second := Definitions()
	if err := domain.ValidateRoleDefinitions(second); err != nil {
		t.Fatalf("a later call is affected by changes to an earlier result: %v", err)
	}
	for i, d := range second {
		if !bytes.Equal(d.Content, want[i]) {
			t.Errorf("definition %d of a later call changed:\n%s", i, d.Content)
		}
	}
}

func TestValidateRoleDefinitionsRejects(t *testing.T) {
	good := func() []domain.RoleDefinition { return Definitions() }
	tests := []struct {
		name   string
		mutate func([]domain.RoleDefinition) []domain.RoleDefinition
	}{
		{"nil", func([]domain.RoleDefinition) []domain.RoleDefinition { return nil }},
		{"three definitions", func(d []domain.RoleDefinition) []domain.RoleDefinition { return d[:3] }},
		{"five definitions", func(d []domain.RoleDefinition) []domain.RoleDefinition {
			return append(d, domain.RoleDefinition{Role: domain.RoleEvaluator, Content: []byte("x")})
		}},
		{"wrong order", func(d []domain.RoleDefinition) []domain.RoleDefinition {
			d[0], d[1] = d[1], d[0]
			return d
		}},
		{"duplicate role", func(d []domain.RoleDefinition) []domain.RoleDefinition {
			d[3].Role = domain.RoleCoordinator
			return d
		}},
		{"unknown role", func(d []domain.RoleDefinition) []domain.RoleDefinition {
			d[2].Role = "reviewer"
			return d
		}},
		{"empty role", func(d []domain.RoleDefinition) []domain.RoleDefinition {
			d[0].Role = ""
			return d
		}},
		{"empty content", func(d []domain.RoleDefinition) []domain.RoleDefinition {
			d[1].Content = []byte{}
			return d
		}},
		{"nil content", func(d []domain.RoleDefinition) []domain.RoleDefinition {
			d[2].Content = nil
			return d
		}},
		{"invalid UTF-8", func(d []domain.RoleDefinition) []domain.RoleDefinition {
			d[3].Content = []byte("ok \xff")
			return d
		}},
		{"over the limit", func(d []domain.RoleDefinition) []domain.RoleDefinition {
			d[0].Content = bytes.Repeat([]byte("a"), domain.MaxRoleDefinitionBytes+1)
			return d
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := domain.ValidateRoleDefinitions(tt.mutate(good()))
			if !errors.Is(err, domain.ErrInvalidRoleDefinition) {
				t.Errorf("ValidateRoleDefinitions = %v, want ErrInvalidRoleDefinition", err)
			}
		})
	}
	// The limit itself, and multi-byte text, are accepted.
	d := good()
	d[0].Content = bytes.Repeat([]byte("a"), domain.MaxRoleDefinitionBytes)
	d[1].Content = []byte("日本語 §\n")
	if !utf8.Valid(d[1].Content) {
		t.Fatal("test text is not UTF-8")
	}
	if err := domain.ValidateRoleDefinitions(d); err != nil {
		t.Errorf("a definition at the limit or with multi-byte text: %v", err)
	}
}
