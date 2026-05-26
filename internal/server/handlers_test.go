package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/1mb-dev/shim/internal/adapter"
	"github.com/1mb-dev/shim/internal/config"
	"github.com/1mb-dev/shim/internal/measure"
)

// stub is an Adapter test double the server can drive without DeepSeek.
// Each instance gets a unique name so the global registry doesn't collide
// across tests.
type stub struct {
	mu              sync.Mutex
	name            string
	upstreamHandler http.HandlerFunc
	upstream        *httptest.Server
	missingKey      bool
}

var stubCounter int

func newStub() *stub {
	stubCounter++
	s := &stub{name: "stub-" + strconv.Itoa(stubCounter)}
	s.upstreamHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"stub-1","model":"stub-model",
			"choices":[{"index":0,"message":{"role":"assistant","content":"hello back"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}
		}`))
	}
	s.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		h := s.upstreamHandler
		s.mu.Unlock()
		h(w, r)
	}))
	return s
}

func (s *stub) close() { s.upstream.Close() }

func (s *stub) Name() string         { return s.name }
func (s *stub) DefaultModel() string { return "stub-model" }
func (s *stub) MapModel(string) string {
	return s.DefaultModel()
}
func (s *stub) Validate() error {
	if s.missingKey {
		return errKeyMissing
	}
	return nil
}
func (s *stub) BuildRequest(ctx context.Context, body []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.upstream.URL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}
func (s *stub) NormalizeResponse(r *http.Response) ([]byte, error) {
	defer r.Body.Close()
	body, _ := io.ReadAll(r.Body)
	if r.StatusCode >= 400 {
		return body, errUpstreamStatus(r.StatusCode)
	}
	return body, nil
}

// errKeyMissing matches the substring preflightAdapter scans for.
var errKeyMissing = errStr("UPSTREAM_API_KEY not set")

type errStr string

func (e errStr) Error() string { return string(e) }

type errUpstreamStatus int

func (e errUpstreamStatus) Error() string { return "upstream status" }

// failWriter is an http.ResponseWriter that errors on every Write — used to
// force the json.Encode failure path in handleMessages / handleCountTokens.
// Headers + status are recorded so tests can still assert on them.
type failWriter struct {
	headers http.Header
	code    int
}

func newFailWriter() *failWriter { return &failWriter{headers: http.Header{}} }

func (f *failWriter) Header() http.Header  { return f.headers }
func (f *failWriter) WriteHeader(code int) { f.code = code }
func (f *failWriter) Write(_ []byte) (int, error) {
	return 0, errStr("write failed")
}

// failAfterNFlushWriter implements http.ResponseWriter + http.Flusher and
// succeeds for the first n Writes, then errors. Used to force writeSSE
// failure mid-stream (after some events have already been emitted).
type failAfterNFlushWriter struct {
	headers       http.Header
	code          int
	flushed       int
	successesLeft int
}

func newFailAfterNFlushWriter(n int) *failAfterNFlushWriter {
	return &failAfterNFlushWriter{headers: http.Header{}, successesLeft: n}
}

func (f *failAfterNFlushWriter) Header() http.Header  { return f.headers }
func (f *failAfterNFlushWriter) WriteHeader(code int) { f.code = code }
func (f *failAfterNFlushWriter) Flush()               { f.flushed++ }
func (f *failAfterNFlushWriter) Write(b []byte) (int, error) {
	if f.successesLeft <= 0 {
		return 0, errStr("write failed")
	}
	f.successesLeft--
	return len(b), nil
}

// --- harness helpers ---

func newTestServer(t *testing.T, s *stub) (*Server, *bytes.Buffer) {
	t.Helper()
	adapter.Register(s) // unique name per stub; no collision
	logBuf := &bytes.Buffer{}
	log := slog.New(slog.NewJSONHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg := &config.Config{
		BindAddr:        "127.0.0.1",
		Port:            0,
		Adapter:         s.name,
		UpstreamAPIKey:  "sk-test",
		MaxRequestBytes: 4096,
	}
	srv, err := New(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	return srv, logBuf
}

// doPOST drives a real httptest.NewServer wired to the Server's mux, so
// MaxBytesReader, WriteTimeout, Flusher-capable ResponseWriter, and route
// patterns are all exercised through the same stdlib path production uses.
// The direct-handler-invocation pattern survives only for tests that need
// to inject a custom ResponseWriter (failWriter, failAfterNFlushWriter).
func doPOST(t *testing.T, srv *Server, path, body string) *http.Response {
	t.Helper()
	ts := httptest.NewServer(srv.http.Handler)
	t.Cleanup(ts.Close)
	resp, err := http.Post(ts.URL+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return resp
}

func doGET(t *testing.T, srv *Server, path string) *http.Response {
	t.Helper()
	ts := httptest.NewServer(srv.http.Handler)
	t.Cleanup(ts.Close)
	resp, err := http.Get(ts.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return resp
}

// bodyOf reads + closes resp.Body once and returns it as a string. Use this
// where the previous test pattern called rec.Body.String() — call once per
// test, cache the result.
func bodyOf(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

// --- tests ---

func TestHealth(t *testing.T) {
	s := newStub()
	defer s.close()
	srv, _ := newTestServer(t, s)

	resp := doGET(t, srv, "/health")
	body := bodyOf(t, resp)
	if resp.StatusCode != 200 {
		t.Errorf("status = %d", resp.StatusCode)
	}
	if !strings.Contains(body, `"status":"ok"`) {
		t.Errorf("body = %s", body)
	}
}

func TestMessages_Happy(t *testing.T) {
	s := newStub()
	defer s.close()
	srv, logBuf := newTestServer(t, s)

	body := `{"model":"claude-3-5","max_tokens":10,"messages":[{"role":"user","content":"hi there"}]}`
	resp := doPOST(t, srv, "/v1/messages", body)
	respBody := bodyOf(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, respBody)
	}
	var out map[string]any
	_ = json.Unmarshal([]byte(respBody), &out)
	if out["type"] != "message" {
		t.Errorf("type = %v", out["type"])
	}
	if out["role"] != "assistant" {
		t.Errorf("role = %v", out["role"])
	}
	if out["model"] != "claude-3-5" {
		t.Errorf("model echoed wrong: %v", out["model"])
	}
	// Logged successfully with no leaked prompt content.
	if strings.Contains(logBuf.String(), "hi there") {
		t.Errorf("log leaked prompt content: %s", logBuf.String())
	}
}

// sseEvent is a parsed SSE frame from an event: / data: line pair.
type sseEvent struct {
	name string
	data string
}

// parseSSE splits a response body into ordered events. Blank-line separated;
// uses the documented `event:` / `data:` line prefixes.
func parseSSE(body string) []sseEvent {
	var out []sseEvent
	for _, block := range strings.Split(body, "\n\n") {
		if block == "" {
			continue
		}
		var ev sseEvent
		for _, line := range strings.Split(block, "\n") {
			if n, ok := strings.CutPrefix(line, "event: "); ok {
				ev.name = n
			}
			if d, ok := strings.CutPrefix(line, "data: "); ok {
				ev.data = d
			}
		}
		if ev.name != "" {
			out = append(out, ev)
		}
	}
	return out
}

func TestMessages_StreamSucceeds(t *testing.T) {
	s := newStub()
	defer s.close()
	srv, logBuf := newTestServer(t, s)

	body := `{"model":"claude-3-5","max_tokens":1,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	resp := doPOST(t, srv, "/v1/messages", body)
	respBody := bodyOf(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200, body=%s", resp.StatusCode, respBody)
	}
	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}

	events := parseSSE(respBody)
	wantNames := []string{
		"message_start",
		"content_block_start",
		"content_block_delta",
		"content_block_stop",
		"message_delta",
		"message_stop",
	}
	if len(events) != len(wantNames) {
		t.Fatalf("event count = %d, want %d; body:\n%s", len(events), len(wantNames), respBody)
	}
	for i, want := range wantNames {
		if events[i].name != want {
			t.Errorf("event[%d].name = %q, want %q", i, events[i].name, want)
		}
		// Every data payload must be parseable JSON.
		var obj map[string]any
		if err := json.Unmarshal([]byte(events[i].data), &obj); err != nil {
			t.Errorf("event[%d] data not JSON: %q (%v)", i, events[i].data, err)
		}
	}

	// Original prompt content must NOT leak into logs (redaction check
	// applies on streaming path too).
	if strings.Contains(logBuf.String(), `"hi"`) {
		t.Errorf("log leaked prompt body on streaming path: %s", logBuf.String())
	}
}

// TestMessages_StreamUpstreamError verifies that an upstream failure BEFORE
// the SSE stream begins surfaces as a JSON Anthropic-shaped error, not as
// an SSE frame. The client must see a clean 502 with non-streaming content
// type so retry logic isn't confused mid-stream.
func TestMessages_StreamUpstreamError(t *testing.T) {
	s := newStub()
	defer s.close()
	s.mu.Lock()
	s.upstreamHandler = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"error":"unavailable"}`))
	}
	s.mu.Unlock()
	srv, _ := newTestServer(t, s)

	body := `{"model":"x","max_tokens":1,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	resp := doPOST(t, srv, "/v1/messages", body)
	respBody := bodyOf(t, resp)

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct == "text/event-stream" {
		t.Errorf("Content-Type should not be SSE on upstream error: %q", ct)
	}
	if !strings.Contains(respBody, `"error"`) {
		t.Errorf("body should be Anthropic-shaped error: %s", respBody)
	}
}

// TestMessages_StreamUpstreamMalformed: upstream returns 200 + garbage during
// a streaming request. Same expectation as above — JSON 502, not SSE.
func TestMessages_StreamUpstreamMalformed(t *testing.T) {
	s := newStub()
	defer s.close()
	s.mu.Lock()
	s.upstreamHandler = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`not-json`))
	}
	s.mu.Unlock()
	srv, _ := newTestServer(t, s)

	body := `{"model":"x","max_tokens":1,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	resp := doPOST(t, srv, "/v1/messages", body)
	bodyOf(t, resp) // drain
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
}

func TestMessages_ThinkingRejected(t *testing.T) {
	s := newStub()
	defer s.close()
	srv, _ := newTestServer(t, s)

	body := `{"model":"x","max_tokens":1,"messages":[{"role":"assistant","content":[{"type":"thinking","text":"hmm"}]}]}`
	resp := doPOST(t, srv, "/v1/messages", body)
	respBody := bodyOf(t, resp)
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", resp.StatusCode)
	}
	if !strings.Contains(respBody, "thinking") {
		t.Errorf("body should explain thinking: %s", respBody)
	}
}

func TestMessages_Malformed(t *testing.T) {
	s := newStub()
	defer s.close()
	srv, _ := newTestServer(t, s)

	resp := doPOST(t, srv, "/v1/messages", "not-json")
	bodyOf(t, resp) // drain
	if resp.StatusCode != 400 {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// TestNew_AdapterValidateFails: with Validate() at startup, a misconfigured
// adapter blocks Server construction. The 401 surface moves from per-request
// to fail-loud-on-boot.
func TestNew_AdapterValidateFails(t *testing.T) {
	s := newStub()
	s.missingKey = true
	defer s.close()
	adapter.Register(s)
	logBuf := &bytes.Buffer{}
	log := slog.New(slog.NewJSONHandler(logBuf, nil))
	cfg := &config.Config{
		BindAddr:        "127.0.0.1",
		Port:            0,
		Adapter:         s.name,
		UpstreamAPIKey:  "",
		MaxRequestBytes: 4096,
	}
	_, err := New(cfg, log)
	if err == nil {
		t.Fatal("expected error from New when adapter fails Validate, got nil")
	}
	if !strings.Contains(err.Error(), "UPSTREAM_API_KEY") {
		t.Errorf("error should surface adapter's reason, got: %v", err)
	}
}

func TestMessages_OversizedBody(t *testing.T) {
	s := newStub()
	defer s.close()
	srv, _ := newTestServer(t, s)

	huge := strings.Repeat("x", 8192) // 2× MaxRequestBytes (4096)
	body := `{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"` + huge + `"}]}`
	resp := doPOST(t, srv, "/v1/messages", body)
	respBody := bodyOf(t, resp)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", resp.StatusCode)
	}
	if !strings.Contains(respBody, "MAX_REQUEST_BYTES") {
		t.Errorf("body should mention MAX_REQUEST_BYTES: %s", respBody)
	}
}

func TestMessages_Upstream5xx(t *testing.T) {
	s := newStub()
	defer s.close()
	s.mu.Lock()
	s.upstreamHandler = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"error":"unavailable"}`))
	}
	s.mu.Unlock()

	srv, _ := newTestServer(t, s)
	body := `{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`
	resp := doPOST(t, srv, "/v1/messages", body)
	bodyOf(t, resp) // drain
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
}

func TestMessages_UpstreamBadKey(t *testing.T) {
	s := newStub()
	defer s.close()
	s.mu.Lock()
	s.upstreamHandler = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":"invalid api key"}`))
	}
	s.mu.Unlock()

	srv, _ := newTestServer(t, s)
	body := `{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`
	resp := doPOST(t, srv, "/v1/messages", body)
	respBody := bodyOf(t, resp)
	if resp.StatusCode != 401 {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
	if !strings.Contains(respBody, "rejected") {
		t.Errorf("body should explain key rejection: %s", respBody)
	}
}

func TestMessages_UpstreamRateLimit(t *testing.T) {
	s := newStub()
	defer s.close()
	s.mu.Lock()
	s.upstreamHandler = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
	}
	s.mu.Unlock()
	srv, _ := newTestServer(t, s)
	body := `{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`
	resp := doPOST(t, srv, "/v1/messages", body)
	bodyOf(t, resp) // drain
	if resp.StatusCode != 429 {
		t.Errorf("status = %d, want 429", resp.StatusCode)
	}
}

func TestMessages_UpstreamMalformedJSON(t *testing.T) {
	s := newStub()
	defer s.close()
	s.mu.Lock()
	s.upstreamHandler = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`not-json`))
	}
	s.mu.Unlock()
	srv, _ := newTestServer(t, s)
	body := `{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`
	resp := doPOST(t, srv, "/v1/messages", body)
	bodyOf(t, resp) // drain
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
}

func TestMessages_StopSequencesCapped(t *testing.T) {
	s := newStub()
	defer s.close()
	srv, logBuf := newTestServer(t, s)

	body := `{"model":"x","max_tokens":1,"stop_sequences":["a","b","c","d","e","f"],"messages":[{"role":"user","content":"hi"}]}`
	resp := doPOST(t, srv, "/v1/messages", body)
	respBody := bodyOf(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, respBody)
	}
	if !strings.Contains(logBuf.String(), `"stop_sequences truncated"`) {
		t.Errorf("warn log not emitted: %s", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), `"from":6`) {
		t.Errorf("warn log missing from=6: %s", logBuf.String())
	}
}

func TestMessages_ModelRewriteLogged(t *testing.T) {
	s := newStub()
	defer s.close()
	srv, logBuf := newTestServer(t, s)

	body := `{"model":"claude-3-opus","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`
	resp := doPOST(t, srv, "/v1/messages", body)
	respBody := bodyOf(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, respBody)
	}
	if !strings.Contains(logBuf.String(), `"model rewritten"`) {
		t.Errorf("model rewrite not logged: %s", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), `"requested":"claude-3-opus"`) {
		t.Errorf("rewrite log missing requested key: %s", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), `"resolved":"stub-model"`) {
		t.Errorf("rewrite log missing resolved key: %s", logBuf.String())
	}
}

// TestMessages_EmptyModelLogsRewrite: when client sends model="" and shim
// substitutes a default, that IS a rewrite — must log + increment counter.
// Closes the logModelRewrite empty-input thesis-2 violation from
// /code-review 2026-05-26.
func TestMessages_EmptyModelLogsRewrite(t *testing.T) {
	s := newStub()
	defer s.close()
	srv, logBuf := newTestServer(t, s)

	body := `{"model":"","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`
	resp := doPOST(t, srv, "/v1/messages", body)
	respBody := bodyOf(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, respBody)
	}
	if !strings.Contains(logBuf.String(), `"model rewritten"`) {
		t.Errorf("empty input + substitution should log: %s", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), `"requested":""`) {
		t.Errorf("rewrite log missing empty-requested key: %s", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), `"resolved":"stub-model"`) {
		t.Errorf("rewrite log missing resolved key: %s", logBuf.String())
	}
}

// TestInputTokens_ExtractsText: the token counter must see only actual
// prompt text, not the JSON syntax bytes of json.RawMessage. Closes the
// structural-inflation finding from /code-review 2026-05-26.
func TestInputTokens_ExtractsText(t *testing.T) {
	tests := []struct {
		name string
		body string
		// Upper-bound check that catches the JSON-byte-counting regression
		// (which would feed brackets/keys/quotes into the tokenizer and
		// produce a count much higher than the text-only count).
		maxTokens int
	}{
		{
			name:      "string content 'hi'",
			body:      `{"model":"x","messages":[{"role":"user","content":"hi"}]}`,
			maxTokens: 1, // cl100k: "hi" → 1
		},
		{
			name:      "block-array content 'hi'",
			body:      `{"model":"x","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`,
			maxTokens: 1, // must NOT count the bracket/key/quote bytes
		},
		{
			name:      "system block + user 'hi'",
			body:      `{"model":"x","system":"You are helpful.","messages":[{"role":"user","content":"hi"}]}`,
			maxTokens: 5, // cl100k: "You are helpful. hi" → 5
		},
		{
			name:      "explicit null system + 'hi'",
			body:      `{"model":"x","system":null,"messages":[{"role":"user","content":"hi"}]}`,
			maxTokens: 1, // null must be skipped, not counted as 4 chars
		},
		{
			name:      "image block + text 'hi' — image skipped",
			body:      `{"model":"x","messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://example.com/big.png"}},{"type":"text","text":"hi"}]}]}`,
			maxTokens: 1, // image URL bytes must not count
		},
		{
			name:      "non-string non-array content (number) — silently ignored",
			body:      `{"model":"x","messages":[{"role":"user","content":42}]}`,
			maxTokens: 0, // extractText returns ("", false) → no contribution
		},
		{
			name:      "array of wrong shape — extractText unmarshal-fails, returns 0",
			body:      `{"model":"x","messages":[{"role":"user","content":[1,2,3]}]}`,
			maxTokens: 0, // [1,2,3] is valid JSON but not []AnthropicBlock → graceful skip
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newStub()
			defer s.close()
			srv, _ := newTestServer(t, s)

			resp := doPOST(t, srv, "/v1/messages/count_tokens", tc.body)
			respBody := bodyOf(t, resp)
			if resp.StatusCode != 200 {
				t.Fatalf("status = %d, body = %s", resp.StatusCode, respBody)
			}
			var out map[string]int
			if err := json.Unmarshal([]byte(respBody), &out); err != nil {
				t.Fatal(err)
			}
			if out["input_tokens"] > tc.maxTokens {
				t.Errorf("input_tokens = %d exceeds maxTokens %d — JSON syntax bytes leaking into count? body: %s", out["input_tokens"], tc.maxTokens, respBody)
			}
		})
	}
}

// TestMessages_ThinkingPreventsStopSequencesCounter: stop_sequences cap +
// counter must NOT fire when the request is rejected at the thinking-block
// gate. Closes the ordering finding from /code-review 2026-05-26.
func TestMessages_ThinkingPreventsStopSequencesCounter(t *testing.T) {
	s := newStub()
	defer s.close()
	srv, _ := newTestServer(t, s)

	// 6 stop_sequences (over cap) + a thinking block → should 501 with
	// NO rewrites.stop_sequences increment.
	body := `{"model":"x","max_tokens":1,"stop_sequences":["a","b","c","d","e","f"],"messages":[{"role":"assistant","content":[{"type":"thinking","text":"hmm"}]}]}`
	resp := doPOST(t, srv, "/v1/messages", body)
	bodyOf(t, resp)
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", resp.StatusCode)
	}

	// Inspect /v1/metrics — rewrites.stop_sequences must be zero.
	metricsResp := doGET(t, srv, "/v1/metrics")
	metricsBody := bodyOf(t, metricsResp)
	var snap measure.Snapshot
	if err := json.Unmarshal([]byte(metricsBody), &snap); err != nil {
		t.Fatal(err)
	}
	if got := snap.Rewrites[measure.RewriteStopSequences]; got != 0 {
		t.Errorf("rewrites.stop_sequences = %d after thinking-rejected request, want 0", got)
	}
}

// TestMessages_NoRewriteLogWhenIdentical: when client sends the model name
// the adapter happens to resolve to, no rewrite line — keeps the log honest
// rather than noisy.
func TestMessages_NoRewriteLogWhenIdentical(t *testing.T) {
	s := newStub()
	defer s.close()
	srv, logBuf := newTestServer(t, s)

	body := `{"model":"stub-model","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`
	resp := doPOST(t, srv, "/v1/messages", body)
	respBody := bodyOf(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, respBody)
	}
	if strings.Contains(logBuf.String(), `"model rewritten"`) {
		t.Errorf("rewrite log should not fire when names match: %s", logBuf.String())
	}
}

func TestCountTokens_Malformed(t *testing.T) {
	s := newStub()
	defer s.close()
	srv, _ := newTestServer(t, s)

	resp := doPOST(t, srv, "/v1/messages/count_tokens", "not-json")
	respBody := bodyOf(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	if !strings.Contains(respBody, "malformed JSON") {
		t.Errorf("body should mention malformed JSON: %s", respBody)
	}
}

// TestCountTokens_WithSystem: covers inputTokens' system-populated branch
// (only exercised via /v1/messages otherwise). Joint test for inputTokens
// + handleCountTokens with system block present.
func TestCountTokens_WithSystem(t *testing.T) {
	s := newStub()
	defer s.close()
	srv, _ := newTestServer(t, s)

	body := `{"model":"x","max_tokens":1,"system":"You are a calm assistant.","messages":[{"role":"user","content":"hi"}]}`
	resp := doPOST(t, srv, "/v1/messages/count_tokens", body)
	respBody := bodyOf(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, respBody)
	}
	var out map[string]int
	_ = json.Unmarshal([]byte(respBody), &out)
	// System ~28 chars + content ~2 chars + space = ~31 chars / 4 ≈ 7
	if out["input_tokens"] < 6 {
		t.Errorf("input_tokens = %d, want > 6 (system contributes)", out["input_tokens"])
	}
}

func TestCountTokens(t *testing.T) {
	s := newStub()
	defer s.close()
	srv, _ := newTestServer(t, s)

	body := `{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"hello world from shim"}]}`
	resp := doPOST(t, srv, "/v1/messages/count_tokens", body)
	respBody := bodyOf(t, resp)
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, body = %s", resp.StatusCode, respBody)
	}
	var out map[string]int
	_ = json.Unmarshal([]byte(respBody), &out)
	if out["input_tokens"] < 1 {
		t.Errorf("input_tokens = %d, want > 0", out["input_tokens"])
	}
}

// TestMessages_EncodeFailureLogged: /v1/messages succeeded upstream, but the
// final json.Encode of the Anthropic response into the client connection
// fails (client disconnected, write error, etc). Headers + 200 are already
// committed by stdlib server in production — there's no way to reverse-out.
// Loud-fail through the log is the only honest signal.
func TestMessages_EncodeFailureLogged(t *testing.T) {
	s := newStub()
	defer s.close()
	srv, logBuf := newTestServer(t, s)

	body := `{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	fw := newFailWriter()

	srv.handleMessages(fw, req)

	if !strings.Contains(logBuf.String(), `"response encode failed"`) {
		t.Fatalf("expected response encode failed log, got: %s", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), `"path":"/v1/messages"`) {
		t.Errorf("encode-failure log missing path key: %s", logBuf.String())
	}
}

// TestCountTokens_EncodeFailureLogged: same as above for the count_tokens
// path — silent partial-write surface; only the log saves us.
func TestCountTokens_EncodeFailureLogged(t *testing.T) {
	s := newStub()
	defer s.close()
	srv, logBuf := newTestServer(t, s)

	body := `{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(body))
	fw := newFailWriter()

	srv.handleCountTokens(fw, req)

	if !strings.Contains(logBuf.String(), `"response encode failed"`) {
		t.Fatalf("expected response encode failed log, got: %s", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), `"path":"/v1/messages/count_tokens"`) {
		t.Errorf("encode-failure log missing path key: %s", logBuf.String())
	}
}

// TestStream_WriteFailureLogged: streaming path emits a 6-event sequence;
// fail mid-sequence (after the first 3 events succeed) and assert the log
// line fires + no panic. Connection is already in SSE mode so no error
// response is possible — log is the only honest signal.
func TestStream_WriteFailureLogged(t *testing.T) {
	s := newStub()
	defer s.close()
	srv, logBuf := newTestServer(t, s)

	body := `{"model":"x","max_tokens":1,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	fw := newFailAfterNFlushWriter(3) // first 3 events ok, 4th errors

	// must not panic
	srv.handleMessages(fw, req)

	if !strings.Contains(logBuf.String(), `"sse write failed"`) {
		t.Fatalf("expected sse write failed log, got: %s", logBuf.String())
	}
	if fw.flushed < 3 {
		t.Errorf("expected at least 3 successful flushes before failure, got %d", fw.flushed)
	}
}

// TestMetrics_EndToEnd: issue N=50 requests against /v1/messages, then GET
// /v1/metrics and assert the collector saw all N. Verifies (a) the
// collector wiring fires on every request, (b) the route is registered,
// (c) the JSON shape matches what the README will document.
func TestMetrics_EndToEnd(t *testing.T) {
	s := newStub()
	defer s.close()
	srv, _ := newTestServer(t, s)

	const N = 50
	body := `{"model":"claude-3-opus","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`
	for i := 0; i < N; i++ {
		resp := doPOST(t, srv, "/v1/messages", body)
		bodyOf(t, resp)
		if resp.StatusCode != 200 {
			t.Fatalf("request %d failed: status %d", i, resp.StatusCode)
		}
	}

	resp := doGET(t, srv, "/v1/metrics")
	bodyStr := bodyOf(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("metrics status = %d, body = %s", resp.StatusCode, bodyStr)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	// Wire-shape pins so the JSON tags don't drift silently.
	for _, want := range []string{`"latency"`, `"token_delta"`, `"rewrites"`, `"p50"`, `"p95"`, `"p99"`, `"shim_total"`, `"upstream_prompt_total"`, `"upstream_completion_total"`} {
		if !strings.Contains(bodyStr, want) {
			t.Errorf("metrics JSON missing %s; body:\n%s", want, bodyStr)
		}
	}

	var snap measure.Snapshot
	if err := json.Unmarshal([]byte(bodyStr), &snap); err != nil {
		t.Fatalf("snapshot unmarshal: %v\nbody: %s", err, bodyStr)
	}

	if got := snap.Latency["/v1/messages"].N; got != N {
		t.Errorf("latency N = %d, want %d", got, N)
	}
	if snap.Latency["/v1/messages"].P50 <= 0 {
		t.Errorf("P50 = %v, want > 0", snap.Latency["/v1/messages"].P50)
	}

	if got := snap.Tokens["/v1/messages"].N; got != N {
		t.Errorf("token_delta N = %d, want %d", got, N)
	}
	// Stub's hardcoded upstream response: usage.prompt_tokens=3, completion_tokens=2.
	if got := snap.Tokens["/v1/messages"].UpstreamPromptTotal; got != N*3 {
		t.Errorf("UpstreamPromptTotal = %d, want %d", got, N*3)
	}
	if got := snap.Tokens["/v1/messages"].UpstreamCompletionTotal; got != N*2 {
		t.Errorf("UpstreamCompletionTotal = %d, want %d", got, N*2)
	}
	// Stub always maps claude-3-opus → stub-model, so every request rewrites.
	if got := snap.Rewrites[measure.RewriteModel]; got != N {
		t.Errorf("rewrites[%q] = %d, want %d", measure.RewriteModel, got, N)
	}
	// No stop_sequences in the request body, so no truncation.
	if got := snap.Rewrites[measure.RewriteStopSequences]; got != 0 {
		t.Errorf("rewrites[%q] = %d, want 0", measure.RewriteStopSequences, got)
	}
}

// TestBindAddrExplicit asserts Server.Addr is bind+port — caller misuse
// (binding to :8082 instead of 127.0.0.1:8082) would surface here.
func TestBindAddrExplicit(t *testing.T) {
	s := newStub()
	defer s.close()
	srv, _ := newTestServer(t, s)
	if !strings.HasPrefix(srv.Addr(), "127.0.0.1:") {
		t.Errorf("Addr = %q, want 127.0.0.1: prefix", srv.Addr())
	}
}

// TestMessages_UpstreamBadRequest_400 pins writeUpstreamError's default
// branch (non-401/403/429/5xx upstream status) → maps to 502 errAPI.
// Without this fence, the default branch can mis-class silently. Closes
// Jordan-review HIGH finding (handlers.go:192 default branch uncovered).
func TestMessages_UpstreamBadRequest_400(t *testing.T) {
	s := newStub()
	defer s.close()
	s.mu.Lock()
	s.upstreamHandler = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":"upstream rejected prompt"}`))
	}
	s.mu.Unlock()
	srv, _ := newTestServer(t, s)

	body := `{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`
	resp := doPOST(t, srv, "/v1/messages", body)
	respBody := bodyOf(t, resp)
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 (default branch)", resp.StatusCode)
	}
	if !strings.Contains(respBody, "upstream error") {
		t.Errorf("body should surface default-branch message: %s", respBody)
	}
}

// TestStartShutdown exercises Start (which calls http.Server.ListenAndServe)
// and Shutdown end-to-end against a real socket. Picks a free port via
// net.Listen→Close (brief race acceptable at test scale), then dial-polls
// until bound. Closes a 0%-coverage gap on server lifecycle.
func TestStartShutdown(t *testing.T) {
	s := newStub()
	defer s.close()

	// Grab a free port; close immediately so http.Server can bind it.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	probe.Close()

	adapter.Register(s)
	logBuf := &bytes.Buffer{}
	log := slog.New(slog.NewJSONHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg := &config.Config{
		BindAddr:        "127.0.0.1",
		Port:            port,
		Adapter:         s.name,
		UpstreamAPIKey:  "sk-test",
		MaxRequestBytes: 4096,
	}
	srv, err := New(cfg, log)
	if err != nil {
		t.Fatal(err)
	}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start() }()

	// Poll for the socket to come up. 100 × 10ms = 1s budget.
	bound := false
	for i := 0; i < 100; i++ {
		conn, err := net.DialTimeout("tcp", srv.Addr(), 50*time.Millisecond)
		if err == nil {
			conn.Close()
			bound = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !bound {
		t.Fatal("server never bound")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown: %v", err)
	}

	// Channel receive synchronizes with the Start goroutine — only safe to
	// read logBuf after this. Dialing a TCP socket alone doesn't establish
	// happens-before with the goroutine that wrote to logBuf.
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Start returned non-nil after Shutdown: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after Shutdown")
	}

	if !strings.Contains(logBuf.String(), "shim listening") {
		t.Errorf("expected 'shim listening' log; got: %s", logBuf.String())
	}
}

func TestNew_UnknownAdapter(t *testing.T) {
	cfg := &config.Config{Adapter: "ghost", Port: 1, BindAddr: "127.0.0.1"}
	logBuf := &bytes.Buffer{}
	log := slog.New(slog.NewJSONHandler(logBuf, nil))
	if _, err := New(cfg, log); err == nil {
		t.Fatal("expected error for unknown adapter")
	}
}

// TestMessages_BackTranslationFailureNoRecord: upstream returns valid JSON
// with non-zero Usage but empty choices[] → OpenAIToAnthropic rejects with
// "no choices" → handler 500s → token_delta MUST NOT be credited (the
// request never produced a client-visible 200). Fences the sequencing fix
// from /code-review 2026-05-26 backlog MED-1.
func TestMessages_BackTranslationFailureNoRecord(t *testing.T) {
	s := newStub()
	defer s.close()
	s.mu.Lock()
	s.upstreamHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"stub-1","model":"stub-model",
			"choices":[],
			"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}
		}`))
	}
	s.mu.Unlock()
	srv, _ := newTestServer(t, s)

	body := `{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`
	resp := doPOST(t, srv, "/v1/messages", body)
	bodyOf(t, resp) // drain
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (back-translation failure)", resp.StatusCode)
	}

	metricsResp := doGET(t, srv, "/v1/metrics")
	var snap measure.Snapshot
	if err := json.Unmarshal([]byte(bodyOf(t, metricsResp)), &snap); err != nil {
		t.Fatal(err)
	}
	if entry, ok := snap.Tokens["/v1/messages"]; ok {
		t.Errorf("token_delta should be empty after back-translation failure, got %+v", entry)
	}
}
