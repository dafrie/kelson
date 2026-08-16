# ADR-0037: Release hooks on the spine — two Kustomizations and a dependsOn (supersedes 0019 d3/d4)

- **Status:** Proposed
- **Date:** 2026-08-16

## Context

[ADR-0019](0019-release-command-hook.md) accepted the release command hook's semantics — a field on a
workload component, a `Job` named by the spec hash, the existing phase vocabulary, a rollback that does
not re-run migrations — and gated its mechanism to direct mode, because kelson did not own the Flux
objects that could express a barrier. [ADR-0028](0028-delivery-spine.md) then deleted direct mode and
moved the field to the not-implemented gate table. Both ADRs' "Revisit when" clauses said the same
thing: once kelson owns the `Kustomization`s, the Flux-native replacement — two `Kustomization`s with
`dependsOn` — supersedes 0019 rather than amending it.

kelson has owned those objects since the spine landed. [#227](https://github.com/dafrie/kelson/issues/227)
is the replacement, and this ADR records where its mechanism deliberately differs from what 0019 wrote
for a plane that no longer exists.

## Decision

**1. `components[].release` is un-gated. The field's shape is 0019 decision 1, unchanged.**

**2. The renderer partitions; the controller builds the topology.** Rendered manifests carry a stage:
everything emitted before the release Job (namespace, secrets, data services, charts, the moved
ServiceAccount) is a *prerequisite* and lives in both stages; the Jobs live in the release stage only.
The publisher lays a staged artifact out in two directories, with a generated root `kustomization.yaml`
that keeps the workload path at `./` — so a rollback to a pre-split artifact still resolves. The
renderer stays pure ([ADR-0029](0029-renderer-stays-go.md)): it emits manifests and a partition,
nothing else.

**3. An environment whose components carry hooks gets two `Kustomization`s.** The release one applies
`./release` with `wait: true`, `prune: false`, and `spec.timeout` resolved from the hook's timeout; the
workload one `dependsOn` it. The enforcement path is Flux's own: `dependsOn` refuses to apply the
workload `Kustomization` until the release one is `Ready`, and `wait` makes `Ready` a kstatus health
verdict, which for a `Job` means *completed*. A failed migration therefore means the new revision is
never applied and the previous one keeps serving, with no kelson code in the path that guarantees it.

**4. What 0019 keeps:** the Job's name as the idempotency key (d3's naming), the phase mapping — running
is `Reconciling`, failed is `Rejected`, no new vocabulary (d5) — and rollback semantics (d6): a pinned
rollback drops the release `Kustomization` entirely, so a rollback cannot re-run a migration.

**5. What 0019 loses — the superseded mechanism.** Decision 3 said a failed Job "is deleted and re-run"
on re-deploy. On the spine nothing deletes it: re-applying an unchanged revision addresses the same Job,
already past its `backoffLimit`, and the environment stays `Rejected` on the old revision. The retry is
a **new revision** — a new spec hash names a new Job — which is what 0019's own rationale called the
retry that matters ("a re-deploy, which is a human decision"). The manual escape is deleting the Job;
the next reconcile re-creates it.

**6. The release `Kustomization` never prunes.** The Job name is the idempotency key, and pruning would
delete a completed Job the moment a newer revision replaces it — re-applying an older revision would
then re-run a migration the database already absorbed.

**7. Previews inherit the split.** The preview artifact is the same renderer through the same publisher,
and the `ResourceSet` template instantiates the same two `Kustomization`s per change request. The second
acceptance criterion of #104 — preview environments running migrations — is met, which 0019 recorded as
its first negative consequence.

## Rationale

The barrier 0019 said Flux could not express is now kelson's to write, because
[ADR-0028](0028-delivery-spine.md) made kelson the author of the `Kustomization`s — exactly the
condition 0019 named. Putting Flux in the enforcement path instead of re-implementing a wait means the
ordering guarantee holds even when kelson-server is down; kelson adds only what Flux does not know: the
stage partition, the timeout (Flux's health wait otherwise defaults to `spec.interval`, five minutes —
*shorter* than the hook's ten-minute default, so a healthy fifteen-minute migration would be reported
failed repeatedly), and the status sentence, because Flux's own answer is `DependencyNotReady` naming
another Kustomization rather than the migration that actually failed.

## Consequences

**Positive.**

- The ordering guarantee of #104 is real on the only delivery plane, and previews get it for free.
- A failed migration cannot produce a half-deployed state, and the guarantee survives kelson-server
  being down, because Flux enforces it.
- No new phase, no new reason constant, no new field shape.

**Negative.**

- **A failed hook wedges the environment.** It stays `Rejected` on the previous revision until a new
  revision is deployed or someone deletes the Job by hand. There is no automatic delete-and-re-run, and
  0019 promised one; this ADR is where that promise is withdrawn.
- **Completed release Jobs accumulate** — one per component per revision that changed it — until the
  namespace is deleted. `prune: false` is load-bearing and cannot be softened without breaking rollback
  idempotency.
- **The Job's name travels in `Cause` prose only.** 0019 d5 named `Detail["releaseJob"]` as the
  machine-readable slot; the current `Outcome` has no such map. A UI that wants to link the Job needs a
  small API follow-up.
- **The e2e harness does not exercise hooks.** The envtest suite judges the topology against Flux's real
  CRD schemas, but no kind-cluster test runs a migration end to end yet.

## Revisit when

An author needs a failed migration retried without cutting a new revision — that is a controller-side
delete-and-recreate policy and a new ADR, not a flag. Or the `Detail` map returns to the status surface,
at which point the Job name moves out of prose.
