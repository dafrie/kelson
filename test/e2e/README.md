# The end-to-end suite

A Go test package that drives the real `kelson` binary against a real Kubernetes API server
(issue [#86](https://github.com/dafrie/kelson/issues/86)). Golden files cover the renderer; this
covers what golden files cannot — the delivery adapters, status correlation, rollback, and the
cluster's own answer about what is live.

It runs in CI on `kind` (`.github/workflows/e2e.yml`), and identically on a laptop.

## What runs

Six scenarios. Five drive the CLI against `testdata/minimal.yaml` — one service component, one port,
no routing, no build, no data services — and the sixth, the delivery spine, applies
`testdata/spine.yaml` as custom resources and lets the controller do the rest. Everything they need
exists on a stock single-node kind cluster, except the spine's three prerequisites (below). Each
scenario uses its own environment and therefore its own namespace, so the ones that delete things
can never race the ones that do not.

**`TestDeliverySpine`** ([ADR-0028](../../docs/adr/0028-delivery-spine.md), the R1 exit gate) — the
only scenario that applies nothing but a `Project` and an `Environment`:

1. Both custom resources are applied; the `Environment` reaches `Ready`/`Healthy` for its generation
   and publishes revision 1.
2. The registry holds the tag `<generation>-<spec-hash-short>`, and the artifact's OCI manifest
   carries the Flux media types plus five provenance annotations — whose `kelson.dev/generation` and
   `kelson.dev/spec-hash` must **compose the tag it was fetched by**, so nothing is compared with
   itself.
3. The `OCIRepository` + `Kustomization` pair exists in `kelson-system` with the four provenance
   labels of `internal/delivery/provenance.go`, pinned to that tag, `prune: true`, `wait: true`,
   `path: ./`, `targetNamespace` the environment's own namespace.
4. The workload is live in that namespace, on the image the spec names.
5. A spec edit publishes revision 2; `status.history` holds both, newest first, with digests.
6. `kelson.dev/rollback-to=<revision 1>` moves the pointer and **the cluster actually reverts** —
   the Deployment runs the older image again while the spec still says otherwise — with
   `Ready=RolledBack`, `Progressing=False/RollbackPinned`, and no new history entry, because a
   rollback publishes nothing.
7. Nothing is republished while the pin is in force, observed over a quiet window.
8. A spec edit under the pin publishes revision 3 (new intent wins), the standing annotation goes
   inert without re-pinning, and removing the annotation makes revision 3 live — see the note in
   [docs/e2e.md](../../docs/e2e.md#note-why-step-8-asserts-the-registry-not-the-pointer).

It needs Flux, an in-cluster registry and kelson-controller, which `hack/e2e/spine.sh` installs and
`make test-e2e` runs first. Missing prerequisites are a loud failure naming that script, never a
skip: a skipped exit gate is an exit gate that stopped existing.

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
make test-e2e          # creates the cluster and the spine if needed, then runs the suite
make e2e-down          # delete the cluster when done
```

Or against any cluster you already have — which for `TestDeliverySpine` means one that already has
Flux, the in-cluster registry and kelson-controller on it (`hack/e2e/spine.sh` is what puts them
there, and it is idempotent):

```sh
KELSON_E2E=1 go test -tags e2e -v -timeout 25m ./test/e2e/...
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
  `kubectl cluster-info dump` plus `kind export logs` as an artifact on failure. `TestDeliverySpine`
  dumps more, because it spans more: the custom resources, the Flux pair, the workload namespace, and
  the logs of kelson-controller, source-controller and kustomize-controller. Every observation it
  polls carries the whole `Environment` status on one line, so a timeout reads as a sequence rather
  than as a single frame.
- **`registry.k8s.io/pause`** is the workload: unauthenticated, no Docker Hub anonymous rate limit,
  a few hundred kilobytes, and usually already in the kind node's image store. The component declares
  no `health:` path — the renderer emits probes only when a component has both `port` and `health`,
  so "ready" is a fact about the delivery spine rather than about a probe endpoint's timing.
- **The workflow is not a required check yet.** It is signal while it stabilises; promote it in
  repository settings once it has been green for a while.

## Deliberately not covered yet

- **Builds.** The spine stage now puts a registry in the cluster (`kelson install registry`), so the
  missing half is the build itself: a scenario would push through BuildKit, assert the printed
  reference is digest-pinned and assert the build pod ran unprivileged, then deploy that exact
  digest — follow-up [#117](https://github.com/dafrie/kelson/issues/117), which names this harness as
  its successor. Nothing in this suite pulls an *image* from that registry today, which is why the
  kind nodes are given no containerd trust for it (see [docs/e2e.md](../../docs/e2e.md)); a build
  scenario is what would make that necessary.
- **Policy.** No Kyverno, Gatekeeper or ValidatingAdmissionPolicy is installed, so the live-policy
  case of [#45](https://github.com/dafrie/kelson/issues/45) — a server-side dry-run rejected by a
  real admission webhook, attributed to the engine that rejected it — cannot run here yet. The L2
  dry-run in `TestDeployLifecycle` exercises the mechanism, not the rejection.
- **Cluster-shape matrix.** Gateway API and cert-manager shapes are what issue #86's scope calls for
  beyond this slice. Per [ADR-0003](../../docs/adr/0003-install-model.md) the combinatorial space is
  handled with `ClusterProfile` fixtures rather than real clusters, so this needs only a small number
  of representative shapes — and the one that mattered most now exists: `TestDeliverySpine` runs
  against real source- and kustomize-controllers, so the Flux half of the delivery plane is no longer
  untested against a real reconciler. What is still untested there is a cluster with **no** Flux,
  where the controller must report `FluxNotInstalled` and wait rather than crash-loop.
- **`kelson status --output json`.** There is no JSON output on `status` today, so this suite asserts
  its stable text lines. `kelson diff --output json` is asserted structurally, decoded into
  `internal/diff`'s own types.
- **Failure paths.** `hack/e2e/run.sh` still owns the induced-`CrashLoopBackOff` scenario against
  `examples/hello-e2e`; it is not part of the CI job yet.
