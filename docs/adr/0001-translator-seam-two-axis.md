# ADR 0001 — Per-adapter Translator seam; transport-dialect vs provider-quirk axes

**Status:** Accepted (v0.3.0) · **Date:** 2026-05-29

## Context

Through v0.0.2 shim had one adapter (DeepSeek) and the request handler hard-wired
`translate.AnthropicToOpenAI` / `OpenAIToAnthropic`. That baked one assumption
into the transport-agnostic handler: **the upstream speaks OpenAI ChatCompletions.**

Adding anthropic-passthrough (a transparent proxy to a native Anthropic endpoint)
exposed that this collapsed two independent axes:

- **Transport dialect** — does the upstream speak OpenAI ChatCompletions, or
  native Anthropic Messages (identity, no translation)?
- **Provider quirks** — auth scheme, model-name mapping, response envelope.

The `Adapter` interface modelled quirks but assumed the dialect. Passthrough has
*no* dialect translation, which the interface could not express without either
absurd double-translation or a handler that branches on dialect.

## Decision

Introduce a per-adapter **`translate.Translator`** that owns the transport
dialect; `Adapter.Translator()` returns it. The handler calls the translator
(`ToUpstream` / `FromUpstream` / `StreamChunks`) and never names a dialect —
**zero dialect conditionals in the handler.**

- DeepSeek (and future OpenAI-dialect providers) return `AnthropicOpenAI()`.
- anthropic-passthrough returns `Identity()` — request/response forwarded as raw
  bytes, SSE scanned and forwarded event-by-event verbatim.

`Translator` lives in `internal/translate` (it *is* a translation concept and
references that package's types). This adds a one-way `adapter → translate`
dependency; no cycle (`translate` never imports `adapter`). The translator is
pure for request/response (no I/O, no state); `MapModel`, auth, and the
stop-sequences cap stay on the adapter/dialect, not the handler.

### Rejected alternatives

- **S1 — `IsPassthrough() bool` + handler branch.** The bool makes the handler
  branch on dialect — the exact backchannel the "clean adapter interface" thesis
  warns against. The handler must stay dialect-agnostic.
- **S2 — passthrough as a server "mode".** The upstream's dialect is a property
  of *that upstream* (its base URL, auth, model names = adapter-shaped config).
  A global mode splits config across two layers.

## Consequences

- New provider = one sub-package implementing `Adapter` incl. `Translator()` for
  its dialect; registered explicitly in `cmd/shim/main.go` (no `init()`).
- The seam is symmetric and **locked** after v0.3.0: `ToUpstream(req, raw, model)
  → (body, stopCapped)`, `FromUpstream(body, model) → (bytes, usage)`,
  `StreamChunks(resp, model) → (pull-iterator, usage)`. `FromUpstream`/identity
  return **bytes** (not a struct) so passthrough is lossless — re-encoding through
  shim's response struct would drop fields shim doesn't model.
- The handler forwards an allowlist of upstream response headers
  (`request-id`, `retry-after`, `anthropic-ratelimit-*`) dialect-agnostically.
- Reopening the seam is the signal that a third axis appeared — review before
  changing it.
