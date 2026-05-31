---
title: Getting started
---

# Getting started

## Install

```sh
brew install 1mb-dev/tap/shim
go install github.com/1mb-dev/shim/cmd/shim@latest
docker pull ghcr.io/1mb-dev/shim
```

Or from source (Go 1.25+):

```sh
git clone https://github.com/1mb-dev/shim
cd shim
make build        # -> ./shim
```

## Configure

shim selects an upstream with `ADAPTER` (default `deepseek`). The OpenAI-dialect
presets need an `UPSTREAM_API_KEY`; `ollama` runs keyless. Copy `.env.example`
to `.env` and set your key, or export it:

```sh
export ADAPTER=deepseek
export UPSTREAM_API_KEY=<your key>
```

shim reads config in order: `SHIM_ENV_FILE`, then `./.env`, then
`~/.config/shim/.env` (the last so a background service finds it regardless of
working directory). Every variable and the per-preset model maps:
[Configuration]({{ '/configuration.html' | relative_url }}).

## Run

```sh
shim &            # serves 127.0.0.1:8082 (loopback)
export ANTHROPIC_BASE_URL=http://127.0.0.1:8082
export ANTHROPIC_API_KEY=shim   # any non-empty value; shim auths upstream itself
claude
```

Or let the launcher set both vars and exec claude in one step:

```sh
shim run "write a hello-world go program"
```

Installed via Homebrew, shim can run as an always-on background service (opt-in,
not auto-started). Put your key in `~/.config/shim/.env` first, then:

```sh
brew services start shim
```

## Verify

After a request, see what shim did:

```sh
curl -s http://127.0.0.1:8082/v1/metrics | python3 -m json.tool
```

Latency, the gap between shim's cl100k token count and the upstream's, and a
tally of every request shim rewrote in flight. Full reference:
[Measurement]({{ '/measurement.html' | relative_url }}).

## Security

shim has no inbound authentication and binds loopback by default. Don't bind a
non-loopback address without an authenticating proxy in front — see the
[security model](https://github.com/1mb-dev/shim#security-model) and
[SECURITY.md](https://github.com/1mb-dev/shim/blob/main/SECURITY.md).
