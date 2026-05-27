package measure

import (
	"math/rand"
	"sync"
	"testing"
	"time"
)

func TestNew_EmptySnapshot(t *testing.T) {
	c := New()
	s := c.Snapshot()
	if len(s.Latency) != 0 {
		t.Errorf("expected empty latency, got %v", s.Latency)
	}
	if len(s.Tokens) != 0 {
		t.Errorf("expected empty tokens, got %v", s.Tokens)
	}
	if len(s.Rewrites) != 0 {
		t.Errorf("expected empty rewrites, got %v", s.Rewrites)
	}
	if len(s.UpstreamErrors) != 0 {
		t.Errorf("expected empty upstream_errors, got %v", s.UpstreamErrors)
	}
	if len(s.RequestsSeen) != 0 {
		t.Errorf("expected empty requests_seen, got %v", s.RequestsSeen)
	}
}

func TestRecordLatency_BelowReservoirCap(t *testing.T) {
	c := New()
	// 5 samples: 10, 20, 30, 40, 50 ms — all retained, no reservoir replacement.
	for _, ms := range []float64{10, 20, 30, 40, 50} {
		c.RecordLatency("/v1/messages", time.Duration(ms)*time.Millisecond)
	}
	s := c.Snapshot()
	stats, ok := s.Latency["/v1/messages"]
	if !ok {
		t.Fatal("endpoint not recorded")
	}
	if stats.N != 5 {
		t.Errorf("N = %d, want 5", stats.N)
	}
	// With 5 sorted samples [10,20,30,40,50], rank for p50 = 0.5*(5-1) = 2.0 → sorted[2] = 30
	if stats.P50 != 30 {
		t.Errorf("P50 = %v, want 30", stats.P50)
	}
	// p95 = 0.95*4 = 3.8 → interp between sorted[3]=40 and sorted[4]=50 → 40+10*0.8 = 48
	if stats.P95 != 48 {
		t.Errorf("P95 = %v, want 48", stats.P95)
	}
	// p99 = 0.99*4 = 3.96 → interp → 40 + 10*0.96 = 49.6
	if stats.P99 < 49.59 || stats.P99 > 49.61 {
		t.Errorf("P99 = %v, want ~49.6", stats.P99)
	}
}

func TestRecordLatency_PerEndpointIsolation(t *testing.T) {
	c := New()
	c.RecordLatency("/a", 100*time.Millisecond)
	c.RecordLatency("/b", 200*time.Millisecond)
	c.RecordLatency("/a", 100*time.Millisecond)
	s := c.Snapshot()
	if s.Latency["/a"].N != 2 {
		t.Errorf("/a N = %d, want 2", s.Latency["/a"].N)
	}
	if s.Latency["/b"].N != 1 {
		t.Errorf("/b N = %d, want 1", s.Latency["/b"].N)
	}
	if s.Latency["/b"].P50 != 200 {
		t.Errorf("/b P50 = %v, want 200", s.Latency["/b"].P50)
	}
}

func TestRecordLatency_ExceedsReservoirCap(t *testing.T) {
	c := New()
	// Force determinism: replace the rng with a fixed seed.
	c.rng = rand.New(rand.NewSource(42))
	for i := 0; i < reservoirCap*2; i++ {
		c.RecordLatency("/v1/messages", time.Duration(i)*time.Millisecond)
	}
	s := c.Snapshot()
	stats := s.Latency["/v1/messages"]
	if stats.N != reservoirCap*2 {
		t.Errorf("N = %d, want %d (count tracks all observations)", stats.N, reservoirCap*2)
	}
	// Percentiles are finite; we don't assert exact values under reservoir
	// sampling (RNG-dependent), but they should fall within the input range.
	if stats.P50 < 0 || stats.P50 >= float64(reservoirCap*2) {
		t.Errorf("P50 = %v, out of plausible range", stats.P50)
	}
	if stats.P99 < stats.P50 {
		t.Errorf("P99 (%v) < P50 (%v) — invariant violated", stats.P99, stats.P50)
	}
}

func TestRecordTokenDelta(t *testing.T) {
	c := New()
	c.RecordTokenDelta("/v1/messages", 100, 80, 50)
	c.RecordTokenDelta("/v1/messages", 50, 40, 25)
	s := c.Snapshot()
	stats, ok := s.Tokens["/v1/messages"]
	if !ok {
		t.Fatal("endpoint not recorded")
	}
	if stats.N != 2 {
		t.Errorf("N = %d, want 2", stats.N)
	}
	if stats.ShimTotal != 150 {
		t.Errorf("ShimTotal = %d, want 150", stats.ShimTotal)
	}
	if stats.UpstreamPromptTotal != 120 {
		t.Errorf("UpstreamPromptTotal = %d, want 120", stats.UpstreamPromptTotal)
	}
	if stats.UpstreamCompletionTotal != 75 {
		t.Errorf("UpstreamCompletionTotal = %d, want 75", stats.UpstreamCompletionTotal)
	}
}

// TestRecordTokenDelta_ZeroUsageNoop: an upstream response that omits
// the usage block (both prompt + completion zero) must not register an
// entry — otherwise the per-endpoint averages get diluted by a "0 vs 0"
// observation that adds no signal.
func TestRecordTokenDelta_ZeroUsageNoop(t *testing.T) {
	c := New()
	c.RecordTokenDelta("/v1/messages", 42, 0, 0)
	s := c.Snapshot()
	if _, ok := s.Tokens["/v1/messages"]; ok {
		t.Errorf("zero-usage record should not create an endpoint entry, got %+v", s.Tokens["/v1/messages"])
	}

	// Sanity: a subsequent non-zero record still works.
	c.RecordTokenDelta("/v1/messages", 10, 8, 5)
	s = c.Snapshot()
	stats, ok := s.Tokens["/v1/messages"]
	if !ok {
		t.Fatal("non-zero record did not create entry")
	}
	if stats.N != 1 || stats.ShimTotal != 10 {
		t.Errorf("got %+v, want N=1 ShimTotal=10", stats)
	}
}

// TestRecordTokenDelta_PartialUsageRecorded: if either prompt OR
// completion is non-zero, record it. The guard fires only on the
// "upstream omitted usage entirely" case.
func TestRecordTokenDelta_PartialUsageRecorded(t *testing.T) {
	c := New()
	c.RecordTokenDelta("/v1/messages", 5, 3, 0)
	c.RecordTokenDelta("/v1/messages", 5, 0, 7)
	s := c.Snapshot()
	stats, ok := s.Tokens["/v1/messages"]
	if !ok {
		t.Fatal("partial-usage record should create entry")
	}
	if stats.N != 2 {
		t.Errorf("N = %d, want 2", stats.N)
	}
}

// TestRecordRewriteEvent_ExactKeys: asserts the exact rewrite kinds shim
// surfaces. Mutation-survival: rename either constant and this test fails
// with a clear diagnostic on which name moved.
func TestRecordRewriteEvent_ExactKeys(t *testing.T) {
	c := New()
	c.RecordRewriteEvent(RewriteModel)
	c.RecordRewriteEvent(RewriteModel)
	c.RecordRewriteEvent(RewriteStopSequences)
	s := c.Snapshot()
	if got := s.Rewrites[RewriteModel]; got != 2 {
		t.Errorf("Rewrites[%q] = %d, want 2", RewriteModel, got)
	}
	if got := s.Rewrites[RewriteStopSequences]; got != 1 {
		t.Errorf("Rewrites[%q] = %d, want 1", RewriteStopSequences, got)
	}
	// String-literal pin so renaming the constant ALSO breaks this assertion.
	if RewriteModel != "model" {
		t.Errorf("RewriteModel const drifted: %q", RewriteModel)
	}
	if RewriteStopSequences != "stop_sequences" {
		t.Errorf("RewriteStopSequences const drifted: %q", RewriteStopSequences)
	}
}

// TestSnapshot_IsACopy: callers can mutate returned maps without disturbing
// the live collector — critical because the snapshot is JSON-serialized
// into a response body that may be modified by handlers downstream.
func TestSnapshot_IsACopy(t *testing.T) {
	c := New()
	c.RecordRewriteEvent(RewriteModel)
	s1 := c.Snapshot()
	s1.Rewrites[RewriteModel] = 999
	s2 := c.Snapshot()
	if s2.Rewrites[RewriteModel] != 1 {
		t.Errorf("snapshot mutation leaked back to collector: got %d, want 1", s2.Rewrites[RewriteModel])
	}
}

// TestConcurrent_RecordAndSnapshot: 50 writer goroutines + 10 reader
// goroutines hammer the collector simultaneously. Run under -race to catch
// any locking gap.
func TestConcurrent_RecordAndSnapshot(t *testing.T) {
	c := New()
	const writers = 50
	const perWriter = 200
	const readers = 10

	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < perWriter; j++ {
				c.RecordLatency("/v1/messages", time.Duration(j)*time.Microsecond)
				c.RecordTokenDelta("/v1/messages", j, j+1, j+2)
				c.RecordRewriteEvent(RewriteModel)
			}
		}(i)
	}
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = c.Snapshot()
			}
		}()
	}
	wg.Wait()

	s := c.Snapshot()
	wantTotal := writers * perWriter
	if got := s.Latency["/v1/messages"].N; got != wantTotal {
		t.Errorf("latency N = %d, want %d", got, wantTotal)
	}
	if got := s.Tokens["/v1/messages"].N; got != wantTotal {
		t.Errorf("tokens N = %d, want %d", got, wantTotal)
	}
	if got := s.Rewrites[RewriteModel]; got != wantTotal {
		t.Errorf("rewrites = %d, want %d", got, wantTotal)
	}
}

// TestRecordRequestSeen: cheap counter, per-endpoint isolation, accumulates.
func TestRecordRequestSeen(t *testing.T) {
	c := New()
	c.RecordRequestSeen("/v1/messages")
	c.RecordRequestSeen("/v1/messages")
	c.RecordRequestSeen("/health")
	s := c.Snapshot()
	if got := s.RequestsSeen["/v1/messages"]; got != 2 {
		t.Errorf("/v1/messages = %d, want 2", got)
	}
	if got := s.RequestsSeen["/health"]; got != 1 {
		t.Errorf("/health = %d, want 1", got)
	}
}

// TestRecordUpstreamError_ClassBuckets: 4xx and 5xx are bucketed; status
// codes outside those ranges (3xx, 1xx, oddities) contribute to Total +
// ByStatus only.
func TestRecordUpstreamError_ClassBuckets(t *testing.T) {
	c := New()
	c.RecordUpstreamError("/v1/messages", 400)
	c.RecordUpstreamError("/v1/messages", 429)
	c.RecordUpstreamError("/v1/messages", 502)
	c.RecordUpstreamError("/v1/messages", 502)
	c.RecordUpstreamError("/v1/messages", 301) // odd: 3xx, not bucketed
	s := c.Snapshot()
	stats, ok := s.UpstreamErrors["/v1/messages"]
	if !ok {
		t.Fatal("endpoint not recorded")
	}
	if stats.Total != 5 {
		t.Errorf("Total = %d, want 5", stats.Total)
	}
	if stats.Class4xx != 2 {
		t.Errorf("Class4xx = %d, want 2 (400+429)", stats.Class4xx)
	}
	if stats.Class5xx != 2 {
		t.Errorf("Class5xx = %d, want 2 (502+502)", stats.Class5xx)
	}
	if got := stats.ByStatus["400"]; got != 1 {
		t.Errorf("ByStatus[400] = %d, want 1", got)
	}
	if got := stats.ByStatus["429"]; got != 1 {
		t.Errorf("ByStatus[429] = %d, want 1", got)
	}
	if got := stats.ByStatus["502"]; got != 2 {
		t.Errorf("ByStatus[502] = %d, want 2", got)
	}
	if got := stats.ByStatus["301"]; got != 1 {
		t.Errorf("ByStatus[301] = %d, want 1 (3xx counted in ByStatus only)", got)
	}
}

// TestRecordUpstreamError_PerEndpoint: errors against different endpoints
// stay isolated.
func TestRecordUpstreamError_PerEndpoint(t *testing.T) {
	c := New()
	c.RecordUpstreamError("/v1/messages", 502)
	c.RecordUpstreamError("/v1/other", 400)
	s := c.Snapshot()
	if s.UpstreamErrors["/v1/messages"].Total != 1 {
		t.Errorf("/v1/messages.Total = %d, want 1", s.UpstreamErrors["/v1/messages"].Total)
	}
	if s.UpstreamErrors["/v1/other"].Total != 1 {
		t.Errorf("/v1/other.Total = %d, want 1", s.UpstreamErrors["/v1/other"].Total)
	}
	if s.UpstreamErrors["/v1/messages"].ByStatus["400"] != 0 {
		t.Errorf("cross-endpoint leak: /v1/messages saw a 400")
	}
}

// TestSnapshot_DeepCopiesByStatus: mutating a returned ByStatus map must
// not leak back to the collector. Same invariant as TestSnapshot_IsACopy
// for rewrites, applied to the nested map.
func TestSnapshot_DeepCopiesByStatus(t *testing.T) {
	c := New()
	c.RecordUpstreamError("/v1/messages", 400)
	s1 := c.Snapshot()
	s1.UpstreamErrors["/v1/messages"].ByStatus["400"] = 999
	s2 := c.Snapshot()
	if got := s2.UpstreamErrors["/v1/messages"].ByStatus["400"]; got != 1 {
		t.Errorf("ByStatus mutation leaked: got %d, want 1", got)
	}
}

func TestPercentile_Empty(t *testing.T) {
	if got := percentile(nil, 50); got != 0 {
		t.Errorf("nil = %v, want 0", got)
	}
	if got := percentile([]float64{}, 99); got != 0 {
		t.Errorf("empty = %v, want 0", got)
	}
}

func TestPercentile_Single(t *testing.T) {
	if got := percentile([]float64{42}, 50); got != 42 {
		t.Errorf("got %v, want 42", got)
	}
	if got := percentile([]float64{42}, 99); got != 42 {
		t.Errorf("got %v, want 42", got)
	}
}
