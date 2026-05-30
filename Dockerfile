# syntax=docker/dockerfile:1
#
# Minimal runtime image: the static shim binary + CA certs (needed to verify
# TLS for HTTPS upstreams), nothing else. goreleaser cross-compiles the binary
# and places it in the build context as `shim`; this Dockerfile only packages it
# (no Go toolchain in the image).
FROM alpine:3.20 AS certs
RUN apk add --no-cache ca-certificates

FROM scratch
# goreleaser's dockers_v2 builds multi-platform in one pass and stages each
# target's binary under $TARGETPLATFORM/ (e.g. linux/amd64/shim); buildx sets
# TARGETPLATFORM per --platform. (Classic single-arch `COPY shim` would only
# work for one arch.)
ARG TARGETPLATFORM
COPY --from=certs /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY ${TARGETPLATFORM}/shim /shim

# shim binds 127.0.0.1:8082 by default (thesis: never bind all interfaces
# without an authenticating proxy). In a container, reach it by setting
# BIND_ADDR=0.0.0.0 (and fronting with auth) or using host networking.
EXPOSE 8082

# Run unprivileged (numeric UID works without /etc/passwd on scratch). shim
# writes nothing and reads only its compiled-in tokenizer, so nobody is enough.
USER 65534:65534

# No HEALTHCHECK: a scratch image has no shell/curl to run one. Use the
# orchestrator's HTTP probe against /healthz (k8s httpGet, compose, etc.).
ENTRYPOINT ["/shim"]
