# ClusterProfile detection

The renderer takes a `ClusterProfile` as an input and never looks one up (issue #20, ADR-0001). This
document is about the *other* side of that contract: where a `ClusterProfile` comes from when you have
a real cluster in front of you. Detection lives in `internal/clusterprofile/detect`, a sibling of the
profile *type* that the renderer imports, never a dependency of it.

## The shape contract

`internal/clusterprofile/clusterprofile.go` fixes the answers detection is allowed to give:

| State | Meaning |
|---|---|
| `Field == nil` (component pointers) | **Not detected.** e.g. no Gateway API registered. |
| `Field != nil` | **Present.** `Version` empty means installed, version unknown. |
| `ClusterProfile.Incomplete` | **Could not tell.** Crucially, *not* "absent". |

The third bucket is the whole point of the design (issue #56). If a probe lacked RBAC to list
ClusterIssuers and reported `CertManager: nil`, the renderer would emit a route with no TLS and the
manifest would look deliberate. So a `Forbidden` — or a `NotFound` on the API endpoint itself — appends
a `Gap{Field, Reason}` to `Incomplete`; only a *successful empty list* means absent. `Gap.Reason`
names the missing permission so a human can grant it and re-run.

## How the probe works

Presence is established once, with a single read of `/apis`: an API group being registered means the
component that owns it is installed. That one call tells us about Gateway API, cert-manager,
external-secrets, CloudNativePG, the Valkey operator, Flux, flux-operator, ArgoCD, metrics-server, the
policy engines and Prometheus without issuing a list per group.

The two data operators are the only findings that carry a version *and* a served-resource list, and
for the same reason: kelson writes a CR against each of them, so "present" alone cannot answer whether
a component is renderable. Both versions are read from the operator Deployment's image tag (falling
back to the chart's `app.kubernetes.io/version` label when the image is digest-pinned), and both
served sets come from discovery — `clusters`/`databases` in `postgresql.cnpg.io`,
`valkeyclusters`/`valkeynodes` in `valkey.io`. Neither read may demote its operator to absent when it
fails: absent is the one answer that would tell a caller to install a *second* operator into a cluster
that can only have one ([ADR-0005](adr/0005-delegate-to-operators.md)). They fail into a gap instead
(`cnpg.version`, `valkey.crds`, …), and `internal/clusterprofile/postgres` and
`internal/clusterprofile/valkey` turn the facts into per-capability verdicts.

Flux and flux-operator are two findings, not one: most Flux installs have no operator, and the
delivery plane prefers the operator's `FluxReport` for "is Flux itself healthy" where it exists
([#137](https://github.com/dafrie/kelson/issues/137)). Recording `fluxOperator` is what keeps that
preference a finding instead of something the adapter establishes by attempting the read and falling
back ([#157](https://github.com/dafrie/kelson/issues/157)) — the inversion ADR-0003 exists to prevent.
Detection reads the `fluxcd.controlplane.io` group's registration and nothing else: flux-operator is
AGPL-3.0 and kelson is MIT, so no Go module of theirs is imported anywhere.

The per-resource details (which classes, issuers, stores exist) are layered on top with their own
lists, and each is individually allowed to fail into a `Gap` without demoting the owning component to
absent. If `/apis` itself is forbidden, *every* group-backed field becomes a gap at once — there is no
way to tell a missing component from an invisible one.

The probe never fails because a single component could not be read; it fails only when the cluster
itself is unusable (no credentials, unreachable), the same loud failure the delivery layer reports for
a server-side preview.

## Read-only by construction

`kelson profile` and `kelson render --profile from-cluster` need no cluster-admin. The exact ClusterRole
is `deploy/rbac/detect-clusterrole.yaml`, and a test (`deploy`/RBAC enforcement in the detect package)
parses that file and fails if any verb outside `{get, list, watch}` appears — the criterion is enforced,
not asserted in prose.

## CLI

`kelson profile` captures the current cluster and writes the profile to stdout, so
`kelson profile > cluster.yaml && kelson render --profile cluster.yaml` round-trips. Incomplete gaps
are printed to stderr as a warning *and* kept in the YAML, because a user piping stdout to a file
silently burying a partial profile is the failure mode this whole subsystem exists to prevent.
`--kubeconfig` selects a context the usual way (`$KUBECONFIG`, in-cluster, `~/.kube/config`).

The render and diff commands take `--profile from-cluster` and a `--kubeconfig` flag, wired to the same
detector.
