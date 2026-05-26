package tokens

import (
	"fmt"
	"sync"

	tiktoken "github.com/pkoukk/tiktoken-go"
	tloader "github.com/pkoukk/tiktoken-go-loader"
)

// encodingName is the BPE encoding shim uses for input-side token counting.
// cl100k_base is OpenAI's GPT-3.5/GPT-4 tokenizer. DeepSeek may use a
// different scheme on the upstream side, so the result is a closer
// approximation than chars/4 but NOT a trusted count. README "Measurement"
// section discloses this adjacent to /v1/metrics token_delta.
const encodingName = "cl100k_base"

var (
	encoder    *tiktoken.Tiktoken
	initOnce   sync.Once
	initErr    error
	initCalled bool
)

// Init loads the cl100k_base BPE table. Safe to call multiple times;
// repeated calls are no-ops. Returns the first init error if loading
// failed. Call from server.New so a corrupt embed fails startup loudly,
// not on first request.
func Init() error {
	initOnce.Do(func() {
		tiktoken.SetBpeLoader(tloader.NewOfflineLoader())
		enc, err := tiktoken.GetEncoding(encodingName)
		if err != nil {
			initErr = fmt.Errorf("tokens: load %s: %w", encodingName, err)
			return
		}
		encoder = enc
		initCalled = true
	})
	return initErr
}

// Count returns the number of BPE tokens in s under cl100k_base. Returns 0
// for the empty string. Panics if Init has not been called successfully —
// thesis-2 loud-fail: a count without init is a wiring bug.
func Count(s string) int {
	if !initCalled {
		panic("tokens: Count called before Init (or Init failed)")
	}
	if s == "" {
		return 0
	}
	return len(encoder.EncodeOrdinary(s))
}
