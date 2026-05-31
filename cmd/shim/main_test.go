package main

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/1mb-dev/shim/internal/config"
)

// TestBuildAdapter_AnthropicNoDeepseekMisroute fences the v0.5 fix: with
// ADAPTER=anthropic and UPSTREAM_BASE_URL unset, buildAdapter must resolve the
// adapter to api.anthropic.com — never inherit a deepseek host. The latent
// misroute existed while config.Load globally defaulted UPSTREAM_BASE_URL to
// the deepseek endpoint; this asserts the wiring (config empty → anthropic's
// own default) composes correctly, the gap unit tests on either half missed.
func TestBuildAdapter_AnthropicNoDeepseekMisroute(t *testing.T) {
	cfg := &config.Config{Adapter: "anthropic", UpstreamAPIKey: "k"} // UpstreamBaseURL: ""
	a, err := buildAdapter(cfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	req, err := a.BuildRequest(context.Background(), []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if req.URL.Host != "api.anthropic.com" {
		t.Errorf("anthropic empty-base host = %q, want api.anthropic.com", req.URL.Host)
	}
	if strings.Contains(req.URL.Host, "deepseek") {
		t.Fatalf("misroute: anthropic traffic routed to deepseek host %q", req.URL.Host)
	}
}
