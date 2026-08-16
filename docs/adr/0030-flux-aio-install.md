# ADR-0030: The install substrate is flux-aio, pre-rendered at release time and pinned

- **Status:** Accepted (Amended 2026-08-14: `kelson install` may also offer a `registry` catalog entry —
  an in-cluster OCI registry for a cluster that has none, applying this ADR's "kelson-authored, not
  fetched" exception a second time, for a second reason: CNCF Distribution publishes no install manifest
  at all, not even as a timoni module. See the amendment below.

  Revised in place 2026-08-16, on the owner's direction: decision 1's "offers" became "installs with
  kelson's own install" — the substrate is ensured automatically at chart-install time, with an
  off-switch, and adoption-on-presence unchanged. ADR-0021's offer-only posture still governs every
  optional component.)
- **Date:** 2026-08-14

> Extends [ADR-0021](0021-installing-missing-components.md)'s catalog with an entry whose bytes kelson
> produces rather than fetches, and states the trade against that ADR's "nothing is vendored" rule
> explicitly. Follows [ADR-0003](0003-install-model.md)'s detection doctrine unchanged.
> Exists because [ADR-0028](0028-delivery-spine.md) makes Flux a hard requirement.

## Context

[ADR-0028](0028-delivery-spine.md) makes Flux the only reconciliation path, which turns a question that
used to be optional into a blocking one: **what happens when someone points kelson at a cluster with no
Flux?** Previously the answer was "use direct mode". There is no direct mode any more.

The current catalog entry answers it with flux-operator: `kelson install flux` fetches
flux-operator's pinned `install.yaml` and creates a `FluxInstance` that installs the Flux controllers
([ADR-0021](0021-installing-missing-components.md) decision 5). That is a good answer for a substantial
cluster and a poor one for the clusters kelson most wants to be easy on. Full Flux is six controllers,
six Deployments, six sets of RBAC and roughly a gigabyte of memory at rest, plus flux-operator itself on
top — on a two-node k3s box or a single-node edge cluster, that is a platform installing a platform.

[flux-aio](https://github.com/controlplaneio-fluxcd/flux-aio) is the Flux distribution built for exactly
that case: all Flux controllers in **one pod**, one Deployment, one ServiceAccount, sized for k3s, edge
and lightweight clusters, from the same vendor as flux-operator. Functionally it is Flux — the same
controllers, the same CRDs, the same reconciliation semantics — packaged differently.

The obstacle is distribution. flux-aio is published upstream **only as a timoni module**. There is no
`install.yaml` release asset to pin a URL and a SHA-256 against, which is precisely the shape
[ADR-0021](0021-installing-missing-components.md) decision 2 requires of every catalog row.

## Decision

### 1. On a cluster without Flux, installing kelson installs flux-aio

*(Revised in place 2026-08-16, on the owner's direction. As originally accepted this decision read
"`kelson install` **offers** flux-aio" — and the lived result was a project created in the UI sitting
silently unreconciled on a cluster that had never been offered anything. A PaaS that does not deliver
is not a PaaS; the substrate is not an optional component.)*

Detection is unchanged: [ADR-0003](0003-install-model.md) says never install what is already there. A
cluster with Flux present is adopted and nothing is installed, whether that Flux came from
flux-operator, from `flux bootstrap`, from flux-aio or from a vendor's distribution — the
`ClusterProfile`'s `flux` finding is what matters, not its provenance.

A cluster with no Flux at all gets the substrate **with kelson's own install**: the chart runs a
one-shot ensure step (detection first, adoption on presence) that installs flux-aio — or full Flux via
flux-operator while the flux-aio snapshot is absent — with an explicit off-switch for operators who
want the old behaviour. `kelson install flux-aio` / `kelson install flux` remain as the CLI's explicit
path, and flux-operator remains required for PR previews (decision 4). The offer-only posture survives
for every *optional* component (ADR-0021); the delivery substrate stopped being one.

### 2. flux-aio is rendered at kelson release time, in CI, by the pinned timoni binary

`hack/flux-aio-render.sh` runs in the release pipeline:

- downloads the **pinned** `timoni` binary and verifies its checksum;
- runs it against the **pinned** flux-aio module version and digest;
- writes the rendered YAML to the path the catalog reads;
- fails the release if the rendered bytes differ from what is committed without an accompanying pin
  update.

The rendered manifests are then published into the existing install catalog
(`internal/delivery/install`) as an ordinary row: digest-pinned, applied with per-object provenance,
removable by `kelson uninstall --component flux-aio`, exactly as
[ADR-0021](0021-installing-missing-components.md) decision 3 and 4 require. The pin row records the
upstream module reference and its digest alongside kelson's own digest of the rendered output, so both
questions — *which upstream did this come from* and *are these the bytes kelson shipped* — have an
answer in the table.

### 3. Timoni is never a runtime dependency, never a Go dependency, never a user-visible concept

Three separate refusals, each for its own reason.

**Never a runtime dependency.** Nothing on a user's machine or in a cluster invokes `timoni`. The
binary runs once per kelson release, on a CI runner, and its output is bytes.

**Never a Go dependency.** Importing timoni as a library would drag the CUE runtime into kelson's module
graph, which is the dependency-weight failure [ADR-0022](0022-sops-age.md) documented in detail for
`getsops/sops` — 98 modules and a 74 MB binary for a function that could be written directly. Here the
same reasoning applies with more force, because the alternative is not writing the function but
downloading its output. The `.golangci.yml` fences do not move.

**Never a user-visible concept.** A kelson user installing Flux on their k3s box does not learn what
timoni is, does not install it, and does not encounter CUE. They run `kelson install flux-aio` and get
a Deployment. This matters because [ADR-0029](0029-renderer-stays-go.md) rejects CUE as kelson's
authoring language, and a user who met timoni during install would reasonably conclude otherwise.

### 4. flux-operator stays in the catalog, optional, and required only for PR previews

Unchanged from [ADR-0017](0017-pr-previews.md) and
[ADR-0021](0021-installing-missing-components.md): `ResourceSet` and `ResourceSetInputProvider` are
flux-operator CRDs, previews need them, and previews are the only feature that does. So flux-aio serves
the spine, flux-operator is added when someone wants previews, and the `ClusterProfile`'s existing
`fluxOperator` finding is what the preview gate reads. An environment that declares `previews:` on a
cluster with flux-aio and no flux-operator gets the capability gap reported with an offer to install,
which is the ADR-0003 pattern rather than a new one.

## Rationale

**The trade against "nothing is vendored", stated rather than skirted.**
[ADR-0021](0021-installing-missing-components.md) says it plainly — *"Nothing is vendored into this
repository. The repository holds the URL and the digest and never a copy of the manifest"* — and the
Bitnami lesson behind that rule is real: Kubero vendored Bitnami charts, Broadcom withdrew them, and
Kubero's users' running systems were at the mercy of a decision Kubero had no part in.

What this ADR ships is a **mechanically regenerated snapshot in kelson's custody**, and the difference
from vendoring is worth being precise about, because it is the whole justification:

- It is not hand-edited and cannot be. It is the output of one script against two pins, and CI fails if
  the committed bytes are not what the script produces.
- The upstream reference is recorded — module version and digest — so the provenance question has a
  documented answer rather than a folk memory.
- It is verifiable by anyone in one command: run the same pinned timoni against the same pinned module
  and compare checksums.
- It is small: one Deployment's worth of manifests, not a chart ecosystem.

The Bitnami failure was *unverifiable, unreproducible copies with no path back to their source*. This is
a reproducible build artifact. The rule's spirit is honoured; its letter is not, and this paragraph is
the record of that.

**Why not ask upstream for an `install.yaml`.** Worth doing, and it would delete this ADR's entire
mechanism. It is not something kelson's release can depend on happening.

**Why not vendor the CUE module and render it ourselves.** That requires a CUE evaluator in kelson,
which is the Go dependency decision 3 refuses, for the reason ADR-0029 gives at length.

**Why not run timoni at install time on the user's machine.** It would make a CUE toolchain a
prerequisite for installing kelson, put a network fetch of a third-party module in the install path, and
make the installed bytes vary with the module's availability on the day. Rendering at release time makes
the installed bytes a property of the kelson version — which is what a pin is *for*.

**Why flux-aio rather than a hand-written minimal Flux.** kelson could write its own single-pod Flux
assembly. It would be a Flux distribution kelson maintains, tracking six controllers' releases forever,
which is the opposite of [ADR-0005](0005-delegate-to-operators.md)'s delegation rule. flux-aio is
maintained by the same people who maintain flux-operator, which is the same people kelson already
depends on for previews.

## Consequences

**Positive.**

- The k3s and edge story is real: one pod of Flux, offered on a cluster that has none, installed by the
  same verb and the same provenance machinery as every other catalog component.
- ADR-0028's hard Flux requirement stops being a barrier for the users least able to absorb it.
- flux-operator becomes optional rather than foundational, so a cluster that never wants previews never
  installs an AGPL-3.0 component.
- The installed bytes are a function of the kelson version, reproducible from two pins, checkable by
  anyone.
- The user-facing surface gains one catalog row and no new concepts.

**Negative — stated as plainly as the positives.**

- **kelson now ships someone else's manifests in its own repository**, which is a real exception to
  ADR-0021's rule however carefully it is fenced. Every future "can we just vendor this?" will cite this
  ADR, and the answer will have to be that the exception was earned by a mechanically reproducible
  pipeline and a missing upstream release asset, not by convenience.
- **The release pipeline gains a third-party binary and a step that can fail.** A timoni release that
  breaks the module, or a module release that breaks rendering, blocks a kelson release until someone
  pins around it.
- **Two Flux installation shapes now exist in the catalog**, and a user has to be told which one they
  have when a support question arrives. `kubectl get deploy -n flux-system` answers it, and the
  documentation has to say so.
- **flux-aio's single pod is a single failure domain.** One controller's memory pressure or crash loop
  affects all of them, and the resource isolation full Flux gives is genuinely lost. That is the trade
  flux-aio exists to make and it is right for small clusters and wrong for large ones — which is why
  it is the offer for a cluster with no Flux, not a migration for a cluster that has some.
- **flux-aio may lag upstream Flux releases**, so kelson's minimum Flux version is bounded by whichever
  of the two moves slower, and ADR-0028's `ExternalArtifact` fallback (Flux ≥2.7) inherits that bound.
- **`kelson uninstall --component flux-aio` removes the reconciler every deployment depends on.** The
  per-object provenance of ADR-0021 makes the deletion correct and safe; it does not make it wise, and
  the refusal path has to say what will stop reconciling.

## Amendment (2026-08-14): install may also provide a registry

[ADR-0028](0028-delivery-spine.md) does to registries what it did to Flux: an optional integration
becomes a hard requirement. The delivery spine pushes every rendered revision as an OCI artifact, and
`kelson build` already needed somewhere to push application images — a cluster with neither now has two
missing prerequisites instead of one, and the second one has no offer. This amendment gives it one, on
the same "offer, never assume" terms decision 1 states for Flux itself.

**The decision.** `kelson install` gains a `registry` row in the catalog
(`internal/delivery/install/pins.go`, `internal/delivery/install/registry.go`): a Deployment, a
`kelson-registry` Service and a PersistentVolumeClaim in `kelson-system`, running
[CNCF Distribution](https://github.com/distribution/distribution) — the project `registry:2` and
`docker.io/library/registry` both ultimately are — pinned by **image digest**
(`ghcr.io/distribution/distribution@sha256:…`, the project's own registry rather than a Docker Official
Images mirror of it), never a tag. Bring-your-own remains the default and the recommendation for a team;
this row exists for the self-contained, lightweight cluster this ADR already writes for — the k3s and
edge audience flux-aio serves, now with the second missing prerequisite answered the same way.

**Why the spine needs zero node configuration, and app images need real node work.** The two consumers
of "a registry" in kelson are not symmetric, and the offer's node story follows that asymmetry exactly:

- The delivery spine's `OCIRepository` is pulled by **one thing only** — Flux's source-controller,
  running as an ordinary pod with a service account and a network route. A pod-to-Service pull inside
  the cluster is not different in kind from any other in-cluster HTTP call kelson already makes; no node,
  no containerd, no kubelet ever touches it. This is the same reason decision 1's flux-aio offer costs a
  cluster nothing beyond one Deployment: the thing being installed only ever talks to other things
  already inside the cluster's own network.
- An **application image**, once a component that names one is deployed, is pulled by the **node's own
  container runtime** scheduling the Pod — and that is Kubernetes' province, not kelson's. A plain-HTTP
  registry with no publicly-trusted certificate is precisely what every container runtime refuses by
  default, and making a node trust it is a node-level configuration change (a k3s `registries.yaml`
  entry, a containerd `hosts.toml`, a kubelet flag, depending on the distribution) that no
  cluster-scoped Deployment can reach into and make for it. `docs/install.md` carries the honest,
  per-distribution version of that story — k3s's one-file mirror config, kind's `hosts.toml` (the same
  shape `hack/local/up.sh` already sets up for local development), and the plain recommendation that a
  managed cluster (EKS, GKE, AKS) should bring its own registry, because node images and node-level trust
  on those are the control plane's to configure, not a workload's.

**Why this is the same exception as decision 2, applied a second time, for a different underlying
reason.** flux-aio is "kelson-authored, not fetched" because upstream publishes **only a timoni module** —
there is a build artifact, just not the plain YAML release asset ADR-0021 decision 2 wants to pin.
Distribution is a smaller case of the same problem with one fewer moving part: there is no Kubernetes
manifest published **at all**, timoni or otherwise, because the project ships a container image and
nothing else. So the objects are Go, not a rendered snapshot — small and stable enough that a
`hack/*-render.sh` pipeline would be machinery this row does not need — and what carries the pin is the
image digest rather than a manifest digest. The provenance discipline is unchanged: every object still
gets `kelson.dev/installed-component: registry`, still gets a created-versus-adopted ownership
annotation, and `kelson uninstall --component registry` still sweeps by that label, exactly as every
other row.

**Why this row is never offered automatically, unlike every component that came before it.** Decision 1's
"offer, never assume" and ADR-0021's Yes/No/Unknown detection both presuppose that kelson can ask the
cluster "is this here?" — a `ClusterProfile` finding, a Gap when the probe cannot look. A registry has no
such finding: it could be running outside this cluster entirely, behind credentials kelson is never
handed, so `ClusterProfile` carries no signal for it and `Component.Presence` can only ever answer No,
never Yes or Unknown. Detection-first eligibility does not fail on a registry — it simply cannot narrow
the offer at all, which is a different problem than the one ADR-0021's tri-state solves. The answer is
narrower than a detection gap: `--all-missing` declines `registry` unconditionally (not the
ingress-stack-conditional exception envoy-gateway gets), and only naming it — `kelson install registry`
— installs one. A sweep that cannot tell "genuinely missing" from "kelson cannot see it" must not guess,
and here it never can tell, so it never guesses.

**Consequences, stated the same way the ADR above states its own.** Positive: the second missing
prerequisite ADR-0028 created now has an answer, on the cluster shape that needed it most, with no new
concept beyond one more catalog row. Negative: an operator who installs this and later points a node at
plain HTTP has done a node-level trust change kelson did not do for them and cannot verify — the
gap between "installed" and "app images actually pull" is real and `docs/install.md` names it rather
than implying the offer closes it. Garbage collection is not part of this row: nothing prunes the
registry's storage automatically, and `docs/install.md` documents the manual `registry garbage-collect`
command rather than promising an R1 feature that does not exist. `kelson uninstall --component registry`
deletes the PersistentVolumeClaim if kelson created it, and with it every image and manifest artifact the
registry held — the same "correct and safe, not necessarily wise" trade the Consequences section above
already accepts for `kelson uninstall --component flux-aio`, extended to data rather than only to a
reconciler.

## Revisit when

- **flux-aio publishes a plain YAML release asset.** The render script and the committed snapshot both
  disappear and the row becomes an ordinary ADR-0021 pin — the outcome this ADR would prefer.
- **flux-aio is abandoned or diverges from Flux.** The fallback is flux-operator, already in the
  catalog, so this is a catalog-default change rather than a redesign.
- **The single-pod trade proves wrong for real users**, e.g. one controller's resource use starving the
  others on the clusters kelson actually runs on.
- **flux-operator ships previews-equivalent functionality in flux-aio**, or Flux upstream absorbs
  `ResourceSet`. Decision 4's split stops being necessary and the catalog loses a row.
- **CNCF Distribution publishes a Kubernetes install manifest.** The amendment's second exception
  disappears the same way the first one would: `registry.go`'s composed objects are replaced by a
  fetched, digest-pinned manifest, and the row becomes an ordinary ADR-0021 pin.
- **`ExternalArtifact` ([#228](https://github.com/dafrie/kelson/issues/228)) ships.** The spine gains a
  registry-less path for manifests; the `registry` offer's justification narrows to what `kelson build`
  still needs, and the amendment's framing of "two missing prerequisites" should be revisited alongside
  it.
