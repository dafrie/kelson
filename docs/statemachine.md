# Deployment state machine — "is my change live?"

Design reference for `internal/delivery/statemachine` (issue #37). It is the
hardest problem in the delivery plane for one reason: **kelson does not apply.**
kelson publishes an artifact and points a `Kustomization` at it; Flux applies
([ADR-0028](adr/0028-delivery-spine.md)). Something still has to give the user a
truthful answer about their change, without owning the apply step.

The answer is not a spinner. It is a state machine fed by correlated
observations, with exactly one timer.

Since the rebuild there is exactly one thing feeding it: the controller's step 6,
watching the Flux objects it owns and the workloads they produced, writing the
result into `Environment.status`. The engine itself is unchanged — same phases,
same transition table, same timeout, same correlation rules.

> **Transition ([#224](https://github.com/dafrie/kelson/issues/224)).** The
> direct-mode source went with the applier. The engine, its phases and its
> transition table are intact and unused until the controller feeds them.

## The model

```
Proposed ──► Committed ──► Reconciling ──► Applied ──► Healthy
    │            │              │              │          │
    └──► Rejected◄┘             ├──► Rejected  └──► Degraded ◄┘
                                └──► Degraded ──► Healthy / Applied / Reconciling
```

| Phase | Meaning |
|---|---|
| `Proposed` | kelson rendered the manifests; nothing has been published yet. |
| `Committed` | The revision is durable: the artifact is pushed and the `OCIRepository` is pinned to its tag. Nothing has acted on it yet. |
| `Reconciling` | A reconciler has picked up **this** revision and is working. |
| `Applied` | The resources are in the API server at this revision. |
| `Healthy` | Applied **and** the workloads are healthy. Terminal. |
| `Rejected` | A reconciler processed this revision and refused it. Terminal. |
| `Degraded` | Applied but unhealthy. **Not** terminal — it can recover. |

An engine tracks **one revision**. A new deployment gets a new engine, starting
at `Proposed`.

### Transition table

| From | Allowed to |
|---|---|
| `Proposed` | `Committed`, `Rejected` |
| `Committed` | `Reconciling`, `Applied`, `Healthy`, `Degraded`, `Rejected` |
| `Reconciling` | `Applied`, `Healthy`, `Degraded`, `Rejected` |
| `Applied` | `Healthy`, `Degraded` |
| `Healthy` | `Degraded` |
| `Degraded` | `Healthy`, `Applied`, `Reconciling` |
| `Rejected` | — (terminal) |

Self transitions are always legal: repeated observations of the same phase are
normal. They are just not *progress* — which matters for the timeout below.

Three rules generate the table:

- **Forward only, skips allowed.** One observation may be the first to arrive
  after several things happened: a Flux `Kustomization` with health checks
  reports `Ready=True` in a single step, and a polling source can miss phases
  entirely. Backwards moves are not legal — a revision cannot become
  uncommitted.
- **Nothing precedes the commit.** A revision whose artifact was never published
  cannot be reconciling or live. A source claiming otherwise has lost track of
  which revision it is reporting on.
- **Rejection is pre-apply.** Once applied, "the reconciler refused it" is a
  contradiction. That situation is `Degraded`.

An observation implying an illegal transition is a **programming error in the
source**, not a state to clamp into something plausible. `Validate` returns an
`*InvalidTransitionError`, `Must` panics, and the engine aborts `Run` with the
error rather than silently ignoring the observation.

## The three answers

These must never be confused, because each sends the user somewhere different.
`State.Answer()` keeps them apart:

| Answer | State | What it means | What the user does |
|---|---|---|---|
| `waiting` | `Committed`, not stuck | Not picked up **yet**. | Wait. |
| `progressing` | `Reconciling` / `Applied` | Something is working on it. | Wait. |
| `live` | `Healthy` | Done. | Nothing. |
| **`stuck`** | timeout expired, no failure phase | Nobody ever picked it up. | **Look at Flux** — a suspended `Kustomization`, a source-controller that cannot pull the artifact, or no Flux running at all. |
| **`rejected`** | `Rejected` | Processed and refused. | **Fix the change and redeploy.** It is not live. |
| **`degraded`** | `Degraded` | Applied and unhealthy. | **Look at the workload.** It *is* live. |

The failure answers are checked before `Stuck`, so a degraded deployment that
also times out still reads as `degraded` — "unhealthy" is more actionable than
"waited too long".

Every failure carries a `Cause{Component, Reason, Message}` naming the
responsible component and why, rendering as
`flux: NotReady: Kustomization checkout-production is not ready`. If a source
reports a failure phase with no cause, the engine synthesises one: an
unexplained failure is a bug report nobody can act on.

`State.Err()` projects a settled failure onto the structured delivery error
taxonomy: stuck-before-pickup becomes `delivery/not-watched` (issue #34), the
rest `delivery/apply-failed`. That first code was written for the shape where a
user's own `Kustomization` watched a path kelson wrote into, and nothing watched
it. kelson now owns both objects ([ADR-0028](adr/0028-delivery-spine.md)
decision 3), so the misconfiguration it named is unreachable; what remains under
it is *published, and no reconciler acted* — the remediation points at Flux
rather than at a path.

## Correlation

Provenance is what makes an answer trustworthy without owning apply. Every
rendered resource carries `kelson.dev/revision` and `kelson.dev/spec-hash`
(see [architecture](architecture.md#provenance)), and the engine correlates
observations against the `Target` derived from the delivered `ManifestSet`:

1. an observation naming a revision matches only if it equals the target
   revision;
2. with no revision, `kelson.dev/spec-hash` is the fallback;
3. an observation with no provenance at all is **dropped** — attributing it to
   this revision would be a guess.

This is load-bearing, not bookkeeping. A reconciler that is perfectly healthy on
the *previous* revision reports `Healthy` continuously. Without correlation that
reads as success; with it, it reads as "still on `1111111`, has not picked up
`9f1c2ab`" — and the phase stays `Committed` until the timeout turns it into a
stuck verdict that names the old revision. Uncorrelated observations are not
thrown away either: they are counted in `State.Stale` and their revision kept in
`State.ObservedRevision`, because they are the evidence in the eventual cause.

## Timeout and stuck detection

One configurable duration, `Config.Timeout` (default 5 minutes), and it is the
**only** timer in the engine.

- It is a **progress** timeout — the budget for *one* phase change, not for the
  whole rollout. It resets on every phase change.
- Repeated observations of the same phase do **not** reset it. A chatty
  reconciler saying "still Committed" forever is precisely the wedge that has to
  be detected, so chatter must not defer the verdict.
- On expiry the state is marked `Stuck` (a flag, not a phase — the phase it
  wedged in *is* the diagnosis) and gets a cause derived from that phase:

| Wedged in | Reason | Cause says |
|---|---|---|
| `Proposed` | `NotCommitted` | the artifact was never published |
| `Committed` | `NotPickedUp` | the reconciler has not picked it up, and what it is still reporting instead |
| `Reconciling` | `StalledReconciling` | the reconciler has been working on it this long with no progress |
| `Applied` | `HealthUnknown` | applied but never reported healthy |
| `Degraded` | *(the source's own)* | the existing health cause is kept — the timeout adds nothing |

Progress after a stuck verdict clears the flag: something moved after all.

`Run` returns when the machine settles (`Healthy` / `Rejected`), when the
timeout fires, or on context cancellation. Rejected, degraded and stuck are
**answers**, returned in the `State` with a `nil` error; a non-nil error from
`Run` means the machinery itself failed (a broken watch, a stream that ended
before a verdict, an illegal transition) — never a false "healthy".

## How the controller feeds it

A source implements one interface:

```go
type Source interface {
    Watch(ctx context.Context, out chan<- delivery.Status) error
}
```

`Watch` blocks and pushes one `delivery.Status` per change — **watch-driven, not
polling**. The engine owns the only timer; if a source polled internally and the
engine polled too, poll latency would be indistinguishable from lack of
progress. Under the spine the source watches two things — the `OCIRepository`
and `Kustomization` kelson owns, and the workloads their apply produced — and
translates their conditions into phases. Requirements:

- stamp each `Status` with the revision it observed (`Status.Revision`, or
  `kelson.dev/revision` in `Detail`) so it can be correlated. **Do not filter
  out observations of other revisions** — those are the evidence for a stuck
  verdict;
- carry a `Cause` on every failure phase;
- put health/diagnostic detail in `Status.Detail` (a `reason` key becomes
  `Cause.Reason`); it is passed through to the caller verbatim;
- use `statemachine.Send` to push, and return `ctx.Err()` on cancellation.

Helpers for common shapes: `SourceFunc`, `Chan(<-chan delivery.Status)` for a
source that already multiplexes its watches, and — where no event stream exists —
`Poll(Observer, interval)`, which confines polling to the source where it
belongs.

Driving it:

```go
engine, err := statemachine.New(statemachine.Config{
    Target:    statemachine.TargetFromSet(set),
    Source:    fluxSource,
    Component: "flux",
    Timeout:   5 * time.Minute,
    OnState:   func(s statemachine.State) { ui.Update(s) },
})
final, err := engine.Run(ctx)
```

`Engine.State()` is a snapshot, safe from any goroutine while `Run` is going —
that is how a UI or `kelson status` reads progress. `State.Status()` projects
back onto `delivery.Status`, which is what the controller writes into
`Environment.status` — so every reader gets the engine's verdict rather than a
second, divergent opinion computed at read time.
