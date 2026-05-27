//go:build e2e

// Package e2e provides a process-boundary integration harness for shim.
// FakeUpstream is a stand-in for the OpenAI-compatible upstream the adapter
// talks to. Records every request it sees and returns programmable
// responses so tests can assert what shim sent and force failure modes
// the unit suite can't reach.
package e2e

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
)

// RecordedRequest captures one POST shim made to the fake upstream.
// Body is the JSON-parsed payload so tests can read fields without
// re-unmarshalling.
type RecordedRequest struct {
	Path    string
	Headers http.Header
	Body    map[string]any
	Raw     []byte
}

// CannedResponse controls the response the fake will return for the next
// request. Body is either a Go value to JSON-encode, or []byte for raw
// (used for SSE and forced-error bodies).
type CannedResponse struct {
	Status      int
	ContentType string // defaults to application/json
	Body        any
}

// FakeUpstream is an httptest.Server that mirrors the OpenAI chat
// completions API surface shim talks to. Concurrency-safe.
type FakeUpstream struct {
	server *httptest.Server
	// URL is the value to plug into UPSTREAM_BASE_URL. Includes the /v1
	// suffix so the adapter's "/chat/completions" suffix matches the real
	// DeepSeek endpoint shape.
	URL string

	mu                 sync.Mutex
	received           []RecordedRequest
	next               *CannedResponse
	toolContinuationOn bool // see EnforceToolContinuationContract
}

// NewFakeUpstream starts a fresh fake upstream. Call Close when done.
func NewFakeUpstream() *FakeUpstream {
	f := &FakeUpstream{}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	f.URL = f.server.URL + "/v1"
	return f
}

// Close shuts down the underlying httptest.Server.
func (f *FakeUpstream) Close() { f.server.Close() }

// Received returns a copy of all requests the fake has seen.
func (f *FakeUpstream) Received() []RecordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]RecordedRequest, len(f.received))
	copy(out, f.received)
	return out
}

// Last returns the most recent request the fake saw, or zero value if
// none.
func (f *FakeUpstream) Last() RecordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.received) == 0 {
		return RecordedRequest{}
	}
	return f.received[len(f.received)-1]
}

// Reset clears the recorded-request log and any queued canned response.
func (f *FakeUpstream) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.received = nil
	f.next = nil
}

// SetNext queues the next response. Cleared after one use; subsequent
// requests fall back to the default chat-completion response.
func (f *FakeUpstream) SetNext(r CannedResponse) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next = &r
}

// EnforceToolContinuationContract turns on a proxy for DeepSeek's actual
// rule that "thinking active on prior turn → reasoning_content required
// on continuation when prior turn made tool_calls." Real DeepSeek returns
// the canonical 400 below when this rule fires; the fake mimics it when:
// the request body contains tool_calls in any assistant message in
// `messages` AND the request did NOT explicitly set
// thinking={type:"disabled"}.
//
// This is a PROXY for the real contract, not a faithful model — DeepSeek
// actually checks whether reasoning_content was present on the prior
// assistant turn that made tool_calls. The proxy collapses that to
// "thinking is not disabled" because Stage 2.6b's whole job is to ensure
// shim sends thinking=disabled on every outbound, which sidesteps the
// real contract entirely. If you ever need to test the real contract
// (Stage 2.6c), reshape this enforcement.
func (f *FakeUpstream) EnforceToolContinuationContract(on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.toolContinuationOn = on
}

func (f *FakeUpstream) handle(w http.ResponseWriter, r *http.Request) {
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
	contractOn := f.toolContinuationOn
	f.mu.Unlock()

	if contractOn && violatesToolContinuationContract(parsed) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":{"message":"The ` + "`reasoning_content`" + ` in the thinking mode must be passed back to the API.","type":"invalid_request_error","param":null,"code":"invalid_request_error"}}`))
		return
	}

	if next == nil {
		next = &CannedResponse{Status: 200, Body: defaultChatResponse()}
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

// violatesToolContinuationContract returns true when the request has
// tool_calls in any assistant message of `messages` AND thinking is not
// explicitly disabled. See EnforceToolContinuationContract for the proxy
// semantics — this is NOT a faithful model of DeepSeek's actual rule.
func violatesToolContinuationContract(body map[string]any) bool {
	thinkingDisabled := false
	if t, ok := body["thinking"].(map[string]any); ok {
		if v, ok := t["type"].(string); ok && v == "disabled" {
			thinkingDisabled = true
		}
	}
	if thinkingDisabled {
		return false
	}
	msgs, ok := body["messages"].([]any)
	if !ok {
		return false
	}
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if mm["role"] != "assistant" {
			continue
		}
		if tcs, ok := mm["tool_calls"].([]any); ok && len(tcs) > 0 {
			return true
		}
	}
	return false
}

// DefaultChatResponse returns the canned OpenAI ChatCompletions payload
// the fake serves when no SetNext is queued. Fixed ID + usage so the
// streaming golden file is deterministic.
func defaultChatResponse() map[string]any {
	return map[string]any{
		"id":      "chatcmpl-e2e-fixed",
		"object":  "chat.completion",
		"created": 1700000000,
		"model":   "deepseek-chat",
		"choices": []map[string]any{{
			"index": 0,
			"message": map[string]any{
				"role":    "assistant",
				"content": "Hello there.",
			},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{
			"prompt_tokens":     7,
			"completion_tokens": 3,
			"total_tokens":      10,
		},
	}
}
