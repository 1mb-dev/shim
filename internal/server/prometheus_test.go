package server

import (
	"strings"
	"testing"

	"github.com/1mb-dev/shim/internal/measure"
)

// TestRenderPrometheus pins the exact wire output for a known Snapshot. Exact
// match (not substring) because exposition is a wire format — a stray space,
// missing newline, or wrong TYPE is a real bug a Contains check would miss.
// Determinism comes from sorted map iteration in renderPrometheus.
func TestRenderPrometheus(t *testing.T) {
	snap := measure.Snapshot{
		RequestsSeen: map[string]int{"/v1/messages": 3, "/health": 1},
		Rewrites:     map[string]int{"model": 2},
		UpstreamErrors: map[string]measure.UpstreamErrorStats{
			"/v1/messages": {Total: 1, Class4xx: 1, ByStatus: map[string]int{"429": 1}},
		},
		Tokens: map[string]measure.TokenStats{
			"/v1/messages": {ShimTotal: 10, UpstreamPromptTotal: 7, UpstreamCompletionTotal: 3, N: 1},
		},
		Latency: map[string]measure.LatencyStats{
			"/v1/messages": {P50: 100, P95: 200, P99: 300, N: 3},
		},
	}

	want := `# HELP shim_requests_seen_total Requests received per endpoint, counted at handler entry before parsing.
# TYPE shim_requests_seen_total counter
shim_requests_seen_total{endpoint="/health"} 1
shim_requests_seen_total{endpoint="/v1/messages"} 3
# HELP shim_rewrites_total Inbound-traffic mutations shim applied, by kind (thesis 2: loud-fail on drift).
# TYPE shim_rewrites_total counter
shim_rewrites_total{kind="model"} 2
# HELP shim_upstream_errors_total Upstream non-2xx responses, by endpoint and status code.
# TYPE shim_upstream_errors_total counter
shim_upstream_errors_total{endpoint="/v1/messages",status="429"} 1
# HELP shim_tokens_shim_total cl100k_base prompt tokens shim counted, per endpoint (vs upstream's own count).
# TYPE shim_tokens_shim_total counter
shim_tokens_shim_total{endpoint="/v1/messages"} 10
# HELP shim_tokens_upstream_prompt_total Prompt tokens the upstream reported, per endpoint.
# TYPE shim_tokens_upstream_prompt_total counter
shim_tokens_upstream_prompt_total{endpoint="/v1/messages"} 7
# HELP shim_tokens_upstream_completion_total Completion tokens the upstream reported, per endpoint.
# TYPE shim_tokens_upstream_completion_total counter
shim_tokens_upstream_completion_total{endpoint="/v1/messages"} 3
# HELP shim_token_observations_total Responses with upstream usage recorded, per endpoint (the token-delta denominator).
# TYPE shim_token_observations_total counter
shim_token_observations_total{endpoint="/v1/messages"} 1
# HELP shim_latency_seconds Request latency percentiles per endpoint, in seconds (point-in-time reservoir estimate exported as a gauge, not a histogram).
# TYPE shim_latency_seconds gauge
shim_latency_seconds{endpoint="/v1/messages",quantile="0.5"} 0.1
shim_latency_seconds{endpoint="/v1/messages",quantile="0.95"} 0.2
shim_latency_seconds{endpoint="/v1/messages",quantile="0.99"} 0.3
# HELP shim_latency_observations_total Latency observations recorded per endpoint.
# TYPE shim_latency_observations_total counter
shim_latency_observations_total{endpoint="/v1/messages"} 3
`
	if got := string(renderPrometheus(snap)); got != want {
		t.Errorf("render mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestRenderPrometheus_Escaping: label values escape backslash, double-quote,
// and newline per the exposition spec.
func TestRenderPrometheus_Escaping(t *testing.T) {
	snap := measure.Snapshot{
		RequestsSeen: map[string]int{`weird"\` + "\npath": 1},
	}
	got := string(renderPrometheus(snap))
	wantLine := `shim_requests_seen_total{endpoint="weird\"\\\npath"} 1`
	if !strings.Contains(got, wantLine) {
		t.Errorf("escaping wrong:\ngot:  %q\nwant line: %q", got, wantLine)
	}
}

// TestRenderPrometheus_Empty: a zero-value Snapshot emits nothing (no family
// with zero series — keeps scrapes clean).
func TestRenderPrometheus_Empty(t *testing.T) {
	if got := renderPrometheus(measure.Snapshot{}); len(got) != 0 {
		t.Errorf("empty snapshot should render nothing, got %q", got)
	}
}

// TestMetricsPrometheus_Endpoint drives a real request through the stub server,
// then scrapes /metrics — exercising the handler wiring + the full record→render
// path end to end (requests_seen, the model rewrite, token delta).
func TestMetricsPrometheus_Endpoint(t *testing.T) {
	s := newStub()
	defer s.close()
	srv, _ := newTestServer(t, s)

	post := doPOST(t, srv, "/v1/messages", `{"model":"x","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)
	post.Body.Close()

	resp := doGET(t, srv, "/metrics")
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Errorf("Content-Type = %q, want Prometheus text exposition", ct)
	}
	body := bodyOf(t, resp)
	for _, want := range []string{
		`shim_requests_seen_total{endpoint="/v1/messages"}`,
		`shim_rewrites_total{kind="model"} 1`, // stub MapModel "x"→"stub-model" is a rewrite
		"# TYPE shim_latency_seconds gauge",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics missing %q\n---\n%s", want, body)
		}
	}
}
