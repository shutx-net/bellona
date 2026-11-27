// Package wordfreq counts the words of a text.
package wordfreq

import (
	"strings"
	"unicode"
)

// Count returns how often each word occurs in text. A word is a maximal run
// of letters (unicode.IsLetter), and words are compared in lower case.
func Count(text string) map[string]int {
	counts := make(map[string]int)
	words := strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r)
	})
	for _, word := range words {
		counts[strings.ToLower(word)]++
	}
	return counts
}
