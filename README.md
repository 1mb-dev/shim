# shim

A Go-native proxy that lets Claude Code run against any OpenAI-compatible
model provider. Set `ANTHROPIC_BASE_URL` to point at shim, and Claude Code's
Messages-API requests get translated into OpenAI ChatCompletions and routed
to your configured upstream (Stage 0 ships one adapter: DeepSeek).

Single static binary. Zero runtime dependencies. Stdlib-leaning.

**Status: Stage 0 (in development).** What's listed under "What works" is
what's wired. Anything in "What doesn't" returns a clear error rather than
silently misbehaving.

---

## What works

- `POST /v1/messages` — Anthropic Messages API. Non-streaming AND streaming (`{"stream": true}` returns the canonical Anthropic SSE event sequence: `message_start` → `content_block_start` → `content_block_delta` → `content_block_stop` → `message_delta` → `message_stop`).
- `POST /v1/messages/count_tokens` — approximate token count (see [Measurement](#measurement)).
- `GET /health` — `{"status":"ok"}`.
- Translation: system blocks, user/assistant text, image blocks (base64 + URL), `stop_sequences` (capped at 4 per OpenAI's limit; over-cap requests are truncated and a `warn` log line emitted), `tools[]`, all `tool_choice` variants, `tool_use ↔ tool_result` roundtrip.
- One adapter: **DeepSeek** (`https://api.deepseek.com/v1`).
- Model mapping: the requested `model` value is replaced with the adapter's upstream model (Stage 0 DeepSeek = `deepseek-chat`, override via `UPSTREAM_MODEL`). When the names differ, an `info` log line records both. shim is not a model router — the client picks the adapter, the adapter picks the model.
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
| `ADAPTER` | `deepseek` | Adapter to use. Stage 0 only registers `deepseek`. |
| `UPSTREAM_API_KEY` | _required_ | Bearer token sent to the upstream. |
| `UPSTREAM_BASE_URL` | `https://api.deepseek.com/v1` | Upstream root. |
| `UPSTREAM_MODEL` | (empty) | Override default model (`deepseek-chat`). Set to `deepseek-reasoner` for R1. |
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

The Stage 0 `count_tokens` endpoint and the `usage` field on responses use
an **approximation** — `len(text) / 4` per the OpenAI tokenizer guidance.
This is sufficient for in-session sanity checks but is **not** a substitute
for a real tokenizer when calculating bills. An exact tokenizer is planned
at the measurement-stage boundary; the specific library is not yet chosen.

Response usage shape (Anthropic Messages contract):

```json
{
  "usage": {
    "input_tokens": 123,
    "output_tokens": 45
  }
}
```

Source values come from the upstream's `usage.prompt_tokens` and
`usage.completion_tokens` — they reflect whatever the upstream reports, not
a shim-side recount.

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
  launcher/           # shim run
  server/             # HTTP server + handlers + error taxonomy
testdata/fixtures/    # recorded upstream responses for tests
```

Adding a provider is a new sub-package under `internal/adapter/` that
implements `adapter.Adapter` and registers itself in `init()`. Add a blank
import in `cmd/shim/main.go` and a config switch on `ADAPTER`.

## License

[MIT](LICENSE).
