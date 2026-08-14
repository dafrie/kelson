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

### The release barrier in direct mode

A rendered set is *ordered* — namespaces first, then the data services and charts, then the release
Job of any component that declares one, then the workloads — and order is all a set of manifests can
express ([#89](https://github.com/dafrie/kelson/issues/89)). Applying a Job before a Deployment does
not mean the Job finished first.

The direct adapter therefore **stops** at the release Job
([#104](https://github.com/dafrie/kelson/issues/104), [ADR-0019](adr/0019-release-command-hook.md)):
it applies everything up to and including the Job, polls the Job to a terminal state, and only then
applies the rest of the set. It is the one point in the apply loop where the adapter waits, and it is
bounded twice over — by the Job's own `activeDeadlineSeconds`, and by the caller's context (the
`--timeout` of `kelson deploy`).

Three consequences, all of them the point:

- **A failed migration fails the deploy before anything rolls.** No workload of the new revision is
  applied, no history entry is recorded, nothing is pruned — so the previous revision keeps serving.
  The error is `delivery/release-failed`, naming the Job and carrying the tail of its pod's output.
- **The wait is visible.** The adapter reports the Job through `Options.Progress` in the same
  `delivery.Status` shape the state machine consumes: `Reconciling` while it runs, `Rejected` when it
  fails, with the Job in `Cause` and in `Detail["releaseJob"]`. No new phase — see the ADR.
- **A rollback does not re-run it.** Rolling the workload back does not roll a migration back, so
  re-running the old revision's release command would only repeat work the database has already done.

**Flux mode refuses the field rather than pretending.** kelson commits files and a `Kustomization`
kelson does not own applies them in one pass; there is no commit that says "and stop here until this
Job is Complete". Rendering it anyway would produce migrations that run beside the rollout instead of
before it, so `release:` in a non-direct environment is a render error
(`render/release-requires-direct`) with the gap named in the message.

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

## Preview: admission rejections and what the dry-run cannot see (#45)

A server-side dry-run (`kelson diff --dry-run=server`, `internal/delivery/dryrun`)
is not kelson's model of what the API server would do — it is the API server's
own answer, and that answer includes admission control. A dry-run apply runs the
cluster's `ValidatingAdmissionPolicy` objects and its validating webhooks, so a
Kyverno, Gatekeeper or plain-webhook denial arrives at preview time. It is
reported as a structured finding, never as a generic apply failure, and never
left to appear at deploy time after a preview that read clean.

Each denial becomes a `diff.PolicyViolation` carrying:

| field | what it holds |
|---|---|
| `code` | `policy/webhook-denied`, `policy/admission-policy-denied`, `policy/validation-failed`, `policy/audit-finding` |
| `policy` / `rule` | the policy, constraint or webhook name the server named — for a `ValidatingAdmissionPolicy`, the policy and its binding |
| `resource`, `path` | what was rejected, and the field where the server pointed at one |
| `message` | the server's own words, verbatim: the policy author wrote that sentence for this moment |
| `remediation` | the object to go and read (`kubectl get clusterpolicy …`, `kubectl get constraints …`, `kubectl get validatingadmissionpolicy …`, `kubectl get validatingwebhookconfigurations …`) |
| `enforcement` | `enforce` (a veto, a blocker) or `audit` (a policy-report finding, a warning) |

Parsing is conservative. Each engine formats its rejection as prose rather than
as a protocol, so the recognisers match the shapes those engines actually write;
a message that merely mentions a webhook, or says a request was denied without
the denial shape, is **not** attributed to anything. An unattributable rejection
is reported as unvalidated with the server's message intact — kelson never
invents a policy name for an agent or a human to go chasing.

### The honesty boundary

A dry-run does not reach every check, and the preview says so rather than
implying a coverage it does not have. Two cases, both reported as
`diff.Unvalidated` with a `reason`:

- **`dry-run-unsupported`** — a webhook whose `sideEffects` is neither `None`
  nor `NoneOnDryRun` is never called for a dry-run request. With
  `failurePolicy: Fail` the API server fails the request outright; that failure
  names the webhook and is reported as a gap, not as a rejection.
- **`webhook-excludes-dry-run`** — the same webhook with `failurePolicy: Ignore`
  is skipped *silently*: the dry-run succeeds and the preview would otherwise
  look like a complete verdict. So the preview lists the cluster's
  `ValidatingWebhookConfigurations` and reports each unreachable webhook that
  could match the batch. The match is coarse (group/version/resource);
  `namespaceSelector` and `objectSelector` are not evaluated, so this
  over-reports rather than under-reports. The read is best-effort — a caller
  without permission gets no gap entries, never a failed preview — and the grant
  is in `deploy/rbac/detect-clusterrole.yaml`.

### Preview of a helm component

One component type previews less than the rest, and it says so rather than looking complete.

A `kind: helm` component renders two resources — a chart source and a `HelmRelease` — and nothing else
([the model](model.md#helm-components-a-chart-delegated),
[ADR-0016](adr/0016-delivery-flows-v0.md)). **The preview therefore shows the `HelmRelease` changing:
the chart, the version and the values. It never shows the workloads the chart produces.** A chart
upgrade that rewrites every manifest it ships appears as one changed `version:` line, and a reviewer
approving it is approving a version number, not a set of manifests.

This is a documented v0 downgrade, taken deliberately, and it is worth being blunt about why it cannot
simply be fixed here. Expanding a chart means fetching it and running `helm template` against it, which
is network I/O — the one thing the renderer may never do
([ADR-0001](adr/0001-hybrid-state-model.md), [#20](https://github.com/dafrie/kelson/issues/20)) — so a
manifest-level chart preview is not a renderer feature that was skipped. The upgrade path is an
**advisory server-side** `helm template`: server-side because it needs to fetch the chart, advisory
because it is a second opinion about what the cluster will do rather than the rendered truth every other
diff shows. It changes the preview and not the delegation.

Two consequences follow, and neither is a defect in the diff:

- **Nothing is missing from the diff.** The two resources kelson owns are diffed exactly the way every
  other resource is, including under a server-side dry-run. What is absent from the diff is absent from
  kelson's inventory too — the chart's objects are helm-controller's, created under Helm's own release
  ownership, never pruned or adopted by kelson.
- **Drift inside the release is helm-controller's.** It has its own drift detection and its own
  reconcile interval; kelson does not police that layer and does not report on it.

### Exit semantics

`kelson diff` exit codes and the Diff RPC's `exit_semantics` are the same
contract, and both call `diff.Blocked` rather than restating it:

| code | meaning |
|---|---|
| 0 | no changes |
| 2 | changes present |
| 3 | blocked: an enforce-mode violation, or an unvalidated resource whose prerequisite is genuinely absent (`reason` `missing-prerequisite` or `unattributed-rejection`) |

A coverage gap is deliberately **not** a blocker. Nothing rejected anything, so
failing a pipeline on it would punish a hole in the preview as if it were a
verdict — but it is always printed and always present in the JSON, because an
incomplete preview must never read as a clean one.

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

## When a field cannot change in place (`delivery/immutable-field`)

Some fields are immutable once an object exists — most consequentially a
Deployment's `spec.selector`. An apply that changes one is rejected by the API
server with a 422, and it will be rejected identically on every retry: there is
no edit that reaches the new value, because the live object is what has to go.

The direct adapter classifies that rejection as `delivery/immutable-field`
rather than the generic `delivery/apply-failed`, names the stuck field, carries
the API server's own words in `cause`, and gives the only remediation that
works:

```
Deployment/checkout-production/web [delivery/immutable-field] the API server
refuses the update: spec.selector cannot change on an existing object: delete
Deployment/checkout-production/web and deploy again — re-deploying without
deleting fails the same way, and there is no in-place edit that reaches the new
value
```

The code exists because the generic remediation ("fix the spec, then
re-deploy") is actively wrong here: the spec is what the object *should* be.
[ADR-0027](adr/0027-finish-the-component-rename.md) is the change that made this
reachable — it renamed the selector label, so any workload deployed before it
must be deleted and redeployed. Deleting the whole environment
(`kelson uninstall --project <p> --env <e>`) and deploying again does the same
job for more than one workload.
