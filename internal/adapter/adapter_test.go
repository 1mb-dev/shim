package adapter

import (
	"context"
	"net/http"
	"sync"
	"testing"
)

// fake is a minimal Adapter for registry tests.
type fake struct{ name string }

func (f *fake) Name() string                                                { return f.name }
func (f *fake) DefaultModel() string                                        { return "default" }
func (f *fake) MapModel(string) string                                      { return "mapped" }
func (f *fake) BuildRequest(context.Context, []byte) (*http.Request, error) { return nil, nil }
func (f *fake) NormalizeResponse(*http.Response) ([]byte, error)            { return nil, nil }

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
