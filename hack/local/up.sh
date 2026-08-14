#!/usr/bin/env bash
# Stands up a complete local kelson on kind (docs/local.md): a cluster, an
# in-cluster image registry the build plane can push to, and a Helm-installed
# kelson-server carrying the web UI. Idempotent: re-running converges, and
# rebuilds + redeploys the dev image so the running server is the checkout.
#
# Everything lives inside the cluster on purpose — `down.sh` (or a plain
# `kind delete cluster`) sweeps all of it, and there is no sidecar container
# whose lifecycle this script would otherwise have to manage.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../e2e/lib.sh
source "$SCRIPT_DIR/../e2e/lib.sh"

# The e2e harness owns 'kelson-e2e' and tears it down freely; this cluster is
# for clicking around and lives until you delete it, so the two never share.
CLUSTER_NAME="kelson-local"
KUBECONFIG_FILE="$BIN_DIR/local.kubeconfig"

NAMESPACE="kelson-system"
IMAGE="kelson-server:dev"

# One registry name that works from every vantage point:
#   - build pods (BuildKit, the CNB lifecycle) push to it through CoreDNS,
#     because it is an ordinary Service FQDN;
#   - each node's containerd pulls from it through the hosts.toml written
#     below, which maps the name onto the NodePort on loopback.
# The name appears in image pins, so it must be one both resolvers agree on —
# that is why it is the FQDN and not a short name or a docker-network alias.
REGISTRY_HOST="kelson-registry.${NAMESPACE}.svc.cluster.local:5000"
REGISTRY_NODEPORT=30500

check_docker
require_cmd kubectl "Install kubectl: https://kubernetes.io/docs/tasks/tools/#kubectl"
require_cmd helm "Install helm: https://helm.sh/docs/intro/install/"
require_cmd node "Install Node 22+ (builds the web UI): https://nodejs.org"
install_kind

log "== cluster =="
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
"$KIND" export kubeconfig --name "$CLUSTER_NAME" --kubeconfig "$KUBECONFIG_FILE"
export KUBECONFIG="$KUBECONFIG_FILE"

log "== registry (in-cluster, plain HTTP) =="
kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl -n "$NAMESPACE" apply -f - >/dev/null <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: kelson-registry
  labels: {app: kelson-registry}
spec:
  replicas: 1
  selector:
    matchLabels: {app: kelson-registry}
  template:
    metadata:
      labels: {app: kelson-registry}
    spec:
      containers:
        - name: registry
          image: registry:2
          ports: [{containerPort: 5000}]
          # emptyDir: images vanish with the pod. Fine here — a local build is
          # one 'kelson build' away, and the alternative is a PV this script
          # would have to manage.
          volumeMounts: [{name: data, mountPath: /var/lib/registry}]
      volumes:
        - name: data
          emptyDir: {}
---
apiVersion: v1
kind: Service
metadata:
  name: kelson-registry
  labels: {app: kelson-registry}
spec:
  type: NodePort
  selector: {app: kelson-registry}
  ports:
    - port: 5000
      targetPort: 5000
      nodePort: ${REGISTRY_NODEPORT}
EOF

# containerd on each node cannot resolve Service DNS, so give it the mapping
# by hand: the registry's Service name, served over plain HTTP, reachable at
# the NodePort on loopback. Written every run — it is idempotent and a new
# node (recreated cluster) needs it again.
for node in $("$KIND" get nodes --name "$CLUSTER_NAME"); do
	docker exec "$node" mkdir -p "/etc/containerd/certs.d/${REGISTRY_HOST}"
	docker exec -i "$node" sh -c "cat > '/etc/containerd/certs.d/${REGISTRY_HOST}/hosts.toml'" <<EOF
[host."http://127.0.0.1:${REGISTRY_NODEPORT}"]
  capabilities = ["pull", "resolve"]
EOF
done

log "== build the dev image (web UI + kelson-server) =="
(cd "$E2E_ROOT" && make ui)
VERSION="$(git -C "$E2E_ROOT" describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)"
COMMIT="$(git -C "$E2E_ROOT" rev-parse --short HEAD 2>/dev/null || echo none)"
CTX="$(mktemp -d)"
trap 'rm -rf "$CTX"' EXIT
(cd "$E2E_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH="$("$GO" env GOARCH)" "$GO" build \
	-ldflags "-s -w -X github.com/dafrie/kelson/internal/version.Version=${VERSION} -X github.com/dafrie/kelson/internal/version.Commit=${COMMIT}" \
	-o "$CTX/kelson-server" ./cmd/kelson-server)
cp "$E2E_ROOT/Dockerfile" "$CTX/"
docker build -q -t "$IMAGE" "$CTX" >/dev/null
"$KIND" load docker-image "$IMAGE" --name "$CLUSTER_NAME"

log "== install kelson =="
if ! kubectl -n "$NAMESPACE" get secret kelson-auth >/dev/null 2>&1; then
	kubectl -n "$NAMESPACE" create secret generic kelson-auth \
		--from-literal=password="$(head -c 18 /dev/urandom | base64 | tr '+/' '-_')"
fi

helm upgrade --install kelson "$E2E_ROOT/deploy/chart/kelson" \
	--namespace "$NAMESPACE" \
	--set image.repository=kelson-server \
	--set image.tag=dev \
	--set image.pullPolicy=Never \
	--set auth.existingSecret.name=kelson-auth \
	--set server.registry="${REGISTRY_HOST}/kelson" \
	--set "server.insecureRegistries={${REGISTRY_HOST}}" \
	>/dev/null

# The tag is always 'dev', so a Deployment that already ran keeps its old
# image without this nudge.
kubectl -n "$NAMESPACE" rollout restart deploy/kelson >/dev/null
kubectl -n "$NAMESPACE" rollout status deploy/kelson-registry --timeout=120s
kubectl -n "$NAMESPACE" rollout status deploy/kelson --timeout=180s

PASSWORD="$(kubectl -n "$NAMESPACE" get secret kelson-auth -o jsonpath='{.data.password}' | base64 -d)"
log "kelson is running"
log ""
log "  export KUBECONFIG=${KUBECONFIG_FILE}"
log "  kubectl -n ${NAMESPACE} port-forward svc/kelson 8420:8420"
log ""
log "  then open http://127.0.0.1:8420 and log in with: ${PASSWORD}"
log ""
log "builds push to ${REGISTRY_HOST} inside the cluster; tear everything down with 'make kind-down'"
