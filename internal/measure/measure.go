// Package measure aggregates per-request measurements in memory so shim
// can answer the question "what are you actually doing to my traffic?"
// loudly, in a single JSON payload exposed at /v1/metrics.
//
// Stage 1 measurements (Fork 2-a, 3-a in todos/shim-stage1-plan.md):
//   - Per-endpoint latency reservoir (fixed-size, percentile-at-read).
//   - Per-endpoint token-delta totals (shim chars/4 vs upstream usage.*_tokens).
//   - Rewrite-event counts (model rewrites, stop_sequences truncations, ...).
//
// All state is in-memory; restart resets to zero. Persistence is a Stage 2
// decision.
package measure

import (
	"math"
	"math/rand"
	"sort"
	"sync"
	"time"
)

// Rewrite event kinds. Adding a new kind = add a constant here + wire the
// call site + extend the rewrites table in README.
const (
	RewriteModel         = "model"
	RewriteStopSequences = "stop_sequences"
)

// reservoirCap bounds per-endpoint latency samples held in memory. 1024 is
// cheap (~8KiB/endpoint) and gives stable enough p50/p95/p99 estimates for
// the Stage 1 "is anything weird happening?" question.
const reservoirCap = 1024

// Collector aggregates measurements across goroutines. All public methods
// are safe to call concurrently; percentile compute is deferred to Snapshot.
type Collector struct {
	mu       sync.Mutex
	latency  map[string]*reservoir
	tokens   map[string]*tokenAggregate
	rewrites map[string]int
	rng      *rand.Rand
}

// New returns an empty Collector. The reservoir RNG is seeded from
// time.Now() at construction; callers wanting determinism can build their
// own and replace the rng field via tests (same-package access).
func New() *Collector {
	return &Collector{
		latency:  map[string]*reservoir{},
		tokens:   map[string]*tokenAggregate{},
		rewrites: map[string]int{},
		rng:      rand.New(rand.NewSource(time.Now().UnixNano())),
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

// RecordTokenDelta adds one (shim-approximation, upstream-claimed) pair for
// endpoint. shimApprox is the chars/4 count; upstreamPrompt and
// upstreamCompletion are the upstream usage fields. Totals + count surface
// the delta at Snapshot time.
func (c *Collector) RecordTokenDelta(endpoint string, shimApprox, upstreamPrompt, upstreamCompletion int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.tokens[endpoint]
	if t == nil {
		t = &tokenAggregate{}
		c.tokens[endpoint] = t
	}
	t.shimTotal += shimApprox
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

// Snapshot returns a point-in-time view of all aggregates with percentiles
// computed from current reservoir state. Returned maps are copies; callers
// can mutate freely.
func (c *Collector) Snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	snap := Snapshot{
		Latency:  make(map[string]LatencyStats, len(c.latency)),
		Tokens:   make(map[string]TokenStats, len(c.tokens)),
		Rewrites: make(map[string]int, len(c.rewrites)),
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
	return snap
}

// Snapshot is the JSON-marshallable view returned by Collector.Snapshot.
// The wire shape is committed in todos/shim-stage1-plan.md §1 step 6;
// breaking changes need a CHANGELOG entry.
type Snapshot struct {
	Latency  map[string]LatencyStats `json:"latency"`
	Tokens   map[string]TokenStats   `json:"token_delta"`
	Rewrites map[string]int          `json:"rewrites"`
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
// upstream_prompt_total — a wide gap signals the chars/4 approximation is
// off for the traffic shape (Stage 2 tokenizer call).
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
