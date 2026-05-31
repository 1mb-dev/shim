// Package adapter defines the contract every upstream provider satisfies, plus
// the small shared helpers around it (ReadNormalizedResponse, the
// InboundHeaders context helper).
//
// Two ways to add a provider, by transport dialect:
//   - An OpenAI-dialect provider is a DATA row in internal/adapter/openaichat's
//     preset registry (base URL + per-role model map + auth flag) — no new file.
//   - A genuinely new dialect (not OpenAI-chat, not native Anthropic) is a
//     self-contained sub-package implementing Adapter, including Translator()
//     for its dialect.
//
// cmd/shim/main.go's buildAdapter constructs the adapter — one branch per
// dialect, no init()-time registration, no blank-import side effects.
package adapter

import (
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/1mb-dev/shim/internal/translate"
)

// Adapter binds shim to one upstream provider across two axes: the transport
// dialect (which Translator it returns — OpenAI-ChatCompletions for the
// openaichat presets, identity for anthropic-passthrough) and provider quirks
// (model-name format, auth/header peculiarities, response envelopes). The
// Translator handles the dialect; everything provider-specific lives INSIDE
// the adapter. The translator stays pure.
//
// BuildRequest wraps the translator's already-built upstream body in an
// http.Request bound to the provider's endpoint; NormalizeResponse reads the
// buffered response for the translator's FromUpstream.
type Adapter interface {
	// Name returns the lookup key used by config (e.g. "deepseek").
	Name() string

	// MapModel converts an Anthropic-style model name (which may be e.g.
	// "claude-3-5-sonnet-20240620") into the upstream's expected name.
	// Adapters handle the empty-input case themselves — typically by
	// returning a "default" model.
	MapModel(anthropicModel string) string

	// Validate confirms the adapter has all configuration it needs to serve
	// requests. Called once at server startup; non-nil error blocks startup.
	// Replaces the prior substring-match preflight backchannel on the
	// request path.
	Validate() error

	// BuildRequest takes an already-translated OpenAI ChatCompletions body
	// and returns an http.Request bound to {BaseURL}/chat/completions with
	// auth headers set. Adapter must call req.WithContext(ctx).
	BuildRequest(ctx context.Context, body []byte) (*http.Request, error)

	// NormalizeResponse reads a buffered (non-streaming) upstream response and
	// returns a canonical body for the Translator's FromUpstream, erroring on
	// non-2xx (the server routes that to writeUpstreamError). Adapter is
	// responsible for unwrapping any provider-specific envelope. The streaming
	// path does NOT call this — it gates on HTTP status directly, since a
	// native-Anthropic passthrough cannot normalize a live SSE stream to bytes.
	//
	// The implementation MUST close resp.Body: the non-stream handler delegates
	// the close here and does not close it itself. Failing to close leaks a
	// connection per non-stream request.
	NormalizeResponse(resp *http.Response) ([]byte, error)

	// Translator returns the wire-format translator for this adapter's
	// transport dialect. OpenAI-dialect adapters return
	// translate.AnthropicOpenAI(); a native-Anthropic passthrough returns an
	// identity translator. The server handler calls it instead of hard-wiring
	// a dialect, keeping dialect knowledge inside the adapter.
	Translator() translate.Translator
}

// inboundHeadersKey is the context key under which the server stashes the
// client's request headers (via WithInboundHeaders) so adapters that proxy
// verbatim — e.g. anthropic-passthrough forwarding anthropic-version /
// anthropic-beta — can read selected ones in BuildRequest without a signature
// change. Adapters that don't need them ignore it. The server selects which
// headers an adapter may read; it never forwards inbound Authorization
// (shim authenticates upstream itself).
type inboundHeadersKey struct{}

// ReadNormalizedResponse is the shared body of NormalizeResponse for adapters
// whose upstream needs no envelope unwrapping (deepseek, anthropic-passthrough):
// it returns the body unchanged on 2xx and, on non-2xx, returns the body
// alongside an error carrying the status (the server routes that to
// writeUpstreamError). It ALWAYS closes resp.Body — the NormalizeResponse
// contract delegates the close here. name prefixes errors so they stay
// attributable to the calling adapter.
func ReadNormalizedResponse(name string, resp *http.Response) ([]byte, error) {
	if resp == nil {
		return nil, fmt.Errorf("%s: nil response", name)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%s: read body: %w", name, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return body, fmt.Errorf("%s: upstream status %d", name, resp.StatusCode)
	}
	return body, nil
}

// WithInboundHeaders returns ctx carrying the client's request headers.
func WithInboundHeaders(ctx context.Context, h http.Header) context.Context {
	return context.WithValue(ctx, inboundHeadersKey{}, h)
}

// InboundHeaders returns the client request headers attached by
// WithInboundHeaders, or nil if none were set.
func InboundHeaders(ctx context.Context) http.Header {
	h, _ := ctx.Value(inboundHeadersKey{}).(http.Header)
	return h
}
