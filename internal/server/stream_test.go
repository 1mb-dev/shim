package server

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/1mb-dev/shim/internal/translate"
)

// noFlushWriter is an http.ResponseWriter that does NOT implement http.Flusher.
// Used to exercise writeSSE's early bail when the underlying writer can't
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

func TestWriteSSE_NoFlusher(t *testing.T) {
	w := &noFlushWriter{}
	events := []translate.SSEEvent{{Name: "message_start", Data: map[string]string{"type": "message_start"}}}

	err := writeSSE(w, events)
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
