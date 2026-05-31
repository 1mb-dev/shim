# shim

An HTTP proxy that puts the Anthropic Messages API in front of OpenAI-compatible
model providers, and measures every request it forwards. Point Claude Code at
shim via `ANTHROPIC_BASE_URL`; its Messages-API calls are translated to OpenAI
ChatCompletions and routed to your configured upstream.

shim is a measurement layer for Claude Code's upstream, not a way to get cheaper
tokens. `/v1/metrics` reports per-request latency and token drift, every request
shim rewrites in flight is logged, and `/v1/messages/explain` returns the exact
upstream body shim would send before a request leaves the box.

The OpenAI-dialect providers (**deepseek**, **openai**, **openrouter**,
**ollama**) are data rows in one preset registry: base URL, per-role model map,
auth flag. Adding another is a row, not a file. **anthropic-passthrough** is the
other transport, a transparent proxy to a native Anthropic-Messages endpoint with
no translation. Select via `ADAPTER`.

Single static binary, stdlib-leaning, one runtime dependency (`pkoukk/tiktoken-go`;
cl100k_base BPE tables embedded at compile time, no network fetch at startup).
See [Dependencies](#dependencies).

A per-adapter translator carries two transport dialects: OpenAI ChatCompletions
(the preset family) and identity (passthrough). "What works" is what's wired;
anything under "What doesn't" returns a clear error rather than silently
misbehaving.

## When NOT to use shim

If you only need DeepSeek and don't care about measurement, **skip shim
entirely.** Per [DeepSeek's official Claude Code integration
guide](https://api-docs.deepseek.com/quick_start/agent_integrations/claude_code),
DeepSeek now serves a native Anthropic Messages API at
`https://api.deepseek.com/anthropic`. Point Claude Code at it directly:

```sh
export ANTHROPIC_BASE_URL=https://api.deepseek.com/anthropic
export ANTHROPIC_AUTH_TOKEN=<your DeepSeek API key>
```

No proxy needed.

## When shim adds value

- **Honest measurement.** `GET /v1/metrics` surfaces per-endpoint latency
  (p50/p95/p99), the gap between shim's cl100k_base BPE count and the
  upstream's claimed count, and a running tally of every request shim
  rewrote in flight. See [Measurement](#measurement).
- **Loud-fail visibility on heuristic drift.** When shim modifies your
  traffic — model name rewrite, `stop_sequences` truncation past OpenAI's
  cap of 4, etc. — it logs the event and increments a counter in
  `/v1/metrics`. Silent forwarding of modified requests is a bug.
- **Transparent observability in front of real Anthropic.** The
  anthropic-passthrough adapter forwards requests and responses verbatim to a
  native Anthropic endpoint — zero translation risk — so you get shim's
  redacted logs, `/v1/metrics`, and loud-fail in front of Claude itself. See
  [Transparent passthrough](#transparent-passthrough).
- **Multi-provider routing.** Four OpenAI-dialect providers (deepseek, openai,
  openrouter, ollama) ride one translator and one measurement layer, as data
  rows in `internal/adapter/openaichat`. Adding the next is a row — base URL,
  per-role model map, auth flag — not a new file.

---

## What works

- `POST /v1/messages` — Anthropic Messages API, non-streaming and streaming. `{"stream": true}` returns the canonical Anthropic SSE sequence (`message_start` → `content_block_start` → `content_block_delta` → `content_block_stop` → `message_delta` → `message_stop`). The translating presets buffer the upstream then emit that sequence in one burst — correct protocol, no per-token latency yet; passthrough streams live. See [streaming caveat](#what-doesnt-yet).
- `POST /v1/messages/count_tokens` — cl100k_base BPE count (see [Measurement](#measurement)).
- `POST /v1/messages/explain` — dry-run: returns the upstream request shim *would* send + every mutation it would apply (model rewrite, stop-sequence cap), **without calling the upstream**. The tangible "loud-fail on drift" view; reuses the real translation path. See [Measurement](#measurement).
- `GET /v1/metrics` — JSON snapshot: per-endpoint latency p50/p95/p99, shim-vs-upstream token-delta totals, rewrite-event counts. See [Measurement](#measurement).
- `GET /metrics` — the same signals in Prometheus text-exposition format (scrapeable). See [Measurement](#measurement).
- `GET /health` (and the alias `/healthz`) — `{"status":"ok"}` (liveness); `GET /readyz` — `{"status":"ready"}` (readiness).
- Translation: system blocks, user/assistant text, image blocks (base64 + URL), `stop_sequences` (capped at 4 per OpenAI's limit; over-cap requests are truncated and a `warn` log line emitted), `tools[]`, all `tool_choice` variants, `tool_use ↔ tool_result` roundtrip.
- Thinking / `reasoning_content` roundtrip. The `thinking` request field passes through to the upstream; an upstream `reasoning_content` response becomes an Anthropic thinking block (`signature: "shim-passthrough-v1"`, constant, not verified on roundtrip), and thinking blocks echoed back on continuations translate back to `reasoning_content`. Thinking precedes tool_use in assistant turns; multiple thinking blocks concatenate into one `reasoning_content`. Live only for upstreams that surface reasoning (DeepSeek does; OpenAI hides it, so thinking blocks are a no-op there, not a bug). Rationale in [Thinking-block signatures](#thinking-block-signatures).
- Adapters: the OpenAI-dialect **preset registry** (`deepseek` `https://api.deepseek.com/v1`, `openai` `https://api.openai.com/v1`, `openrouter` `https://openrouter.ai/api/v1`, `ollama` `http://localhost:11434/v1` — all translate) and **anthropic-passthrough** (`https://api.anthropic.com`, native Anthropic Messages — forwards verbatim). Ollama runs keyless; the rest need `UPSTREAM_API_KEY`. Select via `ADAPTER`; per-preset model maps in [`.env.example`](.env.example). See [Transparent passthrough](#transparent-passthrough).
- Upstream response headers forwarded on an allowlist: `request-id`, `retry-after`, and the `anthropic-ratelimit-*` family (so clients can trace requests and back off). Content-framing and hop-by-hop headers are never forwarded — shim sets those itself.
- Model mapping: Claude Code sends `claude-opus*`/`claude-sonnet*`/`claude-haiku*`; each preset maps the three roles to its own upstream models (e.g. deepseek routes opus → `deepseek-v4-pro`, sonnet/haiku → `deepseek-v4-flash`; the full per-preset table is in [`.env.example`](.env.example)). Precedence per role: `UPSTREAM_{OPUS,SONNET,HAIKU}_MODEL` env override > preset role default > `UPSTREAM_MODEL` catch-all (only for presets with no role default, e.g. ollama) > preset default. The bare/`<role>-` hyphen anchor is deliberate — `claude-opus` and `claude-opus-4-8` both match opus, but `claude-opusxxx` must not. Non-claude names pass through unless `UPSTREAM_MODEL` is set. Every rewrite logs `info` and increments `rewrites.model` in `/v1/metrics`. Preset model IDs are verified-current (2026-05) but drift with vendor releases — override via the env vars above.
- `shim run [args...]` launcher: locates `claude` on PATH, injects `ANTHROPIC_BASE_URL` + `ANTHROPIC_API_KEY=shim`, execs it, propagates exit code. Tested end-to-end with `claude --bare -p`.
- Redacted-by-default JSON logs via `log/slog`. `Authorization`, prompt/message content, URL query strings, and credential-shaped keys are scrubbed at log-write time.
- Cross-compiled binaries: `darwin/arm64`, `linux/amd64`, `linux/arm64`.

## What doesn't (yet)

These all return a clear error — never silent forwarding.

- **`thinking: {display: "omitted"}` / `redacted_thinking` blocks.** Anthropic supports a "show me the signature but redact the content" mode for thinking blocks. shim doesn't — there's no stateless path to reproduce a signature for absent content. Defer until a real user behind the feature exists.
- **Per-token streaming for the *translating* presets.** shim drives the upstream non-streaming, then emits the canonical SSE sequence in one burst: correct protocol, no per-token latency. Per-token streaming for translated providers is future work. (Passthrough already streams live.)
- **Prompt caching markers.** Not translated (passthrough forwards them verbatim, untranslated).
- **Housekeeping short-circuits** (e.g. quota probes, title generation). Forwarded to upstream as normal traffic.
- **OpenAI Responses / "o"-series reasoning API.** The preset family speaks chat-completions only; the Responses API is a different transport dialect, out of scope.
- **TUI / GUI / chatbot wrappers.** Not in scope.

**Streaming caveat (per dialect):** the **translating** presets are buffer-then-restream — shim drives the upstream non-streaming, then emits the canonical Anthropic SSE sequence in one burst (right protocol, no per-token latency benefit yet). The **passthrough** path streams the upstream's native Anthropic SSE through live, event-by-event, byte-for-byte.

## Install

Build from source (Go 1.22+):

```sh
git clone https://github.com/1mb-dev/shim
cd shim
make build              # → ./shim
make build-all          # → dist/shim-darwin-arm64, dist/shim-linux-{amd64,arm64}
```

`go install github.com/1mb-dev/shim/cmd/shim@latest` resolves once the repo is
public. The release pipeline (GoReleaser) also produces a `FROM scratch`
container image and a Homebrew cask; both publish at the public flip.

## Dependencies

Runtime (compile-time embedded; no network fetch at startup, no
toolchain required at runtime):

- [`github.com/pkoukk/tiktoken-go`](https://github.com/pkoukk/tiktoken-go) — BPE tokenizer for cl100k_base counting on `/v1/messages/count_tokens` and `/v1/metrics` `token_delta.shim_total`.
- [`github.com/pkoukk/tiktoken-go-loader`](https://github.com/pkoukk/tiktoken-go-loader) — embeds BPE tables (cl100k + o200k + p50k + r50k) via `go:embed`. shim only uses cl100k; the other three add ~5MB of dead weight to the binary.

Both are community ports (not OpenAI-official), pre-1.0, single-maintainer.
They are compile-time embedded, so the runtime supply-chain exposure is code
vendored at build time, not fetched at startup. The token count is a
cross-tokenizer approximation/drift signal, not a billing-grade count (see
[Token counting](#token-counting)).

Binary footprint: ~14 MB per platform (darwin-arm64, linux-amd64, linux-arm64).
The embedded tokenizer accounts for ~7 MB of that; the binary is still a
single-file static drop-in.

## Config

Copy `.env.example` to `.env` and fill in `UPSTREAM_API_KEY`. All variables:

| Variable | Default | Purpose |
|---|---|---|
| `BIND_ADDR` | `127.0.0.1` | Listen address. **Do not bind 0.0.0.0** unless you accept that the proxy carries your upstream API key and has no auth of its own. |
| `PORT` | `8082` | TCP port. |
| `ADAPTER` | `deepseek` | `deepseek` / `openai` / `openrouter` / `ollama` (OpenAI-dialect, translating) or `anthropic` (transparent passthrough). Unknown values fail at startup. |
| `UPSTREAM_API_KEY` | _required (except `ollama`)_ | Credential sent upstream — `Authorization: Bearer` for the OpenAI-dialect presets, `x-api-key` for anthropic-passthrough. `ollama` runs keyless (a key is still forwarded if set). |
| `UPSTREAM_BASE_URL` | per-preset default | Upstream root. Empty → the chosen preset's default (deepseek `…/v1`, openai `…/v1`, openrouter `…/api/v1`, ollama `…:11434/v1`, anthropic `https://api.anthropic.com`). Set to point at a non-default host. |
| `UPSTREAM_OPUS_MODEL` | (preset role default) | Override for `claude-opus*` on the active OpenAI-dialect preset; passthrough forwards the model name unchanged. |
| `UPSTREAM_SONNET_MODEL` | (preset role default) | Override for `claude-sonnet*`. |
| `UPSTREAM_HAIKU_MODEL` | (preset role default) | Override for `claude-haiku*`. |
| `UPSTREAM_MODEL` | (empty) | Catch-all for non-claude names, and the role models for presets without role defaults (e.g. ollama). Empty = pass through. |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error`. |
| `LOG_REDACT` | `true` | Scrub secrets and prompt content from logs. Set `false` for local debugging only. |
| `MAX_REQUEST_BYTES` | `1048576` | Oversize body returns HTTP 413 Anthropic-shaped error. |

## Security model

shim has **no built-in authentication.** It trusts the network boundary
between itself and the client. Defaults assume one user, one machine:
`BIND_ADDR=127.0.0.1` is loopback-only, and the inbound `Authorization`
header is discarded (shim authenticates upstream with `UPSTREAM_API_KEY`
from `.env`). No inbound rate-limiting, per-route auth, or quota tracking.

If you bind to a non-loopback address, anyone on that network can route
through shim, burning your upstream quota and exposing prompt content.
Don't do it without an authenticating reverse proxy in front. shim emits a
startup `WARN` when `BIND_ADDR` is not loopback. This applies doubly to the
keyless `ollama` preset: with no upstream key gating abuse either, a wide bind
is a fully open relay to your local model. shim has no inbound auth on *any*
endpoint — `/v1/metrics`, `/health`, and the rest are open on the bind address.

Logs scrub `Authorization`, prompt/message content, URL query strings,
and credential-shaped keys by default (`LOG_REDACT=true`). Set
`LOG_REDACT=false` only for local debugging.

## Transparent passthrough

Set `ADAPTER=anthropic` to run shim as a transparent proxy in front of a native
Anthropic Messages endpoint:

```sh
ADAPTER=anthropic
UPSTREAM_BASE_URL=https://api.anthropic.com   # default; override for a compatible endpoint
UPSTREAM_API_KEY=<your Anthropic key>          # sent upstream as x-api-key
```

shim does **no translation** on this path: the request body is forwarded
byte-for-byte (so fields shim doesn't model — `metadata`, `top_k`,
`service_tier`, … — survive), the response body is returned verbatim, and
streaming is true Anthropic-SSE pass-through (event-by-event, live). shim
forwards the client's `anthropic-version` / `anthropic-beta` headers when
present and injects `2023-06-01` (logged) when absent.

The point is observability with zero translation risk: shim's redacted logs,
`/v1/metrics`, and loud-fail measurement in front of real Claude. Because there
is no translation, `/v1/metrics` `token_delta` here is purely a cl100k-vs-
Anthropic tokenizer drift signal (see [Token counting](#token-counting)).

If you want *only* a transparent Anthropic proxy with no measurement, you don't
need shim — point Claude Code at the endpoint directly. shim earns its place
when you want the measurement and loud-fail layer.

Transparency covers **errors** too: on an upstream non-2xx the
passthrough path forwards the upstream status and error body **verbatim** — the
native-Anthropic error envelope is already correctly shaped, so re-wrapping it
would only lose fidelity. This is the error-path analog of the response
pass-through and is owned by the dialect (`Translator.FromUpstreamError`), so
the request handlers stay dialect-free. The DeepSeek (translating) path still
re-classifies the status and emits shim's own Anthropic-shaped envelope on
error — there the upstream body is OpenAI-shaped and may carry prompt content,
so it must not leak; its detail stays in the `upstream error` log line. See
`docs/adr/0002-translator-seam-error-path.md`.

## Operational limits

Hardcoded (not env-configurable):

| Limit | Value | Source |
|---|---|---|
| `ReadHeaderTimeout` | 10s | `internal/server/server.go` |
| `WriteTimeout` | 200s | `internal/server/server.go` — caps streaming wall-clock |
| `IdleTimeout` | 120s | `internal/server/server.go` |
| `MaxHeaderBytes` | 1 MiB | `internal/server/server.go` |
| Upstream `Client.Timeout` | 180s | `internal/server/server.go` (`newUpstreamClient`) |
| Upstream `TLSHandshakeTimeout` | 10s | `internal/server/server.go` |
| Upstream `ResponseHeaderTimeout` | 30s | `internal/server/server.go` |

The 200s server `WriteTimeout` is the hard upper bound on any single
response (streaming or non-streaming); it's sized to outlive the 180s upstream
`Client.Timeout` so an upstream cancellation surfaces as a recordable upstream
error rather than a server-side write timeout. The 180s ceiling covers
reasoning-mode generations under the buffer-then-restream path.

## Run

Two ways:

**Manual.** Start the server, point Claude Code at it:

```sh
./shim &
export ANTHROPIC_BASE_URL=http://127.0.0.1:8082
export ANTHROPIC_API_KEY=shim   # any non-empty value works; shim auths upstream itself
claude
```

**Launcher.** `shim run` sets both vars and execs claude in one step:

```sh
./shim &
./shim run "write a hello-world go program"
```

The launcher prints a single breadcrumb line to stderr (`shim run → claude=/path/to/claude, base=http://...`) so you can see what it resolved before claude's own output starts.

`shim version` prints the build version (set at release; `dev` for a plain `go build`).

## Measurement

`GET /v1/metrics` returns a JSON snapshot of what shim has done since
startup. Per-endpoint latency (p50/p95/p99 from a 1024-sample reservoir),
the gap between shim's cl100k_base BPE count and the upstream's claimed
count, how often shim rewrites requests in flight, and counters for
total requests seen + upstream non-2xx responses.

```sh
curl -s http://127.0.0.1:8082/v1/metrics | python3 -m json.tool
```

```json
{
    "latency": {
        "/v1/messages": {
            "p50": 0.316637, "p95": 0.980266, "p99": 1.585670, "n": 14
        },
        "/v1/messages/count_tokens": {
            "p50": 0.046325, "p95": 0.054247, "p99": 0.054951, "n": 3
        }
    },
    "token_delta": {
        "/v1/messages": {
            "shim_total": 86,
            "upstream_prompt_total": 336,
            "upstream_completion_total": 168,
            "n": 14
        }
    },
    "rewrites": {
        "model": 14,
        "stop_sequences": 2
    },
    "requests_seen": {
        "/v1/messages": 14,
        "/v1/messages/count_tokens": 3
    },
    "upstream_errors": {
        "/v1/messages": {
            "total": 1,
            "class_4xx": 1,
            "class_5xx": 0,
            "by_status": {"400": 1}
        }
    },
    "panics_total": 0
}
```

**How to read it.**

- `latency.<path>.{p50,p95,p99}` — milliseconds, from the per-endpoint
  reservoir. `n` is total observations since startup (the reservoir caps at
  1024 samples for percentile compute; `n` keeps counting past that).
- `token_delta.<path>.shim_total` is shim's cl100k_base BPE count of every
  prompt's input. `upstream_prompt_total` is what the upstream reported back
  in `usage.prompt_tokens`. The gap is the drift — under cl100k the
  shim-side number is reproducible; the upstream may use a different
  tokenizer (DeepSeek's is not published), so a wide gap means the two
  tokenizers disagree on this traffic shape, not that one is wrong. If the
  upstream omits the `usage` block, shim skips the observation rather than
  recording zeros. For **anthropic-passthrough** the upstream *is* Anthropic, so
  the delta compares cl100k against Anthropic's (also unpublished) tokenizer —
  still a drift signal, not a verification of shim, since passthrough does no
  translation.
- `rewrites.model` counts how often shim replaced the requested model name
  (the translating presets rewrite every claude-* request, so this tracks
  `/v1/messages` `n`). `rewrites.stop_sequences` counts over-cap truncations.
- `requests_seen.<path>` counts every handler entry — the denominator for
  any ratio operators want to compute (errors per request, rewrites per
  request, etc.). Increments before parsing or validation; counts all
  attempts, not just successes. Only the client API endpoints
  (`/v1/messages`, `/v1/messages/count_tokens`) record: the probe/observability
  endpoints (`/health`, `/healthz`, `/readyz`, `/metrics`, `/v1/metrics`) and
  the `/v1/messages/explain` dry-run are deliberately excluded so liveness
  probes and metric scrapes don't pollute the signal.
- `upstream_errors.<path>` counts non-2xx responses from the configured
  upstream. `total` is all of them; `class_4xx` + `class_5xx` bucket by
  HTTP class (3xx and oddities contribute to `total` and `by_status` only).
  `by_status` is the per-code breakdown for drill-down. The companion
  diagnostic — the upstream body itself — is captured on the
  `upstream error` log line; see "Errors and debugging" below.
- `panics_total` counts handler panics the server recovered: instead of a
  silently dropped connection, a panic becomes an Anthropic-shaped 500, a
  stack-bearing `handler panic recovered` log line, and this counter (thesis 2).
  A non-zero value is always a bug to investigate.

**Caveats.** The endpoint is loopback-only by default (no auth, matches
`/health`). State is in-memory only and resets on restart. The JSON shape may
change; breaking changes land in `CHANGELOG.md`.

### Prometheus

`GET /metrics` exposes the same aggregates in Prometheus text-exposition
format, so shim can sit *in front of* Grafana/Prometheus rather than
replace them. Hand-rolled — no `client_golang` dependency, so the binary stays
a single static file.

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

Latency is a **gauge** with a `quantile` label, not a summary: the reservoir
yields point-in-time percentiles, not histogram buckets — a gauge is the honest
representation of what shim has. Units are seconds (Prometheus base-unit
convention); the JSON `/v1/metrics` above reports milliseconds.

### Token counting

The `count_tokens` endpoint and the `token_delta.shim_total` field above
use **cl100k_base** — OpenAI's GPT-3.5/GPT-4 BPE tokenizer, loaded via
`pkoukk/tiktoken-go` with offline-embedded tables. shim calls
`EncodeOrdinary` (special tokens like `<|endoftext|>` are not processed
specially), so the count is reproducible byte-for-byte across runs for
any given input.

DeepSeek (and most non-OpenAI upstreams) don't publish their tokenizer,
so cl100k is an **approximation across tokenizers** — close enough for
in-session sanity checks and `/v1/metrics` drift signal, **not** a
substitute for the upstream's own count when reconciling a bill.

Response usage shape (Anthropic Messages contract) — these values come
straight from the upstream's `usage.prompt_tokens` and
`usage.completion_tokens`, not from shim's cl100k count:

```json
{
  "usage": {
    "input_tokens": 123,
    "output_tokens": 45
  }
}
```

## Errors and debugging

When the configured upstream returns a non-2xx, shim emits a single
`upstream error` log line at error level before writing the
Anthropic-shaped error response to the client:

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

The same event also increments
`upstream_errors[/v1/messages].by_status[400]` in `/v1/metrics`. The
metrics counter is the histogram; this log line is the per-request
diagnostic.

**Field reference.**

- `upstream_status` — the actual HTTP code the upstream returned (separate
  from shim's response status, which is the Anthropic-shaped translation).
- `resolved_model` — the model name after `Adapter.MapModel`, i.e. what
  shim sent to the upstream. Joinable to the prior `model rewritten` log
  line without timestamp triangulation.
- `body_preview` — the first 1024 bytes of the upstream response body,
  recorded verbatim (truncated, not pretty-printed). The cap lives at
  `upstreamBodyLogBytes` in `internal/server/handlers.go`; patch the
  constant if you need a different value.

**Upstream-echo disclosure.** The `body_preview` field is NOT routed
through shim's key-based redactor. Its content is by definition
operator-facing diagnostic — that's the only reason the field exists.
Some upstreams echo a fragment of the offending request back in their
error response (e.g. a quoted snippet of the prompt that exceeded the
context window). On those upstreams, `body_preview` will carry that
fragment. This is the deliberate trade-off for thesis-1 honesty at the
boundary: an opaque "upstream status 400" tells you nothing about what
to fix. The upstream body is never echoed to the client, only logged.

If your shim deployment ships logs to a destination where upstream-echoed
prompt content is a concern, run a downstream redactor against the
`body_preview` field at the log sink. Shim does not pre-redact here
because the diagnostic value depends on the verbatim form.

### Thinking-block signatures

Anthropic's extended-thinking blocks carry a `signature` field for
multi-turn continuity — clients pass it back unchanged on continuations,
and Anthropic's API verifies it server-side (HMAC-shaped, keyed by an
internal secret that clients cannot reproduce).

shim attaches a **constant** signature (`shim-passthrough-v1`) to every
emitted thinking block and **does not verify** what clients send back.
The design intent:

- The loopback threat model (default bind `127.0.0.1:8082`) makes
  tamper-evidence unnecessary — the only caller is the same user's
  Claude Code.
- DeepSeek's `reasoning_content` field has no signature concept; the
  field is discarded on outbound translation regardless of value.
- Anthropic clients treat the signature as opaque (they cannot verify
  locally — only the API server has the key), so any string round-trips
  successfully through them.

This is a deliberate design choice, not an oversight. A future reader
looking at the constant string + missing verification should NOT add
HMAC back as "fix the gap" — it would be verification theater for a
property no caller in the deployment model requires. If shim ever runs
exposed beyond loopback, revisit then with a real threat model.

## Project layout

```
cmd/shim/             # CLI entry: shim, shim run
internal/
  config/             # zero-dep .env loader
  obslog/             # log/slog with redaction
  adapter/            # Adapter interface + InboundHeaders ctx helper
    openaichat/       # OpenAI-dialect core + preset registry (deepseek/openai/openrouter/ollama)
    anthropic/        # native-Anthropic (transparent passthrough) adapter
  translate/          # Anthropic ↔ OpenAI + per-adapter Translator seam (passthrough.go = identity)
  tokens/             # cl100k_base BPE counter
  measure/            # /v1/metrics collector (latency, token delta, rewrites)
  launcher/           # shim run
  server/             # HTTP server + handlers + error taxonomy
testdata/fixtures/    # recorded upstream responses for tests
```

Adding a provider depends on its transport dialect. An **OpenAI-dialect**
provider is a data row in `openaichat`'s preset registry — base URL, per-role
model map, auth flag, optional headers — no new file. A **genuinely new
dialect** (not OpenAI-chat, not native Anthropic) is a new sub-package under
`internal/adapter/` implementing `adapter.Adapter` (including `Translator()`
for its dialect), wired into `cmd/shim/main.go`'s `registerAdapter` —
one branch per dialect, no `init()`-time registration.

## License

[MIT](LICENSE).
