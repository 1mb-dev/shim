# shim

A Go-native proxy that lets Claude Code run against any OpenAI-compatible
model provider. Set `ANTHROPIC_BASE_URL` to point at shim, and Claude Code's
Messages-API requests get translated into OpenAI ChatCompletions and routed
to your configured upstream. Stage 0/1 ships one adapter: DeepSeek.

Single static binary. Zero runtime dependencies. Stdlib-leaning.

**Status: Stage 1 shipped, Stage 1.5 in progress.** What's listed under
"What works" is what's wired. Anything in "What doesn't" returns a clear
error rather than silently misbehaving.

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
  (p50/p95/p99), the gap between shim's token approximation and the
  upstream's claimed count, and a running tally of every request shim
  rewrote in flight. See [Measurement](#measurement).
- **Loud-fail visibility on heuristic drift.** When shim modifies your
  traffic — model name rewrite, `stop_sequences` truncation past OpenAI's
  cap of 4, etc. — it logs the event and increments a counter in
  `/v1/metrics`. Silent forwarding of modified requests is a bug.
- **Multi-provider routing (Stage 3+).** Once shim ships a second adapter
  (OpenAI proper / Groq / Ollama — selection pending), the same
  measurement layer compares behaviour across providers. The Adapter
  interface in `internal/adapter/` is the contract.

---

## What works

- `POST /v1/messages` — Anthropic Messages API. Non-streaming AND streaming (`{"stream": true}` returns the canonical Anthropic SSE event sequence: `message_start` → `content_block_start` → `content_block_delta` → `content_block_stop` → `message_delta` → `message_stop`).
- `POST /v1/messages/count_tokens` — approximate token count (see [Measurement](#measurement)).
- `GET /v1/metrics` — per-endpoint latency p50/p95/p99, shim-vs-upstream token-delta totals, rewrite-event counts. See [Measurement](#measurement).
- `GET /health` — `{"status":"ok"}`.
- Translation: system blocks, user/assistant text, image blocks (base64 + URL), `stop_sequences` (capped at 4 per OpenAI's limit; over-cap requests are truncated and a `warn` log line emitted), `tools[]`, all `tool_choice` variants, `tool_use ↔ tool_result` roundtrip.
- One adapter: **DeepSeek** (`https://api.deepseek.com/v1`, OpenAI-compatible endpoint).
- Model mapping: shim mirrors DeepSeek's [own server-side prefix rule](https://api-docs.deepseek.com/quick_start/agent_integrations/claude_code). Claude Code sends `claude-opus*`/`claude-sonnet*`/`claude-haiku*`; shim routes opus to `deepseek-v4-pro[1m]`, sonnet and haiku to `deepseek-v4-flash`. Override per role via `UPSTREAM_OPUS_MODEL` / `UPSTREAM_SONNET_MODEL` / `UPSTREAM_HAIKU_MODEL`. Non-claude-prefix names pass through unchanged unless `UPSTREAM_MODEL` is set as a catch-all. Every rewrite logs `info` and increments `rewrites.model` in `/v1/metrics`.
- `shim run [args...]` launcher: locates `claude` on PATH, injects `ANTHROPIC_BASE_URL` + `ANTHROPIC_API_KEY=shim`, execs it, propagates exit code. Tested end-to-end with `claude --bare -p`.
- Redacted-by-default JSON logs via `log/slog`. `Authorization`, prompt/message content, URL query strings, and credential-shaped keys are scrubbed at log-write time.
- Cross-compiled binaries: `darwin/arm64`, `linux/amd64`, `linux/arm64`.

## What doesn't (yet)

These all return a clear error — never silent forwarding.

- **Extended thinking.** Requests containing `{"type": "thinking", ...}` content blocks return HTTP 501 with message `extended thinking not yet supported`.
- **Prompt caching markers.** Not translated.
- **Housekeeping short-circuits** (e.g. quota probes, title generation). Forwarded to upstream as normal traffic.
- **Multiple adapters.** Only DeepSeek in Stage 0.
- **TUI / GUI / chatbot wrappers.** Not in scope.

**Streaming caveat:** Stage 0 ships a buffer-then-restream MVP — shim drives the upstream as non-streaming, then emits the canonical Anthropic SSE event sequence in one burst. Clients see the right protocol; per-token latency benefit lands when true upstream SSE pass-through ships.

## Install

```sh
go install github.com/1mb-dev/shim/cmd/shim@latest
```

Or from source:

```sh
git clone https://github.com/1mb-dev/shim
cd shim
make build              # → ./shim
make build-all          # → dist/shim-darwin-arm64, dist/shim-linux-{amd64,arm64}
```

Requires Go 1.22+.

## Config

Copy `.env.example` to `.env` and fill in `UPSTREAM_API_KEY`. All variables:

| Variable | Default | Purpose |
|---|---|---|
| `BIND_ADDR` | `127.0.0.1` | Listen address. **Do not bind 0.0.0.0** unless you accept that the proxy carries your upstream API key and has no auth of its own. |
| `PORT` | `8082` | TCP port. |
| `ADAPTER` | `deepseek` | Adapter to use. Stage 0/1 only registers `deepseek`. |
| `UPSTREAM_API_KEY` | _required_ | Bearer token sent to the upstream. |
| `UPSTREAM_BASE_URL` | `https://api.deepseek.com/v1` | Upstream root. |
| `UPSTREAM_OPUS_MODEL` | (empty → `deepseek-v4-pro[1m]`) | Override for `claude-opus*` inputs. |
| `UPSTREAM_SONNET_MODEL` | (empty → `deepseek-v4-flash`) | Override for `claude-sonnet*` inputs. |
| `UPSTREAM_HAIKU_MODEL` | (empty → `deepseek-v4-flash`) | Override for `claude-haiku*` inputs. |
| `UPSTREAM_MODEL` | (empty) | Catch-all override for non-claude-prefix names (e.g. legacy `claude-3-5-sonnet-*`, direct `deepseek-v4-pro`). Empty = pass through. |
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
Don't do it without an authenticating reverse proxy in front.

Logs scrub `Authorization`, prompt/message content, URL query strings,
and credential-shaped keys by default (`LOG_REDACT=true`). Set
`LOG_REDACT=false` only for local debugging.

## Operational limits

Hardcoded in Stage 0 (not env-configurable):

| Limit | Value | Source |
|---|---|---|
| `ReadHeaderTimeout` | 10s | `internal/server/server.go` |
| `WriteTimeout` | 70s | `internal/server/server.go` — caps streaming wall-clock |
| `IdleTimeout` | 120s | `internal/server/server.go` |
| `MaxHeaderBytes` | 1 MiB | `internal/server/server.go` |
| Upstream `Client.Timeout` | 60s | `internal/adapter/deepseek/deepseek.go` |
| Upstream `TLSHandshakeTimeout` | 10s | `internal/adapter/deepseek/deepseek.go` |
| Upstream `ResponseHeaderTimeout` | 30s | `internal/adapter/deepseek/deepseek.go` |

The 70s server `WriteTimeout` is the hard upper bound on any single
response (streaming or non-streaming). Long completions that need more
than ~60s upstream will be truncated mid-emit; the headroom over
`Client.Timeout` is thin by design.

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

## Measurement

`GET /v1/metrics` returns a JSON snapshot of what shim has done since
startup. Per-endpoint latency (p50/p95/p99 from a 1024-sample reservoir),
the gap between shim's token approximation and the upstream's claimed
count, and how often shim rewrites requests in flight.

```sh
curl -s http://127.0.0.1:8082/v1/metrics | python3 -m json.tool
```

```json
{
    "latency": {
        "/health": {
            "p50": 0.002517, "p95": 0.003018, "p99": 0.003067, "n": 5
        },
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
    }
}
```

**How to read it.**

- `latency.<path>.{p50,p95,p99}` — milliseconds, from the per-endpoint
  reservoir. `n` is total observations since startup (the reservoir caps at
  1024 samples for percentile compute; `n` keeps counting past that).
- `token_delta.<path>.shim_total` is shim's `chars/4` approximation of every
  prompt's input. `upstream_prompt_total` is what the upstream reported back
  in `usage.prompt_tokens`. The gap is the drift — wide gap means the
  heuristic is off for your traffic shape and a real tokenizer would change
  the bill estimate.
- `rewrites.model` counts how often shim replaced the requested model name
  (Stage 0's DeepSeek adapter rewrites every request, so this matches
  `/v1/messages` `n`). `rewrites.stop_sequences` counts over-cap truncations.

**Caveats.** The endpoint is loopback-only by default (no auth — matches
`/health`). State is in-memory only and resets on restart. The JSON shape
is committed for Stage 1 but unstable until v0.1.0; breaking changes will
land in `CHANGELOG.md`.

### Token approximation

The `count_tokens` endpoint and the `token_delta.shim_total` field above
use an **approximation** — `len(text) / 4` per the OpenAI tokenizer
guidance. This is sufficient for in-session sanity checks but is **not** a
substitute for a real tokenizer when calculating bills. An exact tokenizer
is planned at the measurement-stage boundary; the specific library is not
yet chosen.

Response usage shape (Anthropic Messages contract) — these values come
straight from the upstream's `usage.prompt_tokens` and
`usage.completion_tokens`, not from shim's approximation:

```json
{
  "usage": {
    "input_tokens": 123,
    "output_tokens": 45
  }
}
```

## Project layout

```
cmd/shim/             # CLI entry: shim, shim run
internal/
  config/             # zero-dep .env loader
  obslog/             # log/slog with redaction
  adapter/            # interface + registry
    deepseek/         # Stage 0 adapter
  translate/          # Anthropic ↔ OpenAI
  tokens/             # approximation
  measure/            # /v1/metrics collector (latency, token delta, rewrites)
  launcher/           # shim run
  server/             # HTTP server + handlers + error taxonomy
testdata/fixtures/    # recorded upstream responses for tests
```

Adding a provider is a new sub-package under `internal/adapter/` that
implements `adapter.Adapter` and registers itself in `init()`. Add a blank
import in `cmd/shim/main.go` and a config switch on `ADAPTER`.

## License

[MIT](LICENSE).
