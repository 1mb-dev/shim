---
title: Configuration
---

# Configuration

Everything is an environment variable (or a line in a `.env` file; copy
[`.env.example`](https://github.com/1mb-dev/shim/blob/main/.env.example)).

| Variable | Default | Purpose |
|---|---|---|
| `BIND_ADDR` | `127.0.0.1` | Listen address. Loopback by default; binding wide exposes an unauthenticated proxy carrying your upstream key. shim emits a startup `WARN` when this is not loopback. |
| `PORT` | `8082` | TCP port. |
| `ADAPTER` | `deepseek` | `deepseek` / `openai` / `openrouter` / `ollama` (OpenAI-dialect, translating, buffered SSE) or `anthropic` (transparent passthrough, live SSE). Unknown values fail at startup. |
| `UPSTREAM_API_KEY` | required (except `ollama`) | `Authorization: Bearer` for the OpenAI-dialect presets; `x-api-key` for anthropic. `ollama` runs keyless. |
| `UPSTREAM_BASE_URL` | per-preset | Override the upstream root. Empty uses the chosen preset's default. |
| `UPSTREAM_OPUS_MODEL` | preset default | Override the upstream model for `claude-opus*`. |
| `UPSTREAM_SONNET_MODEL` | preset default | Override for `claude-sonnet*`. |
| `UPSTREAM_HAIKU_MODEL` | preset default | Override for `claude-haiku*`. |
| `UPSTREAM_MODEL` | (empty) | Catch-all for non-claude names, and the role model for presets without role defaults (ollama). |
| `LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error`. |
| `LOG_REDACT` | `true` | Scrub secrets and prompt content from logs. Set `false` for local debugging only. |
| `MAX_REQUEST_BYTES` | `1048576` | Oversize body returns an Anthropic-shaped HTTP 413. |

## Presets

Each preset maps `claude-opus*` / `claude-sonnet*` / `claude-haiku*` to its own
upstream models; override any role with `UPSTREAM_{OPUS,SONNET,HAIKU}_MODEL`.
The default base URLs and model maps (verified-current, but they drift with
vendor releases) are the canonical reference in
[`.env.example`](https://github.com/1mb-dev/shim/blob/main/.env.example).

| Preset | Base URL | Auth | Streaming |
|---|---|---|---|
| `deepseek` | `api.deepseek.com/v1` | key | buffered |
| `openai` | `api.openai.com/v1` | key | buffered |
| `openrouter` | `openrouter.ai/api/v1` | key | buffered |
| `ollama` | `localhost:11434/v1` | keyless | buffered |
| `anthropic` | `api.anthropic.com` | key (`x-api-key`) | live passthrough |

Adding an OpenAI-dialect preset is a data row in `internal/adapter/openaichat`,
not a new file — see
[CONTRIBUTING](https://github.com/1mb-dev/shim/blob/main/CONTRIBUTING.md).

## Operational limits

Hardcoded (not env-configurable): `ReadHeaderTimeout` 10s, `WriteTimeout` 200s,
`IdleTimeout` 120s, `MaxHeaderBytes` 1 MiB, upstream `Client.Timeout` 180s. The
200s server write timeout outlives the 180s upstream timeout so an upstream
cancellation surfaces as a recordable upstream error, not a server write
timeout, and covers reasoning-mode generations under the buffer-then-restream
path.
