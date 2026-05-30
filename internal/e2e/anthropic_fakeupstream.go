//go:build e2e

package e2e

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
)

// AnthropicFakeUpstream is a stand-in for a native Anthropic Messages endpoint —
// the upstream the anthropic-passthrough adapter talks to. It mirrors
// FakeUpstream (records requests, serves a programmable CannedResponse) but
// speaks the Anthropic wire shape and is mounted at /v1/messages. Concurrency-safe.
type AnthropicFakeUpstream struct {
	server *httptest.Server
	// baseURL is the value for UPSTREAM_BASE_URL. It has NO /v1 suffix — the
	// anthropic adapter appends "/v1/messages" itself (unlike the deepseek
	// adapter, which appends "/chat/completions" to a /v1-suffixed base).
	baseURL string

	mu       sync.Mutex
	received []RecordedRequest
	next     *CannedResponse
}

// NewAnthropicFakeUpstream starts a fresh fake. Call Close when done.
func NewAnthropicFakeUpstream() *AnthropicFakeUpstream {
	f := &AnthropicFakeUpstream{}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	f.baseURL = f.server.URL
	return f
}

// BaseURL is the value to plug into HarnessOpts.UpstreamURL / UPSTREAM_BASE_URL.
func (f *AnthropicFakeUpstream) BaseURL() string { return f.baseURL }

// Close shuts down the underlying httptest.Server.
func (f *AnthropicFakeUpstream) Close() { f.server.Close() }

// Last returns the most recent request the fake saw, or zero value if none.
func (f *AnthropicFakeUpstream) Last() RecordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.received) == 0 {
		return RecordedRequest{}
	}
	return f.received[len(f.received)-1]
}

// SetNext queues the next response. Cleared after one use; subsequent requests
// fall back to the default Anthropic message response.
func (f *AnthropicFakeUpstream) SetNext(r CannedResponse) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next = &r
}

func (f *AnthropicFakeUpstream) handle(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	parsed := map[string]any{}
	_ = json.Unmarshal(raw, &parsed)

	f.mu.Lock()
	f.received = append(f.received, RecordedRequest{
		Path:    r.URL.Path,
		Headers: r.Header.Clone(),
		Body:    parsed,
		Raw:     raw,
	})
	next := f.next
	f.next = nil
	f.mu.Unlock()

	if next == nil {
		next = &CannedResponse{Status: 200, Body: defaultAnthropicResponse()}
	}

	ct := next.ContentType
	if ct == "" {
		ct = "application/json"
	}
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(next.Status)
	if b, ok := next.Body.([]byte); ok {
		_, _ = w.Write(b)
		return
	}
	_ = json.NewEncoder(w).Encode(next.Body)
}

// defaultAnthropicResponse is the canned Anthropic message served when no
// SetNext is queued. Fixed id + usage for deterministic assertions.
func defaultAnthropicResponse() map[string]any {
	return map[string]any{
		"id":    "msg_e2e_fixed",
		"type":  "message",
		"role":  "assistant",
		"model": "claude-test",
		"content": []map[string]any{
			{"type": "text", "text": "Hello from passthrough."},
		},
		"stop_reason": "end_turn",
		"usage":       map[string]any{"input_tokens": 11, "output_tokens": 4},
	}
}
