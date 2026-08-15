# ADR-0036: autoDeploy — an environment opts in, its components follow their sources

- **Status:** Proposed
- **Date:** 2026-08-15

> Completes [ADR-0034](0034-forge-driven-delivery.md) decision 4, whose `autoDeploy` was deliberately
> deferred until the tracking question had a subject. [ADR-0035](0035-sources.md) supplied it: a
> component is bound to exactly one source, so "which pushes move this environment" decomposes into
> "which components follow their source". Shape chosen by the owner (2026-08-15): **per environment
> and per component.** Builds on the trigger pipeline, statuses and `ReportBuild` of ADR-0034 as
> merged (#248, PRs #250/#253); refs [#239](https://github.com/dafrie/kelson/issues/239).

## Context

Every kelson deploy today is initiated by a person or a pipeline verb. The webhook handler receives
`push` events and answers `"acted": false, "reason": "autoDeploy is not implemented"`;
`ReportBuild` with a `ref` and no PR refuses with the same pointer. Both refusals were written to be
replaced by this decision.

What must not move: [ADR-0016](0016-delivery-flows-v0.md)'s promotion posture (production moves when
a person moves its pin), rule P3 (a pinned component ignores everything), and ADR-0034 decision 1's
trigger discipline (an event enqueues reconciliation of true state; it never mutates directly).

## Decision

### 1. The flag exists at two scopes, and the innermost wins

```yaml
# Environment
spec:
  autoDeploy: true              # default false — manual deploys stay the default
  components:                   # the environment's existing per-component override list
    - name: worker
      autoDeploy: false         # worker stays manual even while the environment tracks
```

A component's effective setting is: its own override if set, else the environment's, else `false`.
No error states between the levels — the same P1–P3 instinct as every other override. A component
may also set `autoDeploy: true` under an environment that leaves it unset, which makes single-component
tracking cheap to express.

### 2. A push moves only what is bound to it

On a relevant push, the stale set is: components of auto-deploying environments whose
[ADR-0035](0035-sources.md) binding resolves to the pushed repository **and** whose source `ref`
matches the pushed ref (branch-normalized; tag refs match tag sources). A SHA-pinned source never
auto-deploys — there is nothing to track. A component pinned by P3 never moves regardless of the
flag. An environment with an empty stale set does nothing, silently.

### 3. Two trigger paths in, one pipeline out

- **`ReportBuild` with `ref` and no PR** (`build.by: ci`): the currently-refused half becomes the
  mirror of the preview path — validate, resolve the stale set, render with the reported
  component-keyed pins, publish to the environments' artifacts, let Flux reconcile. Components
  reported but not stale (not bound, not tracking, pinned) are named in the response message, not
  silently deployed.
- **Webhook `push`** (`build.by: kelson`): resolve the stale set; for a single-source project run
  the build plane at the pushed head, then render+publish. A **multi-source** kelson-built project
  still refuses with `build/several-sources` ([#252](https://github.com/dafrie/kelson/issues/252))
  — the refusal lands on the environment's conditions, naming the CI path, rather than vanishing
  into a webhook 202.
- **No poll backstop in this slice, stated honestly.** An instance that cannot receive webhooks and
  does not report builds keeps manual deploys — exactly today's behavior. The poller (periodic
  `ls-remote` against tracked sources) is deliberately deferred; the docs must say "webhook or CI
  report", not imply a poll that does not exist.

### 4. Outcomes are written back, and the trigger is audited

An auto-deploy reports a commit status per [ADR-0034](0034-forge-driven-delivery.md) decision 5
(the `OutcomeReporter` machinery exists; the d5 text always said "per preview publish and per
auto-deploy"). The audit trail records the trigger per [ADR-0026](0026-agent-audit-trail.md) with
an honest principal: the reporting agent identity for `ReportBuild`, and the forge connection (as a
system principal, not a person) for a webhook push — never an invented human.

## Rationale

- **Environment default + component override** rather than a component list on the flag, because it
  is the shape every other per-component decision in the model already takes, and because the
  common cases collapse to one line: `autoDeploy: true` (track everything), or one `false` under it
  (track everything but the risky one).
- **The binding does the routing** rather than a per-component ref field, because ADR-0035 already
  answered "which repository moves this component"; a second answer would be the two-fields-that-
  disagree failure #239 warned about.
- **Refusing multi-source kelson builds** rather than building the pushed source only, because
  per-component images do not exist yet (#252) and a partial deploy that quietly reuses the other
  components' stale images from a different commit is the kind of surprise this project refuses.

## Consequences

**Positive.** The core loop — push to a branch, watch it ship — exists, opt-in, without touching
promotion, pins, previews or the manual default. CI-built and kelson-built projects get it through
the same pipeline they already use.

**Negative.**

- A busy tracked branch is a busy registry and a busy build plane (ADR-0034 already accepted this
  for previews; `autoDeploy` widens it to standing environments — registry retention gets more
  urgent, not less).
- Two flags at two scopes are more surface than one; the resolver keeps them honest, but the UI
  must show the *effective* setting per component, not make readers compute it.
- Without the poll backstop, webhook loss silently degrades tracking to manual. The delivery-state
  surface (last webhook delivery, last report) is what keeps that visible; it must say so.
- An auto-deploy that fails (render error, publish refusal) has no human watching a terminal. The
  environment's conditions and the commit status are the only messengers, which raises the bar on
  their honesty rather than lowering it.

## Revisit when

- **The source poller is wanted** — an instance that can neither receive webhooks nor report builds
  but still wants tracking. It is one interval loop over `ls-remote`, and it should reuse this
  ADR's stale-set resolution unchanged.
- **#252 lands** — multi-source kelson-built projects stop refusing and the stale set can build
  per component.
- **Promotion gating on green** (ADR-0016's deferred posture, Kargo interop #11) — `autoDeploy`
  plus statuses is most of the machinery; the policy decision remains its own.
