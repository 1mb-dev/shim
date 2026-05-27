//go:build smoke

// Package smoke runs an opt-in live round-trip against api.deepseek.com.
// Build-tag gated AND env-var gated so it cannot accidentally execute
// during `make test`, `make e2e`, or unattended CI runs.
//
// Required env to run:
//
//	SHIM_SMOKE=1                       // hard gate; absent → t.Skip
//	DEEPSEEK_SMOKE_API_KEY=<key>       // distinct from UPSTREAM_API_KEY
//
// Optional:
//
//	SHIM_SMOKE_MODEL=<claude-…>        // default: claude-sonnet-4-6 (cheapest mapping)
//
// Cost per run: roughly $0.001 against the deepseek-v4-flash tier.
package smoke

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	gateEnv    = "SHIM_SMOKE"
	keyEnv     = "DEEPSEEK_SMOKE_API_KEY"
	modelEnv   = "SHIM_SMOKE_MODEL"
	defaultMdl = "claude-sonnet-4-6"
)

// TestSmoke_LiveDeepSeek spawns ./shim against api.deepseek.com, sends one
// minimal /v1/messages call, asserts a clean 200 + non-empty assistant text
// + /v1/metrics populated. Hard-skips when the gate envs are missing.
func TestSmoke_LiveDeepSeek(t *testing.T) {
	if os.Getenv(gateEnv) != "1" {
		t.Skipf("smoke gate closed: set %s=1 to run", gateEnv)
	}
	apiKey := os.Getenv(keyEnv)
	if apiKey == "" {
		t.Fatalf("%s required (use a distinct key from UPSTREAM_API_KEY for billing isolation)", keyEnv)
	}
	model := os.Getenv(modelEnv)
	if model == "" {
		model = defaultMdl
	}

	shim, cleanupBin, err := buildShim(t)
	if err != nil {
		t.Fatalf("build shim: %v", err)
	}
	t.Cleanup(cleanupBin)

	srv := startShim(t, shim, apiKey)
	defer srv.stop()

	// Send one minimal message.
	body, _ := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": 32,
		"messages":   []map[string]any{{"role": "user", "content": "Reply with the single word OK."}},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", srv.url+"/v1/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		srv.dump()
		t.Fatalf("POST /v1/messages: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		srv.dump()
		t.Fatalf("status=%d body=%s", resp.StatusCode, respBody)
	}

	// Response shape sanity (Anthropic, not OpenAI).
	var ar struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(respBody, &ar); err != nil {
		t.Fatalf("unmarshal response: %v\nbody: %s", err, respBody)
	}
	if ar.Type != "message" || ar.Role != "assistant" {
		t.Errorf("response not Anthropic-shaped: %+v", ar)
	}
	if len(ar.Content) == 0 || ar.Content[0].Type != "text" || strings.TrimSpace(ar.Content[0].Text) == "" {
		t.Errorf("empty or wrong content: %+v", ar.Content)
	}

	// Metrics should now reflect the call.
	mResp, err := http.Get(srv.url + "/v1/metrics")
	if err != nil {
		t.Fatalf("metrics get: %v", err)
	}
	defer mResp.Body.Close()
	var snap struct {
		Tokens map[string]struct {
			N int `json:"n"`
		} `json:"token_delta"`
		RequestsSeen   map[string]int `json:"requests_seen"`
		UpstreamErrors map[string]struct {
			Total int `json:"total"`
		} `json:"upstream_errors"`
		Rewrites map[string]int `json:"rewrites"`
	}
	if err := json.NewDecoder(mResp.Body).Decode(&snap); err != nil {
		t.Fatalf("metrics decode: %v", err)
	}
	if snap.RequestsSeen["/v1/messages"] < 1 {
		t.Errorf("requests_seen[/v1/messages] = %d, want >=1", snap.RequestsSeen["/v1/messages"])
	}
	if snap.Tokens["/v1/messages"].N < 1 {
		t.Errorf("token_delta[/v1/messages].n = %d, want >=1", snap.Tokens["/v1/messages"].N)
	}
	if got := snap.UpstreamErrors["/v1/messages"].Total; got != 0 {
		t.Errorf("upstream_errors total = %d, want 0 (clean round-trip)", got)
	}
	if snap.Rewrites["model"] < 1 {
		t.Errorf("rewrites.model = %d, want >=1 (claude-* → deepseek-*)", snap.Rewrites["model"])
	}

	t.Logf("smoke ok — model=%s reply=%q", model, strings.TrimSpace(ar.Content[0].Text))
}

// --- lifecycle helpers (self-contained; mirrors internal/e2e but no fake upstream) ---

type liveShim struct {
	url       string
	cmd       *exec.Cmd
	stderrBuf *boundedBuffer
	waitErr   chan error
	stopped   sync.Once
	cancel    context.CancelFunc
	t         *testing.T
}

func (s *liveShim) stop() {
	s.stopped.Do(func() {
		if s.cmd == nil || s.cmd.Process == nil {
			return
		}
		_ = s.cmd.Process.Signal(os.Interrupt)
		select {
		case <-s.waitErr:
		case <-time.After(5 * time.Second):
			_ = s.cmd.Process.Kill()
			<-s.waitErr
		}
		s.cancel()
	})
}

func (s *liveShim) dump() {
	s.t.Logf("=== shim stderr ===\n%s", s.stderrBuf.String())
}

var addrRe = regexp.MustCompile(`"addr":"(127\.0\.0\.1:\d+)"`)

func startShim(t *testing.T, binary, apiKey string) *liveShim {
	t.Helper()
	envFile, err := os.CreateTemp(t.TempDir(), "shim-smoke-env-*.env")
	if err != nil {
		t.Fatalf("create env file: %v", err)
	}
	envFile.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	cmd := exec.CommandContext(ctx, binary)
	cmd.Env = append(os.Environ(),
		"SHIM_ENV_FILE="+envFile.Name(),
		"BIND_ADDR=127.0.0.1",
		"PORT=0",
		"ADAPTER=deepseek",
		"UPSTREAM_BASE_URL=https://api.deepseek.com/v1",
		"UPSTREAM_API_KEY="+apiKey,
		"LOG_LEVEL=info",
		"LOG_REDACT=true",
	)
	sbuf := newBoundedBuffer(1 << 20)
	cmd.Stderr = sbuf
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start shim: %v", err)
	}
	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()

	s := &liveShim{
		cmd:       cmd,
		stderrBuf: sbuf,
		waitErr:   waitErr,
		cancel:    cancel,
		t:         t,
	}

	addr, err := discoverAddr(sbuf, waitErr, 3*time.Second)
	if err != nil {
		s.dump()
		s.stop()
		t.Fatalf("discover addr: %v", err)
	}
	s.url = "http://" + addr

	if err := waitHealthy(s.url, 3*time.Second); err != nil {
		s.dump()
		s.stop()
		t.Fatalf("health: %v", err)
	}
	return s
}

func discoverAddr(buf *boundedBuffer, waitErr <-chan error, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if m := addrRe.FindStringSubmatch(buf.String()); m != nil {
			return m[1], nil
		}
		select {
		case err := <-waitErr:
			return "", fmt.Errorf("shim exited before binding: %v", err)
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	return "", fmt.Errorf("timed out waiting for listen log")
}

func waitHealthy(url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("health did not return 200 within %v", timeout)
}

func buildShim(t *testing.T) (string, func(), error) {
	t.Helper()
	tmp, err := os.MkdirTemp("", "shim-smoke-bin-*")
	if err != nil {
		return "", nil, err
	}
	out := filepath.Join(tmp, "shim")
	root, err := findRepoRoot()
	if err != nil {
		os.RemoveAll(tmp)
		return "", nil, err
	}
	cmd := exec.Command("go", "build", "-o", out, "./cmd/shim")
	cmd.Dir = root
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		os.RemoveAll(tmp)
		return "", nil, fmt.Errorf("build shim: %w", err)
	}
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

// boundedBuffer is a write-only buffer that drops oldest bytes when over
// cap. Mirrors internal/e2e/harness.go; duplicated to keep smoke a leaf
// package with no internal deps.
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
