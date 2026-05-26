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
	// DefaultModel is used when MapModel has no override.
	DefaultModel = "deepseek-chat"
)

// instance is the singleton registered at init(). State is filled in by
// Configure before the server starts; subsequent reads from the request
// path are race-free because no further mutations occur.
var instance = &impl{client: newClient()}

type impl struct {
	baseURL       string
	apiKey        string
	modelOverride string // "" means use DefaultModel
	client        *http.Client
}

// Configure injects runtime configuration into the registered adapter. Call
// exactly once before starting the server.
func Configure(baseURL, apiKey, modelOverride string) {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	instance.baseURL = strings.TrimRight(baseURL, "/")
	instance.apiKey = apiKey
	instance.modelOverride = modelOverride
}

func (a *impl) Name() string { return Name }

func (a *impl) DefaultModel() string {
	if a.modelOverride != "" {
		return a.modelOverride
	}
	return DefaultModel
}

// MapModel collapses any Anthropic-style model name to the DeepSeek default
// (or the configured override). The proxy is not a model router — clients
// pick the adapter; the adapter picks the model.
func (a *impl) MapModel(_ string) string {
	return a.DefaultModel()
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
