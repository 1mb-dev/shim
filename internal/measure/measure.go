// Package measure aggregates per-request measurements in memory so shim
// can answer the question "what are you actually doing to my traffic?"
// loudly, in a single JSON payload exposed at /v1/metrics.
//
// Stage 1 measurements (Fork 2-a, 3-a in todos/shim-stage1-plan.md):
//   - Per-endpoint latency reservoir (fixed-size, percentile-at-read).
//   - Per-endpoint token-delta totals (shim cl100k_base count vs upstream usage.*_tokens).
//   - Rewrite-event counts (model rewrites, stop_sequences truncations, ...).
//
// All state is in-memory; restart resets to zero. Persistence is a Stage 2
// decision.
package measure

import (
	"math"
	"math/rand"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Rewrite event kinds. Adding a new kind = add a constant here + wire the
// call site + extend the rewrites table in README.
const (
	RewriteModel            = "model"
	RewriteStopSequences    = "stop_sequences"
	RewriteThinkingDisabled = "thinking_disabled"
)

// reservoirCap bounds per-endpoint latency samples held in memory. 1024 is
// cheap (~8KiB/endpoint) and gives stable enough p50/p95/p99 estimates for
// the Stage 1 "is anything weird happening?" question.
const reservoirCap = 1024

// Collector aggregates measurements across goroutines. All public methods
// are safe to call concurrently; percentile compute is deferred to Snapshot.
type Collector struct {
	mu             sync.Mutex
	latency        map[string]*reservoir
	tokens         map[string]*tokenAggregate
	rewrites       map[string]int
	upstreamErrors map[string]*upstreamErrorAgg
	requestsSeen   map[string]int
	requests       map[string]int // request-feature counters (e.g. thinking_enabled_seen)
	rng            *rand.Rand
}

// New returns an empty Collector. The reservoir RNG is seeded from
// time.Now() at construction; callers wanting determinism can build their
// own and replace the rng field via tests (same-package access).
func New() *Collector {
	return &Collector{
		latency:        map[string]*reservoir{},
		tokens:         map[string]*tokenAggregate{},
		rewrites:       map[string]int{},
		upstreamErrors: map[string]*upstreamErrorAgg{},
		requestsSeen:   map[string]int{},
		requests:       map[string]int{},
		rng:            rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// RecordLatency adds an observation for endpoint. Duration is stored as
// floating-point milliseconds.
func (c *Collector) RecordLatency(endpoint string, d time.Duration) {
	ms := float64(d.Nanoseconds()) / 1e6
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.latency[endpoint]
	if r == nil {
		r = &reservoir{samples: make([]float64, 0, reservoirCap)}
		c.latency[endpoint] = r
	}
	r.record(ms, c.rng)
}

// RecordTokenDelta adds one (shim-count, upstream-claimed) pair for
// endpoint. shimCount is the cl100k_base BPE count from tokens.Count;
// upstreamPrompt and upstreamCompletion are the upstream usage fields.
// Totals + count surface the delta at Snapshot time.
//
// No-op when BOTH upstreamPrompt and upstreamCompletion are zero —
// upstream omitted the usage block, recording zeros would dilute the
// shim/upstream comparison without adding signal. README discloses this.
func (c *Collector) RecordTokenDelta(endpoint string, shimCount, upstreamPrompt, upstreamCompletion int) {
	if upstreamPrompt == 0 && upstreamCompletion == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.tokens[endpoint]
	if t == nil {
		t = &tokenAggregate{}
		c.tokens[endpoint] = t
	}
	t.shimTotal += shimCount
	t.upstreamPromptTotal += upstreamPrompt
	t.upstreamCompletionTotal += upstreamCompletion
	t.n++
}

// RecordRewriteEvent increments the counter for kind. Use the Rewrite*
// constants; arbitrary strings are accepted but break grep-ability.
func (c *Collector) RecordRewriteEvent(kind string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rewrites[kind]++
}

// RecordRequestSeen increments the per-endpoint request counter. Called
// at handler entry, before any parsing or validation. The denominator
// for any per-endpoint ratio (errors/seen, rewrites/seen) operators want
// to compute from /v1/metrics.
func (c *Collector) RecordRequestSeen(endpoint string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requestsSeen[endpoint]++
}

// RecordThinkingEnabledSeen increments the counter for every inbound
// /v1/messages request carrying thinking={type:enabled}. Stage 2.6b
// 501s these; the counter is the empirical demand signal that decides
// whether Stage 2.6c (full reasoning_content roundtrip) fires.
func (c *Collector) RecordThinkingEnabledSeen() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests["thinking_enabled_seen"]++
}

// RecordUpstreamError records a non-2xx response from the upstream. status
// is the upstream's HTTP code (e.g., 400, 502, 429). Aggregated by class
// (4xx/5xx) and per-status for drill-down. Operators read both to answer
// "what's failing" without reading raw logs.
func (c *Collector) RecordUpstreamError(endpoint string, status int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a := c.upstreamErrors[endpoint]
	if a == nil {
		a = &upstreamErrorAgg{byStatus: map[int]int{}}
		c.upstreamErrors[endpoint] = a
	}
	a.total++
	switch {
	case status >= 400 && status < 500:
		a.class4xx++
	case status >= 500 && status < 600:
		a.class5xx++
	}
	a.byStatus[status]++
}

// Snapshot returns a point-in-time view of all aggregates with percentiles
// computed from current reservoir state. Returned maps are copies; callers
// can mutate freely.
func (c *Collector) Snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	snap := Snapshot{
		Latency:        make(map[string]LatencyStats, len(c.latency)),
		Tokens:         make(map[string]TokenStats, len(c.tokens)),
		Rewrites:       make(map[string]int, len(c.rewrites)),
		UpstreamErrors: make(map[string]UpstreamErrorStats, len(c.upstreamErrors)),
		RequestsSeen:   make(map[string]int, len(c.requestsSeen)),
		Requests:       make(map[string]int, len(c.requests)),
	}
	for ep, r := range c.latency {
		snap.Latency[ep] = LatencyStats{
			P50: percentile(r.samples, 50),
			P95: percentile(r.samples, 95),
			P99: percentile(r.samples, 99),
			N:   r.seen,
		}
	}
	for ep, t := range c.tokens {
		snap.Tokens[ep] = TokenStats{
			ShimTotal:               t.shimTotal,
			UpstreamPromptTotal:     t.upstreamPromptTotal,
			UpstreamCompletionTotal: t.upstreamCompletionTotal,
			N:                       t.n,
		}
	}
	for k, v := range c.rewrites {
		snap.Rewrites[k] = v
	}
	for ep, a := range c.upstreamErrors {
		byStatus := make(map[string]int, len(a.byStatus))
		for code, n := range a.byStatus {
			byStatus[strconv.Itoa(code)] = n
		}
		snap.UpstreamErrors[ep] = UpstreamErrorStats{
			Total:    a.total,
			Class4xx: a.class4xx,
			Class5xx: a.class5xx,
			ByStatus: byStatus,
		}
	}
	for k, v := range c.requestsSeen {
		snap.RequestsSeen[k] = v
	}
	for k, v := range c.requests {
		snap.Requests[k] = v
	}
	return snap
}

// Snapshot is the JSON-marshallable view returned by Collector.Snapshot.
// The wire shape is committed in todos/shim-stage1-plan.md §1 step 6;
// breaking changes need a CHANGELOG entry.
type Snapshot struct {
	Latency        map[string]LatencyStats       `json:"latency"`
	Tokens         map[string]TokenStats         `json:"token_delta"`
	Rewrites       map[string]int                `json:"rewrites"`
	UpstreamErrors map[string]UpstreamErrorStats `json:"upstream_errors"`
	RequestsSeen   map[string]int                `json:"requests_seen"`
	Requests       map[string]int                `json:"requests"`
}

// UpstreamErrorStats reports counts of upstream non-2xx responses per
// endpoint. ByStatus keys are stringified codes ("400", "502", ...) so the
// JSON shape is canonical; Total = Class4xx + Class5xx for status codes
// in the standard error ranges (3xx and oddities are counted only in
// Total + ByStatus).
type UpstreamErrorStats struct {
	Total    int            `json:"total"`
	Class4xx int            `json:"class_4xx"`
	Class5xx int            `json:"class_5xx"`
	ByStatus map[string]int `json:"by_status"`
}

// LatencyStats reports percentiles in milliseconds. N is total observations
// recorded for this endpoint (NOT capped at reservoir size — n past
// reservoirCap means random replacement applied).
type LatencyStats struct {
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
	P99 float64 `json:"p99"`
	N   int     `json:"n"`
}

// TokenStats reports cumulative sums. The delta is shim_total vs.
// upstream_prompt_total — a wide gap signals the cl100k count diverges
// meaningfully from the upstream's actual tokenizer for the traffic shape.
type TokenStats struct {
	ShimTotal               int `json:"shim_total"`
	UpstreamPromptTotal     int `json:"upstream_prompt_total"`
	UpstreamCompletionTotal int `json:"upstream_completion_total"`
	N                       int `json:"n"`
}

// --- internals ---

type reservoir struct {
	samples []float64
	seen    int // total observations recorded, before reservoir replacement
}

// record adds v to the reservoir using Vitter's algorithm R: while under
// capacity, append; once full, replace at index ∈ [0, seen) with
// probability cap/seen.
func (r *reservoir) record(v float64, rng *rand.Rand) {
	r.seen++
	if len(r.samples) < cap(r.samples) {
		r.samples = append(r.samples, v)
		return
	}
	idx := rng.Intn(r.seen)
	if idx < cap(r.samples) {
		r.samples[idx] = v
	}
}

type tokenAggregate struct {
	shimTotal               int
	upstreamPromptTotal     int
	upstreamCompletionTotal int
	n                       int
}

type upstreamErrorAgg struct {
	total    int
	class4xx int
	class5xx int
	byStatus map[int]int
}

// percentile returns the p-th percentile of samples (p in [0, 100]) using
// linear interpolation between sorted ranks. Returns 0 for empty input.
func percentile(samples []float64, p float64) float64 {
	if len(samples) == 0 {
		return 0
	}
	sorted := make([]float64, len(samples))
	copy(sorted, samples)
	sort.Float64s(sorted)
	rank := p / 100 * float64(len(sorted)-1)
	lo := int(math.Floor(rank))
	hi := int(math.Ceil(rank))
	if lo == hi {
		return sorted[lo]
	}
	return sorted[lo] + (sorted[hi]-sorted[lo])*(rank-float64(lo))
}
