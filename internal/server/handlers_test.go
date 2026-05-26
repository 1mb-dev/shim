package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/1mb-dev/shim/internal/adapter"
	"github.com/1mb-dev/shim/internal/config"
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

func TestNew_UnknownAdapter(t *testing.T) {
	cfg := &config.Config{Adapter: "ghost", Port: 1, BindAddr: "127.0.0.1"}
	logBuf := &bytes.Buffer{}
	log := slog.New(slog.NewJSONHandler(logBuf, nil))
	if _, err := New(cfg, log); err == nil {
		t.Fatal("expected error for unknown adapter")
	}
}
