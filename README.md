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

## Quick start

```sh
brew install 1mb-dev/tap/shim         # or: go install github.com/1mb-dev/shim/cmd/shim@latest
export UPSTREAM_API_KEY=<deepseek key>  # ADAPTER=deepseek by default; see Config for others
shim &                                # serves 127.0.0.1:8082
ANTHROPIC_BASE_URL=http://127.0.0.1:8082 ANTHROPIC_API_KEY=shim claude
```

Then watch what it did: `curl -s localhost:8082/v1/metrics | python3 -m json.tool`.

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
- **Live per-token streaming on the *translating* presets.** By design, shim drives these upstreams non-streaming and emits the canonical Anthropic SSE in one burst — correct protocol and event ordering, no per-token latency. Real per-token streaming is the headline post-1.0 enhancement and lands as a non-breaking change (the SSE event shape is unchanged). Passthrough already streams live.
- **Prompt caching markers.** Not translated (passthrough forwards them verbatim, untranslated).
- **Housekeeping short-circuits** (e.g. quota probes, title generation). Forwarded to upstream as normal traffic.
- **OpenAI Responses / "o"-series reasoning API.** The preset family speaks chat-completions only; the Responses API is a different transport dialect, out of scope.
- **TUI / GUI / chatbot wrappers.** Not in scope.

**Streaming caveat (per dialect):** the **translating** presets are buffer-then-restream — shim drives the upstream non-streaming, then emits the canonical Anthropic SSE sequence in one burst (right protocol, no per-token latency benefit yet). The **passthrough** path streams the upstream's native Anthropic SSE through live, event-by-event, byte-for-byte.

## Install

```sh
brew install 1mb-dev/tap/shim
go install github.com/1mb-dev/shim/cmd/shim@latest
```

Or build from source (Go 1.25+):

```sh
git clone https://github.com/1mb-dev/shim && cd shim
make build              # → ./shim
make build-all          # → dist/shim-{darwin-arm64,linux-amd64,linux-arm64}
```

## Dependencies

Runtime (compile-time embedded; no network fetch at startup, no
toolchain required at runtime):

- [`github.com/pkoukk/tiktoken-go`](https://github.com/pkoukk/tiktoken-go) — BPE tokenizer for cl100k_base counting on `/v1/messages/count_tokens` and `/v1/metrics` `token_delta.shim_total`.
- [`github.com/pkoukk/tiktoken-go-loader`](https://github.com/pkoukk/tiktoken-go-loader) — embeds BPE tables (cl100k + o200k + p50k + r50k) via `go:embed`. shim only uses cl100k; the other three add ~5MB of dead weight to the binary.

Both are community ports (not OpenAI-official), pre-1.0, single-maintainer.
They are compile-time embedded, so the runtime supply-chain exposure is code
vendored at build time, not fetched at startup. The token count is a
cross-tokenizer approximation/drift signal, not a billing-grade count (see
[docs/measurement.md](docs/measurement.md#token-counting)).

Binary footprint: ~14 MB per platform (darwin-arm64, linux-amd64, linux-arm64).
The embedded tokenizer accounts for ~7 MB of that; the binary is still a
single-file static drop-in.

## Config

Copy `.env.example` to `.env` and fill in `UPSTREAM_API_KEY`. All variables:

| Variable | Default | Purpose |
|---|---|---|
| `BIND_ADDR` | `127.0.0.1` | Listen address. **Do not bind 0.0.0.0** unless you accept that the proxy carries your upstream API key and has no auth of its own. |
| `PORT` | `8082` | TCP port. |
| `ADAPTER` | `deepseek` | `deepseek` / `openai` / `openrouter` / `ollama` (OpenAI-dialect, translating, buffered SSE) or `anthropic` (transparent passthrough, live SSE). Unknown values fail at startup. |
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

No translation on this path: the request is forwarded byte-for-byte (so fields
shim doesn't model — `metadata`, `top_k`, … — survive), the response is returned
verbatim, streaming is live Anthropic-SSE, and upstream errors pass through with
their status and body unchanged (the native envelope is already correct). shim
forwards the client's `anthropic-version` / `anthropic-beta` and injects
`2023-06-01` (logged) when absent.

The point is observability with zero translation risk: shim's redacted logs,
`/v1/metrics`, and loud-fail in front of real Claude. `token_delta` here is a
cl100k-vs-Anthropic drift signal, not a verification, since no translation
happens. If you want a transparent Anthropic proxy *without* measurement, skip
shim and point Claude Code at the endpoint directly. Seam detail:
[ADR 0002](docs/adr/0002-translator-seam-error-path.md).

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

A few ways:

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

**Service** (Homebrew). shim can run as a managed background daemon so it's always up — no manual `./shim &`. Opt-in; install does not auto-start it:

```sh
brew services start shim
```

A service has no working directory of its own, so put config where shim looks for it. It reads config in order: `SHIM_ENV_FILE`, then `./.env`, then `~/.config/shim/.env`:

```sh
mkdir -p ~/.config/shim
printf 'ADAPTER=deepseek\nUPSTREAM_API_KEY=<your key>\n' > ~/.config/shim/.env
```

The keyless `ollama` preset needs no key — `brew services start shim` just works against a local Ollama.

`shim version` prints the build version (set at release; `dev` for a plain `go build`).

## Measurement

shim's reason to exist. `GET /v1/metrics` returns a JSON snapshot since startup:
per-endpoint latency (p50/p95/p99), the gap between shim's cl100k_base token count
and the upstream's claimed count, how often shim rewrote a request in flight, and
counts of requests seen, upstream non-2xx, and recovered panics. `GET /metrics`
serves the same aggregates in Prometheus text format.

```sh
curl -s http://127.0.0.1:8082/v1/metrics | python3 -m json.tool
```

```json
{
  "latency":         {"/v1/messages": {"p50": 0.32, "p95": 0.98, "p99": 1.59, "n": 14}},
  "token_delta":     {"/v1/messages": {"shim_total": 86, "upstream_prompt_total": 336, "n": 14}},
  "rewrites":        {"model": 14, "stop_sequences": 2},
  "upstream_errors": {"/v1/messages": {"total": 1, "by_status": {"400": 1}}},
  "panics_total": 0
}
```

State is in-memory and resets on restart; both endpoints are loopback-only by
default. The token delta is a cross-tokenizer drift signal, not a billing-grade
count. Full field reference, the Prometheus metric table, and token-counting
notes: [docs/measurement.md](docs/measurement.md).

## Errors and debugging

On an upstream non-2xx, shim logs one `upstream error` line (carrying
`upstream_status`, `resolved_model`, and a capped `body_preview` of the upstream
body) and increments `upstream_errors` in `/v1/metrics`. The client gets an
Anthropic-shaped error; the upstream body is logged, never echoed to the client.

`body_preview` is operator-facing diagnostic and is **not** redacted — some
upstreams echo a prompt fragment in their error body, so redact at your log sink
if that matters. Details: [docs/measurement.md](docs/measurement.md#errors-and-debugging).

### Thinking-block signatures

shim attaches a constant `signature` (`shim-passthrough-v1`) to emitted thinking
blocks and does not verify what clients send back: the loopback threat model makes
tamper-evidence unnecessary, and DeepSeek discards the field. Deliberate — don't
add HMAC back as "the missing fix" (rationale in
[docs/measurement.md](docs/measurement.md#thinking-block-signatures)).

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
