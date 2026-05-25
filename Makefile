# shim — Go-native Anthropic ↔ OpenAI translation proxy
# Stage 0 targets. CGO_ENABLED=0 for static binaries.

BINARY    := shim
GOFLAGS   := -trimpath
LDFLAGS   := -s -w
BUILD_DIR := dist

# Resolve tools from GOPATH/bin so callers don't need it on PATH.
GOBIN     := $(shell go env GOPATH)/bin
LINT      := $(GOBIN)/golangci-lint

# Stage 0 cross-compile matrix (3 platforms — see plan §AC 2 amendment).
PLATFORMS := darwin/arm64 linux/amd64 linux/arm64

.PHONY: help build build-all test test-race coverage lint vet clean

help:
	@echo "targets: build build-all test coverage lint vet clean"

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

# Default `test` runs with -race per plan §2 (every package, every run).
test:
	go test -race ./...

coverage:
	go test -race -coverprofile=cover.out ./...
	@go tool cover -func=cover.out | tail -1

lint:
	$(LINT) run ./...

vet:
	go vet ./...

clean:
	rm -rf $(BUILD_DIR) $(BINARY) cover.out
