// Package obslog wraps log/slog with a redacting JSON handler. Default-on
// redaction strips Authorization-style headers, prompt/message content, and
// the query portion of URL-shaped values, regardless of nesting depth.
package obslog

import (
	"context"
	"io"
	"log/slog"
	"strings"
)

const redacted = "[REDACTED]"

// New returns a slog.Logger writing JSON to w. When redact is true, all
// attrs are walked recursively and sensitive keys / URL query strings are
// scrubbed before emission. Set redact=false ONLY for local debugging.
func New(w io.Writer, level slog.Level, redact bool) *slog.Logger {
	base := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})
	if !redact {
		return slog.New(base)
	}
	return slog.New(&redactingHandler{inner: base})
}

// redactingHandler wraps any slog.Handler and rewrites attrs before passing
// them through. WithAttrs / WithGroup also redact so pre-bound attrs can't
// leak.
type redactingHandler struct {
	inner slog.Handler
}

func (h *redactingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *redactingHandler) Handle(ctx context.Context, r slog.Record) error {
	nr := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(a slog.Attr) bool {
		nr.AddAttrs(redactAttr(a))
		return true
	})
	return h.inner.Handle(ctx, nr)
}

func (h *redactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		out[i] = redactAttr(a)
	}
	return &redactingHandler{inner: h.inner.WithAttrs(out)}
}

func (h *redactingHandler) WithGroup(name string) slog.Handler {
	return &redactingHandler{inner: h.inner.WithGroup(name)}
}

// redactAttr returns a copy of a with its value scrubbed when warranted.
// Groups are walked depth-first so nested attrs can't escape redaction.
func redactAttr(a slog.Attr) slog.Attr {
	if a.Value.Kind() == slog.KindGroup {
		inner := a.Value.Group()
		out := make([]slog.Attr, len(inner))
		for i, g := range inner {
			out[i] = redactAttr(g)
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(out...)}
	}
	if isSensitiveKey(a.Key) {
		return slog.String(a.Key, redacted)
	}
	if a.Value.Kind() == slog.KindString {
		if stripped, ok := stripURLQuery(a.Value.String()); ok {
			return slog.String(a.Key, stripped)
		}
	}
	return a
}

// isSensitiveKey returns true for keys whose values must never be logged.
// Matching is case-insensitive: substring for credential-shaped names, exact
// for known HTTP headers and request-body content keys.
func isSensitiveKey(k string) bool {
	lk := strings.ToLower(k)
	switch lk {
	case "authorization", "cookie", "set-cookie", "x-api-key",
		"proxy-authorization", "messages", "system", "prompt",
		"content", "body", "system_prompt", "tool_result", "input":
		return true
	}
	for _, sub := range []string{"api_key", "apikey", "secret", "password", "token"} {
		if strings.Contains(lk, sub) {
			return true
		}
	}
	return false
}

// stripURLQuery returns the URL with its query string replaced by a redacted
// sentinel when the input is URL-shaped (http/https scheme + '?'). The
// second return value is true only when a strip happened.
func stripURLQuery(s string) (string, bool) {
	q := strings.IndexByte(s, '?')
	if q < 0 {
		return s, false
	}
	if !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") {
		return s, false
	}
	return s[:q] + "?" + redacted, true
}
