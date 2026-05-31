---
title: shim
---

An HTTP proxy that puts the Anthropic Messages API in front of OpenAI-compatible
model providers, and measures every request it forwards. Point Claude Code at
shim and its calls are translated to OpenAI ChatCompletions and routed to your
upstream — or passed through verbatim to a native Anthropic endpoint.

shim is a measurement layer, not a way to get cheaper tokens: `/v1/metrics`
reports per-request latency and token drift, every request shim rewrites in
flight is logged, and `/v1/messages/explain` returns the exact upstream body
shim would send before a request leaves the box.

## Install

```sh
go install github.com/1mb-dev/shim/cmd/shim@latest
```

## Quick start

```sh
export UPSTREAM_API_KEY=<deepseek key>   # ADAPTER=deepseek by default
shim &                                   # serves 127.0.0.1:8082
ANTHROPIC_BASE_URL=http://127.0.0.1:8082 ANTHROPIC_API_KEY=shim claude
```

Then see what it did: `curl -s localhost:8082/v1/metrics | python3 -m json.tool`.

## Docs

- [Measurement reference]({{ '/measurement.html' | relative_url }}) — `/v1/metrics`, Prometheus, errors, token counting
- [README](https://github.com/1mb-dev/shim#readme) — configuration, security model, limitations
- [Architecture decisions](https://github.com/1mb-dev/shim/tree/main/docs/adr)
- [Changelog](https://github.com/1mb-dev/shim/blob/main/CHANGELOG.md)
- [Source](https://github.com/1mb-dev/shim)
