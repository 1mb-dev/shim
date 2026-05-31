// Package openaichat implements the Adapter against any OpenAI-ChatCompletions
// upstream. One core serves every provider that speaks the dialect; providers
// differ only by DATA, expressed as rows in the presets registry (base URL,
// auth requirement, per-role model defaults, optional extra headers) — not by
// per-provider Go types. Adding a provider is a row, not a file.
//
// Usage: cmd/shim/main.go resolves the ADAPTER name to a preset:
//
//	a, err := openaichat.New("deepseek", openaichat.Config{APIKey: key})
//	srv, err := server.New(cfg, log, a)
//
// The Translator is the canonical anthropic↔openai one for every preset; the
// dialect lives in translate, the provider quirks live in the row.
package openaichat

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/1mb-dev/shim/internal/adapter"
	"github.com/1mb-dev/shim/internal/translate"
)

// roleModels holds a preset's per-role default upstream model names. An empty
// role field means "no role-specific default" — MapModel then falls through to
// the catch-all UPSTREAM_MODEL override or Default (e.g. Ollama, which has a
// single local model for all roles). Default must be non-empty for every preset.
type roleModels struct {
	Opus    string
	Sonnet  string
	Haiku   string
	Default string
}

// preset is the static, per-provider data row. Everything that distinguishes
// one OpenAI-dialect upstream from another lives here.
type preset struct {
	defaultBaseURL string            // used when UPSTREAM_BASE_URL is empty
	authRequired   bool              // false ⇒ no API key needed (e.g. Ollama)
	models         roleModels        // per-role default model names
	extraHeaders   map[string]string // optional provider headers; usually nil
}

// presets is the registry keyed by ADAPTER name. New providers are added here.
var presets = map[string]preset{
	// DeepSeek's OpenAI-format endpoint. Mapping mirrors DeepSeek's own
	// server-side rule on its native Anthropic endpoint (per the official
	// Claude Code integration guide): the "[1m]" 1M-context variant only works
	// on DeepSeek's /anthropic endpoint, not this OpenAI-format one.
	"deepseek": {
		defaultBaseURL: "https://api.deepseek.com/v1",
		authRequired:   true,
		models: roleModels{
			Opus:    "deepseek-v4-pro",
			Sonnet:  "deepseek-v4-flash",
			Haiku:   "deepseek-v4-flash",
			Default: "deepseek-chat",
		},
	},

	// OpenAI proper. Chat-completions only — reasoning/"o"-series models that
	// require the Responses API are out of scope (a different transport dialect).
	// reasoning_content does not round-trip (OpenAI hides reasoning), so thinking
	// blocks are a no-op here — not a bug. Model IDs verified 2026-05; they drift
	// with releases — override per role via UPSTREAM_*_MODEL (e.g. set sonnet to
	// gpt-5.4-mini to trade capability for cost).
	"openai": {
		defaultBaseURL: "https://api.openai.com/v1",
		authRequired:   true,
		models: roleModels{
			Opus:    "gpt-5.5",
			Sonnet:  "gpt-5.5",
			Haiku:   "gpt-5.4-mini",
			Default: "gpt-5.5",
		},
	},

	// OpenRouter — one key, many providers. Defaults route claude-* back to
	// Anthropic via OpenRouter (preserves the caller's model intent). NOTE:
	// double-translation (Anthropic→OpenAI→OpenRouter→provider) — shim measures
	// only its own hop. No attribution headers injected by default (extraHeaders
	// nil). Slugs verified 2026-05; they drift — override via UPSTREAM_*_MODEL.
	"openrouter": {
		defaultBaseURL: "https://openrouter.ai/api/v1",
		authRequired:   true,
		models: roleModels{
			Opus:    "anthropic/claude-opus-4.8",
			Sonnet:  "anthropic/claude-sonnet-4.6",
			Haiku:   "anthropic/claude-haiku-4.5",
			Default: "anthropic/claude-sonnet-4.6",
		},
	},

	// Ollama local (OpenAI-compatible endpoint). No auth required — Validate
	// passes keyless; an optional key is still forwarded if set (proxy setups).
	// One local model serves all roles via the empty-role-default fall-through.
	// Default llama3.3; override via UPSTREAM_MODEL (e.g. qwen2.5-coder for coding).
	"ollama": {
		defaultBaseURL: "http://localhost:11434/v1",
		authRequired:   false,
		models: roleModels{
			Default: "llama3.3",
		},
	},
}

// Config carries the runtime/env configuration that overrides a preset's
// static defaults. Field semantics match the UPSTREAM_* env vars.
type Config struct {
	BaseURL       string // UPSTREAM_BASE_URL; empty ⇒ preset.defaultBaseURL
	APIKey        string // UPSTREAM_API_KEY
	ModelOverride string // UPSTREAM_MODEL (catch-all for non-claude-* inputs)
	OpusModel     string // UPSTREAM_OPUS_MODEL
	SonnetModel   string // UPSTREAM_SONNET_MODEL
	HaikuModel    string // UPSTREAM_HAIKU_MODEL
}

type impl struct {
	name         string
	baseURL      string
	apiKey       string
	authRequired bool
	models       roleModels
	extraHeaders map[string]string
	// per-deployment overrides (UPSTREAM_* env), empty ⇒ use preset defaults
	modelOverride string
	opusModel     string
	sonnetModel   string
	haikuModel    string
}

// New resolves name to a preset row and returns a configured adapter, merging
// cfg over the row's defaults. Unknown name fails loudly (listing valid
// presets) — the config gate proper is Adapter.Validate at server startup.
// Each call returns a fresh instance.
func New(name string, cfg Config) (adapter.Adapter, error) {
	p, ok := presets[name]
	if !ok {
		return nil, fmt.Errorf("openaichat: unknown preset %q (valid: %s)", name, strings.Join(presetNames(), ", "))
	}
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = p.defaultBaseURL
	}
	return &impl{
		name:          name,
		baseURL:       strings.TrimRight(baseURL, "/"),
		apiKey:        cfg.APIKey,
		authRequired:  p.authRequired,
		models:        p.models,
		extraHeaders:  p.extraHeaders,
		modelOverride: cfg.ModelOverride,
		opusModel:     cfg.OpusModel,
		sonnetModel:   cfg.SonnetModel,
		haikuModel:    cfg.HaikuModel,
	}, nil
}

// presetNames returns the sorted registry keys, for loud-fail error messages.
func presetNames() []string {
	out := make([]string, 0, len(presets))
	for k := range presets {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (a *impl) Name() string { return a.name }

// MapModel routes an Anthropic-style model name to the upstream model using the
// preset's per-role defaults. Precedence per role: explicit env override
// (UPSTREAM_*_MODEL) > preset role default > (only if the preset has no role
// default) UPSTREAM_MODEL catch-all > preset Default. UPSTREAM_MODEL therefore
// does NOT override a role that has a preset default — preserving the original
// DeepSeek behavior.
//
//   - "claude-opus" or "claude-opus-*"     → opus mapping
//   - "claude-sonnet" or "claude-sonnet-*" → sonnet mapping
//   - "claude-haiku" or "claude-haiku-*"   → haiku mapping
//   - "" (empty)   → UPSTREAM_MODEL if set, else preset Default
//   - anything else → UPSTREAM_MODEL if set, else pass through unchanged
//
// The hyphen anchor (exact bare pointer OR "<role>-" prefix) is deliberate:
// bare "claude-opus" and versioned "claude-opus-4-7" both resolve to opus, but
// "claude-opusxxx" must NOT match (it would silently re-route an unrelated
// model). Legacy names like "claude-3-5-sonnet-*" don't match and fall through.
//
// Whenever MapModel returns a value different from the input, the server logs a
// "model rewritten" line and increments rewrites.model (thesis-2).
func (a *impl) MapModel(model string) string {
	switch {
	case model == "claude-opus" || strings.HasPrefix(model, "claude-opus-"):
		return a.roleModel(a.opusModel, a.models.Opus)
	case model == "claude-sonnet" || strings.HasPrefix(model, "claude-sonnet-"):
		return a.roleModel(a.sonnetModel, a.models.Sonnet)
	case model == "claude-haiku" || strings.HasPrefix(model, "claude-haiku-"):
		return a.roleModel(a.haikuModel, a.models.Haiku)
	case model == "":
		if a.modelOverride != "" {
			return a.modelOverride
		}
		return a.models.Default
	default:
		if a.modelOverride != "" {
			return a.modelOverride
		}
		return model
	}
}

// roleModel resolves one role: explicit env override wins; else the preset's
// role default; else (preset has none) the UPSTREAM_MODEL catch-all or Default.
func (a *impl) roleModel(envOverride, presetRoleDefault string) string {
	if envOverride != "" {
		return envOverride
	}
	if presetRoleDefault != "" {
		return presetRoleDefault
	}
	if a.modelOverride != "" {
		return a.modelOverride
	}
	return a.models.Default
}

// Validate confirms the adapter has the configuration it needs. Called once at
// server startup; non-nil error blocks startup. This is the single auth gate —
// config.Load no longer globally requires UPSTREAM_API_KEY, so presets with
// authRequired=false (Ollama) start keyless.
func (a *impl) Validate() error {
	if a.baseURL == "" {
		return fmt.Errorf("%s: not configured (call New first)", a.name)
	}
	if a.authRequired && a.apiKey == "" {
		return fmt.Errorf("%s: UPSTREAM_API_KEY not set", a.name)
	}
	return nil
}

// BuildRequest binds the already-translated OpenAI body to
// {baseURL}/chat/completions. The Authorization: Bearer header is set iff an
// API key is present (a no-auth preset like Ollama still accepts an optional
// key behind a proxy); extraHeaders (usually nil) are applied last.
func (a *impl) BuildRequest(ctx context.Context, body []byte) (*http.Request, error) {
	// Config invariants hold here: New always sets baseURL, and Validate gated
	// the key at startup (the single auth gate). No re-check — see Validate.
	url := a.baseURL + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%s: build request: %w", a.name, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if a.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+a.apiKey)
	}
	for k, v := range a.extraHeaders {
		req.Header.Set(k, v)
	}
	return req, nil
}

// NormalizeResponse returns the upstream body unchanged on 2xx and errors on
// non-2xx (OpenAI-dialect upstreams need no envelope unwrapping). The shared
// body lives in adapter.ReadNormalizedResponse, which also closes resp.Body.
func (a *impl) NormalizeResponse(resp *http.Response) ([]byte, error) {
	return adapter.ReadNormalizedResponse(a.name, resp)
}

// Translator returns the canonical OpenAI-ChatCompletions dialect translator,
// shared by every preset (the dialect is one axis; provider quirks are the row).
func (a *impl) Translator() translate.Translator { return translate.AnthropicOpenAI() }
