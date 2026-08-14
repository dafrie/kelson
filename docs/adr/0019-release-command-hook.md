# ADR-0019: The release command hook — a component field, direct mode only, no new phase

- **Status:** Accepted
- **Date:** 2026-08-14

## Context

[ADR-0007](0007-data-services.md) closed with a consequence it could not act on: *"Preview databases
need migrations to run before the application starts, which pulls the release-command hook earlier than
planned."* [#104](https://github.com/dafrie/kelson/issues/104) is that hook — run a command against the
database after provisioning and before the application starts — and it asks for four things: an
ordering guarantee (database, then release command, then rollout), a failure that blocks the rollout
rather than producing a half-deployed state, timeout semantics, and a plain statement of what rollback
does to a migration.

Three questions had to be answered before any of that could be written, and each of them is a place
where the obvious answer is wrong.

**Where does the field live?** The issue says "`release:` command on an Application", written in
[ADR-0006](0006-project-application-environment.md)'s vocabulary, where *Application* is the leaf that
[ADR-0014](0014-components.md) renamed to *Component*. A Project-level field reads well in prose — one
app, one migration — and falls apart on contact: the command needs an image, an environment, service
bindings and an identity, all of which belong to a component, and a Project-level hook would have to
pick one component's and pretend it had not.

**Can every delivery mode do this?** kelson's rule is that a spec field renders something real in every
delivery mode or refuses per mode honestly. ADR-0016 decision 4 and ADR-0017 both gate a field to Flux.
This one is the first that cannot be delivered *by* Flux: the guarantee is not "the Job exists" but "the
Job finished before the Deployments changed", and only the mode where kelson performs the apply itself
can stop between two resources.

**Is a migration a new deployment phase?** The seven phases of `delivery.Phase` are shared vocabulary —
CLI, API, UI and state machine — and it is tempting to add `Migrating` beside them so a deploy log can
say the word.

## Decision

**1. `release:` is a field on a workload component, not on the Project.**

```yaml
components:
  - name: web
    port: 8080
    release:
      command: ["./manage.py", "migrate"]   # required
      timeout: 30m                          # optional, default 10m
```

It is refused on `kind: cron` (a cron component already is a command on a schedule), and on data
components and charts for the reason every workload field is. A project that wants one migration for
several components puts the hook on the one whose rollout must wait for it.

The struct is a struct rather than a bare command list because the second question a migration raises —
how long may it take — has no other place to live. Retries are deliberately *not* a field: see decision
5.

**2. It renders a `ServiceAccount` + `Job` pair, placed after the data services and charts and before
every workload.** The Job carries the component's image, its whole resolved environment (bindings and
secret references included), `restartPolicy: Never`, `backoffLimit: 2` and the `activeDeadlineSeconds`
the timeout resolves to. The component's ServiceAccount *moves* here rather than being duplicated: the
Job's pod names it, and a pod naming a ServiceAccount that does not exist yet is refused rather than
queued, which in a mode that waits for the Job is a deadlock.

**3. The Job's name is `release-<component>-<first 8 hex of the spec hash>`, and the name is the
idempotency key.** Re-applying an unchanged revision addresses the Job that already ran — completed
means the migration is not re-run and the deploy proceeds; failed means it is deleted and re-run. A
changed revision hashes differently, so a deploy runs its migrations. This mirrors how the build plane
names build Jobs after their request.

**4. The field renders in `direct` mode only. Every other mode is a render error,
`render/release-requires-direct`.** In direct mode the adapter applies up to the Job, waits for it, and
only then applies the workloads. In Flux mode kelson commits files that somebody else's `Kustomization`
applies in one pass, with no barrier kelson can express, so the migration would run beside the rollout
rather than before it and a failure would not stop anything.

**5. A migration is expressed in the existing phases, with the Job named in `Cause` and `Detail`.**
Running is `Reconciling`; failed is `Rejected`. No eighth phase.

**6. A rollback does not re-run the release command,** and a failed release command fails the deploy
before any workload of the new revision is applied — no history entry, no prune, previous revision still
serving.

## Rationale

**The component is where the inputs already are.** Every alternative reintroduces the same problem in a
different place: a Project-level hook needs an image (whose?), a separate `kind: release` component
needs its own image, env and bindings duplicated from the component it shadows, and an
`overlays:`-supplied Job needs the author to hand-write everything resolution already produced. Heroku's
release phase is per-app because a Heroku app is one image; kelson's per-image unit is the component.

**Refusing in Flux mode is the honest half of the same rule that gates charts to Flux.** ADR-0016
accepted a delivery-mode-gated field for chart delegation and said explicitly it was not a pattern to
reach for. This is the second and third time it has been reached for (previews, then this), and the
justification each time is the same and is checkable: does the mode have a mechanism for what the field
promises? Flux has helm-controller and flux-operator; it does not have a way for kelson to interpose a
barrier inside somebody else's reconciliation. A `dependsOn` between two Kustomizations would express
it, but kelson does not own the Kustomization — the environment's `delivery.git` points at a path a
user's Kustomization already reconciles — so building that would be a different feature (kelson owning
Flux objects), not this one.

**A phase is a vocabulary change; a cause is a detail.** Adding `Migrating` would oblige every consumer
of `delivery.Phase` — the CLI's printer, the API's enum, the UI's badges, the transition table, and any
adapter that must never emit it — to learn a word that exactly one delivery mode can ever report.
`Reconciling` already means "something is actively working on this revision", and `Rejected` already
means "processed, refused, not live", which is precisely the state a failed migration leaves: the
change never went live and the previous revision is still serving. `State.Err()` already writes the
right sentence for it. The Job's name travels in `Detail["releaseJob"]`, so a UI can link to it without
parsing prose — which is what the phase would have been used for anyway.

**`backoffLimit: 2` is a race absorber, not a retry policy.** Zero would fail a first deploy because a
data service applied seconds earlier is not accepting connections yet; a large number would turn a
genuinely broken migration into a long wait. Two attempts (10s, then 20s of backoff) absorb the
start-up window and fail fast otherwise. The retry that matters for a broken migration is a re-deploy,
which is a human decision and needs no field.

**Rollback leaves the schema forward.** kelson has no down-migration to run, and a rendered set contains
no record of what a migration did. Re-running the old revision's release command would repeat work the
database has already done; pretending a rollback undoes a schema change would be worse than either. The
honest behaviour is to skip it and say so in the documentation, which is what #55 asked for.

## Consequences

**Positive.**

- The ordering guarantee of #104 is real in direct mode and verifiable: the adapter's event log shows
  the Job read to completion between the last pre-workload apply and the first workload apply.
- A failed migration cannot produce a half-deployed state. Nothing of the new revision is applied,
  nothing is recorded, and the failure quotes the command's own output.
- Nothing new was added to the phase vocabulary, so every existing consumer of the state machine
  reports a migration correctly without a change.
- The Job's name makes re-deploys safe without kelson tracking migration state anywhere.

**Negative.**

- **Previews cannot use it.** Previews are Flux-only (ADR-0017) and this is direct-only, so the second
  acceptance criterion of #104 — preview environments running migrations automatically — is *not* met
  by this change. The refusal is loud rather than silent, and closing that gap means either kelson
  owning the preview's Kustomization or an in-artifact ordering mechanism; both are separate work.
- **kelson does not wait for a data service to be ready before starting the release command.** The
  `backoffLimit` absorbs a short window; a database that takes longer than roughly thirty seconds to
  accept connections on a first deploy will fail the migration and the deploy with it.
- **A third delivery-mode-gated field.** The model now has fields whose renderability depends on the
  Environment in two directions. Each is justified, and each is one more thing an author has to know
  before writing a spec that renders everywhere.
- **`Apply` can block for the length of a migration.** The adapter reports progress through a callback
  and the wait is bounded by the caller's context, but a delivery adapter that sometimes takes half an
  hour is a shape callers have to plan for.
- **Re-running is bounded, but idempotency is still the author's promise.** kelson can say when a
  command is re-run; it cannot say what happens when it is.

## Revisit when

Previews need migrations badly enough to justify kelson owning the Flux objects that could express
`dependsOn` + health checks — at which point the direct-only gate becomes a mode-specific
*implementation* rather than a refusal, and this ADR is superseded rather than amended.
