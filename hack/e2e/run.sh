#!/usr/bin/env bash
# Drives the full kind-based E2E lifecycle (issue #86) against
# examples/hello-e2e: cluster -> build -> profile -> render -> deploy ->
# induce a CrashLoopBackOff -> status -> rollback -> verify restored.
#
# Every stage asserts and exits non-zero with a clear message on failure.
# The deploy/status/rollback stage needs `kelson deploy/status/rollback`
# (issue #135); when the binary predates that CLI wiring, this script fails
# fast with an explicit message instead of silently skipping assertions.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
source "$SCRIPT_DIR/lib.sh"

EXAMPLE_DIR="$E2E_ROOT/examples/hello-e2e"
PROJECT_FILE="$EXAMPLE_DIR/project.yaml"
ENV_FILE="$EXAMPLE_DIR/e2e.yaml"
ENV_NAME="e2e"
PROJECT_NAME="hello-e2e"
APP_NAME="web"
SELECTOR="kelson.dev/project=${PROJECT_NAME},kelson.dev/application=${APP_NAME}"
# NAMESPACE is derived from the rendered manifests once render has run (below)
# rather than hardcoded, so it can't drift from what the renderer actually
# targets.
NAMESPACE=""

WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

start_ts=$(date +%s)

# wait_for polls a kubectl jsonpath query until it contains a substring, or
# gives up after retries*interval_s seconds. Used where a guaranteed CLI
# exit code isn't enough and we also want to see the cluster agree.
wait_for() {
	local description="$1" jsonpath="$2" want="$3" retries="$4" interval_s="$5"
	local seen=""
	for _ in $(seq 1 "$retries"); do
		seen=$(kubectl -n "$NAMESPACE" get pods -l "$SELECTOR" -o jsonpath="$jsonpath" 2>/dev/null || true)
		[[ "$seen" == *"$want"* ]] && return 0
		sleep "$interval_s"
	done
	die "timed out waiting for ${description} (last observed: '${seen}')"
}

log "== stage: cluster =="
"$SCRIPT_DIR/up.sh"
export KUBECONFIG="$KUBECONFIG_FILE"

log "== stage: build kelson =="
(cd "$E2E_ROOT" && "$GO" build -o "$KELSON" ./cmd/kelson) || die "go build ./cmd/kelson failed"

log "== stage: profile + render =="
"$KELSON" profile --kubeconfig "$KUBECONFIG_FILE" >"$WORKDIR/cluster-profile.yaml" ||
	die "kelson profile failed against the kind cluster"
"$KELSON" render -f "$PROJECT_FILE" -f "$ENV_FILE" --env "$ENV_NAME" \
	--profile "$WORKDIR/cluster-profile.yaml" >"$WORKDIR/rendered.yaml" ||
	die "kelson render failed for examples/hello-e2e"
grep -q '^kind: Deployment$' "$WORKDIR/rendered.yaml" || die "render did not produce a Deployment for hello-e2e/web"
grep -q '^kind: Service$' "$WORKDIR/rendered.yaml" || die "render did not produce a Service for hello-e2e/web"
log "render OK: $(grep -c '^kind:' "$WORKDIR/rendered.yaml") manifests"

NAMESPACE="$(grep -m1 'namespace:' "$WORKDIR/rendered.yaml" | sed -E 's/^[[:space:]]*namespace:[[:space:]]*//')"
[[ -n "$NAMESPACE" ]] || die "could not determine the target namespace from the rendered manifests"
log "target namespace: ${NAMESPACE}"

log "== stage: probe for deploy/status/rollback (issue #135) =="
if ! "$KELSON" deploy --help >/dev/null 2>&1; then
	die "kelson binary lacks 'deploy' — build from a branch containing #135 (the CLI wiring for deploy/status/rollback). Cluster provisioning, profile capture and render all passed; the deploy/status/rollback lifecycle cannot run until that command exists."
fi

# TODO(#150): the renderer targets namespace "<project>-<environment>" but
# never emits a Namespace manifest, so `kelson deploy` server-side-applies
# into a namespace that doesn't exist yet and fails. Remove this stage once
# the renderer emits the Namespace itself. Idempotent, and covers the broken
# revision and rollback below too — they deploy into the same namespace.
log "== stage: ensure namespace (workaround for #150) =="
kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f - >/dev/null ||
	die "failed to ensure namespace ${NAMESPACE} exists"

log "== stage: deploy (good revision) =="
if ! "$KELSON" deploy -f "$PROJECT_FILE" -f "$ENV_FILE" --env "$ENV_NAME" \
	--profile from-cluster --kubeconfig "$KUBECONFIG_FILE" --timeout 120s --yes; then
	die "kelson deploy did not reach healthy for the good revision (non-zero exit; deploy's contract is exit 0 only on healthy)"
fi
log "deploy exited 0"

kubectl -n "$NAMESPACE" rollout status deployment/"$APP_NAME" --timeout=60s ||
	die "kubectl does not agree the Deployment is rolled out after a successful deploy"
wait_for "a ready ${APP_NAME} pod" '{.items[*].status.containerStatuses[*].ready}' "true" 1 0
log "kubectl confirms ${APP_NAME} is running and ready in ${NAMESPACE}"

log "== stage: induce CrashLoopBackOff =="
# whoami exits 2 on an unrecognised flag (verified locally: `docker run
# traefik/whoami --this-flag-does-not-exist` -> "flag provided but not
# defined", exit 2), so overriding the command is a deterministic,
# architecture-independent way to crash-loop it without a broken image tag,
# which would produce ImagePullBackOff instead of the CrashLoopBackOff verdict
# this stage needs.
cat >"$WORKDIR/broken-project.yaml" <<'EOF'
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata:
  name: hello-e2e

spec:
  image: docker.io/traefik/whoami:v1.12.0

  applications:
    - name: web
      port: 80
      health: /healthz
      command: ["/whoami", "--this-flag-does-not-exist"]
EOF

if "$KELSON" deploy -f "$WORKDIR/broken-project.yaml" -f "$ENV_FILE" --env "$ENV_NAME" \
	--profile from-cluster --kubeconfig "$KUBECONFIG_FILE" --timeout 90s --yes; then
	die "kelson deploy exited 0 for a revision that crash-loops and should never report healthy"
fi
log "deploy correctly refused to report healthy for the broken revision"

wait_for "CrashLoopBackOff" '{.items[*].status.containerStatuses[*].state.waiting.reason}' "CrashLoopBackOff" 12 5
log "kubectl confirms CrashLoopBackOff on ${SELECTOR}"

log "== stage: kelson status names the failure =="
status_out=$("$KELSON" status -f "$WORKDIR/broken-project.yaml" -f "$ENV_FILE" --env "$ENV_NAME" --kubeconfig "$KUBECONFIG_FILE" 2>&1 || true)
printf '%s\n' "$status_out"
# Assert on kelson's own stable verdict slug, not the kubelet's raw reason:
# mid-crash-cycle the containerStatus reason briefly reads "Error" instead of
# "CrashLoopBackOff", and which one status samples is a race we don't control.
grep -q "crash-loop-back-off" <<<"$status_out" || die "kelson status did not print the crash-loop-back-off verdict"
log "kelson status verdict confirmed"

log "== stage: rollback =="
if ! "$KELSON" rollback -f "$PROJECT_FILE" -f "$ENV_FILE" --env "$ENV_NAME" --kubeconfig "$KUBECONFIG_FILE" --yes; then
	die "kelson rollback exited non-zero (rollback's contract is exit 0 on a successful restore with --yes)"
fi
log "rollback exited 0"

kubectl -n "$NAMESPACE" rollout status deployment/"$APP_NAME" --timeout=60s ||
	die "kubectl does not agree the Deployment rolled out after rollback"

command_after=$(kubectl -n "$NAMESPACE" get deployment "$APP_NAME" -o jsonpath='{.spec.template.spec.containers[0].command}')
if [[ -n "$command_after" && "$command_after" != "[]" ]]; then
	die "rollback did not restore the original container command (found: ${command_after})"
fi

# The broken revision's pod may still be terminating when we get here; it
# matches the selector but says nothing about the restored revision. Consider
# only pods that are not being deleted, and give the terminating one time to go.
ready_after=""
for _ in $(seq 1 12); do
	ready_after=$(kubectl -n "$NAMESPACE" get pods -l "$SELECTOR" \
		-o go-template='{{range .items}}{{if not .metadata.deletionTimestamp}}{{range .status.containerStatuses}}{{.ready}} {{end}}{{end}}{{end}}' 2>/dev/null || true)
	if [[ "$ready_after" == *"true"* && "$ready_after" != *"false"* ]]; then
		break
	fi
	sleep 5
done
[[ "$ready_after" == *"true"* && "$ready_after" != *"false"* ]] ||
	die "not every surviving ${APP_NAME} pod is ready after rollback (found: ${ready_after:-none})"
log "kubectl confirms the original workload is restored and healthy"

elapsed=$(($(date +%s) - start_ts))
log "full lifecycle completed in ${elapsed}s"
