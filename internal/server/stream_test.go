package server

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
)

// noFlushWriter is an http.ResponseWriter that does NOT implement http.Flusher.
// Used to exercise streamSSE's early bail when the underlying writer can't
// flush — a misconfigured reverse proxy in front of shim would surface here.
type noFlushWriter struct {
	h    http.Header
	body bytes.Buffer
	code int
}

func (n *noFlushWriter) Header() http.Header {
	if n.h == nil {
		n.h = http.Header{}
	}
	return n.h
}
func (n *noFlushWriter) Write(b []byte) (int, error) { return n.body.Write(b) }
func (n *noFlushWriter) WriteHeader(code int)        { n.code = code }

func TestStreamSSE_NoFlusher(t *testing.T) {
	w := &noFlushWriter{}
	// streamSSE must bail before writing any byte, so the chunk content is
	// irrelevant — any valid SSE frame does.
	chunk := []byte("event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
	sent := false
	next := func() ([]byte, bool, error) {
		if sent {
			return nil, false, nil
		}
		sent = true
		return chunk, true, nil
	}

	err := streamSSE(w, next)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "Flush") {
		t.Errorf("error should mention Flush: %v", err)
	}
	if n := w.body.Len(); n != 0 {
		t.Errorf("no bytes should be written when Flush is unsupported, got %d", n)
	}
}
