# moltable CLI build targets.
#
# Everything here runs from apps/cli/. The one exception is
# release-dry-run, which has to invoke goreleaser from the repo root
# because .goreleaser.yaml lives there.

.PHONY: build test test-race vet lint release-dry-run install-local clean help

GO ?= go
BINARY ?= moltable

# Repo root, relative to this Makefile. Used by release-dry-run.
REPO_ROOT := $(abspath $(dir $(lastword $(MAKEFILE_LIST)))/../..)

# Git commit + build date for ldflags injection. Same fields goreleaser
# injects at release time, so local `make build` and `make install-local`
# binaries also surface a real SHA in `moltable version`. Falls back to
# `unknown` outside a git checkout.
VERSION_PKG := github.com/gczh/moltable-cli/internal/version
GIT_COMMIT  := $(shell git -C $(REPO_ROOT) rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_DATE  := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS     := -X $(VERSION_PKG).BinaryCommit=$(GIT_COMMIT) -X $(VERSION_PKG).BinaryBuildDate=$(BUILD_DATE)

help: ## Show this help.
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z_-]+:.*## / {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## Build the CLI binary into ./moltable.
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/moltable

test: ## Run unit tests.
	$(GO) test -count=1 ./...

test-race: ## Run unit tests with the race detector.
	$(GO) test -count=1 -race ./...

vet: ## Run `go vet`.
	$(GO) vet ./...

lint: vet ## Alias for vet (real linter wired by CI).

release-dry-run: ## Run goreleaser in snapshot mode against the repo root.
	cd $(REPO_ROOT) && goreleaser release --snapshot --clean

install-local: ## Install the CLI into $$GOPATH/bin (or $$GOBIN).
	$(GO) install -ldflags "$(LDFLAGS)" ./cmd/moltable

clean: ## Remove the build output.
	rm -f $(BINARY)
