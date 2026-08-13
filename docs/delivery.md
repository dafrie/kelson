# Delivery plane — adapter interface and concurrency

Design reference for the delivery plane (M2). Implements issues #32, #38, #40.
The load-bearing rule — **adapters never influence rendering** — is stated in
[ADR-0001](adr/0001-hybrid-state-model.md) and enforced by the package
structure: the renderer is a pure function; `internal/delivery` only ever
receives already-rendered manifests.

## The adapter interface

Three concepts, mirrored in `internal/delivery`:

- **`Adapter`** — one delivery mode's last mile: `Apply`, `Status`, `History`,
  `Rollback`.
- **`Capabilities`** — negotiation, so callers know up front whether PRs,
  git or rollback are supported (`SupportsPR`, `RequiresGit`,
  `SupportsRollback`).
- **`Registry`** — per-environment adapter selection by delivery mode
  (`direct` / `flux`; the `argocd` adapter is removed per
  [ADR-0012](adr/0012-flux-only-gitops.md), and the seam is where it would
  return).

A further adapter requires: implement `Adapter`, register it. No renderer
changes. That is the acceptance test for #32 and it is structural, not
aspirational.

### Status: one state machine, three answers

The deployment state machine (issue #37):

```
Proposed ──► Committed(sha) ──► Reconciling ──► Applied ──► Healthy
                     │                         │
                     └──► Rejected             └──► Degraded
```

Adapters report their observation; the state machine owns transitions. The
three answers that must never be confused:

1. **not picked up yet** (`Committed`, no progress) — keep waiting, or report
   a stuck state with a cause after a timeout.
2. **rejected** (`Rejected`) — the reconciler processed the change and refused
   it; name the cause.
3. **applied but unhealthy** (`Degraded`) — it is live and wrong; surface
   health detail.

Every failure transition carries a `Cause` naming the responsible component
and the reason. The engine, the transition table, provenance correlation and
the timeout policy are specified in [the state machine](statemachine.md)
(`internal/delivery/statemachine`); adapters plug into it by implementing a
single watch-based `Source`.

### History, uniform across modes

Direct mode keeps a rendered-history store (issue #38); Git modes derive
history from the repository. Both surface through the same `History() []Entry`
API so the CLI/UI/API see one shape. `kelson eject --to-git` later replays
that history rather than exporting it.

### Promotion is not a delivery operation

Moving a known-good image from one environment to another is an *authoring* change, not a mode of
delivery: it edits the target Environment's per-component image pin and then takes the ordinary deploy
path, whichever adapter that environment uses. No adapter knows what a promotion is, and none needs to
([ADR-0016](adr/0016-delivery-flows-v0.md); the field is documented in
[the model](model.md#promotion)).

## Optimistic concurrency (#40)

Concurrent edits — agent vs human, or a hand-edit in Git mode vs a commit —
must conflict loudly, never last-write-wins.

### Spec versioning

The authoring plane carries a version per spec document. The API accepts an
expected version on writes; a mismatch returns `delivery/conflict` with both
the expected and the observed version, plus the field that diverged.

### Git mode

Read-modify-write against the latest HEAD. kelson never renders from cached
state. If HEAD moved between read and write, the write fails with a conflict
rather than being force-pushed.

### Which operations may retry automatically

- **May retry**: applying an unchanged manifest set (idempotent), a git commit
  that lost a race before touching the remote, a rollback to the current
  revision.
- **Must surface**: a spec write that conflicts, a PR merge conflict, a
  server-side apply that conflicts on a field owned by another controller.
  These are the "discard human intent invisibly" failures and always go to the
  user as a structured `delivery/conflict` error.
