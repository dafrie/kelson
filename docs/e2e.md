# Running the E2E harness locally

`hack/e2e/` (issue #86) takes `examples/hello-e2e` from a spec to running workloads and back on a
local [kind](https://kind.sigs.k8s.io/) cluster — the delivery adapters, status correlation and
rollback exercised against a real API server, not a fake client. Golden files cover the renderer
(issue #2); this harness covers what golden files cannot.

There are two harnesses over the same cluster, and they divide by audience:

| | `hack/e2e/run.sh` (`make e2e`) | `test/e2e/` (`make test-e2e`) |
|---|---|---|
| Shape | bash script | Go package behind the `e2e` build tag |
| Spec | `examples/hello-e2e` | `test/e2e/testdata/minimal.yaml` |
| Covers | deploy → induced `CrashLoopBackOff` → status → rollback | deploy → status → diff → redeploy → rollback, the label-selector additivity check, and `kelson uninstall` against both a kelson-created and an adopted namespace |
| Runs in CI | no | yes — `.github/workflows/e2e.yml`, not a required check yet |

The Go suite is the one CI runs and the one to extend; the script keeps the failure-path scenario the
Go suite does not have yet. Both create the same cluster through `hack/e2e/up.sh`. See
`test/e2e/README.md` for the Go suite's scenarios, environment variables and flake posture.

## Prerequisites

- Docker Desktop (or another reachable Docker daemon) — the scripts check for this and fail with a
  plain-language message if it isn't running.
- `kubectl` on `PATH`.
- Go, to build the `kelson` binary from this checkout.

`kind` itself is not a prerequisite: `hack/e2e/up.sh` downloads a pinned, checksum-verified binary
into `hack/bin/` (gitignored) the first time it runs.

## Running it

```sh
make e2e-up    # create the kind cluster (idempotent — safe to re-run)
make e2e       # build kelson, deploy, induce a failure, verify status, roll back
make e2e-down  # delete the cluster (idempotent)
```

`make e2e` provisions the cluster if needed and then runs `hack/e2e/run.sh`, which:

1. builds `kelson` from this checkout (`hack/bin/kelson`);
2. captures a `ClusterProfile` with `kelson profile` and renders `examples/hello-e2e` against it,
   asserting the expected Kubernetes kinds come out;
3. `kelson deploy`s the good revision and asserts, via both the CLI's exit code and `kubectl`, that
   the workload is running and ready;
4. deploys a broken revision (an overridden container command that exits on start) to induce a
   `CrashLoopBackOff`, and asserts `kelson status` names it;
5. `kelson rollback`s and asserts, via both the exit code and `kubectl`, that the original workload is
   restored and healthy.

Every stage asserts and exits non-zero with a clear message on failure — nothing is skipped silently.
If the `kelson` binary predates the `deploy`/`status`/`rollback` CLI wiring (issue #135), stages 1–2
still run and the script then fails fast with an explicit message rather than faking the rest.

`make e2e` leaves the cluster up afterward so its state can be inspected with `kubectl
--kubeconfig hack/bin/e2e.kubeconfig`; run `make e2e-down` when done.

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
