# The kelson model — Project, Application, Environment

This is the authoritative reference for the authoring-plane schemas ([ADR-0006](adr/0006-project-application-environment.md)).
It resolves the open questions ADR-0006 and issue #24 called out, before implementation. The Go types in
`internal/model` and the generated JSON Schemas in `schema/` are derived from this document; when they
disagree, this document is wrong until an ADR says otherwise.

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
| `Project.spec.services` and every `{from: {service, key}}` binding | M9 · Data services ([#10](https://github.com/dafrie/kelson/issues/10)) |
| `Environment.spec.services` | M9 · Data services |
| `Project.spec.defaults.secrets`, `Environment.spec.secrets` | M8 · Secrets |
| `Project.spec.defaults.policy`, `Environment.spec.policy` | M7 · Agent surface & MCP |
| `Environment.spec.cluster` | M10 · Environments & promotion |

The gate lives in validation only: `internal/model/notimplemented.go` holds the table, and
`internal/model/coverage_test.go` fails the build if a new spec field is neither consumed nor gated.
Resolution and rendering of these fields already work, so a milestone lands by deleting a table row.

## Documents

The model is two YAML (or JSON) documents. There is deliberately no third kind.

```yaml
apiVersion: kelson.dev/v1alpha1
kind: Project                    # shared configuration + applications
---
apiVersion: kelson.dev/v1alpha1
kind: Environment                # where applications run, and what differs there
```

A **Project** names its Applications inline (`spec.applications`). An **Environment** binds itself to a
Project (`spec.project`) and carries target, routing, delivery, policy and per-Application overrides.
Projects stay environment-agnostic (a Project document never mentions an environment) and Environments
stay project-agnostic in shape (nothing in the Environment schema depends on which project it binds to).
Environments are scoped to a Project by reference; they are not owned objects embedded in the Project.

There is exactly one Application spec format. Project-level "shared configuration" is not a second spec
format: it is a small set of fields on the Project (`source`, `build`, `image`, `env`, `services`) that
act as defaults merged into each Application by rule P1 below. An Application written inline in a Project
and one authored field-by-field against the JSON Schema are the same document.

## Resolutions of ADR-0006's open questions

**1. Precedence when Project and Environment both set a value.**
See the precedence rules below. Short version: the innermost scope wins; Environment overrides
Application overrides Project. For Environment-scoped concerns (delivery, policy, secrets), an explicit
Environment value always wins over a Project default; values never merge across the Project/Environment
boundary — the winner is taken whole.

**2. Project-level shared configuration without a second spec format.**
Shared fields live on the Project as *defaults for every Application*: `env` merges key-by-key,
`source`/`build`/`image` supply the artifact an Application omits. `services` are Project-scoped because
bindings cross Applications (web and worker both use the same database). That is the whole mechanism.

**3. May an Application belong to more than one Project?**
No. An Application is exactly one entry in exactly one Project and is identified by the pair
`(project name, application name)`. Sharing behaviour across Projects is expressed by duplicating the
Application into both Projects or by extracting a library chart via `overlays`, never by membership in
two Projects.

**4. Application-type differences.**
No explicit `type:` field. The workload kind is *derived*:

| Spec shape                    | Workload   |
|-------------------------------|------------|
| `port:` set                   | web service (Deployment + Service + routing) |
| `schedule:` set               | CronJob    |
| neither                       | worker (Deployment, no routing) |

`schedule:` and `port:` are mutually exclusive (validation error `schema/mutually-exclusive`), as are
`schedule:` with `domains:`/`health:`. A cron job that also serves traffic is two Applications. The rule
keeps the happy path free of vocabulary; every kind is reachable, none needs to be named.

**5. Hiding the Project when there is only one Application.**
Presentation, not schema. The schema keeps the Project level always (uniformity beats special cases for
agents and for the API). The CLI and UI may synthesize or hide it: `kelson` accepts a bare Application
file and wraps it in a Project of the same name, and the UI shows Projects with one Application without
the grouping chrome.

**6. What is versioned.**
The **Project document** is the versioned unit: one file, one commit, optimistic-concurrency version
([ADR-0001](adr/0001-hybrid-state-model.md)). Applications still **deploy independently** from any
version — rollback of `web` does not touch `worker`. Each rendered workload carries its own spec-hash,
so an unchanged Application produces an unchanged artifact and no rollout. Coordinated multi-Application
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

**P1 — Environment variables:** merge at key level, innermost scope wins:

```
Project.spec.env
  < Application.env
    < Environment.spec.applications[].env   (matched by application name)
```

A key set at an inner scope replaces the outer value wholesale. There are no delete markers; to unset a
Project-level variable for one Environment, override it to an empty string.

**P2 — Per-Application scaling and resources:**
`Environment.spec.applications[].replicas/resources` replace the Application's values wholesale
(no deep merge of `requests` vs `limits`). Absent override → the Application's values; absent there →
`replicas: {min: 1}` fixed and no resource requests/limits.

**P3 — Image and command:** Application `image:` wins over Project `image:`. A built artifact
(Project `source` + `build`) supplies the image when neither sets one; `build.strategy: none` with no
image anywhere is a validation error (`semantic/no-image-source`). Application `command:` always wins;
Project has no command.

Until a build produces that artifact, such an application resolves to an *unresolved* image, and
rendering it fails with the structured render error `image/unresolved` naming each application —
kelson never emits a placeholder image into a manifest. Supply the built reference with `--image`
(`kelson render`, `diff`, `deploy`, `status`, `rollback`), which stands in for Project `image:` and so
still loses to an Application `image:`.

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

**P5 — Service presets:** `Environment.spec.services[].preset` (matched by service name) replaces the Project
service's preset for that Environment — `shared` in development, `ha-small` in production, from one Project
spec ([ADR-0007](adr/0007-data-services.md)).

**P6 — Overlays:** concatenate, Project first, then Environment. Each patch applies in order to the
resources rendered so far; manifests are emitted as extra resources in order. Overlays are the escape
hatch of record: any long-tail requirement not in the schema goes here ([docs/architecture.md](architecture.md)).

**Domain defaulting (not precedence, but resolved here):** Application `domains:` are explicit FQDNs and
win. If an Application has `port:` but no `domains:`, and the Environment sets `routing.domainSuffix`,
the default hostname is `<application>.<domainSuffix>` — e.g. `web.staging.acme.run`. Environments should
carry distinct suffixes so defaults never collide.

## Services and bindings

> Not implemented yet. `services:` and `from:` bindings are rejected with `schema/not-implemented`
> until M9 · Data services lands ([#141](https://github.com/dafrie/kelson/issues/141)): nothing
> provisions the credential Secret a binding names, so rendering one would produce a workload that
> never starts.

```yaml
spec:
  services:
    - name: db
      type: postgres           # postgres | valkey
      preset: ha-small         # shared | small | ha-small | ha-medium | branch
  env:
    DATABASE_URL:
      from: { service: db, key: uri }
```

A binding is **never** a value. The value lives in the credential Secret the data layer provisions for
service `db` — named `<project>-<service>-credentials` in the target namespace — and the renderer emits a
`secretKeyRef` against it. Well-known keys per service type:

| Type | Keys |
|---|---|
| `postgres` | `uri`, `host`, `port`, `database`, `username`, `password` |
| `valkey`   | `uri`, `host`, `port`, `password` |

Unknown service names and keys are validation errors (`ref/unknown-service`, `ref/unknown-service-key`)
with the list of valid keys in the remediation. Both the renderer and agents therefore reason about
bindings from the schema alone.

## Secrets: references, never literals

Per [ADR-0009](adr/0009-secrets.md), enforcement lives in one place and validation mirrors it so authors
see the failure before render:

- An environment value is either a plain string or `{from: ...}` — nothing else is representable.
- A string value is rejected with `secret/literal` when:
  - it parses as a URL containing a password (`postgres://user:pass@host/db`), or
  - the variable name matches a secret pattern (`PASSWORD`, `SECRET`, `TOKEN`, `_KEY`, `PRIVATE`,
    `CREDENTIAL`, `AUTH`) and the value is non-empty.
- The error names the field and a fix that exists today. There is no `kelson secret set` command
  ([#142](https://github.com/dafrie/kelson/issues/142)) and `from:` bindings are themselves gated
  ([#141](https://github.com/dafrie/kelson/issues/141)), so the remediation points at an overlay patch
  referencing a Secret you manage, until M8 · Secrets lands.

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
  applications:
    - name: web                      # must name an Application in the Project
      replicas: { min: 3, max: 20 }
      resources:
        requests: { cpu: 500m, memory: 512Mi }
        limits:   { memory: 1Gi }
      env:
        LOG_LEVEL: warning
  services:                          # whole block rejected until M9 (#141)
    - name: db
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
  applications:
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
(`$.spec.applications[2].port`) and, when parsed from YAML, a 1-based line/column.

Stable code taxonomy:

| Code | Class | Example |
|---|---|---|
| `schema/unknown-field` | schema | `spec.port` on Project |
| `schema/missing-required` | schema | Environment without `spec.project` |
| `schema/invalid-format` | schema | malformed domain, quantity, cron, name |
| `schema/out-of-range` | schema | `port: 70000` |
| `schema/invalid-enum` | schema | `delivery.mode: github` |
| `schema/duplicate-name` | schema | two Applications named `web` |
| `schema/mutually-exclusive` | semantic-shape | `schedule:` with `port:` |
| `schema/not-implemented` | schema | `services:`, `policy:`, `secrets:`, `cluster:` — validated, not yet rendered |
| `ref/unknown-application` | semantic | Environment override for undeclared app |
| `ref/unknown-service` | semantic | `from: {service: cache}` never declared |
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
