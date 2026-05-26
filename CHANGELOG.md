# Changelog

All notable changes will be documented here.
Format follows [Keep a Changelog](https://keepachangelog.com/).

## [Unreleased] — Stage 2 (2026-05-26)

Real tokenizer replaces the Stage 0 `chars/4` approximation; structural
debt deferred from Stage 1 paid before adapter #2 (Stage 3) lands on it.
Four `/code-review` MED items folded in.

### Added
- `internal/tokens.Count` and `tokens.Init` — cl100k_base BPE tokenizer via `pkoukk/tiktoken-go` with the offline loader from `pkoukk/tiktoken-go-loader`. `tokens.Init` runs from `server.New` so a broken embed blocks startup rather than panicking on first request.
- Tuned upstream `*http.Client` on the server: `MaxIdleConnsPerHost=10`, `ForceAttemptHTTP2=true`, explicit `tls.Config{MinVersion: TLS12}`, split timeouts unchanged from Stage 0.
- `deepseek.New(opts) (adapter.Adapter, error)` constructor — returns a fresh per-instance adapter; caller registers via `adapter.Register`.

### Changed
- `/v1/messages/count_tokens` and `/v1/metrics` `token_delta.shim_total` now report cl100k_base counts. Under cl100k the number is exact and reproducible; vs. DeepSeek's actual (unpublished) tokenizer it remains an approximation — `/v1/metrics` is still a drift signal, not a billing-grade count. README documents the caveat.
- `RecordTokenDelta` call sites moved to after-success in both `handlers.go` and `stream.go`; failed back-translation or SSE build no longer credits `shim_total` for a request the client never saw a 200 for.
- `RecordTokenDelta` no-ops when both `Usage.PromptTokens` and `Usage.CompletionTokens` are zero — upstream omitted the block; recording 0/0 dilutes averages without signal.

### Removed
- `internal/tokens.Approximate` — no remaining callers post-cl100k.
- `clientProvider` optional interface from `internal/server/server.go` and the `deepseek.(*impl).Client()` method that paired with it. One tuned HTTP client now lives on `Server`; per-adapter clients revisitable when Stage 3's adapter #2 forces the question.
- `Adapter.DefaultModel() string` from the `internal/adapter.Adapter` interface — vestigial post-prefix-mapping; `MapModel("")` already encoded the "caller sent no model, pick one" path inside each adapter.
- `deepseek.Configure(opts)` + the package-level `instance` singleton + `init()` registration. Use `deepseek.New(opts)` + `adapter.Register(a)` explicitly from `cmd/shim/main.go`.

### Binary size
~7 MB increase from cl100k BPE tables (offline loader embeds all four encodings; shim only uses cl100k, but the others go along for the ride). All three cross-builds: 6.5–7.0 MB → 14 MB. Still single-file static — T3 (drop-in binary) preserved.

### Runtime dependencies
First runtime deps in shim's history. Both fetched at `go build` and embedded; no network fetch or external tooling at startup:

- `github.com/pkoukk/tiktoken-go` v0.1.8
- `github.com/pkoukk/tiktoken-go-loader` v0.0.2

Transitive: `dlclark/regexp2`, `google/uuid` (compile-time, via tiktoken-go).

## [Unreleased] — Stage 1.5 (2026-05-26)

Parity pass against [DeepSeek's official Claude Code integration guide](https://api-docs.deepseek.com/quick_start/agent_integrations/claude_code) — DeepSeek now exposes a native Anthropic Messages API at `api.deepseek.com/anthropic`, with server-side claude-prefix model mapping. shim mirrors that mapping rule and clarifies when shim adds value vs. when to use the native endpoint directly.

### Added
- Prefix-aware `MapModel` in DeepSeek adapter: `claude-opus*` → `deepseek-v4-pro[1m]`; `claude-sonnet*` → `deepseek-v4-flash`; `claude-haiku*` → `deepseek-v4-flash`. Mirrors DeepSeek's own server-side rule.
- Per-role config env vars: `UPSTREAM_OPUS_MODEL`, `UPSTREAM_SONNET_MODEL`, `UPSTREAM_HAIKU_MODEL` override the role defaults independently.
- `internal/adapter/deepseek.ConfigureOpts` struct — `Configure(...)` switched from positional args so future fields don't break call sites.

### Changed
- README: new "When NOT to use shim" / "When shim adds value" sections at the top. Pure-DeepSeek users are pointed directly at `api.deepseek.com/anthropic`. shim's value-prop reframed around measurement, loud-fail visibility, and Stage 3+ multi-provider routing.
- `.env.example`: documents per-role env vars + `UPSTREAM_MODEL` catch-all semantics + the new claude-prefix rule.
- `UPSTREAM_MODEL` semantics: was a blanket override; now applies only to non-`claude-{opus,sonnet,haiku}` inputs (e.g. legacy `claude-3-5-sonnet-*`, direct `deepseek-v4-pro`).

### Known limitations
- Web Search tool (Claude Code → DeepSeek native) not yet translated through shim. Use the native endpoint if you need it.
- `ANTHROPIC_AUTH_TOKEN` (the guide's recommended env var name) is not set by `shim run`; shim's launcher still injects `ANTHROPIC_API_KEY=shim` (Claude Code accepts either; shim's loopback config doesn't care).
- Legacy `claude-3-5-sonnet-*`-style names no longer auto-rewrite to `deepseek-chat` — they fall through to the catch-all branch. Users on legacy names: set `UPSTREAM_MODEL=deepseek-chat` in `.env` or update to current model identifiers.

## [Unreleased] — Stage 1 (2026-05-26)

### Added
- `GET /v1/metrics` — in-memory snapshot of per-endpoint latency p50/p95/p99 (from a 1024-sample reservoir), shim-vs-upstream token-delta totals, and rewrite-event counts (model rewrites, `stop_sequences` truncations). Loopback-only, no auth (matches `/health`). State resets on restart.
- `Adapter.Validate() error` on the adapter interface — startup-time check; misconfigured adapter blocks `Server.New` rather than failing per-request with a 401.
- `internal/measure/` package: `Collector`, `Snapshot`, `LatencyStats`, `TokenStats`, Vitter-R reservoir sampling, mutex-guarded under `-race`.
- Tests: real `httptest.NewServer` for handler tests (exercises mux + `MaxBytesReader` + `WriteTimeout` end-to-end), `Start`/`Shutdown` lifecycle test, `writeUpstreamError` default-branch test (was uncovered), `json.Encode` / `writeSSE` failure-path tests.

### Changed
- Per-handler latency capture wraps `defer` in a closure so `time.Since(start)` evaluates at return, not at defer-statement time (the naïve form would have silently recorded ~0ms — a thesis-1 violation caught by `golangci-lint` pre-merge).
- README "Measurement" section now leads with `/v1/metrics` and grounds its JSON example in a live smoke capture.

### Removed
- `preflightAdapter` substring-match backchannel and its per-request call site — replaced by `Adapter.Validate()` at startup. Closes Linus-review HIGH (handlers.go:180-187).
- `internal/tokens.ApproximateMessages` and `CountConcat` — unused since Stage 0. Hand-rolled `errAs` in `internal/launcher` — replaced by `errors.As`.

## [Unreleased] — Stage 0 (2026-05-25)

Initial cut. Single static Go binary, zero runtime dependencies.

### Added
- `POST /v1/messages` — Anthropic Messages API, non-streaming and streaming (buffer-then-restream MVP).
- `POST /v1/messages/count_tokens` — `chars/4` approximation.
- `GET /health`.
- Translation: system blocks, user/assistant text, image content (base64 + URL), `stop_sequences`, `tools[]`, all `tool_choice` variants, `tool_use ↔ tool_result` roundtrip.
- DeepSeek upstream adapter; new providers add one file via the registry.
- Redacted JSON logs via `log/slog` (default-on; `LOG_REDACT=false` for opt-in debugging).
- Zero-dep `.env` loader; body-size cap (default 1 MiB); inbound `Authorization` header discarded (shim auths upstream itself).
- `shim run [args...]` launcher: locate `claude`, inject env vars, exec.
- Cross-compiled binaries for `darwin/arm64`, `linux/amd64`, `linux/arm64`.
- MIT license.

### Not yet (returns a clear error)
- Extended thinking blocks → HTTP 501.
- Prompt-caching markers, housekeeping short-circuits, multi-adapter shipping — see README "What doesn't (yet)".

### Known limitations (tracked for Stage 1)
- Streaming is buffer-then-restream, not true per-token SSE pass-through.
- Server uses `http.DefaultClient`; the DeepSeek adapter's tuned client is not yet routed through the Adapter interface.
- Token counting is `chars/4` approximation; exact tokenizer lands at the measurement-stage boundary.
- Model mapping collapses every requested model name to the adapter's configured upstream (logged when names differ).
- `preflightAdapter` substring-matches on the adapter's error string to detect missing config — leaky Adapter interface contract; needs `Adapter.Validate() error` in Stage 1.
- 70s server `WriteTimeout` caps any single response wall-clock (including streams); 60s upstream `Client.Timeout` leaves thin headroom for long completions.
- `stop_sequences` capped at 4 per OpenAI's contract; over-cap requests are truncated with a `warn` log line.
