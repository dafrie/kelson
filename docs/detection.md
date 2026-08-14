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
external-secrets, CloudNativePG, the Valkey operator, Flux, helm-controller, flux-operator, ArgoCD,
metrics-server, the policy engines and Prometheus without issuing a list per group.

Three findings carry a version *and* a served-resource list, and for the same reason: kelson writes a CR
against each of them, so "present" alone cannot answer whether a component is renderable. The two data
operators are two of them; **helm-controller** is the third, because a `kind: helm` component renders a
`HelmRelease` ([ADR-0016](adr/0016-delivery-flows-v0.md)). Every version is read from the controller
Deployment's image tag (falling back to the chart's `app.kubernetes.io/version` label when the image is
digest-pinned), and every served set comes from discovery — `clusters`/`databases` in
`postgresql.cnpg.io`, `valkeyclusters`/`valkeynodes` in `valkey.io`, `helmreleases` in
`helm.toolkit.fluxcd.io`. None of those reads may demote its controller to absent when it fails: absent
is the one answer that would tell a caller to install a *second* operator into a cluster that can only
have one ([ADR-0005](adr/0005-delegate-to-operators.md)). They fail into a gap instead (`cnpg.version`,
`valkey.crds`, `helmController.crds`, …), and `internal/clusterprofile/postgres`,
`internal/clusterprofile/valkey` and `internal/clusterprofile/helm` turn the facts into per-capability
verdicts.

**helm-controller is a separate finding from Flux**, and that is not bookkeeping: flux-operator's
`FluxInstance` installs a *components subset*, and "source-controller and helm-controller only" is a
supported shape ([#60](https://github.com/dafrie/kelson/issues/60)). So a cluster can be running Flux
and have nothing that reconciles a `HelmRelease`, which is exactly the half-install a capability check
exists to catch. The controller is located by `app.kubernetes.io/component=helm-controller` rather than
by `app.kubernetes.io/name`, because Flux's own manifests set `name` to `flux` for the whole suite and
`component` to the individual controller.

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

## Version skew and explicit degradation

Adopting a component means inheriting its version skew ([ADR-0003](adr/0003-install-model.md)). Knowing
that cert-manager is *present* is not enough: an install too old to serve the API kelson renders against
accepts the command, accepts the apply, and fails confusingly long afterwards. So the profile's versions
are judged against a declared floor, and the judgement is stated in a fixed vocabulary
([#57](https://github.com/dafrie/kelson/issues/57)).

The floors, the ceilings and what each one costs live in one table,
`internal/clusterprofile/support/matrix.go`, which is also the source
[docs/reference/support-matrix.md](reference/support-matrix.md) is generated from — two copies of a
support matrix is the exact failure the issue named. `internal/clusterprofile/support` turns that table
plus a `ClusterProfile` into a report; it is a pure package, so preview and the renderer may import it.

| Statement | Meaning |
|---|---|
| *(nothing)* | The component is at or above its floor, inside the tested range, or absent. Absent is not a version question. |
| `[unsupported]` | Checked and too old. Names the component, the version found, the version required, **what specifically degrades**, and the upgrade. |
| `[unknown]` | The version is absent, unparseable, or hidden behind a detection `Gap`. Neither confirmed supported nor rejected; the statement says what is now unverified. |
| `[note]` | Newer than the version kelson is tested against. Not a refusal — see below. |

The named degradation is the point. "cnpg 1.20.0 is too old" sends a reader to the source; "…so every
`kind: postgres` component refuses to render" is a decision they can make. Every matrix row carries that
sentence and a test fails if one does not.

**Unknown is a third answer, not a soft no.** It follows the same rule as the shape contract above and
uses the same vocabulary as every other judgement kelson makes about a cluster
(`clusterprofile.Outcome`, [#144](https://github.com/dafrie/kelson/issues/144)). A caller gating on
`Report.Degraded()` is told about components that were *checked and found wanting*, never about ones
that could not be checked — the `Gap` reason travels in the statement so the missing permission is
named rather than shrugged at.

**The floor refuses; the ceiling only notes.** A cluster newer than the version kelson is exercised
against stays supported, because "we have not tried it" is not evidence of a problem and refusing on it
would break working clusters to prevent a hypothetical. The note exists so that a surprise on an
untested release is attributable instead of mysterious. Today only the `kubernetes` row declares a
ceiling; it is the version the e2e harness runs (`hack/e2e/lib.sh`), and a test in the detect package
fails if the two ever disagree.

The statements are **composed at display time, never stored in the profile.** They are a judgement
*about* the profile, and a judgement written into the input would be carried, stale, into every later
`kelson render --profile cluster.yaml` that read the file back. Anything holding a `ClusterProfile` —
the CLI, the server, a client that received the YAML from `GetProfile` — can recompute them.

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

The skew statements are printed to stderr wherever a profile enters a command — `profile`, and every
`render`, `diff`, `preview`, `promote` and `deploy` that resolves one, live or from a file — which is
what puts an unsupported combination in front of a reader at preview time rather than at apply time:

```console
$ kelson profile > cluster.yaml
warning: version skew — 2 statement(s) about what this cluster changes (docs/reference/support-matrix.md):
  - [unsupported] cnpg 1.20.0 is below the minimum supported version 1.23.0 …. Degraded: every `kind: postgres` component …
  - [unknown] flux version unknown (no version was reported); … Unverified: the GitOps delivery flow …
```

It is a report, not a global refusal. The refusals that exist are specific and live where the decision
belongs: `internal/clusterprofile/postgres`, `valkey` and `helm` already refuse the presets and kinds
their operator cannot serve, and the renderer surfaces that as an error naming the version. Turning a
too-old cert-manager into a whole-command failure would refuse specs that ask nothing of cert-manager,
which is a worse answer than the skew.
