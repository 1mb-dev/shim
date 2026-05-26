package deepseek

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testAdapter returns a freshly-configured impl for isolation, leaving the
// global singleton untouched.
func testAdapter(baseURL, apiKey string) *impl {
	return &impl{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		client:  newClient(),
	}
}

func TestNameAndDefaultModel(t *testing.T) {
	a := testAdapter("https://api.deepseek.com/v1", "k")
	if a.Name() != "deepseek" {
		t.Errorf("Name = %q", a.Name())
	}
	if a.DefaultModel() != "deepseek-chat" {
		t.Errorf("DefaultModel = %q", a.DefaultModel())
	}
	a.modelOverride = "deepseek-reasoner"
	if a.DefaultModel() != "deepseek-reasoner" {
		t.Errorf("override not honoured: %q", a.DefaultModel())
	}
}

func TestMapModel(t *testing.T) {
	a := testAdapter("x", "y")
	// Anthropic name → DeepSeek default; we don't route by claude-* prefix.
	if got := a.MapModel("claude-3-5-sonnet-20240620"); got != "deepseek-chat" {
		t.Errorf("MapModel(claude-...) = %q", got)
	}
	if got := a.MapModel("anything"); got != "deepseek-chat" {
		t.Errorf("MapModel(anything) = %q", got)
	}
}

func TestBuildRequest(t *testing.T) {
	a := testAdapter("https://api.deepseek.com/v1/", "sk-test")
	req, err := a.BuildRequest(context.Background(), []byte(`{"model":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if req.Method != http.MethodPost {
		t.Errorf("method = %q", req.Method)
	}
	wantURL := "https://api.deepseek.com/v1/chat/completions"
	if req.URL.String() != wantURL {
		t.Errorf("url = %q, want %q (trailing slash should be trimmed)", req.URL.String(), wantURL)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer sk-test" {
		t.Errorf("Authorization = %q", got)
	}
	if got := req.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	body, _ := io.ReadAll(req.Body)
	if string(body) != `{"model":"x"}` {
		t.Errorf("body = %q", body)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		apiKey  string
		wantSub string // "" = expect nil
	}{
		{"valid", "https://api.deepseek.com/v1", "sk-test", ""},
		{"missing baseURL", "", "sk-test", "not configured"},
		{"missing apiKey", "https://api.deepseek.com/v1", "", "UPSTREAM_API_KEY"},
		{"both missing — baseURL check wins", "", "", "not configured"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := testAdapter(tc.baseURL, tc.apiKey)
			err := a.Validate()
			if tc.wantSub == "" {
				if err != nil {
					t.Errorf("got %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("got %v, want substring %q", err, tc.wantSub)
			}
		})
	}
}

func TestBuildRequest_NotConfigured(t *testing.T) {
	a := &impl{client: newClient()} // empty baseURL
	if _, err := a.BuildRequest(context.Background(), []byte(`{}`)); err == nil {
		t.Fatal("expected error for unconfigured adapter")
	}
}

func TestBuildRequest_NoAPIKey(t *testing.T) {
	a := testAdapter("https://x", "")
	if _, err := a.BuildRequest(context.Background(), []byte(`{}`)); err == nil {
		t.Fatal("expected error for missing API key")
	}
}

func TestNormalizeResponse_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	a := testAdapter(srv.URL, "k")
	body, err := a.NormalizeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != `{"ok":true}` {
		t.Errorf("body = %s", body)
	}
}

func TestNormalizeResponse_Non2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"error":"unavailable"}`))
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	a := testAdapter(srv.URL, "k")
	body, err := a.NormalizeResponse(resp)
	if err == nil {
		t.Fatal("expected error on 503")
	}
	if string(body) != `{"error":"unavailable"}` {
		t.Errorf("body lost: %s", body)
	}
}

// TestHappyPath wires BuildRequest + Do + NormalizeResponse end-to-end
// against a stub upstream serving the recorded fixture.
func TestHappyPath(t *testing.T) {
	fixture := loadFixture(t, "deepseek-helloworld.json")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify shim sent what we expect.
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("Authorization = %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"model"`) {
			t.Errorf("body missing model key: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write(fixture)
	}))
	defer srv.Close()

	a := testAdapter(srv.URL, "sk-test")
	req, err := a.BuildRequest(context.Background(), []byte(`{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := a.NormalizeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"chatcmpl-fixture-helloworld"`) {
		t.Errorf("fixture lost on return path: %s", body)
	}
}

// TestFixtureSchema asserts the recorded fixture still satisfies the
// minimum OpenAI ChatCompletions shape. Fails loud when DeepSeek's response
// shape drifts and we accidentally re-record a malformed fixture.
func TestFixtureSchema(t *testing.T) {
	raw := loadFixture(t, "deepseek-helloworld.json")
	var f struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Index   int `json:"index"`
			Message struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("fixture not valid JSON: %v", err)
	}
	if f.ID == "" {
		t.Error("missing id")
	}
	if f.Model == "" {
		t.Error("missing model")
	}
	if len(f.Choices) == 0 {
		t.Fatal("missing choices[]")
	}
	if f.Choices[0].Message.Role == "" {
		t.Error("missing choices[0].message.role")
	}
	if f.Choices[0].FinishReason == "" {
		t.Error("missing choices[0].finish_reason")
	}
	if f.Usage.PromptTokens == 0 && f.Usage.CompletionTokens == 0 {
		t.Error("usage is zero; fixture should record non-zero tokens")
	}
}

func TestConfigure(t *testing.T) {
	// Snapshot and restore the singleton so we don't pollute other tests.
	saved := *instance
	defer func() { *instance = saved }()

	Configure("", "sk-cfg", "")
	if instance.baseURL != DefaultBaseURL {
		t.Errorf("empty baseURL should default, got %q", instance.baseURL)
	}
	Configure("https://x/v1/", "sk-cfg", "deepseek-reasoner")
	if instance.baseURL != "https://x/v1" {
		t.Errorf("trailing slash not trimmed: %q", instance.baseURL)
	}
	if instance.DefaultModel() != "deepseek-reasoner" {
		t.Errorf("override lost: %q", instance.DefaultModel())
	}
}

// --- helpers ---

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	// testdata/fixtures lives at the repo root. Walk up from package dir.
	dirs := []string{
		filepath.Join("..", "..", "..", "testdata", "fixtures", name),
	}
	for _, p := range dirs {
		if data, err := os.ReadFile(p); err == nil {
			return data
		}
	}
	t.Fatalf("fixture %s not found", name)
	return nil
}
