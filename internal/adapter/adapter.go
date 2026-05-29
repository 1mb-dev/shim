// Package adapter defines the contract every upstream provider satisfies and
// a registry that lets the server look adapters up by name.
//
// Adding a new provider is a single self-contained sub-package:
//
//	package myprovider
//	import "github.com/1mb-dev/shim/internal/adapter"
//
//	func New(opts Opts) (adapter.Adapter, error) { ... }
//
// cmd/shim/main.go constructs the adapter via its New, then calls
// adapter.Register with the returned instance — no init()-time
// registration, no blank-import side effects.
package adapter

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	"github.com/1mb-dev/shim/internal/translate"
)

// Adapter binds shim to one upstream provider across two axes: the transport
// dialect (which Translator it returns — OpenAI-ChatCompletions for DeepSeek,
// identity for anthropic-passthrough) and provider quirks (model-name format,
// auth/header peculiarities, response envelopes). The Translator handles the
// dialect; everything provider-specific lives INSIDE the adapter. The
// translator stays pure.
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
	// returning a "default" model — so callers never need a separate
	// DefaultModel() probe.
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

var (
	mu       sync.RWMutex
	registry = map[string]Adapter{}
)

// Register adds a in the global registry. Called from cmd/shim/main.go
// after constructing the adapter via its New. Panics on duplicate
// registration so misconfiguration fails at startup, not at first request.
func Register(a Adapter) {
	if a == nil {
		panic("adapter: Register called with nil adapter")
	}
	name := a.Name()
	if name == "" {
		panic("adapter: Register called with empty Name()")
	}
	mu.Lock()
	defer mu.Unlock()
	if _, exists := registry[name]; exists {
		panic(fmt.Sprintf("adapter: %q already registered", name))
	}
	registry[name] = a
}

// Get returns the adapter registered under name. The bool is false when no
// such adapter exists — callers must handle this loudly (T2).
func Get(name string) (Adapter, bool) {
	mu.RLock()
	defer mu.RUnlock()
	a, ok := registry[name]
	return a, ok
}

// Names returns the sorted list of registered adapter names. Useful for
// error messages when Get fails.
func Names() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	// Avoid importing "sort" here; caller can sort if order matters.
	return out
}
