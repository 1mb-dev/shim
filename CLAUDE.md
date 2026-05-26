# shim — project conventions

Go-native HTTP proxy translating Anthropic Messages API ↔ OpenAI ChatCompletions. Single static binary, zero runtime deps. Stage 0 ships one adapter (DeepSeek). Repo `github.com/1mb-dev/shim`; currently PRIVATE.

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
- Handler tests use the in-process stub-adapter pattern (`internal/server/handlers_test.go`), not network calls.
- Default bind is `127.0.0.1:8082` — explicit loopback. Never bind `:8082` (all interfaces) without an authenticating reverse proxy in front.
- DeepSeek's tuned HTTP client lives in the adapter package, exposed via `clientProvider` optional interface in `internal/server/server.go` — Stage 1 may collapse this when a second adapter forces a cleaner shape.

## Source-of-truth docs (gitignored, local)
- `todos/handoff-2026-05-25-shim-stage0.md` — full Stage 0 handoff context
- `todos/shim-stage0-plan.md` — 14-step Stage 0 implementation plan
- `todos/shim-stage0-notes.md` — plan deviation log
- `todos/project-review-2026-05-26.md` — comprehensive review feeding the Stage 1 huddle

## External Action Gate
Pushes, PRs, comments, releases — anything externally visible — require explicit user approval in a fresh user message. The repo is currently PRIVATE; flipping public is itself an external action. Per global `~/.claude/CLAUDE.md`.
