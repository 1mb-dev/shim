package main

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/1mb-dev/shim/internal/config"
)

// TestDispatchVersion: `shim version` (and -v/--version) print "shim <version>"
// and exit 0 without starting the server.
func TestDispatchVersion(t *testing.T) {
	for _, arg := range []string{"version", "-v", "--version"} {
		var out bytes.Buffer
		code, err := dispatch([]string{arg}, &out)
		if err != nil || code != 0 {
			t.Fatalf("dispatch %q = (%d, %v), want (0, nil)", arg, code, err)
		}
		if got := strings.TrimSpace(out.String()); got != "shim dev" {
			t.Errorf("%q output = %q, want %q", arg, got, "shim dev")
		}
	}
}

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

// TestBuildAdapter_OpenAIDialectPreset covers the default→openaichat branch of
// buildAdapter (the anthropic test covers the other branch): a preset name
// resolves to an openaichat adapter at the preset's own base URL.
func TestBuildAdapter_OpenAIDialectPreset(t *testing.T) {
	cfg := &config.Config{Adapter: "deepseek", UpstreamAPIKey: "k"}
	a, err := buildAdapter(cfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if a.Name() != "deepseek" {
		t.Errorf("name = %q, want deepseek", a.Name())
	}
	req, err := a.BuildRequest(context.Background(), []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if req.URL.Host != "api.deepseek.com" {
		t.Errorf("host = %q, want api.deepseek.com", req.URL.Host)
	}
}

// TestBuildAdapter_Unknown: an unknown ADAPTER fails loudly (the loud-fail the
// removed registry once owned now lives in openaichat.New, surfaced here).
func TestBuildAdapter_Unknown(t *testing.T) {
	cfg := &config.Config{Adapter: "ghost", UpstreamAPIKey: "k"}
	if _, err := buildAdapter(cfg, slog.Default()); err == nil {
		t.Fatal("expected error for unknown ADAPTER, got nil")
	}
}
