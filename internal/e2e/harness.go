//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sync"
	"testing"
	"time"
)

// shimBinary is the path to the ./shim binary built in TestMain. Set
// once per test binary; all Harness instances reuse it.
var shimBinary string

// HarnessOpts are optional overrides for Start.
type HarnessOpts struct {
	// ExtraEnv is appended to the standard env block; later entries
	// override earlier ones (Go's exec.Cmd semantics).
	ExtraEnv []string
}

// Harness owns one spawned ./shim process + one fake upstream for the
// duration of a single test. Construct via Start.
type Harness struct {
	URL      string // http://127.0.0.1:<port> — port discovered from shim's listen log
	Port     string // bare port for sub-process env (./shim run needs PORT)
	Upstream *FakeUpstream

	t         *testing.T
	cmd       *exec.Cmd
	stderrBuf *boundedBuffer
	cancel    context.CancelFunc
	waitErr   chan error
	stopped   sync.Once
	envFile   string
}

// Start launches a fresh shim + fake upstream pair. Cleanup is
// registered via t.Cleanup — kill shim first, then close upstream, so
// shutdown stderr stays clean.
func Start(t *testing.T, opts ...HarnessOpts) *Harness {
	t.Helper()
	if shimBinary == "" {
		t.Fatal("shimBinary unset; TestMain must call BuildShim")
	}

	upstream := NewFakeUpstream()

	// Empty env file so config.Load reads nothing from disk; all config
	// comes from the explicit env block below.
	envFile, err := os.CreateTemp(t.TempDir(), "shim-empty-env-*.env")
	if err != nil {
		upstream.Close()
		t.Fatalf("create empty env file: %v", err)
	}
	envFile.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)

	var opt HarnessOpts
	if len(opts) > 0 {
		opt = opts[0]
	}

	cmd := exec.CommandContext(ctx, shimBinary)
	cmd.Env = append(os.Environ(),
		"SHIM_ENV_FILE="+envFile.Name(),
		"BIND_ADDR=127.0.0.1",
		"PORT=0",
		"ADAPTER=deepseek",
		"UPSTREAM_BASE_URL="+upstream.URL,
		"UPSTREAM_API_KEY=test-key",
		"LOG_LEVEL=info",
		"LOG_REDACT=true",
	)
	cmd.Env = append(cmd.Env, opt.ExtraEnv...)

	sbuf := newBoundedBuffer(1 << 20)
	cmd.Stderr = sbuf

	if err := cmd.Start(); err != nil {
		upstream.Close()
		cancel()
		t.Fatalf("start shim: %v", err)
	}

	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()

	h := &Harness{
		Upstream:  upstream,
		t:         t,
		cmd:       cmd,
		stderrBuf: sbuf,
		cancel:    cancel,
		waitErr:   waitErr,
		envFile:   envFile.Name(),
	}

	addr, err := h.discoverAddr(3 * time.Second)
	if err != nil {
		h.dump()
		h.stop()
		upstream.Close()
		t.Fatalf("discover addr: %v", err)
	}
	h.URL = "http://" + addr
	if i := lastColon(addr); i >= 0 {
		h.Port = addr[i+1:]
	}

	if err := h.waitHealthy(3 * time.Second); err != nil {
		h.dump()
		h.stop()
		upstream.Close()
		t.Fatalf("health: %v", err)
	}

	t.Cleanup(h.cleanup)
	return h
}

func (h *Harness) cleanup() {
	h.stop()
	h.Upstream.Close()
}

// stop terminates the shim process. Sends SIGINT (shim's main.go listens
// for it and shuts down gracefully); escalates to SIGKILL after 5s.
// Idempotent.
func (h *Harness) stop() {
	h.stopped.Do(func() {
		if h.cmd == nil || h.cmd.Process == nil {
			return
		}
		_ = h.cmd.Process.Signal(os.Interrupt)
		select {
		case <-h.waitErr:
		case <-time.After(5 * time.Second):
			_ = h.cmd.Process.Kill()
			<-h.waitErr
		}
		h.cancel()
	})
}

// Stderr returns the current stderr capture as a string.
func (h *Harness) Stderr() string { return h.stderrBuf.String() }

func (h *Harness) dump() {
	h.t.Logf("=== shim stderr ===\n%s", h.Stderr())
}

// addrRe matches the "shim listening" slog JSON record's addr field.
// Server emits: {"time":"...","level":"INFO","msg":"shim listening","addr":"127.0.0.1:54321","adapter":"deepseek"}
var addrRe = regexp.MustCompile(`"addr":"(127\.0\.0\.1:\d+)"`)

func (h *Harness) discoverAddr(timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if m := addrRe.FindStringSubmatch(h.Stderr()); m != nil {
			return m[1], nil
		}
		select {
		case err := <-h.waitErr:
			return "", fmt.Errorf("shim exited before binding: %v", err)
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	return "", fmt.Errorf("timed out waiting for listen log")
}

func (h *Harness) waitHealthy(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(h.URL + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("health check did not return 200 within %v", timeout)
}

// MetricsSnapshot is the parsed /v1/metrics view used for delta
// assertions across test cases sharing a shim process.
type MetricsSnapshot struct {
	Latency        map[string]LatencyStats
	Tokens         map[string]TokenStats
	Rewrites       map[string]int
	UpstreamErrors map[string]UpstreamErrorStats
	RequestsSeen   map[string]int
}

// LatencyStats mirrors measure.LatencyStats with N as the count of
// observations.
type LatencyStats struct {
	P50, P95, P99 float64
	N             int
}

// TokenStats mirrors measure.TokenStats.
type TokenStats struct {
	ShimTotal               int
	UpstreamPromptTotal     int
	UpstreamCompletionTotal int
	N                       int
}

// UpstreamErrorStats mirrors measure.UpstreamErrorStats. ByStatus keys
// are stringified status codes ("400", "502", ...).
type UpstreamErrorStats struct {
	Total    int
	Class4xx int
	Class5xx int
	ByStatus map[string]int
}

// Metrics fetches and parses /v1/metrics. Fails the test on error.
func (h *Harness) Metrics() *MetricsSnapshot {
	h.t.Helper()
	resp, err := http.Get(h.URL + "/v1/metrics")
	if err != nil {
		h.t.Fatalf("metrics get: %v", err)
	}
	defer resp.Body.Close()
	var raw struct {
		Latency map[string]struct {
			P50 float64 `json:"p50"`
			P95 float64 `json:"p95"`
			P99 float64 `json:"p99"`
			N   int     `json:"n"`
		} `json:"latency"`
		Tokens map[string]struct {
			ShimTotal               int `json:"shim_total"`
			UpstreamPromptTotal     int `json:"upstream_prompt_total"`
			UpstreamCompletionTotal int `json:"upstream_completion_total"`
			N                       int `json:"n"`
		} `json:"token_delta"`
		Rewrites       map[string]int `json:"rewrites"`
		UpstreamErrors map[string]struct {
			Total    int            `json:"total"`
			Class4xx int            `json:"class_4xx"`
			Class5xx int            `json:"class_5xx"`
			ByStatus map[string]int `json:"by_status"`
		} `json:"upstream_errors"`
		RequestsSeen map[string]int `json:"requests_seen"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		h.t.Fatalf("metrics decode: %v", err)
	}
	snap := &MetricsSnapshot{
		Latency:        make(map[string]LatencyStats, len(raw.Latency)),
		Tokens:         make(map[string]TokenStats, len(raw.Tokens)),
		Rewrites:       raw.Rewrites,
		UpstreamErrors: make(map[string]UpstreamErrorStats, len(raw.UpstreamErrors)),
		RequestsSeen:   raw.RequestsSeen,
	}
	for k, v := range raw.Latency {
		snap.Latency[k] = LatencyStats{P50: v.P50, P95: v.P95, P99: v.P99, N: v.N}
	}
	for k, v := range raw.Tokens {
		snap.Tokens[k] = TokenStats{
			ShimTotal:               v.ShimTotal,
			UpstreamPromptTotal:     v.UpstreamPromptTotal,
			UpstreamCompletionTotal: v.UpstreamCompletionTotal,
			N:                       v.N,
		}
	}
	for k, v := range raw.UpstreamErrors {
		snap.UpstreamErrors[k] = UpstreamErrorStats{
			Total:    v.Total,
			Class4xx: v.Class4xx,
			Class5xx: v.Class5xx,
			ByStatus: v.ByStatus,
		}
	}
	return snap
}

// boundedBuffer is a write-only buffer that drops oldest bytes when
// over cap. Concurrency-safe.
type boundedBuffer struct {
	mu  sync.Mutex
	buf []byte
	cap int
}

func newBoundedBuffer(cap int) *boundedBuffer {
	return &boundedBuffer{cap: cap}
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.cap {
		b.buf = b.buf[len(b.buf)-b.cap:]
	}
	return n, nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

// BuildShim compiles ./cmd/shim into a temp file. Call from TestMain.
// Returns a cleanup func plus the binary path (also stored in the
// package-level shimBinary).
func BuildShim() (string, func(), error) {
	tmp, err := os.MkdirTemp("", "shim-e2e-bin-*")
	if err != nil {
		return "", nil, err
	}
	out := filepath.Join(tmp, "shim")

	repoRoot, err := findRepoRoot()
	if err != nil {
		os.RemoveAll(tmp)
		return "", nil, err
	}

	cmd := exec.Command("go", "build", "-o", out, "./cmd/shim")
	cmd.Dir = repoRoot
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		os.RemoveAll(tmp)
		return "", nil, fmt.Errorf("build shim: %w", err)
	}
	shimBinary = out
	return out, func() { os.RemoveAll(tmp) }, nil
}

func findRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod found from %s", dir)
		}
		dir = parent
	}
}

func lastColon(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == ':' {
			return i
		}
	}
	return -1
}
