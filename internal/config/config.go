// Package config loads shim configuration from a .env file and the process
// environment. Zero external dependencies.
package config

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config holds runtime configuration. Fields are populated from the process
// environment, optionally pre-seeded from a .env file.
type Config struct {
	BindAddr        string
	Port            int
	Adapter         string
	UpstreamAPIKey  string
	UpstreamBaseURL string
	UpstreamModel   string // optional; catch-all for non-claude-* inputs
	// Per-role overrides — empty means "use adapter default for this role."
	// Claude Code sends claude-opus*/sonnet*/haiku* model names; the adapter
	// maps those to upstream-specific models. These env vars override the
	// adapter's per-role default.
	UpstreamOpusModel   string
	UpstreamSonnetModel string
	UpstreamHaikuModel  string
	LogLevel            string
	LogRedact           bool
	MaxRequestBytes     int64
}

var defaults = map[string]string{
	"BIND_ADDR":         "127.0.0.1",
	"PORT":              "8082",
	"ADAPTER":           "deepseek",
	"LOG_LEVEL":         "info",
	"LOG_REDACT":        "true",
	"MAX_REQUEST_BYTES": "1048576",
}

// Load reads a .env file (if present) into the process environment, then
// constructs a Config. Existing process env always wins over .env entries.
func Load(envPath string) (*Config, error) {
	if err := loadDotEnv(envPath); err != nil {
		return nil, fmt.Errorf("load .env: %w", err)
	}
	// Auth requirement is enforced per-adapter by Adapter.Validate at server
	// startup (a no-auth preset like Ollama needs no key), not globally here.

	port, err := strconv.Atoi(get("PORT"))
	if err != nil {
		return nil, fmt.Errorf("PORT not an integer: %w", err)
	}
	maxBytes, err := strconv.ParseInt(get("MAX_REQUEST_BYTES"), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("MAX_REQUEST_BYTES not an integer: %w", err)
	}
	redact, err := strconv.ParseBool(get("LOG_REDACT"))
	if err != nil {
		return nil, fmt.Errorf("LOG_REDACT not a bool: %w", err)
	}

	return &Config{
		BindAddr:            get("BIND_ADDR"),
		Port:                port,
		Adapter:             get("ADAPTER"),
		UpstreamAPIKey:      os.Getenv("UPSTREAM_API_KEY"),
		UpstreamBaseURL:     os.Getenv("UPSTREAM_BASE_URL"), // empty ⇒ adapter/preset supplies its default
		UpstreamModel:       os.Getenv("UPSTREAM_MODEL"),
		UpstreamOpusModel:   os.Getenv("UPSTREAM_OPUS_MODEL"),
		UpstreamSonnetModel: os.Getenv("UPSTREAM_SONNET_MODEL"),
		UpstreamHaikuModel:  os.Getenv("UPSTREAM_HAIKU_MODEL"),
		LogLevel:            get("LOG_LEVEL"),
		LogRedact:           redact,
		MaxRequestBytes:     maxBytes,
	}, nil
}

// DefaultEnvPath resolves which .env file to load, in order: SHIM_ENV_FILE
// (explicit override), ./.env (working directory — the dev workflow), then
// <user-config>/shim/.env (so a background service started by `brew services`
// finds its config regardless of working directory). Returns "" when none
// exists; Load then runs from the process environment alone.
//
// <user-config> is $XDG_CONFIG_HOME or ~/.config — i.e. ~/.config/shim/.env.
func DefaultEnvPath() string {
	if v := os.Getenv("SHIM_ENV_FILE"); v != "" {
		return v
	}
	if fileExists(".env") {
		return ".env"
	}
	if dir := userConfigDir(); dir != "" {
		if p := filepath.Join(dir, "shim", ".env"); fileExists(p) {
			return p
		}
	}
	return ""
}

// userConfigDir returns $XDG_CONFIG_HOME, else ~/.config. (os.UserConfigDir
// returns ~/Library/Application Support on macOS; shim uses the cross-platform
// ~/.config so the docs name one path on every OS.)
func userConfigDir() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return x
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config")
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func get(key string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaults[key]
}

// loadDotEnv parses a .env file (no-op if missing) and sets entries in the
// process env when the variable is not already set. Process env wins.
func loadDotEnv(path string) error {
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	pairs, err := parseDotEnv(f)
	if err != nil {
		return err
	}
	for k, v := range pairs {
		if _, set := os.LookupEnv(k); set {
			continue
		}
		if err := os.Setenv(k, v); err != nil {
			return err
		}
	}
	return nil
}

// parseDotEnv is pure: input → key/value pairs. No env mutation. Empty lines
// and lines starting with '#' are skipped. Surrounding single or double
// quotes on values are stripped.
func parseDotEnv(r io.Reader) (map[string]string, error) {
	out := make(map[string]string)
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 1 {
			return nil, fmt.Errorf("malformed line: %q", line)
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		if len(val) >= 2 && (val[0] == '"' || val[0] == '\'') && val[0] == val[len(val)-1] {
			val = val[1 : len(val)-1]
		}
		out[key] = val
	}
	return out, scanner.Err()
}
