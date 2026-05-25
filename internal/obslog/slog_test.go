package obslog

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// captured decodes a single JSON log line.
func captured(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	out := strings.TrimSpace(buf.String())
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("decode log line %q: %v", out, err)
	}
	return m
}

func TestRedactHeaders(t *testing.T) {
	var buf bytes.Buffer
	lg := New(&buf, slog.LevelInfo, true)
	lg.Info("req",
		slog.String("Authorization", "Bearer sk-secret-123"),
		slog.String("X-Api-Key", "abc"),
		slog.String("Cookie", "session=xyz"),
	)
	m := captured(t, &buf)
	for _, k := range []string{"Authorization", "X-Api-Key", "Cookie"} {
		if got := m[k]; got != "[REDACTED]" {
			t.Errorf("%s = %v, want [REDACTED]", k, got)
		}
	}
}

func TestRedactContent(t *testing.T) {
	var buf bytes.Buffer
	lg := New(&buf, slog.LevelInfo, true)
	lg.Info("req",
		slog.String("messages", `[{"role":"user","content":"hello"}]`),
		slog.String("content", "the actual prompt"),
		slog.String("prompt", "another prompt"),
		slog.String("system", "system prompt"),
	)
	m := captured(t, &buf)
	for _, k := range []string{"messages", "content", "prompt", "system"} {
		if got := m[k]; got != "[REDACTED]" {
			t.Errorf("%s = %v, want [REDACTED]", k, got)
		}
	}
}

func TestRedactCredentialSubstring(t *testing.T) {
	var buf bytes.Buffer
	lg := New(&buf, slog.LevelInfo, true)
	lg.Info("env",
		slog.String("UPSTREAM_API_KEY", "sk-xxx"),
		slog.String("MY_SECRET_TOKEN", "yyy"),
		slog.String("user_password", "pw"),
		slog.String("oauth_token", "tok"),
	)
	m := captured(t, &buf)
	for _, k := range []string{"UPSTREAM_API_KEY", "MY_SECRET_TOKEN", "user_password", "oauth_token"} {
		if got := m[k]; got != "[REDACTED]" {
			t.Errorf("%s = %v, want [REDACTED]", k, got)
		}
	}
}

func TestRedactURLQuery(t *testing.T) {
	var buf bytes.Buffer
	lg := New(&buf, slog.LevelInfo, true)
	lg.Info("req",
		slog.String("url", "https://api.example.com/v1/chat?api_key=sk-xxx&foo=bar"),
		slog.String("safe_url", "https://api.example.com/v1/chat"),
		slog.String("not_a_url", "x?y"),
	)
	m := captured(t, &buf)
	if got := m["url"]; got != "https://api.example.com/v1/chat?[REDACTED]" {
		t.Errorf("url = %v, want stripped query", got)
	}
	if got := m["safe_url"]; got != "https://api.example.com/v1/chat" {
		t.Errorf("safe_url = %v, want unchanged", got)
	}
	if got := m["not_a_url"]; got != "x?y" {
		t.Errorf("not_a_url = %v, want unchanged (not a URL)", got)
	}
}

func TestRedactNestedGroup(t *testing.T) {
	var buf bytes.Buffer
	lg := New(&buf, slog.LevelInfo, true)
	lg.Info("req",
		slog.Group("headers",
			slog.String("Authorization", "Bearer sk-secret"),
			slog.String("Content-Type", "application/json"),
		),
		slog.Group("body",
			slog.Group("inner",
				slog.String("content", "the prompt"),
			),
		),
	)
	m := captured(t, &buf)
	hdrs, ok := m["headers"].(map[string]any)
	if !ok {
		t.Fatalf("headers not a group: %T", m["headers"])
	}
	if hdrs["Authorization"] != "[REDACTED]" {
		t.Errorf("headers.Authorization = %v, want [REDACTED]", hdrs["Authorization"])
	}
	if hdrs["Content-Type"] != "application/json" {
		t.Errorf("headers.Content-Type = %v, want unchanged", hdrs["Content-Type"])
	}
	body, ok := m["body"].(map[string]any)
	if !ok {
		t.Fatalf("body not a group")
	}
	inner, ok := body["inner"].(map[string]any)
	if !ok {
		t.Fatalf("body.inner not a group")
	}
	if inner["content"] != "[REDACTED]" {
		t.Errorf("body.inner.content = %v, want [REDACTED]", inner["content"])
	}
}

func TestWithAttrsRedacted(t *testing.T) {
	var buf bytes.Buffer
	lg := New(&buf, slog.LevelInfo, true)
	lg2 := lg.With(slog.String("Authorization", "Bearer sk-secret"))
	lg2.Info("req")
	m := captured(t, &buf)
	if m["Authorization"] != "[REDACTED]" {
		t.Errorf("Authorization (via With) = %v, want [REDACTED]", m["Authorization"])
	}
}

func TestRedactDisabled(t *testing.T) {
	var buf bytes.Buffer
	lg := New(&buf, slog.LevelInfo, false) // redact OFF
	lg.Info("req",
		slog.String("Authorization", "Bearer sk-secret"),
		slog.String("messages", "raw prompt"),
		slog.String("url", "https://example.com/x?key=secret"),
	)
	m := captured(t, &buf)
	if m["Authorization"] != "Bearer sk-secret" {
		t.Errorf("Authorization = %v, want raw when redact=false", m["Authorization"])
	}
	if m["messages"] != "raw prompt" {
		t.Errorf("messages = %v, want raw when redact=false", m["messages"])
	}
	if m["url"] != "https://example.com/x?key=secret" {
		t.Errorf("url = %v, want raw when redact=false", m["url"])
	}
}

// TestRedactPanicRecovery — a handler that panics during inner.Handle must
// not leak attrs via Go's recover/defer machinery. We simulate this by
// wrapping a pathological inner handler that captures attrs before panicking;
// the redacting layer must have already scrubbed them.
func TestRedactPanicRecovery(t *testing.T) {
	cap := &captureHandler{}
	h := &redactingHandler{inner: cap}
	lg := slog.New(h)

	defer func() {
		_ = recover()
		// Whatever attrs the inner handler saw must already be redacted.
		for _, a := range cap.seen {
			if a.Key == "Authorization" && a.Value.String() != "[REDACTED]" {
				t.Errorf("inner handler saw raw Authorization: %v", a.Value)
			}
			if a.Key == "messages" && a.Value.String() != "[REDACTED]" {
				t.Errorf("inner handler saw raw messages: %v", a.Value)
			}
		}
	}()

	lg.Info("req",
		slog.String("Authorization", "Bearer sk-secret"),
		slog.String("messages", "raw prompt"),
	)
}

// captureHandler records attrs and panics on Handle. Used by
// TestRedactPanicRecovery to assert that the redactor scrubs BEFORE the
// downstream handler sees them.
type captureHandler struct {
	seen []slog.Attr
}

func (c *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (c *captureHandler) Handle(_ context.Context, r slog.Record) error {
	r.Attrs(func(a slog.Attr) bool {
		c.seen = append(c.seen, a)
		return true
	})
	panic("simulated downstream panic")
}
func (c *captureHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	c.seen = append(c.seen, attrs...)
	return c
}
func (c *captureHandler) WithGroup(string) slog.Handler { return c }
