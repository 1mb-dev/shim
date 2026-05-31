# Changelog

All notable changes will be documented here.
Format follows [Keep a Changelog](https://keepachangelog.com/).

## [0.5.0] — OpenAI-dialect preset registry (2026-05-31)

The "3rd adapter" reframed: OpenAI, OpenRouter and Ollama all speak the OpenAI
dialect `deepseek` already implemented, so the adapter collapsed into one
`internal/adapter/openaichat` core with providers as **data rows** (base URL +
per-role model map + auth flag + optional headers). Adding an OpenAI-dialect
provider is now a row, not a file. The translator seam is untouched (still
4 methods, two axes) — this is the provider-quirk axis. See
`docs/adr/0001-translator-seam-two-axis.md`.

### Added
- `internal/adapter/openaichat` — the OpenAI-ChatCompletions core; `deepseek` becomes a preset row, joined by `openai` (`https://api.openai.com/v1`), `openrouter` (`https://openrouter.ai/api/v1`), and `ollama` (`http://localhost:11434/v1`). `New(name, Config)` resolves the row and merges env config; unknown name fails loudly listing valid presets.
- Keyless `ollama` preset (`authRequired:false`) — starts and runs with no `UPSTREAM_API_KEY`; an optional key is still forwarded if set (proxy setups).
- `make smoke-ollama` — free, offline live round-trip (build tag `smoke` + `SHIM_OLLAMA_SMOKE=1`; skips if Ollama unreachable at `localhost:11434`).

### Changed
- The auth gate moved from `config.Load` (which globally required `UPSTREAM_API_KEY`) to `Adapter.Validate`, run once at startup — so `authRequired:false` presets start keyless. Per-role model precedence: `UPSTREAM_*_MODEL` env override > preset role default > `UPSTREAM_MODEL` catch-all > preset default.
- `UPSTREAM_BASE_URL` has **no global default**; each preset/adapter supplies its own. This also fixed a latent misroute live since v0.3 — the anthropic passthrough was silently defaulting to the deepseek endpoint when `UPSTREAM_BASE_URL` was unset.
- `registerAdapter` branches one case per transport dialect (`anthropic` → `anthropic.New`; default → `openaichat.New`), not one per provider.
- Preset model IDs verified-current (2026-05): deepseek `v4-pro`/`v4-flash`, openai `gpt-5.5`/`gpt-5.4-mini`, openrouter `claude-{opus-4.8,sonnet-4.6,haiku-4.5}`, ollama `llama3.3`. They drift with vendor releases — override via `UPSTREAM_*_MODEL`.

## [0.4.0] — Honest measurement made real + installable (2026-05-30)

Makes the honest-measurement thesis machine-consumable and the binary actually
installable. No new adapters (a 3rd is a fast-follow) — adapters are fixtures,
honesty is the product.

### Added
- `GET /metrics` — Prometheus text exposition (v0.0.4) of the same aggregates as the JSON `/v1/metrics`: `shim_requests_seen_total`, `shim_rewrites_total`, `shim_upstream_errors_total`, `shim_tokens_*`, and `shim_latency_seconds` (a gauge with a `quantile` label — reservoir percentiles, not a histogram; seconds). Hand-rolled, zero new dependencies (stays a single static binary).
- `GET /healthz` (alias of `/health`) + `GET /readyz` — conventional liveness/readiness probe paths.
- `POST /v1/messages/explain` — dry-run returning the upstream request shim *would* send + every mutation it would apply (model rewrite, stop-sequence cap), **without calling the upstream**. `transport` (passthrough|translated) is byte-derived, so it reflects what would actually hit the wire.
- Release pipeline: `.goreleaser.yaml` (per-arch archives + a `FROM scratch` multi-arch image — binary + CA certs, nonroot — to GHCR + a Homebrew cask to `1mb-dev/tap`) and `.github/workflows/release.yml` (a validate job runs test/lint/snapshot on every push/PR; the publish job is double-gated on a `v*` tag **and** `vars.PUBLISH_ENABLED` — inert until the public flip).

### Changed
- Measurement records **only client API traffic** (`/v1/messages`, `/v1/messages/count_tokens`). Probe/observability endpoints (`/health`, `/healthz`, `/readyz`, `/metrics`, `/v1/metrics`) and the `/explain` dry-run no longer self-record, so liveness probes and metric scrapes can't dominate `requests_seen` or pollute the latency reservoirs. **Shape change:** those paths no longer appear in `/v1/metrics` `requests_seen`/`latency`.

## [0.3.1] — Passthrough error transparency + e2e (2026-05-30)

Completes the passthrough's transparency story on the error path and proves the
whole passthrough flow through the real binary. See
`docs/adr/0002-translator-seam-error-path.md`.

### Added
- `Translator.FromUpstreamError(upstreamStatus, upstreamBody) → (clientStatus, clientBody)` — the error-path analog of `FromUpstream`. anthropic-passthrough forwards the upstream status + error body **verbatim** (native-Anthropic errors are already correctly shaped); DeepSeek re-classifies (401/403→401, 429→429, 4xx/5xx→502) and emits shim's own Anthropic envelope (the OpenAI error body must not leak). Handlers stay dialect-free.
- Full-process passthrough e2e: parameterized harness (`HarnessOpts.Adapter`/`UpstreamURL`) + an `AnthropicFakeUpstream`. Covers verbatim non-stream (fields shim doesn't model survive), byte-identical SSE, and error transparency (400/429/500/529 pass through, not re-mapped).
- `translate.AnthropicErrorJSON` + `translate.ErrType*` — the Anthropic error envelope + type taxonomy, now the single source of truth (the server aliases only what it raises directly).

### Changed
- The upstream-error path emits a single `upstream error` log line carrying both `upstream_status` and the client-facing `status` (was: a second `request failed` line). A 2xx response that fails to read is now a dialect-independent 502, not routed through the dialect error render.
- `adapter.ReadNormalizedResponse` — shared `NormalizeResponse` body for deepseek + anthropic (they were byte-identical bar the error prefix).

### Docs
- ADR 0002 — extends the Translator seam to the error path (same transport-dialect axis as ADR 0001). README passthrough limitation note updated: error transparency now ships.

## [0.3.0] — Translator seam + anthropic-passthrough (2026-05-29)

The "v1 shape": a pluggable per-adapter translator carries two transport
dialects, and a transparent passthrough adapter delivers shim's observability
in front of a native Anthropic endpoint with zero translation risk. See
`docs/adr/0001-translator-seam-two-axis.md`.

### Added
- `translate.Translator` interface + `Adapter.Translator()` — relocates dialect knowledge out of the request handlers (which hard-wired `AnthropicToOpenAI` since Stage 0) into the adapter. Handlers are now dialect-free. DeepSeek returns `AnthropicOpenAI()`.
- `internal/adapter/anthropic` — transparent passthrough to a native Anthropic Messages endpoint (`ADAPTER=anthropic`). Identity `MapModel`; `x-api-key` auth; forwards client `anthropic-version`/`anthropic-beta` (injects `2023-06-01` + logs when absent). Request and non-streaming response forwarded **byte-for-byte** (fields shim doesn't model survive).
- True Anthropic-SSE byte-passthrough for the passthrough path (`translate.Identity().StreamChunks`): scans the upstream stream per-event and forwards each verbatim, live, while sniffing usage from `message_start`/`message_delta`. Not buffer-then-restream.
- Upstream response-header forwarding on an allowlist (`request-id`, `retry-after`, `anthropic-ratelimit-*`); hop-by-hop/content-framing headers never forwarded.
- `adapter.WithInboundHeaders`/`InboundHeaders` — context helper so passthrough can forward selected client headers without a `BuildRequest` signature change.

### Changed
- `ADAPTER` selects `deepseek` or `anthropic`; unknown values fail at startup (no silent fallback). `UPSTREAM_BASE_URL` defaults per adapter.
- OpenAI `stop_sequences` cap moved from the dialect-agnostic handler into the OpenAI translator (passthrough is no longer capped); the server still emits the loud-fail metric/log via the translator's `stopCapped` report.
- Non-streaming response path writes the translator's response **bytes** (was: re-encode a parsed struct) — lossless for passthrough.

### Docs
- README: passthrough adapter + "when to use", response-header forwarding, per-dialect streaming caveat, token-delta tokenizer-drift note. Corrected stale operational-limit timeouts (`Client.Timeout` 60→180s, `WriteTimeout` 70→200s) and the `init()`-registration description (adapters register explicitly in `main.go`).
- ADR 0001 — the Translator seam + transport-dialect/provider-quirk two-axis model.

## [0.0.2] — Stage 2.6c (2026-05-27)

Full reasoning_content ↔ thinking-block roundtrip. Stage 2.6b's
empirical reproduction proved DeepSeek v4-pro ignores
`thinking: {type: "disabled"}` — the L1 control plane could never fix
the 400 loop because the upstream doesn't honor the disable. Maya's
huddle framing wins retroactively: this work was correctness, not a
feature; L2 was the only path that closes the contract.

### Added
- `OpenAIMessage.ReasoningContent` field — captures DeepSeek's thinking-mode reasoning text on inbound responses; carries shim's outbound reconstruction on continuations (round-trips the contract).
- `AnthropicBlock.Thinking` + `AnthropicBlock.Signature` fields — only meaningful for `type: "thinking"` blocks per Anthropic's spec.
- `translate.thinkingSignature = "shim-passthrough-v1"` constant — shim doesn't verify on roundtrip (clients pass opaquely; DeepSeek discards). See README "Thinking-block signatures" for the no-verification posture rationale.
- Bidirectional translation: response-side `reasoning_content` → Anthropic thinking block (with constant sig); request-side thinking block → `reasoning_content` (sig discarded).
- Block ordering on response side: thinking → text → tool_use, per Anthropic spec.
- Multiple thinking blocks in one assistant turn concatenate (newline-separated) into one reasoning_content on outbound.
- `extractText` now counts thinking-block content for `shim_total` (keeps measurement honest when clients echo thinking back).
- 5 new translate-pkg unit tests + 1 handler test + 2 e2e cases: `TestE2E_ReasoningRoundtrip_3Turn` (positive case) + `TestE2E_ThinkingMissing_StubEnforces400` (negative case fences the stub contract).
- README "Thinking-block signatures" subsection — documents the no-verification posture explicitly so future readers don't add HMAC back as "the missing fix."

### Changed
- `handleMessages` no longer 501s on assistant-side thinking content blocks (`containsThinkingBlock` removed) — they translate to reasoning_content now.
- `handleMessages` no longer 501s on request-level `thinking: {type: "enabled"}` (2.6b guard removed) — forwarded identity, reasoning roundtrips.
- `AnthropicToOpenAI` no longer injects `{type: "disabled"}` when client omits thinking — DeepSeek ignored it anyway. Pass nil through.
- `FakeUpstream.violatesToolContinuationContract` proxy rule updated to match DeepSeek's real contract: thinking active + tool_calls in history + no reasoning_content on prior assistant turn = 400.

### Removed (breaking changes to `/v1/metrics` JSON shape)
- `requests` top-level field + `requests.thinking_enabled_seen` counter (added in 2.6b). The "501 fired" semantic dies when L2 lifts the 501; no clean repurpose.
- `rewrites.thinking_disabled` counter (added in 2.6b). The inject-disabled logic is gone; the counter would always be zero.
- `measure.RecordThinkingEnabledSeen` method, `measure.RewriteThinkingDisabled` const, `Collector.requests` field, `Snapshot.Requests` field.
- 2.6b-era tests: `TestMessages_ThinkingEnabled_Returns501`, `TestMessages_NoThinking_InjectsDisabled`, `TestMessages_ThinkingRejected`, `TestMessages_ThinkingPreventsStopSequencesCounter`, `TestAnthropicToOpenAI_ThinkingRejected`. Replaced with positive-case tests for the new pass-through + roundtrip behaviors.

### Known limitations (deferred — see plan)
- `thinking: {display: "omitted"}` — no stateless path to reproduce signature for absent content.
- `redacted_thinking` blocks — same reason.
- Streaming `delta.reasoning_content` per-token forwarding — alongside true SSE passthrough.

## [0.0.2] — Stage 2.6b-followup (2026-05-27)

Live experiment hit `Client.Timeout=60s` on a multi-persona review
through shim; legitimate long generations need wider headroom.

### Changed
- Upstream `http.Client.Timeout` raised 60s → 180s in `server.go::newUpstreamClient`. Covers DeepSeek v4-pro reasoning-mode generations under buffer-then-restream MVP (~30-60s think + ~30-60s content).
- Server `WriteTimeout` raised 70s → 200s. Sized to outlive Client.Timeout so upstream cancellations surface as recordable upstream errors rather than as server-side write timeouts.

### Known gap surfaced (deferred to backlog)
- Body-read timeouts mid-stream get bucketed as `upstream_errors.by_status.200` because `upstream.StatusCode` reflects the headers that arrived before the timeout. See `todos/backlog.md`.

## [0.0.2] — Stage 2.6b (2026-05-27)

Thinking control plane + L2-demand telemetry. Stage 2.6's body capture
identified the reasoning_content roundtrip bug within one session.
Huddle (Linus + Maya + Jordan + Alex) recommended split-and-measure over
atomic L1+L2: bug fix ships now, full reasoning_content ↔ thinking-block
translation (Stage 2.6c) fires only if `requests.thinking_enabled_seen`
counter shows real demand.

### Added
- `thinking` request param parsed from inbound Anthropic Messages requests; forwarded to DeepSeek's OpenAI-format endpoint via the matching `thinking` body param. When client omits the field, shim injects `{type: "disabled"}` on outbound — reverses DeepSeek's silent-default-enabled behavior that 400'd on tool-call continuations.
- `requests.thinking_enabled_seen` counter in `/v1/metrics` — fires when a client sends `thinking: {type: "enabled"}`. The L2-demand telemetry for Stage 2.6c.
- `rewrites.thinking_disabled` counter in `/v1/metrics` — fires when shim injects `{type: "disabled"}` because the client omitted thinking. Explicit client-requested disabled is NOT counted (it's not a rewrite).
- `TestE2E_ToolContinuation_NoLongerTriggers400` — 2-turn fixture (tool_call + tool_result continuation) that pre-2.6b would fail under the fake stub's tool-continuation contract proxy. Named regression fence.
- Three unit tests: `TestMessages_ThinkingEnabled_Returns501`, `TestMessages_NoThinking_InjectsDisabled`, `TestMessages_ThinkingDisabled_PassesThrough`.
- `FakeUpstream.EnforceToolContinuationContract(bool)` in the e2e harness — opt-in stub mode that 400s on tool-call continuations without `thinking: {type: "disabled"}`. PROXY for DeepSeek's real contract, not faithful model; documented in godoc.
- `thinking` and `reasoning_content` added to `obslog` scrub list — defense-in-depth; these fields don't appear in shim's own log calls today but the Stage 2.6 `body_preview` may carry them when DeepSeek echoes errors. (Deliberately NOT scrubbing `reasoning_effort` — config string, not content.)

### Changed
- `AnthropicRequest` gains `Thinking *AnthropicThinkingConfig` field (minimal `{Type string}` shape; budget_tokens/display will land in 2.6c when there's a code path that uses them).
- `OpenAIRequest` gains `Thinking *DeepSeekThinkingConfig` field (minimal `{Type string}` shape).
- `measure.Collector` gains `requests map[string]int` and corresponding `Requests` field on `Snapshot` with JSON key `requests`. Generic enough that future request-feature counters land here too.

### 501 inline guard
- Mirrors the existing `containsThinkingBlock` 501 in `handleMessages` — different layer (request-level config vs message-level content blocks). Both gates fall in Stage 2.6c.

### Deferred to Stage 2.6c (gated on telemetry)
- reasoning_content ↔ thinking-block translation (data plane).
- Constant signature `"shim-passthrough-v1"` on thinking blocks (locked design, Linus call).
- `reasoningClient` sibling http.Client with longer timeout (Alex).
- `reasoning_latency_ms` histogram.
- Tool+thinking interleave fixtures.

### Deferred indefinitely
- `display: "omitted"` and `redacted_thinking` blocks — no stateless roundtrip path.
- Streaming `delta.reasoning_content` per-token forwarding — alongside true SSE passthrough work.
- Env-var knob `SHIM_THINKING` — adds config sprawl with no user-behind-the-knob.

## [0.0.2] — Stage 2.6 (2026-05-27)

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
