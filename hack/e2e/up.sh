#!/usr/bin/env bash
# Creates the kind cluster the E2E harness (issue #86) runs against.
# Idempotent: re-running with the cluster already up is a no-op beyond
# refreshing the kubeconfig.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
source "$SCRIPT_DIR/lib.sh"

check_docker
require_cmd kubectl "Install kubectl: https://kubernetes.io/docs/tasks/tools/#kubectl"
install_kind

if cluster_exists; then
	log "kind cluster '${CLUSTER_NAME}' already exists; reusing it"
else
	log "creating kind cluster '${CLUSTER_NAME}' (kind ${KIND_VERSION}, node image ${NODE_IMAGE})"
	"$KIND" create cluster \
		--name "$CLUSTER_NAME" \
		--image "$NODE_IMAGE" \
		--wait 120s \
		--kubeconfig "$KUBECONFIG_FILE"
fi

# Always refresh: covers both the fresh-create path and a pre-existing
# cluster whose kubeconfig file this harness hasn't written yet.
"$KIND" export kubeconfig --name "$CLUSTER_NAME" --kubeconfig "$KUBECONFIG_FILE"

log "cluster ready — KUBECONFIG=${KUBECONFIG_FILE}"
