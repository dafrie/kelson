# kelson build tooling (issue #18); release tooling added in issue #21.

GO      ?= go
GOLANGCI?= golangci-lint
NPM     ?= npm
BIN     ?= $(CURDIR)/bin
UI      ?= $(CURDIR)/ui
# Where kelson-server embeds the UI from (internal/webui). Everything the copy
# below puts here is gitignored; the two committed files are excluded from every
# rm in this file by name.
EMBED   ?= $(CURDIR)/internal/webui/static
KEEP    := ! -name .gitignore ! -name placeholder.html

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "0.0.0-dev")
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
LDFLAGS := -s -w \
  -X github.com/dafrie/kelson/internal/version.Version=$(VERSION) \
  -X github.com/dafrie/kelson/internal/version.Commit=$(COMMIT)

# Build tools that are installed on demand rather than vendored. Both are
# pinned: an unpinned generator is a generator whose output changes when
# somebody else cuts a release, which is the one thing a committed generated
# artifact must never do.
CONTROLLER_GEN         ?= $(GOBIN_DIR)/controller-gen
CONTROLLER_GEN_VERSION ?= v0.19.0
SETUP_ENVTEST          ?= $(GOBIN_DIR)/setup-envtest
# The API-server version the envtest suite runs against. It does not have to
# match the k8s.io libraries in go.mod: what is being tested is that a real API
# server accepts the generated CRDs, and a floor is more useful than a mirror.
ENVTEST_K8S_VERSION    ?= 1.36.2
GOBIN_DIR              ?= $(shell $(GO) env GOPATH)/bin

.PHONY: all build binaries server ui ui-clean proto generate deepcopy test test-e2e envtest lint fmt clean install release release-snapshot e2e-up e2e e2e-down kind-up kind-down

all: lint test build

# Verifies the whole module compiles. The actual binaries are produced by
# `make binaries` (with version ldflags) or by goreleaser (`make release`).
#
# Neither this nor `binaries` depends on `ui`, and that is deliberate: the
# checkout must build with no Node installed (CI cross-compiles without it), so
# the Go path stays pure and a server built this way serves the placeholder page
# instead of the UI. `make server` is the one that builds both.
build:
	$(GO) build ./...

# Builds the cmd/* binaries with version ldflags injected into bin/.
binaries:
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN)/kelson            ./cmd/kelson
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN)/kelson-server     ./cmd/kelson-server
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN)/kelson-mcp        ./cmd/kelson-mcp
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN)/kelson-controller ./cmd/kelson-controller

# kelson-server with the real web UI inside it — what a release ships and what
# you want when clicking around locally. Needs Node.
server: ui
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN)/kelson-server ./cmd/kelson-server

# Builds ui/ and copies the result where go:embed can reach it. The copy is a
# build artifact and is gitignored there, so this leaves `git status` clean —
# which goreleaser depends on, since it refuses to release from a dirty tree
# (see internal/webui/webui.go for why the directory is shaped this way).
ui: ui-clean
	cd $(UI) && { [ -d node_modules ] || $(NPM) ci; } && $(NPM) run build
	cp -R $(UI)/dist/. $(EMBED)/

# Empties the embed directory back to its two committed files. `ui` runs it
# first so a hashed asset from a previous build is never embedded beside a
# current one — nothing removes it otherwise, since every name is new.
ui-clean:
	find $(EMBED) -mindepth 1 -maxdepth 1 $(KEEP) -exec rm -rf {} +

# Regenerates internal/api/gen and ui/src/gen from proto/ (ADR-0013 §4). Both
# outputs are committed, so a change to proto/ that is not followed by this
# target leaves the schema and the served code disagreeing. Needs buf,
# protoc-gen-go and protoc-gen-connect-go on PATH, plus `npm ci` in ui/ for the
# TypeScript plugin (buf.gen.yaml runs ui/node_modules/.bin/protoc-gen-es). It
# is deliberately not part of `all`, because the checkout must build without
# any of them.
proto:
	buf generate

# Regenerates everything produced from the Go types: schema/*.json and
# deploy/crds/*.yaml (internal/schemagen), docs/reference/ (internal/specrefdoc),
# the ClusterProfile support matrix, and api/kelson/v1alpha1's deep copies. Every
# output is committed and drift-tested, so a change to internal/model that is not
# followed by this target leaves a red test rather than a silent disagreement.
#
# Like `proto`, it is deliberately not part of `all`: the checkout must build
# without controller-gen installed.
generate: deepcopy
	$(GO) generate ./internal/model ./internal/specrefdoc ./internal/clusterprofile/support/gendoc
	# Helm requires plain, untemplated YAML under a chart's crds/, and a chart is
	# packaged from its own directory — so the chart carries a copy rather than a
	# reference (deploy/chart/crds_test.go compares them).
	cp deploy/crds/kelson.dev_*.yaml deploy/chart/kelson/crds/

# api/kelson/v1alpha1/zz_generated.deepcopy.go — the one build tool ADR-0027
# decision 3 adopts. It is NOT pointed at internal/model: the spec structs get
# their deep copies by hand there (internal/model/deepcopy.go explains why), and
# controller-gen calls into them instead of recursing.
deepcopy:
	@command -v $(CONTROLLER_GEN) >/dev/null 2>&1 || \
		GOBIN=$(GOBIN_DIR) $(GO) install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION)
	$(CONTROLLER_GEN) object paths=./api/...

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

# `ui` first for the same reason .github/workflows/release.yml runs it before
# goreleaser: goreleaser only compiles Go, so whatever is in the embed directory
# when it starts is what the binaries carry.
release-snapshot: ui
	goreleaser release --snapshot --clean

clean: ui-clean
	$(GO) clean ./...
	rm -rf $(BIN) dist/ $(UI)/dist

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

# A complete local kelson on kind (docs/local.md): cluster, in-cluster
# registry, and a Helm install of a dev image built from this checkout with
# the web UI inside. Idempotent; re-running rebuilds and redeploys the image.
kind-up:
	hack/local/up.sh

kind-down:
	hack/local/down.sh

# The Go end-to-end suite (test/e2e, behind the `e2e` build tag) — the shape CI
# runs. It provisions the same kind cluster first and leaves it up afterwards.
# `go test ./...` never runs it: the build tag keeps it out, and KELSON_E2E=1 is
# required on top of that. See test/e2e/README.md.
#
# spine.sh (which calls e2e-up itself) installs Flux, the in-cluster registry
# and kelson-controller on that cluster, because TestDeliverySpine — the R1 exit
# gate — asserts against all three and fails loudly rather than skipping when
# they are absent. It is idempotent, so re-running this target re-uses what is
# already there and only refreshes the controller image.
test-e2e:
	hack/e2e/spine.sh
	KELSON_E2E=1 KUBECONFIG=$(CURDIR)/hack/bin/e2e.kubeconfig \
		$(GO) test -tags e2e -v -timeout 25m ./test/e2e/...

# The controller's envtest suite (internal/controller, behind the `envtest`
# build tag): the generated CRDs against a real API server, which is the only
# place the structural schema, the pruning and the CEL rules are actually
# exercised. It needs no cluster — envtest runs an apiserver and an etcd from
# downloaded binaries — but it does need those binaries, which is why it is a
# target of its own and why `go test ./...` never runs it. Same two gates as
# test-e2e: a build tag and an environment variable (see internal/controller's
# envtest_test.go).
envtest:
	@command -v $(SETUP_ENVTEST) >/dev/null 2>&1 || \
		GOBIN=$(GOBIN_DIR) $(GO) install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(CURDIR)/hack/bin -p path)" \
		KELSON_ENVTEST=1 $(GO) test -tags envtest -v -timeout 10m ./internal/controller/...
