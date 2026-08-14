# Running the E2E harness locally

`hack/e2e/` (issue #86) takes `examples/hello-e2e` from a spec to running workloads and back on a
local [kind](https://kind.sigs.k8s.io/) cluster — the delivery adapters, status correlation and
rollback exercised against a real API server, not a fake client. Golden files cover the renderer
(issue #2); this harness covers what golden files cannot.

There are two harnesses over the same cluster, and they divide by audience:

| | `hack/e2e/run.sh` (`make e2e`) | `test/e2e/` (`make test-e2e`) |
|---|---|---|
| Shape | bash script | Go package behind the `e2e` build tag |
| Spec | `examples/hello-e2e` | `test/e2e/testdata/minimal.yaml`, `testdata/spine.yaml` |
| Covers | render → apply → induced `CrashLoopBackOff` → status → restore, and that the gated verbs refuse honestly | the **delivery spine** end to end (below), plus render → status → diff → re-apply, the label-selector additivity check, and `kelson uninstall` against a kelson-created namespace, an adopted one, and one a second project shares |
| Runs in CI | no | yes — `.github/workflows/e2e.yml`, not a required check yet |

The Go suite is the one CI runs and the one to extend; the script keeps the failure-path scenario the
Go suite does not have yet. Both create the same cluster through `hack/e2e/up.sh`. See
`test/e2e/README.md` for the Go suite's scenarios, environment variables and flake posture.

## Prerequisites

- Docker Desktop (or another reachable Docker daemon) — the scripts check for this and fail with a
  plain-language message if it isn't running.
- `kubectl` on `PATH`.
- `helm` on `PATH`, for the spine stage only (`hack/e2e/spine.sh` installs the chart).
- Go, to build the `kelson` and `kelson-controller` binaries from this checkout.
- Outbound network, for the spine stage: the pinned flux-operator manifest, the Flux and Distribution
  images, and `registry.k8s.io/pause`.

`kind` itself is not a prerequisite: `hack/e2e/up.sh` downloads a pinned, checksum-verified binary
into `hack/bin/` (gitignored) the first time it runs.

## Running it

```sh
make e2e-up    # create the kind cluster (idempotent — safe to re-run)
make e2e       # build kelson, render, apply, induce a failure, verify status, restore
make e2e-down  # delete the cluster (idempotent)
```

`make e2e` provisions the cluster if needed and then runs `hack/e2e/run.sh`, which:

1. builds `kelson` from this checkout (`hack/bin/kelson`);
2. captures a `ClusterProfile` with `kelson profile` and renders `examples/hello-e2e` against it,
   asserting the expected Kubernetes kinds come out;
3. asserts that `kelson deploy` and `kelson rollback` **refuse** with the `delivery/not-implemented`
   code naming [#224](https://github.com/dafrie/kelson/issues/224) — the machinery behind them was
   deleted by [ADR-0028](adr/0028-delivery-spine.md), and a gated verb is only acceptable if it
   refuses in a shape a caller can act on;
4. applies the rendered set with `kubectl` — the anti-lock-in property ADR-0028 decision 10 relies
   on, and the same set the controller publishes as an artifact — and asserts the workload is running
   and ready;
5. applies a broken revision (an overridden container command that exits on start) to induce a
   `CrashLoopBackOff`, and asserts `kelson status` names it;
6. re-applies the good revision and asserts, via `kubectl`, that the original workload is restored
   and healthy.

Every stage asserts and exits non-zero with a clear message on failure — nothing is skipped silently.
The deploy/rollback lifecycle itself now lives in the Go suite's spine scenario below, against the
controller rather than against the CLI.

`make e2e` leaves the cluster up afterward so its state can be inspected with `kubectl
--kubeconfig hack/bin/e2e.kubeconfig`; run `make e2e-down` when done.

## The spine stage

`TestDeliverySpine` (`test/e2e/spine_test.go`) is the exit gate for R1
([ADR-0028](adr/0028-delivery-spine.md), [docs/roadmap.md](roadmap.md)): **a spec deployed end to end
on a kind cluster** — applied as custom resources, published as an OCI artifact, reconciled by Flux,
reported back in `Environment.status`. It is the only scenario in the suite that applies nothing but
a `Project` and an `Environment` and then asserts on what the cluster did about them.

`hack/e2e/spine.sh` builds the cluster side of that sentence, and `make test-e2e` runs it first:

```sh
make test-e2e     # spine.sh (cluster, Flux, registry, controller) then the Go suite
make e2e-down     # delete the cluster when done
```

It is idempotent, so re-running converges and only rebuilds the controller image. In order:

1. **Flux**, through `kelson install flux --yes` — the pinned, SHA-256-verified flux-operator release
   in `internal/delivery/install/pins.go` plus the `FluxInstance` it creates. It is kelson's own
   install path rather than a stanza copied into the script, so the harness cannot drift from the
   version the pins table promises, and `kelson install` gets end-to-end coverage it otherwise has
   nowhere. The stage waits for the `FluxInstance` to go `Ready` and for source-controller and
   kustomize-controller to roll out, because kelson-controller detects Flux **once, at start-up**
   (`internal/clusterprofile/detect`): a controller that starts first reports `FluxNotInstalled`
   until it is restarted, so the ordering here is load-bearing rather than incidental.
2. **The registry**, through `kelson install registry --yes` — the in-cluster CNCF Distribution the
   catalog row authors, at `kelson-registry.kelson-system.svc.cluster.local:5000`, plain HTTP.
3. **The controller image**, built from this checkout with `Dockerfile.controller` and `kind load`ed.
4. **The chart**, `helm upgrade --install` with `controller.enabled=true`, `auth.insecure=true`,
   the image just loaded, and the registry named both as `controller.registry` and in
   `controller.insecureRegistries` — kelson never speaks plain HTTP to a host it was not told about.
   `replicaCount=0` scales kelson-server to nothing: it is not what this stage proves and its image
   is not built here, so scheduling a pod that would sit in `ImagePullBackOff` would be noise in
   every diagnostic dump.

**No containerd trust is written onto the kind nodes**, unlike `hack/local/up.sh`. Nothing here asks
the *kubelet* to pull from the in-cluster registry: the only clients of it are kelson-controller
pushing an artifact and source-controller pulling one, both in-cluster Go processes reaching a
Service FQDN. The workload image is `registry.k8s.io/pause`, pulled from the internet as usual.

The scenario then asserts, in one linear sequence because each step needs the state the last one
left:

1. the `Environment` reaches `Ready`/`Healthy` for its generation;
2. the registry holds a tag `<generation>-<spec-hash-short>` whose OCI manifest carries the Flux
   media types and five provenance annotations — and whose `kelson.dev/generation` and
   `kelson.dev/spec-hash` **compose the tag it was fetched by**;
3. the `OCIRepository` + `Kustomization` pair exists in `kelson-system`, carries the four provenance
   labels of `internal/delivery/provenance.go`, is pinned to that tag, and is `prune: true`,
   `wait: true`, `path: ./`, `targetNamespace: <the environment's>`;
4. the workload is live in the environment's namespace on the image the spec names;
5. a spec edit publishes a second revision, and `status.history` holds both, newest first;
6. `kubectl annotate … kelson.dev/rollback-to=<revision 1>` moves the pointer, and **the deployed
   bytes revert** — the Deployment runs the older image again while the spec still says otherwise —
   with `Ready=RolledBack`, `Progressing=False/RollbackPinned` and no new history entry, because a
   rollback publishes nothing;
7. nothing is republished while the pin is in force (a quiet window, not a converging poll);
8. a spec edit under the pin publishes a third revision — new intent wins;
9. removing the annotation resumes tracking, and revision 3 goes live.

### Known: an inert rollback re-arms

ADR-0028 decision 5 names two ways out of a rollback — remove the annotation, or edit the spec — and
only the first is stable today. `rollbackFor` (`internal/controller/rollback.go`) correctly computes
the generation an *inert* rollback should keep, but `EnvironmentReconciler.Reconcile`
(`internal/controller/environment.go`) writes `status.rollbackRevision` / `status.rollbackGeneration`
only when the rollback is **active**, and clears them otherwise. So the inert reconcile publishes the
edited spec and then wipes the field the next reconcile needs: that reconcile sees a standing
annotation against an empty `status.rollbackRevision`, calls it a *new* rollback, and pins again. A
spec edit under the annotation therefore publishes once and flaps back.

Step 8 above asserts only the half that holds either way — the edit is published, and the artifact
and the history record it — and step 9 uses the way out that is stable. Fixing the reconciler is a
change to `internal/controller`, not to this harness.

## The example

`examples/hello-e2e` is deliberately minimal: one component, one port, one health path, deployed
with [`traefik/whoami`](https://github.com/traefik/whoami) — a small, real, publicly hosted image that
actually starts and answers HTTP (every path, 200), which keeps the "healthy" case boringly
deterministic. It carries no routing,
since a stock kind cluster has no Gateway API for the renderer to attach a route to. Declaring domains
against such a cluster is a render-time capability gap since
[#140](https://github.com/dafrie/kelson/issues/140), not a silent Ingress
(`internal/renderer/routing.go`).

## What's out of scope here

Per [ADR-0003](adr/0003-install-model.md), the combinatorial cluster-shape space (Gateway API,
cert-manager, flux-aio vs full Flux, flux-operator, ...) is covered by `ClusterProfile` fixtures in the
renderer's golden
tests, not by additional kind clusters. This harness runs a single, minimal shape; issue #86 tracks
extending it to a small set of representative shapes and wiring it into CI.

**`kelson build` is also out of scope here today.** The build plane is unit-tested end to end against
fakes — the workload renderer purely, the executor against a fake clientset, the command through its
connector seam — but nothing has yet run a real BuildKit Job against a real registry. That needs a
cluster and a registry, which is what this harness has, so it belongs here: a build stage would push
to a cluster-internal registry, assert the printed reference is digest-pinned, assert the build pod
ran unprivileged, and then deploy that exact digest. Tracked by issue #86; see
[docs/build.md](build.md).
