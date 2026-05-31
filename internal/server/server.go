// Package server wires shim's HTTP routes and owns the per-request
// translation flow.
package server

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/1mb-dev/shim/internal/adapter"
	"github.com/1mb-dev/shim/internal/config"
	"github.com/1mb-dev/shim/internal/measure"
	"github.com/1mb-dev/shim/internal/tokens"
)

// Server bundles the HTTP server with its dependencies. Construct via New;
// run via Start; tear down via Shutdown.
type Server struct {
	cfg     *config.Config
	log     *slog.Logger
	adapter adapter.Adapter
	client  *http.Client
	http    *http.Server
	measure *measure.Collector
}

// New constructs a Server. The adapter must already be registered
// (cmd/shim/main.go constructs the adapter via its New and calls
// adapter.Register). Adapter.Validate() runs once here; misconfiguration
// fails startup loudly rather than the first request.
func New(cfg *config.Config, log *slog.Logger) (*Server, error) {
	a, ok := adapter.Get(cfg.Adapter)
	if !ok {
		return nil, fmt.Errorf("adapter %q not registered (available: %v)", cfg.Adapter, adapter.Names())
	}

	if err := a.Validate(); err != nil {
		return nil, fmt.Errorf("adapter %q validation failed: %w", cfg.Adapter, err)
	}

	// Load cl100k_base BPE up front so a corrupt embed blocks startup
	// rather than panicking on first /v1/messages or /v1/metrics read.
	if err := tokens.Init(); err != nil {
		return nil, fmt.Errorf("tokens init: %w", err)
	}

	s := &Server{
		cfg:     cfg,
		log:     log,
		adapter: a,
		client:  newUpstreamClient(),
		measure: measure.New(),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/messages", s.handleMessages)
	mux.HandleFunc("POST /v1/messages/count_tokens", s.handleCountTokens)
	mux.HandleFunc("POST /v1/messages/explain", s.handleExplain)
	mux.HandleFunc("GET /v1/metrics", s.handleMetrics)
	mux.HandleFunc("GET /metrics", s.handleMetricsPrometheus)
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /readyz", s.handleReady)

	s.http = &http.Server{
		Addr:              cfg.BindAddr + ":" + strconv.Itoa(cfg.Port),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// WriteTimeout sized to outlive Client.Timeout below — so when an
		// upstream call is slow, the cancellation surfaces as an
		// upstream-error (visible in logs + metrics) instead of as a
		// server-side write timeout (which has no upstream context).
		WriteTimeout:   200 * time.Second,
		IdleTimeout:    120 * time.Second,
		MaxHeaderBytes: 1 << 20, // 1 MiB; matches net/http default, made explicit to pair with MAX_REQUEST_BYTES.
	}
	return s, nil
}

// Addr returns the bind address the server will listen on.
func (s *Server) Addr() string { return s.http.Addr }

// Start blocks serving HTTP until the server is shut down. Listens
// first, then logs the actually-bound addr (matters when PORT=0 picks
// an ephemeral port — thesis-1: don't lie about what we did).
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.http.Addr)
	if err != nil {
		return err
	}
	s.log.Info("shim listening",
		slog.String("addr", ln.Addr().String()),
		slog.String("adapter", s.adapter.Name()),
	)
	if err := s.http.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// Shutdown gracefully drains in-flight requests until ctx is done.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.http.Shutdown(ctx)
}

// newUpstreamClient builds the *http.Client shim uses to talk to the
// configured adapter's upstream. Split timeouts so a slow handshake or
// slow header response can't pin a goroutine for the whole 60s window;
// per-host connection limits + HTTP/2 attempt + explicit TLS minimum so
// connection reuse is healthy across bursts of upstream calls.
//
// Single client lives on Server; adapters no longer own one. If a future
// adapter needs different tuning (e.g. higher per-host concurrency for a
// fast upstream like Groq), revisit whether to make this per-adapter then.
func newUpstreamClient() *http.Client {
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		MaxConnsPerHost:       0, // unbounded; rely on upstream + downstream rate limits
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}
	return &http.Client{
		// 180s ceiling: covers DeepSeek v4-pro reasoning-mode generations
		// (~30-60s think + ~30-60s content under buffer-then-restream MVP).
		// Real Claude Code experiment hit Client.Timeout at 60s on a
		// multi-persona review (Stage 2.6b reproduction, 2026-05-27).
		// Pair: server WriteTimeout = 200s in New() so the upstream
		// timeout fires first and surfaces as a recordable upstream error.
		Timeout:   180 * time.Second,
		Transport: transport,
	}
}
