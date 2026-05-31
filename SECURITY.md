# Security policy

## Reporting

Report vulnerabilities privately through GitHub Security Advisories:
[Report a vulnerability](https://github.com/1mb-dev/shim/security/advisories/new).
Don't open a public issue for a security report.

## Supported versions

Latest release only. Fixes land on `main` and ship in the next tag.

## Threat model

shim has no inbound authentication. It trusts the network boundary between
itself and the client, and is built to run on loopback (`BIND_ADDR=127.0.0.1`,
the default). The inbound `Authorization` header is discarded; shim authenticates
upstream with `UPSTREAM_API_KEY`.

Binding to a non-loopback address exposes an unauthenticated proxy carrying your
upstream key, and for keyless presets (Ollama) an open relay to the local model.
shim emits a startup `WARN` in that case. Put an authenticating reverse proxy in
front if you must bind wide. This posture is by design, not a vulnerability.

Logs redact `Authorization`, prompt and message content, URL query strings, and
credential-shaped keys by default (`LOG_REDACT=true`).
