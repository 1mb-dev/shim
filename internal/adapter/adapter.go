// Package adapter defines the contract every upstream provider satisfies and
// a registry that lets the server look adapters up by name.
//
// Adding a new provider is a single self-contained sub-package:
//
//	package myprovider
//	import "github.com/1mb-dev/shim/internal/adapter"
//	func init() { adapter.Register(&Adapter{}) }
//
// The server enables a provider via a blank import in cmd/shim/main.go.
package adapter

import (
	"context"
	"fmt"
	"net/http"
	"sync"
)

// Adapter normalises one OpenAI-compatible upstream. The translator emits a
// canonical OpenAI ChatCompletions request body; the adapter wraps it in an
// http.Request bound to its upstream, then normalises the response back to
// canonical OpenAI shape so the translator can convert to Anthropic Messages.
//
// Quirks (model-name format, header peculiarities, response-shape variances)
// live INSIDE the adapter. The translator stays pure.
type Adapter interface {
	// Name returns the lookup key used by config (e.g. "deepseek").
	Name() string

	// DefaultModel is used when a request omits a mappable model name.
	DefaultModel() string

	// MapModel converts an Anthropic-style model name (which may be e.g.
	// "claude-3-5-sonnet-20240620") into the upstream's expected name.
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

	// NormalizeResponse reads the upstream response and returns a canonical
	// OpenAI-shape JSON body. Used by the translator. Adapter is responsible
	// for unwrapping any provider-specific envelope.
	NormalizeResponse(resp *http.Response) ([]byte, error)
}

var (
	mu       sync.RWMutex
	registry = map[string]Adapter{}
)

// Register adds a in the global registry. Intended to be called from an
// adapter sub-package's init(). Panics on duplicate registration so
// misconfiguration fails at startup, not at first request.
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
