# The end-to-end suite

A Go test package that drives the real `kelson` binary against a real Kubernetes API server
(issue [#86](https://github.com/dafrie/kelson/issues/86)). Golden files cover the renderer; this
covers what golden files cannot — the delivery adapters, status correlation, rollback, and the
cluster's own answer about what is live.

It runs in CI on `kind` (`.github/workflows/e2e.yml`), and identically on a laptop.

## What runs

Five scenarios, all against `testdata/minimal.yaml` — one service component, one port, no routing,
no build, no data services. Everything it needs exists on a stock single-node kind cluster. Each
scenario uses its own environment and therefore its own namespace, so the ones that delete things
can never race the ones that do not.

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

**`TestDeleteByLabelIsExactlyTheRenderedSet`** — the property `kelson uninstall` is built on
([#59](https://github.com/dafrie/kelson/issues/59)), proved with kubectl as the deleting tool so it
holds independently of the command:

1. The set selected by `kelson.dev/project=<project>` is **exactly** the set the renderer produced —
   nothing rendered went unlabelled (it would survive an uninstall), nothing extra got labelled (it
   would be deleted by one).
2. Deleting by that selector removes exactly that set and leaves the rest of the namespace alone: an
   unlabelled bystander, a resource labelled for a different project, and the namespace's own
   furniture (`kube-root-ca.crt`, the `default` ServiceAccount).
3. The Namespace survives that delete and carries `kelson.dev/namespace-ownership`, which is what
   makes deleting it a separate decision. The renderer stamps `declared` and claims nothing about
   authorship (`internal/renderer/namespace.go`); the delivery plane resolves it to `created` or
   `adopted` at apply time.

**`TestUninstallRemovesExactlyWhatKelsonDeployed`** — the same claim as the verb, in the case where
kelson created the namespace:

1. The deploy recorded `kelson.dev/namespace-ownership=created` — asserted *before* the uninstall,
   so the namespace assertion below cannot pass for the wrong reason.
2. Without `--yes` and with no terminal, it prints the preview, deletes nothing and exits non-zero.
3. With `--yes` it previews, deletes and reports per object; no bystander appears on a `deleted`
   line.
4. Every rendered object is gone, and the namespace with it.
5. A second run reports `nothing to do` and exits 0.

**`TestUninstallLeavesAnAdoptedNamespaceAndItsBystanders`** — the half that carries the
non-destructive claim. The test creates the namespace *before* the deploy, so kelson records
`adopted`. After `kelson uninstall`: everything kelson labelled is gone, the namespace is still
`Active`, and the unlabelled bystander, the other project's labelled object and the namespace's own
furniture are all still there. It also asserts the local rendered history for the environment was
removed.

**`TestUninstallLeavesACreatedNamespaceAnotherProjectMovedInto`**
([#215](https://github.com/dafrie/kelson/issues/215)) — the namespace kelson *created*, shared. The
deploy records `created`, and the test then plants a resource carrying kelson's full provenance for
another `(project, environment)` — which an overridable `spec.namespace` makes possible. The
uninstall must delete its own resources, leave the namespace `Active` with the other project's
resource in it, and say whose resources it is standing for. It clears its namespace before deploying,
because it is the one scenario that deliberately leaves one behind.

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
