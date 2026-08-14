# ADR-0017: PR previews — the `previews:` block, the artifact tag, and what stage 1 does not do

- **Status:** Accepted
- **Date:** 2026-08-14

## Context

[ADR-0016](0016-delivery-flows-v0.md) decision 5 settled the *shape* of PR previews and nothing else:
kelson renders concrete manifests for each pull request and publishes them as an OCI artifact, and a
flux-operator `ResourceSet` + `ResourceSetInputProvider` pair consumes the artifact per pull request
and owns only the lifecycle. It left three questions that have to be answered before a line of code
can be written.

**What does an author write?** ADR-0016 names no field. Previews are not a property of a component —
every component in the project appears in a preview — and they are not a property of the Project
either, because the same project has environments that spawn previews and environments that must
not. The remaining home is the Environment, and that is where this ADR puts it.

**What tag does the preview artifact carry?** flux-operator exports `id` (the change request number)
and `sha` (the head commit) for every pull request it finds. An `OCIRepository` can pin either. The
two choices are not equivalent and one of them is wrong.

**Who does what, and when?** The renderer, the publisher and the cluster-side lifecycle machinery are
three separate pieces of work. Shipping them as one change would mean a large diff nobody can review
against a design nobody has agreed. This ADR fixes the contract between them so they can land
separately, and states plainly what the first slice leaves broken.

Two constraints framed the answers more than preference did.

**flux-operator's API is consumed, never imported.** flux-operator is AGPL-3.0 and kelson is MIT, so
kelson knows this API the way it knows CloudNativePG's: as YAML it writes and unstructured reads it
performs, with no Go module of theirs anywhere in this repository
(docs/architecture.md, "Living with flux-operator"). Field names below were verified against the
upstream `fluxcd.controlplane.io/v1` reference for `ResourceSet` and `ResourceSetInputProvider`;
they are written, not linked against, and a rename upstream is a golden-file change here.

**The renderer is pure and stays pure.** A `ResourceSet` is a template engine living in the cluster.
The entire reason ADR-0016 chose artifact-per-PR is that kelson's own output must never contain a
hole another controller fills in — that property is what the diff, the dry-run and every golden file
rest on. So the one template kelson writes has to be small enough that a reader can hold it in their
head, and it must contain no application manifests at all.

## Decision

### 1. The authoring surface is `Environment.spec.previews`

Previews are an environment-class concern — *this environment spawns per-pull-request children* — so
they are declared on the Environment document:

```yaml
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata:
  name: staging
spec:
  project: checkout
  namespace: checkout-staging
  delivery:
    mode: flux
    git: { repo: git@github.com:acme/deploy.git, path: checkout/staging }
  previews:
    provider: github                                   # github | gitlab
    repo: https://github.com/acme/checkout             # whose pull requests become previews
    secretRef: github-auth                             # a Secret NAME, never a token
    interval: 10m                                      # how often the forge is polled
    filter:
      labels: [deploy/preview]
      includeBranch: "^feat/.*"
      excludeBranch: "^wip/.*"
      limit: 10
    skip:
      labels: [deploy/preview-pause, "!ci/passed"]
    artifacts:
      repository: oci://ghcr.io/acme/checkout-previews
      secretRef: ghcr-auth                             # optional pull secret
```

Four things about that block are decisions, not defaults.

**`repo` is the source repository, not the delivery repository.** `delivery.git.repo` is where kelson
commits rendered manifests; `previews.repo` is where the pull requests live. They are usually
different repositories and there is no defaulting between them, because guessing wrong would point
the poller at the wrong forge and produce an empty preview set that looks like "no open PRs".

**Auth is a Secret name and only a Secret name.** There is no token field, no username field and no
inline credential anywhere under `previews:` — [ADR-0009](0009-secrets.md) and [#79](https://github.com/dafrie/kelson/issues/79)
put credentials in Secrets somebody else manages, and the spec carries a reference. The Secret's own
shape is flux-operator's business: basic auth (`username`/`password`) or the GitHub App keys, both
documented upstream. kelson never reads it.

**`filter.limit` defaults to 10, not to flux-operator's 100.** The default is a cost posture. An
environment that quietly stands up a hundred preview namespaces the first time somebody bulk-labels a
backlog is a surprise that arrives as a cluster bill, and the fix — raising one number — is cheaper
than the discovery. The ceiling is always rendered, so what the cluster enforces is always visible in
the manifest.

**`skip.labels` is CI gating and belongs in the spec.** A preview updated to a commit whose image has
not finished building is a broken preview with a green input, and the upstream remedy is a label the
CI job adds before it builds and removes when it finishes. That is a property of how a team's CI
works, so it is authored, not inferred.

### 2. The artifact is tagged with the commit SHA

The rendered `OCIRepository` pins `ref.tag: << inputs.sha >>` — the head commit of the pull request —
and **not** `pr-<< inputs.id >>`.

A per-PR tag is mutable by construction: the only way to update a preview would be to repoint the
same tag, which discards the artifact history for that pull request, makes "what is actually running
in this preview" unanswerable from the registry, and makes rollback-within-a-PR impossible. A SHA tag
is immutable, one artifact per push, and it is already the convention CI uses for the image
(`github.event.pull_request.head.sha`) — so the preview's manifests and the image they reference are
addressed by the same string. When the input's `sha` changes, the `OCIRepository` object changes, and
source-controller fetches the new artifact immediately rather than on its polling interval.

The `id` still appears, in the names: it is what a human types when they go looking, and it is stable
across pushes in a way a SHA is not.

### 3. Names and namespaces are `<project>-<environment>-pr<id>`

One scheme, derived from names that already exist:

| Object | Name | Namespace |
|---|---|---|
| `ResourceSetInputProvider` | `<project>-<environment>-previews` | the environment's namespace |
| `ResourceSet` | `<project>-<environment>-previews` | the environment's namespace |
| per-PR `OCIRepository` | `<project>-<environment>-pr<id>` | the environment's namespace |
| per-PR `Kustomization` | `<project>-<environment>-pr<id>` | the environment's namespace |
| the preview itself | — | `<project>-<environment>-pr<id>` |

The lifecycle machinery lives in the parent environment's namespace and the preview's own objects
live in a namespace of their own. That split is what makes teardown a namespace delete plus a
`prune`, and it is what keeps a preview from being able to touch the environment it previews.

**The cap is on `<project>-<environment>`, which must be at most 54 characters.** Both derived names
add exactly nine: `-previews` for the lifecycle pair, and `-pr` plus a change-request number of up to
six digits for the preview. Sixty-three is the DNS-1123 label limit a namespace must satisfy, so 54 is
the number, and the renderer refuses at the field rather than letting flux-operator fail at reconcile
time on a name it templated. Six digits is a real assumption: a repository that reaches pull request
1,000,000 renders a namespace one character too long, and the failure is upstream's rather than
kelson's. Naming it here is cheaper than discovering it there.

### 4. The `ResourceSet` template contains an `OCIRepository` and a `Kustomization`, and nothing else

The whole of `spec.resourcesTemplate`:

```yaml
---
apiVersion: source.toolkit.fluxcd.io/v1beta2
kind: OCIRepository
metadata:
  name: checkout-staging-pr<< inputs.id >>
  namespace: checkout-staging
spec:
  interval: 10m
  url: oci://ghcr.io/acme/checkout-previews
  ref:
    tag: << inputs.sha >>
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: checkout-staging-pr<< inputs.id >>
  namespace: checkout-staging
spec:
  interval: 10m
  prune: true
  wait: true
  targetNamespace: checkout-staging-pr<< inputs.id >>
  sourceRef:
    kind: OCIRepository
    name: checkout-staging-pr<< inputs.id >>
  path: ./
```

**No application manifests appear here, ever.** The artifact carries them, concrete, rendered by the
same renderer every other kelson output comes from. If a Deployment ever needs to appear in this
template, artifact-per-PR has failed and ADR-0016 decision 5 needs revisiting — it is not a small
extension of this one.

**`targetNamespace` is a boundary, not a convenience.** The artifact is rendered for the preview's
namespace and already names it on every resource, so `targetNamespace` is normally redundant. It is
written anyway because it makes the blast radius of a mis-published artifact a property of the
manifest kelson wrote rather than of the artifact somebody else pushed: whatever namespace the
artifact's resources claim, they land in the pull request's. The one thing it cannot constrain is the
cluster-scoped `Namespace` object the artifact carries — kelson's renderer emits one for every set,
and this is where the preview's namespace comes from. Stage 2's publisher is therefore *bound* to the
scheme in decision 3, and a publisher that renders a different namespace name creates a stray one.

**`prune: true` plus `wait: true` is what makes [#103](https://github.com/dafrie/kelson/issues/103)
hold.** A preview database is a data component inside the per-PR artifact, so it is applied by this
`Kustomization` and pruned by this `Kustomization`. There is no second thing to remember to delete,
which is the entire reason ADR-0016 put preview databases inside the set. The storage-capability gate
is unchanged: a preview whose environment cannot host the requested preset fails to render, exactly
as the parent environment would.

**`commonMetadata` carries the provenance labels.** `app.kubernetes.io/managed-by: kelson`,
`kelson.dev/project` and `kelson.dev/environment` land on every object the `ResourceSet` generates, so
the children are discoverable by the same selector as everything else kelson writes.

The `OCIRepository` is written at `source.toolkit.fluxcd.io/v1beta2`, the same version kelson already
writes for chart sources (`internal/renderer/helm.go`). One API version per group across this
codebase is a rule worth more than tracking each kind's promotion date: the writer and the readers in
`internal/delivery/flux` would otherwise drift apart one kind at a time.

### 5. Previews are Flux-only, with the same gate shape as Helm components

An Environment carrying `previews:` renders only when its delivery mode is `flux`. Anything else is
the structured render error `render/previews-require-flux`, naming the environment, the mode and the
fix — mirroring `render/helm-requires-flux` field for field.

ADR-0016's Helm gate asked that any future field wanting the same exemption cite its paragraph
deliberately. This one does, and the argument is stronger rather than weaker: a `HelmRelease` applied
without helm-controller is inert, but a `ResourceSet` applied without flux-operator is inert *and* the
CRD is usually not even served, so a direct-mode apply fails on an unknown kind with a message about
`fluxcd.controlplane.io/v1` that says nothing about previews. The gate is decided from spec data in
the pure renderer, so the same document renders the same way against every cluster; whether
flux-operator is actually installed is a ClusterProfile finding
(`internal/clusterprofile`, [#157](https://github.com/dafrie/kelson/issues/157)), never a rendering
decision.

### 6. Stage 1 delivers the cluster-side machinery only

The work splits three ways, and this ADR commits only the first:

1. **Stage 1 (this ADR's implementation).** The `previews:` block on the Environment, its validation,
   and the two manifests it renders — the `ResourceSetInputProvider` and the `ResourceSet`.
2. **Stage 2.** The publisher: on pull request events, the server or a CI job renders the preview with
   the same renderer and pushes the manifests as an OCI artifact tagged with the head SHA, to
   `previews.artifacts.repository`. It reuses `internal/build/registry` for the reference and
   credential machinery and `internal/delivery/eject`'s layout vocabulary for what goes in the
   artifact.
3. **Stage 3.** Surfacing previews in the UI and the API.

**Until stage 2 lands, a rendered `ResourceSet` waits for artifacts nobody publishes.** flux-operator
will find the labelled pull requests, create an `OCIRepository` per pull request, and report that the
artifact does not exist. That is the honest state of the feature after stage 1 and the documentation
says so in those words rather than describing a working preview system.

### 7. Preview children are not enumerated by kelson's Status and History RPCs

A preview is a child environment that kelson did not record: no `Environment` document describes it,
no delivery history entry exists for it, and the thing that created it is flux-operator reacting to a
label on a pull request. `StatusService` and the delivery history answer questions about environments
kelson deployed, and in stage 1 they do not walk preview namespaces, do not aggregate preview health,
and do not report how many previews are running.

What exists instead is the provenance labels and the naming scheme: `kelson.dev/project` and
`kelson.dev/environment` on every generated object, in namespaces named `<project>-<environment>-pr<id>`.
A `kubectl get` with a selector answers the question today. An API that answers it is stage 3, and it
is a new decision about what a preview *is* to kelson's model — not an extension of this one.

## Rationale

- **The Environment is the only document that can carry this.** Component-level is wrong (a preview is
  the whole project), Project-level is wrong (staging previews, production must not), and a new
  document kind would be a fourth noun for something that is a property of an existing one.
- **The SHA tag is the choice that keeps the registry honest.** Every alternative makes the answer to
  "what is running in preview 412" depend on when you ask.
- **A nine-character budget on both derived names is a coincidence worth exploiting.** One cap, one
  error, one number in the documentation, instead of a table of limits per object kind.
- **The smallest possible template is the strongest possible statement of ADR-0016 decision 5.** Two
  resources, four templated holes, no conditionals: a reader can verify by inspection that no
  application manifest can appear in it.
- **Splitting the work at the artifact boundary means each half is testable alone.** Stage 1 is golden
  files against a pure function. Stage 2 is a publisher whose output is bytes the renderer already
  produces. Neither needs the other to be reviewed.

## Consequences

**Positive.** The authoring surface is nine fields and no new document. The renderer gains one file
and no new capability — the two manifests are built with the same node builders and stamped with the
same provenance as everything else. Preview databases inherit teardown for free, which was #103's
entire ask. kelson's own PR poller and GC, which the M10 epic forbids, are still not written and now
have a named reason not to be.

**Negative — stated as plainly as the positives.**

- **Stage 1 ships something that does not work yet.** An author can write `previews:`, render, deploy,
  and watch flux-operator report missing artifacts indefinitely. That is a feature in a half-state, and
  the mitigation is documentation rather than a gate: gating the field behind
  [#141](https://github.com/dafrie/kelson/issues/141)'s `schema/not-implemented` would be the more
  conservative call, and it was rejected because the cluster-side machinery is genuinely rendered,
  genuinely correct and genuinely useful to review before the publisher exists. The coverage rule is
  satisfied because every field is consumed by the renderer — the field is not silent, it is early.
- **The publisher and the renderer agree by convention, not by type.** Stage 2 must render the preview
  into the namespace decision 3 names and push to the tag decision 2 names. Nothing checks that, and
  the failure mode of getting it wrong — a stray namespace, or an `OCIRepository` pointing at a tag
  that will never exist — is quiet. A shared constant is the obvious mitigation and it is stage 2's to
  add.
- **The `ResourceSet` runs with flux-operator's own permissions.** Neither `spec.serviceAccountName` on
  the `ResourceSet` nor on the generated `Kustomization` is set, because kelson has no field for it,
  which means previews reconcile with whatever the operator can do cluster-wide. On a multi-tenant
  cluster that is too much. The upstream remedy is a per-namespace ServiceAccount and impersonation,
  and adding it is a spec field plus an RBAC story that this ADR does not attempt.
- **There is no TTL and no cost control beyond `filter.limit`.** A preview lives as long as its pull
  request is open and labelled. A pull request open for three months holds a database for three
  months. flux-operator has no TTL either, so this is not a gap kelson can close by configuration —
  it is a scheduled reaper somebody has to write, and it is future work with nobody's name on it yet.
- **A preview's status and history are invisible to kelson.** Everything in decision 7 is a thing a
  user will reasonably expect and not get: no preview list, no per-preview health, no "which PRs are
  deployed" answer from the API. `kubectl` answers it and kelson does not.
- **Previews inherit the Helm gate's cost.** An Environment with `previews:` is valid until somebody
  changes `delivery.mode`, at which point the same document stops rendering. That is now the second
  delivery-mode-gated surface, and the Helm precedent's warning stands: this is a decision taken
  twice, not a pattern.
- **Only GitHub and GitLab are offered.** flux-operator also supports Azure DevOps, Gitea/Forgejo and
  AWS CodeCommit change requests. Each is a one-line addition to an enum and a mapping, and each is
  also a shape nobody here has run; the enum stays at two until somebody wants a third and can say
  what it does.

## Revisit when

- **Stage 2 lands the publisher.** That is when the convention in this ADR's second negative becomes a
  shared constant, and when the documentation stops having to say the feature waits for artifacts.
- **Anyone needs a preview to run under its own ServiceAccount.** That is a spec field, an RBAC
  decision and probably a per-preview `Role`; it meets multi-tenancy ([#60](https://github.com/dafrie/kelson/issues/60), [#84](https://github.com/dafrie/kelson/issues/84)) before it meets this
  ADR.
- **TTL or a preview budget becomes real.** A reaper is a clock, which the renderer may not have
  ([ADR-0001](0001-hybrid-state-model.md)) — so it is a controller or a server-side job, and where it lives is the decision, not
  whether previews should expire.
- **The API needs to enumerate previews.** That is decision 7 reopening, and it starts by deciding
  whether a preview is a first-class Environment in kelson's model or stays a thing the labels
  describe.
