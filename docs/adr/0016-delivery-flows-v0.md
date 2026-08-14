# ADR-0016: The four M10 delivery flows, and what each one is in v0

- **Status:** Accepted (2026-08-14: the delivery-mode gate of decision 4, and the gate-as-precedent
  paragraph, are superseded by [ADR-0028](0028-delivery-spine.md) — there is one delivery path, so
  nothing is mode-gated. Decisions 1, 2, 3 and 5 stand; promotion is still a per-environment image pin
  and a spec edit, now written as a CR patch.)
- **Date:** 2026-08-13

## Context

[M10](https://github.com/dafrie/kelson/issues/11) is scoped as *Environments & promotion*, and the
epic names one mechanism precisely — PR previews are delegated to flux-operator, kelson's own PR
poller and GC must not exist — while leaving "promotion flows between environments" as a line item
with no shape. A 2026-08-13 review reframed the milestone around four delivery flows that users ask
for by name and that kelson has no answer to today:

1. **Manual upload** — "I have an artifact, deploy it", without kelson building anything.
2. **Promotion** — "what runs in staging, run it in production", without rebuilding.
3. **Canary** — "shift traffic gradually and roll back on bad signals".
4. **Helm components** — "this dependency ships as a chart; run it beside my components".

Plus the fifth shape the epic already commits to and now needs a rendering answer: **PR previews**.

A research pass produced four facts that carried the decisions below more than any preference did.

**Blob-then-fetch is the only shape a source upload has.** `internal/build`'s `Request` documents the
rule as a boundary, not a convenience: the build pod clones the source itself, *"nothing uploads a
tarball through kelson"*, which keeps the control plane out of the data path. Every platform that does
accept an upload lands in the same place from the other side — the client puts the archive somewhere
the builder can read (object storage behind a signed URL, or an OCI blob) and the control plane only
ever passes a reference. So a tarball path is not a small feature on the existing build plane. It is
either a reversal of that rule or a new storage dependency, and either one is an ADR.

**flux-operator's `ResourceSet` already owns the preview lifecycle.** A `ResourceSetInputProvider`
polls the forge for labelled pull requests and turns each into an input; a `ResourceSet` instantiates
its resources per input and deletes them when the input disappears — create, update and garbage
collection, teardown on merge or close included. Integration is CR-only (flux-operator is AGPL-3.0).
What is *not* provided is the manifests: something still has to say what a preview of this project
looks like.

**Progressive delivery costs an object, and both vendors charge a different one.** Flagger takes
ownership of the `HTTPRoute` — it creates and re-weights the route, so the route kelson renders stops
being kelson's. Argo Rollouts replaces the `Deployment` kind with its own `Rollout`, so the workload
kelson renders stops being a Deployment. Neither trade is reversible by configuration, and both are
decoration without a metrics provider to decide the analysis step, which kelson does not have until
M13.

**A Helm preview cannot be honest and cheap at the same time.** What kelson can diff with no I/O is
the `HelmRelease` it writes: chart, version, values. What a reviewer wants is the manifests the chart
expands into, and getting those means `helm template` against a fetched chart — not a pure function of
the spec, and therefore not something the renderer may do
([ADR-0001](0001-hybrid-state-model.md), [#20](https://github.com/dafrie/kelson/issues/20)). Every
other component type previews as concrete manifests. A chart component cannot, at v0 prices.

## Decision

### 1. Manual upload is a prebuilt image reference, and nothing else

The flow ships as an affordance over a path that already exists end to end: `spec.image` on the
Project, `image` on a component, `DeployRequest.image` on the wire, and the UI's image mode. There is
nothing to build.

**Source-tarball upload and custom Cloud Native Buildpacks builders are deferred.** They are not "the
same feature, later": a tarball path reverses the documented rule that nothing uploads a tarball
through kelson, and it needs its own ADR when it is picked up, arguing the blob-then-fetch shape and
the storage dependency on their own merits.

### 2. Promotion is a per-environment image pin, and it is a spec field

An Environment may pin a component's image for that environment only:
`Environment.spec.components[].image`. Resolution precedence is explicit and documented —
**environment override image > component image > project `spec.image`** — and the pin satisfies the
built-from-source machinery exactly the way `--image` does: a `source` + `build` component whose
environment pins an image resolves to that image, not to the unresolved sentinel.

**Promote staging → production is then three existing operations**: read staging's deployed digest,
write production's pin, deploy. Diff, history and rollback need no new concepts, because nothing new
happened — the spec changed by one line and the existing machinery reported it.

**Not a server-state release record.** The rejected alternative was a promotion object in the
cluster-backed store of [ADR-0013](0013-server-state-and-api-v0.md). A pin that lives only in server
state does not survive `kelson eject`, and an ejected repository that no longer reproduces what runs
would break the promise [ADR-0003](0003-install-model.md) and
[#59](https://github.com/dafrie/kelson/issues/59) make about leaving. The digest belongs in the
document the user owns.

### 3. Canary is deferred until M13

No Flagger integration, no Argo Rollouts plugin, no `strategy: canary` field in v0. Automated analysis
requires a metrics provider, which arrives with observability (M13), and both integration shapes
surrender something structural — the HTTPRoute or the Deployment kind — which is a decision to take
with the metrics story in hand rather than ahead of it. Revisit with M13.

### 4. Helm components delegate to helm-controller, in Flux mode only

A Helm component renders a `HelmRelease` (plus its source) and helm-controller installs and upgrades
it — the same delegation rule as every managed data type
([ADR-0005](0005-delegate-to-operators.md)): kelson writes a CR, kelson does not template an engine.
flux-operator installs the source and helm controller subset.

**The field is available in Flux mode only, and that is deliberate.** It is the first spec surface
whose availability depends on the environment's delivery mode; direct mode has no helm-controller to
delegate to, and a `HelmRelease` applied where nothing reconciles it is a manifest that does nothing.
The refusal is structured and names the mode.

**The values-only diff is a documented v0 downgrade, not a bug.** A preview of a Helm component shows
the chart, version and values that changed, and says so in those words. The upgrade path is an
advisory server-side `helm template` — server-side because it needs to fetch the chart, advisory
because it is a second opinion about what the cluster will do, and outside the renderer either way.

**Landed.** `kind: helm` renders a `HelmRepository` or an `OCIRepository` plus a
`helm.toolkit.fluxcd.io/v2` `HelmRelease`, and those two resources are the whole of kelson's inventory
for the component. Three things the implementation had to decide that this decision did not. The
version pin is **required** — an unpinned chart resolves at apply time, so the same document would
install different manifests on different days and the values-only diff would report *no change at all*,
which turns this decision's worst negative into a silent one. The two source forms are separate fields
(`source.repository` / `source.oci`) rather than one URL kelson sniffs, because they render different
Flux kinds and carry the pin in different places — the release's `version` for a repository, the
artifact's `ref.tag` for OCI. And the mode gate lives in the **pure renderer**, reading the resolved
environment's delivery mode, rather than in the delivery plane or in validation: the mode is spec data,
so the gate stays deterministic and a Project document remains valid on its own terms against every
environment it will ever meet. Secret material goes in `valuesFrom`, and nothing enforces that beyond
documenting it — kelson does not content-sniff a value to guess whether it is a credential. See
[docs/model.md](../model.md) "Helm components".

### 5. PR previews are artifact-per-PR: kelson renders, `ResourceSet` fans out

For each pull request, kelson renders **concrete manifests** for that preview and publishes them as an
OCI artifact. A flux-operator `ResourceSet` + `ResourceSetInputProvider` pair consumes the artifact
per pull request and owns only the lifecycle: create on open, update on push, destroy on merge or
close.

**The renderer stays concrete.** No template holes, no placeholders resolved by another controller,
no second rendering language — the preview manifests are the same kind of output every other kelson
render produces, and they diff the same way.

**Preview databases live inside the `ResourceSet` lifecycle**
([#103](https://github.com/dafrie/kelson/issues/103)). A preview database created outside the set
would outlive the pull request that asked for it; inside it, teardown is the same delete the rest of
the preview gets, and the leak test the epic requires has one thing to check rather than two.

## Rationale

- **Three of the four flows are already reachable through machinery kelson has**: an image reference,
  a spec field, a CR. The one that is not — canary — is the one deferred.
- **Promotion as a spec edit keeps one source of truth.** kelson's whole claim is that the manifests
  and the documents that produce them are yours; a promotion mechanism that lives outside them would
  be the first thing that stops being true after uninstall.
- **The pin is smaller than the flow it enables.** A per-environment image is one field, and it turns
  promotion, hotfix-to-one-environment and pin-production-while-staging-moves into the same operation
  with no new verbs.
- **Delegation is the established answer for third-party topology.** helm-controller is to a chart
  what CloudNativePG is to a Postgres cluster, and refusing to template charts in the renderer is the
  same refusal ADR-0005 already makes.
- **Rendering concrete manifests per pull request is cheaper than templating.** Every alternative
  makes the renderer emit something it cannot fully evaluate, which is the property the diff, the
  dry-run and the golden tests all rest on.

## Consequences

**Positive.** Promotion lands with no new dependency, no new state and no new API concept — the
narrowest possible slice, and the one the other flows build on. PR previews reuse the renderer as-is.
Helm components arrive without a chart engine in the codebase. The canary decision is taken once, with
metrics available, instead of twice.

**Negative — stated as plainly as the positives.**

- **The Helm gate is a precedent, and it is a real one.** Until now every spec field meant the same
  thing in every delivery mode; a delivery-mode-gated field means an author can write a valid document
  that becomes invalid by changing `delivery.mode`. This ADR accepts that *for chart delegation only*,
  because the alternative is either vendoring a template engine or pretending direct mode can
  reconcile a `HelmRelease`. Any future field that wants the same exemption must cite this paragraph
  deliberately — the gate is a decision, not a pattern to reach for.
- **Manual upload will disappoint the people who asked for it.** "Upload" to most users means a
  tarball or a zip, and what ships is "give kelson an image reference". The honest framing is that
  kelson does not accept artifacts it cannot trace to a source, and that the tarball path is deferred
  with a named reason — a reversal of `internal/build`'s no-upload rule needs its own ADR, and this
  one does not grant it.
- **Promotion does not gate anything.** There are no approval gates, no environment ordering, no
  "production may only receive what staging ran", and no automatic promotion on a green build. A
  promotion is a spec edit whose diff happens to be one image line, subject to exactly the same
  policy, review and delivery path as any other spec edit. Teams who want gates have them where they
  already are: pull request review in Flux mode, and `policy` when M7 lands.
- **Promotion also keeps no record of its own.** There is no promotion history, no "promoted from"
  provenance, and no query for what came from where. History records that the image changed, because
  that is what happened.
- **A pinned environment stops tracking builds, and that is the point** — but it is a foot-gun the
  first time. `--image` stands in for `spec.image` and therefore *loses* to a pin, so a CI job that
  passes a fresh digest will not move a pinned environment. Unpinning is deleting the field.
- **Values-only Helm previews are worse than what kelson promises everywhere else.** A reviewer
  approving a chart upgrade sees the values, not the manifests, and a chart can change everything
  between versions without a single value moving.
- **Canary users get nothing in v0**, including the ones who only want the manual version. Deferring
  the analysis engine defers the traffic-splitting affordance with it.

## Revisit when

- **M13 lands a metrics provider.** That is the canary decision, with the HTTPRoute-versus-Deployment
  ownership trade taken in full view of what the analysis step can actually read.
- **A server-side `helm template` preview exists.** That upgrades the values-only diff and removes the
  worst of this ADR's negatives; it does not change the delegation decision.
- **Anyone picks up source-tarball upload or custom builders.** New ADR, arguing blob-then-fetch and
  the storage dependency — not an amendment to this one.
- **Promotion needs a verb.** A `kelson promote` command, an API affordance and a UI action are
  porcelain over the pin and are deliberately out of this ADR's scope. If porcelain turns out to need
  state — an approval, an ordering, a record of who promoted what — that is a new decision, and it
  meets [Kargo](https://github.com/akuity/kargo) interop (#11) before it meets a new kelson object.

  **Answered, and it needed no state.** `kelson promote`, `DeployService.Promote` and the
  `promote_application` MCP tool landed as pure porcelain: they read the source environment's latest
  revision from the delivery history, splice the pin into the target Environment document, show the
  diff and stop. No promotion object, no approval, no provenance record — this decision's "promotion
  keeps no record of its own" survives the verb intact. Two things the porcelain had to decide that
  this ADR did not: the digest comes from the *recorded manifests* of the source's latest revision
  rather than from its spec (promoting an intention would defeat the point), and a component the
  revision does not carry is skipped with a reason rather than guessed. The write is a byte splice,
  not a re-serialization, because [ADR-0013](0013-server-state-and-api-v0.md) §1's byte fidelity is a
  promise about the user's document and a promotion may change one line of it. See
  [docs/model.md](../model.md) "Promotion".
