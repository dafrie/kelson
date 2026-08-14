# ADR-0017: PR previews — the `previews:` block, the artifact tag, and what stage 1 does not do

- **Status:** Accepted (2026-08-14: [ADR-0028](0028-delivery-spine.md) adopts this ADR's artifact and
  publisher as the delivery spine's, so previews and ordinary deploys share one OCI format and one
  determinism test; decision 5's Flux-only gate becomes vacuous and `ResourceSet` stays previews-only,
  as recorded here.)
- **Date:** 2026-08-14
- **Amended:** 2026-08-14 — [Stage 2, the publisher](#stage-2--the-publisher-amended-2026-08-14)
  (decisions 8–11)
- **Amended:** 2026-08-14 — [Stage 3, the API and the UI](#stage-3--the-api-and-the-ui-amended-2026-08-14)
  (decisions 12–15), which reopens decision 7

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
   artifact. **Landed 2026-08-14 — see decisions 8–11, which settle "the server or a CI job" as a CI
   job.**
3. **Stage 3.** Surfacing previews in the UI and the API. **Landed 2026-08-14 — see decisions
   12–15.**

**Until stage 2 lands, a rendered `ResourceSet` waits for artifacts nobody publishes.** flux-operator
will find the labelled pull requests, create an `OCIRepository` per pull request, and report that the
artifact does not exist. That was the honest state of the feature after stage 1. It is now the state
of an environment whose CI does not run `kelson preview publish`, which is the same symptom with a
documented cause.

### 7. Preview children are not enumerated by kelson's Status and History RPCs

> **Reopened by decision 12.** `PreviewService.ListPreviews` now enumerates them. What stands
> unchanged is everything this decision says about `StatusService` and the delivery history: they
> still do not walk preview namespaces and still do not aggregate preview health. The answer below —
> that a preview is what the labels and the naming scheme describe — is the answer decision 12 built
> on rather than replaced.

A preview is a child environment that kelson did not record: no `Environment` document describes it,
no delivery history entry exists for it, and the thing that created it is flux-operator reacting to a
label on a pull request. `StatusService` and the delivery history answer questions about environments
kelson deployed, and in stage 1 they do not walk preview namespaces, do not aggregate preview health,
and do not report how many previews are running.

What exists instead is the provenance labels and the naming scheme: `kelson.dev/project` and
`kelson.dev/environment` on every generated object, in namespaces named `<project>-<environment>-pr<id>`.
A `kubectl get` with a selector answers the question today. An API that answers it is stage 3, and it
is a new decision about what a preview *is* to kelson's model — not an extension of this one.

## Stage 2 — the publisher (amended 2026-08-14)

Stage 1 left the artifacts unpublished and named three questions it did not answer: *who* publishes,
*what exactly* the artifact contains, and what to do about the convention this ADR admitted was
unchecked. This section answers them and is the second half of the decision above, not a new one.

### 8. The publisher is a CLI verb in the application repository's CI

`kelson preview publish -f <spec> --env <name> --pr <number> --sha <commit> [--image <ref>]`, run on
pull request events, beside the build that produced the image.

That is where the inputs already are. The checkout of the pull request's head is on the runner, the
image was built there seconds earlier, and the registry credential is in the environment because the
build step just pushed with it. Nothing has to be fetched, impersonated or stored for the publish to
happen, and the composition is one line of YAML: build, then publish the digest.

The alternatives were real and are worth recording.

**A server-side publisher on a forge webhook.** kelson-server receives a `pull_request` event, checks
out the head, renders and pushes. It needs a webhook per repository, a forge credential kelson does
not hold today (ADR-0009 keeps credentials in Secrets somebody else manages, and this would be a new
class of them), a checkout of somebody else's source *inside the control plane*, and an answer to
"which image does this preview run?" that CI already has and the server does not. It also puts the
control plane in the data path for every pull request, which is the shape ADR-0010 avoided for builds.

**An in-cluster job triggered by the `ResourceSetInputProvider`.** flux-operator already knows about
each pull request, so a job could render on its behalf. But it would clone the source in the cluster,
would have to discover the image by convention, and — decisively — the artifact would then be produced
by the same lifecycle that consumes it, so a preview could never be published *before* its
`OCIRepository` exists. The first reconcile would always fail.

**A `Publish` RPC on kelson-server.** Not rejected, deferred: a server that renders and pushes on
request is a small addition to this decision rather than a change to it, and nothing in the flow needs
it. When it lands it will call the same package this CLI verb calls.

The cost of choosing CI is that publishing is a step somebody has to add to a workflow, and a
repository that forgets it gets exactly the stage 1 experience: an `OCIRepository` reporting a missing
artifact. That is a visible failure with a documented fix, which is why it was preferred to the
invisible ones above.

`kelson preview render` is the same render printed instead of pushed — the dry rung of the same
ladder `kelson diff` and `kelson deploy --dry-run` climb, and the place to look when a preview is not
what it should be.

### 9. The convention is now shared code

This ADR's second negative — "the publisher and the renderer agree by convention, not by type" — is
closed by `internal/preview/naming`: one package spelling the `-pr<id>` namespace, the lifecycle pair's
name, the 54-character cap and its six-digit reservation, the tag, and the `<< inputs.id >>` /
`<< inputs.sha >>` holes themselves. `internal/renderer/previews.go` builds the `ResourceSet` template
out of it and `internal/preview` renders and tags out of it, so neither side spells a preview name
itself. A contract test substitutes the inputs flux-operator would substitute and asserts that the tag
the cluster pins and the namespace it targets are the ones a publish actually produces.

It is shared code and not a shared *type*, deliberately: a type would have to be carried through the
pure renderer's node builders and through the artifact's descriptors to be worth anything, and the
failure it would prevent is one that four exported functions and one test already prevent.

### 10. What the artifact is

A Flux OCI artifact: config media type `application/vnd.cncf.flux.config.v1+json`, one layer of
`application/vnd.cncf.flux.content.v1.tar+gzip` holding the rendered set as a flat directory of YAML
files, inside an `application/vnd.oci.image.manifest.v1+json` manifest. The layout is
`internal/delivery/git`'s `ManifestFiles` — the same function that lays out a Git-mode commit, as
stage 1 said it would be — so an artifact's contents and an ejected repository's contents are the same
bytes with the same names, and the `Kustomization`'s `path: ./` finds them at the root.

It carries `org.opencontainers.image.revision` (`pr-<id>@sha1:<commit>`), `.source` (the forge
repository), `.created`, and `kelson.dev/project`, `kelson.dev/environment`, `kelson.dev/preview`, so
an artifact in a registry answers the same "whose is this?" question a resource in a cluster does.

**The artifact is deterministic, digest included.** The tar is written in render order with fixed
modes, no ownership and a fixed timestamp, and `created` is that same fixed timestamp rather than the
wall clock. Republishing an unchanged commit therefore produces a digest the registry already holds
and uploads nothing. The build time is not lost — the commit's date is in the repository, the revision
annotation names the commit, and the registry records the push — and what is gained is that "the same
render produces the same artifact" is a property a test asserts instead of a hope. This is ADR-0001's
determinism rule applied one layer out.

### 11. A preview's hostnames belong to the change request

The preview render is the parent environment's render with three substitutions and nothing else: the
namespace becomes the preview's, the `previews:` block is dropped (a preview that kept it would render
a `ResourceSetInputProvider` of its own and every pull request would spawn previews of itself), and
every hostname gains the change request.

The hostname rewrite suffixes the *first DNS label*: `web.staging.acme.run` becomes
`web-pr412.staging.acme.run`. Inserting a label instead would fall outside the single-label wildcard
certificate and wildcard DNS record that already serve the parent environment, and break TLS on the
first preview.

It applies to authored hostnames too, which is the uncomfortable half and the safety-critical one: a
component that names `api.acme.com` must not have its preview claim `api.acme.com`. Two previews of
one environment would otherwise fight over one hostname, and a preview would be able to take
production's traffic. So a preview never serves the hostname the spec asks for, and that is deliberate
rather than a limitation to fix later.

## Stage 3 — the API and the UI (amended 2026-08-14)

Decision 7 said kelson does not enumerate preview children, and said why: a preview is an environment
kelson did not record, so answering "which pull requests are deployed" is a decision about what a
preview *is* to kelson's model rather than an extension of anything above. This section takes that
decision, and takes the smaller of the two available.

### 12. A preview stays a thing the cluster describes

`PreviewService.ListPreviews(spec, environment)` returns one environment's previews. It does **not**
make a preview a first-class Environment: no document is written for it, no history entry is recorded,
`StatusService` still does not walk preview namespaces, and nothing in the schema can create or delete
one. The service is read-only and has no field a write could arrive in, for the same reason decision 8
put the publisher in CI — previews are created by a CI job and destroyed by flux-operator, so a write
RPC would either duplicate the publisher without the checkout and the image reference CI already has,
or delete an object the operator recreates on its next poll.

The alternative was to promote a preview to an Environment kelson knows about, with its own history
and its own status. That is a larger model change than this feature justifies: it would give kelson a
second way for an environment to come into existence, one whose lifecycle kelson does not own, and
every RPC that takes an environment name would then have to answer what it means for a preview.

### 13. The pair is the source, not the namespaces

The read enumerates the per-change-request `OCIRepository` and `Kustomization` in the *environment's*
namespace, and derives the preview's namespace from their names. It does not list preview namespaces.

The reason is the failure this ADR itself calls the visible one. An environment whose CI does not run
`kelson preview publish` has, per change request, an `OCIRepository` reporting a missing artifact, a
`Kustomization` waiting on it, and **no namespace at all** — so a namespace-based enumeration answers
"no previews" for precisely the case a reader most needs to see. The pair exists for every preview
flux-operator knows about; the namespace exists only for the ones that landed.

The `ResourceSetInputProvider`'s exported inputs are deliberately not read. They answer "which change
requests are labelled", which is a different question, and answering it would mean kelson spelling an
upstream status field to report something no kelson manifest created.

**A preview's phase is decided server-side**, in `internal/delivery/flux`, out of the two Ready
conditions: `ready`, `applying`, `awaiting-artifact`, `failed`, `unknown`. That is the delivery state
machine's rule applied again ([#37](https://github.com/dafrie/kelson/issues/37)) — one place decides
what a state means and every client renders the same word. `awaiting-artifact` is its own phase rather
than a kind of failure because its usual cause is a CI step nobody added, and "failed" would send a
reader to the manifests instead of to the workflow.

**Hostnames are observed, not derived.** They are read from the HTTPRoutes in the preview's own
namespace rather than computed from the parent's render through `naming.Host`. Deriving them would
report what kelson *would* publish; reading them reports what the preview serves, which is the only
version of that answer worth showing next to a phase. The cost is a cluster-scoped read the deploy
chart cannot grant — a preview's namespace does not exist when the chart is installed — so that one
read degrades to silence, and the UI says the absence is not a claim.

### 14. The gate is shown, never worked around

An environment that declares `previews:` outside Flux mode gets its settings, no cluster read, and the
renderer's own `render/previews-require-flux` in the response's `errors`. The refusal is obtained from
the renderer by name rather than by rendering the environment and filtering: a render fails for a dozen
unrelated reasons — an unresolved image, a missing overlay — and answering "why are there no previews"
with the first of those would be worse than the gate's own sentence. This is the reason
`renderer.PreviewsRequireFlux` is exported and its siblings are not.

The UI shows that error in the panel every other structured refusal reaches a reader through, with its
code and its remediation intact, and it keeps showing the configuration underneath: a reader whose mode
is wrong still needs to see what they configured.

### 15. The previews block is authored in the form, under the same byte guard as everything else

The edit form gains the `previews:` block and the `delivery:` stanza it depends on, held to the rule
`ui/src/spec/edit.ts` already enforces: the form may edit a document only when reading and rewriting it
reproduces the stored bytes exactly. So a block written in the styling this ADR and docs/model.md show
— label lists as flow sequences — round-trips and stays editable; the same block written as a block
sequence is read, displayed, and left read-only because the rebuild would restyle it; a spacing the
reader cannot reproduce is refused outright rather than guessed at.

Two things the form states rather than enforces. It does not disable the control outside Flux mode: the
server's refusal explains itself and a greyed-out checkbox cannot. And it does not pretend the block is
sufficient — the field notes name `kelson preview publish` as the other half, because an environment
configured in the UI and nowhere else gets change requests whose artifacts never arrive, which is
exactly the half-state decision 8 left behind.

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

- **Stage 1 ships something that does not work yet.** ~~An author can write `previews:`, render, deploy,
  and watch flux-operator report missing artifacts indefinitely.~~ *Closed by stage 2 (decision 8): the
  publisher exists, and an environment whose CI runs `kelson preview publish` gets working previews. A
  repository that does not add the step still gets the half-state, now with a documented fix.* The
  original argument stands as recorded: gating the field behind
  [#141](https://github.com/dafrie/kelson/issues/141)'s `schema/not-implemented` would have been the more
  conservative call, and it was rejected because the cluster-side machinery is genuinely rendered,
  genuinely correct and genuinely useful to review before the publisher exists.
- **The publisher and the renderer agree by convention, not by type.** ~~Nothing checks that.~~ *Closed
  by stage 2 (decision 9): the scheme is `internal/preview/naming`, both sides call it, and a contract
  test substitutes flux-operator's inputs to assert that the tag the cluster pins and the namespace it
  targets are the ones a publish produces. It is shared code, not a shared type — the reason is in
  decision 9.*
- **A preview never serves the hostname the spec asks for.** Decision 11 rewrites every hostname,
  authored ones included, because the alternative is a preview claiming production's traffic. The cost
  is that a component whose behaviour depends on its own hostname — an OAuth redirect URI, a CORS
  allow-list, a cookie domain — behaves differently in a preview than in the environment it previews,
  and kelson does not tell it what its preview hostname is. An `env:` value carrying the rendered
  hostname would fix that and is not attempted here.
- **An artifact repository on a port cannot be authored.** `previews.artifacts.repository` is validated
  by reading any `:` after `oci://` as a tag, so `oci://registry.internal:5000/acme/previews` is
  rejected as "carries a tag or a digest". That is a stage 1 validation bug this stage exposed rather
  than caused — the publisher itself parses the reference correctly — and it makes a self-hosted
  registry on a non-default port unusable for previews until the check learns the distribution
  grammar's rule (a `:` introduces a tag only when no `/` follows it).
- **Nothing verifies who published an artifact.** The `OCIRepository` fetches whatever is tagged with
  the head commit, from whoever could write to the repository. Flux supports cosign and notation
  verification and kelson writes no `spec.verify`, so the trust boundary is the registry's write
  access. Signing previews is a spec field plus a key story, and it belongs with the same decision for
  the images kelson builds rather than being invented here for previews alone.
- **Published artifacts are never deleted.** Teardown deletes the preview's namespace and its two
  objects; the artifacts stay in the registry, one per push, forever. That is the deliberate cost of
  the immutable-tag choice in decision 2 — the history is the feature — but it is storage nobody
  reclaims, and registry retention policy is the only tool for it today.
- **The `ResourceSet` runs with flux-operator's own permissions.** Neither `spec.serviceAccountName` on
  the `ResourceSet` nor on the generated `Kustomization` is set, because kelson has no field for it,
  which means previews reconcile with whatever the operator can do cluster-wide. On a multi-tenant
  cluster that is too much. The upstream remedy is a per-namespace ServiceAccount and impersonation,
  and adding it is a spec field plus an RBAC story that this ADR does not attempt.
- **There is no TTL and no cost control beyond `filter.limit`.** A preview lives as long as its pull
  request is open and labelled. A pull request open for three months holds a database for three
  months. flux-operator has no TTL either, so this is not a gap kelson can close by configuration —
  it is a scheduled reaper somebody has to write, and it is future work with nobody's name on it yet.
- **A preview's status and history are invisible to kelson.** ~~Everything in decision 7 is a thing a
  user will reasonably expect and not get: no preview list, no per-preview health, no "which PRs are
  deployed" answer from the API. `kubectl` answers it and kelson does not.~~ *Half closed by stage 3
  (decisions 12–13): `ListPreviews` answers which change requests are running, at which commit, in
  which namespace, on which hostnames, and why one is not. What is still true is the rest of decision
  7 — there is no delivery history for a preview and nothing to roll one back to, because kelson never
  recorded a revision for it. A preview's past remains the registry's and the forge's.*
- **A preview's hostnames need a cluster-scoped read the chart does not grant.** Decision 13 reads them
  from the preview namespace's HTTPRoutes, and that namespace does not exist when the deploy chart is
  installed, so no namespaced Role can cover it. An install with the chart's RBAC sees its previews and
  not their hostnames. The read fails soft for exactly that reason, which means "no hostnames" is a
  state with two causes the API does not distinguish.
- **Previews inherit the Helm gate's cost.** An Environment with `previews:` is valid until somebody
  changes `delivery.mode`, at which point the same document stops rendering. That is now the second
  delivery-mode-gated surface, and the Helm precedent's warning stands: this is a decision taken
  twice, not a pattern.
- **Only GitHub and GitLab are offered.** flux-operator also supports Azure DevOps, Gitea/Forgejo and
  AWS CodeCommit change requests. Each is a one-line addition to an enum and a mapping, and each is
  also a shape nobody here has run; the enum stays at two until somebody wants a third and can say
  what it does.

## Revisit when

- ~~**Stage 2 lands the publisher.**~~ *Done, 2026-08-14: decisions 8–11 above. The convention became
  shared code rather than a shared constant, and the documentation no longer says the feature waits for
  artifacts.*
- **kelson-server grows a `Publish` RPC.** Decision 8 defers rather than rejects it. The question to
  answer then is not "should the server publish?" but "where does the server get the source checkout
  and the image reference that CI already has?" — and if the answer is "from the caller", the RPC is a
  thin wrapper over the same package and this decision does not change.
- **A preview needs to know its own hostname.** Decision 11's negative: an OAuth redirect URI or a
  cookie domain configured for the parent environment is wrong in every preview. The fix is an
  `env:` value carrying the rendered hostname, which is a new authoring surface and a question about
  what else a preview should be told about itself.
- **Anyone needs a preview to run under its own ServiceAccount.** That is a spec field, an RBAC
  decision and probably a per-preview `Role`; it meets multi-tenancy ([#60](https://github.com/dafrie/kelson/issues/60), [#84](https://github.com/dafrie/kelson/issues/84)) before it meets this
  ADR.
- **TTL or a preview budget becomes real.** A reaper is a clock, which the renderer may not have
  ([ADR-0001](0001-hybrid-state-model.md)) — so it is a controller or a server-side job, and where it lives is the decision, not
  whether previews should expire.
- ~~**The API needs to enumerate previews.**~~ *Done, 2026-08-14: decisions 12–15. It stayed a thing
  the labels and the naming scheme describe. Revisit when somebody needs a preview's **history** —
  which commits it has run, and a way back to one — because that is the half decision 12 did not take,
  and taking it means kelson recording revisions for environments it did not create.*
- **A preview needs a hostname the chart's RBAC can reach.** Decision 13's negative. The fix is either
  a cluster-scoped HTTPRoute read the installer opts into, or deriving hostnames from the parent's
  render — and the second is a different answer, not a cheaper one, because it reports what kelson
  would publish rather than what the preview serves.
