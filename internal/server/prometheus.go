package server

import (
	"sort"
	"strconv"
	"strings"

	"github.com/1mb-dev/shim/internal/measure"
)

// renderPrometheus maps a measure.Snapshot to the Prometheus text exposition
// format (version 0.0.4). It is hand-rolled — no prometheus/client_golang
// dependency: the data is already aggregated in the Snapshot and the format is
// trivial, so shim stays a stdlib-leaning single static binary (thesis 3;
// v0.4 Fork 1-a).
//
// Counters end in _total. Latency percentiles are exported as a gauge with a
// `quantile` label: shim's reservoir yields point-in-time percentiles, not
// histogram buckets, so a gauge is the honest representation of what shim
// actually has (Fork 2-a) — it is deliberately NOT a Prometheus summary (we
// have no _sum). Latency is in seconds (Prometheus base-unit convention), not
// the milliseconds the JSON /v1/metrics endpoint reports.
//
// Map iteration is sorted so output is deterministic — stable across scrapes
// and golden-testable. A metric family with no series is omitted entirely.
func renderPrometheus(snap measure.Snapshot) []byte {
	var b strings.Builder

	emitIntCounter(&b, "shim_requests_seen_total",
		"Requests received per endpoint, counted at handler entry before parsing.",
		"endpoint", snap.RequestsSeen)

	emitIntCounter(&b, "shim_rewrites_total",
		"Inbound-traffic mutations shim applied, by kind (thesis 2: loud-fail on drift).",
		"kind", snap.Rewrites)

	emitUpstreamErrors(&b, snap.UpstreamErrors)
	emitTokenDeltas(&b, snap.Tokens)
	emitLatency(&b, snap.Latency)

	return []byte(b.String())
}

// emitIntCounter writes a single-label counter family from a map[label]→count.
func emitIntCounter(b *strings.Builder, name, help, label string, m map[string]int) {
	if len(m) == 0 {
		return
	}
	writeHeader(b, name, help, "counter")
	for _, k := range sortedKeys(m) {
		writeSample(b, name, [][2]string{{label, k}}, float64(m[k]), true)
	}
}

func emitUpstreamErrors(b *strings.Builder, m map[string]measure.UpstreamErrorStats) {
	if len(m) == 0 {
		return
	}
	const name = "shim_upstream_errors_total"
	writeHeader(b, name, "Upstream non-2xx responses, by endpoint and status code.", "counter")
	for _, ep := range sortedKeys(m) {
		byStatus := m[ep].ByStatus
		for _, code := range sortedKeys(byStatus) {
			writeSample(b, name,
				[][2]string{{"endpoint", ep}, {"status", code}},
				float64(byStatus[code]), true)
		}
	}
}

func emitTokenDeltas(b *strings.Builder, m map[string]measure.TokenStats) {
	if len(m) == 0 {
		return
	}
	eps := sortedKeys(m)
	emitTokenField(b, "shim_tokens_shim_total",
		"cl100k_base prompt tokens shim counted, per endpoint (vs upstream's own count).",
		eps, m, func(t measure.TokenStats) int { return t.ShimTotal })
	emitTokenField(b, "shim_tokens_upstream_prompt_total",
		"Prompt tokens the upstream reported, per endpoint.",
		eps, m, func(t measure.TokenStats) int { return t.UpstreamPromptTotal })
	emitTokenField(b, "shim_tokens_upstream_completion_total",
		"Completion tokens the upstream reported, per endpoint.",
		eps, m, func(t measure.TokenStats) int { return t.UpstreamCompletionTotal })
	emitTokenField(b, "shim_token_observations_total",
		"Responses with upstream usage recorded, per endpoint (the token-delta denominator).",
		eps, m, func(t measure.TokenStats) int { return t.N })
}

func emitTokenField(b *strings.Builder, name, help string, eps []string,
	m map[string]measure.TokenStats, get func(measure.TokenStats) int) {
	writeHeader(b, name, help, "counter")
	for _, ep := range eps {
		writeSample(b, name, [][2]string{{"endpoint", ep}}, float64(get(m[ep])), true)
	}
}

func emitLatency(b *strings.Builder, m map[string]measure.LatencyStats) {
	if len(m) == 0 {
		return
	}
	eps := sortedKeys(m)

	const gauge = "shim_latency_seconds"
	writeHeader(b, gauge,
		"Request latency percentiles per endpoint, in seconds (point-in-time reservoir estimate exported as a gauge, not a histogram).",
		"gauge")
	for _, ep := range eps {
		st := m[ep]
		writeSample(b, gauge, [][2]string{{"endpoint", ep}, {"quantile", "0.5"}}, latencySec(st.P50), false)
		writeSample(b, gauge, [][2]string{{"endpoint", ep}, {"quantile", "0.95"}}, latencySec(st.P95), false)
		writeSample(b, gauge, [][2]string{{"endpoint", ep}, {"quantile", "0.99"}}, latencySec(st.P99), false)
	}

	const count = "shim_latency_observations_total"
	writeHeader(b, count, "Latency observations recorded per endpoint.", "counter")
	for _, ep := range eps {
		writeSample(b, count, [][2]string{{"endpoint", ep}}, float64(m[ep].N), true)
	}
}

// --- exposition primitives ---

func writeHeader(b *strings.Builder, name, help, typ string) {
	b.WriteString("# HELP ")
	b.WriteString(name)
	b.WriteByte(' ')
	b.WriteString(escapeHelp(help))
	b.WriteByte('\n')
	b.WriteString("# TYPE ")
	b.WriteString(name)
	b.WriteByte(' ')
	b.WriteString(typ)
	b.WriteByte('\n')
}

func writeSample(b *strings.Builder, name string, labels [][2]string, value float64, isInt bool) {
	b.WriteString(name)
	if len(labels) > 0 {
		b.WriteByte('{')
		for i, l := range labels {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(l[0])
			b.WriteString(`="`)
			b.WriteString(escapeLabelValue(l[1]))
			b.WriteByte('"')
		}
		b.WriteByte('}')
	}
	b.WriteByte(' ')
	if isInt {
		b.WriteString(strconv.FormatInt(int64(value), 10))
	} else {
		b.WriteString(strconv.FormatFloat(value, 'g', -1, 64))
	}
	b.WriteByte('\n')
}

// sortedKeys returns the string keys of any string-keyed map, sorted — so
// exposition output is deterministic.
func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// Prometheus exposition escaping: label values escape backslash, double-quote,
// and newline; HELP text escapes backslash and newline (not quotes).
var (
	labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	helpEscaper  = strings.NewReplacer(`\`, `\\`, "\n", `\n`)
)

func escapeLabelValue(s string) string { return labelEscaper.Replace(s) }
func escapeHelp(s string) string       { return helpEscaper.Replace(s) }

// latencySec converts the milliseconds measure.LatencyStats stores into the
// seconds Prometheus expects as a base unit. Named so the conversion is explicit
// at each call site and not silently omitted on a future latency gauge.
func latencySec(ms float64) float64 { return ms / 1000 }
