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

	"github.com/vnykmshr/shim/internal/adapter"
	"github.com/vnykmshr/shim/internal/config"
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
func (s *stub) BuildRequest(ctx context.Context, body []byte) (*http.Request, error) {
	if s.missingKey {
		return nil, errKeyMissing
	}
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

func doPOST(srv *Server, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	switch path {
	case "/v1/messages":
		srv.handleMessages(rec, req)
	case "/v1/messages/count_tokens":
		srv.handleCountTokens(rec, req)
	}
	return rec
}

func doGET(srv *Server, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	srv.handleHealth(rec, req)
	return rec
}

// --- tests ---

func TestHealth(t *testing.T) {
	s := newStub()
	defer s.close()
	srv, _ := newTestServer(t, s)

	rec := doGET(srv, "/health")
	if rec.Code != 200 {
		t.Errorf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestMessages_Happy(t *testing.T) {
	s := newStub()
	defer s.close()
	srv, logBuf := newTestServer(t, s)

	body := `{"model":"claude-3-5","max_tokens":10,"messages":[{"role":"user","content":"hi there"}]}`
	rec := doPOST(srv, "/v1/messages", body)
	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
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

func TestMessages_StreamRejected(t *testing.T) {
	s := newStub()
	defer s.close()
	srv, logBuf := newTestServer(t, s)

	body := `{"model":"x","max_tokens":1,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	rec := doPOST(srv, "/v1/messages", body)
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "streaming") {
		t.Errorf("body should explain streaming: %s", rec.Body.String())
	}
	if strings.Contains(logBuf.String(), `"hi"`) {
		t.Errorf("log leaked prompt body on 501 path: %s", logBuf.String())
	}
}

func TestMessages_ThinkingRejected(t *testing.T) {
	s := newStub()
	defer s.close()
	srv, _ := newTestServer(t, s)

	body := `{"model":"x","max_tokens":1,"messages":[{"role":"assistant","content":[{"type":"thinking","text":"hmm"}]}]}`
	rec := doPOST(srv, "/v1/messages", body)
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "thinking") {
		t.Errorf("body should explain thinking: %s", rec.Body.String())
	}
}

func TestMessages_Malformed(t *testing.T) {
	s := newStub()
	defer s.close()
	srv, _ := newTestServer(t, s)

	rec := doPOST(srv, "/v1/messages", "not-json")
	if rec.Code != 400 {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestMessages_MissingAPIKey(t *testing.T) {
	s := newStub()
	s.missingKey = true
	defer s.close()
	srv, _ := newTestServer(t, s)

	body := `{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`
	rec := doPOST(srv, "/v1/messages", body)
	if rec.Code != 401 {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "UPSTREAM_API_KEY") {
		t.Errorf("body should mention UPSTREAM_API_KEY: %s", rec.Body.String())
	}
}

func TestMessages_OversizedBody(t *testing.T) {
	s := newStub()
	defer s.close()
	srv, _ := newTestServer(t, s)

	huge := strings.Repeat("x", 8192) // 2× MaxRequestBytes (4096)
	body := `{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"` + huge + `"}]}`
	rec := doPOST(srv, "/v1/messages", body)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "MAX_REQUEST_BYTES") {
		t.Errorf("body should mention MAX_REQUEST_BYTES: %s", rec.Body.String())
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
	rec := doPOST(srv, "/v1/messages", body)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
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
	rec := doPOST(srv, "/v1/messages", body)
	if rec.Code != 401 {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "rejected") {
		t.Errorf("body should explain key rejection: %s", rec.Body.String())
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
	rec := doPOST(srv, "/v1/messages", body)
	if rec.Code != 429 {
		t.Errorf("status = %d, want 429", rec.Code)
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
	rec := doPOST(srv, "/v1/messages", body)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

func TestCountTokens(t *testing.T) {
	s := newStub()
	defer s.close()
	srv, _ := newTestServer(t, s)

	body := `{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"hello world from shim"}]}`
	rec := doPOST(srv, "/v1/messages/count_tokens", body)
	if rec.Code != 200 {
		t.Errorf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out map[string]int
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["input_tokens"] < 1 {
		t.Errorf("input_tokens = %d, want > 0", out["input_tokens"])
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
