# kelson build tooling (issue #18); release tooling added in issue #21.

GO      ?= go
GOLANGCI?= golangci-lint
BIN     ?= $(CURDIR)/bin

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "0.0.0-dev")
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
LDFLAGS := -s -w \
  -X github.com/dafrie/kelson/internal/version.Version=$(VERSION) \
  -X github.com/dafrie/kelson/internal/version.Commit=$(COMMIT)

.PHONY: all build binaries test lint fmt clean install release release-snapshot

all: lint test build

# Verifies the whole module compiles. The actual binaries are produced by
# `make binaries` (with version ldflags) or by goreleaser (`make release`).
build:
	$(GO) build ./...

# Builds the cmd/* binaries with version ldflags injected into bin/.
binaries:
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN)/kelson       ./cmd/kelson
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN)/kelson-server ./cmd/kelson-server

test:
	$(GO) test -race ./...

lint:
	$(GOLANGCI) run ./...

fmt:
	gofmt -l -w .

install: # install golangci-lint if missing
	command -v $(GOLANGCI) >/dev/null 2>&1 || $(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest

# Cuts a release. Run `git tag vX.Y.Z && git push origin vX.Y.Z` instead —
# that triggers .github/workflows/release.yml, which runs goreleaser. Use
# `release-snapshot` locally to exercise the goreleaser config without tagging.
release:
	goreleaser release --clean

release-snapshot:
	goreleaser release --snapshot --clean

clean:
	$(GO) clean ./...
	rm -rf $(BIN) dist/
