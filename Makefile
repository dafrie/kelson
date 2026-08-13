# kelson build tooling (issue #18); release tooling added in issue #21.

GO      ?= go
GOLANGCI?= golangci-lint
BIN     ?= $(CURDIR)/bin

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "0.0.0-dev")
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
LDFLAGS := -s -w \
  -X github.com/dafrie/kelson/internal/version.Version=$(VERSION) \
  -X github.com/dafrie/kelson/internal/version.Commit=$(COMMIT)

.PHONY: all build binaries proto test lint fmt clean install release release-snapshot e2e-up e2e e2e-down

all: lint test build

# Verifies the whole module compiles. The actual binaries are produced by
# `make binaries` (with version ldflags) or by goreleaser (`make release`).
build:
	$(GO) build ./...

# Builds the cmd/* binaries with version ldflags injected into bin/.
binaries:
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN)/kelson       ./cmd/kelson
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN)/kelson-server ./cmd/kelson-server

# Regenerates internal/api/gen from proto/ (ADR-0013 §4). The output is
# committed, so a change to proto/ that is not followed by this target leaves
# the schema and the served code disagreeing. Needs buf, protoc-gen-go and
# protoc-gen-connect-go on PATH; it is deliberately not part of `all`, because
# the checkout must build without them.
proto:
	buf generate

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

# kind-based E2E harness (issue #86): a spec to running workloads and back on
# a local kind cluster. e2e-up/e2e-down are idempotent and independent of
# each other; `make e2e` provisions the cluster and runs the full lifecycle
# but leaves it up afterward so the state can be inspected — run
# `make e2e-down` when done. See docs/e2e.md.
e2e-up:
	hack/e2e/up.sh

e2e: e2e-up
	hack/e2e/run.sh

e2e-down:
	hack/e2e/down.sh
