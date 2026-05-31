package translate

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// Direct unit tests for the OpenAI-dialect seam wrappers (anthropicOpenAI's
// ToUpstream/FromUpstream/StreamChunks). The translation internals are covered
// in translate_test.go; these pin the WRAPPER contract — model injection, the
// stopCapped report, the bytes-not-struct return, and the SSE iterator — which
// was otherwise exercised only via the server/e2e tests (0% in the unit
// profile). Mirrors the identity-side tests in passthrough_test.go.

const openAIRespBody = `{"choices":[{"index":0,"message":{"role":"assistant","content":"hi there"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`

func TestAnthropicOpenAI_ToUpstream_ModelAndStopCap(t *testing.T) {
	req := &AnthropicRequest{
		Model: "claude-opus", MaxTokens: 1,
		StopSequences: []string{"a", "b", "c", "d", "e"}, // 5 > cap of 4
		Messages:      []AnthropicMessage{{Role: "user", Content: mustJSON("hi")}},
	}
	body, stopCapped, err := anthropicOpenAI{}.ToUpstream(req, nil, "deepseek-v4-pro")
	if err != nil {
		t.Fatal(err)
	}
	if stopCapped != 1 {
		t.Errorf("stopCapped = %d, want 1 (5 - cap 4)", stopCapped)
	}
	var o OpenAIRequest
	if err := json.Unmarshal(body, &o); err != nil {
		t.Fatalf("ToUpstream body not OpenAIRequest JSON: %v", err)
	}
	if o.Model != "deepseek-v4-pro" {
		t.Errorf("model = %q, want injected deepseek-v4-pro", o.Model)
	}
	if o.Stream {
		t.Error("Stream = true, want false (buffer-then-restream MVP)")
	}
	if len(o.Stop) != maxStopSequences {
		t.Errorf("stop len = %d, want %d (capped)", len(o.Stop), maxStopSequences)
	}
}

func TestAnthropicOpenAI_FromUpstream_BytesAndUsage(t *testing.T) {
	body, usage, err := anthropicOpenAI{}.FromUpstream([]byte(openAIRespBody), "claude-opus-4-8")
	if err != nil {
		t.Fatal(err)
	}
	if usage.InputTokens != 3 || usage.OutputTokens != 2 {
		t.Errorf("usage = %+v, want input 3 output 2", usage)
	}
	// bytes-not-struct: the return is raw JSON, which must parse as an Anthropic
	// response echoing the client's model.
	var resp AnthropicResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("FromUpstream body not AnthropicResponse JSON: %v", err)
	}
	if resp.Role != "assistant" || resp.Model != "claude-opus-4-8" {
		t.Errorf("resp role/model = %q/%q, want assistant/claude-opus-4-8", resp.Role, resp.Model)
	}
	if len(resp.Content) == 0 || resp.Content[0].Text != "hi there" {
		t.Errorf("content = %+v, want first block text 'hi there'", resp.Content)
	}
}

func TestAnthropicOpenAI_StreamChunks_CanonicalSequence(t *testing.T) {
	resp := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(openAIRespBody))}
	next, usage, err := anthropicOpenAI{}.StreamChunks(resp, "claude-opus-4-8")
	if err != nil {
		t.Fatal(err)
	}
	if usage == nil || usage.InputTokens != 3 {
		t.Fatalf("usage = %+v, want non-nil with input 3 (buffered upstream → known up front)", usage)
	}
	var all strings.Builder
	n := 0
	for {
		b, ok, err := next()
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		all.Write(b)
		n++
	}
	got := all.String()
	for _, want := range []string{"message_start", "content_block_delta", "message_stop"} {
		if !strings.Contains(got, want) {
			t.Errorf("stream missing %q\n---\n%s", want, got)
		}
	}
	if n < 6 {
		t.Errorf("event count = %d, want >= 6 (canonical Anthropic SSE sequence)", n)
	}
}
