// Package server wires the HTTP routes for shim's three Stage 0 endpoints
// and owns the per-request translation flow.
package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/1mb-dev/shim/internal/adapter"
	"github.com/1mb-dev/shim/internal/config"
)

// clientProvider is the optional-interface extension adapters use to supply
// their own HTTP client (with adapter-specific timeouts, retries, etc.).
// Adapters that don't implement it fall back to http.DefaultClient.
type clientProvider interface {
	Client() *http.Client
}

// Server bundles the HTTP server with its dependencies. Construct via New;
// run via Start; tear down via Shutdown.
type Server struct {
	cfg     *config.Config
	log     *slog.Logger
	adapter adapter.Adapter
	client  *http.Client
	http    *http.Server
}

// New constructs a Server. The adapter must already be registered (via the
// blank import of its package) and configured (e.g. via deepseek.Configure).
// Adapter.Validate() runs once here; misconfiguration fails startup loudly
// rather than the first request.
func New(cfg *config.Config, log *slog.Logger) (*Server, error) {
	a, ok := adapter.Get(cfg.Adapter)
	if !ok {
		return nil, fmt.Errorf("adapter %q not registered (available: %v)", cfg.Adapter, adapter.Names())
	}

	if err := a.Validate(); err != nil {
		return nil, fmt.Errorf("adapter %q validation failed: %w", cfg.Adapter, err)
	}

	client := http.DefaultClient
	if cp, ok := a.(clientProvider); ok {
		client = cp.Client()
	}

	s := &Server{
		cfg:     cfg,
		log:     log,
		adapter: a,
		client:  client,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/messages", s.handleMessages)
	mux.HandleFunc("POST /v1/messages/count_tokens", s.handleCountTokens)
	mux.HandleFunc("GET /health", s.handleHealth)

	s.http = &http.Server{
		Addr:              cfg.BindAddr + ":" + strconv.Itoa(cfg.Port),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      70 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20, // 1 MiB; matches net/http default, made explicit to pair with MAX_REQUEST_BYTES.
	}
	return s, nil
}

// Addr returns the bind address the server will listen on.
func (s *Server) Addr() string { return s.http.Addr }

// Start blocks serving HTTP until the server is shut down. Logs a single
// "shim listening" line once the listener is up.
func (s *Server) Start() error {
	s.log.Info("shim listening",
		slog.String("addr", s.http.Addr),
		slog.String("adapter", s.adapter.Name()),
	)
	if err := s.http.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// Shutdown gracefully drains in-flight requests until ctx is done.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.http.Shutdown(ctx)
}
