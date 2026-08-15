# ADR-0034: Forge-driven delivery — webhooks, the CI hand-off, and server-side preview publishing

- **Status:** Accepted
- **Date:** 2026-08-15

> Amends [ADR-0017](0017-pr-previews.md) decision 8: the publisher gains two server-side trigger
> paths, and the CI verb's job shrinks from "render and publish" to "build and report". Everything
> else in ADR-0017 stands — the `previews:` block, the SHA tag, the naming scheme, the
> `ResourceSet` lifecycle and the artifact format the spine adopted. Depends on
> [ADR-0033](0033-git-connections.md) for the credential and the webhook endpoint, and on
> [ADR-0028](0028-delivery-spine.md) for the render→publish→ensure path everything below converges
> on. Nothing here starts before R1/R2 of the rebuild ([#224](https://github.com/dafrie/kelson/issues/224),
> [#225](https://github.com/dafrie/kelson/issues/225)) are done.

## Context

Two facts about today's design, both deliberate, both now worth revisiting on changed premises.

**Previews require editing CI.** [ADR-0017](0017-pr-previews.md) decision 8 put the publisher in the
application repository's CI — `kelson preview publish` beside the build — because that is where the
checkout, the fresh image and the registry credential already were, and because the server-side
alternative needed *"a webhook per repository, a forge credential kelson does not hold today, a
checkout of somebody else's source inside the control plane, and an answer to 'which image does this
preview run?' that CI already has and the server does not."* Every clause of that sentence has moved:
[ADR-0033](0033-git-connections.md) gives kelson one app-level webhook covering every installed
repository and the credential to act on it; kelson's own build plane
([ADR-0010](0010-build-strategy.md)) is a checkout that happens in a build pod, not in the control
plane; and the image question has two honest answers, spelled out below. Decision 8 itself
anticipated this: the `Publish` RPC was *"not rejected, deferred… a thin wrapper over the same
package"*.

**Nothing deploys on push.** Every deploy today is initiated by a human or a CLI. The category's
core loop — push to main, watch it ship — has no kelson answer at all, and the same webhook that
makes previews reactive is 90% of the machinery for it.

The product bar this ADR is held to: **install the GitHub App, get previews** — no CI edits, no
hand-made Secrets, no webhook setup. And for teams that do build in their own CI, the integration
should be *"I built the image, you take it from here"*, not "run kelson's renderer inside your
workflow".

## Decision

### 1. One trigger pipeline, three sources, one downstream path

Deploys and preview publishes are triggered by exactly three things:

| Source | Carries | Arrives via |
|---|---|---|
| **Forge webhook** | `push` / `pull_request` events for installed repositories | `<server>/forge/<provider>/webhook`, HMAC-verified ([ADR-0033](0033-git-connections.md)) |
| **CI report** | "these images exist for this commit" | `BuildService.ReportBuild` RPC / `kelson ci report-build` |
| **Poll** | the reconciliation backstop | flux-operator's `ResourceSetInputProvider` interval (previews); periodic re-resolve (tracking environments) |

All three converge on the same server-side path: resolve → render → publish → ensure — the
[ADR-0028](0028-delivery-spine.md) spine for environments, the shared publisher of
[ADR-0017](0017-pr-previews.md) decision 10 for previews. A webhook is never load-bearing: it makes
the poll instant, and its loss degrades latency, never correctness. An event mutates nothing
directly — it enqueues work that re-reads the spec, the connection and the forge, so a forged or
replayed event can at worst cause a redundant reconcile of true state.

### 2. The preview lifecycle stays flux-operator's; the webhook makes it reactive

`ResourceSet` and `ResourceSetInputProvider` remain untouched, and kelson still never grows its own
PR poller or GC (the M10 rule). On a relevant `pull_request` event, kelson annotates the
environment's `ResourceSetInputProvider` with `reconcile.fluxcd.io/requestedAt` — exactly what a
Flux `Receiver` would do, without requiring notification-controller to be exposed. Polling stays as
configured (`previews.interval`) and is the only path on instances that cannot receive deliveries.

### 3. Who builds the preview image is a per-project choice, and both answers work

`Project.spec.build` gains one field, honoring both postures:

```yaml
build:
  strategy: auto          # unchanged (ADR-0010)
  by: kelson              # kelson | ci — who produces images for this project
```

**`by: kelson` (default for projects with `source:`).** On PR open / synchronize (or poll), kelson
runs its own build plane at the head SHA — the same drivers, the same in-pod clone, now with the
connection's minted credential ([ADR-0033](0033-git-connections.md) decision 5) — then renders the
preview with the fresh image, publishes the artifact tagged with the SHA, and reports status
(decision 5 below). Zero CI configuration. This is the Vercel loop, and the control plane stays out
of the *data* path exactly as [ADR-0010](0010-build-strategy.md) drew it: the server triggers and
observes; bytes flow between the build pod, the registry and Flux.

**`by: ci`.** CI's contract shrinks to build-and-report:

```sh
kelson ci report-build --project checkout --sha "$GITHUB_SHA" \
  --image web=ghcr.io/acme/checkout-web@sha256:… [--pr 412]
```

`ReportBuild` records the images for that commit and triggers the same server-side render→publish —
for the PR's preview when `--pr` is given, for tracking environments (decision 4) on a branch head.
CI never runs kelson's renderer, never needs the spec checkout, never holds the artifact-registry
credential. `kelson preview publish` survives unchanged for fully self-contained CI (air-gapped
runners, no reachable kelson), demoted from recommended path to escape hatch; ADR-0017's workflow
example moves to that framing.

**The skip-label CI gate becomes mostly unnecessary** on both paths — kelson publishes only after
the image exists — but `skip.labels` stays: it is still the author's pause switch, and flux-operator
still honors it.

### 4. Environments may track their source, opt-in

```yaml
# Environment.spec
autoDeploy: true          # default false — deploys happen when a human says so
```

An environment with `autoDeploy: true` re-renders and republishes on a `push` to the project's
source ref (webhook or poll, per decision 1) and on a matching `ReportBuild`. The default stays
manual: [ADR-0016](0016-delivery-flows-v0.md)'s promotion posture — production moves when its pin
moves — is unchanged, and a pinned component ignores tracking by the existing precedence rules.
Spelling of the field is a schema decision to finalize at implementation; the semantics — per
environment, opt-in, off by default — are this ADR's. If the per-environment source-ref override
([#239](https://github.com/dafrie/kelson/issues/239)) is adopted, tracking follows the
environment's *effective* ref — the two designs must land as one answer to "which pushes move this
environment", not two fields that disagree.

### 5. Outcomes are written back to the forge

Through the connection's `StatusReporter` capability, when the connection has it:

- a commit **status/check** per preview publish and per auto-deploy, linking to the environment or
  preview detail page, terminal states matching the delivery state machine's phases;
- **one** PR comment per preview, upserted in place — hosts, phase, last revision — never a comment
  per push.

Absence of the capability (a `generic` connection) degrades to nothing, silently: statuses are a
courtesy of the integration, not a delivery dependency.

### 6. CI is a principal

`ReportBuild` authenticates with an agent identity ([ADR-0024](0024-agent-identities.md)) — a
scoped, expiring credential minted for the pipeline, auditable per
[ADR-0026](0026-agent-audit-trail.md). CI was always an agent in every sense that matters; this
makes it one in the sense kelson enforces. No new auth mechanism is invented.

## Rationale

- **One pipeline rather than per-source flows**, because the failure mode of event-driven systems is
  an event that means something different from the poll that follows it. Every source degenerates to
  "reconcile this (project, ref/PR) now"; the truth is always re-read.
- **`by:` on the Project, not the Environment**, because who builds an image is a property of the
  artifact's provenance, not of where it runs — the same reason `source:` and `build:` live on the
  Project. Previews and environments of one project share one answer.
- **Report-then-render rather than render-in-CI**, because the renderer's inputs (stored spec,
  ClusterProfile, connection) live server-side, and shipping them to CI was the tail wagging the dog:
  ADR-0017 put rendering in CI to be near the image, and a report RPC moves the image reference
  instead — one string, not three inputs.
- **Registry-convention detection (an `ImageRepository`-style poller) was considered and rejected**
  for the report path: inferring "CI finished" from a tag appearing is racy (multi-arch manifests
  land in pieces), convention-coupled, and answers "an image exists" when the question is "the build
  for this commit is complete". An explicit report is one CLI line and carries exactly the answer.
- **The two-decision split** (0033 credentials, 0034 flows) mirrors 0027/0028: one ADR for where a
  capability lives, one for the loop that uses it, so each can be revisited alone.

## Consequences

**Positive.**

- Install the app → previews, statuses, PR comments, with zero CI edits, for kelson-built projects.
- CI-built projects integrate with one line that states exactly what CI knows and nothing it
  doesn't.
- Push-to-deploy exists, opt-in, riding the same pipeline — no second mechanism to test.
- ADR-0017's *"a repository that forgets the step gets an `OCIRepository` reporting a missing
  artifact"* failure mode disappears for both paths: kelson publishes when the image is ready,
  because kelson is the one who knows.

**Negative — stated as plainly as the positives.**

- **The control plane is now in the notification path for every PR push** — the shape decision 8
  avoided. Bounded: the server renders and publishes (work it already does for every deploy), builds
  run in pods, and a down server means previews lag to the poll interval rather than fail. But it is
  real load with PR-count scaling, and it needs a queue with backpressure, not a goroutine per
  webhook.
- **Two build postures are a permanent test matrix.** `by: kelson` × `by: ci`, each × webhook/poll,
  for previews and for tracking environments. The pipeline convergence in decision 1 is what keeps
  the matrix's downstream half at one path; the upstream half is irreducible.
- **`autoDeploy` re-renders on push even when nothing but source changed**, which is correct (the
  image is new) but makes a busy branch a busy registry. Immutable tags mean unchanged renders
  deduplicate to existing digests, but image layers do not; registry retention becomes operationally
  relevant sooner ([ADR-0028](0028-delivery-spine.md) "Revisit when" already names this).
- **Status write-back widens the app's permission set** (`statuses:write`, `checks:write`,
  `pull_requests:write` for the comment). Users who install with less get silently fewer features;
  the connection's capability display must say which, per ADR-0033's honesty rule.
- **A reported image is trusted on the reporter's word.** `ReportBuild` carries a digest and kelson
  publishes manifests referencing it without verifying provenance — the same trust boundary
  ADR-0017 recorded for artifacts ("the trust boundary is the registry's write access"), now with a
  second writer. Signing/verification remains one decision for images and artifacts together, still
  unowned.
- **Webhook delivery is unobservable-by-default infrastructure.** Debugging "why didn't my preview
  update" now spans forge delivery logs, kelson's queue and flux-operator's poll. The delivery-state
  surface promised in ADR-0033 (last delivery, last poke, next poll) is not optional polish; it is
  the difference between a support question and a bug report.

## Revisit when

- **R2 lands and the first slice ships.** The order of slices (connection + private builds →
  app flow + pickers → webhook + reactive previews → server-side publish + report → autoDeploy →
  status write-back) is implementation sequencing, owned by the tracker, not this record.
- **The GitLab adapter arrives.** MR events, group tokens and no manifest flow will stress
  decisions 1 and 5; the pipeline should absorb it as a new `WebhookSource`/`StatusReporter`, and if
  it cannot, that is this ADR failing early and worth knowing.
- **Promotion wants gating on green.** `autoDeploy` plus statuses is two-thirds of "promote when
  staging is healthy"; the remaining third is a policy decision [ADR-0016](0016-delivery-flows-v0.md)
  deliberately declined and Kargo interop (#11) has first claim on.
- **Load makes the queue a product surface.** If webhook volume ever needs per-connection rate
  limiting or priority, that is operational maturity this ADR only leaves room for.
