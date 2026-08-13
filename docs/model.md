# The kelson model — Project, Component, Environment

This is the authoritative reference for the authoring-plane schemas ([ADR-0006](adr/0006-project-application-environment.md),
as amended by [ADR-0014](adr/0014-components.md)). It resolves the open questions ADR-0006 and issue #24
called out, before implementation. The Go types in `internal/model` and the generated JSON Schemas in
`schema/` are derived from this document; when they disagree, this document is wrong until an ADR says
otherwise.

**The leaf is a Component.** ADR-0006 called it an Application and put managed data services in a second
list beside it; ADR-0014 unified the two into one `spec.components` list with a closed set of kinds —
`service`, `worker`, `cron`, `agent`, `postgres`, `valkey`. Where this page says *component*, ADR-0006 says
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
| `Project.spec.defaults.secrets`, `Environment.spec.secrets` | M8 · Secrets |
| `Project.spec.defaults.policy`, `Environment.spec.policy` | M7 · Agent surface & MCP |
| `Environment.spec.cluster` | M10 · Environments & promotion |

The gate lives in validation only: `internal/model/notimplemented.go` holds the table, and
`internal/model/coverage_test.go` fails the build if a new spec field is neither consumed nor gated.
Resolution of these fields already works, so a milestone lands by deleting a table row — the data-service
fields and the `from:` bindings left the table exactly that way with
[#89](https://github.com/dafrie/kelson/issues/89).

Not every refusal is a gate. A field can be consumed and still have values kelson will not render:
`kind: valkey`, `preset: branch`, and a preset the target cluster's CloudNativePG cannot host are
structured *render* errors, because the check needs a ClusterProfile and validation deliberately has
none. See [docs/data-services.md](data-services.md).

And not every refusal is either: a field that belongs to another kind is a plain validation error, because
one list means one type carrying fields only some of its kinds use. `preset` on a worker, `port` on a
`kind: postgres`, `tools` on anything but an agent, and a written `kind:` that contradicts the component's
shape are all `schema/mutually-exclusive` rather than fields that quietly resolve into nothing.

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
Project (`spec.project`) and carries target, routing, delivery, policy and per-Component overrides.
Projects stay environment-agnostic (a Project document never mentions an environment) and Environments
stay project-agnostic in shape (nothing in the Environment schema depends on which project it binds to).
Environments are scoped to a Project by reference; they are not owned objects embedded in the Project.

There is exactly one Component spec format and exactly one list. Project-level "shared configuration" is
not a second spec format: it is a small set of fields on the Project (`source`, `build`, `image`, `env`)
that act as defaults merged into each workload Component by rule P1 below. A Component written inline in a
Project and one authored field-by-field against the JSON Schema are the same document.

## Resolutions of ADR-0006's open questions

**1. Precedence when Project and Environment both set a value.**
See the precedence rules below. Short version: the innermost scope wins; Environment overrides
Component overrides Project. For Environment-scoped concerns (delivery, policy, secrets), an explicit
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

`schedule:` and `port:` are mutually exclusive (validation error `schema/mutually-exclusive`), as are
`schedule:` with `domains:`/`health:`. A cron job that also serves traffic is two Components. The rule
keeps the happy path free of vocabulary; every kind is reachable, none needs to be named.

Writing `kind:` is allowed for every kind and required for the data kinds. When written it is checked
against the enum *and* against the shape — `kind: service` needs a port, `kind: cron` needs a schedule,
`kind: worker` and `kind: agent` need neither — so an explicit kind states what a component is and never
silently overrules the fields that say otherwise ([ADR-0014](adr/0014-components.md)).

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

**7. How service bindings are expressed.**
See *Services and bindings* below. Bindings are data (`{from: {service, key}}`), enumerable through the
JSON Schema, so the renderer can turn them into `secretKeyRef`s and agents can reason about them without
parsing prose.

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

**P3 — Image and command:** Component `image:` wins over Project `image:`. A built artifact
(Project `source` + `build`) supplies the image when neither sets one; `build.strategy: none` with no
image anywhere is a validation error (`semantic/no-image-source`). Component `command:` always wins;
Project has no command.

Until a build produces that artifact, such a component resolves to an *unresolved* image, and
rendering it fails with the structured render error `image/unresolved` naming each component —
kelson never emits a placeholder image into a manifest. Supply the built reference with `--image`
(`kelson render`, `diff`, `deploy`, `status`, `rollback`), which stands in for Project `image:` and so
still loses to a Component `image:`.

**P4 — Environment-scoped concerns (delivery, policy, secrets):** an explicit Environment value always
wins over the Project `defaults` value; otherwise the Project default; otherwise the built-in default:

| Field | Built-in default |
|---|---|
| `delivery.mode` | `direct` |
| `policy.agents`  | `propose-only` |
| `policy.require` | none |
| `secrets.backend`| `cluster` |

These values never merge across the boundary: there is no "strictest of both" arithmetic. If production
must stay propose-only, that is written on the production Environment. `delivery.git` exists only on
Environments (a Project-level Git target for deployments would be meaningless; every environment needs
its own repo/branch/path).

**P5 — Data-component presets:** `Environment.spec.components[].preset` (matched by component name) replaces
the Project component's preset for that Environment — `shared` in development, `ha-small` in production,
from one Project spec ([ADR-0007](adr/0007-data-services.md)). An override block carries the fields its
target's kind uses and nothing else: `preset` for a data component, `replicas`/`resources`/`env` for a
workload. Crossing that line is a validation error, not a silent no-op.

**P6 — Overlays:** concatenate, Project first, then Environment. Each patch applies in order to the
resources rendered so far; manifests are emitted as extra resources in order. Overlays are the escape
hatch of record: any long-tail requirement not in the schema goes here ([docs/architecture.md](architecture.md)).

**Domain defaulting (not precedence, but resolved here):** Component `domains:` are explicit FQDNs and
win. If a Component has `port:` but no `domains:`, and the Environment sets `routing.domainSuffix`,
the default hostname is `<component>.<domainSuffix>` — e.g. `web.staging.acme.run`. Environments should
carry distinct suffixes so defaults never collide.

## Identity: one ServiceAccount per component

Every workload component renders its own ServiceAccount, named after the component, and its pod template
references it ([ADR-0014](adr/0014-components.md) decision D). It is metadata-only today; it exists so
policy has an attachment point that is already in place when there is policy to attach — the per-component
identity that [kagent](https://kagent.dev) calls the most important blast-radius control for agents, and
that costs nothing to apply uniformly.

Data components render none: CloudNativePG creates and owns the identity its clusters run under, which is
what delegating topology to an operator means ([ADR-0005](adr/0005-delegate-to-operators.md)).

The rendered identity labels are unchanged by the rename: pods still carry `kelson.dev/application` and
Deployments still select on it. A selector is immutable in Kubernetes, so renaming that label would orphan
every running workload — the spec's vocabulary changed, the cluster's did not.

## Data components and bindings

> Implemented for `kind: postgres` since [#89](https://github.com/dafrie/kelson/issues/89). What each
> preset renders, the sizing defaults and the capability rules are in
> [docs/data-services.md](data-services.md). Still refused, loudly and by the *renderer* rather than by
> validation: `kind: valkey` ([#98](https://github.com/dafrie/kelson/issues/98)), `preset: branch`
> ([#99](https://github.com/dafrie/kelson/issues/99)), and any preset the target cluster's
> CloudNativePG cannot host.

```yaml
spec:
  components:
    - name: db
      kind: postgres           # postgres | valkey — always explicit, never derived
      preset: ha-small         # shared | small | ha-small | ha-medium | branch
  env:
    DATABASE_URL:
      from: { service: db, key: uri }
```

The binding key stays `service:` after the rename: what it names is the service a data component provides,
and every other kind is unbindable. Binding to a workload is `ref/unknown-service` with the bindable names
in the remediation.

A binding is **never** a value. The value lives in the Secret the component's operator generates — for a
dedicated postgres preset that is CloudNativePG's `<cluster>-app`, where `<cluster>` is
`<project>-<environment>-<component>` — and the renderer emits a `secretKeyRef` against it. kelson's key
names are the spec's contract and are mapped onto the operator's own (`database` is CNPG's `dbname`).
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

## Secrets: references, never literals

Per [ADR-0009](adr/0009-secrets.md), enforcement lives in one place and validation mirrors it so authors
see the failure before render:

- An environment value is either a plain string or `{from: ...}` — nothing else is representable.
- A string value is rejected with `secret/literal` when:
  - it parses as a URL containing a password (`postgres://user:pass@host/db`), or
  - the variable name matches a secret pattern (`PASSWORD`, `SECRET`, `TOKEN`, `_KEY`, `PRIVATE`,
    `CREDENTIAL`, `AUTH`) and the value is non-empty.
- The error names the field and a fix that exists today. For a database URL that is a `from:` binding
  against a declared service, which renders end to end since
  [#89](https://github.com/dafrie/kelson/issues/89). For anything else there is still no
  `kelson secret set` command ([#142](https://github.com/dafrie/kelson/issues/142)), so the remediation
  points at an overlay patch referencing a Secret you manage, until M8 · Secrets lands.

The heuristic deliberately errs toward rejection on secret-shaped variables; the fix is cheap and correct
in both directions. The Environment is designed to pick the backend (`cluster` built-in default,
`externalSecrets` with `store:`, `sops`) — a schema-level choice since day one so v0.2 backends are not a
breaking change — but `secrets:` is rejected with `schema/not-implemented` until M8, because nothing
reads the resolved backend.

## Environment schema

```yaml
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata:
  name: production
spec:
  project: checkout                  # required: the Project this environment deploys
  cluster: prod-eu                   # rejected until M10 — there is no cluster registry (#141)
  namespace: checkout-prod           # target namespace
  routing:
    domainSuffix: acme.run
    gatewayClass: envoy              # Gateway API only (#140); a spec with `ingressClass` is rejected
    tls: true                        # default true
  delivery:
    mode: flux                       # direct | flux (argocd removed — ADR-0012)
    git:                             # required for flux, forbidden for direct
      repo: git@github.com:acme/deploy.git
      branch: main
      path: checkout/production
  policy:                            # whole block rejected until M7 (#141)
    agents: propose-only             # allow | propose-only
    require: [dry-run]               # only dry-run is defined today
    deployers: [team-platform]       # who may deploy; default: the Project's team
  secrets:                           # whole block rejected until M8 (#141)
    backend: sops                    # cluster | externalSecrets | sops
  components:                        # one override list, matched by name
    - name: web                      # must name a Component in the Project
      replicas: { min: 3, max: 20 }
      resources:
        requests: { cpu: 500m, memory: 512Mi }
        limits:   { memory: 1Gi }
      env:
        LOG_LEVEL: warning
    - name: db                       # P5: per-environment topology override
      preset: ha-small
```

Delivery mode is per-Environment ([ADR-0001](adr/0001-hybrid-state-model.md)): development applies
directly, production goes through pull requests, one renderer feeding both.

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

Defaults fill the rest: direct delivery, propose-only agents, cluster secrets, one replica.

## Validation (issue #28)

`internal/model` reports **all** problems, structured — never fail-fast prose. Every error carries
`{code, resource, field, message, remediation, docsUrl}`, a JSONPath-style field path
(`$.spec.components[2].port`) and, when parsed from YAML, a 1-based line/column.

Stable code taxonomy:

| Code | Class | Example |
|---|---|---|
| `schema/unknown-field` | schema | `spec.port` on Project |
| `schema/missing-required` | schema | Environment without `spec.project` |
| `schema/invalid-format` | schema | malformed domain, quantity, cron, name |
| `schema/out-of-range` | schema | `port: 70000` |
| `schema/invalid-enum` | schema | `delivery.mode: github` |
| `schema/duplicate-name` | schema | two Components named `web` |
| `schema/mutually-exclusive` | semantic-shape | `schedule:` with `port:`; `preset:` on a worker; `kind:` against the shape |
| `schema/not-implemented` | schema | `tools:`, `policy:`, `secrets:`, `cluster:` — validated, not yet rendered |
| `ref/unknown-component` | semantic | Environment override for an undeclared component |
| `ref/unknown-service` | semantic | `from: {service: cache}` names no data component |
| `ref/unknown-service-key` | semantic | `from: {service: db, key: tls}` |
| `secret/literal` | semantic | secret value where a reference belongs |
| `semantic/no-image-source` | semantic | no image and `build.strategy: none` |
| `semantic/git-target-missing` | semantic | `mode: flux` without `git.repo` |

Every class carries a remediation: ranges state the accepted range, enums list the valid values,
references list declared names, and secret literals name the exact `from:` replacement. `docsUrl` uses
the stable basis `https://kelson.dev/model/errors/<code>`; these URLs are a compatibility promise like
the codes themselves.

Validation runs in two passes over the same document: **decode** (YAML/JSON to typed values, collecting
unknown fields and type errors with positions) and **check** (schema-level ranges/formats plus the
semantic rules above). Cross-document rules (Environment ↔ Project references) run in
`ValidateSet(project, environment)`.
