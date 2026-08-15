# The kelson model — Project, Component, Environment

This is the authoritative reference for the authoring-plane schemas ([ADR-0006](adr/0006-project-application-environment.md),
as amended by [ADR-0014](adr/0014-components.md)). It resolves the open questions ADR-0006 and issue #24
called out, before implementation. The Go types in `internal/model` and the generated JSON Schemas in
`schema/` are derived from this document; when they disagree, this document is wrong until an ADR says
otherwise.

**An ADR now says otherwise.** [ADR-0027](adr/0027-crd-native-control-plane.md) makes `Project` and
`Environment` custom resources, [ADR-0028](adr/0028-delivery-spine.md) deletes delivery modes and the
`delivery:` block with them, and [ADR-0031](adr/0031-single-cluster-single-tenant.md) deletes
`Environment.spec.cluster`. This page describes the model those ADRs decided. The code still carries the
deleted vocabulary until R1/R2 land ([#224](https://github.com/dafrie/kelson/issues/224),
[#225](https://github.com/dafrie/kelson/issues/225)); every place that matters carries a **Transition**
note saying so. The two documents themselves are unchanged in shape — the same `apiVersion`, the same
`kind`, the same `spec` — and are now applied to a cluster as well as read from a file.

**The leaf is a Component.** ADR-0006 called it an Application and put managed data services in a second
list beside it; ADR-0014 unified the two into one `spec.components` list with a closed set of kinds —
`service`, `worker`, `cron`, `agent`, `postgres`, `valkey`, and `helm` since
[ADR-0016](adr/0016-delivery-flows-v0.md). Where this page says *component*, ADR-0006 says
*application*, and the shape it decided is otherwise unchanged.

Target shape: one HA, TLS-terminated, database-backed service with a worker and a cron job in about
thirty lines, with no duplication. Everything past that target is progressive disclosure — reachable,
not present.

## What this document describes, and what kelson implements today

This is the *designed* model. Several fields below are designed, typed and validated but not yet
consumed by anything downstream. kelson **rejects** those fields with `schema/not-implemented` rather
than accepting them and rendering nothing — a spec that silently does half of what it says is worse
than one that fails ([#141](https://github.com/dafrie/kelson/issues/141)). Each error names the
milestone that will implement the field.

| Field | Rejected until |
|---|---|
| `Project.spec.components[].tools` (`kind: agent`) | M7 · Agent surface & MCP ([#75](https://github.com/dafrie/kelson/issues/75)) |
| `Project.spec.defaults.policy.deployers`, `Environment.spec.policy.deployers` | tenancy ([#231](https://github.com/dafrie/kelson/issues/231)) |
| `Project.spec.components[].release` | the Flux-native `dependsOn` split ([#227](https://github.com/dafrie/kelson/issues/227), [ADR-0028](adr/0028-delivery-spine.md) decision 8) |

The rest of `policy:` is enforced as of [ADR-0025](adr/0025-agent-policy.md) — `deployers` stays gated
because it is about human subjects, which kelson does not model yet
([ADR-0031](adr/0031-single-cluster-single-tenant.md) decision 2).

**`Environment.spec.cluster` is deleted, not gated.**
[ADR-0031](adr/0031-single-cluster-single-tenant.md) decision 1: a gate is right for a field whose shape
is known and whose implementation is pending, and multi-cluster placement has several plausible shapes
(a cluster reference, a placement policy, a selector over a fleet, a per-target kubeconfig). Keeping one
guessed spelling prejudges the design and delivers nothing. It was already refused at validation, so no
document that works today stops working. Multi-cluster is
[#232](https://github.com/dafrie/kelson/issues/232).

> **Transition ([#224](https://github.com/dafrie/kelson/issues/224)).** The field and its
> `notimplemented.go` row are still in `internal/model` until the removal PR
> ([#234](https://github.com/dafrie/kelson/issues/234) carries the spec-vocabulary deletions as one
> behaviour change). Writing it is a `schema/not-implemented` refusal today and an unknown field after.

The gate lives in validation only: `internal/model/notimplemented.go` holds the table, and
`internal/model/coverage_test.go` fails the build if a new spec field is neither consumed nor gated.
Resolution of these fields already works, so a milestone lands by deleting a table row — the data-service
fields and the `from:` bindings left the table exactly that way with
[#89](https://github.com/dafrie/kelson/issues/89). A row may also *narrow* before it disappears:
`secrets:` was gated whole until [ADR-0018](adr/0018-secret-references.md) narrowed it to `store`
alone, and [ADR-0020](adr/0020-external-secrets.md) removed that last row when `store` and
`refreshInterval` became an ExternalSecret's `secretStoreRef` and `spec.refreshInterval`
([#80](https://github.com/dafrie/kelson/issues/80)).

Not every refusal is a gate. A field can be consumed and still have values kelson will not render:
`preset: branch`, and a preset the target cluster's operator cannot host, are structured *render*
errors, because the check needs a ClusterProfile and validation deliberately has none. See
[docs/data-services.md](data-services.md). `backend: externalSecrets` refuses the same way —
`render/external-secrets-not-installed`, `render/external-secrets-store-not-found` and
`render/external-secrets-store-ambiguous` are all decided from the ClusterProfile.

**No field's availability depends on a delivery mode any more.** There is one spine
([ADR-0028](adr/0028-delivery-spine.md)), so `render/helm-requires-flux`,
`render/previews-require-flux` and `render/sops-requires-flux` are vacuous and deleted with the mode
plumbing that fed them, and `render/release-requires-direct` becomes a gate-table row
([above](#what-this-document-describes-and-what-kelson-implements-today)) because the mode it required
is the one that no longer exists. ADR-0016's *"an author can write a valid document that becomes
invalid by changing `delivery.mode`"* is no longer a property this model has.

> **Transition ([#224](https://github.com/dafrie/kelson/issues/224) /
> [#234](https://github.com/dafrie/kelson/issues/234)).** The three vacuous codes and the mode plumbing
> are still in `internal/renderer` and `internal/model`. Their removal is one behaviour-change PR with
> the label rename, so a document written today may still meet them.

And not every refusal is either: a field that belongs to another kind is a plain validation error, because
one list means one type carrying fields only some of its kinds use. `preset` on a worker, `port` on a
`kind: postgres`, `chart` on anything but a `kind: helm`, `tools` on anything but an agent, and a written
`kind:` that contradicts the component's shape are all `schema/mutually-exclusive` rather than fields
that quietly resolve into nothing.

## Documents

The model is two YAML (or JSON) documents. There is deliberately no third kind.

```yaml
apiVersion: kelson.dev/v1alpha1
kind: Project                    # shared configuration + components
---
apiVersion: kelson.dev/v1alpha1
kind: Environment                # where components run, and what differs there
```

A **Project** names its Components inline (`spec.components`). An **Environment** binds itself to a
Project (`spec.project`) and carries target, routing, secrets, policy and per-Component overrides.
Projects stay environment-agnostic (a Project document never mentions an environment) and Environments
stay project-agnostic in shape (nothing in the Environment schema depends on which project it binds to).
Environments are scoped to a Project by reference; they are not owned objects embedded in the Project.

There is exactly one Component spec format and exactly one list. Project-level "shared configuration" is
not a second spec format: it is a small set of fields on the Project (`source`, `build`, `image`, `env`)
that act as defaults merged into each workload Component by rule P1 below. A Component written inline in a
Project and one authored field-by-field against the JSON Schema are the same document.

[ADR-0033](adr/0033-git-connections.md) adds a third kind, `GitConnection`. It does not join the two
above: it carries forge identifiers and a Secret reference rather than a workload, is never named by a
Project or Environment, and the renderer never reads one — it is read only by the planes that already
have cluster access (the server, the controller, the build plane). See
[its reference](reference/gitconnection.md).

## Resolutions of ADR-0006's open questions

**1. Precedence when Project and Environment both set a value.**
See the precedence rules below. Short version: the innermost scope wins; Environment overrides
Component overrides Project. For Environment-scoped concerns (policy, secrets), an explicit
Environment value always wins over a Project default; values never merge across the Project/Environment
boundary — the winner is taken whole.

**2. Project-level shared configuration without a second spec format.**
Shared fields live on the Project as *defaults for every workload Component*: `env` merges key-by-key,
`source`/`build`/`image` supply the artifact a Component omits. Data components are Project-scoped for the
same reason they always were — bindings cross components, and web and worker use the same database — they
are simply entries in the same list now. That is the whole mechanism.

**3. May a Component belong to more than one Project?**
No. A Component is exactly one entry in exactly one Project and is identified by the pair
`(project name, component name)`. Sharing behaviour across Projects is expressed by duplicating the
Component into both Projects or by extracting a library chart via `overlays`, never by membership in
two Projects.

**4. Component-type differences.**
The kind is *derived* from the shape wherever the shape can say it, and written where it cannot:

| Spec shape                    | Kind       |
|-------------------------------|------------|
| `port:` set                   | `service` — Deployment + Service + routing |
| `schedule:` set               | `cron` — CronJob |
| neither                       | `worker` — Deployment, no routing |
| `kind: agent`                 | worker-shaped, with its own identity and (later) tool policy |
| `kind: postgres`, `kind: valkey` | a managed data service; nothing to derive |
| `kind: helm`                  | a third-party chart delegated to helm-controller; nothing to derive |

`schedule:` and `port:` are mutually exclusive (validation error `schema/mutually-exclusive`), as are
`schedule:` with `domains:`/`health:`. A cron job that also serves traffic is two Components. The rule
keeps the happy path free of vocabulary; every kind is reachable, none needs to be named.

Writing `kind:` is allowed for every kind and required for the data kinds and for `helm`. When written it
is checked against the enum *and* against the shape — `kind: service` needs a port, `kind: cron` needs a
schedule, `kind: worker` and `kind: agent` need neither — so an explicit kind states what a component is
and never silently overrules the fields that say otherwise ([ADR-0014](adr/0014-components.md)).

**5. Hiding the Project when there is only one Component.**
Presentation, not schema. The schema keeps the Project level always (uniformity beats special cases for
agents and for the API). The CLI and UI may synthesize or hide it: `kelson` accepts a bare Component
file and wraps it in a Project of the same name, and the UI shows Projects with one Component without
the grouping chrome.

**6. What is versioned.**
The **Project document** is the versioned unit: one file, one commit, optimistic-concurrency version
([ADR-0001](adr/0001-hybrid-state-model.md)). Components still **deploy independently** from any
version — rollback of `web` does not touch `worker`. Each rendered workload carries its own spec-hash,
so an unchanged Component produces an unchanged artifact and no rollout. Coordinated multi-Component
rollback is explicitly out of scope (ADR-0006 consequence) and would be a separately designed operation.

**7. How service bindings and secret references are expressed.**
See *Data components and bindings* and *Secrets* below. Both are data — `{from: {service, key}}` for a
binding, `{secret: <name>, key: <key>}` for a reference to a Secret kelson does not manage — and both are
enumerable through the JSON Schema, so the renderer turns them into the same `secretKeyRef` and agents
reason about them without parsing prose.

**8. Score as an input format (#31).**
Deferred, and not part of these schemas. Interop stays a later translator that emits this model.

## Precedence rules

Numbered P1–P6, in force everywhere (renderer, `kelson render`, the API) and covered by
`internal/model` tests and renderer golden tests (#26, #25 acceptance).

P1, P2 and P3 apply to workload components; P5 applies to data components. They live in one spec list and
are told apart by kind, but no rule reaches both halves.

**P1 — Environment variables:** merge at key level, innermost scope wins:

```
Project.spec.env
  < Component.env
    < Environment.spec.components[].env   (matched by component name)
```

A key set at an inner scope replaces the outer value wholesale. There are no delete markers; to unset a
Project-level variable for one Environment, override it to an empty string.

**P2 — Per-Component scaling and resources:**
`Environment.spec.components[].replicas/resources` replace the Component's values wholesale
(no deep merge of `requests` vs `limits`). Absent override → the Component's values; absent there →
`replicas: {min: 1}` fixed and no resource requests/limits.

**P3 — Image and command:** the innermost scope that names an image wins:

```
Project.spec.image
  < Component.image
    < Environment.spec.components[].image   (the per-environment pin, matched by component name)
```

A built artifact (Project `source` + `build`) supplies the image when none of the three sets one;
`build.strategy: none` with no image anywhere is a validation error (`semantic/no-image-source`).
Component `command:` always wins; Project has no command, and an Environment override does not set one.

Until a build produces that artifact, such a component resolves to an *unresolved* image, and
rendering it fails with the structured render error `image/unresolved` naming each component —
kelson never emits a placeholder image into a manifest. Supply the built reference with `--image`
(`kelson render`, `diff`, `deploy`, `status`, `rollback`), which stands in for Project `image:` and so
loses to a Component `image:` **and to an Environment pin**.

The pin is the promotion primitive ([ADR-0016](adr/0016-delivery-flows-v0.md)); see
[Promotion](#promotion) below. It satisfies the build machinery exactly the way `--image` does: a
component built from source whose Environment pins an image is resolved, not unresolved. All three
image fields are held to the same reference check — a blank or whitespace-bearing reference is
`schema/invalid-format` wherever it is written; tag and digest grammar is the registry's to judge.
An image on an override whose target is a data component is `schema/mutually-exclusive`: what a
`kind: postgres` runs is its operator's business (ADR-0005).

**P4 — Environment-scoped concerns (policy, secrets):** an explicit Environment value always
wins over the Project `defaults` value; otherwise the Project default; otherwise the built-in default:

| Field | Built-in default |
|---|---|
| `policy.agents`  | `allow` |
| `policy.require` | none |
| `secrets.backend`| `cluster` |

These values never merge across the boundary: there is no "strictest of both" arithmetic. If production
must stay propose-only, that is written on the production Environment — every guardrail under
`policy:` is opt-in and therefore visible in the spec (ADR-0025).

> **Transition ([#224](https://github.com/dafrie/kelson/issues/224) /
> [#234](https://github.com/dafrie/kelson/issues/234)).** `delivery.mode` (defaulting to `direct`) and
> `delivery.git` are still in the Go types and the JSON Schema. [ADR-0028](adr/0028-delivery-spine.md)
> decision 9 deletes the whole block — `Delivery`, `DeliveryMode`, `GitTarget` and
> `semantic/git-target-missing` with it. Nothing in the target model reads them.

**P5 — Data-component presets:** `Environment.spec.components[].preset` (matched by component name) replaces
the Project component's preset for that Environment — `shared` in development, `ha-small` in production,
from one Project spec ([ADR-0007](adr/0007-data-services.md)). An override block carries the fields its
target's kind uses and nothing else: `preset` for a data component, `image`/`replicas`/`resources`/`env`
for a workload. Crossing that line is a validation error, not a silent no-op.

**P6 — Overlays:** concatenate, Project first, then Environment. Each patch applies in order to the
resources rendered so far; manifests are emitted as extra resources in order. Overlays are the escape
hatch of record: any long-tail requirement not in the schema goes here ([docs/architecture.md](architecture.md)).

**Domain defaulting (not precedence, but resolved here):** Component `domains:` are explicit FQDNs and
win. If a Component has `port:` but no `domains:`, and the Environment sets `routing.domainSuffix`,
the default hostname is `<component>.<domainSuffix>` — e.g. `web.staging.acme.run`. Environments should
carry distinct suffixes so defaults never collide.

## Promotion

**Promoting in kelson v0 is editing one field.** `Environment.spec.components[].image` pins a component
to one image reference in that environment only, and it is the innermost scope of rule P3. Promoting
staging to production is: read the digest staging deployed, write it as production's pin, deploy.

```yaml
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata:
  name: production
spec:
  project: checkout
  components:
    - name: web
      image: ghcr.io/acme/checkout@sha256:9f6ad2c1…   # ← the whole promotion
```

The diff shows exactly that: one image line per promoted component, and nothing else, because nothing
else changed. A spec change bumps the `Environment`'s generation, so the deploy that follows is an
ordinary reconcile with an ordinary artifact, and rollback is the ordinary rollback — a promotion is a
spec edit, so every mechanism that already handles spec edits handles it
([ADR-0016](adr/0016-delivery-flows-v0.md) decision 2, carried intact by
[ADR-0028](adr/0028-delivery-spine.md) decision 6).

Four consequences worth stating before they surprise anyone:

- **The pin lives in the document you own**, not in a release record — so the repository holding your
  Project and Environment documents reproduces what runs. That is the reason it is a spec field.
- **A pinned environment stops moving.** `--image` stands in for Project `image:` and therefore loses
  to a pin: a CI job passing a fresh digest will not change a pinned environment. Unpinning is deleting
  the field. This is what "pinned" means, and it is the point — production changes when someone
  promotes to it.
- **The promotion stamps where it came from.** The patched `Environment` carries
  `kelson.dev/promoted-from: <source-environment>@<revision>`. This is a deliberate walk-back of
  ADR-0016's *"promotion keeps no record of its own"*, and a small one: an annotation has no lifecycle,
  gates nothing and approves nothing. It turns "where did this image come from" from archaeology into a
  `kubectl get` ([ADR-0028](adr/0028-delivery-spine.md) decision 6).
- **Promotion gates nothing.** There is no approval step, no ordering between environments, no
  "production may only receive what staging ran", and no automatic promotion. The gate is wherever spec
  edits are already gated: review of the document in your repository, and — for agents — `spec.policy`
  ([ADR-0025](adr/0025-agent-policy.md)), which can refuse a promotion into an environment outright.

### The porcelain

Writing the pin by hand stays supported and is still the whole mechanism. `kelson promote` and the
`Promote` RPC are the affordance over it, and they add no concept — they read, they write, they show
the diff, and they stop before deploying.

```sh
kelson promote -f project.yaml -f staging.yaml -f production.yaml --from staging --to production
```

It prints what would be pinned and the rendered diff of the target environment, asks for confirmation
(`--yes` skips the question, never the preview), writes the pins into the Environment document on disk
and prints the follow-up: `kelson deploy … --env production`. `--dry-run` stops after the preview and
`--component web` restricts the promotion to one component (repeatable). Editing a *file* stays
byte-faithful — the document comes back with one image line changed per component and comments, blank
lines and key order untouched — because that is a text edit on your disk, not a store round-trip.

**The digest comes from the deployed revision, not from the source environment's spec.** The images are
read out of the manifests the source environment's *latest deployed revision* recorded — the deployed
truth. Promoting the spec would move production to an image staging has not proven. Three consequences
follow: promoting from an environment with nothing deployed is refused (`promote/nothing-deployed`), a
component whose image the recorded revision does not carry is **skipped with a reason** and never
guessed, and a component already pinned to what the source runs is reported as a no-op rather than
rewritten. Under [ADR-0028](adr/0028-delivery-spine.md) that revision is a tag in the registry, mirrored
into `Environment.status.history[]`, so the read is a status read and the artifact behind it is
immutable.

The server-side equivalent is `DeployService.Promote` — same decision, `Environment.status` on one side
and a **patch to the target `Environment`** on the other, with `dry_run` and `idempotency_key` from the
standard ladder and optimistic concurrency on the resource's `version` (`resourceVersion`). It returns
the pins it wrote, the source revision they came from, and the resulting diff with the same
`exit_semantics` `Diff` reports, so a caller needs no second call to find out what the promotion
changes. Agents reach the same operation through the `promote_component` MCP tool
([docs/mcp.md](mcp.md)), which previews by default.

**The byte-splice is gone, and so is the reason it existed.** ADR-0016 built the server-side promotion
as a splice into a stored document because [ADR-0013](adr/0013-server-state-and-api-v0.md) §1 promised
the store returned your bytes verbatim. [ADR-0027](adr/0027-crd-native-control-plane.md) decision 6 ends
that promise: a custom resource is a decoded, re-serialized object, so there is no surrounding document
to preserve and the write is a patch to a field. That is simpler, and it is a real loss for anyone who
treated the server as their spec repository — the answer being that a spec repository should be a
repository, where byte fidelity is git's job and always was. `promote/document-unwritable` (a flow-style
`components:` list the pin cannot be spliced into) survives only on the **file** path, where there is
still a document whose formatting kelson refuses to rewrite.

The UI's promote screen (`/projects/<project>/<environment>/promote`) drives the same RPC: the
environment in the path is the target, the plan and its diff are shown before anything is
written, and a successful promotion leads to the deploy flow rather than deploying itself.

## Identity: one ServiceAccount per component

Every workload component renders its own ServiceAccount, named after the component, and its pod template
references it ([ADR-0014](adr/0014-components.md) decision D). It is metadata-only today; it exists so
policy has an attachment point that is already in place when there is policy to attach — the per-component
identity that [kagent](https://kagent.dev) calls the most important blast-radius control for agents, and
that costs nothing to apply uniformly.

Data components render none: CloudNativePG creates and owns the identity its clusters run under, which is
what delegating topology to an operator means ([ADR-0005](adr/0005-delegate-to-operators.md)).

The rendered identity labels carry the same vocabulary as the spec: pods carry `kelson.dev/component`
and Deployments select on it. ADR-0014 originally held that label at the old spelling because a
Deployment's selector is immutable and renaming it would orphan every running workload;
[ADR-0032](adr/0032-finish-the-component-rename.md) finished the rename while nothing was deployed
that could be orphaned. The consequence is real and has no migration path: a workload deployed before
that change cannot be updated in place afterwards, and must be deleted and redeployed.

## Data components and bindings

> Implemented for `kind: postgres` since [#89](https://github.com/dafrie/kelson/issues/89) and for
> `kind: valkey` since [#98](https://github.com/dafrie/kelson/issues/98). What each preset renders,
> the sizing defaults and the capability rules are in [docs/data-services.md](data-services.md).
> Still refused, loudly and by the *renderer* rather than by validation: `preset: branch`
> ([#99](https://github.com/dafrie/kelson/issues/99)), `preset: shared`
> ([#93](https://github.com/dafrie/kelson/issues/93)), and any preset the target cluster's operator
> cannot host.

```yaml
spec:
  components:
    - name: db
      kind: postgres           # postgres | valkey — always explicit, never derived
      preset: ha-small         # shared | small | ha-small | ha-medium | branch
    - name: cache
      kind: valkey
      preset: small
      auth: { secret: cache-auth, key: password }   # kind: valkey only
  env:
    DATABASE_URL:
      from: { service: db, key: uri }
    CACHE_PASSWORD:
      from: { service: cache, key: password }
```

The binding key stays `service:` after the rename: what it names is the service a data component provides,
and every other kind is unbindable. Binding to a workload is `ref/unknown-service` with the bindable names
in the remediation.

A binding is **never** a secret value in the spec. A *credential* lives in the Secret the component's
operator generates — for a dedicated postgres preset that is CloudNativePG's `<cluster>-app`, where
`<cluster>` is `<project>-<environment>-<component>` — and the renderer emits a `secretKeyRef` against
it. kelson's key names are the spec's contract and are mapped onto the operator's own (`database` is
CNPG's `dbname`). A key that is *not* a credential — a cache's host, port and URI — renders as a plain
value, because minting a Secret to hold a Service name would obey the letter of
[ADR-0009](adr/0009-secrets.md) and make the manifest harder to read.

**`auth:` is the third source of a credential, and the only one an author names.** A `kind: valkey`
component takes `auth: {secret, key}` — the same `{secret, key}` shape as an env reference, because it
is the same promise — and kelson writes that name into two places: the operator's ACL user, and the
`secretKeyRef` its `password` binding resolves to. Without it a cache has no `password` key to bind
and no password at all, which the refusal says along with the two commands that fix it. `auth:` is
refused on every other kind (`schema/mutually-exclusive`): postgres gets its credential from the
operator that generates it, and a workload or a chart uses an env reference, which is the same two
fields in the place they take effect. A cache's `uri` never carries the password at any setting —
[docs/data-services.md](data-services.md) has the two-command flow and the reasoning.

Well-known keys per data kind:

| Kind | Keys |
|---|---|
| `postgres` | `uri`, `host`, `port`, `database`, `username`, `password` |
| `valkey`   | `uri`, `host`, `port`, `password` |

Unknown component names and keys are validation errors (`ref/unknown-service`, `ref/unknown-service-key`)
with the list of valid keys in the remediation. Both the renderer and agents therefore reason about
bindings from the schema alone.

A binding to a `shared`-preset component is a render error, not a `secretKeyRef`: the shared cluster's
credentials live in the shared cluster's namespace and a pod cannot reference a Secret across one. The
database is still created; distributing its credentials is
[#93](https://github.com/dafrie/kelson/issues/93). See [docs/data-services.md](data-services.md).

**A binding and a secret reference are the same mechanism.** Both are mappings in an env value, both
render into `valueFrom.secretKeyRef` through the same code, and neither can carry a value. They differ
only in who names the Secret: a binding names a *component* and kelson derives the Secret its operator
generates, while `{secret: <name>, key: <key>}` names the Secret directly, for every credential that is
not a data service kelson manages. See [Secrets](#secrets-references-never-literals) and
[ADR-0018](adr/0018-secret-references.md).

## Release commands: migrations, before the rollout

> **Gated — the field validates and renders nothing** ([ADR-0028](adr/0028-delivery-spine.md)
> decision 8). [ADR-0019](adr/0019-release-command-hook.md)'s guarantee was *"the Job finished before
> the Deployments changed"*, and its own rationale said only *the mode where kelson performs the apply
> itself can stop between two resources*. That mode is gone, so `release:` moves into the gate table:
> refused by name, with the tracked work in the message
> ([#227](https://github.com/dafrie/kelson/issues/227)), rather than silently dropping a migration.
>
> The replacement is known and not built: two `Kustomization`s with `dependsOn`, the first holding the
> release Job with a health check, the second the workloads. It is possible now precisely *because*
> kelson owns the Kustomization ([ADR-0028](adr/0028-delivery-spine.md) decision 3) — ADR-0019's own
> "Revisit when" predicted this exact resolution. The shape below is what #227 has to honour, and is
> kept for that reason.
>
> **Transition ([#224](https://github.com/dafrie/kelson/issues/224)).** Until R1 lands, the direct
> adapter still implements the barrier as described here, in the mode that is being deleted.

A `release:` command runs to completion, and successfully, **before the revision's workloads roll**.
It is where database migrations go.

```yaml
spec:
  components:
    - name: db
      kind: postgres
      preset: ha-small
    - name: web
      port: 8080
      release:
        command: ["./manage.py", "migrate", "--noinput"]   # required
        timeout: 30m                                        # optional, default 10m
  env:
    DATABASE_URL: { from: { service: db, key: uri } }
```

**It belongs to a component, not to the Project.** Everything the command needs is a component's: the
image it runs, the env it reads, the bindings it resolves and the ServiceAccount it runs under. A
Project-level hook would have to pick one component's image and then pretend it had not. Put it on the
component whose rollout must wait for it — usually the service that talks to the database. It is
refused on `kind: cron` (a cron component already *is* a command on a schedule), and on data
components and charts for the reason every workload field is: what they run is their operator's
business.

### What is rendered, and in which order

One `ServiceAccount` (the component's own, moved here from its workload group because the Job's pod
names it) and one `Job`, placed **after the data services and the charts, and before every workload**.
The Job carries the component's image, the component's whole environment — bindings and secret
references included, so the migration reads exactly the `DATABASE_URL` the application reads —
`restartPolicy: Never`, `backoffLimit: 2` and the `activeDeadlineSeconds` your `timeout` resolves to.

Order in a rendered set is not a wait: applying a Job before a Deployment says nothing about the Job
having finished. **The waiting is delivery's**, and that is the whole reason the field is gated: the
one path that could stop between two resources was the mode kelson applied in itself. The Flux-native
answer puts the wait between two `Kustomization`s instead — see
[docs/delivery.md](delivery.md#release-commands-and-the-barrier-that-is-not-built-yet).

The small `backoffLimit` is deliberate and is not a retry policy for broken migrations. It exists so a
*first* deploy — where the database was created seconds earlier and is not yet accepting connections —
does not fail for a reason that has nothing to do with the migration. kelson does not wait for a data
service to be ready before starting the release command; the two pod retries (10s, then 20s) are what
absorbs that window today.

### Failure, and what stays running

A failed release command **fails the deploy before any workload of the new revision is applied**. The
previous revision keeps serving, nothing is recorded in the history, and nothing is pruned. The error
is a structured `delivery/release-failed` naming the Job, and it carries the tail of the command's own
output as its cause, because the sentence you need is in there rather than in anything kelson could
write:

```
release-web-5be0c165 [delivery/release-failed] the release command failed: BackoffLimitExceeded;
the workloads of this revision were not applied, so the previous revision is still running
(cause: ERROR: relation "orders" does not exist)
```

While it runs, the deployment reads as `Reconciling` with the Job named; a failure is `Rejected` —
"processed, refused, not live", which is exactly what happened. A migration does not get a phase of
its own, and [ADR-0019](adr/0019-release-command-hook.md) records why.

### Running twice, and not running twice

The Job's name is `release-<component>-<first 8 of the spec hash>`, so **the name is the idempotency
key**:

- re-deploying an **unchanged** revision finds the Job it already ran and, if it completed, does not
  run it again. A completed release command is a fact about a revision, not about a deploy attempt.
- a **changed** revision — a new image, a new release command, a new env value — is a different spec
  hash and therefore a different Job, so a deploy runs its migrations.
- re-deploying a revision whose Job **failed** deletes it and runs it again. A Job cannot be edited, so
  a verdict that must be re-taken is a delete and a create; without it the first transient failure
  would be permanent short of `kubectl`.

**Write idempotent migrations anyway.** kelson bounds how often a command is re-run; it cannot bound
what the command does when it is. A migration that is not safe to re-run will eventually be re-run — by
a retry, by an interrupted deploy, by a redeploy after an unrelated fix — and every migration framework
worth using already tracks what it has applied.

### Rollback does not undo a migration

Rolling the workload back re-applies the previous revision's manifests and **deliberately does not
re-run its release command**. A schema change is not in the rendered output and kelson has no
down-migration to run, so there is nothing to roll back to: the rolled-back workloads meet the newer
schema. Plan for that — keep migrations backwards-compatible with the revision you might roll back to
([#55](https://github.com/dafrie/kelson/issues/55)).

### Promotion moves the image, never the database

Promoting is editing one field — the target Environment's per-component image pin
([above](#promotion)) — and deploying that environment. The release Job is then rendered **for the
target environment**: its namespace, its bindings, its secret references. Production's migration runs
against production's database, staging's against staging's, and a preview's against the preview's own,
because a binding resolves to the Secret *that environment's* operator generated
(`checkout-production-db-app` and `checkout-staging-db-app` are different Secrets in different
namespaces). Nothing about a promotion carries the source environment's data, credentials or schema
state across; what is promoted is an image reference.

The consequence worth stating: promoting a revision whose migration already ran in staging **runs it
again in production**, against production's database, because it is a different database. That is what
you want, and it is another reason the migration must be idempotent.

### What the gate costs, stated plainly

Anyone whose deploy runs migrations loses the ordering guarantee ADR-0019 built, and gets an error
message instead of a silent omission — honest, and still a regression
([ADR-0028](adr/0028-delivery-spine.md) consequences). Until
[#227](https://github.com/dafrie/kelson/issues/227) lands, run migrations the way you would without
kelson: a `Job` through `spec.overlays`, or a command against the database out of band. An overlay
carries the same *ordering* limitation — a rendered set is ordered, and order is not a wait — so
whichever you choose, keep the migration backwards-compatible with the revision still serving.

## Helm components: a chart, delegated

> Implemented since [ADR-0016](adr/0016-delivery-flows-v0.md) decision 4. It needs **helm-controller
> in the cluster**, which is a capability finding rather than a mode choice
> ([below](#what-it-needs-in-the-cluster)).

Some dependencies ship as a chart and nothing else. `kind: helm` runs one beside your components:
kelson renders a `HelmRelease` and the source it fetches from, and helm-controller installs and
upgrades it. It is the same delegation rule every managed data type gets
([ADR-0005](adr/0005-delegate-to-operators.md)) — **kelson writes a CR, kelson does not template an
engine**. There is no Helm library in the codebase and there is not going to be one.

```yaml
spec:
  components:
    - name: ingress
      kind: helm
      chart: ingress-nginx           # the chart's name inside its source
      chartVersion: 4.11.3           # required — see "Pinning" below
      source:
        repository: https://kubernetes.github.io/ingress-nginx   # or: oci: oci://ghcr.io/acme/charts
      values:                        # plain configuration, verbatim into the HelmRelease
        controller:
          replicaCount: 2
      valuesFrom:                    # where secret material goes
        - secretRef: ingress-tls-values
```

| Field | Meaning |
|---|---|
| `chart` | the chart's name within its source |
| `chartVersion` | the exact version. Required |
| `source.repository` | a classic Helm repository URL (the one serving `index.yaml`) → renders a `HelmRepository` |
| `source.oci` | an OCI registry URL *without* the chart name → renders an `OCIRepository` |
| `values` | chart values, written verbatim into `HelmRelease.spec.values` |
| `valuesFrom` | `secretRef`/`configMapRef` names helm-controller merges in before `values` |

Exactly one of `source.repository` and `source.oci` is set; both is `schema/mutually-exclusive` and
neither is `schema/missing-required`, because the two render different Flux source kinds and kelson will
not pick for you. Every workload field — `image`, `command`, `port`, `health`, `schedule`, `domains`,
`replicas`, `resources`, `env` — and `preset` are `schema/mutually-exclusive` on a helm component, and
the chart fields are the same error on every other kind. A helm component takes **no per-environment
override**: the chart version and its values live on the Project component, and an environment that needs
different values needs its own component.

### What kelson owns, and what it does not

kelson's inventory for a helm component is **two resources**: the source and the `HelmRelease`.
Everything the chart expands into — its Deployments, its Services, its CRDs — is created by
helm-controller under Helm's own release ownership. kelson never prunes it, never adopts it, and never
diffs it. `targetNamespace` is the environment's namespace, so the chart's objects land beside your
components.

### The preview downgrade, stated plainly

**A preview of a helm component shows the `HelmRelease` changing — the chart, the version, the values —
and never the workloads the chart produces.** A chart upgrade that rewrites every manifest it ships
appears in the diff as one changed `version:` line. This is worse than what kelson promises everywhere
else, where a diff is the concrete manifests, and ADR-0016 accepts it as a **documented v0 downgrade**
rather than a bug: honest previews of a chart require `helm template` against a fetched chart, which is
network I/O and therefore not something the pure renderer may do
([ADR-0001](adr/0001-hybrid-state-model.md), [#20](https://github.com/dafrie/kelson/issues/20)). The
upgrade path is an advisory *server-side* `helm template`, and it does not change the delegation
decision. Drift *inside* the release belongs to helm-controller's own drift detection, which is the
layer kelson explicitly does not police. See [delivery](delivery.md#preview-of-a-helm-component).

### Pinning is required

`chartVersion` has no default and an empty one is `schema/missing-required`. An unpinned chart resolves
at apply time, which means the same document installs different manifests on different days — and
because the diff only ever shows the `HelmRelease`, it would report *no change at all* while the cluster
changed underneath it. Pinning is what makes the values-only preview survivable.

### `values` is not a secret store

`values` is plain configuration. It is written verbatim into the `HelmRelease`, which is **not** a
Secret and is **not** redacted anywhere: it appears in the rendered output, in every diff, and in the
published artifact in plain text — where anyone who can pull from the registry can read it. Secret
manifests kelson renders are redacted in display surfaces; a `HelmRelease` is not one of them, and
pretending otherwise would be the more dangerous mistake.

So chart credentials go in `valuesFrom`, as a `secretRef` to a Secret somebody else manages in the
environment's namespace. helm-controller reads it at release time and kelson never sees the value
([ADR-0009](adr/0009-secrets.md)). It is the same bargain a workload's
`{secret: <name>, key: <key>}` makes ([ADR-0018](adr/0018-secret-references.md)) — the spec carries a
Secret's name and somebody else does the reading — with the resolution done by helm-controller instead
of the kubelet, because a chart's values are not a container's environment.

**Nothing enforces this beyond saying it.** kelson does *not* inspect the content of a value to guess
whether it is a credential: a key called `password` is accepted, because content-sniffing would block
legitimate chart configuration (charts have `passwordSecretName` keys and `auth.existingSecret` keys and
plenty of harmless `token` fields) and would still miss anything under a name nobody predicted. The rule
is a rule you follow, not a check you pass. The one thing validation does require of `values` is string
keys, which Helm requires anyway.

### What it needs in the cluster

helm-controller, and its source-controller. A `HelmRelease` applied where no helm-controller runs is an
object that is accepted and then does nothing — the silent success
[#141](https://github.com/dafrie/kelson/issues/141) exists to prevent — so its absence is reported as a
**capability finding** (`internal/clusterprofile/helm`, `kelson profile`), never as a rendering decision.
Since [ADR-0028](adr/0028-delivery-spine.md) that is the only question left to ask: every environment
reconciles through Flux, so the mode gate `render/helm-requires-flux` is vacuous and deleted with the
rest ([above](#what-this-document-describes-and-what-kelson-implements-today)). A document that
validates renders, everywhere.

Both controllers are in flux-aio and in a default Flux install, so on the substrate
[ADR-0030](adr/0030-flux-aio-install.md) offers they are already there. A flux-operator `FluxInstance`
naming just those two components remains a supported and common shape
([#60](https://github.com/dafrie/kelson/issues/60)).

## Previews: a child environment per pull request

> Implemented since [ADR-0017](adr/0017-pr-previews.md), which decides the design;
> [ADR-0016](adr/0016-delivery-flows-v0.md) decision 5 decided the shape. It is the **one feature that
> needs flux-operator** ([ADR-0030](adr/0030-flux-aio-install.md) decision 4): `ResourceSet` and
> `ResourceSetInputProvider` are its CRDs. **Two halves have to be in place**: the `previews:` block
> below, and something that publishes the artifacts it points at — either a CI step reporting its
> build to kelson-server, or `kelson preview publish` in CI. Read
> [Publishing the artifacts](#publishing-the-artifacts) before turning this on. Once they are,
> `PreviewService.ListPreviews` and the web UI's previews section say which change requests are
> running ([Seeing your previews](#seeing-your-previews)).

An environment may spawn a child environment per open pull request. kelson does not poll the forge and
does not garbage-collect: a flux-operator `ResourceSetInputProvider` finds the change requests and a
`ResourceSet` creates, updates and deletes one preview per change request. What kelson supplies is the
manifests — rendered **concretely**, per pull request, and published as an OCI artifact the preview's
`Kustomization` applies.

```yaml
spec:
  previews:
    provider: github                                   # github | gitlab
    repo: https://github.com/acme/checkout             # whose pull requests become previews
    secretRef: github-auth                             # optional: bring your own Secret NAME, never a token
    interval: 10m                                      # how often the forge is polled
    filter:
      labels: [deploy/preview]                         # only labelled change requests
      includeBranch: "^feat/.*"                        # Go regular expressions
      excludeBranch: "^wip/.*"
      limit: 10                                        # simultaneous previews; default 10
    skip:
      labels: [deploy/preview-pause, "!ci/passed"]     # pause updates; ! means "while absent"
    artifacts:
      repository: oci://ghcr.io/acme/checkout-previews # no tag — kelson chooses it
      secretRef: ghcr-auth                             # optional pull secret
```

| Field | Meaning |
|---|---|
| `provider` | the forge. `github` → `GitHubPullRequest`, `gitlab` → `GitLabMergeRequest` |
| `repo` | HTTP(S) URL of the repository whose change requests become previews |
| `secretRef` | name of a Secret holding forge credentials, in the environment's namespace. Optional — see below |
| `interval` | forge polling interval; default `10m` |
| `filter.labels` | only change requests carrying one of these labels get a preview |
| `filter.includeBranch` / `filter.excludeBranch` | Go regular expressions against the branch name |
| `filter.limit` | maximum simultaneous previews. Default **10** |
| `skip.labels` | pause *updates* while a label is present; `!label` pauses while it is absent |
| `artifacts.repository` | the `oci://` repository per-pull-request manifests are published to |
| `artifacts.secretRef` | a docker-registry Secret for a private artifact repository |

`repo` is the **source** repository — whose pull requests become previews — and `artifacts.repository`
is where the per-pull-request manifests are pushed. kelson defaults neither from the other, and an SSH
remote in `repo` is `schema/invalid-format`: the forge is reached over its HTTP API.

`secretRef` is optional ([ADR-0033](adr/0033-git-connections.md) decision 4). Leave it unset and kelson
materializes the flux-operator Secret itself — `username`/`password` from a token connection, the
`githubApp*` keys from an app connection — from whichever [git connection](adr/0033-git-connections.md)
covers `repo`, writing it to `<project>-<environment>-previews` in the environment's namespace. The
renderer points the `ResourceSetInputProvider` at that same derived name, so nothing has to store it.
Set `secretRef` to bring your own Secret instead; a named Secret wins untouched, and kelson never reads
or writes it. A `secretRef` is still held to being a Secret *name* — a DNS-1123 label — never a token,
whichever way it got there.

`filter.limit` defaults to 10 rather than flux-operator's own 100. The ceiling is a cost control, and
an environment that quietly stands up a hundred preview namespaces the first time somebody bulk-labels
a backlog is a surprise that arrives as a cluster bill. The value is always written into the rendered
manifest, so what the cluster will enforce is readable without knowing anyone's defaults.

### What kelson renders

Two objects, in the environment's own namespace:

- a **`ResourceSetInputProvider`**, carrying the provider type, the repository URL, the `secretRef` —
  the named one, or `<project>-<environment>-previews` when none was named — the filter, the skip
  labels, and the polling interval as the `fluxcd.controlplane.io/reconcileEvery` annotation;
- a **`ResourceSet`**, whose `resourcesTemplate` instantiates an `OCIRepository` and a `Kustomization`
  per change request — and **nothing else**.

Each preview lands in a namespace of its own, `<project>-<environment>-pr<number>`, which is also the
name of its `OCIRepository` and `Kustomization`. The `Kustomization` sets `prune: true`, `wait: true`
and `targetNamespace`, so teardown on merge or close is one delete and a mis-published artifact cannot
deploy outside the pull request's namespace.

`<project>-<environment>` may be at most **54 characters**, or the render fails with
`render/preview-name-too-long`. Both derived names add nine: `-previews` for the two lifecycle
objects, and `-pr` plus a change request number of up to six digits for the preview. Sixty-three is
the DNS-1123 limit a namespace must satisfy.

The artifact is addressed by the pull request's **head commit SHA**, not by its number. A per-PR tag
would be mutable by construction, which makes "what is running in preview 412" depend on when you ask;
a SHA tag is one artifact per push and is already what CI tags the image with.

### Preview databases are inside the preview

A data component renders per pull request exactly as it renders anywhere else, so a preview's database
is applied by the preview's own `Kustomization` and pruned by it. There is no second thing to remember
to delete, which is the whole reason [ADR-0016](adr/0016-delivery-flows-v0.md) put preview databases
inside the `ResourceSet` lifecycle ([#103](https://github.com/dafrie/kelson/issues/103)). The
storage-capability gate is unchanged: a preset the cluster cannot host fails the preview's render the
same way it fails the parent environment's (see [data services](data-services.md)).

### Publishing the artifacts

The `previews:` block renders the cluster-side machinery and nothing else. Something has to push the
manifests each preview applies, and there are now two things that can. Without either, flux-operator
finds the labelled pull requests, creates an `OCIRepository` for each, and reports that the artifact
does not exist.

**The server publishes, and CI only reports** ([ADR-0034](adr/0034-forge-driven-delivery.md)
decision 3). A project that sets `build.by: ci` hands kelson one sentence per build — this commit,
this change request, these digest-pinned images — and kelson renders the preview from the spec it
already holds, publishes the artifact under the head commit and asks flux-operator to look now. CI
never runs kelson's renderer, never needs a checkout of the spec and never holds the
artifact-registry credential.

> **What exists today.** The server side is complete: `BuildService.ReportBuild` publishes, and
> kelson-server publishes with the credential in `--registry-config`. The *client* side is the RPC
> itself — the `kelson ci report-build` verb ADR-0034 sketches is not written yet
> ([#248](https://github.com/dafrie/kelson/issues/248)), so a pipeline calls the method directly. A
> report for a project whose `build.by` is `kelson` (the default for a project with `source:`) is
> answered `accepted: false` naming the field, because those images come from kelson's own build
> plane; a report with no `--pr` is refused, because the tracking environments it would feed
> (`autoDeploy`, decision 4) are not in the model yet.

**Or CI publishes, with `kelson preview publish`, run in the application repository's CI on pull
request events** — that is where the pull request's checkout and the image built from it already are
([ADR-0017](adr/0017-pr-previews.md) decision 8). ADR-0034 demotes this from the recommended path to
the escape hatch for pipelines that cannot reach a kelson server at all — an air-gapped runner, a
control plane behind a network the runner has no route to — and it is unchanged for those.

```yaml
# .github/workflows/preview.yml
name: preview
on:
  pull_request:
    types: [opened, synchronize, reopened, labeled]

jobs:
  publish:
    # The label that gates the preview in `filter.labels` gates the job that
    # feeds it, so a pull request nobody asked to preview costs nothing.
    if: contains(github.event.pull_request.labels.*.name, 'deploy/preview')
    runs-on: ubuntu-latest
    permissions:
      contents: read
      packages: write
      pull-requests: write        # only for the skip label below
    steps:
      - uses: actions/checkout@v4
        with:
          ref: ${{ github.event.pull_request.head.sha }}

      # `skip.labels` pauses the preview while this label is present, so the
      # cluster does not update to a commit whose image is still building.
      # Add it before the build, remove it after the publish.
      - run: gh pr edit "$PR" --add-label deploy/preview-pause
        env:
          PR: ${{ github.event.pull_request.number }}
          GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}

      - uses: docker/login-action@v3
        with:
          registry: ghcr.io
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}

      # Build first: publish needs the image the preview will actually run.
      # The last line of `kelson build` is the digest-pinned reference.
      - id: build
        run: echo "image=$(kelson build -f spec.yaml --env staging --registry ghcr.io/acme | tail -1)" >> "$GITHUB_OUTPUT"

      - run: |
          kelson preview publish -f spec.yaml \
            --environment staging \
            --pr  ${{ github.event.pull_request.number }} \
            --sha ${{ github.event.pull_request.head.sha }} \
            --image ${{ steps.build.outputs.image }}

      - run: gh pr edit "$PR" --remove-label deploy/preview-pause
        env:
          PR: ${{ github.event.pull_request.number }}
          GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}
```

| Flag | Meaning |
|---|---|
| `-f` | the spec files, repeatable, exactly as `kelson render` takes them |
| `--env` (or `--environment`) | the environment whose `previews:` this belongs to |
| `--pr` | the change request number, as the forge numbers it |
| `--sha` | the head commit **in full** — an abbreviation tags the artifact where nothing looks |
| `--image` | the image the preview runs, standing in for `spec.image` (rule P3) |
| `--registry-secret` | resolve the push credential from a cluster Secret instead of the runner's `docker login` |
| `--profile` | the ClusterProfile to render against, as everywhere else |

The push credential is the runner's `docker login` by default (`$DOCKER_CONFIG/config.json`, then
`~/.docker/config.json`), which is what `docker/login-action` writes. A runner with cluster access and
no login can use `--registry-secret <name>`, resolved from the **parent environment's** namespace —
the preview's namespace does not exist yet, since creating it is what the artifact is for.

`kelson preview render` takes the same flags and prints the manifests instead of pushing them. It is
the rung to reach for when a preview is not what it should be: the artifact's contents are exactly
those bytes, so anything wrong there is wrong in the cluster, and anything right there is a publishing
or a reconciliation problem instead.

The artifact is deterministic — same render, same digest, timestamps included — so re-running a job
for an unchanged commit uploads nothing.

### Seeing your previews

`PreviewService.ListPreviews` answers which change requests are running, and the web UI draws it as a
**Previews** section on each environment of the app detail screen. Both read the cluster; neither
creates or destroys anything, because a preview appears when CI publishes an artifact and disappears
when the change request closes.

Per change request you get its number, the **head commit the `OCIRepository` actually pins** — what is
running, not what should be — the preview's namespace, the hostnames its HTTPRoutes claim, its age, and
one phase:

| Phase | What it means |
|---|---|
| `ready` | the artifact was fetched and applied |
| `applying` | fetched; the apply has not settled |
| `awaiting-artifact` | nothing published for this commit — usually the CI step, not the manifests |
| `failed` | fetched, and the apply failed |
| `unknown` | neither condition has reported yet |

Above the list is the lifecycle pair itself, because an empty list has three very different causes and
they are not interchangeable: flux-operator is not installed, the `ResourceSet` pair has never been
deployed to the cluster, or the poller cannot reach the forge. Each says so in its own words.

Two limits worth knowing. **Hostnames need a read of the preview's own namespace**, which is created at
reconcile time and so cannot be in a namespaced RBAC grant — the deploy chart grants the four flux
reads in the environment's namespace and not that one, so an install with the chart's RBAC sees its
previews without their hostnames. That read fails soft, and "no hostnames" is therefore not a claim
that the preview serves nothing. And **there is no preview history**: see below.

### What previews do not do

**A preview never serves the hostname the spec asks for.** Every hostname gains the change request in
its first DNS label: `web.staging.acme.run` becomes `web-pr412.staging.acme.run`, and an authored
`api.acme.com` becomes `api-pr412.acme.com`. Two previews would otherwise fight over one hostname and
a preview could take production's traffic. Only the first label changes, so the wildcard certificate
and wildcard DNS record that already serve the environment serve its previews too — but a component
configured with its own hostname (an OAuth redirect URI, a cookie domain) is configured with the wrong
one, and kelson does not tell it what its preview hostname is.

**A preview has no history and nothing to roll back to.** A preview is an environment kelson did not
record: no Environment document describes it and no delivery history entry exists for it. `kelson
history` and `kelson rollback` are about environments kelson deployed, and a preview is not one — its
past is the registry's, one artifact per push, and the forge's. `ListPreviews`
([below](#seeing-your-previews)) answers what is running now; nothing answers what ran before.

**There is no TTL.** A preview lives as long as its pull request is open and labelled; a pull request
open for three months holds a database for three months. `filter.limit` is the only cost control
there is. flux-operator has no expiry either, so this is future work with nobody's name on it, not a
setting somebody forgot to expose.

**The `ResourceSet` runs with flux-operator's own permissions.** kelson sets no `serviceAccountName`
on either object, because there is no field for one. On a multi-tenant cluster that is more authority
than a preview should have, and the fix is a spec field plus an RBAC story that ADR-0017 does not
attempt.

**Published artifacts are never deleted, and nobody verifies who published them.** Teardown deletes
the preview's namespace and its two objects; the artifacts stay in the registry, one per push. The
`OCIRepository` fetches whatever carries the head commit's tag, from whoever could write to that
repository — kelson writes no `spec.verify`, so the trust boundary is the registry's write access.

**An artifact repository on a port cannot be authored.** `artifacts.repository` reads any `:` after
`oci://` as a tag, so `oci://registry.internal:5000/acme/previews` is rejected as
`schema/invalid-format`. A registry on a non-default port is therefore unusable for previews today.

### What previews need in the cluster

flux-operator, and it is the only feature that needs it
([ADR-0030](adr/0030-flux-aio-install.md) decision 4): previews *are* its `ResourceSet` lifecycle. On a
cluster running flux-aio and no flux-operator, an environment declaring `previews:` gets the capability
gap reported with an offer to install — the [ADR-0003](adr/0003-install-model.md) pattern, read from
the `ClusterProfile`'s `fluxOperator` finding ([`internal/clusterprofile`](detection.md)), never a
rendering decision. The old mode gate `render/previews-require-flux` is vacuous under one spine and is
deleted with the rest
([above](#what-this-document-describes-and-what-kelson-implements-today)).

Worth knowing before choosing a substrate: flux-operator is AGPL-3.0, so a cluster that never wants
previews never installs one.

## Secrets: references, never literals

> The reference syntax is [ADR-0018](adr/0018-secret-references.md); the doctrine it implements is
> [ADR-0009](adr/0009-secrets.md).

**An environment value is one of exactly three things.** A scalar is a value; a mapping is a reference,
and which reference is decided by its own key:

```yaml
env:
  LOG_LEVEL: info                                    # a plain string — non-secret configuration
  DATABASE_URL: { secret: checkout-db, key: url }    # a secret reference (ADR-0018)
  CACHE_URL:    { from: { service: cache, key: uri } }  # a data-service binding (ADR-0009)
```

There is no fourth form and no templating language — no `${...}`, no interpolation into a larger
string. A variable comes from one place and the spec says which place in a shape the JSON Schema
describes, so an agent generates it without parsing prose. A mapping that is neither form is
`schema/invalid-format` and the remediation lists all three.

### What a reference names

`secret:` is the name of a **Secret in the environment's namespace**, and `key:` is a key within it.
kelson references it and nothing else: it does not create it, read it, diff it or own it. The rendered
manifest carries `valueFrom.secretKeyRef` and the kubelet performs the projection at pod start.

Writing the Secret is out of band, and kelson has its own command for it since
[#116](https://github.com/dafrie/kelson/issues/116):

```bash
kelson secret set checkout-db --project checkout --env production url=postgres://…

# or, keeping the value out of your shell history:
read -rs PW && printf '%s' "$PW" | kelson secret set checkout-db \
  --project checkout --env production --from-stdin password
kelson secret set tls --project checkout --env production --from-file tls.key=./tls.key
```

`kubectl -n <namespace> create secret generic <name> --from-literal=<key>=…` writes the same object
and remains the alternative on a machine that has kubectl and not kelson. Names are DNS-1123 labels;
keys use Kubernetes' own key alphabet (letters, digits, `-`, `_`, `.`) — the same rules the reference
itself is held to, so a Secret kelson will write is always one a spec can name.

Under the default `cluster` backend `kelson secret set` **merges**: keys it is not given are preserved,
so rotating one credential leaves the others alone. (Under `sops` it writes the whole file and refuses
to drop a key silently — see [The `sops` backend](#the-sops-backend) — because carrying the other keys
forward would need a decryption key kelson never holds.) Pass `-f <spec>` and kelson reads
`secrets.backend` to decide where the value goes; without it, it assumes `cluster`.

`kelson secret list --project <p> --env <e>` reports names, keys and ages and never
a value — kelson does not store secret values, the cluster does (ADR-0009), and there is no flag that
would print one. `kelson secret delete` removes a Secret kelson wrote and refuses one it did not: every
Secret kelson writes carries `kelson.dev/managed-secret: "true"`, listing is a label query over it, and
a namespace's TLS material and service-account tokens are neither listed nor deletable through kelson.

A reference is legal wherever an env value is: `Project.spec.env`, a component's `env`, and
`Environment.spec.components[].env`. **Rule P1 is unchanged** — references merge key by key like any
other value, innermost scope wins, and an environment may replace a plain value with a reference or a
reference with a plain value. Nothing in the merge asks what form either side has.

### The guarantee, and its boundary

**Rendered output never contains a secret value, by construction.** Everything typed as a reference
stays a reference from the YAML you write to the bytes the cluster receives; kelson never inlines a
Secret's data into a workload manifest (the renderer has no cluster client and cannot read one); and a
plaintext Secret is not something the renderer can emit at all — there is no code path that writes
`data:` or `stringData:`, so there is no field a value could be placed in.

What that does **not** claim: kelson cannot stop you writing a password as a plain string.
`SESSION_PEPPER: hunter2` renders. A string value is rejected with `secret/literal` when

- it parses as a URL containing a password (`postgres://user:pass@host/db`), or
- the variable name matches a secret pattern (`PASSWORD`, `SECRET`, `TOKEN`, `_KEY`, `PRIVATE`,
  `CREDENTIAL`, `AUTH`) and the value is non-empty,

and that is a heuristic, deliberately erring toward rejection because the fix is cheap in both
directions. The error names the field and leads with the reference form and the `kelson secret set`
that writes the Secret behind it, keeps the `from:` binding as the shorter path for a managed service
([#89](https://github.com/dafrie/kelson/issues/89)), and keeps
`spec.overlays` last. Overlays remain the escape hatch, including for a raw `kind: Secret` — and
`internal/redact` replaces its `data`/`stringData` with `[redacted]` in every diff, preview, API
read-back and log ([#117](https://github.com/dafrie/kelson/issues/117)).

### Choosing the backend

`Environment.spec.secrets.backend` selects the mechanism that puts a value where a reference points,
and the spec text does not change when it changes:

| Backend | What it does | Status |
|---|---|---|
| `cluster` | the reference addresses a Kubernetes Secret written out of band. The built-in default | renders |
| `externalSecrets` | an `ExternalSecret` per referenced Secret, resolved by external-secrets from Vault or a cloud secret manager | renders ([ADR-0020](adr/0020-external-secrets.md)) |
| `sops` | values encrypted with age, shipped **inside the artifact**, decrypted in-cluster by kustomize-controller | renders ([ADR-0022](adr/0022-sops-age.md), transport amended by [ADR-0028](adr/0028-delivery-spine.md) §7) |

An unknown backend is `render/secret-backend-unsupported` — a **render** error rather than a validation
one, decided from spec data alone before anything is emitted, so the same document renders the same way
against every cluster. `sops` needs no gate any more: kustomize-controller decrypts per-Kustomization
and does not care whether the source is a `GitRepository` or an `OCIRepository`, so
`render/sops-requires-flux` is vacuous under one spine and deleted with the rest.

### The `externalSecrets` backend

The spec text does not change. `{secret: payments, key: api-key}` still renders the same
`valueFrom.secretKeyRef` it renders under `cluster`; what the backend adds is the resource that
*populates* the Secret that reference addresses — one `ExternalSecret` per Secret name, with one `data`
entry per key something in the environment reads, rendered ahead of every workload.

```yaml
secrets:
  backend: externalSecrets
  store: vault-backend      # optional: a SecretStore in this namespace, or a ClusterSecretStore
  refreshInterval: 15m      # optional: a positive Go duration, default 1h
```

`store` is a **name**, resolved against the ClusterProfile at render time, and the *kind* comes from
the profile — whether `vault-backend` is a namespaced `SecretStore` or a cluster-scoped
`ClusterSecretStore` is a fact about the cluster, not something the spec restates. It may be omitted
when the cluster offers exactly one store. Anything less definite is refused rather than guessed:

| Code | When |
|---|---|
| `render/external-secrets-not-installed` | the profile reports no external-secrets operator. A profile with a detection *gap* on `externalSecrets` renders — "we could not look" is never "it is absent" |
| `render/external-secrets-store-not-found` | `store` names one the cluster does not have, or none is named and the cluster has none. The error lists what it does have |
| `render/external-secrets-store-ambiguous` | no `store` and several are available, or the name matches both a `SecretStore` and a `ClusterSecretStore` |

Each remote value is addressed as `remoteRef: {key: <secret name>, property: <key>}` — the spec's own
two parts, unchanged, so a value is where an author would look for it. The path *prefix* (a Vault mount,
an AWS name prefix, a GCP project) belongs to the SecretStore's own `spec.provider` and has no spelling
in the kelson spec: kelson references a store and never configures one.

**kelson needs no per-backend code.** Vault, AWS Secrets Manager, GCP Secret Manager and Azure Key
Vault are backends *of external-secrets*, configured in the SecretStore; an `ExternalSecret` is
provider-agnostic. ADR-0020 records the API reading that rests on.

**A failed sync is visible.** The observation plane reads each ExternalSecret's `Ready` condition and
reports `secret-sync-failed` with the controller's own reason and message, in the same verdict list as
the workloads and above them — a Secret that never synced is why the pods below it cannot start.
Nothing reads the Secret itself: kelson holds no value under this backend at any point.

### The `sops` backend

The spec text does not change here either. `{secret: checkout-db, key: url}` renders the same
`secretKeyRef`, and a test asserts the sops render differs from the cluster render by nothing. What the
backend changes is *where the value lives*: encrypted with [age](https://age-encryption.org), shipped
inside the published artifact beside the workloads that reference it, and decrypted on the way into the
cluster by kustomize-controller.

```yaml
secrets:
  backend: sops
  ageRecipients:                     # required: the PUBLIC age keys, `age1…`
    - age13w78znajf5kee8msacel80jz6qeuc9tyxhuqkwnqcsaymlrj7clsy4fgdw
  ageKeySecret: sops-age             # optional; the Secret holding the age identity, default sops-age
```

This is the backend that closes ADR-0009's documented gap: **a cluster rebuilt from the artifact comes
back with its secrets**, because they are in it. What has to survive outside is one age identity.

| | |
|---|---|
| **What is encrypted** | the values under `data`/`stringData` and nothing else. The Secret's name, namespace and key names stay readable, which is what makes an encrypted secret reviewable |
| **Who decrypts** | kustomize-controller, per-Kustomization. It does not care whether the source is a `GitRepository` or an `OCIRepository`, which is why the mechanism survived the transport change intact ([ADR-0028](adr/0028-delivery-spine.md) §7) |
| **Who writes the decryption block** | **kelson**, on the `Kustomization` it owns, from the environment's `ageKeySecret`. This reverses [ADR-0022](adr/0022-sops-age.md) §5 and closes its worst failure mode: "encrypted but never decrypted" is no longer reachable by skipping a step |
| **What kelson holds** | the public recipients, and nothing else. There is no age private key anywhere in kelson and no flag that takes one |
| **`set` writes the whole Secret** | carrying the other keys forward would need the identity kelson does not have, so a write that would drop keys is refused with those keys named (`secret/sops-partial-set`) |

One step is still the operator's, because kelson cannot do it: creating the Secret that holds the age
**identity** in the cluster. `kelson secret set` prints the command. [Secrets](secrets.md) is the full
guide — setup, rotation and recovery — and [ADR-0022](adr/0022-sops-age.md) records why the format is
implemented rather than imported and why rotation reports rather than re-encrypts.

> **Transition ([#225](https://github.com/dafrie/kelson/issues/225)).** `kelson secret set` writes the
> ciphertext to `<delivery.git.path>/secrets/<name>.enc.yaml` today, because the transport is still a
> git repository. Where the ciphertext is held once the artifact is the transport — so that the
> publisher picks it up — is R2 work; the ADRs decide that it travels *in the artifact* and do not
> decide where it is stored on the way there.

## Environment schema

```yaml
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata:
  name: production
spec:
  project: checkout                  # required: the Project this environment deploys
  namespace: checkout-prod           # target namespace
  routing:
    domainSuffix: acme.run
    gatewayClass: envoy              # Gateway API only (#140); a spec with `ingressClass` is rejected
    tls: true                        # default true
  policy:                            # agent guardrails, enforced server-side (ADR-0025)
    agents: propose-only             # allow | propose-only; default allow
    require: [dry-run]               # only dry-run is defined today
    maxReplicas: 5                   # the largest an agent may scale a workload here
    protect: [db]                    # components an agent may not remove or scale to zero
    forbid: [secret-set]             # deploy|rollback|promote|build|secret-set|secret-delete|spec-write|spec-delete
    deployers: [team-platform]       # who may deploy; still rejected — tenancy (#231)
  secrets:
    backend: cluster                 # cluster | externalSecrets | sops
    store: vault-backend             # externalSecrets only; optional when the cluster offers one store
    refreshInterval: 1h              # externalSecrets only; a positive Go duration, default 1h
    ageRecipients: [age1…]           # sops only; required — the PUBLIC age keys secrets are encrypted to
    ageKeySecret: sops-age           # sops only; the Secret holding the age identity, default sops-age
  previews:                          # needs flux-operator — see "Previews" below
    provider: github                 # github | gitlab
    repo: https://github.com/acme/checkout           # the SOURCE repo, not the artifact repository
    secretRef: github-auth           # optional; a Secret name, never a token — omit to materialize one (ADR-0033)
    artifacts:
      repository: oci://ghcr.io/acme/checkout-previews
  components:                        # one override list, matched by name
    - name: web                      # must name a Component in the Project
      image: ghcr.io/acme/checkout@sha256:9f6ad2c1…   # P3: the promotion pin
      replicas: { min: 3, max: 20 }
      resources:
        requests: { cpu: 500m, memory: 512Mi }
        limits:   { memory: 1Gi }
      env:
        LOG_LEVEL: warning
    - name: db                       # P5: per-environment topology override
      preset: ha-small
```

There is no `delivery:` block and no `cluster:` field: one spine
([ADR-0028](adr/0028-delivery-spine.md)) and one cluster
([ADR-0031](adr/0031-single-cluster-single-tenant.md)). Where artifacts are pushed is the controller's
configuration (`--registry`, `--push-secret`), not application description — the same rule that puts the
build destination on the server rather than in the spec.

> **Transition ([#224](https://github.com/dafrie/kelson/issues/224) /
> [#234](https://github.com/dafrie/kelson/issues/234)).** Both fields are still accepted by the schema:
> `cluster:` as a `schema/not-implemented` refusal, `delivery:` as a live block whose `mode` defaults to
> `direct`. Documents that set them keep working until the removal PR; documents that omit them are
> already written for the target model.

## The minimum viables

Progressive disclosure means these complete documents are valid:

```yaml
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: { name: hello }
spec:
  image: ghcr.io/acme/hello:v1
  components:
    - name: web
      port: 8080
---
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: { name: dev }
spec:
  project: hello
  namespace: hello-dev
```

Defaults fill the rest: no agent guardrails, cluster secrets, one replica. Delivery needs nothing said
about it — every environment renders, publishes an artifact and is reconciled by Flux.

## Validation (issue #28)

`internal/model` reports **all** problems, structured — never fail-fast prose. Every error carries
`{code, resource, field, message, remediation, docsUrl}`, a JSONPath-style field path
(`$.spec.components[2].port`) and, when parsed from YAML, a 1-based line/column.

Stable code taxonomy:

| Code | Class | Example |
|---|---|---|
| `schema/unknown-field` | schema | `spec.port` on Project |
| `schema/missing-required` | schema | Environment without `spec.project` |
| `schema/invalid-format` | schema | malformed domain, quantity, cron, name; an env mapping that is neither reference form |
| `schema/out-of-range` | schema | `port: 70000` |
| `schema/invalid-enum` | schema | `secrets.backend: vault` |
| `schema/duplicate-name` | schema | two Components named `web` |
| `schema/mutually-exclusive` | semantic-shape | `schedule:` with `port:`; `preset:` on a worker; `chart:` on a service; `kind:` against the shape |
| `schema/not-implemented` | schema | `tools:`, `policy.deployers:`, `release:` — validated, not yet rendered |
| `ref/unknown-component` | semantic | Environment override for an undeclared component |
| `ref/unknown-service` | semantic | `from: {service: cache}` names no data component |
| `ref/unknown-service-key` | semantic | `from: {service: db, key: tls}` |
| `secret/literal` | semantic | secret value where a reference belongs |
| `semantic/no-image-source` | semantic | no image and `build.strategy: none` |
| `semantic/auth-provider-mismatch` | semantic | GitConnection `auth.githubApp` with `provider: generic` — the app-manifest flow and installation tokens are GitHub's ([ADR-0033](adr/0033-git-connections.md)) |

`semantic/git-target-missing` existed to require a git target for a mode that no longer exists, and goes
with the `delivery:` block ([ADR-0028](adr/0028-delivery-spine.md) decision 9). The codes are a
compatibility promise, so a code is retired by deleting what could raise it — never by reusing it for
something else.

Every class carries a remediation: ranges state the accepted range, enums list the valid values,
references list declared names, and secret literals name the exact `{secret: …, key: …}` replacement
and the command that creates the Secret. `docsUrl` uses
the stable basis `https://kelson.dev/model/errors/<code>`; these URLs are a compatibility promise like
the codes themselves.

Validation runs in two passes over the same document: **decode** (YAML/JSON to typed values, collecting
unknown fields and type errors with positions) and **check** (schema-level ranges/formats plus the
semantic rules above). Cross-document rules (Environment ↔ Project references) run in
`ValidateSet(project, environment)`.
