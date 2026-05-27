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

// testAdapter returns a freshly-built impl for isolated unit tests.
func testAdapter(baseURL, apiKey string) *impl {
	return &impl{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
	}
}

// testAdapterRoles is testAdapter with the per-role model overrides also
// settable. Empty values fall back to DefaultOpusModel / DefaultSonnetModel /
// DefaultHaikuModel.
func testAdapterRoles(baseURL, apiKey, modelOverride, opus, sonnet, haiku string) *impl {
	return &impl{
		baseURL:       strings.TrimRight(baseURL, "/"),
		apiKey:        apiKey,
		modelOverride: modelOverride,
		opusModel:     opus,
		sonnetModel:   sonnet,
		haikuModel:    haiku,
	}
}

func TestName(t *testing.T) {
	a := testAdapter("https://api.deepseek.com/v1", "k")
	if a.Name() != "deepseek" {
		t.Errorf("Name = %q", a.Name())
	}
}

func TestMapModel(t *testing.T) {
	// Table covers the full prefix matrix from the DeepSeek official guide:
	//   claude-opus*   → opus model (default or override)
	//   claude-sonnet* → sonnet model
	//   claude-haiku*  → haiku model
	// Plus the fallback branches: empty input, non-claude name, legacy
	// claude-3-* (no prefix match, falls through to pass-through/override).
	tests := []struct {
		name          string
		modelOverride string // UPSTREAM_MODEL
		opus          string
		sonnet        string
		haiku         string
		input         string
		want          string
	}{
		// Per-role defaults
		{"opus default", "", "", "", "", "claude-opus-4-7", DefaultOpusModel},
		{"opus default exact", "", "", "", "", "claude-opus", DefaultOpusModel},
		{"opus default with [1m] suffix", "", "", "", "", "claude-opus-4-7[1m]", DefaultOpusModel},
		{"sonnet default", "", "", "", "", "claude-sonnet-4-6", DefaultSonnetModel},
		{"sonnet default exact", "", "", "", "", "claude-sonnet", DefaultSonnetModel},
		{"haiku default", "", "", "", "", "claude-haiku-4-5", DefaultHaikuModel},
		{"haiku default exact", "", "", "", "", "claude-haiku", DefaultHaikuModel},

		// Per-role overrides
		{"opus override", "", "my-opus", "", "", "claude-opus", "my-opus"},
		{"sonnet override", "", "", "my-sonnet", "", "claude-sonnet", "my-sonnet"},
		{"haiku override", "", "", "", "my-haiku", "claude-haiku", "my-haiku"},

		// Override independence — only opus set should not affect sonnet/haiku
		{"only opus override leaves sonnet default", "", "my-opus", "", "", "claude-sonnet-4-6", DefaultSonnetModel},
		{"only opus override leaves haiku default", "", "my-opus", "", "", "claude-haiku-4-5", DefaultHaikuModel},

		// Empty input falls back to modelOverride or DefaultModel
		{"empty input no override", "", "", "", "", "", DefaultModel},
		{"empty input with override", "deepseek-reasoner", "", "", "", "", "deepseek-reasoner"},

		// Non-claude-{opus,sonnet,haiku} inputs
		{"deepseek-v4-pro pass-through", "", "", "", "", "deepseek-v4-pro", "deepseek-v4-pro"},
		{"deepseek-v4-pro with UPSTREAM_MODEL override", "deepseek-reasoner", "", "", "", "deepseek-v4-pro", "deepseek-reasoner"},
		{"unknown name pass-through", "", "", "", "", "unknown-model-name", "unknown-model-name"},

		// Legacy claude-3-* — does NOT match prefix rule, treated as non-claude
		{"legacy claude-3-5-sonnet pass-through", "", "", "", "", "claude-3-5-sonnet-20240620", "claude-3-5-sonnet-20240620"},
		{"legacy claude-3-5-sonnet with UPSTREAM_MODEL", "deepseek-chat", "", "", "", "claude-3-5-sonnet-20240620", "deepseek-chat"},

		// Hyphen-anchor fence — strings that share the prefix but have no
		// hyphen separator (or no hyphen + extra chars) must NOT match.
		// Pre-fix, `strings.HasPrefix(model, "claude-opus")` matched
		// `claude-opusxxx` and silently re-routed it to opus default.
		{"claude-opusxxx pass-through (no hyphen anchor)", "", "", "", "", "claude-opusxxx", "claude-opusxxx"},
		{"claude-sonnetx pass-through (no hyphen anchor)", "", "", "", "", "claude-sonnetx", "claude-sonnetx"},
		{"claude-haiku2 pass-through (no hyphen anchor)", "", "", "", "", "claude-haiku2", "claude-haiku2"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := testAdapterRoles("x", "y", tc.modelOverride, tc.opus, tc.sonnet, tc.haiku)
			if got := a.MapModel(tc.input); got != tc.want {
				t.Errorf("MapModel(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
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
	a := &impl{} // empty baseURL
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
	resp, err := http.DefaultClient.Do(req)
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

func TestNew(t *testing.T) {
	a, err := New(ConfigureOpts{APIKey: "sk-cfg"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got := a.(*impl)
	if got.baseURL != DefaultBaseURL {
		t.Errorf("empty baseURL should default, got %q", got.baseURL)
	}

	a, err = New(ConfigureOpts{
		BaseURL:       "https://x/v1/",
		APIKey:        "sk-cfg",
		ModelOverride: "deepseek-reasoner",
		OpusModel:     "custom-opus",
		SonnetModel:   "custom-sonnet",
		HaikuModel:    "custom-haiku",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got = a.(*impl)
	if got.baseURL != "https://x/v1" {
		t.Errorf("trailing slash not trimmed: %q", got.baseURL)
	}
	if mapped := got.MapModel(""); mapped != "deepseek-reasoner" {
		t.Errorf("empty-input fallback should use modelOverride, got %q", mapped)
	}
	if got.opusModel != "custom-opus" {
		t.Errorf("opusModel = %q, want custom-opus", got.opusModel)
	}
	if got.sonnetModel != "custom-sonnet" {
		t.Errorf("sonnetModel = %q, want custom-sonnet", got.sonnetModel)
	}
	if got.haikuModel != "custom-haiku" {
		t.Errorf("haikuModel = %q, want custom-haiku", got.haikuModel)
	}
}

// TestNew_FreshInstance: each call to New returns a distinct instance so
// tests (and any future multi-tenant use) don't share state.
func TestNew_FreshInstance(t *testing.T) {
	a1, _ := New(ConfigureOpts{APIKey: "k1"})
	a2, _ := New(ConfigureOpts{APIKey: "k2"})
	if a1.(*impl) == a2.(*impl) {
		t.Error("New returned the same pointer twice — singleton leak")
	}
	if a1.(*impl).apiKey == a2.(*impl).apiKey {
		t.Errorf("instances share apiKey: %q == %q", a1.(*impl).apiKey, a2.(*impl).apiKey)
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
