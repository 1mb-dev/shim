# Measurement reference

Full reference for shim's measurement surface. The README has the overview;
this is the per-field detail.

## `/v1/metrics` (JSON)

`GET /v1/metrics` returns a snapshot of what shim has done since startup.

```sh
curl -s http://127.0.0.1:8082/v1/metrics | python3 -m json.tool
```

```json
{
    "latency": {
        "/v1/messages": {"p50": 0.316637, "p95": 0.980266, "p99": 1.585670, "n": 14},
        "/v1/messages/count_tokens": {"p50": 0.046325, "p95": 0.054247, "p99": 0.054951, "n": 3}
    },
    "token_delta": {
        "/v1/messages": {"shim_total": 86, "upstream_prompt_total": 336, "upstream_completion_total": 168, "n": 14}
    },
    "rewrites": {"model": 14, "stop_sequences": 2},
    "requests_seen": {"/v1/messages": 14, "/v1/messages/count_tokens": 3},
    "upstream_errors": {
        "/v1/messages": {"total": 1, "class_4xx": 1, "class_5xx": 0, "by_status": {"400": 1}}
    },
    "panics_total": 0
}
```

- `latency.<path>.{p50,p95,p99}` — milliseconds, from the per-endpoint reservoir. `n` is total observations since startup (the reservoir caps at 1024 samples for percentile compute; `n` keeps counting past that).
- `token_delta.<path>.shim_total` is shim's cl100k_base BPE count of every prompt's input. `upstream_prompt_total` is what the upstream reported in `usage.prompt_tokens`. The gap is the drift: the shim-side number is reproducible under cl100k, the upstream may use a different (unpublished) tokenizer, so a wide gap means the two disagree on this traffic shape, not that one is wrong. If the upstream omits `usage`, shim skips the observation rather than recording zeros. For anthropic-passthrough the upstream *is* Anthropic, so the delta compares cl100k against Anthropic's (also unpublished) tokenizer — still a drift signal, not a verification of shim.
- `rewrites.model` counts how often shim replaced the requested model name (the translating presets rewrite every claude-* request, so this tracks `/v1/messages` `n`). `rewrites.stop_sequences` counts over-cap truncations.
- `requests_seen.<path>` counts every handler entry — the denominator for any per-request ratio. Increments before parsing or validation; counts all attempts. Only the client API endpoints (`/v1/messages`, `/v1/messages/count_tokens`) record; the probe/observability endpoints (`/health`, `/healthz`, `/readyz`, `/metrics`, `/v1/metrics`) and the `/v1/messages/explain` dry-run are excluded so probes and scrapes don't pollute the signal.
- `upstream_errors.<path>` counts non-2xx responses from the upstream. `total` is all of them; `class_4xx` + `class_5xx` bucket by HTTP class (3xx and oddities contribute to `total` and `by_status` only). `by_status` is the per-code breakdown. The companion diagnostic — the upstream body — is on the `upstream error` log line (see [Errors and debugging](#errors-and-debugging)).
- `panics_total` counts handler panics the server recovered: a panic becomes an Anthropic-shaped 500, a stack-bearing `handler panic recovered` log line, and this counter, instead of a silently dropped connection. A non-zero value is always a bug to investigate.

State is in-memory and resets on restart. The endpoint is loopback-only by default (no auth, matches `/health`). The JSON shape may change; breaking changes land in `CHANGELOG.md`.

## `/metrics` (Prometheus)

`GET /metrics` exposes the same aggregates in Prometheus text-exposition format, so shim can sit *in front of* Grafana/Prometheus rather than replace them. Hand-rolled — no `client_golang` dependency, so the binary stays a single static file.

```text
# HELP shim_rewrites_total Inbound-traffic mutations shim applied, by kind ...
# TYPE shim_rewrites_total counter
shim_rewrites_total{kind="model"} 14
shim_upstream_errors_total{endpoint="/v1/messages",status="400"} 1
shim_latency_seconds{endpoint="/v1/messages",quantile="0.95"} 0.980266
```

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `shim_requests_seen_total` | counter | `endpoint` | client requests seen |
| `shim_rewrites_total` | counter | `kind` | inbound mutations (`model`, `stop_sequences`) |
| `shim_upstream_errors_total` | counter | `endpoint`, `status` | upstream non-2xx |
| `shim_tokens_shim_total` | counter | `endpoint` | shim's cl100k prompt-token count |
| `shim_tokens_upstream_prompt_total` | counter | `endpoint` | upstream-reported prompt tokens |
| `shim_tokens_upstream_completion_total` | counter | `endpoint` | upstream-reported completion tokens |
| `shim_token_observations_total` | counter | `endpoint` | responses with usage recorded |
| `shim_latency_seconds` | gauge | `endpoint`, `quantile` | latency percentile (reservoir estimate, seconds) |
| `shim_latency_observations_total` | counter | `endpoint` | latency observations |
| `shim_panics_total` | counter | _(none)_ | handler panics recovered (emitted only when non-zero) |

Latency is a gauge with a `quantile` label, not a summary: the reservoir yields point-in-time percentiles, not histogram buckets, so a gauge is the honest representation of what shim has. Units are seconds (Prometheus base-unit convention); the JSON `/v1/metrics` reports milliseconds.

## Token counting

The `count_tokens` endpoint and the `token_delta.shim_total` field use cl100k_base — OpenAI's GPT-3.5/GPT-4 BPE tokenizer, loaded via `pkoukk/tiktoken-go` with offline-embedded tables. shim calls `EncodeOrdinary` (special tokens like `<|endoftext|>` are not processed specially), so the count is reproducible byte-for-byte for any given input.

DeepSeek (and most non-OpenAI upstreams) don't publish their tokenizer, so cl100k is an approximation across tokenizers — close enough for in-session sanity checks and the `/v1/metrics` drift signal, not a substitute for the upstream's own count when reconciling a bill.

Response `usage` (Anthropic Messages contract) comes straight from the upstream's `usage.prompt_tokens` / `usage.completion_tokens`, not from shim's cl100k count:

```json
{"usage": {"input_tokens": 123, "output_tokens": 45}}
```

## Errors and debugging

On an upstream non-2xx, shim emits a single `upstream error` log line at error level before writing the Anthropic-shaped error to the client:

```json
{
  "level": "ERROR",
  "msg": "upstream error",
  "endpoint": "/v1/messages",
  "adapter": "deepseek",
  "upstream_status": 400,
  "resolved_model": "deepseek-v4-pro",
  "body_preview": "{\"error\":{\"type\":\"context_length_exceeded\",\"message\":\"...\"}}"
}
```

The same event increments `upstream_errors[/v1/messages].by_status[400]` in `/v1/metrics`. The counter is the histogram; this line is the per-request diagnostic.

- `upstream_status` — the HTTP code the upstream returned (separate from shim's response status, the Anthropic-shaped translation).
- `resolved_model` — the model after `Adapter.MapModel`, i.e. what shim sent upstream. Joinable to the prior `model rewritten` line.
- `body_preview` — the first 1024 bytes of the upstream response body, verbatim (cap at `upstreamBodyLogBytes` in `internal/server/handlers.go`).

`body_preview` is NOT routed through shim's key-based redactor — it is operator-facing diagnostic, the only reason the field exists. Some upstreams echo a fragment of the offending request in their error body (e.g. a prompt snippet that exceeded the context window), so `body_preview` will carry it. This is the deliberate trade-off for honesty at the boundary: an opaque "upstream status 400" tells you nothing about what to fix. The upstream body is never echoed to the client, only logged. If your logs ship somewhere upstream-echoed prompt content matters, run a downstream redactor against `body_preview` at the sink.

## Thinking-block signatures

Anthropic's extended-thinking blocks carry a `signature` field for multi-turn continuity — clients pass it back unchanged, and Anthropic's API verifies it server-side (HMAC-shaped, keyed by an internal secret clients can't reproduce).

shim attaches a constant signature (`shim-passthrough-v1`) to every emitted thinking block and does not verify what clients send back:

- The loopback threat model (default bind `127.0.0.1:8082`) makes tamper-evidence unnecessary — the only caller is the same user's Claude Code.
- DeepSeek's `reasoning_content` field has no signature concept; the field is discarded on outbound translation regardless of value.
- Anthropic clients treat the signature as opaque (they can't verify locally — only the API server has the key), so any string round-trips through them.

This is deliberate, not an oversight. A reader seeing the constant string + missing verification should NOT add HMAC back as "fix the gap" — it would be verification theater for a property no caller in the deployment model requires. If shim ever runs exposed beyond loopback, revisit with a real threat model.
