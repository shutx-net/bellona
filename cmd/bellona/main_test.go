package main

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/shutx-net/bellona/internal/domain"
	"github.com/shutx-net/bellona/internal/fsm"
	"github.com/shutx-net/bellona/internal/version"
)

func TestVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"version"}, &stdout, &stderr); code != 0 {
		t.Fatalf("status = %d, want 0 (stderr %q)", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want nothing", stderr.String())
	}
	want := fmt.Sprintf("bellona %s\nworkflow %s %d\nevent schema %d\n",
		version.Engine, fsm.WorkflowID, fsm.WorkflowVersion, domain.EventSchemaVersion)
	if got := stdout.String(); got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
	lines := strings.Split(strings.TrimSuffix(stdout.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("version printed %d lines, want 3: %q", len(lines), lines)
	}
	for i, prefix := range []string{"bellona ", "workflow ", "event schema "} {
		if !strings.HasPrefix(lines[i], prefix) {
			t.Errorf("line %d = %q, want prefix %q", i+1, lines[i], prefix)
		}
	}
}

func TestUsage(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"no arguments", nil},
		{"empty arguments", []string{}},
		{"unknown subcommand", []string{"run"}},
		{"version with an argument", []string{"version", "x"}},
		{"version twice", []string{"version", "version"}},
		{"upper-case version", []string{"Version"}},
		{"flag", []string{"--version"}},
		{"empty argument", []string{""}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(tt.args, &stdout, &stderr); code != 2 {
				t.Errorf("status = %d, want 2", code)
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want nothing", stdout.String())
			}
			if got, want := stderr.String(), "usage: bellona version\n"; got != want {
				t.Errorf("stderr = %q, want %q", got, want)
			}
		})
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestVersionWriteFailure(t *testing.T) {
	var stderr bytes.Buffer
	if code := run([]string{"version"}, failingWriter{}, &stderr); code != 1 {
		t.Errorf("status = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "disk full") {
		t.Errorf("stderr = %q, want the write error", stderr.String())
	}
}
