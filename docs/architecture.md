# Architecture

## Four planes

kelson separates cleanly into four planes. The boundaries are load-bearing — most of the design's
properties fall out of keeping them honest.

```
┌──────────────────────────────────────────────────────────────────────┐
│  AUTHORING          CLI · Web UI · HTTP API · MCP server             │
│                     four peer clients of one typed API               │
│                     intent → validated Application spec              │
└─────────────────────────────┬────────────────────────────────────────┘
                              │  Application spec
┌─────────────────────────────▼────────────────────────────────────────┐
│  RENDERING          pure function: (spec, ClusterProfile) → manifests │
│                     no cluster · no network · no clock · no DB        │
│                     deterministic · golden-file tested                │
└─────────────────────────────┬────────────────────────────────────────┘
                              │  plain Kubernetes YAML + provenance
┌─────────────────────────────▼────────────────────────────────────────┐
│  DELIVERY           pluggable adapter, identical input                │
│                     ├── direct    kelson server-side applies          │
│                     └── flux      commit → GitRepository → Kustomize  │
└─────────────────────────────┬────────────────────────────────────────┘
                              │
┌─────────────────────────────▼────────────────────────────────────────┐
│  OBSERVATION        watches live resources, correlates to app model   │
│                     via kelson.dev/revision · status, logs, metrics   │
└──────────────────────────────────────────────────────────────────────┘
```

### Why the rendering plane must stay pure

Purity is not aesthetics. It buys, concretely:

- **Hybrid delivery for free.** Direct and GitOps modes consume identical rendered output; the only
  difference is who calls `apply`. That is an adapter, not a second codebase. Incumbents cannot do this
  because their rendering is entangled with their apply logic.
- **Trustworthy previews.** A diff is only meaningful if rendering the same spec twice gives the same
  bytes.
- **Cheap, fast tests.** The bulk of kelson's correctness lives in golden files, not in an integration
  suite that needs a cluster.
- **A real escape hatch.** Because output is plain YAML, users can always inspect, patch or take it and
  leave.

`ClusterProfile` is an input rather than an ambient lookup precisely to preserve purity — see below.

## The model

Three concepts ([ADR-0006](adr/0006-project-application-environment.md)), and no more without a strong
argument. Deliberately not an OAM-style hierarchy: developers should deploy before learning vocabulary.

- **Project** — shared configuration and ownership. Image, common environment, service bindings, team.
- **Application** — one deployable, rendering to one workload.
- **Environment** — where it runs and what differs there.

```yaml
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata:
  name: checkout

spec:
  source:
    git: https://github.com/acme/checkout
    ref: main
  build:
    strategy: auto          # auto | dockerfile | buildpacks | none

  env:                      # shared by every Component
    LOG_LEVEL: info
    DATABASE_URL:
      from: { service: db, key: uri }   # binding, never a literal

  components:               # one list; kinds tell them apart (ADR-0014)
    - name: db
      kind: postgres
      preset: ha-small      # topology preset → CloudNativePG Cluster with PITR

    - name: web
      port: 8080
      health: /healthz
      domains: [checkout.acme.com]
      replicas: { min: 2, max: 10 }

    - name: worker
      command: ["bundle", "exec", "sidekiq"]

    - name: nightly-report
      schedule: "0 3 * * *"

  # Escape hatch. Always available, never required.
  overlays:
    - patch: ./k8s/patches/affinity.yaml
    - manifest: ./k8s/extra/networkpolicy.yaml
```

One HA, TLS-terminated, database-backed service with a worker and a cron job, in about thirty lines with no
duplication. Everything beyond this is progressive disclosure: reachable, not present by default.

Every leaf of the spec is a component, and the kind is derived where the shape says it (`port:` → service,
`schedule:` → cron, neither → worker) and written where it cannot be (`kind: postgres`)
([ADR-0014](adr/0014-components.md)).

This is close to today's shape, not identical to it. The `kind: postgres` component and its `from:` binding
render since [#89](https://github.com/dafrie/kelson/issues/89); `policy:` (M7), `secrets:` (M8) and
`cluster:` (M10) are still rejected with `schema/not-implemented` rather than accepted and rendered as
nothing ([#141](https://github.com/dafrie/kelson/issues/141)), as is `tools:` on a `kind: agent` component
(M7, [#75](https://github.com/dafrie/kelson/issues/75)). See [the model reference](model.md) for the
current table.

Environments carry what differs between deployments — cluster, namespace, domain suffix, replica and
resource overrides, delivery mode, policy. Projects stay environment-agnostic; environments stay
project-agnostic.

**Why the abstraction stays thin.** Every concept kelson invents is one that neither a new developer nor an
LLM has seen before. Kubernetes primitives are in the training data; `kelson.dev` concepts are not. So the
spec models what genuinely recurs and hands everything else to `overlays`, which is a better answer to
long-tail requests than adding fields until the surface is overwhelming.

## ClusterProfile — adopt, don't install

On install and periodically after, kelson probes the cluster and records what it finds:

```yaml
kubernetes:      { version: v1.31.2, platform: k3s, nodeArchitectures: [amd64] }
gatewayAPI:      { version: v1.6.0, classes: [envoy] }
ingressClasses:  [{ name: nginx, controller: k8s.io/ingress-nginx, default: true }]
certManager:     { clusterIssuers: [letsencrypt-prod] }
externalSecrets: { clusterSecretStores: [vault-backend] }
prometheus:      { serviceMonitor: true, podMonitor: true }
flux:            { version: v2.4.0 }
fluxOperator:    {}
policyEngines:   [{ name: kyverno, version: v1.13.0 }]
metricsServer:   {}
incomplete:
  - field: storageClasses
    reason: 'list storageclasses.storage.k8s.io denied'
```

Absent components are simply missing from the document — `cnpg` and `argocd` are not installed here.
An empty mapping such as `metricsServer: {}` means present with an unknown version, which is a different
answer again.

`incomplete` is why the profile can be trusted. A probe that lacks permission to list storage classes
records the gap instead of reporting none, because a manifest rendered from "we didn't look" and one
rendered from "it isn't there" are indistinguishable afterwards — and only one of them is correct. It is
the same rule preview follows for policy: *checked and failing* and *could not check* never collapse into
one answer. Every such judgement in the codebase answers in one vocabulary — `clusterprofile.Outcome`,
whose package doc states the discipline once — and each `unknown` points at the `Gap` that explains it.

The renderer takes this as an input. Same spec, different cluster, correct output: a `Certificate`
where cert-manager is present rather than kelson running its own ACME client; a `ServiceMonitor` only
if something will read it. Routing is **Gateway API only** — a cluster without Gateway API is a hard,
loudly-reported capability gap with an offer to install a Gateway implementation (#140), never a
silent fallback. `ingressClasses` stays in the profile as advisory data: it powers the migration
nudge for clusters running retired ingress controllers, and is never rendered against.

Passing the profile explicitly — rather than having the renderer query the cluster — is what keeps
rendering pure and golden-testable across arbitrary cluster shapes.

**Rule: never install what is already there.** No second ingress controller. No competing cert-manager.
This is the single feature that makes kelson viable for teams already running Kubernetes properly.

## Provenance

Every rendered resource is stamped:

```yaml
metadata:
  labels:
    app.kubernetes.io/managed-by: kelson
    kelson.dev/application: checkout
    kelson.dev/environment: production
  annotations:
    kelson.dev/spec-hash:        sha256:…   # normalized spec
    kelson.dev/revision:         <git sha>  # Git mode
    kelson.dev/renderer-version: 0.4.1
```

This is what lets the observation plane answer "is my change live?" without owning the apply step. The UI
shows a real state machine rather than a spinner:

```
Proposed ──► Committed(sha) ──► Reconciling ──► Applied ──► Healthy
                                     │                         │
                                     └──► Rejected             └──► Degraded
```

`renderer-version` matters for a subtle reason: it lets kelson distinguish "the user changed something"
from "we changed how we render," which is the difference between a real diff and a spurious one across
upgrades.

## Delivery adapters

Both consume identical rendered manifests.

**`direct`** — kelson server-side applies with field management, so ownership conflicts surface as
conflicts rather than silent overwrites. Rendered output is still versioned (implicit local repo or OCI
artifact), so direct-mode users keep diffs, history and rollback.

**`flux`** — commit to the configured repo and path, then poke `flux reconcile` rather than waiting for
the poll interval. Perceived latency ends up comparable to direct mode.

Flux is the only supported GitOps mode ([ADR-0012](adr/0012-flux-only-gitops.md)). The adapter seam
stays pluggable — an Argo CD adapter existed, was removed pre-release to keep the feature × mode test
matrix honest, and may return at "compose with existing Argo" scope.

Delivery mode is **per-environment**, not per-install. Dev can be direct while production goes through
pull requests. This is the practical shape of the hybrid decision.

## Living with flux-operator

kelson deliberately sits **above** [flux-operator](https://fluxoperator.dev/) rather than beside it:
kelson is the app-altitude author and observer (spec, capability-aware rendering, diff/dry-run, build,
app-shaped status, UI/API/MCP); flux-operator is the substrate manager (Flux install and upgrade via
`FluxInstance`, per-PR preview lifecycle via `ResourceSet` + `ResourceSetInputProvider`, Flux health
via `FluxReport`, its own Flux-altitude MCP server); the Flux controllers do the reconciling. kelson
never reconciles in GitOps mode — it writes inputs and reads status.

Four cluster shapes, one install path:

1. **Existing Flux** — adopt. kelson installs nothing, writes rendered manifests to a path an existing
   `Kustomization` already watches (a path nothing watches is a hard `not-watched` error), and reads
   status back from Kustomization conditions and `FluxReport` where available. "Where available" is a
   detection answer: `ClusterProfile.fluxOperator` records the operator's API group, so the adapter is
   told which source to read rather than discovering it by trying one
   ([#157](https://github.com/dafrie/kelson/issues/157)).
2. **No GitOps** — direct mode; or, opting in, kelson installs flux-operator, creates a `FluxInstance`
   and per-environment `GitRepository`/`Kustomization`, then behaves exactly like shape 1.
3. **Bare VPS bootstrap** — k3s, then flux-operator, then the same additive installer as shape 2.
   Bootstrap never forks the install path.
4. **PR previews** — kelson renders `ResourceSet` templates (containing kelson-rendered app manifests
   with provenance labels); flux-operator instantiates one environment per labelled pull request and
   tears it down on close. kelson surfaces these previews, it does not manage their lifecycle.

The division of labour is symmetric: kelson does not reimplement reconciliation, Flux lifecycle or
PR-preview GC — and `ResourceSet` templating (plain input substitution) does not replace kelson's
renderer, which is capability-aware via `ClusterProfile`. Integration is CR-only: flux-operator is
AGPL-3.0 and kelson is MIT, so kelson creates and reads its CRs with the dynamic client and never
imports its Go modules. For agents, kelson's MCP (app-altitude, [ADR-0008](adr/0008-mcp-surface.md))
and flux-operator's MCP (Flux-altitude) are complementary layers an agent can hold simultaneously.

### Upgrading direct → Git

Because direct mode already maintains a rendered-manifest history, `kelson eject --to-git <repo>` replays
that history into a real repository as commits and switches the environment's adapter. No re-modelling,
no export/import step.

## Preview: three levels

| Level | Mechanism | Catches | Latency |
|---|---|---|---|
| **L1 Rendered diff** | Render old spec and new spec, diff | Config mistakes, unintended blast radius | instant |
| **L2 Server-side dry-run** | `--dry-run=server` against the real API server, diff vs live | Admission webhooks, Kyverno/OPA rejections, quota violations, immutable-field errors, sidecar injection, defaulting | ~1s |
| **L3 Ephemeral live** | Preview namespace, or vcluster for CRD/cluster-scoped fidelity | Everything — real rollout, real probes, real behavior | seconds–minutes |

L2 deserves emphasis: it is the API server's own answer, not kelson's model of what the API server would
do. You see the exact object that *would* be persisted, including every mutation applied on the way in. No
PaaS in this category surfaces it, and it is the cheapest high-fidelity signal available.

L3 shares all its machinery with pull-request preview environments — built once, used for both.

## Agent surface

kelson's agent story is an API design commitment, not a chatbot.

**Capability parity, not surface parity** ([ADR-0008](adr/0008-mcp-surface.md)). The MCP server exposes no
capability the API lacks, which is what stops it becoming a privileged backdoor with its own logic. But its
*shape* is deliberately different: tools are task-shaped rather than resource-shaped, because a
sixty-endpoint API mapped one-to-one gives sixty tools and makes "why is checkout broken" cost eight round
trips. One `diagnose_application` that composes status, events, a bounded log window and the recent
revision is the same capability in a form an agent can actually use. That surface ships as `kelson-mcp`
— seven tools over stdio, unauthenticated like the server beneath it ([the MCP server](mcp.md)).

The test: *if MCP needs a **capability** the API lacks, the API is wrong. If it needs a different
**shape**, that is the point.*

Required properties of every mutating operation:

- **`dryRun: none | render | server`** — an agent can always ask "what would this do?" before doing it.
- **Idempotency keys** — a retried call after a timeout must not double-deploy.
- **Structured errors** — `{ code, resource, field, message, remediation, docsUrl }`. Never a bare 500
  with a stack trace. An agent should be able to act on the error without parsing prose.
- **Explicit versioning** — optimistic concurrency on spec version, so concurrent agent and human edits
  conflict loudly rather than last-write-wins.

**Agents are principals.** An agent gets its own identity, scoped and time-limited credentials, and its
own audit trail — never a human's token. Per-environment policy governs what it may do unsupervised:

```yaml
policy:
  development: { agents: allow }                    # deploy freely
  staging:     { agents: allow, require: dry-run }
  production:  { agents: propose-only }             # must open a PR
```

The insight that makes this coherent: **an agent proposing a change and a human opening a pull request
travel the identical path.** Render, dry-run, policy check, review, merge. The GitOps mechanism *is* the
agent safety mechanism — one thing to build, one thing to reason about.

**Observation, not polling.** A watch/SSE event stream lets agents react to outcomes. Plus structured
`explain` endpoints — "why is this application degraded?" returns causal, machine-readable data
(failing probes, recent revision, events, resource pressure), not a log dump for an LLM to guess at.

## Secrets

The spec carries **references, never values**, and the goal is that a literal fails validation in every
delivery mode. Enforcing it in the shared model validation means one rule covers CLI, UI, API and agents —
there is no second path to secure. Today's enforcement is honest-but-heuristic: a name pattern plus
URL-credential detection, which catches the common shapes and misses creatively named literals; making
the guarantee structural is tracked in [#82](https://github.com/dafrie/kelson/issues/82). Full reasoning
in [ADR-0009](adr/0009-secrets.md).

Three backends, chosen per Environment. The schema accommodates all three from day one so adding the later
two is not a breaking change.

| Backend | Where the value lives | Ships |
|---|---|---|
| `cluster` | Kubernetes Secret written by kelson via the API | v0.1 |
| `externalSecrets` | Vault, AWS/GCP/Azure secret manager | v0.2 |
| `sops` | Encrypted in Git, age keys | v0.2 |

In v0.1 rendered manifests contain only `secretKeyRef`, and **kelson does not persist secret values** —
the cluster is the store, read back masked for display. No prerequisites: `kelson secret set FOO=bar`
(command planned, M8).
The cost is that secrets are not part of the reproducible artifact, so a cluster rebuild from Git alone
will not restore them; the v0.2 backends close that for anyone who needs it.

Two failures not repeated from the category: build-time secrets go through BuildKit secret mounts rather
than build arguments or image layers, and no secret value is ever written to a log, event, error or diff.

## Data services

Delegated to CloudNativePG and a Valkey operator, with kelson owning only the application-facing
abstraction. Full reasoning in [ADR-0007](adr/0007-data-services.md).

**Presets determine topology** (the spec field is `preset:` — these are topology presets, not paid
tiers; kelson has no paid anything). CNPG recommends one database
per cluster, which is right for production and unaffordable below it — ten apps across three
environments is 30 pods and roughly 15 GB before any application code runs.

| Preset | Topology | Branchable | Cost |
|---|---|---|---|
| `shared` | `Database` CRD in a shared cluster | no | no pod |
| `small` | dedicated cluster, 1 instance | yes | 1 pod + PVC |
| `ha-small`, `ha-medium` | dedicated, 3 instances, synchronous | as source | 3 pods |
| `branch` | dedicated, bootstrapped from a source | is a branch | 1 pod + PVC |

The preset is an Environment-level override, so one Project spec covers `shared` in development and
`ha-small` in production.

**Branching** (v0.2 — [M9b](https://github.com/dafrie/kelson/milestone/18)) works on every *dedicated*
preset and is fast where storage cooperates. It does **not** work on `shared` — the default development
preset — because CSI snapshots are PVC-level and a CNPG cluster's PVC is the whole cluster, so a
database cannot be branched out of a shared cluster. Branching snapshots a dedicated source and
bootstraps a new dedicated cluster from it, and the UI says which presets are branchable at the point
of use.

| Mechanism | Requires | Speed | Storage |
|---|---|---|---|
| Thin snapshot clone | Ceph RBD, ZFS, LVM-thin | seconds | thin |
| Full snapshot clone | EBS, GCE PD, Azure Disk | minutes | full size |
| Backup restore with PITR | object store configured | minutes to hours | full size |
| Logical dump and restore | nothing | slow | full size |
| Empty plus migrations | nothing | seconds | minimal |

kelson selects from `ClusterProfile` and reports which mechanism it used, with the expected duration and
storage cost, before the operation starts. k3s ships local-path, which has no snapshot driver at all —
and restore-based branching requires an object store, which a bare-VPS bootstrap does not have either.
That path gets logical dump/restore or empty-plus-migrations until the user configures an object store
or opts into snapshot-capable storage — and the UI says so at the point of use rather than leaving it to
be discovered.

The flagship use is not preview databases. It is **branch production as of ten minutes ago, run the
migration against it, and see what breaks** — which falls straight out of CNPG's point-in-time recovery.

Two invariants: backups are configured **once per environment**, never per database; and retention is
**asymmetric** — persistent environments retain on spec removal, previews destroy on close.

## Component map

| Component | Language | Role |
|---|---|---|
| `kelson-server` | Go | API, renderer, delivery adapters, observation, policy |
| `kelson-controller` | Go, controller-runtime | Reconciles CRDs in direct mode; ClusterProfile detection |
| `kelson` (CLI) | Go | Local render/diff/deploy; single static binary |
| `kelson-mcp` | Go | MCP server; task-shaped surface with capability parity to the API ([ADR-0008](adr/0008-mcp-surface.md)) |
| `kelson-ui` | TypeScript / React | Web UI |

API transport: ConnectRPC (gRPC and HTTP/JSON from one schema definition, giving the CLI, UI and MCP
server generated clients from a single source). Auth via OIDC, mapping to Kubernetes RBAC through
impersonation where the deployment model allows, so kelson does not become a privilege-escalation
bypass around the cluster's own authorization.

## What we deliberately do not build

Postgres failover (CloudNativePG). Kafka operations (Strimzi). Certificate issuance (cert-manager).
Secret storage (external-secrets / Vault). A metrics TSDB (Prometheus). A CD reconciler (Flux, Argo CD).
Progressive delivery mechanics (Argo Rollouts, Flagger). Cluster lifecycle (k3s, Talos).

kelson's contribution is the **coherent application model across all of them**, the pure renderer, and the
agent-grade API. Every one of those is a place where an incumbent chose to reimplement and later regretted
it — most visibly Kubero, whose vendored Bitnami charts broke working installations when the upstream
catalog was withdrawn.

This constrains **what kelson promises**, not what you can run. Any workload or Helm chart is installable;
kelson runs it, routes to it and binds applications to it. It just makes no durability claim about
anything it did not provision as a managed type. See [ADR-0005](adr/0005-delegate-to-operators.md).
