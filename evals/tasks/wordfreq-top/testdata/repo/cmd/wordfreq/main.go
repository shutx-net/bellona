// Command wordfreq reads text from standard input and prints how often each
// word occurs, one `word count` line per word, sorted by word.
package main

import (
	"fmt"
	"io"
	"os"
	"sort"

	"example.com/wordfreq"
)

func main() {
	text, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wordfreq:", err)
		os.Exit(1)
	}
	counts := wordfreq.Count(string(text))
	words := make([]string, 0, len(counts))
	for word := range counts {
		words = append(words, word)
	}
	sort.Strings(words)
	for _, word := range words {
		fmt.Printf("%s %d\n", word, counts[word])
	}
}
