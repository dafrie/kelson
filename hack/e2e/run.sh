#!/usr/bin/env bash
# Drives the kind-based E2E lifecycle (issue #86) against examples/hello-e2e:
# cluster -> build -> profile -> render -> apply -> induce a CrashLoopBackOff
# -> status -> verify the verdict.
#
# Every stage asserts and exits non-zero with a clear message on failure.
#
# The deploy and rollback stages are gone, not skipped. `kelson deploy` and
# `kelson rollback` are ConnectRPC clients of kelson-server now (R2, issue
# #225): they write a spec / a rollback annotation and follow
# kelson-controller's status, and this harness stands up neither a
# kelson-server nor a controller — a bare kind cluster is the whole of what
# up.sh creates. So this script asserts the honest thing a façade-backed verb
# does with no reachable --server (a clear, fast refusal naming the flag —
# never a hang, never a silent no-op) and applies the rendered set with
# kubectl instead, which is the property ADR-0028 decision 10 relies on and
# the same set the controller will publish for Flux. The real deploy/rollback
# round trip, against a live kelson-server and controller, is
# hack/e2e/spine.sh.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
source "$SCRIPT_DIR/lib.sh"

EXAMPLE_DIR="$E2E_ROOT/examples/hello-e2e"
PROJECT_FILE="$EXAMPLE_DIR/project.yaml"
ENV_FILE="$EXAMPLE_DIR/e2e.yaml"
ENV_NAME="e2e"
PROJECT_NAME="hello-e2e"
COMPONENT_NAME="web"
SELECTOR="kelson.dev/project=${PROJECT_NAME},kelson.dev/component=${COMPONENT_NAME}"
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
# Without the Namespace (issue #150) the apply stage below fails on its first
# resource against a fresh cluster, so assert it rather than pre-creating it.
grep -q '^kind: Namespace$' "$WORKDIR/rendered.yaml" || die "render did not produce a Namespace for hello-e2e"
grep -q '^kind: Deployment$' "$WORKDIR/rendered.yaml" || die "render did not produce a Deployment for hello-e2e/web"
grep -q '^kind: Service$' "$WORKDIR/rendered.yaml" || die "render did not produce a Service for hello-e2e/web"
log "render OK: $(grep -c '^kind:' "$WORKDIR/rendered.yaml") manifests"

NAMESPACE="$(grep -m1 'namespace:' "$WORKDIR/rendered.yaml" | sed -E 's/^[[:space:]]*namespace:[[:space:]]*//')"
[[ -n "$NAMESPACE" ]] || die "could not determine the target namespace from the rendered manifests"
log "target namespace: ${NAMESPACE}"

log "== stage: deploy/rollback fail cleanly with no reachable kelson-server =="
# kelson deploy and kelson rollback are ConnectRPC clients of kelson-server now
# (R2, issue #225) and this harness runs none — up.sh stands up a bare kind
# cluster only. So the property worth pinning here, against the real binary,
# is that a façade-backed verb with no reachable --server fails fast and names
# the flag: never a hang, never a silent no-op. The real deploy/rollback round
# trip, against a live kelson-server and controller, is hack/e2e/spine.sh.
UNREACHABLE_SERVER="http://127.0.0.1:1"
for verb in deploy rollback; do
	verb_start_ts=$(date +%s)
	if refusal=$("$KELSON" "$verb" -f "$PROJECT_FILE" -f "$ENV_FILE" --env "$ENV_NAME" \
		--server "$UNREACHABLE_SERVER" --yes 2>&1); then
		die "kelson ${verb} exited 0 against an unreachable --server"
	fi
	verb_elapsed=$(($(date +%s) - verb_start_ts))
	[[ "$verb_elapsed" -le 15 ]] ||
		die "kelson ${verb} took ${verb_elapsed}s to fail against an unreachable --server; it must fail fast, not hang"
	grep -q -- "--server" <<<"$refusal" ||
		die "kelson ${verb} did not name --server in its refusal: ${refusal}"
	grep -q "KELSON_SERVER" <<<"$refusal" ||
		die "kelson ${verb} did not name \$KELSON_SERVER in its refusal: ${refusal}"
done
log "deploy and rollback fail cleanly, naming --server, with no reachable kelson-server"

log "== stage: apply the rendered set =="
kubectl apply -f "$WORKDIR/rendered.yaml" ||
	die "kubectl could not apply the rendered set; a kelson render must be plain, applicable manifests"
log "kubectl applied the rendered set"

kubectl -n "$NAMESPACE" rollout status deployment/"$COMPONENT_NAME" --timeout=120s ||
	die "kubectl does not agree the Deployment is rolled out after applying the rendered set"
wait_for "a ready ${COMPONENT_NAME} pod" '{.items[*].status.containerStatuses[*].ready}' "true" 1 0
log "kubectl confirms ${COMPONENT_NAME} is running and ready in ${NAMESPACE}"

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

"$KELSON" render -f "$WORKDIR/broken-project.yaml" -f "$ENV_FILE" --env "$ENV_NAME" \
	--profile from-cluster --kubeconfig "$KUBECONFIG_FILE" >"$WORKDIR/broken.yaml" ||
	die "kelson render failed for the broken revision"
kubectl apply -f "$WORKDIR/broken.yaml" || die "kubectl could not apply the broken revision"
log "the broken revision is applied"

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

log "== stage: restore the good revision =="
# `kelson rollback` would do this against a real kelson-server and
# controller, neither of which this harness runs, so the restore is a
# re-apply of the good render — which proves the cluster recovers but NOT the
# property rollback exists for, that the artifact restored is bit-identical to
# the one that was live. That round trip is hack/e2e/spine.sh.
kubectl apply -f "$WORKDIR/rendered.yaml" || die "kubectl could not re-apply the good revision"

kubectl -n "$NAMESPACE" rollout status deployment/"$COMPONENT_NAME" --timeout=120s ||
	die "kubectl does not agree the Deployment rolled out after re-applying the good revision"

command_after=$(kubectl -n "$NAMESPACE" get deployment "$COMPONENT_NAME" -o jsonpath='{.spec.template.spec.containers[0].command}')
if [[ -n "$command_after" && "$command_after" != "[]" ]]; then
	die "the restore did not clear the broken container command (found: ${command_after})"
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
	die "not every surviving ${COMPONENT_NAME} pod is ready after the restore (found: ${ready_after:-none})"
log "kubectl confirms the original workload is restored and healthy"

elapsed=$(($(date +%s) - start_ts))
log "full lifecycle completed in ${elapsed}s"
