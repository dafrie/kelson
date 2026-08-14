# Delivery plane — the spine, and concurrency

Design reference for the delivery plane. The load-bearing rule —
**delivery never influences rendering** — is stated in
[ADR-0001](adr/0001-hybrid-state-model.md) and enforced by the package
structure: the renderer is a pure function; `internal/delivery` only ever
receives already-rendered manifests.

There is **one** delivery path ([ADR-0028](adr/0028-delivery-spine.md)). The
`Adapter` / `Registry` / `Capabilities` seam ADR-0001 introduced is collapsed
with the modes it selected between: one implementation needs no interface to
choose it.

## The spine

`kelson-controller` reconciles an `Environment` in six steps
([ADR-0027](adr/0027-crd-native-control-plane.md) creates it,
[ADR-0028](adr/0028-delivery-spine.md) is what it does):

| Step | What happens | Failure |
|---|---|---|
| 1 validate | `internal/model/validate.go` | `Ready=False`, `reason: SpecInvalid`, `status.validationErrors[]`, stop |
| 2 detect | read the `ClusterProfile` | a capability gap, reported with what would close it |
| 3 render | `(spec, ClusterProfile) → manifests`, pure | a structured render error |
| 4 publish | push the set as an immutable OCI artifact | a registry error; nothing downstream runs |
| 5 ensure | server-side apply the `OCIRepository` + `Kustomization` pair | a conflict on a field another manager owns |
| 6 observe | Flux conditions + workloads → the state machine → `Environment.status` | see [the three answers](#status-one-state-machine-three-answers) |

**The artifact.** `<registry>/kelson/<project>-<environment>:<generation>-<spec-hash-short>`.
`<generation>` is the `Environment`'s `.metadata.generation` — allocated by the
API server, monotonic, bumped on spec change and not on status writes, so
kelson allocates no revision numbers of its own. The tag is written once and
never rewritten; republishing an unchanged spec produces a digest the registry
already holds and uploads nothing. The publisher is the one PR previews already
use ([ADR-0017](adr/0017-pr-previews.md) decision 10) — one media type, one
determinism test, two callers.

**The two objects.** An `OCIRepository` pinned to the tag just pushed, and a
`Kustomization` with `path: ./`, `prune: true` and `spec.decryption` when the
environment's secret backend is `sops`. Both live in `kelson-system`, not the
workload namespace — they are kelson's objects, and a `Kustomization` deleted by
someone tidying an application namespace is a deployment that silently stops
reconciling. Both are applied with server-side apply under field manager
`kelson-controller` and carry the provenance labels, so
`kubectl get kustomizations -n kelson-system -l kelson.dev/project=x` is the
inventory.

What kelson stops owning is the interesting half: kustomize-controller does
apply ordering, wait-for-ready, prune by inventory, drift correction and retry
with backoff. Those are the five things the direct adapter reimplemented.

### History, rollback, promotion

| Verb | Mechanism |
|---|---|
| history | the registry's tag list. `Environment.status.history[]` mirrors the most recent 20 (revision, digest, spec hash, timestamp, resolved images, outcome) for humans and the API; the record is the registry, and a query past the window is a registry query |
| rollback | the annotation `kelson.dev/rollback-to: <revision>` on the `Environment`. The controller repoints the `OCIRepository` at that immutable tag and **suspends re-render** — steps 3 and 4 do not run — so the current spec cannot be republished over what you just rolled back to. Two things resume tracking and only two: removing the annotation, or editing the spec. The state is visible: `Progressing=False`, `reason: RollbackPinned`, naming both ways out |
| promotion | an authoring change, not a delivery operation: patch the target Environment's per-component image pin, stamped `kelson.dev/promoted-from: <env>@<revision>`, then reconcile normally ([the model](model.md#promotion)) |

Rollback stops being a replay. Nothing re-renders, nothing re-applies from a
stored journal — a pointer moves to bytes that already exist and cannot have
changed.

> **Transition (R1/R2, [#224](https://github.com/dafrie/kelson/issues/224) /
> [#225](https://github.com/dafrie/kelson/issues/225)).** The old machinery is
> deleted. `internal/delivery/direct` (the server-side applier, its wait logic
> and its JSONL history store), `internal/delivery/git` (the go-git writer),
> `internal/delivery/rollback`, the flux *adapter* and the
> `Adapter`/`Registry`/`Capabilities` seam are gone, and so are the ConfigMap
> spec and history stores. Surviving: `flux`'s status reader, reconciler,
> dynamic client and preview reader, plus `statemachine`, `install`,
> `uninstall`, `kube`, `dryrun` and `provenance.go`; `ManifestFiles` — the
> artifact's layout function — moved to `internal/preview`, the publisher.
>
> **Nothing applies yet.** `kelson deploy`, `kelson rollback`, `kelson promote`,
> `DeployService.{Deploy(dry_run=none),Rollback,History,Promote}` and
> `RenderService.Diff(from_revision)` refuse with the structured
> `delivery/not-implemented` code naming #224. `kelson render`, `kelson diff`,
> `kelson build`, `kelson profile`, `kelson install`/`uninstall`, the cluster
> secret backend and the MCP read and dry-run tools are unaffected.
> `kelson status` and `kelson explain` answer from the observation plane and
> state, in their output, that the delivery phase is not reported.

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
(`internal/delivery/statemachine`); step 6 feeds it by implementing a single
watch-based `Source` over the Flux objects and the workloads.

### Release commands, and the barrier that is not built yet

A rendered set is *ordered* — namespaces first, then the data services and charts, then the release
Job of any component that declares one, then the workloads — and order is all a set of manifests can
express ([#89](https://github.com/dafrie/kelson/issues/89)). Applying a Job before a Deployment does
not mean the Job finished first, so somebody has to wait.

The only path that could wait was the one where kelson performed the apply itself, and that path is
gone. `components[].release` is therefore a **validated refusal**
([ADR-0028](adr/0028-delivery-spine.md) decision 8): it is in `internal/model`'s gate table, refused by
name, rendering nothing, with [#227](https://github.com/dafrie/kelson/issues/227) in the message —
the mechanism this project already uses for a field it cannot honour, and strictly better than silently
dropping a migration.

**The Flux-native replacement is known and not built.** Two `Kustomization`s with `dependsOn`: the
first containing the release Job with a health check, the second the workloads. kustomize-controller
already waits on `dependsOn` and already assesses Job health, so the barrier becomes a dependency
edge instead of a pause in an apply loop — and it is possible only because kelson now owns the
`Kustomization` ([ADR-0028](adr/0028-delivery-spine.md) decision 3). ADR-0019's own "Revisit when"
predicted exactly this, and said that when it happens ADR-0019 is superseded rather than amended.

What the barrier must still guarantee when it is built is what the deleted apply loop guaranteed, and
these three claims carry forward from [ADR-0019](adr/0019-release-command-hook.md) unchanged:

- **A failed migration fails the deploy before anything rolls.** No workload of the new revision is
  applied and nothing is pruned, so the previous revision keeps serving. The error is
  `delivery/release-failed`, naming the Job and carrying the tail of its pod's output.
- **The wait is visible.** The Job is reported in the same `delivery.Status` shape the state machine
  consumes: `Reconciling` while it runs, `Rejected` when it fails, with the Job in `Cause`. No new
  phase — see the ADR.
- **A rollback does not re-run it.** Rolling the workload back does not roll a migration back, so
  re-running the old revision's release command would only repeat work the database has already done.

### History is the registry

There is no rendered-history store to keep. The registry holds every artifact ever published for an
environment, immutably, and that *is* the history — nothing stores rendered manifests a second time,
and the 1 MiB ConfigMap budget of [ADR-0013](adr/0013-server-state-and-api-v0.md) §1 stops being
arithmetic the code has to do. `Environment.status.history[]` is a bounded mirror of the most recent 20
entries for the CLI, the UI and the API to read in one call.

The cost, stated where it matters: **a lifecycle policy on your registry is now a data-retention
policy on kelson's history**, and nothing in kelson says so at the point where you set it.

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

Concurrent edits — agent vs human, or a `kubectl apply` against a UI write —
must conflict loudly, never last-write-wins.

### Spec versioning

The spec's version is the custom resource's `resourceVersion`, carried through
the API as the same opaque `version` string it always was
([ADR-0027](adr/0027-crd-native-control-plane.md) decision 6). The API accepts
an expected version on writes and a mismatch is `store/version-conflict`; the
store vocabulary is unchanged, only what it is a vocabulary *about*.

### Field ownership

Writes are server-side applies with a named field manager — `kelson-server` for
API writes, `kelson-controller` for the Flux objects it owns — so two managers
disagreeing about a field is a conflict the API server reports rather than a
silent overwrite. The controller writes only `status`, which is a subresource,
so a controller status write cannot race a user's spec write at all.

### Which operations may retry automatically

- **May retry**: publishing an unchanged manifest set (the digest already
  exists), a reconcile that lost a race and can re-read, a rollback to the
  revision already pinned.
- **Must surface**: a spec write that conflicts on `resourceVersion`, and a
  server-side apply that conflicts on a field owned by another manager. These
  are the "discard human intent invisibly" failures and always go to the user
  as a structured conflict error.

## When a field cannot change in place (`delivery/immutable-field`)

Some fields are immutable once an object exists — most consequentially a
Deployment's `spec.selector`. An apply that changes one is rejected by the API
server with a 422, and it will be rejected identically on every retry: there is
no edit that reaches the new value, because the live object is what has to go.

kelson classifies that rejection as `delivery/immutable-field` rather than the
generic `delivery/apply-failed`, names the stuck field, carries the API server's
own words in `cause`, and gives the only remediation that works:

```
Deployment/checkout-production/web [delivery/immutable-field] the API server
refuses the update: spec.selector cannot change on an existing object: delete
Deployment/checkout-production/web and deploy again — re-deploying without
deleting fails the same way, and there is no in-place edit that reaches the new
value
```

The code exists because the generic remediation ("fix the spec, then
re-deploy") is actively wrong here: the spec is what the object *should* be.
[ADR-0032](adr/0032-finish-the-component-rename.md) is the change that made this
reachable — it renamed the selector label, so any workload deployed before it
must be deleted and redeployed. Deleting the whole environment
(`kelson uninstall --project <p> --env <e>`) and deploying again does the same
job for more than one workload.

The classifier that raised it lived in the direct adapter and went with it
([ADR-0028](adr/0028-delivery-spine.md)); the code and its helpers stay in
`internal/delivery` because the failure belongs to any last mile that applies to
a live API server, and the spine's reconcilers meet it too
([#224](https://github.com/dafrie/kelson/issues/224)). Today the rejection
surfaces as Flux's own `Kustomization` failure condition, which reports the API
server's message verbatim.
