// Package config loads shim configuration from a .env file and the process
// environment. Zero external dependencies.
package config

import (
	"bufio"
	"fmt"
	"io"
	"os"
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
	UpstreamModel   string // optional
	LogLevel        string
	LogRedact       bool
	MaxRequestBytes int64
}

var defaults = map[string]string{
	"BIND_ADDR":         "127.0.0.1",
	"PORT":              "8082",
	"ADAPTER":           "deepseek",
	"UPSTREAM_BASE_URL": "https://api.deepseek.com/v1",
	"LOG_LEVEL":         "info",
	"LOG_REDACT":        "true",
	"MAX_REQUEST_BYTES": "1048576",
}

var required = []string{"UPSTREAM_API_KEY"}

// Load reads a .env file (if present) into the process environment, then
// constructs a Config. Existing process env always wins over .env entries.
func Load(envPath string) (*Config, error) {
	if err := loadDotEnv(envPath); err != nil {
		return nil, fmt.Errorf("load .env: %w", err)
	}
	for _, k := range required {
		if strings.TrimSpace(os.Getenv(k)) == "" {
			return nil, fmt.Errorf("%s not set in %s or process env", k, envPath)
		}
	}

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
		BindAddr:        get("BIND_ADDR"),
		Port:            port,
		Adapter:         get("ADAPTER"),
		UpstreamAPIKey:  os.Getenv("UPSTREAM_API_KEY"),
		UpstreamBaseURL: get("UPSTREAM_BASE_URL"),
		UpstreamModel:   os.Getenv("UPSTREAM_MODEL"),
		LogLevel:        get("LOG_LEVEL"),
		LogRedact:       redact,
		MaxRequestBytes: maxBytes,
	}, nil
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
