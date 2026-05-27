# Stage 2.5b — live smoke

One opt-in test. Spawns `./shim` against the real DeepSeek API,
sends one minimal request, asserts the round-trip works end to end.
Closes the gap between the harness (which uses a fake upstream) and
production reality.

## Running

```sh
export SHIM_SMOKE=1
export DEEPSEEK_SMOKE_API_KEY=<your key>
make smoke
```

Both env vars are required. Missing `SHIM_SMOKE` → test skips
silently. Missing the key with `SHIM_SMOKE=1` → test fails fast with
a clear message.

Optional:

```sh
export SHIM_SMOKE_MODEL=claude-opus-4-7   # exercises today's-bug path
```

Default model is `claude-sonnet-4-6` (cheapest mapping → `deepseek-v4-flash`).

## Why a separate key

`DEEPSEEK_SMOKE_API_KEY` is intentionally distinct from `UPSTREAM_API_KEY`
so smoke runs land on a separate billing line. Recommend a key with a
low monthly cap dedicated to smoke + integration testing.

Cost per run: ~$0.001 on the flash tier. Cheap enough to run guilt-free
before every push; expensive enough that you'd notice 10,000× by accident.

## What gets asserted

- HTTP 200 from `/v1/messages`
- Response body has Anthropic shape (`type: "message"`, `role: "assistant"`)
- Non-empty assistant text
- `/v1/metrics` after the call shows `requests_seen[/v1/messages] >= 1`
- `token_delta[/v1/messages].n >= 1` (upstream returned usage)
- `upstream_errors[/v1/messages].total == 0` (clean round-trip)
- `rewrites.model >= 1` (claude-* got mapped to deepseek-*)

## When to run

- Before tagging a release.
- After any change to `internal/adapter/deepseek/`.
- After any change to `internal/translate/` that touches request building.
- When updating DeepSeek-side model IDs.

NOT in CI by default. NOT on every `make test`. NOT on every commit.
The build tag (`//go:build smoke`) and `SHIM_SMOKE=1` gate are
belt-and-suspenders against accidental invocation.
