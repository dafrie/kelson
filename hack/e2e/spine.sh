#!/usr/bin/env bash
# Provisions the R1 delivery spine (ADR-0028) on the E2E kind cluster, so that
# test/e2e's TestDeliverySpine has a real controller, a real registry and a real
# Flux to assert against.
#
# It is the prerequisite half of the exit gate for R1 (docs/roadmap.md): "apply
# a Project and an Environment, artifact published, Flux reconciles,
# Environment.status reaches Healthy". This script builds the cluster side of
# that sentence; the Go suite makes the assertions.
#
# Idempotent, like up.sh: every stage converges, and re-running against a
# provisioned cluster refreshes the controller image and the release rather than
# failing.
#
# # Two choices worth stating, because both had an alternative
#
# **Flux and the registry are installed with `kelson install`, not with a stanza
# copied into this script.** kelson already owns an install path with a pinned,
# checksum-verified upstream manifest for flux-operator plus the FluxInstance it
# creates (internal/delivery/install/pins.go), and an authored, digest-pinned
# CNCF Distribution for the registry (`kelson install registry`, ADR-0030's
# 2026-08-14 amendment). Re-implementing either here would test a fixture rather
# than the product: it would leave `kelson install registry` with no end-to-end
# coverage at all, and it would let this harness's Flux drift from the version
# the pins table promises. hack/local/up.sh predates both rows and still runs
# its own registry Deployment; that is the copy to retire, not the one to
# duplicate.
#
# **No containerd trust is written onto the kind nodes.** hack/local/up.sh
# writes a hosts.toml per node because there the *kubelet* pulls application
# images from the in-cluster registry. Nothing does that here: the only clients
# of this registry are the controller (pushing an OCI artifact) and
# source-controller (pulling it), both in-cluster Go processes reaching a
# Service FQDN over plain HTTP, which they accept because they are told the host
# is insecure (--insecure-registries / OCIRepository `insecure: true`). The
# workload image is registry.k8s.io/pause, pulled from the internet as usual. A
# hosts.toml here would be ceremony that proves nothing.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
source "$SCRIPT_DIR/lib.sh"

# The release namespace, which is also the Flux namespace kelson's own
# OCIRepository/Kustomization pair lives in (ADR-0028 decision 3,
# controller.DefaultFluxNamespace) and the namespace `kelson install registry`
# lands in (install.RegistryNamespace).
NAMESPACE="kelson-system"
RELEASE="kelson"

CONTROLLER_IMAGE="kelson-controller"
CONTROLLER_TAG="e2e"
SERVER_IMAGE="kelson-server"

# The cluster-internal registry endpoint. It is install.RegistryEndpoint
# (internal/delivery/install/registry.go) spelled in bash; the Go suite reads
# the constant itself and asserts the controller was started with exactly this
# value, so a change on one side fails loudly rather than silently pushing
# somewhere else.
REGISTRY_NAME="kelson-registry"
REGISTRY_ENDPOINT="${REGISTRY_NAME}.${NAMESPACE}.svc.cluster.local:5000"

# How often Flux re-checks the pair. Shorter than the binary's 5m default
# because drift correction is the slowest path this suite ever waits on; it is
# not deploy latency, which arrives through the apply kelson-controller does.
RECONCILE_INTERVAL="1m"

check_docker
require_cmd kubectl "Install kubectl: https://kubernetes.io/docs/tasks/tools/#kubectl"
require_cmd helm "Install helm: https://helm.sh/docs/intro/install/"
install_kind

start_ts=$(date +%s)

# await polls a command until it succeeds. Every wait in this script is one of
# these: bounded, and loud about what it was waiting for when it gives up.
await() {
	local desc="$1" timeout="$2"
	shift 2
	local deadline=$((SECONDS + timeout))
	until "$@" >/dev/null 2>&1; do
		if ((SECONDS >= deadline)); then
			die "timed out after ${timeout}s waiting for ${desc} (last command: $*)"
		fi
		sleep 5
	done
}

log "== stage: cluster =="
"$SCRIPT_DIR/up.sh"
export KUBECONFIG="$KUBECONFIG_FILE"

log "== stage: build kelson =="
# `kelson install` is what installs Flux and the registry below, so the CLI is a
# prerequisite of the cluster rather than only of the assertions.
(cd "$E2E_ROOT" && "$GO" build -o "$KELSON" ./cmd/kelson) || die "go build ./cmd/kelson failed"

log "== stage: flux (kelson install flux) =="
# Already-installed is a refusal, not an error: `kelson install` never modifies a
# component it did not install, and prints why. So this is safe to re-run.
"$KELSON" install flux --yes --kubeconfig "$KUBECONFIG_FILE" ||
	die "kelson install flux failed"

await "the flux-operator deployment" 300 kubectl -n flux-system get deploy flux-operator
kubectl -n flux-system rollout status deploy/flux-operator --timeout=300s ||
	die "flux-operator never became available"
# The FluxInstance is what actually installs the controllers; the operator
# reconciles it asynchronously, so the CR's own Ready condition is the honest
# thing to wait on before looking for the deployments it creates.
kubectl -n flux-system wait --for=condition=Ready fluxinstance/flux --timeout=600s ||
	die "the FluxInstance never became Ready; flux-operator did not finish installing the Flux controllers"
for component in source-controller kustomize-controller; do
	await "the ${component} deployment" 300 kubectl -n flux-system get deploy "$component"
	kubectl -n flux-system rollout status "deploy/${component}" --timeout=300s ||
		die "${component} never became available"
done
# The controller detects Flux by API group, once, at start-up
# (internal/clusterprofile/detect): if these are not served before the chart is
# installed below, the controller comes up with no Flux watches and reports
# FluxNotInstalled until it is restarted. That ordering is the whole reason this
# stage precedes the chart stage.
await "the OCIRepository CRD to be served" 300 kubectl get crd ocirepositories.source.toolkit.fluxcd.io
await "the Kustomization CRD to be served" 300 kubectl get crd kustomizations.kustomize.toolkit.fluxcd.io
log "flux is installed and serving both kinds kelson writes"

log "== stage: registry (kelson install registry) =="
"$KELSON" install registry --yes --kubeconfig "$KUBECONFIG_FILE" ||
	die "kelson install registry failed"
await "the ${REGISTRY_NAME} deployment" 120 kubectl -n "$NAMESPACE" get deploy "$REGISTRY_NAME"
kubectl -n "$NAMESPACE" rollout status "deploy/${REGISTRY_NAME}" --timeout=300s ||
	die "the in-cluster registry never became available (a PersistentVolumeClaim needs a default StorageClass; kind ships local-path)"
log "the in-cluster registry is serving at ${REGISTRY_ENDPOINT}"

log "== stage: build and load the controller image =="
VERSION="$(git -C "$E2E_ROOT" describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)"
COMMIT="$(git -C "$E2E_ROOT" rev-parse --short HEAD 2>/dev/null || echo none)"
CTX="$(mktemp -d)"
trap 'rm -rf "$CTX"' EXIT
(cd "$E2E_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH="$("$GO" env GOARCH)" "$GO" build \
	-ldflags "-s -w -X github.com/dafrie/kelson/internal/version.Version=${VERSION} -X github.com/dafrie/kelson/internal/version.Commit=${COMMIT}" \
	-o "$CTX/kelson-controller" ./cmd/kelson-controller) ||
	die "go build ./cmd/kelson-controller failed"
# Dockerfile.controller expects the binary for the target platform beside it,
# which is what goreleaser gives it and what the line above just produced.
cp "$E2E_ROOT/Dockerfile.controller" "$CTX/Dockerfile"
docker build -q -t "${CONTROLLER_IMAGE}:${CONTROLLER_TAG}" "$CTX" >/dev/null ||
	die "docker build of ${CONTROLLER_IMAGE}:${CONTROLLER_TAG} failed"
"$KIND" load docker-image "${CONTROLLER_IMAGE}:${CONTROLLER_TAG}" --name "$CLUSTER_NAME" ||
	die "kind load of ${CONTROLLER_IMAGE}:${CONTROLLER_TAG} failed"

# The server image too, same pattern with the server's own Dockerfile. It used
# to be skipped (replicaCount=0) on the argument that the server is not what
# this harness proves — and then three chart-vs-server skews in one day (a flag
# the binary had dropped, two missing RBAC verbs) shipped green because the one
# pod that would have failed was never scheduled anywhere CI could see. The
# server runs here now, and the smoke in test/e2e exercises its write and watch
# paths against the rendered Role.
SERVER_CTX="$(mktemp -d)"
trap 'rm -rf "$CTX" "$SERVER_CTX"' EXIT
(cd "$E2E_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH="$("$GO" env GOARCH)" "$GO" build \
	-ldflags "-s -w -X github.com/dafrie/kelson/internal/version.Version=${VERSION} -X github.com/dafrie/kelson/internal/version.Commit=${COMMIT}" \
	-o "$SERVER_CTX/kelson-server" ./cmd/kelson-server) ||
	die "go build ./cmd/kelson-server failed"
cp "$E2E_ROOT/Dockerfile" "$SERVER_CTX/Dockerfile"
docker build -q -t "${SERVER_IMAGE}:${CONTROLLER_TAG}" "$SERVER_CTX" >/dev/null ||
	die "docker build of ${SERVER_IMAGE}:${CONTROLLER_TAG} failed"
"$KIND" load docker-image "${SERVER_IMAGE}:${CONTROLLER_TAG}" --name "$CLUSTER_NAME" ||
	die "kind load of ${SERVER_IMAGE}:${CONTROLLER_TAG} failed"

log "== stage: install the chart =="
# Values worth explaining, since each one is a deliberate deviation from what an
# operator would type:
#
#   auth.insecure=true  the chart refuses to render without an auth decision,
#                       and this cluster is a throwaway kind cluster.
#   replicaCount=1      the server pod runs here, from the image loaded above.
#                       It used to be 0, and every chart-vs-server skew (a
#                       dropped flag, a missing RBAC verb) shipped green
#                       because of it; the rollout wait below and the smoke in
#                       test/e2e are what catch that class now.
#   image.pullPolicy=Never   both images came from `kind load`, not from a
#                       registry, so a pull would fail.
#   controller.registry / insecureRegistries   the in-cluster registry, plain
#                       HTTP, named as insecure exactly once — kelson never
#                       downgrades a push it was not told about.
helm upgrade --install "$RELEASE" "$E2E_ROOT/deploy/chart/kelson" \
	--namespace "$NAMESPACE" --create-namespace \
	--set auth.insecure=true \
	--set image.repository="$SERVER_IMAGE" \
	--set image.tag="$CONTROLLER_TAG" \
	--set image.pullPolicy=Never \
	--set replicaCount=1 \
	--set controller.enabled=true \
	--set controller.image.repository="$CONTROLLER_IMAGE" \
	--set controller.registry="$REGISTRY_ENDPOINT" \
	--set "controller.insecureRegistries={${REGISTRY_ENDPOINT}}" \
	--set controller.reconcileInterval="$RECONCILE_INTERVAL" ||
	die "helm upgrade --install of deploy/chart/kelson failed"

# The tag never changes, so a Deployment that already ran keeps the previous
# image without this nudge — the same reason hack/local/up.sh restarts. It also
# guarantees the controller's one-shot start-up detection happens *after* the
# Flux stage above, which is what makes the ordering in this script load-bearing
# rather than incidental.
kubectl -n "$NAMESPACE" rollout restart "deploy/${RELEASE}-controller" >/dev/null ||
	die "could not restart the controller deployment"
kubectl -n "$NAMESPACE" rollout status "deploy/${RELEASE}-controller" --timeout=300s ||
	die "kelson-controller never became available"

# The server, same nudge and the wait that catches a crash-looping pod — a
# chart arg the binary refuses is exactly a rollout that never completes.
kubectl -n "$NAMESPACE" rollout restart "deploy/${RELEASE}" >/dev/null ||
	die "could not restart the server deployment"
kubectl -n "$NAMESPACE" rollout status "deploy/${RELEASE}" --timeout=300s ||
	die "kelson-server never became available — its logs are the first read: kubectl -n ${NAMESPACE} logs deploy/${RELEASE}"

elapsed=$(($(date +%s) - start_ts))
log "the delivery spine is provisioned in ${elapsed}s"
log ""
log "  export KUBECONFIG=${KUBECONFIG_FILE}"
log "  kubectl -n ${NAMESPACE} logs deploy/${RELEASE}-controller -f"
log ""
log "run the assertions with: make test-e2e"
