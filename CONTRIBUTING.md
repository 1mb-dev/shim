# Contributing

shim is a small, single-maintainer tool. Issues and PRs are welcome within the
scope below. Open an issue before a large change.

## Build and test

```sh
make build   # ./shim
make test    # go test -race ./...
make lint    # golangci-lint
make e2e     # process-boundary suite against a fake upstream
```

## Adding an OpenAI-dialect provider

A provider that speaks OpenAI ChatCompletions is a data row in the
`internal/adapter/openaichat` preset registry: base URL, per-role model map,
auth flag, optional headers. No new file. A genuinely new transport dialect
(not OpenAI-chat, not native Anthropic) is a new sub-package implementing
`adapter.Adapter` and its own `translate.Translator`.

## Scope

Out of scope by design (see "What doesn't" in the README): per-token streaming
for the translating presets, prompt-caching translation, the OpenAI Responses
API, and inbound authentication. PRs for these will be declined.

## Conventions

Commits are `<type>(<scope>): <subject>`. Match the surrounding code, keep
changes atomic, and add tests for new code.
