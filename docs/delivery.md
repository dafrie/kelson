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
determinism test, two callers. The push is bounded twice: one minute per HTTP
request and two minutes for the whole publish. A reconcile carries no deadline
of its own and the worker pool is small, so a registry that accepts a connection
and then answers nothing would otherwise hold one worker for as long as it liked
— and a controller that has quietly stopped reconciling every other environment
is a worse failure than a publish that gave up and requeued.

**The two objects.** An `OCIRepository` pinned to the artifact just pushed — by
`spec.ref.tag` *and* `spec.ref.digest`, whenever kelson knows the digest — and a
`Kustomization` with `path: ./`, `prune: true`, `wait: true`,
`targetNamespace: <the resolved namespace>` and `spec.decryption` when the
environment's secret backend is `sops`. Both are named `<project>-<environment>`
and both live in `kelson-system`, not the workload namespace — they are kelson's
objects, and a `Kustomization` deleted by someone tidying an application
namespace is a deployment that silently stops reconciling. Both are applied with
server-side apply under field manager `kelson-controller` and carry the
provenance labels, so
`kubectl get kustomizations -n kelson-system -l kelson.dev/project=x` is the
inventory.

Both halves of the reference are written because they answer different
questions. The tag is the name a human reads out of
`kubectl get ocirepository`; the digest is the bytes. source-controller prefers
the digest when both are set, so pinning both means what the cluster pulls
cannot differ from what this controller pushed — a kelson tag is written once
and never rewritten, but a registry is a shared system with mirrors, retention
policies and operators in it, and "cannot differ" is worth more than "should not
differ". The digest is unknown in exactly one case: a rollback to a
`status.history` entry that carries none, which is a tag-only pin. A rollback to
a revision the mirror has forgotten resolves its digest from the registry, so it
pins both halves like any other.

`wait: true` is the load-bearing one. kustomize-controller assesses the health
of everything it applied and only then reports `Ready`, so **`Ready` means
healthy and not merely applied** — the phase in `Environment.status` is correct
without the controller reading a single Pod. What it is not is *diagnosable*,
which is what the readback below adds.

Beside the standard provenance the pair carries one label of its own,
`kelson.dev/environment-namespace`: the namespace the *custom resource* lives
in. Two `Environment`s of the same name binding `Project`s of the same name, in
two namespaces, resolve to one object name in one Flux namespace, and a
server-side apply would take the object without a word. The label is what makes
that collision detectable — a live object naming a different namespace is
refused with `NameConflict` and nothing is written.

What kelson stops owning is the interesting half: kustomize-controller does
apply ordering, wait-for-ready, prune by inventory, drift correction and retry
with backoff. Those are the five things the direct adapter reimplemented.

### Deleting an Environment deletes its workloads

`Environment` carries the finalizer `kelson.dev/environment`, added on the first
reconcile that successfully applies the pair. On deletion the controller removes
the `Kustomization` first — which prunes everything it applied — then the
`OCIRepository`, and only then releases the finalizer
([ADR-0028](adr/0028-delivery-spine.md), the 2026-08-14 amendment).

> **`kubectl delete environment production` is not a bookkeeping operation.**
> It removes the `Kustomization`, the `Kustomization` prunes its inventory, and
> the running application goes with it. That is what a `prune: true`
> `Kustomization` *is*; it is not a new behaviour, but it is not one to derive
> from first principles at the moment you run the command. The one way to delete
> an `Environment` and keep its workloads is
> [`kelson.dev/orphan-on-delete`](#the-escape-hatch-kelsondevorphan-on-delete),
> below — set it before the delete, or on an `Environment` still sitting in
> `Terminating`.

Two things are deliberately left behind. **The workload namespace**, because
deleting a namespace cascades to everything inside it — including resources
kelson never created — and the provenance labels can never prove kelson created
the namespace rather than adopting one that already existed.
`kelson uninstall` is the verb that reasons about namespaces. And **the
published artifacts**, because they are the history and they are immutable:
deleting an `Environment` must not make its own record unrecoverable, and
re-applying the same spec finds every revision it ever published still there.

The order matters for one reason: deleting the `OCIRepository` first would leave
the `Kustomization` pointing at a source that no longer exists, so it would stop
reconciling with an error, prune nothing, and the workloads would outlive the
`Environment` that declared them.

The finalizer is added *after* the first apply, which leaves one API round trip
in which the pair exists and nothing protects it. A delete that lands there
takes the custom resource immediately — there is no finalizer to block it — so
the reconcile that applied the pair is the only thing left that knows about it,
and it tears the pair down itself rather than returning
([ADR-0028](adr/0028-delivery-spine.md), the amendment). It is the one teardown
no later reconcile can retry.

#### The escape hatch: `kelson.dev/orphan-on-delete`

```sh
kubectl annotate environment production kelson.dev/orphan-on-delete=true
kubectl delete environment production      # the application keeps running
```

The annotation is an explicit opt-in and the teardown above stays the default
([#242](https://github.com/dafrie/kelson/issues/242),
[ADR-0028](adr/0028-delivery-spine.md), the 2026-08-15 amendment). With it in
force the finalizer still runs and still releases itself — an opt-out that made
the custom resource undeletable would be a worse trap than the behaviour it opts
out of — and the only thing it skips is the teardown. It is read at *deletion*
time, so it can also be added to an `Environment` that is already `Terminating`,
which is how a stuck deletion is released; it skips whatever teardown has not
happened yet and cannot restore what has.

**What an orphaned environment is.** The `OCIRepository` and the `Kustomization`
named `<project>-<environment>` stay in `kelson-system`, keep their
`kelson.dev/*` provenance labels, and go on reconciling the last artifact kelson
published — pinned to an immutable tag and digest, drift-corrected every five
minutes, forever. The application does not notice. What is gone is the thing that
*declared* them: no `Environment` in any namespace binds those objects any more,
so nothing re-renders, nothing re-publishes, and no `status` describes them.
That is what *orphaned* means here, and it is discoverable in the one place it
has to be — the cluster:

```sh
kubectl get ocirepositories,kustomizations -n kelson-system -l kelson.dev/managed-by=kelson \
  -L kelson.dev/project,kelson.dev/environment,kelson.dev/environment-namespace
```

Any row whose `kelson.dev/environment-namespace` / `kelson.dev/environment` pair
names an `Environment` that no longer exists is an orphan. The deletion also
records a `Normal` event with reason `Orphaned` on the `Environment` it deleted,
which outlives the object: `kubectl get events -n <namespace>` is where the
answer to "why is there a `Kustomization` here that nothing owns" still is, for
as long as the cluster keeps events.

**Two ways back.** Re-apply an `Environment` of the same name, in the same
namespace, binding the same `Project`, and the controller **adopts the pair
back**: the objects are named after the project and environment and are applied
with server-side apply, so the next reconcile converges on the ones that were
left behind rather than colliding with them (a live object labelled with a
*different* environment namespace is still refused with `NameConflict` — that
check is what keeps two environments of one name apart, and adoption does not
weaken it). Or finish the job by hand:
`kubectl delete kustomization <project>-<environment> -n kelson-system` prunes
the workloads exactly as the teardown would have, and the `OCIRepository` can go
after it.

**A value kelson cannot read means orphan.** `true`, `True`, `1` opt in;
`false` and `0` are the default said out loud. Anything else — `ture`, `yes`, an
empty value — is honoured as an opt-in *and* recorded as a `Warning` event with
reason `OrphanAnnotationMalformed`. The two mistakes are not the same size:
reading a typo as "no" prunes a production namespace because of a
misspelling in the annotation whose whole purpose was to prevent that, and
nothing undoes it, while reading it as "yes" leaves an application running that
one `kubectl delete kustomization` removes whenever somebody notices. Annotations
are not validated by a webhook today — kelson has none — which is exactly why
this parses defensively rather than assuming the value is well formed.

### When delivery refuses

Steps 4 to 6 refuse with a closed set of reasons, and each one decides exactly
one requeue behaviour — so the reason in `status.conditions` also tells a reader
whether anything is going to happen next without them.

| Reason | What happened | What the controller does next |
|---|---|---|
| `FluxNotInstalled` | the `ClusterProfile` reports no Flux | retries in 5m, **no error return, never a crash loop** — a cluster with no Flux is the expected state of a fresh install ([ADR-0030](adr/0030-flux-aio-install.md)), and `kelson install` is the fix |
| `RegistryNotConfigured` | the controller was started without `--registry` | status only; nothing changes on its own |
| `ArtifactRefInvalid` | the prefix, the names or the generation do not make a repository and a tag | status only |
| `NameConflict` | a live object of that name belongs to a different environment namespace | status only, and **nothing is written** |
| `RollbackTargetUnknown` | `kelson.dev/rollback-to` names a revision that is in neither `status.history` nor the registry's tag list | status only |
| `RegistryUnreachable` | the registry never answered | error return → controller-runtime's exponential backoff |
| `PushDenied` | the registry answered a push and said no | retries in 5m; a credential is an operator's to fix, and retrying into a rate limit helps nobody |
| `RegistryReadDenied` | the registry answered a *read* and said no — listing tags and resolving a rollback target need pull scope, not only push | retries in 5m; reported as itself so "kelson may not look" is never recorded as "that revision does not exist" |
| `FluxApplyForbidden` | the API server refused the write | retries in 5m; RBAC is an operator's to grant |
| `FieldManagerConflict` | a server-side apply conflicted despite `ForceOwnership` | retries in 5m; something structural is contended |

Step 2 has one refusal of its own, and it is deliberately not in that table.
`ClusterProfileUnavailable` means the controller could not read what the cluster
provides — an RBAC gap, an API server that did not answer — and it exists
because that failure and a cluster that genuinely has no Flux produce the same
empty `ClusterProfile` and mean opposite things. Reporting it as
`FluxNotInstalled` would tell an operator to install what they may already have.
The controller retries with backoff and probes again each time; the start-up
probe that failed no longer freezes an empty profile for the life of the
process, though the Flux *watches* still depend on the start-up answer and still
need a restart ([#133](https://github.com/dafrie/kelson/issues/133)).

Step 3 has one of the same shape. Resolution is handed the `GitSource` objects
the instance declares, because a component's `source:` may name one
([ADR-0035](adr/0035-sources.md) §3) and only a plane with cluster access can
see that half of the scope; the controller lists them from its own namespace,
the one `--flux-namespace` names. `SourcesUnavailable` means that listing
failed. It is not `SpecInvalid`, for the reason `ClusterProfileUnavailable` is
not `FluxNotInstalled`: resolving against the empty list would refuse the
component with `ref/unknown-source` — "this instance offers: nothing" — and send
an author to fix a binding that was never wrong. Nothing is rendered and the
controller retries with backoff.

Two conditions carry the answer. `Ready` is whether the environment is serving
what it should. `Progressing` is whether kelson is still working on it — and the
two disagree in exactly one situation, which is the reason the second condition
exists: a rolled-back environment is `Ready=True` (the pinned revision is live)
with `Progressing=False`, `reason: RollbackPinned` (and it is deliberately not
tracking your spec).

### History, rollback, promotion

| Verb | Mechanism |
|---|---|
| history | the registry's tag list. `Environment.status.history[]` mirrors the most recent 20 (revision, digest, spec hash, timestamp, the image each component resolved to, outcome) for humans and the API; the record is the registry, and `kelson history` pages past the window into it — those revisions arrive marked `beyond_window`, carrying the one fact the registry has (this revision exists) and nothing else, because when it was published, what it ran and how it ended were observations of a cluster. An entry is written only on a **new** publish, deduped by revision, and the newest entry's `outcome` is refreshed while it is the current revision and frozen once a newer one takes its place — so an old entry says how that deployment *ended*, not what it looked like one second in |
| rollback | the annotation `kelson.dev/rollback-to: <revision>` on the `Environment`, naming any revision the mirror **or the registry** can confirm — an aged-out one is the same pointer move, and the status says kelson cannot describe it. The controller repoints the `OCIRepository` at that immutable tag and **suspends re-render** — steps 3 and 4 do not run — so the current spec cannot be republished over what you just rolled back to. Two things resume tracking and only two: removing the annotation, or editing the spec. The state is visible: `Progressing=False`, `reason: RollbackPinned`, naming both ways out |
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
> artifact's layout function — now lives in `internal/artifact`, the publisher
> the preview pipeline and the spine share (ADR-0028 decision 2).
>
> **The controller deploys, and the CLI verbs deploy through it.**
> `kelson-controller` runs all six steps above: applying a `Project` and an
> `Environment` publishes an artifact, creates the Flux pair and drives
> `Environment.status` to `Healthy`. `DeployService.{Deploy,Status,Rollback,
> History,Promote}` are reshaped over the CRs — SSA of the spec, an
> `Environment.status` watch, the `kelson.dev/rollback-to` merge patch, the
> promotion splice — and `kelson deploy`/`rollback`/`promote`/`history` are
> ConnectRPC clients of that façade rather than refusing (#225). What has not
> moved: `RenderService.Diff(from_revision)` still refuses with
> `delivery/not-implemented` naming #224, because a revision diff needs the
> rendered-history store ADR-0027 deleted and nothing has replaced it yet.
> `kelson render`, `kelson diff`, `kelson build`, `kelson profile`,
> `kelson install`/`uninstall`, the cluster secret backend and the MCP read and
> dry-run tools are unaffected. `kelson status` now reads the delivery phase
> from `DeployService.Status` too — a ConnectRPC client of the façade for that
> half alone (`--server`/`--password`/`--token`, matching every other
> façade-backed verb), degrading to a "not reported" line naming `--server`
> when none answers, because the workload half still reads the cluster
> directly and needs no server at all. `kelson explain` still answers from the
> observation plane alone and states, in its output, that the delivery phase is
> not reported.
>
> The chart's controller RBAC has landed ([#226](https://github.com/dafrie/kelson/issues/226)):
> `create`/`patch`/`delete` on `source.toolkit.fluxcd.io` `ocirepositories` and
> `kustomize.toolkit.fluxcd.io` `kustomizations` in the flux namespace, and
> `patch` on `environments` — the resource itself, because that is where a
> CustomResourceDefinition keeps `metadata.finalizers`. `environments/finalizers`
> is the kubebuilder spelling and the chart grants it too, but a CRD serves no
> `/finalizers` endpoint, so granting only the subresource authorizes nothing:
> the controller then publishes the artifact, Flux applies it, and every
> reconcile still ends in `cannot patch resource "environments"` with `.status`
> never written.

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
watch-based `Source` over the Flux objects and the workloads. The third answer
is the one [the workload readback](#the-workload-readback-which-pod-which-container)
makes specific.

A phase that settled badly reaches `Ready=False` with a reason of its own, and
the three are three different things to go and do
([#256](https://github.com/dafrie/kelson/issues/256)):

| Phase | `Ready` reason | What happened |
|---|---|---|
| `Rejected` | `ApplyFailed` | kelson published the revision and Flux would not put it on the cluster: the build, the decryption, the dry-run or the apply. It is not `FluxApplyForbidden`, which is the API server refusing *kelson's* write of the two Flux objects, before Flux has seen anything |
| `Degraded` | `WorkloadDegraded` | it is live and a workload under it is definitively failing; `status.workloads` names the workload, the Pod and the container, and the message names the first of them |
| `Degraded` | `Unhealthy` | it is live and reported unhealthy with no workload kelson can name: a health check or a prune that failed over a kind the readback does not classify, or a readback that is not wired or was refused |

The message stays Flux's own words — or the readback's — in every case. The
reason is the half a `kubectl get -o jsonpath` or an agent branches on, which
is why the readback's verdict wins whenever it has one: both are true of the
same environment, and only one of them can say which Pod.

### The workload readback: which Pod, which container

`Ready=False` on a `Kustomization` is true and not actionable. Step 6 therefore
does a second read ([#240](https://github.com/dafrie/kelson/issues/240)): it
lists the `Deployment`s carrying this environment's provenance labels, lists the
Pods in each one's selector set, and runs them through the *same* classifier
`kelson status` uses (`internal/observation`, [#53](https://github.com/dafrie/kelson/issues/53)).
The result is `Environment.status.workloads`:

```yaml
status:
  phase: Degraded
  revision: 7-1a2b3c4d
  workloads:
    checked: 3
    healthy: 2
    degraded: 1
    unhealthy:
      - resource: Deployment/checkout-production/worker
        code: crash-loop-back-off
        reason: CrashLoopBackOff
        remediation: the container keeps crashing: read its logs, fix the command or startup error, then redeploy
        containers:
          - pod: worker-6d4f9c-2xq7b
            name: app
            code: crash-loop-back-off
            reason: CrashLoopBackOff
```

Five properties, each of which is a decision:

- **The counts are complete; the list is a bounded sample.** `degraded` says how
  many are failing, `unhealthy` holds at most 10 of them and each entry at most
  5 containers, ordered by resource name and by pod-then-container. A status is
  copied into every watch event every controller in the cluster receives, and a
  bad rollout with fifty replicas would otherwise put fifty crash dumps there.
  The order is what keeps a truncated list stable rather than churning under
  `kubectl get -w`.
- **The codes are the observation plane's, not a second vocabulary.**
  `crash-loop-back-off`, `image-pull-back-off`, `failing-probe`,
  `insufficient-resources`, `scheduling-failed`, `missing`, `secret-sync-failed`
  — the same strings `kelson status` reports, restated as constants in
  `api/kelson/v1alpha1` and drift-tested against `internal/observation`.
- **No container output, ever.** The pod name, the container name and the
  kubelet's reason identify the failure; the container's *logs* are where a
  connection string leaks, and an `Environment`'s status is readable by anyone
  who can read the `Environment`. `kelson logs` reads them instead, under the
  server's own grant.
- **"Could not check" is not "checked and fine".** A readback the API server
  refused sets `workloads.unavailable` with the reason and leaves every count at
  zero — the [`ClusterProfile`](detection.md)'s discipline — and never
  fails the reconcile that already delivered the revision.
- **It may only downgrade, and only after Flux has settled.** A definitive
  failure turns a settled `Healthy` or `Applied` into `Degraded` — with
  `Ready=False`, `reason: WorkloadDegraded` — because
  `wait: true` reports on the moment the set converged and says nothing about
  the Pod that started crash-looping ten minutes later. It never upgrades, and
  it never touches `Committed` or `Reconciling`: while Flux is still working the
  Pods on the cluster are the *previous* revision's, and reporting them would
  make every rolling update flash `Degraded`.

The grant this needs is read-only `get`/`list` on `pods` and `deployments`, in
the controller's `ClusterRole`. No `watch` (the readback lists through a direct
client on a loop that already requeues), no `pods/log`, and no other workload
kind: the classifier reads a `Deployment` and its Pods, so `CronJob`s, `Job`s
and the operator-owned data services stay covered by `wait: true` alone.

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

The mirror is not the ceiling. `kelson history` lists the registry's tag list past the window and
`kelson rollback --to` restores anything in it ([#241](https://github.com/dafrie/kelson/issues/241)):
both processes read the record through the same `--registry` and `--registry-config`, and the
credential needs pull scope as well as push. What the registry cannot give back is everything the
mirror held beside the tag — when a revision was published, which images it ran, how that deployment
ended — because those were observations of a cluster. An aged-out revision therefore arrives marked
as one kelson can restore exactly and cannot describe, in the CLI, in the UI and in the rollback
preview, rather than as a row of blanks.

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
