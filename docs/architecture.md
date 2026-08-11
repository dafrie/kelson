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
│                     ├── flux      commit → GitRepository → Kustomize  │
│                     └── argocd    commit → Application → sync         │
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

  env:                      # shared by every Application
    LOG_LEVEL: info
    DATABASE_URL:
      from: { service: db, key: uri }   # binding, never a literal

  services:
    - name: db
      type: postgres
      plan: ha-small        # → CloudNativePG Cluster with PITR

  applications:
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
kind: ClusterProfile
detected:
  gatewayAPI:     { present: true,  version: v1.6.0, classes: [envoy] }
  ingressClasses: [nginx]
  certManager:    { present: true,  clusterIssuers: [letsencrypt-prod] }
  externalSecrets:{ present: true,  stores: [vault-backend] }
  prometheus:     { present: true,  crd: ServiceMonitor }
  cnpg:           { present: false }
  flux:           { present: true,  version: v2.x }
  argocd:         { present: false }
  policy:         { kyverno: true }
  metricsServer:  { present: true }
```

The renderer takes this as an input. Same spec, different cluster, correct output: `HTTPRoute` where
Gateway API exists and `Ingress` where it doesn't; a `Certificate` where cert-manager is present rather
than kelson running its own ACME client; a `ServiceMonitor` only if something will read it.

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

All three consume identical rendered manifests.

**`direct`** — kelson server-side applies with field management, so ownership conflicts surface as
conflicts rather than silent overwrites. Rendered output is still versioned (implicit local repo or OCI
artifact), so direct-mode users keep diffs, history and rollback.

**`flux`** — commit to the configured repo and path, then poke `flux reconcile` rather than waiting for
the poll interval. Perceived latency ends up comparable to direct mode.

**`argocd`** — commit, then trigger sync via the Argo API. kelson reads Argo's computed sync and health
status rather than recomputing its own.

Delivery mode is **per-environment**, not per-install. Dev can be direct while production goes through
pull requests. This is the practical shape of the hybrid decision.

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

**The MCP server is a thin adapter over the same typed API the UI uses.** If it needs endpoints the UI
doesn't have, that is a signal the API is wrong.

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

A hard constraint that falls out of Git mode: **plaintext secrets can never be committed.** Two supported
paths, both keeping cleartext out of Git and out of kelson's own storage:

1. **external-secrets** (preferred) — kelson renders `ExternalSecret` resources referencing your Vault,
   AWS/GCP secret manager or similar. kelson never holds the value.
2. **SOPS + age** — encrypted at rest in the repo, decrypted in-cluster by the Flux SOPS integration or
   an equivalent.

Direct mode may write `Secret` resources to the cluster directly, but the spec never carries literals —
only references. This is enforced by the renderer, not by convention.

## Data services

Delegated to CloudNativePG and a Valkey operator, with kelson owning only the application-facing
abstraction. Full reasoning in [ADR-0007](adr/0007-data-services.md).

**Plans determine topology.** CNPG recommends one database per cluster, which is right for production and
unaffordable below it — ten apps across three environments is 30 pods and roughly 15 GB before any
application code runs.

| Plan | Topology | Branchable | Cost |
|---|---|---|---|
| `shared` | `Database` CRD in a shared cluster | no | no pod |
| `small` | dedicated cluster, 1 instance | yes | 1 pod + PVC |
| `ha-small`, `ha-medium` | dedicated, 3 instances, synchronous | as source | 3 pods |
| `branch` | dedicated, bootstrapped from a source | is a branch | 1 pod + PVC |

Plan is an Environment-level override, so one Project spec covers `shared` in development and `ha-small`
in production.

**Branching** works everywhere and is fast where storage cooperates. CSI snapshots are PVC-level and a CNPG
cluster's PVC is the whole cluster, so a database cannot be branched out of a shared cluster — branching
snapshots a dedicated source and bootstraps a new dedicated cluster from it.

| Mechanism | Requires | Speed | Storage |
|---|---|---|---|
| Thin snapshot clone | Ceph RBD, ZFS, LVM-thin | seconds | thin |
| Full snapshot clone | EBS, GCE PD, Azure Disk | minutes | full size |
| Backup restore with PITR | object store configured | minutes to hours | full size |
| Logical dump and restore | nothing | slow | full size |
| Empty plus migrations | nothing | seconds | minimal |

kelson selects from `ClusterProfile` and reports which mechanism it used, with the expected duration and
storage cost, before the operation starts. k3s ships local-path, which has no snapshot driver at all, so
the bootstrap path gets restore-based branching until a user opts into snapshot-capable storage — and the
UI says so at the point of use rather than leaving it to be discovered.

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
| `kelson-mcp` | Go | MCP server; thin adapter over the API |
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
