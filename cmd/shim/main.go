// Command shim is a Go-native proxy that lets Claude Code run against any
// OpenAI-compatible model provider via ANTHROPIC_BASE_URL.
//
// Two sub-commands:
//
//	shim          // start the server (default)
//	shim run …    // launch claude with ANTHROPIC_BASE_URL/_API_KEY injected
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	// Adapter registration via init().
	"github.com/1mb-dev/shim/internal/adapter/deepseek"
	"github.com/1mb-dev/shim/internal/config"
	"github.com/1mb-dev/shim/internal/launcher"
	"github.com/1mb-dev/shim/internal/obslog"
	"github.com/1mb-dev/shim/internal/server"
)

func main() {
	code, err := dispatch(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "shim:", err)
		os.Exit(1)
	}
	os.Exit(code)
}

// dispatch parses argv and returns (exitCode, err). Exit code is honoured
// when err is nil; err prints "shim: ..." and exits 1.
func dispatch(args []string) (int, error) {
	if len(args) >= 1 && args[0] == "run" {
		return runLauncher(args[1:])
	}
	return 0, runServer()
}

func runLauncher(args []string) (int, error) {
	cfg, err := loadConfig()
	if err != nil {
		return 0, err
	}
	base := fmt.Sprintf("http://%s:%d", cfg.BindAddr, cfg.Port)
	return launcher.Run(launcher.Options{
		BaseURL: base,
		APIKey:  "shim",
		Args:    args,
	})
}

func runServer() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	log := setupLogger(cfg)
	deepseek.Configure(deepseek.ConfigureOpts{
		BaseURL:       cfg.UpstreamBaseURL,
		APIKey:        cfg.UpstreamAPIKey,
		ModelOverride: cfg.UpstreamModel,
		OpusModel:     cfg.UpstreamOpusModel,
		SonnetModel:   cfg.UpstreamSonnetModel,
		HaikuModel:    cfg.UpstreamHaikuModel,
	})

	srv, err := server.New(cfg, log)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start() }()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		log.Info("shim shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

func loadConfig() (*config.Config, error) {
	envPath := ".env"
	if v := os.Getenv("SHIM_ENV_FILE"); v != "" {
		envPath = v
	}
	return config.Load(envPath)
}

func setupLogger(cfg *config.Config) *slog.Logger {
	level := slog.LevelInfo
	switch strings.ToLower(cfg.LogLevel) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	return obslog.New(os.Stderr, level, cfg.LogRedact)
}
