package config

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseDotEnv(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    map[string]string
		wantErr bool
	}{
		{
			name: "basic",
			in:   "FOO=bar\nBAZ=qux\n",
			want: map[string]string{"FOO": "bar", "BAZ": "qux"},
		},
		{
			name: "comments and blanks ignored",
			in:   "# this is a comment\n\nFOO=bar\n# another\nBAZ=qux\n",
			want: map[string]string{"FOO": "bar", "BAZ": "qux"},
		},
		{
			name: "double-quoted value",
			in:   `FOO="bar baz"` + "\n",
			want: map[string]string{"FOO": "bar baz"},
		},
		{
			name: "single-quoted value",
			in:   "FOO='bar baz'\n",
			want: map[string]string{"FOO": "bar baz"},
		},
		{
			name: "whitespace around equals",
			in:   "  FOO  =  bar  \n",
			want: map[string]string{"FOO": "bar"},
		},
		{
			name: "value with equals sign",
			in:   "URL=https://example.com/path?a=1&b=2\n",
			want: map[string]string{"URL": "https://example.com/path?a=1&b=2"},
		},
		{
			name:    "malformed line (no equals)",
			in:      "NOTAVAR\n",
			wantErr: true,
		},
		{
			name:    "malformed line (leading equals)",
			in:      "=value\n",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseDotEnv(strings.NewReader(tc.in))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLoadMissingRequired(t *testing.T) {
	t.Setenv("UPSTREAM_API_KEY", "")
	_, err := Load(filepath.Join(t.TempDir(), "nonexistent.env"))
	if err == nil {
		t.Fatal("expected error for missing UPSTREAM_API_KEY")
	}
	if !strings.Contains(err.Error(), "UPSTREAM_API_KEY") {
		t.Errorf("error did not name the missing var: %v", err)
	}
}

func TestLoadDefaults(t *testing.T) {
	t.Setenv("UPSTREAM_API_KEY", "sk-test")
	for _, k := range []string{"BIND_ADDR", "PORT", "ADAPTER", "UPSTREAM_BASE_URL", "LOG_LEVEL", "LOG_REDACT", "MAX_REQUEST_BYTES", "UPSTREAM_MODEL", "UPSTREAM_OPUS_MODEL", "UPSTREAM_SONNET_MODEL", "UPSTREAM_HAIKU_MODEL"} {
		t.Setenv(k, "")
	}
	cfg, err := Load(filepath.Join(t.TempDir(), "nonexistent.env"))
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	checks := map[string]any{
		"BindAddr":            "127.0.0.1",
		"Port":                8082,
		"Adapter":             "deepseek",
		"UpstreamBaseURL":     "https://api.deepseek.com/v1",
		"UpstreamModel":       "",
		"UpstreamOpusModel":   "",
		"UpstreamSonnetModel": "",
		"UpstreamHaikuModel":  "",
		"LogLevel":            "info",
		"LogRedact":           true,
		"MaxRequestBytes":     int64(1048576),
	}
	got := map[string]any{
		"BindAddr":            cfg.BindAddr,
		"Port":                cfg.Port,
		"Adapter":             cfg.Adapter,
		"UpstreamBaseURL":     cfg.UpstreamBaseURL,
		"UpstreamModel":       cfg.UpstreamModel,
		"UpstreamOpusModel":   cfg.UpstreamOpusModel,
		"UpstreamSonnetModel": cfg.UpstreamSonnetModel,
		"UpstreamHaikuModel":  cfg.UpstreamHaikuModel,
		"LogLevel":            cfg.LogLevel,
		"LogRedact":           cfg.LogRedact,
		"MaxRequestBytes":     cfg.MaxRequestBytes,
	}
	for k, want := range checks {
		if got[k] != want {
			t.Errorf("%s = %v, want %v", k, got[k], want)
		}
	}
}

// TestLoadPerRoleOverridesIndependent: setting only UPSTREAM_OPUS_MODEL must
// not contaminate the sonnet/haiku fields. Each role is independent.
func TestLoadPerRoleOverridesIndependent(t *testing.T) {
	t.Setenv("UPSTREAM_API_KEY", "sk-test")
	t.Setenv("UPSTREAM_OPUS_MODEL", "deepseek-v4-pro[1m]")
	t.Setenv("UPSTREAM_SONNET_MODEL", "")
	t.Setenv("UPSTREAM_HAIKU_MODEL", "")
	t.Setenv("UPSTREAM_MODEL", "")
	cfg, err := Load(filepath.Join(t.TempDir(), "nonexistent.env"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.UpstreamOpusModel != "deepseek-v4-pro[1m]" {
		t.Errorf("OpusModel = %q, want deepseek-v4-pro[1m]", cfg.UpstreamOpusModel)
	}
	if cfg.UpstreamSonnetModel != "" {
		t.Errorf("SonnetModel leaked: %q", cfg.UpstreamSonnetModel)
	}
	if cfg.UpstreamHaikuModel != "" {
		t.Errorf("HaikuModel leaked: %q", cfg.UpstreamHaikuModel)
	}
}

func TestLoadBadInt(t *testing.T) {
	t.Setenv("UPSTREAM_API_KEY", "sk-test")
	t.Setenv("PORT", "not-a-port")
	_, err := Load(filepath.Join(t.TempDir(), "nonexistent.env"))
	if err == nil || !strings.Contains(err.Error(), "PORT") {
		t.Fatalf("expected PORT error, got %v", err)
	}
}

func TestLoadBadBool(t *testing.T) {
	t.Setenv("UPSTREAM_API_KEY", "sk-test")
	t.Setenv("LOG_REDACT", "maybe")
	_, err := Load(filepath.Join(t.TempDir(), "nonexistent.env"))
	if err == nil || !strings.Contains(err.Error(), "LOG_REDACT") {
		t.Fatalf("expected LOG_REDACT error, got %v", err)
	}
}

func TestLoadProcessEnvWins(t *testing.T) {
	t.Setenv("UPSTREAM_API_KEY", "from-proc")

	envPath := filepath.Join(t.TempDir(), ".env")
	if err := writeFile(envPath, "UPSTREAM_API_KEY=from-file\n"); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.UpstreamAPIKey != "from-proc" {
		t.Errorf("UpstreamAPIKey = %q, want from-proc (process env should win)", cfg.UpstreamAPIKey)
	}
}
