// Package deepseek implements the Stage 0 Adapter against DeepSeek's
// OpenAI-compatible chat-completions API. DeepSeek hugs the OpenAI contract
// tightly, so NormalizeResponse is a pass-through.
//
// Usage: blank-import this package from cmd/shim/main.go to trigger
// registration, then call deepseek.Configure(cfg) before starting the
// server.
package deepseek

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/1mb-dev/shim/internal/adapter"
)

const (
	// Name is the registry key for this adapter.
	Name = "deepseek"
	// DefaultBaseURL is used when config leaves UpstreamBaseURL empty.
	DefaultBaseURL = "https://api.deepseek.com/v1"
	// DefaultModel is the legacy fallback used when the caller sends an
	// empty model name and no UPSTREAM_MODEL is configured.
	DefaultModel = "deepseek-chat"
	// DefaultOpusModel maps claude-opus* requests per DeepSeek's official
	// Claude Code integration guide
	// (https://api-docs.deepseek.com/quick_start/agent_integrations/claude_code).
	DefaultOpusModel = "deepseek-v4-pro[1m]"
	// DefaultSonnetModel maps claude-sonnet* requests per the same guide.
	DefaultSonnetModel = "deepseek-v4-flash"
	// DefaultHaikuModel maps claude-haiku* requests per the same guide.
	DefaultHaikuModel = "deepseek-v4-flash"
)

// instance is the singleton registered at init(). State is filled in by
// Configure before the server starts; subsequent reads from the request
// path are race-free because no further mutations occur.
var instance = &impl{client: newClient()}

type impl struct {
	baseURL       string
	apiKey        string
	modelOverride string // catch-all UPSTREAM_MODEL; empty = use per-role defaults / pass-through
	opusModel     string // UPSTREAM_OPUS_MODEL; empty = DefaultOpusModel
	sonnetModel   string // UPSTREAM_SONNET_MODEL; empty = DefaultSonnetModel
	haikuModel    string // UPSTREAM_HAIKU_MODEL; empty = DefaultHaikuModel
	client        *http.Client
}

// ConfigureOpts holds Configure parameters as a struct so signature changes
// don't ripple to every call site.
type ConfigureOpts struct {
	BaseURL       string
	APIKey        string
	ModelOverride string
	OpusModel     string
	SonnetModel   string
	HaikuModel    string
}

// Configure injects runtime configuration into the registered adapter. Call
// exactly once before starting the server.
func Configure(opts ConfigureOpts) {
	baseURL := opts.BaseURL
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	instance.baseURL = strings.TrimRight(baseURL, "/")
	instance.apiKey = opts.APIKey
	instance.modelOverride = opts.ModelOverride
	instance.opusModel = opts.OpusModel
	instance.sonnetModel = opts.SonnetModel
	instance.haikuModel = opts.HaikuModel
}

func (a *impl) Name() string { return Name }

func (a *impl) DefaultModel() string {
	if a.modelOverride != "" {
		return a.modelOverride
	}
	return DefaultModel
}

// MapModel routes an Anthropic-style model name to the DeepSeek upstream
// model. Mapping rule mirrors DeepSeek's own server-side rule on its native
// Anthropic endpoint (per the official Claude Code integration guide):
//
//   - claude-opus*   → DefaultOpusModel  (or UPSTREAM_OPUS_MODEL)
//   - claude-sonnet* → DefaultSonnetModel  (or UPSTREAM_SONNET_MODEL)
//   - claude-haiku*  → DefaultHaikuModel  (or UPSTREAM_HAIKU_MODEL)
//   - empty string   → UPSTREAM_MODEL if set, else DefaultModel
//   - anything else  → UPSTREAM_MODEL if set, else pass through unchanged
//
// Note: Legacy names like `claude-3-5-sonnet-20240620` do NOT match the
// prefix rule and fall to the pass-through branch — same as DeepSeek's own
// native endpoint would do. Users on legacy names should set UPSTREAM_MODEL
// or update to current model identifiers.
//
// Whenever MapModel returns a value different from the input, the server
// logs a "model rewritten" line and increments the rewrites.model counter
// in /v1/metrics (thesis-2: never silently forward modified traffic).
func (a *impl) MapModel(model string) string {
	switch {
	case strings.HasPrefix(model, "claude-opus"):
		if a.opusModel != "" {
			return a.opusModel
		}
		return DefaultOpusModel
	case strings.HasPrefix(model, "claude-sonnet"):
		if a.sonnetModel != "" {
			return a.sonnetModel
		}
		return DefaultSonnetModel
	case strings.HasPrefix(model, "claude-haiku"):
		if a.haikuModel != "" {
			return a.haikuModel
		}
		return DefaultHaikuModel
	case model == "":
		if a.modelOverride != "" {
			return a.modelOverride
		}
		return DefaultModel
	default:
		if a.modelOverride != "" {
			return a.modelOverride
		}
		return model
	}
}

// Validate confirms the adapter has the configuration it needs to serve
// requests. Called once at server startup; non-nil error blocks startup.
func (a *impl) Validate() error {
	if a.baseURL == "" {
		return fmt.Errorf("deepseek: not configured (call Configure first)")
	}
	if a.apiKey == "" {
		return fmt.Errorf("deepseek: UPSTREAM_API_KEY not set")
	}
	return nil
}

func (a *impl) BuildRequest(ctx context.Context, body []byte) (*http.Request, error) {
	if a.baseURL == "" {
		return nil, fmt.Errorf("deepseek: not configured (call Configure first)")
	}
	if a.apiKey == "" {
		return nil, fmt.Errorf("deepseek: UPSTREAM_API_KEY not set")
	}
	url := a.baseURL + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("deepseek: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.apiKey)
	return req, nil
}

// NormalizeResponse reads the upstream body and returns it unchanged when
// the status is 2xx. Non-2xx responses propagate the body so callers can
// build a useful error; the second return value carries the status hint.
func (a *impl) NormalizeResponse(resp *http.Response) ([]byte, error) {
	if resp == nil {
		return nil, fmt.Errorf("deepseek: nil response")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("deepseek: read body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return body, fmt.Errorf("deepseek: upstream status %d", resp.StatusCode)
	}
	return body, nil
}

// Client exposes the configured HTTP client so the server can call
// Adapter.BuildRequest then run the request itself. Returned for the
// configured singleton.
func Client() *http.Client { return instance.client }

// newClient builds the HTTP client with split timeouts so a slow TCP
// handshake or slow header response can't pin a goroutine for the whole
// 60-second client timeout.
func newClient() *http.Client {
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return &http.Client{
		Timeout:   60 * time.Second,
		Transport: transport,
	}
}

func init() {
	adapter.Register(instance)
}
