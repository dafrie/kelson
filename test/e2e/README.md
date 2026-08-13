# The end-to-end suite

A Go test package that drives the real `kelson` binary against a real Kubernetes API server
(issue [#86](https://github.com/dafrie/kelson/issues/86)). Golden files cover the renderer; this
covers what golden files cannot — the delivery adapters, status correlation, rollback, and the
cluster's own answer about what is live.

It runs in CI on `kind` (`.github/workflows/e2e.yml`), and identically on a laptop.

## What runs

Two scenarios, both against `testdata/minimal.yaml` — one service component, one port, no routing,
no build, no data services. Everything it needs exists on a stock single-node kind cluster.

**`TestDeployLifecycle`** — the delivery spine, end to end:

1. `kelson deploy` the initial revision; wait until the Deployment reports the expected image on a
   ready pod (asserted through `kubectl`, not through kelson's own status).
2. `kelson status` names the adapter, the delivery phase (`Healthy`) and the observation verdict
   (`Deployment/kelson-e2e/web healthy`). Both halves are asserted: a phase without a verdict is the
   conflation [#53](https://github.com/dafrie/kelson/issues/53) exists to prevent.
3. Mutate the spec's image tag.
4. `kelson diff --output json` reports the image change — twice: offline against `--from` (L1) and
   server-side dry-run against the live cluster (L2). Both must exit 2, the "changes present" half of
   the CI exit contract ([#46](https://github.com/dafrie/kelson/issues/46)).
5. `kelson deploy` the mutated revision; wait for the new image to be live.
6. `kelson rollback` (no `--to`, so the target is revision 1) and then verify **the cluster actually
   reverted** — the Deployment runs the original image again, while the spec file on disk still says
   otherwise. That is the assertion the scenario exists for: rollback replays recorded bytes rather
   than re-rendering ([#38](https://github.com/dafrie/kelson/issues/38)), and only the cluster can
   confirm it restored anything.

**`TestDeleteByLabelIsExactlyTheRenderedSet`** — the non-destructive uninstall claim
([#59](https://github.com/dafrie/kelson/issues/59)), scoped to what is provable today. There is no
`kelson uninstall` command; building one is #59's feature work. What this asserts instead is the
property such a command would depend on:

1. The set selected by `kelson.dev/project=<project>` is **exactly** the set the renderer produced —
   nothing rendered went unlabelled (it would survive an uninstall), nothing extra got labelled (it
   would be deleted by one).
2. Deleting by that selector removes exactly that set and leaves the rest of the namespace alone: an
   unlabelled bystander, a resource labelled for a different project, and the namespace's own
   furniture (`kube-root-ca.crt`, the `default` ServiceAccount).
3. The Namespace survives and still carries `kelson.dev/namespace-ownership`. That annotation is
   why deleting the namespace is not the uninstall path: it records that kelson *declared* the
   namespace, not that it created it (`internal/renderer/namespace.go`).

The two scenarios use different environments (`e2e`, `additivity`) and therefore different
namespaces, so the one that deletes things can never race the one that does not.

## Running it locally

```sh
make test-e2e          # creates the kind cluster if needed, then runs the suite
make e2e-down          # delete the cluster when done
```

Or against any cluster you already have:

```sh
KELSON_E2E=1 go test -tags e2e -v -timeout 15m ./test/e2e/...
```

Environment variables:

| Variable | Meaning |
|---|---|
| `KELSON_E2E` | Must be `1`. Without it every test skips. |
| `KELSON_E2E_BIN` | A prebuilt `kelson` binary. Unset: the suite builds one itself. |
| `KELSON_E2E_KUBECONFIG` | Kubeconfig for this suite only. Unset: the ambient `KUBECONFIG`. |

The suite leaves its namespaces in place so a failure can be inspected with `kubectl`.

## Flake posture

- **Two gates, so a laptop never trips it.** The `e2e` build tag keeps the package out of
  `go test ./...` entirely, and `KELSON_E2E=1` is required on top. Setting that variable is a promise
  that a cluster is reachable — from there an unreachable API server is a failure, never a skip,
  because a harness that silently skips is a harness that silently stops proving anything.
- **No bare sleeps.** Every wait is a bounded poll that prints what it last observed when its
  deadline expires, so a timeout in a CI log says what the cluster actually held.
- **Every command is echoed** with its exit code and both output streams; a failing test dumps its
  namespace (resources, `describe pods`, events) before returning, and the workflow uploads
  `kubectl cluster-info dump` plus `kind export logs` as an artifact on failure.
- **`registry.k8s.io/pause`** is the workload: unauthenticated, no Docker Hub anonymous rate limit,
  a few hundred kilobytes, and usually already in the kind node's image store. The component declares
  no `health:` path — the renderer emits probes only when a component has both `port` and `health`,
  so "ready" is a fact about the delivery spine rather than about a probe endpoint's timing.
- **The workflow is not a required check yet.** It is signal while it stabilises; promote it in
  repository settings once it has been green for a while.

## Deliberately not covered yet

- **Builds.** `kelson build` needs a registry the cluster can pull from, and there is none here. A
  build scenario would push to a cluster-internal registry, assert the printed reference is
  digest-pinned and assert the build pod ran unprivileged — follow-up
  [#117](https://github.com/dafrie/kelson/issues/117), which names this harness as its successor.
- **Policy.** No Kyverno, Gatekeeper or ValidatingAdmissionPolicy is installed, so the live-policy
  case of [#45](https://github.com/dafrie/kelson/issues/45) — a server-side dry-run rejected by a
  real admission webhook, attributed to the engine that rejected it — cannot run here yet. The L2
  dry-run in `TestDeployLifecycle` exercises the mechanism, not the rejection.
- **Cluster-shape matrix.** Gateway API, cert-manager, Flux and Argo shapes are what issue #86's
  scope calls for beyond this first slice. Per [ADR-0003](../../docs/adr/0003-install-model.md) the
  combinatorial space is handled with `ClusterProfile` fixtures rather than real clusters, so this
  needs only a small number of representative shapes — none of which exist here yet. The Flux
  adapter in particular is untested against a real reconciler.
- **`kelson status --output json`.** There is no JSON output on `status` today, so this suite asserts
  its stable text lines. `kelson diff --output json` is asserted structurally, decoded into
  `internal/diff`'s own types.
- **Failure paths.** `hack/e2e/run.sh` still owns the induced-`CrashLoopBackOff` scenario against
  `examples/hello-e2e`; it is not part of the CI job yet.
