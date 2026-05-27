# shim — Go-native Anthropic ↔ OpenAI translation proxy
# Stage 0 targets. CGO_ENABLED=0 for static binaries.

BINARY    := shim
GOFLAGS   := -trimpath
LDFLAGS   := -s -w
BUILD_DIR := dist

# Resolve golangci-lint: prefer PATH (common on dev boxes via brew/install
# script), fall back to GOPATH/bin.
GOBIN     := $(shell go env GOPATH)/bin
LINT      := $(shell command -v golangci-lint 2>/dev/null || echo $(GOBIN)/golangci-lint)

# Stage 0 cross-compile matrix (3 platforms — see plan §AC 2 amendment).
PLATFORMS := darwin/arm64 linux/amd64 linux/arm64

.PHONY: help build build-all test test-race e2e smoke coverage lint vet clean

help:
	@echo "targets: build build-all test e2e smoke coverage lint vet clean"

build:
	CGO_ENABLED=0 go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BINARY) ./cmd/shim

build-all: clean
	@mkdir -p $(BUILD_DIR)
	@for p in $(PLATFORMS); do \
		GOOS=$$(echo $$p | cut -d/ -f1); \
		GOARCH=$$(echo $$p | cut -d/ -f2); \
		out=$(BUILD_DIR)/$(BINARY)-$${GOOS}-$${GOARCH}; \
		CGO_ENABLED=0 GOOS=$$GOOS GOARCH=$$GOARCH \
			go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $$out ./cmd/shim || exit 1; \
		echo "built $$out"; \
	done

# Default `test` runs with -race per plan §2. Override on platforms where
# the race detector is unavailable (e.g. linux/arm64 on a kernel with
# 47-bit VMA): `make test RACE=`.
RACE ?= -race

test:
	go test $(RACE) ./...
	@echo "# tip: e2e suite gated by build tag — run 'make e2e'"

# Process-boundary E2E: builds ./shim, spawns it against a fake upstream,
# exercises real HTTP. Gated behind the `e2e` build tag so `make test` stays
# fast. CI invokes both targets.
e2e:
	go test -tags e2e -count=1 ./internal/e2e/...
	@echo "# tip: live-upstream smoke — set SHIM_SMOKE=1 + DEEPSEEK_SMOKE_API_KEY and run 'make smoke'"

# Live smoke: spawns ./shim against api.deepseek.com for one real request.
# Double-gated: build tag `smoke` AND env var SHIM_SMOKE=1. Requires
# DEEPSEEK_SMOKE_API_KEY (distinct from UPSTREAM_API_KEY for billing
# isolation). Never run in CI by default; pre-release/pre-push only.
smoke:
	go test -tags smoke -count=1 -v ./internal/smoke/...
	@echo "# smoke complete (skipped if SHIM_SMOKE != 1; see internal/smoke/README.md)"

coverage:
	go test $(RACE) -coverprofile=cover.out ./...
	@go tool cover -func=cover.out | tail -1

lint:
	$(LINT) run ./...

vet:
	go vet ./...

clean:
	rm -rf $(BUILD_DIR) $(BINARY) cover.out
