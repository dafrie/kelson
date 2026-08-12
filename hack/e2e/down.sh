#!/usr/bin/env bash
# Tears down the kind cluster the E2E harness (issue #86) creates.
# Idempotent: does not error if the cluster is already gone.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
source "$SCRIPT_DIR/lib.sh"

if [[ -x "$KIND" ]] && cluster_exists; then
	log "deleting kind cluster '${CLUSTER_NAME}'"
	"$KIND" delete cluster --name "$CLUSTER_NAME"
else
	log "kind cluster '${CLUSTER_NAME}' does not exist; nothing to do"
fi

rm -f "$KUBECONFIG_FILE"
