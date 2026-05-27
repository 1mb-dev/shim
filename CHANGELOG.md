# Changelog

All notable changes will be documented here.
Format follows [Keep a Changelog](https://keepachangelog.com/).

## [Unreleased] — Stage 2.6 (2026-05-27)

Upstream boundary honesty. A real DeepSeek session produced ~20
consecutive `upstream status 400` responses; the log line carried no
body, no class, nothing diagnosable. `writeUpstreamError` was discarding
the upstream body into `_ []byte`. Thesis-1 violation at the boundary
that matters most. One commit closes it.

### Added
- `upstream error` log line (slog ERROR) emitted before the Anthropic-shaped client error. Fields: `endpoint`, `adapter`, `upstream_status`, `resolved_model`, `body_preview`. Joinable to the prior `model rewritten` breadcrumb via `resolved_model` without timestamp triangulation. See README "Errors and debugging".
- `const upstreamBodyLogBytes = 1024` in `internal/server/handlers.go` — caps the bytes of upstream-error body recorded on the log line. Operator-facing only; never echoed to the client.
- README "Errors and debugging" section documenting the new log line, the truncation cap, and the upstream-echo disclosure.

### Changed
- `server.(*Server).writeUpstreamError` signature gains `resolvedModel string` and the previously-discarded body parameter is now used (was `_ []byte`).
- `TestMessages_UpstreamBadRequest_400` and `TestE2E_Upstream400Becomes502` extended to pin the new log fields.

### Known gaps (deliberately deferred — see `todos/shim-stage2.6-plan.md`)
- No request IDs / response correlation header. Solo dev, timestamp-correlatable logs — adding a public response header is premature backward-compat surface.
- No client-visible upstream error-class parsing. The human reads shim logs; `body_preview` already shows the class to the only reader who matters.
- No `/v1/metrics` ring buffer of recent upstream-error bodies. Logs are debugger-grade; metrics ring buffer is operator-grade and not yet justified.
- No retry policy. Cannot be designed without knowing what's retryable; informed by Stage 2.6's captured data.

## [Unreleased] — Stage 2.5 + 2.5b (2026-05-27)

Process-boundary integration testing + measurement-honesty pass before
Stage 3 adapter #2 lands. Closes three gaps the unit suite couldn't
reach: silent shim/launcher bugs, silent upstream degradation, and the
missing live-upstream contract check that today's `deepseek-v4-pro[1m]`
400 needed to surface.

### Added
- `internal/e2e/` — `//go:build e2e` package with `Harness.Start(t)`, fake OpenAI-format upstream, and seven test cases covering happy non-stream + stream, model rewrite log + counter, upstream 400 → 502 envelope, fixed token count, measurement reflection, and `shim run` env injection. Plus a harness self-test for cleanup.
- `make e2e` Makefile target. `make test` is unchanged and does NOT run the e2e suite.
- `internal/smoke/` — `//go:build smoke` package with one opt-in live-DeepSeek round-trip. Double-gated on `SHIM_SMOKE=1` and `DEEPSEEK_SMOKE_API_KEY` (distinct from `UPSTREAM_API_KEY` for billing isolation).
- `make smoke` Makefile target. Never invoked by CI; pre-release/pre-push only. See `internal/smoke/README.md`.
- `measure.RecordRequestSeen(endpoint)` — per-handler-entry counter; the denominator for any per-endpoint ratio.
- `measure.RecordUpstreamError(endpoint, status)` — counts upstream non-2xx responses, bucketed `class_4xx` / `class_5xx` plus `by_status` drill-down. Surfaces silent upstream degradation that previously showed up only as rising latency.
- `/v1/metrics` JSON shape gains `requests_seen` and `upstream_errors` top-level keys. See README's Measurement section.
- Makefile breadcrumb chain: `make test` → tip about `make e2e`; `make e2e` → tip about `make smoke`; `make smoke` → completion line.

### Changed
- `server.Start()` now binds via `net.Listen` first, then logs the resolved addr (matters when `PORT=0` picks an ephemeral port — thesis-1, don't lie about what we did).
- DeepSeek adapter `DefaultOpusModel` from `deepseek-v4-pro[1m]` to `deepseek-v4-pro`. The `[1m]` 1M-context variant only works on DeepSeek's `/anthropic` endpoint per their create-chat-completion API reference; the OpenAI-format endpoint shim uses accepts exactly `[deepseek-v4-flash, deepseek-v4-pro]`.
- `server.(*Server).writeUpstreamError` signature gains an `endpoint` parameter so future adapters bucket their errors cleanly.

### Known gaps surfaced (deferred)
- **Tool-call JSON not counted in `shim_total`.** `extractText` skips non-text blocks; tool-heavy traffic shows wider shim-vs-upstream gaps. Needs an ADR on tokenization policy before a fix.
- **Rewrite-count vs requests-seen anomaly** observed once (8 vs 7). `requests_seen` makes the next investigation cheap — no log archaeology needed. Held pending more signal.

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
- `RecordTokenDelta` call sites moved in both `handlers.go` and `stream.go` — now fire after translation succeeds (after `OpenAIToAnthropic` / `ToAnthropicSSE`) but before the response is written to the client. Failed back-translation no longer credits `shim_total`. A subsequent write failure mid-body still records, since the protocol-level 200 was already on the wire.
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
