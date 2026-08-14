# ADR-0028: The delivery spine — render, push an OCI artifact, let Flux reconcile

- **Status:** Accepted
- **Date:** 2026-08-14

> Supersedes the hybrid-delivery half of [ADR-0001](0001-hybrid-state-model.md) — the mode list, the
> per-environment mode selection and `direct` as a first-class peer. ADR-0001's core insight, one pure
> renderer behind a pluggable seam, survives and is what makes this change small.
> Supersedes the delivery-mode gates of [ADR-0016](0016-delivery-flows-v0.md) (decision 4's gate, and
> the precedent paragraph); ADR-0016's promotion definition survives intact.
> Supersedes the git-writer mechanics of [ADR-0012](0012-flux-only-gitops.md) while carrying its
> conclusion — Flux, and only Flux — forward as the premise.
> Depends on [ADR-0027](0027-crd-native-control-plane.md), which creates the controller this ADR
> describes the work of.

## Context

[ADR-0001](0001-hybrid-state-model.md) chose hybrid delivery and argued the doubled surface would
dissolve, because *"modes differ only in who calls apply, which is an adapter, not a second codebase"*.
That was true of `apply` and false of everything downstream of it.
[ADR-0012](0012-flux-only-gitops.md) already paid this bill once, deleting the Argo adapter after the
2026-08-12 review found that *"the rendered-output claim is cheap and true; the last mile is not"* —
different health models, different secrets stories, every future feature designed three ways. The
review deleted one of three modes. It did not ask whether two was the right number either.

Since then the evidence has accumulated in one direction, and it is all in this repository:

- **Three spec fields are now gated on delivery mode.** `kind: helm` is Flux-only
  (ADR-0016 decision 4), `previews:` is Flux-only ([ADR-0017](0017-pr-previews.md) decision 5),
  `secrets.backend: sops` is Flux-only ([ADR-0022](0022-sops-age.md) decision 2), and `release:` is
  *direct*-only ([ADR-0019](0019-release-command-hook.md) decision 4). ADR-0016 said in as many words
  that the gate was *"a decision, not a pattern to reach for"*, and it was then reached for three more
  times in eight days. The pattern is not the gate; the pattern is that a real capability exists in one
  mode and cannot be built in the other.
- **Every capability kelson wants next is a Flux capability.** Health readback, preview lifecycle, chart
  delegation, in-cluster decryption, dependency ordering, drift correction, pruning. Direct mode
  reimplements each of them or refuses each of them.
- **Direct mode is the one that has to reimplement Kubernetes.** Apply ordering, wait-for-ready, prune
  by inventory, drift detection, retry with backoff — kustomize-controller does all of it, tested,
  and `internal/delivery/direct` does a subset of it, ours.
- **Git-as-transport costs credentials kelson does not have.** ADR-0022 §5 and ADR-0017 decision 8 both
  hit the same wall from different sides: writing to a forge needs a forge credential in the control
  plane, which is a class of secret [ADR-0009](0009-secrets.md) deliberately does not hold.

Meanwhile ADR-0017's preview pipeline solved the same problem without git at all: render concrete
manifests, push them as a Flux OCI artifact, let a controller consume it. That path is built, tested,
deterministic to the digest, and it needs no forge credential — only a registry, which the build plane
already requires.

The question this ADR answers is therefore not "which delivery modes" but "what is the one path", and
the preview pipeline had already found it.

## Decision

### 1. Flux is the only reconciliation path. There is one spine, and it is a controller loop

`kelson-controller` ([ADR-0027](0027-crd-native-control-plane.md)) reconciles an `Environment` through
six steps:

1. **Validate** — `internal/model/validate.go`. Invalid means `Ready=False`, `reason: SpecInvalid`,
   `status.validationErrors[]`, and stop. Nothing downstream runs.
2. **Detect** — read the `ClusterProfile` ([ADR-0003](0003-install-model.md)). Unchanged.
3. **Resolve and render** — `internal/model`'s resolver and the pure renderer, `(spec, ClusterProfile) →
   manifests`. Unchanged, and still pure: the controller is the caller that has cluster access, the
   renderer still has none.
4. **Publish** — push the rendered set as an immutable OCI artifact (decision 2).
5. **Ensure** — server-side apply a Flux `OCIRepository` + `Kustomization` pair in `kelson-system`
   (decision 3).
6. **Observe** — watch the Flux objects and the workloads, feed the existing
   `internal/delivery/statemachine`, and write the result into `Environment.status`.

Delivery mode ceases to be a concept. There is no adapter to select, no `delivery:` block to write, and
no per-environment choice to make.

### 2. The artifact is immutable and named from the generation and the spec hash

```
<registry>/kelson/<project>-<environment>:<generation>-<spec-hash-short>
```

`<generation>` is the `Environment`'s `.metadata.generation` — monotonic, assigned by the API server,
and therefore a revision number kelson does not have to allocate. `<spec-hash-short>` is the first
eight hex of the resolved spec hash, which is what makes the tag *content*-identified as well as
sequence-identified: two tags with the same hash suffix are the same input, and a rollback target is
recognisable without fetching it.

The artifact is a Flux OCI artifact in exactly the form [ADR-0017](0017-pr-previews.md) decision 10
already specifies — `application/vnd.cncf.flux.config.v1+json` config, one
`application/vnd.cncf.flux.content.v1.tar+gzip` layer holding the rendered set as a flat directory,
deterministic tar with fixed modes and a fixed timestamp, annotated with revision, source and the
`kelson.dev/*` provenance labels. **It is the same publisher**: previews and the spine converge on one
package, one media type, one determinism test. A tag is written once and never rewritten; republishing
an unchanged spec produces a digest the registry already holds and uploads nothing.

`<registry>` is the controller's configuration (`--registry`, `--push-secret`), the same way the build
plane's destination is the server's configuration under
[ADR-0013](0013-server-state-and-api-v0.md) §2 — where an artifact is pushed is not application
description.

### 3. kelson owns exactly two Flux objects per environment, in `kelson-system`

An `OCIRepository` pinned to the tag just pushed, and a `Kustomization` referencing it with
`path: ./`, `prune: true`, and `spec.decryption` when the environment's secret backend is `sops`
(decision 7). Both are applied with server-side apply, field manager `kelson-controller`, and both
carry the standard provenance labels so `kubectl get kustomizations -n kelson-system -l
kelson.dev/project=x` is the inventory.

They live in `kelson-system` rather than the workload namespace because they are kelson's objects, not
the application's, and because a `Kustomization` that can be deleted by someone tidying an application
namespace is a deployment that silently stops reconciling.

This reverses ADR-0022 §5's *"kelson does not write that Kustomization, because the reconciler's own
objects belong to the operator"*. In a world where kelson selects a delivery mode and a user's own
Kustomization reconciles a path in the user's own repository, that was right. In a world where kelson
publishes the artifact, the Kustomization that consumes it has no other plausible owner, and asking the
operator to hand-write it would make the happy path a two-system setup.

#### Amendment, 2026-08-14: an Environment finalizer, and what deleting one destroys

Recorded while building the spine ([#224](https://github.com/dafrie/kelson/issues/224)). Decision 3 said
which objects kelson owns and where they live, and said nothing about what happens when the `Environment`
that caused them goes away. This closes that, and states the consequence in the plainest terms available,
because it is the sharpest edge the spine adds after the registry requirement.

**The decision.** `Environment` carries the finalizer `kelson.dev/environment`, added on the first
reconcile that successfully applies the pair — not before. On deletion the controller removes the
`Kustomization` first, then the `OCIRepository`, and only then releases the finalizer. `IsNotFound` at
either step is success, because a teardown re-runs on every reconcile until the finalizer clears and
"already gone" is the state it is trying to reach.

**Why a finalizer and not an owner reference.** The two Flux objects live in `kelson-system` and the
`Environment` lives in the application's namespace. Kubernetes garbage collection does not cross
namespaces — a cross-namespace owner reference is not merely unsupported, it marks the dependent as an
orphan and makes it eligible for deletion — so ownership has to be enforced by the controller that
created both.

**Why the Kustomization goes first.** Deleting it is what removes the workloads: it prunes its own
inventory on the way out, which is the behaviour `prune: true` bought. Deleting the `OCIRepository`
first would leave the `Kustomization` pointing at a source that no longer exists, so it would stop
reconciling with an error, prune nothing, and the workloads would outlive the `Environment` that
declared them with nothing left in the cluster saying whose they were.

**The sharp edge, stated once and stated plainly: deleting an `Environment` deletes its workloads.**
`kubectl delete environment production` is not a control-plane bookkeeping operation. It removes the
`Kustomization`, the `Kustomization` prunes everything it applied, and the running application goes
with it. That follows from decision 3 rather than being a new choice — a `Kustomization` with
`prune: true` is what a deployment *is* here — but it is the kind of consequence a reader has to be
told rather than left to derive, and it is repeated in [the delivery plane](../delivery.md).

**Two things are deliberately not deleted.** The workload namespace, because deleting a namespace
cascades to everything inside it including resources kelson never created, and the provenance labels
can never prove kelson created the namespace rather than adopting one that was already there
(`internal/delivery/provenance.go`'s ownership annotation exists for exactly this distinction).
`kelson uninstall` is the verb that reasons about namespaces; deleting a custom resource is not. And
the published artifacts, because they *are* the history (decision 4) and they are immutable: deleting
an `Environment` must not make its own record unrecoverable, and re-applying the same spec afterwards
finds every revision it ever published still in the registry.

### 4. History is the registry's tag list, mirrored bounded into status

The registry holds every artifact ever published for an environment, immutably, and that *is* the
history — nothing needs to store rendered manifests a second time, and the 1 MiB ConfigMap budget of
ADR-0013 §1 stops being an arithmetic problem the code has to do.

`Environment.status.history[]` mirrors the most recent entries — **bounded at 20**, matching the
existing `DefaultKeep` — with, per entry: revision (the tag), digest, spec hash, timestamp, the
resolved images, and the outcome. It is a mirror for humans and for the API, not the record; the record
is the registry, and `kelson history` beyond the window is a registry query.

### 5. Rollback is an annotation, and it suspends re-render until it clears

```
kelson.dev/rollback-to: <revision>
```

on the `Environment`. The controller repoints the `OCIRepository` at that immutable tag, sets
`Ready=True` with `reason: RolledBack`, and **suspends re-rendering**: while the annotation is present,
step 3 and step 4 do not run, so the controller cannot immediately re-publish the current spec on top of
the thing you just rolled back to. Two things resume tracking, and only two: removing the annotation,
or **editing the spec** — because a spec edit is an unambiguous statement of new intent, and an operator
who has just fixed the bug should not have to remember to clear an annotation as well.

The state is visible rather than implicit: `status.conditions` carries `Progressing=False` with
`reason: RollbackPinned`, and the reason text names the annotation and both ways out.

`kelson rollback` is porcelain over the annotation. `internal/delivery/rollback` is deleted; what it did
— pick a revision, replay its manifests, apply them — is now "point an `OCIRepository` at a tag that
already exists".

### 6. Promotion is unchanged in substance and becomes a CR patch

[ADR-0016](0016-delivery-flows-v0.md) decision 2 stands entirely: promotion is a per-environment image
pin, `Environment.spec.components[].image`, resolved with the same precedence, and *"three existing
operations: read staging's deployed digest, write production's pin, deploy"*. The digest still comes
from the source environment's deployed revision rather than from its spec.

Two mechanical changes. The write is a patch to a custom resource rather than a byte splice into a
stored document, because [ADR-0027](0027-crd-native-control-plane.md) decision 6 ended byte fidelity in
the store. And the patch stamps

```
kelson.dev/promoted-from: <source-environment>@<revision>
```

on the target `Environment`. This is a small, deliberate walk-back of ADR-0016's *"promotion keeps no
record of its own"*: an annotation is not a promotion object, has no lifecycle, gates nothing and
approves nothing, but it turns "where did this image come from" from an archaeology exercise into a
`kubectl get`. It is auditability, not workflow, and the distinction ADR-0016 was protecting — no
approval state, no ordering, no promotion resource — is preserved.

### 7. SOPS survives, and it survives *better*, because decryption was never about git

kustomize-controller decrypts a `Kustomization`'s sources per-Kustomization, and it does not care
whether the source is a `GitRepository` or an `OCIRepository`. So [ADR-0022](0022-sops-age.md)'s
mechanism is intact end to end: `kelson secret set` still encrypts to the age recipients with kelson
holding no private key, the encrypted `Secret` manifests ship *inside the artifact* alongside the
workloads that reference them, and the `Kustomization` kelson now owns (decision 3) carries the
`spec.decryption.secretRef` pointing at `ageKeySecret`.

This amends ADR-0022's assumption that the transport is a git repository — decisions 3, 4 and 6 of that
ADR are written in terms of a git writer and a commit — and it *closes* that ADR's worst negative.
ADR-0022 §"Consequences" recorded that *"the environment's own Kustomization is still the operator's to
write… so 'committed but never decrypted' remains reachable by skipping a step"*. kelson now writes that
Kustomization, so the decryption block is not skippable, and the failure mode where ciphertext is
applied verbatim as a literal string is gone.

### 8. Renderer mode gates are deleted, and one becomes a gate-table row

Vacuous now — there is no non-Flux mode for them to refuse:

- `render/helm-requires-flux`
- `render/sops-requires-flux`
- `render/previews-require-flux`

Deleted along with the mode plumbing that fed them. ADR-0016's *"an author can write a valid document
that becomes invalid by changing `delivery.mode`"* is no longer a property the model has.

`render/release-requires-direct` is the opposite case and gets the opposite treatment. ADR-0019's
guarantee was *"the Job finished before the Deployments changed"*, and its own rationale said only
*"the mode where kelson performs the apply itself can stop between two resources"*. That mode is gone,
so `components[].release` loses its implementation and **moves into `internal/model`'s gate table**
(`$.spec.components[].release`, `notimplemented.go`) — validated, refused by name with a tracked
follow-up, rendering nothing. That is the mechanism this project already uses for a field it cannot
honour, and it is strictly better than silently dropping a migration.

The Flux-native replacement is known and not built: two `Kustomization`s with `dependsOn`, the first
containing the release Job with a health check, the second the workloads. ADR-0019's own "Revisit when"
predicted exactly this — *"at which point the direct-only gate becomes a mode-specific implementation
rather than a refusal, and this ADR is superseded rather than amended"* — except that the interposition
is now possible precisely *because* kelson owns the Kustomization. Tracked in the issue tracker.

### 9. What is deleted

Spec vocabulary:

- `Environment.spec.delivery` in full — the `Delivery` struct, `DeliveryMode`, `GitTarget`, and the
  `direct` / `flux` enum — and with them the `argocd` value, which had already outlived its adapter by
  a fortnight and is being removed as ADR-0012's own cleanup.
- `semantic/git-target-missing`, which existed to require a git target for a mode that no longer exists.

Code:

- `internal/delivery/direct` — the server-side applier, its wait logic and its JSONL history store.
- `internal/delivery/git` — the go-git writer, minus `ManifestFiles`, which is the artifact's layout
  function (ADR-0017 decision 10) and moves to the publisher.
- `internal/delivery/eject` — decision 10.
- `internal/delivery/rollback` — decision 5.
- `internal/serverstate`'s spec and history stores — ADR-0027 decision 7.

Surviving from `internal/delivery`: `flux`, `statemachine`, `install`, `uninstall`, `kube`, `dryrun`,
`provenance.go`. The `Adapter`/`Registry`/`Capabilities` seam that ADR-0001 introduced and ADR-0012
preserved is collapsed: there is one implementation, so the interface is deleted with it. ADR-0001's
*pure renderer* half is what actually carried the weight, and it is untouched.

### 10. `kelson eject` is deleted, because every deployment is already ejected

`eject` existed to prove ADR-0001's strongest claim — *"kelson becomes deletable; uninstalling leaves
running apps and a plain Kustomize repo"* — by converting an implicit repository into an explicit one.
The claim survives; the conversion has nothing left to do. Every revision kelson has ever deployed is an
immutable OCI artifact containing a flat directory of standard manifests, readable with tools that are
not kelson:

```sh
flux pull artifact oci://<registry>/kelson/<project>-<env>:<rev> --output ./manifests
kelson render -f project.yaml --env production        # the same bytes, offline
```

The anti-lock-in property is *stronger* than it was, because it no longer requires running a kelson
command to obtain it: it is the delivery mechanism, not an export of it. What is genuinely lost is the
one thing `--to-git` did that this does not — replaying history into a git repository with a commit per
revision. That is a registry-to-git conversion someone can write in a shell script, and it is not
kelson's job.

## Rationale

**Why OCI and not a git commit.** A git commit needs a forge credential inside the control plane —
a class of secret ADR-0009 deliberately keeps out of kelson, and the exact wall ADR-0017 decision 8 hit
when it considered a server-side publisher. It also needs a repository to exist, which the
self-contained case (someone with a cluster and no forge) does not have. A registry, by contrast, is
already a hard requirement of the build plane: kelson cannot deploy a built image without one. Choosing
OCI adds zero dependencies to the system as a whole and removes go-git, the git writer's in-memory
filesystem, and the commit/push/retry logic from it.

The users who *want* git in the loop still have it, and have it better: they write specs in a
repository, review them in a pull request, and kelson reconciles what was merged. Git stays the source
of truth for the things git is good at — authored documents, review, blame — and stops being a
transport for machine-generated output.

**Why not `ResourceSet` as the primary path.** flux-operator's `ResourceSet` is genuinely good and
kelson already uses it, for previews, because the *lifecycle* fan-out is the hard part there. As a
spine it is wrong three ways: it is a flux-operator CRD, so the spine would depend on a component
[ADR-0017](0017-pr-previews.md) is careful to consume-never-import because it is AGPL-3.0; flux-aio
does not ship it, so [ADR-0030](0030-flux-aio-install.md)'s lightweight substrate could not run kelson
at all; and it is a template engine in the cluster, which is the thing ADR-0016 decision 5 and ADR-0017
both refused on the grounds that *"the renderer stays concrete"*. `ResourceSet` stays exactly where
ADR-0017 put it: previews only.

**Why one publisher rather than two.** The preview pipeline already renders concrete manifests and
pushes a deterministic Flux OCI artifact. Building a second publisher for the spine would mean two
media-type decisions, two determinism tests and two chances to disagree about what an artifact is. The
convergence is the cheapest part of this whole ADR — the publisher exists, is tested, and gains a
caller.

**Why the generation, not a counter.** ADR-0013's history allocated revision numbers
(`NextRevision`), which is a distributed counter with all the usual problems. `.metadata.generation` is
allocated by the API server, monotonic per object, bumped exactly on spec change and not on status
writes. It is the revision number, already existing, already correct under concurrency.

**Why direct mode is not kept "for laptops".** It is the tempting exception, and it fails on its own
terms: a laptop cluster that runs kelson without Flux gets no chart delegation, no previews, no SOPS,
no drift correction and no pruning — that is, a different product with a worse feature set, which every
feature must then be tested against forever. ADR-0030 answers the actual concern (Flux is heavy for a
k3s node) by making the Flux install small, which is a substrate problem with a substrate solution.

**A registry-less fallback is recorded and not built.** Flux ≥2.7's `ExternalArtifact` lets a controller
publish an artifact through the Flux source API without a registry at all, which would close the
"cluster and no registry" case. It is recorded here so the option is not rediscovered, and it is not
built, because the build plane needs a registry anyway and an unused second publishing path is a second
publishing path to test.

## Consequences

**Positive.**

- One delivery path. Every feature is designed once, tested once, documented once — the argument
  ADR-0012 made for deleting one adapter, applied to the last one.
- Four mode gates and the entire notion of a mode-gated spec field disappear from the model. A document
  that validates renders, everywhere, always.
- Rollback stops being a replay and becomes a pointer move to bytes that already exist and cannot have
  changed.
- kustomize-controller does apply ordering, health assessment, pruning, drift correction and retry.
  kelson stops having opinions about all five.
- History costs nothing to store and is bounded only by the registry's retention.
- The SOPS "committed but never decrypted" failure mode is structurally unreachable.
- Determinism is end-to-end and testable: same spec, same artifact digest, same reconciliation.

**Negative — stated as plainly as the positives.**

- **Flux is now a hard requirement.** ADR-0001 sold "works without adopting GitOps" and that is
  withdrawn. There is no kelson without a reconciler in the cluster, and
  [ADR-0030](0030-flux-aio-install.md) exists because of this sentence.
- **A registry is now a hard requirement for deployment, not only for building.** Someone deploying a
  pre-built public image to a cluster previously needed no registry write access; now they need
  somewhere to push artifacts. This is the sharpest new edge in the whole rebuild, and the
  `ExternalArtifact` note above is the only recorded way out.
- **Release hooks stop working.** `components[].release` becomes a validated refusal with a tracked
  follow-up. Anyone whose deploy runs migrations loses the ordering guarantee ADR-0019 built, and gets
  an error message instead of a silent omission — honest, and still a regression.
- **Feedback is slower and less direct.** Direct mode applied and watched in one process. Now kelson
  pushes and waits for a controller to notice, so the fastest possible deploy is bounded by Flux's
  reconciliation, and a failure is reported through one more layer of translation.
- **kelson owns Kustomizations it did not own before**, reversing ADR-0022 §5's ownership boundary. An
  operator with strong opinions about their Flux objects now finds kelson writing some, in
  `kelson-system`, with `prune: true`.
- **Byte-faithful authored documents are gone from the control plane** (ADR-0027 decision 6), and
  promotion's byte-splice with them.
- **~5,000 lines of working, tested code are deleted** — the direct adapter, the git writer, eject,
  rollback, the adapter registry, the mode plumbing and their tests. It remains in history
  (`git log -- internal/delivery/direct`), and, unlike ADR-0012's Argo deletion, nothing about this one
  is expected to come back.
- **`eject` was a promise in the README and it is being removed**, which reads like a retreat even
  though the property it demonstrated is now the default. The documentation has to make that argument
  every time.
- **Previews and the spine now share a publisher**, so a change to the artifact format is a change to
  both. That is the intended coupling and it is still a coupling.

## Revisit when

- **Flux ships `ExternalArtifact` support kelson can rely on** (≥2.7, and in whatever distribution
  ADR-0030 installs). That is the registry-less path, and it is an addition to this spine rather than a
  replacement of it.
- **The two-`Kustomization` `dependsOn` split is built.** That is release hooks returning, and it
  supersedes [ADR-0019](0019-release-command-hook.md) rather than amending it — as ADR-0019 itself
  predicted.
- **Someone needs an adapter again.** The seam is deleted, not forbidden. A second reconciler would be
  a new ADR that has to answer ADR-0012's parity question with evidence this project did not have when
  it answered it twice before.
- **Registry retention starts deleting revisions someone wanted.** History is the tag list; a lifecycle
  policy on the registry is now, quietly, a data-retention policy on kelson's history, and nothing in
  kelson currently says so at the point where it matters.
