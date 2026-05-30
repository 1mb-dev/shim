//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

var updateGolden = flag.Bool("update", false, "write golden files instead of comparing")

// perCaseBudget caps any single test case at this duration; Jordan's
// flag #3 — silently slowing tests is a Stage-2.5 regression.
const perCaseBudget = 5 * time.Second

func TestMain(m *testing.M) {
	flag.Parse()
	_, cleanup, err := BuildShim()
	if err != nil {
		fmt.Fprintln(os.Stderr, "build shim:", err)
		os.Exit(1)
	}
	code := m.Run()
	cleanup()
	os.Exit(code)
}

// withBudget runs fn under a hard time budget. Fails the test if fn
// returns later than budget, without trying to abort fn (Go tests don't
// support hard preemption).
func withBudget(t *testing.T, budget time.Duration, fn func()) {
	t.Helper()
	start := time.Now()
	fn()
	if elapsed := time.Since(start); elapsed > budget {
		t.Errorf("case exceeded budget: %v > %v", elapsed, budget)
	}
}

// postJSON POSTs a JSON body and returns (status, body).
func postJSON(t *testing.T, url string, body any) (int, []byte) {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequest("POST", url, bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, got
}

// ---- Case 1: happy non-stream ----

func TestE2E_HappyNonStream(t *testing.T) {
	withBudget(t, perCaseBudget, func() {
		h := Start(t)

		before := h.Metrics()

		status, body := postJSON(t, h.URL+"/v1/messages", map[string]any{
			"model":      "claude-sonnet-4-6",
			"max_tokens": 100,
			"messages": []map[string]any{
				{"role": "user", "content": "hello world"},
			},
		})

		if status != 200 {
			t.Fatalf("status=%d body=%s", status, body)
		}

		// Assert shim's response shape (Anthropic, not OpenAI).
		var resp struct {
			ID      string `json:"id"`
			Type    string `json:"type"`
			Role    string `json:"role"`
			Model   string `json:"model"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			StopReason string `json:"stop_reason"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatalf("unmarshal response: %v\nbody: %s", err, body)
		}
		if resp.Type != "message" {
			t.Errorf("type = %q, want message", resp.Type)
		}
		if resp.Role != "assistant" {
			t.Errorf("role = %q, want assistant", resp.Role)
		}
		if resp.Model != "claude-sonnet-4-6" {
			t.Errorf("model = %q, want original claude name preserved", resp.Model)
		}
		if len(resp.Content) != 1 || resp.Content[0].Type != "text" {
			t.Fatalf("content shape unexpected: %+v", resp.Content)
		}
		if resp.Content[0].Text != "Hello there." {
			t.Errorf("text = %q, want 'Hello there.'", resp.Content[0].Text)
		}

		// Assert what shim sent UPSTREAM (Jordan's flag #6).
		got := h.Upstream.Last()
		if got.Path != "/v1/chat/completions" {
			t.Errorf("upstream path = %q", got.Path)
		}
		if m, _ := got.Body["model"].(string); m != "deepseek-v4-flash" {
			t.Errorf("upstream model = %q, want deepseek-v4-flash (sonnet default)", m)
		}
		if s, ok := got.Body["stream"].(bool); ok && s {
			t.Errorf("upstream stream flag set on non-stream request")
		}
		// Authorization header should carry the bearer token.
		if got.Headers.Get("Authorization") != "Bearer test-key" {
			t.Errorf("Authorization = %q", got.Headers.Get("Authorization"))
		}

		// Delta-assert metrics (Alex's flag G).
		after := h.Metrics()
		if d := after.Latency["/v1/messages"].N - before.Latency["/v1/messages"].N; d != 1 {
			t.Errorf("latency.N delta = %d, want 1", d)
		}
		if d := after.Tokens["/v1/messages"].N - before.Tokens["/v1/messages"].N; d != 1 {
			t.Errorf("token_delta.N delta = %d, want 1", d)
		}
		if d := after.Rewrites["model"] - before.Rewrites["model"]; d != 1 {
			t.Errorf("rewrites.model delta = %d, want 1 (claude-sonnet-4-6 → deepseek-v4-flash)", d)
		}
		// Stage 2.5b: requests_seen denominator. handleMessages + the
		// implicit /v1/metrics call h.Metrics makes both increment;
		// assert the /v1/messages delta in isolation.
		if d := after.RequestsSeen["/v1/messages"] - before.RequestsSeen["/v1/messages"]; d != 1 {
			t.Errorf("requests_seen[/v1/messages] delta = %d, want 1", d)
		}
	})
}

// ---- Case 2: happy stream + golden file ----

func TestE2E_HappyStream(t *testing.T) {
	withBudget(t, perCaseBudget, func() {
		h := Start(t)

		status, raw := postJSON(t, h.URL+"/v1/messages", map[string]any{
			"model":      "claude-sonnet-4-6",
			"max_tokens": 100,
			"stream":     true,
			"messages": []map[string]any{
				{"role": "user", "content": "hi"},
			},
		})
		if status != 200 {
			t.Fatalf("status=%d body=%s", status, raw)
		}

		// Normalise: strip the time-varying message_start id is unnecessary
		// because the fake upstream pins it to "chatcmpl-e2e-fixed".
		// Compare bytes verbatim against the golden file.

		golden := filepath.Join("testdata", "golden_stream.txt")
		if *updateGolden {
			if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
				t.Fatalf("mkdir testdata: %v", err)
			}
			if err := os.WriteFile(golden, raw, 0o644); err != nil {
				t.Fatalf("write golden: %v", err)
			}
			t.Logf("updated golden: %s (%d bytes)", golden, len(raw))
			return
		}

		want, err := os.ReadFile(golden)
		if err != nil {
			t.Fatalf("read golden (run with -update to create): %v", err)
		}
		if !bytes.Equal(raw, want) {
			t.Errorf("stream output diverged from golden.\n--- got ---\n%s\n--- want ---\n%s", raw, want)
		}

		// Independent of byte-equality: the event sequence must be in order.
		mustContainInOrder(t, string(raw),
			"event: message_start",
			"event: content_block_start",
			"event: content_block_delta",
			"event: content_block_stop",
			"event: message_delta",
			"event: message_stop",
		)

		// Shim's streaming is buffer-then-restream (translate/stream.go:73-77):
		// upstream is called NON-stream, shim synthesizes SSE locally from the
		// completed response. Asserting the inverse documents this invariant.
		got := h.Upstream.Last()
		if s, ok := got.Body["stream"].(bool); ok && s {
			t.Errorf("upstream should NOT see stream=true (shim buffers); got: %+v", got.Body)
		}
	})
}

// ---- Case 3: model rewrite is loud (log + metric) ----

func TestE2E_ModelRewriteLoud(t *testing.T) {
	withBudget(t, perCaseBudget, func() {
		h := Start(t)

		before := h.Metrics()

		status, body := postJSON(t, h.URL+"/v1/messages", map[string]any{
			"model":      "claude-opus-4-7",
			"max_tokens": 50,
			"messages":   []map[string]any{{"role": "user", "content": "hi"}},
		})
		if status != 200 {
			t.Fatalf("status=%d body=%s", status, body)
		}

		// Metric incremented.
		after := h.Metrics()
		if d := after.Rewrites["model"] - before.Rewrites["model"]; d != 1 {
			t.Errorf("rewrites.model delta = %d, want 1", d)
		}

		// Log line emitted to shim's stderr.
		stderr := h.Stderr()
		if !strings.Contains(stderr, `"msg":"model rewritten"`) {
			t.Errorf("stderr missing model-rewritten log line")
		}
		if !strings.Contains(stderr, `"requested":"claude-opus-4-7"`) {
			t.Errorf("stderr missing requested model in log")
		}
		if !strings.Contains(stderr, `"resolved":"deepseek-v4-pro"`) {
			t.Errorf("stderr missing resolved model in log")
		}
	})
}

// ---- Case 4: upstream 400 → shim 502 with Anthropic-shaped body ----
// This is the regression this whole stage exists to catch.

func TestE2E_Upstream400Becomes502(t *testing.T) {
	withBudget(t, perCaseBudget, func() {
		h := Start(t)

		h.Upstream.SetNext(CannedResponse{
			Status: 400,
			Body: map[string]any{
				"error": map[string]any{
					"message": "Invalid model: deepseek-v4-pro[1m] is not available for this account",
					"type":    "invalid_request_error",
				},
			},
		})

		status, body := postJSON(t, h.URL+"/v1/messages", map[string]any{
			"model":      "claude-opus-4-7",
			"max_tokens": 10,
			"messages":   []map[string]any{{"role": "user", "content": "hi"}},
		})

		// Assertion A: status code.
		if status != 502 {
			t.Errorf("status = %d, want 502", status)
		}

		// Assertion B: body matches Anthropic error envelope.
		var errResp struct {
			Type  string `json:"type"`
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(body, &errResp); err != nil {
			t.Fatalf("body not Anthropic-shaped: %v\nbody: %s", err, body)
		}
		if errResp.Type != "error" || errResp.Error.Type != "api_error" {
			t.Errorf("error envelope wrong: %+v", errResp)
		}
		if !strings.Contains(errResp.Error.Message, "status 400") {
			t.Errorf("error message should cite upstream 400, got: %q", errResp.Error.Message)
		}

		// Assertion C: the upstream-error path emits a SINGLE diagnostic line
		// (v0.3.1 — the dialect renders the client error via FromUpstreamError;
		// the server no longer double-logs a separate "request failed" line).
		// That line carries the client-facing status alongside the upstream
		// status (asserted structurally in C2). The "upstream status 400"
		// client-message detail is verified in the response body at B.
		stderr := h.Stderr()
		if !strings.Contains(stderr, `"status":502`) {
			t.Errorf("stderr missing client status=502 on the upstream-error line")
		}
		if strings.Contains(stderr, `"msg":"request failed"`) {
			t.Errorf("upstream-error path should not emit a separate request-failed line")
		}

		// Assertion C2: Stage 2.6 — the new `upstream error` log line MUST
		// surface the upstream body so an operator can diagnose without
		// re-instrumenting. body_preview, upstream_status, resolved_model are
		// the three fields whose absence was the entire reason for 2.6.
		if !strings.Contains(stderr, `"msg":"upstream error"`) {
			t.Errorf("stderr missing upstream-error log line")
		}
		if !strings.Contains(stderr, `"upstream_status":400`) {
			t.Errorf("stderr missing upstream_status=400")
		}
		if !strings.Contains(stderr, `"resolved_model":"deepseek-v4-pro"`) {
			t.Errorf("stderr missing resolved_model=deepseek-v4-pro")
		}
		if !strings.Contains(stderr, `"body_preview":`) ||
			!strings.Contains(stderr, "deepseek-v4-pro[1m] is not available") {
			t.Errorf("stderr missing body_preview carrying upstream error body")
		}

		// Assertion D: upstream_errors counter incremented for status 400.
		// Stage 2.5b: previously a t.Log gap; now a hard assertion.
		after := h.Metrics()
		stats := after.UpstreamErrors["/v1/messages"]
		if stats.Total < 1 {
			t.Errorf("upstream_errors total = %d, want >=1", stats.Total)
		}
		if stats.Class4xx < 1 {
			t.Errorf("upstream_errors class_4xx = %d, want >=1", stats.Class4xx)
		}
		if got := stats.ByStatus["400"]; got < 1 {
			t.Errorf("upstream_errors.by_status[400] = %d, want >=1", got)
		}
	})
}

// ---- Case 5: count_tokens with pinned fixture ----

func TestE2E_CountTokensFixed(t *testing.T) {
	withBudget(t, perCaseBudget, func() {
		h := Start(t)

		// "hello world" under cl100k_base BPE → 2 tokens. Pinning this
		// makes any tiktoken-go regression immediately visible (Jordan #7).
		status, body := postJSON(t, h.URL+"/v1/messages/count_tokens", map[string]any{
			"model": "claude-sonnet-4-6",
			"messages": []map[string]any{
				{"role": "user", "content": "hello world"},
			},
		})
		if status != 200 {
			t.Fatalf("status=%d body=%s", status, body)
		}
		var resp struct {
			InputTokens int `json:"input_tokens"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if resp.InputTokens != 2 {
			t.Errorf("input_tokens = %d, want 2 (cl100k_base 'hello world')", resp.InputTokens)
		}
	})
}

// ---- Case 6: measurement reflects a real call (delta only) ----

func TestE2E_MeasurementReflectsCall(t *testing.T) {
	withBudget(t, perCaseBudget, func() {
		h := Start(t)
		before := h.Metrics()

		postJSON(t, h.URL+"/v1/messages", map[string]any{
			"model":      "",
			"max_tokens": 50,
			"messages":   []map[string]any{{"role": "user", "content": "hello world"}},
		})

		after := h.Metrics()
		dN := after.Tokens["/v1/messages"].N - before.Tokens["/v1/messages"].N
		if dN != 1 {
			t.Fatalf("token_delta N delta = %d, want 1", dN)
		}
		dShim := after.Tokens["/v1/messages"].ShimTotal - before.Tokens["/v1/messages"].ShimTotal
		dUp := after.Tokens["/v1/messages"].UpstreamPromptTotal - before.Tokens["/v1/messages"].UpstreamPromptTotal
		if dShim <= 0 {
			t.Errorf("shim_total delta = %d, want >0", dShim)
		}
		if dUp != 7 {
			t.Errorf("upstream_prompt_total delta = %d, want 7 (fake-canned)", dUp)
		}
	})
}

// ---- Case 7: `shim run` injects env that reaches the running shim ----

func TestE2E_LauncherEnvInjection(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix-only stub")
	}
	withBudget(t, perCaseBudget, func() {
		h := Start(t)

		// Stub claude: writes its env to a file the test reads.
		stubDir := t.TempDir()
		outPath := filepath.Join(stubDir, "out.txt")
		stubPath := filepath.Join(stubDir, "claude")
		stubBody := fmt.Sprintf(`#!/bin/sh
set -e
{
  echo "BASE_URL=$ANTHROPIC_BASE_URL"
  echo "API_KEY=$ANTHROPIC_API_KEY"
} > "%s"
# Prove env-injected URL reaches a real shim by curling /health through it.
if command -v curl >/dev/null 2>&1; then
  curl -fsS "$ANTHROPIC_BASE_URL/health" >> "%s"
fi
exit 0
`, outPath, outPath)
		if err := os.WriteFile(stubPath, []byte(stubBody), 0o755); err != nil {
			t.Fatalf("write stub: %v", err)
		}

		// Force the launcher to look up our stub instead of the real claude
		// by prefixing PATH with our temp dir + invoking with bin="claude".
		envFile := filepath.Join(stubDir, ".env")
		if err := os.WriteFile(envFile, []byte{}, 0o644); err != nil {
			t.Fatalf("write env: %v", err)
		}

		cmd := exec.Command(shimBinary, "run", "--bare", "hi")
		cmd.Env = append(os.Environ(),
			"PATH="+stubDir+":"+os.Getenv("PATH"),
			"SHIM_ENV_FILE="+envFile,
			"BIND_ADDR=127.0.0.1",
			"PORT="+h.Port,
			"ADAPTER=deepseek",
			"UPSTREAM_BASE_URL="+h.Upstream.URL,
			"UPSTREAM_API_KEY=test-key",
			"LOG_LEVEL=info",
			"LOG_REDACT=true",
		)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("shim run: %v\nstderr: %s", err, stderr.String())
		}

		out, err := os.ReadFile(outPath)
		if err != nil {
			t.Fatalf("read stub out: %v", err)
		}
		got := string(out)
		wantBase := "BASE_URL=" + h.URL
		if !strings.Contains(got, wantBase) {
			t.Errorf("stub did not see expected BASE_URL.\ngot:\n%s\nwant substring: %s", got, wantBase)
		}
		if !strings.Contains(got, "API_KEY=shim") {
			t.Errorf("stub did not see ANTHROPIC_API_KEY=shim:\n%s", got)
		}
		// If curl was present, /health body should be in the file too.
		if strings.Contains(got, "status") && !strings.Contains(got, "ok") {
			t.Errorf("/health did not return ok via launcher-injected URL:\n%s", got)
		}
	})
}

// ---- Harness self-test: cleanup on early failure ----
// Jordan flag #4. If a test fatals inside Start (e.g. port discovery
// fails), the spawned shim must not be orphaned.

func TestHarness_CleanupOnEarlyFailure(t *testing.T) {
	withBudget(t, perCaseBudget, func() {
		var capturedPID int
		t.Run("inner", func(inner *testing.T) {
			h := Start(inner)
			capturedPID = h.cmd.Process.Pid
			// Force cleanup to run by ending the sub-test scope.
		})

		// After inner closes, t.Cleanup ran. Verify the PID is gone.
		if pidAlive(capturedPID) {
			t.Errorf("shim PID %d still alive after inner test cleanup", capturedPID)
		}
	})
}

// ---- helpers ----

func pidAlive(pid int) bool {
	if pid == 0 {
		return false
	}
	// Per-OS liveness check. os.FindProcess on Unix never errors, so we
	// can't rely on it; check the process table directly.
	switch runtime.GOOS {
	case "linux":
		_, err := os.Stat(fmt.Sprintf("/proc/%d", pid))
		return err == nil
	default:
		out, err := exec.Command("ps", "-p", fmt.Sprintf("%d", pid)).Output()
		if err != nil {
			return false
		}
		return regexp.MustCompile(fmt.Sprintf(`(?m)^\s*%d\s`, pid)).Match(out)
	}
}

// ---- Stage 2.6c regression: reasoning_content ↔ thinking-block roundtrip ----
// Replaces the 2.6b TestE2E_ToolContinuation_NoLongerTriggers400. That
// test's 2.6b invariant ("shim injects thinking=disabled to dodge the
// contract") died when 2.6c removed the inject. The 2.6c invariant: shim
// translates DeepSeek's reasoning_content into Anthropic thinking blocks
// on the response, then back to reasoning_content on continuation —
// satisfying DeepSeek's "reasoning_content required on tool continuations
// in thinking mode" contract via roundtrip, not via bypass.

func TestE2E_ReasoningRoundtrip_3Turn(t *testing.T) {
	withBudget(t, perCaseBudget, func() {
		h := Start(t)
		h.Upstream.EnforceToolContinuationContract(true)

		// Turn 1: stub emits a tool_call WITH reasoning_content (DeepSeek
		// v4-pro shape — reasoning model that uses tools also emits its
		// reasoning).
		h.Upstream.SetNext(CannedResponse{
			Status: 200,
			Body: map[string]any{
				"id":      "chatcmpl-tool1",
				"object":  "chat.completion",
				"created": 1700000000,
				"model":   "deepseek-v4-pro",
				"choices": []map[string]any{{
					"index": 0,
					"message": map[string]any{
						"role":              "assistant",
						"content":           nil,
						"reasoning_content": "the user wants weather; i should call get_weather",
						"tool_calls": []map[string]any{{
							"id":   "call_1",
							"type": "function",
							"function": map[string]any{
								"name":      "get_weather",
								"arguments": `{"city":"SF"}`,
							},
						}},
					},
					"finish_reason": "tool_calls",
				}},
				"usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 5, "total_tokens": 10},
			},
		})

		// Turn 1 request — opt-in to thinking.
		status, body := postJSON(t, h.URL+"/v1/messages", map[string]any{
			"model":      "claude-opus-4-7",
			"max_tokens": 50,
			"thinking":   map[string]any{"type": "enabled"},
			"tools": []map[string]any{{
				"name":         "get_weather",
				"description":  "Returns weather",
				"input_schema": map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}},
			}},
			"messages": []map[string]any{
				{"role": "user", "content": "what's the weather in SF?"},
			},
		})
		if status != 200 {
			t.Fatalf("turn 1 status=%d body=%s", status, body)
		}

		// Parse turn 1 response — must include thinking block (with constant
		// signature) and tool_use block, in that order.
		var turn1Resp struct {
			Content []struct {
				Type      string `json:"type"`
				Thinking  string `json:"thinking,omitempty"`
				Signature string `json:"signature,omitempty"`
				ID        string `json:"id,omitempty"`
				Name      string `json:"name,omitempty"`
			} `json:"content"`
		}
		if err := json.Unmarshal(body, &turn1Resp); err != nil {
			t.Fatalf("turn 1 unmarshal: %v", err)
		}
		if len(turn1Resp.Content) < 2 {
			t.Fatalf("turn 1 expected >=2 blocks, got %d: %+v", len(turn1Resp.Content), turn1Resp.Content)
		}
		if turn1Resp.Content[0].Type != "thinking" {
			t.Errorf("turn 1 block[0].type = %q, want thinking (ordering invariant)", turn1Resp.Content[0].Type)
		}
		if turn1Resp.Content[0].Signature != "shim-passthrough-v1" {
			t.Errorf("turn 1 thinking signature = %q, want constant sig", turn1Resp.Content[0].Signature)
		}
		if !strings.Contains(turn1Resp.Content[0].Thinking, "should call get_weather") {
			t.Errorf("turn 1 thinking text missing: %q", turn1Resp.Content[0].Thinking)
		}

		before := h.Metrics()

		// Turn 2: send the assistant's thinking + tool_use back along with a
		// tool_result. This is the request that pre-2.6c (with stub contract
		// enforcement) would 400. Post-2.6c: shim translates the thinking
		// block to reasoning_content; stub sees it on the prior assistant
		// turn, contract satisfied, returns 200.
		status, body = postJSON(t, h.URL+"/v1/messages", map[string]any{
			"model":      "claude-opus-4-7",
			"max_tokens": 50,
			"thinking":   map[string]any{"type": "enabled"},
			"tools": []map[string]any{{
				"name":         "get_weather",
				"description":  "Returns weather",
				"input_schema": map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}},
			}},
			"messages": []map[string]any{
				{"role": "user", "content": "what's the weather in SF?"},
				{"role": "assistant", "content": []map[string]any{
					{"type": "thinking", "thinking": "the user wants weather; i should call get_weather", "signature": "shim-passthrough-v1"},
					{"type": "tool_use", "id": "call_1", "name": "get_weather", "input": map[string]any{"city": "SF"}},
				}},
				{"role": "user", "content": []map[string]any{{
					"type":        "tool_result",
					"tool_use_id": "call_1",
					"content":     "sunny, 22C",
				}}},
			},
		})
		if status != 200 {
			t.Fatalf("turn 2 status=%d body=%s — reasoning roundtrip should satisfy stub's contract", status, body)
		}

		after := h.Metrics()
		dErr := 0
		if a, ok := after.UpstreamErrors["/v1/messages"]; ok {
			dErr += a.Total
		}
		if b, ok := before.UpstreamErrors["/v1/messages"]; ok {
			dErr -= b.Total
		}
		if dErr != 0 {
			t.Errorf("upstream_errors total delta = %d, want 0 (reasoning roundtrip should prevent stub 400)", dErr)
		}
	})
}

// TestE2E_ThinkingMissing_StubEnforces400 — defensive fence proving the
// stub's tool-continuation contract actually fires when reasoning_content
// is missing. Without this, a translator bug that silently drops the
// thinking block would still pass TestE2E_ReasoningRoundtrip_3Turn (the
// 200 would arrive from the stub's default path, not from contract
// satisfaction). This test sends the broken-pre-2.6c request shape
// (assistant turn has tool_calls but no thinking history) and asserts
// the stub 400s — confirms the regression fence is actually fencing.

func TestE2E_ThinkingMissing_StubEnforces400(t *testing.T) {
	withBudget(t, perCaseBudget, func() {
		h := Start(t)
		h.Upstream.EnforceToolContinuationContract(true)

		// Single-turn request that has tool_calls in assistant history but
		// no thinking block. With thinking enabled (default contract path),
		// stub's proxy should 400.
		status, body := postJSON(t, h.URL+"/v1/messages", map[string]any{
			"model":      "claude-opus-4-7",
			"max_tokens": 50,
			"thinking":   map[string]any{"type": "enabled"},
			"tools": []map[string]any{{
				"name":         "get_weather",
				"description":  "Returns weather",
				"input_schema": map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}},
			}},
			"messages": []map[string]any{
				{"role": "user", "content": "what's the weather?"},
				{"role": "assistant", "content": []map[string]any{
					{"type": "tool_use", "id": "call_1", "name": "get_weather", "input": map[string]any{"city": "SF"}},
				}},
				{"role": "user", "content": []map[string]any{{
					"type":        "tool_result",
					"tool_use_id": "call_1",
					"content":     "sunny",
				}}},
			},
		})
		if status != 502 {
			t.Errorf("status = %d, want 502 (stub should 400, shim translates to 502)", status)
		}
		// Stage 2.6 design: shim does not echo upstream error bodies to
		// the client (potential prompt content). The diagnostic detail
		// lives in shim's stderr `upstream error` log line's body_preview
		// field — verify there.
		_ = body
		stderr := h.Stderr()
		if !strings.Contains(stderr, "reasoning_content") {
			t.Errorf("stderr's upstream error body_preview should surface the contract reason: %s", stderr)
		}
	})
}

// ---- Prometheus /metrics scrape (v0.4 P1) ----
// The honest-measurement thesis made scrapeable: after a real call, /metrics
// exposes the same signals as /v1/metrics JSON in Prometheus text format.

func TestE2E_PrometheusMetrics(t *testing.T) {
	withBudget(t, perCaseBudget, func() {
		h := Start(t)

		// One real call populates requests_seen, the model rewrite, token delta.
		status, body := postJSON(t, h.URL+"/v1/messages", map[string]any{
			"model":      "claude-sonnet-4-6",
			"max_tokens": 50,
			"messages":   []map[string]any{{"role": "user", "content": "hi"}},
		})
		if status != 200 {
			t.Fatalf("setup call status=%d body=%s", status, body)
		}

		resp, err := http.Get(h.URL + "/metrics")
		if err != nil {
			t.Fatalf("scrape /metrics: %v", err)
		}
		defer resp.Body.Close()
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
			t.Errorf("Content-Type = %q, want Prometheus text exposition", ct)
		}
		raw, _ := io.ReadAll(resp.Body)
		text := string(raw)
		for _, want := range []string{
			`shim_requests_seen_total{endpoint="/v1/messages"}`,
			`shim_rewrites_total{kind="model"}`,
			`shim_tokens_upstream_prompt_total{endpoint="/v1/messages"} 7`, // fake-canned usage
			"# TYPE shim_latency_seconds gauge",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("/metrics missing %q\n---\n%s", want, text)
			}
		}
	})
}

func mustContainInOrder(t *testing.T, haystack string, needles ...string) {
	t.Helper()
	idx := 0
	for _, n := range needles {
		found := strings.Index(haystack[idx:], n)
		if found < 0 {
			t.Errorf("expected substring %q not found after position %d", n, idx)
			return
		}
		idx += found + len(n)
	}
}
