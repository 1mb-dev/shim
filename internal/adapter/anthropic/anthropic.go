// Package anthropic implements the Adapter as a transparent passthrough to a
// native Anthropic Messages API (api.anthropic.com or a compatible endpoint).
// It performs NO dialect translation: requests are forwarded verbatim and
// responses returned unchanged (see translate.Identity). Its value is shim's
// observability — redacted logs, /v1/metrics, loud-fail — in front of a real
// Anthropic endpoint with zero translation risk.
//
// Construct via New and register from cmd/shim/main.go, mirroring the deepseek
// adapter; no init()-time registration.
package anthropic

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/1mb-dev/shim/internal/adapter"
	"github.com/1mb-dev/shim/internal/translate"
)

const (
	// Name is the registry key (config ADAPTER=anthropic).
	Name = "anthropic"
	// DefaultBaseURL is used when UPSTREAM_BASE_URL is empty.
	DefaultBaseURL = "https://api.anthropic.com"
	// DefaultAnthropicVersion is sent when the client omits anthropic-version.
	// Anthropic requires the header; shim supplies a default and logs the
	// inject (thesis-2: never silently modify inbound traffic).
	DefaultAnthropicVersion = "2023-06-01"
)

type impl struct {
	baseURL string
	apiKey  string
	log     *slog.Logger
}

// ConfigureOpts holds New parameters as a struct so signature changes don't
// ripple to call sites.
type ConfigureOpts struct {
	BaseURL string
	APIKey  string
	Logger  *slog.Logger // for the version-inject loud-fail line
}

// New constructs a configured passthrough adapter. Caller registers the
// returned adapter via adapter.Register. The config gate is Validate at
// server startup.
func New(opts ConfigureOpts) (adapter.Adapter, error) {
	baseURL := opts.BaseURL
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &impl{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  opts.APIKey,
		log:     log,
	}, nil
}

func (a *impl) Name() string { return Name }

// MapModel is identity: a native-Anthropic upstream accepts Anthropic model
// names as-is, so the request's model is forwarded unchanged.
func (a *impl) MapModel(model string) string { return model }

// Validate confirms the adapter has the configuration it needs. Called once at
// server startup; non-nil error blocks startup.
func (a *impl) Validate() error {
	if a.baseURL == "" {
		return fmt.Errorf("anthropic: not configured (call New first)")
	}
	if a.apiKey == "" {
		return fmt.Errorf("anthropic: UPSTREAM_API_KEY not set")
	}
	return nil
}

// BuildRequest binds the (already identity-translated) Anthropic body to
// {BaseURL}/v1/messages with x-api-key auth. It forwards the client's
// anthropic-version / anthropic-beta headers verbatim when present (read from
// the context via adapter.InboundHeaders); when anthropic-version is absent it
// injects DefaultAnthropicVersion and logs the inject.
func (a *impl) BuildRequest(ctx context.Context, body []byte) (*http.Request, error) {
	if a.apiKey == "" {
		return nil, fmt.Errorf("anthropic: UPSTREAM_API_KEY not set")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("anthropic: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", a.apiKey)

	in := adapter.InboundHeaders(ctx)
	if v := in.Get("anthropic-version"); v != "" {
		req.Header.Set("anthropic-version", v)
	} else {
		req.Header.Set("anthropic-version", DefaultAnthropicVersion)
		a.log.Info("anthropic-version injected",
			slog.String("default", DefaultAnthropicVersion),
		)
	}
	if b := in.Get("anthropic-beta"); b != "" {
		req.Header.Set("anthropic-beta", b)
	}
	return req, nil
}

// NormalizeResponse returns the buffered (non-streaming) upstream body unchanged
// on 2xx and errors on non-2xx (passthrough does no envelope unwrapping). The
// shared body lives in adapter.ReadNormalizedResponse, which also closes resp.Body
// (the server delegates the close to this method on the non-stream path).
func (a *impl) NormalizeResponse(resp *http.Response) ([]byte, error) {
	return adapter.ReadNormalizedResponse(Name, resp)
}

// Translator returns the identity translator: native Anthropic upstream, no
// dialect conversion (request forwarded verbatim, response returned unchanged).
func (a *impl) Translator() translate.Translator { return translate.Identity() }
