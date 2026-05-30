package adapter

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/1mb-dev/shim/internal/translate"
)

// fake is a minimal Adapter for registry tests.
type fake struct{ name string }

func (f *fake) Name() string                                                { return f.name }
func (f *fake) MapModel(string) string                                      { return "mapped" }
func (f *fake) Validate() error                                             { return nil }
func (f *fake) BuildRequest(context.Context, []byte) (*http.Request, error) { return nil, nil }
func (f *fake) NormalizeResponse(*http.Response) ([]byte, error)            { return nil, nil }
func (f *fake) Translator() translate.Translator                            { return translate.AnthropicOpenAI() }

// resetRegistry isolates tests from each other. Test-only helper.
func resetRegistry(t *testing.T) {
	t.Helper()
	mu.Lock()
	defer mu.Unlock()
	for k := range registry {
		delete(registry, k)
	}
}

func TestRegisterAndGet(t *testing.T) {
	resetRegistry(t)
	Register(&fake{name: "x"})
	got, ok := Get("x")
	if !ok {
		t.Fatal("Get returned ok=false for registered adapter")
	}
	if got.Name() != "x" {
		t.Errorf("Get returned %q, want x", got.Name())
	}
}

func TestGetMissing(t *testing.T) {
	resetRegistry(t)
	_, ok := Get("never-registered")
	if ok {
		t.Fatal("Get returned ok=true for missing adapter")
	}
}

func TestRegisterDuplicatePanics(t *testing.T) {
	resetRegistry(t)
	Register(&fake{name: "y"})
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on duplicate registration")
		}
	}()
	Register(&fake{name: "y"})
}

func TestRegisterNilPanics(t *testing.T) {
	resetRegistry(t)
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil adapter")
		}
	}()
	Register(nil)
}

func TestRegisterEmptyNamePanics(t *testing.T) {
	resetRegistry(t)
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on empty-name adapter")
		}
	}()
	Register(&fake{name: ""})
}

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

func TestConcurrentGet(t *testing.T) {
	resetRegistry(t)
	Register(&fake{name: "concurrent"})
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok := Get("concurrent"); !ok {
				t.Errorf("Get failed under contention")
			}
		}()
	}
	wg.Wait()
}
