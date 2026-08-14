# ADR-0027: The control plane is CRD-native — `Project` and `Environment` are custom resources

- **Status:** Accepted
- **Date:** 2026-08-14

> Supersedes the storage half of [ADR-0013](0013-server-state-and-api-v0.md) — §1's ConfigMap spec and
> history stores. ADR-0013's API surface (§2), trust model (§3) and codegen convention (§4) are
> unaffected and live on. Amends [ADR-0003](0003-install-model.md): the additive-install doctrine
> stands, and installing a CRD is shown below to be additive.
> Read with [ADR-0028](0028-delivery-spine.md), which is what the controller this ADR creates
> actually does.

## Context

[ADR-0013](0013-server-state-and-api-v0.md) §1 stored specs and deployment history as ConfigMaps and
named its own successor in the same paragraph: *"A CRD-based store is the natural successor when the
controller exists (M13+) and is explicitly the migration target recorded here — the store is behind an
interface precisely so that swap does not touch handlers."* The controller now exists as a decision.
The condition ADR-0013 wrote down has arrived on schedule rather than by surprise, and this ADR is the
swap it promised, taken one step further than a store swap.

Three pressures decided that it is taken now rather than after a release.

**The ConfigMap store is a database with a Kubernetes accent.** It has a spec table
(`kelson-spec-<project>`), a history table (`kelson-hist-<project>-<env>-<revision>`), a retention
policy, listing by label, and an optimistic-concurrency token. Kubernetes offers every one of those,
typed — and a ConfigMap offers none of them as more than a blob plus a convention the code has to keep.
What is actually lost is not elegance: there is no status subresource, so observed state has nowhere to
go; no `kubectl get projects`; no printer columns; no field selectors; no server-side schema check; and
nothing can `watch` for a spec change without re-parsing an opaque data map to find out what changed.

**Delivery is becoming a loop, and a loop needs an object.** [ADR-0028](0028-delivery-spine.md) makes
deployment a reconciliation: render, publish, ensure Flux objects, observe, repeat. A request handler
can return a value; a reconciler has to *record* one — a `.status` with an observed generation, a set
of conditions and a settled revision, updated on a timer by a process nobody is waiting on. On a
ConfigMap that record would have to be invented, in an annotation, with hand-written conflict handling,
and the invention would be a worse copy of a status subresource.

**The reason ADR-0013 gave for avoiding a CRD has weakened on inspection.** It read
[ADR-0003](0003-install-model.md)'s "never install what is already there" and additive-install promise
as *therefore no CRD, because a CRD is an install step*. But a CRD is the most additive thing a project
can install: it registers an API group nobody else claims, mutates no existing object, changes no
existing behaviour, and `kubectl delete crd projects.kelson.dev` removes it entirely. What ADR-0003 was
protecting against — a component that takes over part of a cluster the operator already runs, or a
webhook whose outage breaks unrelated applies — is a real hazard, and this ADR still refuses the second
one (decision 5). "kelson installs nothing" was never the promise; "kelson installs nothing that is
already there, and nothing that changes what is" was.

kelson is pre-release. There is no stored spec anywhere that a migration would have to preserve, so the
back-compatibility question that would normally dominate this decision does not exist. It will exist
after the first tag, which is the argument for doing it before one.

## Decision

### 1. `Project` and `Environment` are CRDs in `kelson.dev/v1alpha1`, with status subresources

Both are namespaced. Both carry a `status` subresource, so a controller's status write cannot race a
user's spec write and the controller needs no write permission on the spec.

`status` carries, on both kinds:

- `observedGeneration` — the `.metadata.generation` this status describes.
- `conditions[]` — standard `metav1.Condition`, with `Ready` as the summary condition.
- `validationErrors[]` — decision 5.

`Environment.status` additionally carries the delivery record: the settled revision, the artifact
reference, the phase from the existing state machine, and the bounded history mirror
([ADR-0028](0028-delivery-spine.md) decides its contents).

The kinds match the documents users already write — one `Project`, one `Environment` per environment —
because [ADR-0006](0006-project-application-environment.md)'s three-level shape and
[ADR-0014](0014-components.md)'s single `components:` list are the model, and a CRD that did not mirror
them would be a third spelling of the same thing.

### 2. A new binary, `kelson-controller`, on controller-runtime, with a hand-rolled `main`

`cmd/kelson-controller` builds a manager, registers two reconcilers, and runs. No kubebuilder.

controller-runtime is a library and kelson takes the library: the manager, the client with its cache,
the informers, leader election, the `Result{RequeueAfter}` contract. What is refused is the scaffolding
around it. kubebuilder's value is standing up a repository that does not exist yet; kelson's repository
exists, with per-plane depguard fences, committed generated artifacts guarded by drift tests, and its
own schema pipeline. Adopting the scaffold means adopting its `Makefile`, its `config/` kustomize
layout, its marker comments and its CRD generation — a second convention for generated artifacts and a
second place a reader must look to find out what the API is. The generated code that *is* wanted
(deepcopy) is one build tool, and decision 3 takes exactly that one.

### 3. Spec structs stay in `internal/model`; `api/kelson/v1alpha1` adds only the wrappers and status

`internal/model` remains the single home of what a kelson spec *is* — `ProjectSpec`, `EnvironmentSpec`,
their component types, their yaml/json tags, and `validate.go`. Nothing is copied.

A new **public** package `api/kelson/v1alpha1` holds, and holds only:

- `Project` / `Environment` with `metav1.TypeMeta` + `metav1.ObjectMeta`, `Spec` typed as the
  corresponding `internal/model` struct, and `Status` typed as the new status structs;
- `ProjectList` / `EnvironmentList`;
- the status types themselves;
- `zz_generated.deepcopy.go`, produced by `controller-gen` run as a pinned build tool and committed
  with a drift test, exactly like `internal/api/gen` and `schema/*.json`;
- `SchemeBuilder` / `AddToScheme`.

It holds no validation, no defaulting and no behaviour. It is public because a CRD's Go types are part
of the contract other people's controllers consume; everything else stays `internal/`.

The split exists for one mechanical reason worth stating: `internal/model` is imported by the pure
renderer and by the CLI, and the `main` depguard rule allows only stdlib, kelson, `jsonschema`, `cobra`
and `yaml.v3`. `metav1` cannot go there. Putting the Kubernetes wrappers in a separate package keeps
apimachinery out of the authoring plane and gives `.golangci.yml` one new rule for `api/**` rather than
a loosened `main`.

### 4. CRD YAML comes out of `internal/schemagen`, into committed `deploy/crds/*.yaml`

`internal/schemagen` already reflects over the model structs to produce `schema/*.json`, and the
output is committed and drift-tested. It is extended to emit, from the same reflection over the same
structs, the `openAPIV3Schema` of each CRD — into `deploy/crds/projects.kelson.dev.yaml` and
`deploy/crds/environments.kelson.dev.yaml`, committed, guarded by the same drift test.

**No kubebuilder markers.** A marker-driven generator would be a second pipeline reading a second set
of annotations to answer the same question `schemagen` already answers: *what is a valid kelson spec?*
Two pipelines means two truths, and the first time they diverge — a field added with a `json` tag and
no marker, or vice versa — the divergence is silent and the JSON Schema and the CRD disagree about
what the user may write. One pipeline, two output formats.

`kelson install` applies `deploy/crds/`, and it is an ordinary catalog entry under
[ADR-0021](0021-installing-missing-components.md)'s rules — pinned, provenance-labelled, removable by
`kelson uninstall`, except that these manifests are kelson's own and therefore embedded rather than
fetched.

### 5. Validation stays in `internal/model/validate.go`; an invalid spec becomes a status, not a rejection

`validate.go` is the single source of truth and it acquires a third caller. The CLI calls it, the
server calls it on `PutSpec`, and the controller calls it at the top of every `Reconcile`. All three
produce the same `model.Error` values with the same slash codes, the same field paths and the same
remediation text.

**In the controller, an invalid spec is a status.** `Ready=False`, `reason: SpecInvalid`, and
`status.validationErrors[]` carrying each error's code, field, message, remediation and — where the
source is available — line and column. The taxonomy on the wire, in the CLI and in the status is one
taxonomy; agents branch on `schema/unknown-field` wherever they meet it. Nothing renders, nothing is
published, and the previous revision keeps serving.

**Cheap structural invariants are additionally CEL rules on the CRD schema**, generated by `schemagen`
from the same table that drives them in Go: required-together fields, enum membership, `kind: service`
needs a `port`, `kind: cron` needs a `schedule`, no duplicate component names. This is deliberately a
second spelling of a subset of `validate.go`, and it is accepted because it buys the thing a status
cannot: `kubectl apply` of a malformed document fails *at the API server*, immediately, at the author's
terminal, instead of being accepted and explained thirty seconds later by a controller. The rules are
generated, so they cannot drift; the taxonomy of record is still `validate.go`'s, and anything needing
resolution, a ClusterProfile or cross-document context stays there and only there.

**An admission webhook is explicitly deferred.** A validating webhook would let the full taxonomy
refuse at apply time, and it costs a serving certificate, a `CABundle` to rotate, a `Service` that must
be reachable from the API server, and a failure mode where a broken kelson deployment starts rejecting
applies. That is a poor trade for a project whose install doctrine is *lightweight and additive*, and a
particularly poor one on the single-node and edge clusters [ADR-0030](0030-flux-aio-install.md)
targets. Tracked in the issue tracker; the CEL rules are what makes deferring it tolerable.

### 6. `kelson-server` survives, as a stateless façade over the Kubernetes API

The ConnectRPC surface of ADR-0013 §2 is unchanged on the wire. What changes is underneath: `SpecService`
reads and writes custom resources instead of ConfigMaps, using server-side apply with field manager
`kelson-server`. `DeployService.Status` reads `Environment.status` instead of computing it. ADR-0013 §1's
rule — *the process holds no state a restart or a second replica would lose or fork* — is more true
after this change than before it.

Optimistic concurrency is still `resourceVersion`, carried through the API as the same opaque `version`
string, and a conflicting write still surfaces as `store/version-conflict`. The store vocabulary
(`store/version-conflict`, `store/not-found`, `store/too-large`) is unchanged; only what it is a
vocabulary *about* changed.

**The loss, stated plainly: byte-faithful round-tripping of the authored document dies here.**
ADR-0013 §1 promised that the store returns what you stored — the user's YAML, verbatim, comments and
key order intact — and built promotion's byte-splice ([ADR-0016](0016-delivery-flows-v0.md)) on that
promise. A custom resource is a decoded, re-serialized object; `PutSpec` followed by `GetSpec` now
returns an *equivalent* document, not the same bytes. Comments do not survive. This is a real
regression for anyone treating the server as their document store, and it is accepted because the
document store the project actually recommends is the user's own git repository, where byte fidelity is
git's job and always was. `kelson-server` becomes what it should have been: an API over cluster state,
not a home for a file.

### 7. The agent and audit stores move to `internal/controlstore`

[ADR-0024](0024-agent-identities.md)'s agent records and [ADR-0026](0026-agent-audit-trail.md)'s
bounded audit ring are also ConfigMaps in `internal/serverstate`, and they are *not* what this ADR
deletes. They are control-plane records — who a principal is, what a principal did — not delivery
state, and neither the controller nor Flux has any interest in them. They relocate to
`internal/controlstore` with their behaviour unchanged, so that the package name stops implying they
are the server's memory of a deployment. Whether they should also become CRDs is a real question with a
weaker case (an audit ring wants a fixed-size buffer more than it wants a typed API) and it is a
tracked follow-up, not this decision.

`internal/serverstate`'s spec and history stores are deleted outright.

## Rationale

**A store swap would have been half the change.** ADR-0013 put the store behind an interface so the
handlers would not move, and that was the right hedge at the time. But the interface's shape —
`Append/List/Latest/Get/Rendered/NextRevision` — is a *journal's* shape, written for a CLI that applies
and records. What ADR-0028 needs is not a journal behind a nicer store; it is an object a controller
converges on. Keeping the interface and putting CRs behind it would have preserved the seam and thrown
away the reason for crossing it.

**The status subresource is the actual prize.** Everything else a CRD gives — typing, `kubectl get`,
printer columns, field selectors — is convenience. `status` is a structural answer to a question
ConfigMaps cannot answer at all: *what does the system currently believe about this object, as of which
generation?* Every reconciliation loop needs it, no ConfigMap has it, and inventing it in annotations is
how projects end up with a status field that lies.

**Validation in one place, refused twice, is not duplication.** The CEL rules and `validate.go` overlap
on purpose and the overlap is generated. The alternative postures were both worse: CEL-only would mean
two taxonomies and the loss of remediation text and line/column positions; `validate.go`-only would mean
`kubectl apply` accepts anything and the author finds out from a controller they may not be watching.
The rule is that the *taxonomy* has one home, not that the *check* runs in one place.

**Not kubebuilder, and this is a repository-shape argument rather than a taste one.** Kubebuilder would
add a second generated-artifact convention to a repository that already has three pipelines
(`buf` → `internal/api/gen`, `schemagen` → `schema/*.json`, `specrefdoc` → `docs/reference/`) all
sharing one convention: committed output, drift test, `make` target. Adding controller-gen for deepcopy
extends that convention by one row. Adding kubebuilder replaces it for one plane.

## Consequences

**Positive.**

- `kubectl get environments -A` answers "what is kelson running here" without kelson, which is the same
  anti-lock-in property [ADR-0001](0001-hybrid-state-model.md) claimed for rendered manifests, now true
  of the control plane too.
- A reconciler has somewhere to write what it observed, so [ADR-0028](0028-delivery-spine.md) does not
  have to invent one.
- The API server rejects malformed documents before kelson sees them, and the OpenAPI schema means
  `kubectl explain project.spec.components` works.
- `kelson-server` gets smaller: no store implementation, no retention logic, no gzip-and-base64
  packing, no 1 MiB budget arithmetic.
- Every existing consumer of the wire API — CLI, UI, MCP, agents — is unchanged, because §2 of ADR-0013
  did not move.

**Negative — stated as plainly as the positives.**

- **The authored document no longer round-trips byte for byte.** Comments and key order are lost on
  store-and-fetch. Anyone who treated `kelson-server` as their spec repository loses something real;
  the answer is that their spec repository should be a repository. Promotion's byte-splice becomes a
  CR patch, which is simpler but no longer preserves the surrounding document's formatting because
  there is no longer a surrounding document to preserve.
- **kelson now has an install step it did not have.** A cluster must have kelson's CRDs registered
  before the server or controller is useful, which is one more thing that can be half-done, one more
  RBAC surface (cluster-scoped `customresourcedefinitions` on install), and a direct amendment to how
  ADR-0013 read ADR-0003. Uninstall must now delete CRDs, which deletes every CR with them — a much
  sharper edge than deleting ConfigMaps, and one the uninstall path has to say out loud.
- **Two validation spellings exist**, and although both are generated from one table, a reader
  debugging a refusal has to know which one refused them. The CEL message is terse where `validate.go`
  is remediation-rich, so the fast refusal is the less helpful one.
- **`api/kelson/v1alpha1` is a public package and therefore a compatibility surface.** `v1alpha1` says
  what it says, but the moment someone imports it, the split between it and `internal/model` becomes a
  boundary this project maintains rather than an implementation detail.
- **A third binary.** `kelson`, `kelson-server`, `kelson-mcp` become four with `kelson-controller`, each
  with its own RBAC, its own image and its own place in the chart.
- **Deferring the webhook means a document can be accepted and then refused.** `kubectl apply` succeeds,
  and the refusal appears in `status.validationErrors` some seconds later. For anyone applying YAML
  directly rather than through kelson, that is a worse experience than a webhook would give, and it is
  the price of not shipping cert wiring.

## Revisit when

- **A webhook becomes cheap enough**, or the CEL subset proves too small to catch what authors actually
  get wrong. The tracked issue owns that decision; this one only defers it.
- **The agent and audit records outgrow a ConfigMap ring.** Decision 7 relocates them without deciding
  their shape. The ADR that makes them CRDs must argue what a typed API buys a fixed-size buffer.
- **`v1alpha1` needs to become `v1beta1`.** Conversion webhooks are the same cert-wiring problem
  deferred in decision 5, and the first stored-version change is when that bill arrives.
- **Someone wants kelson's CRDs without kelson's controller.** The public `api/` package makes that
  physically possible; whether it is supported is a question this ADR does not answer.

## Supersedes

§1 of [ADR-0013](0013-server-state-and-api-v0.md) in full — the ConfigMap spec store, the ConfigMap
history store, the `History` interface seam and the "not a CRD" rationale. ADR-0013 §2 (the v0 API
surface and the one wire error shape), §3 (the trust model and the shared-password amendment) and §4
(codegen convention) are untouched and remain the record.
