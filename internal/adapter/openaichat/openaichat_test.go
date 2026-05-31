package openaichat

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

// ds is the deepseek preset's model defaults — the single source of truth for
// the expected values below (so the table can't drift from the row).
var ds = presets["deepseek"].models

// testAdapter builds a deepseek-preset impl directly (bypassing New's base-URL
// defaulting) so the not-configured / missing-baseURL guards stay testable.
func testAdapter(baseURL, apiKey string) *impl {
	return &impl{
		name:         "deepseek",
		baseURL:      strings.TrimRight(baseURL, "/"),
		apiKey:       apiKey,
		authRequired: presets["deepseek"].authRequired,
		models:       presets["deepseek"].models,
	}
}

// testAdapterRoles is testAdapter with the per-role / catch-all overrides set.
func testAdapterRoles(baseURL, apiKey, modelOverride, opus, sonnet, haiku string) *impl {
	a := testAdapter(baseURL, apiKey)
	a.modelOverride = modelOverride
	a.opusModel = opus
	a.sonnetModel = sonnet
	a.haikuModel = haiku
	return a
}

func TestName(t *testing.T) {
	a := testAdapter("https://api.deepseek.com/v1", "k")
	if a.Name() != "deepseek" {
		t.Errorf("Name = %q", a.Name())
	}
}

func TestMapModel(t *testing.T) {
	// Table covers the full prefix matrix, fallback branches, and the
	// hyphen-anchor fence. Expected values come from the deepseek row (ds).
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
		{"opus default", "", "", "", "", "claude-opus-4-7", ds.Opus},
		{"opus default exact", "", "", "", "", "claude-opus", ds.Opus},
		{"opus default with [1m] suffix", "", "", "", "", "claude-opus-4-7[1m]", ds.Opus},
		{"sonnet default", "", "", "", "", "claude-sonnet-4-6", ds.Sonnet},
		{"sonnet default exact", "", "", "", "", "claude-sonnet", ds.Sonnet},
		{"haiku default", "", "", "", "", "claude-haiku-4-5", ds.Haiku},
		{"haiku default exact", "", "", "", "", "claude-haiku", ds.Haiku},

		// Per-role overrides
		{"opus override", "", "my-opus", "", "", "claude-opus", "my-opus"},
		{"sonnet override", "", "", "my-sonnet", "", "claude-sonnet", "my-sonnet"},
		{"haiku override", "", "", "", "my-haiku", "claude-haiku", "my-haiku"},

		// Override independence — only opus set should not affect sonnet/haiku
		{"only opus override leaves sonnet default", "", "my-opus", "", "", "claude-sonnet-4-6", ds.Sonnet},
		{"only opus override leaves haiku default", "", "my-opus", "", "", "claude-haiku-4-5", ds.Haiku},

		// R1 — UPSTREAM_MODEL must NOT override a role that has a preset default.
		{"UPSTREAM_MODEL does not leak into opus role", "deepseek-reasoner", "", "", "", "claude-opus", ds.Opus},
		{"UPSTREAM_MODEL does not leak into sonnet role", "deepseek-reasoner", "", "", "", "claude-sonnet-4-6", ds.Sonnet},

		// Empty input falls back to modelOverride or preset Default
		{"empty input no override", "", "", "", "", "", ds.Default},
		{"empty input with override", "deepseek-reasoner", "", "", "", "", "deepseek-reasoner"},

		// Non-claude-{opus,sonnet,haiku} inputs
		{"deepseek-v4-pro pass-through", "", "", "", "", "deepseek-v4-pro", "deepseek-v4-pro"},
		{"deepseek-v4-pro with UPSTREAM_MODEL override", "deepseek-reasoner", "", "", "", "deepseek-v4-pro", "deepseek-reasoner"},
		{"unknown name pass-through", "", "", "", "", "unknown-model-name", "unknown-model-name"},

		// Legacy claude-3-* — does NOT match prefix rule, treated as non-claude
		{"legacy claude-3-5-sonnet pass-through", "", "", "", "", "claude-3-5-sonnet-20240620", "claude-3-5-sonnet-20240620"},
		{"legacy claude-3-5-sonnet with UPSTREAM_MODEL", "deepseek-chat", "", "", "", "claude-3-5-sonnet-20240620", "deepseek-chat"},

		// Hyphen-anchor fence — prefix without a hyphen separator must NOT match.
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

// TestHappyPath wires BuildRequest + Do + NormalizeResponse end-to-end against
// a stub upstream serving the recorded deepseek fixture.
func TestHappyPath(t *testing.T) {
	fixture := loadFixture(t, "deepseek-helloworld.json")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

// TestFixtureSchema asserts the recorded fixture still satisfies the minimum
// OpenAI ChatCompletions shape.
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
	a, err := New("deepseek", Config{APIKey: "sk-cfg"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got := a.(*impl)
	if got.baseURL != presets["deepseek"].defaultBaseURL {
		t.Errorf("empty baseURL should default, got %q", got.baseURL)
	}

	a, err = New("deepseek", Config{
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

// TestNew_FreshInstance: each New returns a distinct instance (no shared state).
func TestNew_FreshInstance(t *testing.T) {
	a1, _ := New("deepseek", Config{APIKey: "k1"})
	a2, _ := New("deepseek", Config{APIKey: "k2"})
	if a1.(*impl) == a2.(*impl) {
		t.Error("New returned the same pointer twice — singleton leak")
	}
	if a1.(*impl).apiKey == a2.(*impl).apiKey {
		t.Errorf("instances share apiKey: %q == %q", a1.(*impl).apiKey, a2.(*impl).apiKey)
	}
}

func TestNew_UnknownPreset(t *testing.T) {
	_, err := New("bogus", Config{APIKey: "k"})
	if err == nil {
		t.Fatal("expected error for unknown preset")
	}
	// Message must name the offending preset and list valid ones (loud-fail).
	if !strings.Contains(err.Error(), "bogus") || !strings.Contains(err.Error(), "deepseek") {
		t.Errorf("error should name the bad preset and valid ones, got %v", err)
	}
}

// TestNoAuth fences the authRequired=false path (Fork 2-b, the Ollama enabler):
// Validate passes keyless, BuildRequest sends no Authorization header, but an
// optional key is still forwarded when present (e.g. Ollama behind a proxy).
func TestNoAuth(t *testing.T) {
	a := &impl{
		name:         "synthetic",
		baseURL:      "http://localhost:11434/v1",
		authRequired: false,
		models:       roleModels{Default: "llama3.3"},
	}
	if err := a.Validate(); err != nil {
		t.Fatalf("no-auth Validate should pass keyless: %v", err)
	}
	req, err := a.BuildRequest(context.Background(), []byte(`{}`))
	if err != nil {
		t.Fatalf("no-auth BuildRequest: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "" {
		t.Errorf("no-auth should send no Authorization header, got %q", got)
	}
	a.apiKey = "opt"
	req, _ = a.BuildRequest(context.Background(), []byte(`{}`))
	if got := req.Header.Get("Authorization"); got != "Bearer opt" {
		t.Errorf("optional key should be sent when present, got %q", got)
	}
}

// TestBuildRequest_ExtraHeaders fences the per-row extra-header application
// (Fork 5-b, OpenRouter attribution headers — mechanism present, default nil).
func TestBuildRequest_ExtraHeaders(t *testing.T) {
	a := &impl{
		name:         "synthetic",
		baseURL:      "https://x",
		apiKey:       "k",
		authRequired: true,
		models:       roleModels{Default: "m"},
		extraHeaders: map[string]string{"X-Title": "shim", "HTTP-Referer": "https://example"},
	}
	req, err := a.BuildRequest(context.Background(), []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("X-Title"); got != "shim" {
		t.Errorf("X-Title = %q", got)
	}
	if got := req.Header.Get("HTTP-Referer"); got != "https://example" {
		t.Errorf("HTTP-Referer = %q", got)
	}
}

// TestPresets_Invariants guards every row (incl. those P2 adds): MapModel's
// empty-input / empty-role-default fall-throughs depend on a non-empty Default,
// and BuildRequest needs a base URL.
func TestPresets_Invariants(t *testing.T) {
	for name, p := range presets {
		if p.models.Default == "" {
			t.Errorf("preset %q has empty models.Default", name)
		}
		if p.defaultBaseURL == "" {
			t.Errorf("preset %q has empty defaultBaseURL", name)
		}
	}
}

// TestPresetMapModel covers the per-provider default mappings added in P2,
// including Ollama's empty-role-default fall-through to the preset Default.
func TestPresetMapModel(t *testing.T) {
	cases := []struct {
		preset string
		input  string
		want   string
	}{
		{"openai", "claude-opus-4-7", "gpt-5.5"},
		{"openai", "claude-sonnet-4-6", "gpt-5.5"},
		{"openai", "claude-haiku-4-5", "gpt-5.4-mini"},
		{"openai", "", "gpt-5.5"},
		{"openrouter", "claude-opus", "anthropic/claude-opus-4.8"},
		{"openrouter", "claude-sonnet", "anthropic/claude-sonnet-4.6"},
		{"openrouter", "claude-haiku", "anthropic/claude-haiku-4.5"},
		{"ollama", "claude-opus-4-7", "llama3.3"}, // empty role default → preset Default
		{"ollama", "claude-sonnet", "llama3.3"},
		{"ollama", "", "llama3.3"},
	}
	for _, tc := range cases {
		t.Run(tc.preset+"/"+tc.input, func(t *testing.T) {
			a, err := New(tc.preset, Config{})
			if err != nil {
				t.Fatal(err)
			}
			if got := a.MapModel(tc.input); got != tc.want {
				t.Errorf("%s MapModel(%q) = %q, want %q", tc.preset, tc.input, got, tc.want)
			}
		})
	}
}

// TestPresetMapModel_OllamaOverride: with an empty role default, the
// UPSTREAM_MODEL catch-all wins for a claude-* role before the preset Default,
// but an explicit role override (UPSTREAM_OPUS_MODEL) still beats the catch-all.
func TestPresetMapModel_OllamaOverride(t *testing.T) {
	a, _ := New("ollama", Config{ModelOverride: "qwen2.5-coder"})
	if got := a.MapModel("claude-opus"); got != "qwen2.5-coder" {
		t.Errorf("ollama claude-opus with UPSTREAM_MODEL = %q, want qwen2.5-coder", got)
	}
	a, _ = New("ollama", Config{ModelOverride: "qwen2.5-coder", OpusModel: "deepseek-r1"})
	if got := a.MapModel("claude-opus"); got != "deepseek-r1" {
		t.Errorf("ollama claude-opus with role override = %q, want deepseek-r1", got)
	}
}

// TestPresetAuth: openai requires a key (authRequired); ollama does not.
func TestPresetAuth(t *testing.T) {
	oai, _ := New("openai", Config{}) // no key
	if err := oai.Validate(); err == nil {
		t.Error("openai Validate should fail without a key")
	}
	olm, _ := New("ollama", Config{}) // no key
	if err := olm.Validate(); err != nil {
		t.Errorf("ollama Validate should pass keyless, got %v", err)
	}
}

// --- helpers ---

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	// testdata/fixtures lives at the repo root. Walk up from package dir.
	p := filepath.Join("..", "..", "..", "testdata", "fixtures", name)
	if data, err := os.ReadFile(p); err == nil {
		return data
	}
	t.Fatalf("fixture %s not found", name)
	return nil
}
