# shim — project conventions

Go-native HTTP proxy translating Anthropic Messages API ↔ OpenAI ChatCompletions. Single static binary, zero runtime deps. Stage 0 + 1 + 1.5 + 1.5b shipped; one adapter (DeepSeek) with prefix-aware claude-* mapping per the official guide. Repo `github.com/1mb-dev/shim`; currently PRIVATE.

## Tech stack
- Go 1.22 (see `go.mod`)
- stdlib-leaning; no external runtime deps (test stubs only)
- `golangci-lint` v2 config at `.golangci.yml`

## Make targets
- `make build` — `./shim` for current host (CGO disabled, `-trimpath`, `-s -w`)
- `make build-all` — cross-compile darwin/arm64, linux/amd64, linux/arm64 into `dist/`
- `make test` — `go test -race ./...`
- `make test RACE=` — race detection disabled (Pi 500 needs this: 47-bit VMA breaks TSan)
- `make coverage` — coverage profile + per-file summary
- `make lint` — `golangci-lint run ./...` (resolves binary via PATH then GOPATH/bin)
- `make vet` — `go vet ./...`

## Four locked theses
1. **Honest measurement built-in.** Redacted-by-default logs; surface every transformation. Silent forwards are bugs.
2. **Loud-fail on heuristic drift.** Emit a log line whenever shim modifies inbound traffic (model rewrite, `stop_sequences` cap, etc). Never silently forward.
3. **Drop-in single static binary.** Zero runtime toolchain; one file ships.
4. **Clean Adapter interface.** New provider = one new file under `internal/adapter/<name>/`. If shim grows backchannels into the adapter (substring-matched errors, struct probes), the interface is leaking — that's the signal for a Stage 1 refactor, not more workarounds.

## Code conventions
- `[ASSUMPTION]` markers in code or todos signal Stage 0 default choices needing Stage 1 confirmation (see `internal/translate/translate.go:20-22` for the canonical example).
- Errors at the server boundary fail loudly with an Anthropic-shaped JSON response — see `internal/server/errors.go`.
- All logging goes through the redacting slog handler (`internal/obslog/`). Do not bypass it. Sensitive keys + URL query strings + nested attrs are scrubbed before emission.
- Handler tests drive a real `httptest.NewServer(srv.http.Handler)` via the `doPOST`/`doGET` helpers, exercising mux routing + `MaxBytesReader` + timeouts. Failure-injection tests (encode-failure, SSE write-failure) use direct handler invocation with custom `ResponseWriter` types because they need to inject errors stdlib won't.
- Adapters implement the contract in `internal/adapter/adapter.go`: `Name`, `DefaultModel`, `MapModel`, `Validate`, `BuildRequest`, `NormalizeResponse`. Validate runs once at startup (`server.New`); misconfiguration blocks startup rather than failing per-request.
- Measurements (`internal/measure/`) are collected in-memory and exposed via `GET /v1/metrics`. Per-handler `defer func() { s.measure.RecordLatency(...) }()` (closure-wrapped — naïve form evaluates `time.Since` at defer-statement time). `RecordRewriteEvent` fires from `logModelRewrite` and `stop_sequences` truncation.
- Default bind is `127.0.0.1:8082` — explicit loopback. Never bind `:8082` (all interfaces) without an authenticating reverse proxy in front.
- DeepSeek's tuned HTTP client lives in the adapter package, exposed via `clientProvider` optional interface in `internal/server/server.go` — Stage 2 either collapses this or keeps it; decision deferred until adapter #2 forces the shape.

## Source-of-truth docs (gitignored, local)
- `todos/handoff-2026-05-25-shim-stage0.md` — full Stage 0 handoff context
- `todos/shim-stage{0,1,1.5}-plan.md` — per-stage implementation plans
- `todos/shim-stage{0,1,1.5}-notes.md` — per-stage deviation logs
- `todos/project-review-2026-05-26.md` — Stage 1 entry review (4-persona)
- `todos/stage{1,2}-huddle-2026-05-26.md` — strategic decision artifacts (Linus/Maya/Kai)
- `todos/backlog.md` — deferred items surfaced during reviews, picked up opportunistically
- `todos/pause-notes.md` — latest session pause context

## External Action Gate
Pushes, PRs, comments, releases — anything externally visible — require explicit user approval in a fresh user message. The repo is currently PRIVATE; flipping public is itself an external action. Per global `~/.claude/CLAUDE.md`.
