// Package tokens provides token-count approximations for the
// /v1/messages/count_tokens endpoint. Stage 0 ships a chars/4 heuristic and
// the README discloses this adjacent to the usage.*_tokens documentation. A
// real tokenizer (e.g. cl100k_base via pkoukk/tiktoken-go) lands at the
// measurement-stage boundary, not before.
package tokens

// charsPerToken is the OpenAI-suggested rough average for English text on
// cl100k_base. Source: https://platform.openai.com/tokenizer — "About 4
// characters per token in English."
const charsPerToken = 4

// Approximate returns an approximate token count for s. Never returns less
// than 1 for non-empty input so a 0-length signal indicates empty content.
func Approximate(s string) int {
	if s == "" {
		return 0
	}
	n := len(s) / charsPerToken
	if n < 1 {
		return 1
	}
	return n
}
