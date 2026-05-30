# ADR 0002 — Extend the Translator seam to the upstream error path

**Status:** Accepted (v0.3.1) · **Date:** 2026-05-30 · **Extends:** [ADR 0001](0001-translator-seam-two-axis.md)

## Context

ADR 0001 moved transport-dialect knowledge out of the request handlers into a
per-adapter `translate.Translator` (`ToUpstream` / `FromUpstream` /
`StreamChunks`) and **locked** the seam: reopening it is the signal that a third
axis appeared, review first.

Those three methods cover only the **success** path. The error path stayed in
the server: `writeUpstreamError` had a hardcoded switch that re-classified every
non-2xx upstream status (`400`/`5xx → 502`, `401/403 → 401`, `429 → 429`) and
replaced the body with shim's own Anthropic envelope, dropping the upstream body
(echoed only to the `upstream error` log line).

That switch is correct for the **OpenAI dialect** (DeepSeek): the upstream error
body is OpenAI-shaped and may carry prompt content — leaking it to an Anthropic
client would be both wrong-shaped and a disclosure. But for **anthropic-passthrough**
it is lossy: the upstream *is* native Anthropic, so its status and error body are
already exactly what the client should receive. Re-classifying `400 → 502` and
discarding the body breaks the "transparent proxy" contract on the error path.

So *how an upstream error is rendered to the client* varies by **transport
dialect** — the same first axis ADR 0001 already isolates, not a new one. The
hardcoded server switch had simply left the error path on the dialect-coupled
side of the seam.

## Decision

Add a fourth method to `translate.Translator`, the error-path analog of
`FromUpstream`:

```go
FromUpstreamError(upstreamStatus int, upstreamBody []byte) (clientStatus int, clientBody []byte)
```

- **identity** (passthrough) → returns `(upstreamStatus, upstreamBody)` verbatim.
- **anthropicOpenAI** (DeepSeek) → the prior re-classification, returning a fresh
  Anthropic envelope built from the status; the upstream body is dropped.

`writeUpstreamError` loses its switch and calls the translator. The server keeps
the dialect-agnostic concerns it already owned: record the error for
`/v1/metrics`, log one `upstream error` line (now carrying both `upstream_status`
and the client-facing `status` plus a capped `body_preview`), forward the header
allowlist, and write the bytes. **Handlers remain dialect-free** (the ADR 0001
invariant holds — grep-gate stays green).

The canonical Anthropic error envelope + the error-type taxonomy move into
`translate` (`AnthropicErrorJSON`, `ErrType*`), since they are Anthropic wire
shapes and are now produced from both sides of the seam; `internal/server` keeps
its `errAPI`/… names as thin aliases, so server call sites are unchanged.

### Rejected alternatives

- **Narrower `PassthroughErrors() bool` + keep the server switch.** Leaves the
  OpenAI re-classification (a dialect concern) in the server and is asymmetric
  with `FromUpstream`. A bool that the handler branches on is the backchannel
  ADR 0001 rejected (its S1).
- **Handler probes the dialect / adapter name.** Directly violates the
  dialect-free-handler invariant.

## Consequences

- The seam is four methods, still symmetric (success + error). Re-locked.
- Read-failure on a 2xx response is *not* an upstream error response; the
  non-stream handler routes it to a dialect-independent `502` before reaching
  `FromUpstreamError` (which keys on status and would otherwise mis-render a 2xx).
- The upstream-error path now emits a single log line (no separate
  `request failed` line) — a deliberate shape change vs v0.3.0.
- A future translating dialect inherits the OpenAI re-classification only if it
  returns `AnthropicOpenAI()`; a native dialect gets verbatim error
  transparency for free via `Identity()`.
