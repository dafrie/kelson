#!/usr/bin/env bash
# Tears down the local kelson cluster (docs/local.md). The registry and the
# Helm install live inside the cluster, so deleting it sweeps everything.
# Idempotent: does not error if the cluster is already gone.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../e2e/lib.sh
source "$SCRIPT_DIR/../e2e/lib.sh"

CLUSTER_NAME="kelson-local"
KUBECONFIG_FILE="$BIN_DIR/local.kubeconfig"

if [[ -x "$KIND" ]] && cluster_exists; then
	log "deleting kind cluster '${CLUSTER_NAME}'"
	"$KIND" delete cluster --name "$CLUSTER_NAME"
else
	log "kind cluster '${CLUSTER_NAME}' does not exist; nothing to do"
fi

rm -f "$KUBECONFIG_FILE"
