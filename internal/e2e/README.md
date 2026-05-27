# Stage 2.5 — E2E harness

Process-boundary integration tests. Builds `./shim`, spawns it against a fake
OpenAI-compatible upstream, exercises real HTTP. Closes the gap between
unit/httptest (which can't catch model-mapping drift or launcher
regressions) and live-API smoke (which is network-flaky and key-gated).

## Running

```sh
make e2e                              # standard run
go test -tags e2e -v ./internal/e2e   # verbose
go test -tags e2e ./internal/e2e -run TestE2E_Upstream400Becomes502  # single case
go test -tags e2e ./internal/e2e -update  # rewrite testdata/golden_stream.txt
```

`make test` does NOT include this suite — the `//go:build e2e` tag on every
file gates it out. Build the suite separately via `make e2e`.

## What it covers

| Case | Catches |
|------|---------|
| `HappyNonStream` | translation, model rewrite, request body sent upstream, metrics delta |
| `HappyStream` | SSE event order + byte-exact golden file |
| `ModelRewriteLoud` | thesis-2: every rewrite logs + increments `rewrites.model` |
| `Upstream400Becomes502` | upstream errors become Anthropic-shaped 502, not silent forwards |
| `CountTokensFixed` | `hello world` → 2 tokens (cl100k_base); regressions in tiktoken-go are immediate |
| `MeasurementReflectsCall` | `/v1/metrics` accumulates the right buckets after a real call |
| `LauncherEnvInjection` | `shim run` injects `ANTHROPIC_BASE_URL` pointing at a real listening shim |
| `Harness_CleanupOnEarlyFailure` | the harness itself doesn't orphan processes |

Every case uses **delta-based metric assertions** — never "non-zero". One
shim process serves all cases, so absolute checks would be order-dependent.

## What it does NOT cover

- **Live DeepSeek.** Network flake + key handling make it unsuitable for a
  regular gate. The README's "When NOT to use shim" section + manual
  `make smoke` (TBD) cover live-upstream contract drift.
- **Real `claude` binary.** Case 7 uses a shell-script stub; the launcher's
  exec path is already exercised in `internal/launcher/run_test.go`.
- **Cross-platform matrix.** Single platform; CI matrix is Stage 4+.

## Design notes

- **Port discovery.** Shim is spawned with `PORT=0`; the harness reads the
  bound port from the `"shim listening"` slog record on stderr. Zero race
  window vs. listen-and-close port picking.
- **Lifecycle.** `t.Cleanup` sends SIGINT (shim's `main.go` handles it
  gracefully); escalates to SIGKILL after 5s. Shim is killed *before* the
  fake upstream closes, so shutdown stderr doesn't fill with spurious
  connect-refused noise.
- **Watchdog.** A goroutine watches `cmd.Wait()`; the harness surfaces
  early shim death with stderr in the failure message.
- **Stderr capture.** Bounded buffer (1 MiB cap, drop-oldest); won't blow
  up if a future shim debug-level run goes log-crazy.
- **Per-case budget.** Each case wraps `withBudget(t, 5s, ...)` — silent
  slowdowns fail loudly. Tighten if cases start clustering near the cap.

## Known gaps surfaced by this stage

1. **No upstream-error counter in `/v1/metrics`.** `measure` tracks latency,
   token deltas, and rewrites but not upstream non-2xx counts. Stage 2.5b.
2. ~~Default opus mapping (`deepseek-v4-pro[1m]`) produced live 400s.~~
   **Resolved.** Verified against DeepSeek's `/api/create-chat-completion`
   reference: the OpenAI-format endpoint accepts exactly
   `[deepseek-v4-flash, deepseek-v4-pro]`. The `[1m]` variant is
   Anthropic-endpoint-only. `DefaultOpusModel` now `deepseek-v4-pro`.
