package adapter

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// errReadCloser fails on Read and records Close — exercises the read-failure
// branch and proves ReadNormalizedResponse still closes the body.
type errReadCloser struct{ closed bool }

func (e *errReadCloser) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (e *errReadCloser) Close() error             { e.closed = true; return nil }

func TestReadNormalizedResponse(t *testing.T) {
	mkResp := func(status int, body string) *http.Response {
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}
	}

	// 2xx → body verbatim, no error.
	body, err := ReadNormalizedResponse("test", mkResp(200, `{"ok":true}`))
	if err != nil || string(body) != `{"ok":true}` {
		t.Fatalf("2xx: body=%s err=%v", body, err)
	}

	// non-2xx → body returned AND error carrying name + status.
	body, err = ReadNormalizedResponse("test", mkResp(503, `{"e":1}`))
	if err == nil {
		t.Fatal("non-2xx: expected error")
	}
	if string(body) != `{"e":1}` {
		t.Errorf("non-2xx: body lost: %s", body)
	}
	if !strings.Contains(err.Error(), "test: upstream status 503") {
		t.Errorf("non-2xx: err = %q, want name+status", err)
	}

	// nil response → error, no panic.
	if _, err := ReadNormalizedResponse("test", nil); err == nil {
		t.Error("nil response: expected error")
	}

	// read failure → error AND body still closed (no connection leak).
	erc := &errReadCloser{}
	if _, err := ReadNormalizedResponse("test", &http.Response{StatusCode: 200, Body: erc}); err == nil {
		t.Error("read failure: expected error")
	}
	if !erc.closed {
		t.Error("read failure: body not closed (leak)")
	}
}
