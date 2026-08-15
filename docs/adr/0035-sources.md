# ADR-0035: Sources are declared, then bound — per-component sources and the global tier

- **Status:** Proposed
- **Date:** 2026-08-15

> Reframes issue [#239](https://github.com/dafrie/kelson/issues/239) per the owner's clarification:
> the unit that *uses* a repository is the **component**, one-to-one; what a Project (or the
> instance) does is *declare* which repositories are available. Builds on
> [ADR-0033](0033-git-connections.md) (connections resolve per source URL; the global tier reuses
> its ownership pattern) and composes with [ADR-0034](0034-forge-driven-delivery.md) (`ReportBuild`
> is already component-keyed; `autoDeploy`'s eventual subject is the source, not the environment).
> The Project's single `source:` block ([ADR-0006](0006-project-application-environment.md)) becomes
> a shorthand, not a different model.

## Context

A Project has exactly one `source` — `{git, ref}` — and every buildable component implicitly builds
from it. That is the right default and the wrong ceiling: a project whose `web` lives in one
repository and whose `worker` lives in another cannot be expressed, and a repository shared by many
projects (the platform monorepo, a common tools repo) must be re-declared in each one.

Issue #239 initially framed the gap as environment-level ref overrides ("staging tracks
`develop`"). The owner's clarification (recorded on the issue, 2026-08-15) names the actual model:

- **Declaring** a source is configuration: a Project may offer several; the instance may offer some
  globally, visible to and selectable by everyone — the same instance-wide posture ADR-0033 gave
  connections.
- **Using** a source is a component's act, and it is one-to-one: each buildable component builds
  from exactly one source.

Branch-tracking *environments* remain unrequested and undesigned; if that idea returns it must be
argued against this model.

## Decision

### 1. A source is a named `{git, ref, connection?}`; a Project declares zero or more

```yaml
# Project
spec:
  sources:                       # optional
    - name: app                  # DNS-1123 label, unique in the list
      git: https://github.com/acme/checkout
      ref: main
    - name: tools
      git: https://github.com/acme/build-tools
      ref: v2                    # a ref is per-source, exactly as it is per-project today
      connection: acme-github    # optional, ADR-0033 d4 semantics per source
```

The existing singular `source:` **stays, as shorthand**: it declares the list `[{name: default,
…}]`. Declaring both spellings is a validation error naming the fix. Nothing existing migrates;
nothing existing changes meaning.

### 2. The global tier is a `GitSource` custom resource

A small namespaced CRD in `kelson.dev/v1alpha1` (kelson-system, like `GitConnection`), carrying
exactly the same `{git, ref, connection?}` spec plus the `owner` block ADR-0033 d6 defined —
`instance` today, `user`/`team` reserved for #231. An instance-owned `GitSource` is visible to and
selectable by every project. It has no status beyond validation: reachability belongs to the
connection that serves it.

### 3. A component binds by name; project shadows global; unset means the default

```yaml
components:
  - name: web
    kind: service
    source: app        # a project source, or a GitSource's name
  - name: worker
    kind: service
    source: tools
```

Resolution, in order: the project's `sources` list, then `GitSource` objects. A project-local name
**shadows** a global one — innermost scope wins, the same instinct as the P1–P3 precedence rules. A
component naming a source that resolves nowhere is a validation error listing what is in scope.

A component with **no** `source:` uses the project's default: the shorthand `source:` if that
spelling was used, else the `sources` entry named `default`, else — when the project declares
multiple sources and no `default` — a validation error naming the candidates rather than a silent
pick. A project with no sources at all keeps today's meaning: nothing to build from, image-only
components ([ADR-0010](0010-build-strategy.md)'s `strategy: none` posture).

### 4. The planes follow the binding

- **Build**: a component's clone, ref-resolution and build run against *its* source — repo, ref and
  connection resolved per ADR-0033 d4/d5, per source rather than per project. One project may clone
  several repositories per revision; each build stays per-component, as it already is.
- **Delivery**: unchanged. Rendered output never contained the source; revisions record what they
  already record, per component.
- **`ReportBuild`** ([ADR-0034](0034-forge-driven-delivery.md) d3): already component-keyed
  (`--image web=…`), so a CI in either repository reports the components it builds and no others.
- **`autoDeploy`** (still future, #248): its subject is now precise — a push to repository X
  re-renders what is bound to sources matching X. This is the "one answer, not two fields" the
  #239 discussion asked for.
- **Webhook/preview matching** (ADR-0034 d2): candidate repositories for a project are the union of
  its bound sources' repositories.

## Rationale

- **Declare/use as two acts** matches how every other kelson concept already works — connections
  are declared once and resolved where used; secrets are declared elsewhere and referenced by name.
  The component is the unit that builds ([ADR-0010](0010-build-strategy.md)), so it is the unit
  that names its input.
- **Shadowing over erroring on clash**, because a global name is a convenience, not a claim; a
  project must be able to redefine `tools` without asking the instance.
- **A CRD for the global tier** for the same reasons ADR-0033 d1 gave connections one: instance
  scope, kubectl visibility, the `owner` block already designed. A `GitSource` is deliberately
  dumber than a `GitConnection` — data, not credentials.
- **Refusing the missing default** (multiple sources, unbound component, none named `default`)
  rather than picking the first: a list order is not a decision, and the error names the one-line
  fix.

## Consequences

**Positive.** Multi-repo projects exist; shared repositories are declared once; the single-source
project reads exactly as it always did; `autoDeploy` gains a precise subject before it is built.

**Negative.**

- A second place a build's input can be defined (three with the shorthand). The validation errors
  above are what keep resolution answerable from the spec alone.
- Per-component sources mean per-component clones: a revision of a two-repo project is only as
  reproducible as two refs instead of one, and `kelson build` orchestration fans out.
- The global tier adds a third CRD whose contents affect renders without living in the project —
  `kelson render` stays pure by taking resolved bindings as input, which makes the resolver one
  more thing the server owns and the CLI must replicate.
- Name shadowing can surprise: a project adding a local `tools` silently stops using the global
  one. The UI should show a shadowed name as such.

## Revisit when

- **Tenancy (#231)** — `GitSource.owner` gets enforcement with the same semantics as connections.
- **`autoDeploy` is designed** — it consumes this model's bindings; nothing here pre-decides its
  opt-in shape.
- **Someone asks for per-environment source refs again** — that is a *fourth* declaration site and
  needs this ADR's rules extended, not bypassed.
