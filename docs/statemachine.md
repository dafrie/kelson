# Deployment state machine — "is my change live?"

Design reference for `internal/delivery/statemachine` (issue #37). It is the
hardest problem in the delivery plane for one reason: **in Git mode kelson does
not apply.** kelson commits; Flux applies. Something still has to give
the user a truthful answer about their change, without owning the apply step.

The answer is not a spinner. It is a state machine fed by correlated
observations, with exactly one timer.

## The model

```
Proposed ──► Committed ──► Reconciling ──► Applied ──► Healthy
    │            │              │              │          │
    └──► Rejected◄┘             ├──► Rejected  └──► Degraded ◄┘
                                └──► Degraded ──► Healthy / Applied / Reconciling
```

| Phase | Meaning |
|---|---|
| `Proposed` | kelson rendered the manifests; nothing has been written yet. |
| `Committed` | The revision is durable (a git sha, or a store entry in direct mode). Nothing has acted on it yet. |
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
- **Nothing precedes the commit.** A revision that was never committed cannot be
  reconciling or live. An adapter claiming otherwise has lost track of which
  revision it is reporting on.
- **Rejection is pre-apply.** Once applied, "the reconciler refused it" is a
  contradiction. That situation is `Degraded`.

An observation implying an illegal transition is a **programming error in the
adapter**, not a state to clamp into something plausible. `Validate` returns an
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
| **`stuck`** | timeout expired, no failure phase | Nobody ever picked it up. | **Fix the wiring** — usually a path nothing watches. |
| **`rejected`** | `Rejected` | Processed and refused. | **Fix the change and redeploy.** It is not live. |
| **`degraded`** | `Degraded` | Applied and unhealthy. | **Look at the workload.** It *is* live. |

The failure answers are checked before `Stuck`, so a degraded deployment that
also times out still reads as `degraded` — "unhealthy" is more actionable than
"waited too long".

Every failure carries a `Cause{Component, Reason, Message}` naming the
responsible component and why, rendering as
`flux: NotReady: Kustomization ./apps/checkout is not ready`. If an adapter
reports a failure phase with no cause, the engine synthesises one: an
unexplained failure is a bug report nobody can act on.

`State.Err()` projects a settled failure onto the structured delivery error
taxonomy: stuck-before-pickup becomes `delivery/not-watched` (issue #34, with
the remediation naming the watched path), the rest `delivery/apply-failed`.

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
| `Proposed` | `NotCommitted` | the revision was never committed |
| `Committed` | `NotPickedUp` | the reconciler has not picked it up, what it is still reporting instead, and to check the path it watches |
| `Reconciling` | `StalledReconciling` | the reconciler has been working on it this long with no progress |
| `Applied` | `HealthUnknown` | applied but never reported healthy |
| `Degraded` | *(adapter's own)* | the existing health cause is kept — the timeout adds nothing |

Progress after a stuck verdict clears the flag: something moved after all.

`Run` returns when the machine settles (`Healthy` / `Rejected`), when the
timeout fires, or on context cancellation. Rejected, degraded and stuck are
**answers**, returned in the `State` with a `nil` error; a non-nil error from
`Run` means the machinery itself failed (a broken watch, a stream that ended
before a verdict, an illegal transition) — never a false "healthy".

## How adapters feed it

An adapter implements one interface:

```go
type Source interface {
    Watch(ctx context.Context, out chan<- delivery.Status) error
}
```

`Watch` blocks and pushes one `delivery.Status` per change — **watch-driven, not
polling**. The engine owns the only timer; if an adapter polled internally and
the engine polled too, poll latency would be indistinguishable from lack of
progress. Requirements:

- stamp each `Status` with the revision it observed (`Status.Revision`, or
  `kelson.dev/revision` in `Detail`) so it can be correlated. **Do not filter
  out observations of other revisions** — those are the evidence for a stuck
  verdict;
- carry a `Cause` on every failure phase;
- put health/diagnostic detail in `Status.Detail` (a `reason` key becomes
  `Cause.Reason`); it is passed through to the caller verbatim;
- use `statemachine.Send` to push, and return `ctx.Err()` on cancellation.

Helpers for common shapes: `SourceFunc`, `Chan(<-chan delivery.Status)` for
adapters that already multiplex their watches, and — for backends with no event
stream at all — `Poll(Observer, interval)`, which confines polling to the source
where it belongs.

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
back onto `delivery.Status`, so `Adapter.Status` returns the engine's verdict
rather than a second, divergent opinion.
