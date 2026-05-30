//go:build e2e

package e2e

import (
	"fmt"
	"strings"
	"testing"
)

// Full-process passthrough e2e: a real ./shim with ADAPTER=anthropic in front of
// an Anthropic-shaped fake. Proves the identity dialect end-to-end through the
// binary — request/response/SSE forwarded byte-verbatim, and (the v0.3.1
// deliverable) upstream errors passed through with status + body intact.

// ---- Passthrough case 1: non-stream verbatim (req + resp) ----

func TestE2E_Passthrough_NonStreamVerbatim(t *testing.T) {
	withBudget(t, perCaseBudget, func() {
		up := NewAnthropicFakeUpstream()
		t.Cleanup(up.Close)

		// Response carries fields shim does NOT model (stop_sequence,
		// cache_read_input_tokens) — they must survive the round-trip, proving
		// the body is forwarded verbatim, not re-encoded through shim's struct.
		respBody := `{"id":"msg_x","type":"message","role":"assistant","model":"claude-3-7","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":5,"output_tokens":2,"cache_read_input_tokens":3}}`
		up.SetNext(CannedResponse{Status: 200, Body: []byte(respBody)})

		h := Start(t, HarnessOpts{Adapter: "anthropic", UpstreamURL: up.BaseURL()})

		// Request carries an unmodeled field (top_k) — must reach the upstream.
		status, body := postJSON(t, h.URL+"/v1/messages", map[string]any{
			"model":      "claude-3-7-sonnet",
			"max_tokens": 50,
			"top_k":      5,
			"messages":   []map[string]any{{"role": "user", "content": "hi"}},
		})
		if status != 200 {
			t.Fatalf("status=%d body=%s", status, body)
		}
		if string(body) != respBody {
			t.Errorf("response not byte-verbatim:\n got: %s\nwant: %s", body, respBody)
		}

		got := up.Last()
		if got.Path != "/v1/messages" {
			t.Errorf("upstream path = %q, want /v1/messages", got.Path)
		}
		if tk, ok := got.Body["top_k"].(float64); !ok || tk != 5 {
			t.Errorf("unmodeled top_k not forwarded verbatim: %v", got.Body["top_k"])
		}
		// x-api-key auth + injected anthropic-version (client omitted it).
		if got.Headers.Get("x-api-key") != "test-key" {
			t.Errorf("x-api-key = %q, want test-key", got.Headers.Get("x-api-key"))
		}
		if got.Headers.Get("anthropic-version") != "2023-06-01" {
			t.Errorf("anthropic-version not injected: %q", got.Headers.Get("anthropic-version"))
		}
		if !strings.Contains(h.Stderr(), `"msg":"anthropic-version injected"`) {
			t.Errorf("version-inject not logged (thesis-2): %s", h.Stderr())
		}
	})
}

// ---- Passthrough case 2: stream byte-identical + stream flag forwarded ----

func TestE2E_Passthrough_StreamVerbatim(t *testing.T) {
	withBudget(t, perCaseBudget, func() {
		up := NewAnthropicFakeUpstream()
		t.Cleanup(up.Close)

		sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"usage\":{\"input_tokens\":9,\"output_tokens\":0}}}\n\n" +
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":5}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
		up.SetNext(CannedResponse{Status: 200, ContentType: "text/event-stream", Body: []byte(sse)})

		h := Start(t, HarnessOpts{Adapter: "anthropic", UpstreamURL: up.BaseURL()})

		status, raw := postJSON(t, h.URL+"/v1/messages", map[string]any{
			"model":      "claude-3-7-sonnet",
			"max_tokens": 50,
			"stream":     true,
			"messages":   []map[string]any{{"role": "user", "content": "hi"}},
		})
		if status != 200 {
			t.Fatalf("status=%d body=%s", status, raw)
		}
		// True byte-passthrough: client sees the upstream SSE bytes unchanged
		// (contrast TestE2E_HappyStream, where deepseek buffers + re-synthesizes).
		if string(raw) != sse {
			t.Errorf("stream not byte-identical:\n got: %s\nwant: %s", raw, sse)
		}
		// Passthrough forwards stream=true upstream (deepseek strips it).
		if s, ok := up.Last().Body["stream"].(bool); !ok || !s {
			t.Errorf("upstream should see stream=true on passthrough: %+v", up.Last().Body)
		}

		// Usage sniffed from message_start/message_delta is recorded.
		after := h.Metrics()
		if after.Tokens["/v1/messages"].UpstreamCompletionTotal < 5 {
			t.Errorf("output token usage not sniffed from SSE: %+v", after.Tokens["/v1/messages"])
		}
	})
}

// ---- Passthrough case 3: error transparency (the v0.3.1 deliverable) ----
// Each status the OpenAI dialect would RE-MAP (400→502, 500→502, 529→502) must
// pass through verbatim here — status and Anthropic error body intact.

func TestE2E_Passthrough_ErrorVerbatim(t *testing.T) {
	for _, upstreamStatus := range []int{400, 429, 500, 529} {
		t.Run(fmt.Sprintf("status_%d", upstreamStatus), func(t *testing.T) {
			withBudget(t, perCaseBudget, func() {
				up := NewAnthropicFakeUpstream()
				t.Cleanup(up.Close)

				errBody := fmt.Sprintf(`{"type":"error","error":{"type":"overloaded_error","message":"upstream said %d"}}`, upstreamStatus)
				up.SetNext(CannedResponse{Status: upstreamStatus, Body: []byte(errBody)})

				h := Start(t, HarnessOpts{Adapter: "anthropic", UpstreamURL: up.BaseURL()})

				status, body := postJSON(t, h.URL+"/v1/messages", map[string]any{
					"model":      "claude-3-7-sonnet",
					"max_tokens": 10,
					"messages":   []map[string]any{{"role": "user", "content": "hi"}},
				})
				if status != upstreamStatus {
					t.Errorf("client status = %d, want %d (verbatim, not re-mapped)", status, upstreamStatus)
				}
				if string(body) != errBody {
					t.Errorf("error body not verbatim:\n got: %s\nwant: %s", body, errBody)
				}
				// Still measured + logged as an upstream error (thesis-1).
				if got := h.Metrics().UpstreamErrors["/v1/messages"].ByStatus[fmt.Sprintf("%d", upstreamStatus)]; got < 1 {
					t.Errorf("upstream_errors.by_status[%d] = %d, want >=1", upstreamStatus, got)
				}
			})
		})
	}
}
