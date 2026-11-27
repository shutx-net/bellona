package wordfreq

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// The acceptance tests are hidden from the variant under evaluation. They
// assert only what the request of the task states, and their names carry the
// prefix TestAcceptance so that they cannot collide with the tests that a
// solution adds.

func TestAcceptanceTopOrder(t *testing.T) {
	counts := map[string]int{"pear": 2, "apple": 3, "fig": 2, "kiwi": 1, "date": 3, "cherry": 2}
	want := []WordCount{
		{Word: "apple", Count: 3},
		{Word: "date", Count: 3},
		{Word: "cherry", Count: 2},
		{Word: "fig", Count: 2},
		{Word: "pear", Count: 2},
		{Word: "kiwi", Count: 1},
	}
	for n := 1; n <= len(want); n++ {
		assertAcceptanceTop(t, counts, n, want[:n])
	}
}

func TestAcceptanceTopMoreThanAvailable(t *testing.T) {
	counts := map[string]int{"b": 1, "a": 1, "c": 5}
	want := []WordCount{{Word: "c", Count: 5}, {Word: "a", Count: 1}, {Word: "b", Count: 1}}
	assertAcceptanceTop(t, counts, 4, want)
	assertAcceptanceTop(t, counts, 100, want)
}

func TestAcceptanceTopNothingRequested(t *testing.T) {
	counts := map[string]int{"a": 2, "b": 1}
	assertAcceptanceTop(t, counts, 0, nil)
	assertAcceptanceTop(t, counts, -1, nil)
	assertAcceptanceTop(t, counts, -100, nil)
}

func TestAcceptanceTopEmptyMap(t *testing.T) {
	assertAcceptanceTop(t, map[string]int{}, 3, nil)
	assertAcceptanceTop(t, nil, 3, nil)
	assertAcceptanceTop(t, map[string]int{}, 0, nil)
}

func assertAcceptanceTop(t *testing.T, counts map[string]int, n int, want []WordCount) {
	t.Helper()
	got := Top(counts, n)
	if len(got) != len(want) {
		t.Errorf("Top(%v, %d) returned %d words %v, want %d words %v", counts, n, len(got), got, len(want), want)
		return
	}
	for i := range want {
		if got[i].Word != want[i].Word || got[i].Count != want[i].Count {
			t.Errorf("Top(%v, %d)[%d] = %s %d, want %s %d", counts, n, i, got[i].Word, got[i].Count, want[i].Word, want[i].Count)
		}
	}
}

// runAcceptanceCommand runs the command of the repository with the given
// arguments, feeds it input on standard input and returns its output.
func runAcceptanceCommand(t *testing.T, input string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", append([]string{"run", "./cmd/wordfreq"}, args...)...)
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go run ./cmd/wordfreq %s: %v", strings.Join(args, " "), err)
	}
	return string(out)
}

const acceptanceText = "The cat saw the dog. The bird saw the cat."

func TestAcceptanceCommandWithoutFlag(t *testing.T) {
	want := "bird 1\ncat 2\ndog 1\nsaw 2\nthe 4\n"
	if got := runAcceptanceCommand(t, acceptanceText); got != want {
		t.Errorf("output without -top = %q, want %q", got, want)
	}
}

func TestAcceptanceCommandTopFlag(t *testing.T) {
	tests := []struct {
		n    string
		want string
	}{
		{"1", "the 4\n"},
		{"2", "the 4\ncat 2\n"},
		{"3", "the 4\ncat 2\nsaw 2\n"},
		{"4", "the 4\ncat 2\nsaw 2\nbird 1\n"},
		{"99", "the 4\ncat 2\nsaw 2\nbird 1\ndog 1\n"},
	}
	for _, tt := range tests {
		if got := runAcceptanceCommand(t, acceptanceText, "-top", tt.n); got != tt.want {
			t.Errorf("output with -top %s = %q, want %q", tt.n, got, tt.want)
		}
	}
}
