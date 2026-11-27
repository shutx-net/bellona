package wordfreq

import (
	"reflect"
	"testing"
)

func TestCount(t *testing.T) {
	tests := []struct {
		name string
		text string
		want map[string]int
	}{
		{"empty input", "", map[string]int{}},
		{"only separators", " \t\n,.;!", map[string]int{}},
		{"single word", "hello", map[string]int{"hello": 1}},
		{"case folding", "Go go GO gO", map[string]int{"go": 4}},
		{"punctuation", "Hello, world! Hello.", map[string]int{"hello": 2, "world": 1}},
		{"digits separate words", "a1b22c", map[string]int{"a": 1, "b": 1, "c": 1}},
		{"non-ASCII letters", "Äpfel äpfel Öl", map[string]int{"äpfel": 2, "öl": 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Count(tt.text); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Count(%q) = %v, want %v", tt.text, got, tt.want)
			}
		})
	}
}
