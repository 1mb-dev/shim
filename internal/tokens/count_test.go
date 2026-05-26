package tokens

import (
	"strings"
	"testing"
)

func TestInit_IdempotentNoError(t *testing.T) {
	if err := Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := Init(); err != nil {
		t.Fatalf("Init (second call): %v", err)
	}
}

func TestCount(t *testing.T) {
	if err := Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	tests := []struct {
		name string
		in   string
		// want is the exact cl100k_base count. Values verified against the
		// reference tokenizer; if upstream changes the encoding (it has not
		// since GPT-4 launch), these are the canaries.
		want int
	}{
		{"empty", "", 0},
		{"single ascii char", "a", 1},
		{"hello", "hello", 1},
		{"hello world", "hello world", 2},
		{"two words punctuation", "Hello, world!", 4},
		// "こんにちは" exists as a single merged token in cl100k vocab
		// (one of the surprises — confirms we're hitting real BPE, not a
		// chars-based fallback). Token ID 90115.
		{"japanese konnichiwa", "こんにちは", 1},
		// Code-like input (cl100k handles whitespace + indent specially).
		{"go func snippet", "func main() {}", 4},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Count(tc.in)
			if got != tc.want {
				t.Errorf("Count(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestCount_LongStringStable(t *testing.T) {
	if err := Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	// ~4000 chars repeated paragraph. Real tokenizer should produce a count
	// well under chars/4 (cl100k is denser on English prose).
	para := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 90)
	n := Count(para)
	if n < 500 || n > 1200 {
		// chars/4 would give ~1012; cl100k typically gives ~810 for this text.
		// Wide range — this is a stability/range check, not an exact match.
		t.Errorf("Count of 4000-char paragraph = %d, want in [500, 1200]", n)
	}
}
