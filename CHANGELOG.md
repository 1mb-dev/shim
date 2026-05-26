# Changelog

All notable changes will be documented here.
Format follows [Keep a Changelog](https://keepachangelog.com/).

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
