# Architecture

## Four planes

kelson separates cleanly into four planes. The boundaries are load-bearing — most of the design's
properties fall out of keeping them honest.

```
┌──────────────────────────────────────────────────────────────────────┐
│  AUTHORING          CLI · Web UI · HTTP API · MCP server             │
│                     four peer clients of one typed API               │
│                     intent → Project / Environment custom resources  │
└─────────────────────────────┬────────────────────────────────────────┘
                              │  kelson.dev/v1alpha1 CRs, validated
┌─────────────────────────────▼────────────────────────────────────────┐
│  RENDERING          pure function: (spec, ClusterProfile) → manifests │
│                     no cluster · no network · no clock · no DB        │
│                     deterministic · golden-file tested                │
└─────────────────────────────┬────────────────────────────────────────┘
                              │  plain Kubernetes YAML + provenance
┌─────────────────────────────▼────────────────────────────────────────┐
│  DELIVERY           one spine, run by kelson-controller               │
│                     immutable OCI artifact per generation             │
│                     → Flux OCIRepository + Kustomization              │
└─────────────────────────────┬────────────────────────────────────────┘
                              │
┌─────────────────────────────▼────────────────────────────────────────┐
│  OBSERVATION        Flux conditions + live workloads, correlated via  │
│                     kelson.dev/revision → Environment.status          │
└──────────────────────────────────────────────────────────────────────┘
```

### Why the rendering plane must stay pure

Purity is not aesthetics. It buys, concretely:

- **The artifact is the revision.** Delivery is *publish these exact bytes and point a reconciler at
  them* ([ADR-0028](adr/0028-delivery-spine.md)), so an unchanged spec produces a digest the registry
  already holds and a rollback is a pointer move to bytes that cannot have changed. Determinism is the
  precondition for all of that.
- **Trustworthy previews.** A diff is only meaningful if rendering the same spec twice gives the same
  bytes.
- **Cheap, fast tests.** The bulk of kelson's correctness lives in golden files, not in an integration
  suite that needs a cluster.
- **A real escape hatch.** Because output is plain YAML, users can always inspect, patch or take it and
  leave.

The renderer stays pure **Go**: CUE and timoni were evaluated as the engine during the rebuild and
rejected, because the error taxonomy, the gate table and the golden corpus are the assets and an engine
swap surrenders all three ([ADR-0029](adr/0029-renderer-stays-go.md)). `kind: timoni` is reserved as a
future delegation kind ([#230](https://github.com/dafrie/kelson/issues/230)), not built.

`ClusterProfile` is an input rather than an ambient lookup precisely to preserve purity — see below.

## The model

Three concepts ([ADR-0006](adr/0006-project-application-environment.md), leaf renamed by
[ADR-0014](adr/0014-components.md)), and no more without a strong argument. Deliberately not an
OAM-style hierarchy: developers should deploy before learning vocabulary.

- **Project** — shared configuration and ownership. Image, common environment, service bindings, team.
- **Component** — one deployable, rendering to one workload. Every leaf of the spec is one.
- **Environment** — where it runs and what differs there.

`Project` and `Environment` are **custom resources** in `kelson.dev/v1alpha1`, with status subresources,
reconciled by `kelson-controller` ([ADR-0027](adr/0027-crd-native-control-plane.md)). The documents below
are what you write and what the API server stores; `kubectl get environments -A` answers "what is kelson
running here" without kelson.

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
render since [#89](https://github.com/dafrie/kelson/issues/89); what a field cannot honour it refuses by
name rather than accepting and rendering nothing ([#141](https://github.com/dafrie/kelson/issues/141)) —
`tools:` on a `kind: agent` component (M7, [#75](https://github.com/dafrie/kelson/issues/75)),
`policy.deployers` (tenancy, [#231](https://github.com/dafrie/kelson/issues/231)) and, since
[ADR-0028](adr/0028-delivery-spine.md), `components[].release` pending its Flux-native design
([#227](https://github.com/dafrie/kelson/issues/227)). See [the model reference](model.md) for the
current table.

Environments carry what differs between deployments — namespace, domain suffix, replica and resource
overrides, secrets backend, policy. Projects stay environment-agnostic; environments stay
project-agnostic. There is no delivery mode to choose and no `cluster:` to name: one spine
([ADR-0028](adr/0028-delivery-spine.md)), one cluster ([ADR-0031](adr/0031-single-cluster-single-tenant.md)).

**Why the abstraction stays thin.** Every concept kelson invents is one that neither a new developer nor an
LLM has seen before. Kubernetes primitives are in the training data; `kelson.dev` concepts are not. So the
spec models what genuinely recurs and hands everything else to `overlays`, which is a better answer to
long-tail requests than adding fields until the surface is overwhelming.

## ClusterProfile — adopt, don't install

On install and periodically after, kelson probes the cluster and records what it finds. It is an input
to the renderer and, since [ADR-0028](adr/0028-delivery-spine.md), an input to the controller's
reconcile — step 2 of the loop below reads it, so what the cluster can do is decided in the process
that has cluster access and never in the renderer:

```yaml
kubernetes:      { version: v1.31.2, platform: k3s, nodeArchitectures: [amd64] }
gatewayAPI:      { version: v1.6.0, classes: [envoy] }
ingressClasses:  [{ name: nginx, controller: k8s.io/ingress-nginx, default: true }]
certManager:     { clusterIssuers: [letsencrypt-prod] }
externalSecrets: { clusterSecretStores: [vault-backend], secretStores: [{ name: team-vault, namespace: shop-staging }] }
prometheus:      { serviceMonitor: true, podMonitor: true }
flux:            { version: v2.4.0 }
fluxOperator:    {}
policyEngines:   [{ name: kyverno, version: v1.13.0 }]
metricsServer:   {}
incomplete:
  - field: storageClasses
    reason: 'list storageclasses.storage.k8s.io denied'
```

Absent components are simply missing from the document — `cnpg` is not installed here.
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
    kelson.dev/project: checkout
    kelson.dev/component: web
    kelson.dev/environment: production
  annotations:
    kelson.dev/spec-hash:        sha256:…   # normalized spec
    kelson.dev/revision:         <tag>      # the artifact tag: <generation>-<spec-hash-short>
    kelson.dev/renderer-version: 0.4.1
```

`kelson.dev/project` and `kelson.dev/component` are the labels the renderer writes and Deployments
select on, and the spec's vocabulary and the cluster's are the same words
([ADR-0032](adr/0032-finish-the-component-rename.md)). The rename has no migration path and does not
need one: a Deployment's selector is immutable, so a workload deployed before it is deleted and
redeployed rather than updated in place — and the apply that refuses says exactly that
(`delivery/immutable-field`, [docs/delivery.md](delivery.md)).

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

## The delivery spine

There is one delivery path and it is a controller loop ([ADR-0028](adr/0028-delivery-spine.md)).
`kelson-controller` reconciles an `Environment` in six steps:

```
1 validate   internal/model/validate.go           invalid → Ready=False, status.validationErrors[]
2 detect     ClusterProfile                      what this cluster can do
3 render     (spec, ClusterProfile) → manifests   pure, unchanged
4 publish    immutable OCI artifact              <registry>/kelson/<project>-<env>:<gen>-<hash8>
5 ensure     OCIRepository + Kustomization       server-side applied into kelson-system
6 observe    Flux conditions + workloads         → statemachine → Environment.status
```

**The artifact is the revision.** The tag is the `Environment`'s `.metadata.generation` — allocated by
the API server, monotonic, bumped only on spec change — plus the first eight hex of the resolved spec
hash, so a tag is both sequence- and content-identified. It is written once and never rewritten, in
exactly the Flux OCI artifact form PR previews already publish
([ADR-0017](adr/0017-pr-previews.md) decision 10): one publisher, one media type, one determinism test.

**kelson owns exactly two Flux objects per environment**, in `kelson-system` rather than the workload
namespace, applied with server-side apply under field manager `kelson-controller`: an `OCIRepository`
pinned to the tag just pushed, and a `Kustomization` with `path: ./`, `prune: true`, and
`spec.decryption` when the environment's secret backend is `sops`. kustomize-controller then owns apply
ordering, health assessment, pruning, drift correction and retry — five things kelson stops having
opinions about.

The three verbs follow from that shape rather than adding machinery:

| Verb | What it is |
|---|---|
| history | the registry's tag list, mirrored bounded (20) into `Environment.status.history[]` |
| rollback | the annotation `kelson.dev/rollback-to: <revision>`, which repoints the `OCIRepository` and **suspends re-rendering** until it is removed or the spec is edited |
| promote | a patch to the target `Environment`'s per-component image pin, stamped `kelson.dev/promoted-from: <env>@<revision>` |

**Flux is a hard requirement, and a registry is one too.** [ADR-0001](adr/0001-hybrid-state-model.md)
sold "works without adopting GitOps" and that is withdrawn. [ADR-0030](adr/0030-flux-aio-install.md)
answers the first cost by making the Flux install small; the second — someone deploying a pre-built
public image who now needs somewhere to push artifacts — is the sharpest new edge in the rebuild, and
`ExternalArtifact` ([#228](https://github.com/dafrie/kelson/issues/228)) is the only recorded way out.

**Every deployment is already ejected.** `kelson eject` is deleted because it has nothing left to do:
each revision *is* an immutable artifact of standard manifests, readable with tools that are not kelson.

```sh
flux pull artifact oci://<registry>/kelson/<project>-<env>:<rev> --output ./manifests
kelson render -f project.yaml --env production        # the same bytes, offline
```

> **Transition (R1/R2, [#224](https://github.com/dafrie/kelson/issues/224) /
> [#225](https://github.com/dafrie/kelson/issues/225)).** The controller is being scaffolded now. Until
> R1 lands, the code still carries `Environment.spec.delivery{mode: direct|flux}`, the direct adapter
> that server-side applies from `kelson-server`, the go-git writer, and the ConfigMap spec and history
> stores. They are on the deletion list of ADR-0028 decision 9 and ADR-0027 decision 7, not part of the
> design this page describes.

## Living with flux-operator

kelson deliberately sits **above** [flux-operator](https://fluxoperator.dev/) rather than beside it:
kelson is the app-altitude author and observer (spec, capability-aware rendering, diff/dry-run, build,
app-shaped status, UI/API/MCP); the Flux controllers do the reconciling; flux-operator is a substrate
manager for the clusters that want one. kelson never reconciles — it publishes inputs and reads status.

Since [ADR-0030](adr/0030-flux-aio-install.md), flux-operator is **optional**. It earns its place for
one feature: `ResourceSet` and `ResourceSetInputProvider` are its CRDs and PR previews are built on
them. Nothing else needs it.

Three cluster shapes, one install path:

1. **Existing Flux** — adopt. kelson installs nothing, whatever the Flux came from (`flux bootstrap`,
   flux-operator, flux-aio, a vendor's distribution); the `ClusterProfile`'s `flux` finding is what
   matters, not its provenance. The controller publishes artifacts and creates its own two objects, so
   there is no path for an operator to wire up and no `not-watched` failure to hit.
2. **No Flux** — `kelson install` offers **flux-aio**: all Flux controllers in one pod, pre-rendered at
   kelson release time from a pinned timoni module, shipped as an ordinary pinned catalog row
   ([ADR-0030](adr/0030-flux-aio-install.md)). Full Flux via flux-operator stays available as an
   explicit choice. Offers, never assumes ([ADR-0003](adr/0003-install-model.md)).
3. **PR previews** — flux-operator is added, and kelson renders a `ResourceSetInputProvider` and a
   `ResourceSet` whose template instantiates one `OCIRepository` + `Kustomization` per change request.
   flux-operator owns the lifecycle; kelson supplies the manifests and surfaces what is running.

The division of labour is symmetric: kelson does not reimplement reconciliation, Flux lifecycle or
PR-preview GC — and `ResourceSet` templating (plain input substitution) does not replace kelson's
renderer, which is capability-aware via `ClusterProfile`. That asymmetry is also why `ResourceSet` is
not the spine: it is an AGPL-3.0 CRD flux-aio does not ship, and it is a template engine in the cluster
([ADR-0028](adr/0028-delivery-spine.md) rationale). Integration is CR-only — kelson creates and reads
flux-operator's CRs with the dynamic client and never imports its Go modules. For agents, kelson's MCP
(app-altitude, [ADR-0008](adr/0008-mcp-surface.md)) and flux-operator's MCP (Flux-altitude) are
complementary layers an agent can hold simultaneously.

## Preview: three levels

| Level | Mechanism | Catches | Latency |
|---|---|---|---|
| **L1 Rendered diff** | Render old spec and new spec, diff | Config mistakes, unintended blast radius | instant |
| **L2 Server-side dry-run** | `--dry-run=server` against the real API server, diff vs live | Admission webhooks, Kyverno/OPA rejections, quota violations, immutable-field errors, sidecar injection, defaulting | ~1s |
| **L3 Ephemeral live** | Preview namespace, or vcluster for CRD/cluster-scoped fidelity | Everything — real rollout, real probes, real behavior | seconds–minutes |

L2 deserves emphasis: it is the API server's own answer, not kelson's model of what the API server would
do. You see the exact object that *would* be persisted, including every mutation applied on the way in. No
PaaS in this category surfaces it, and it is the cheapest high-fidelity signal available. That includes
admission control: a Kyverno, Gatekeeper or `ValidatingAdmissionPolicy` denial is surfaced at preview time
naming the policy and quoting its message, and the webhooks a dry-run *cannot* reach are named too —
[the delivery plane](delivery.md#preview-admission-rejections-and-what-the-dry-run-cannot-see-45) has both
halves.

L3 shares all its machinery with pull-request preview environments — built once, used for both.

## Agent surface

kelson's agent story is an API design commitment, not a chatbot.

**Capability parity, not surface parity** ([ADR-0008](adr/0008-mcp-surface.md)). The MCP server exposes no
capability the API lacks, which is what stops it becoming a privileged backdoor with its own logic. But its
*shape* is deliberately different: tools are task-shaped rather than resource-shaped, because a
sixty-endpoint API mapped one-to-one gives sixty tools and makes "why is checkout broken" cost eight round
trips. One `diagnose_component` that composes status, events, a bounded log window and the recent
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
# on each Environment's spec.policy
development: { agents: allow }                    # deploy freely
staging:     { agents: allow, require: [dry-run] }
production:  { agents: propose-only }             # no live mutation without a human
```

The insight that makes this coherent: **an agent proposing a change and a human opening a pull request
travel the identical path.** Render, dry-run, policy check, review, merge — against the repository where
the Project and Environment documents live, which is where kelson recommends they live
([ADR-0027](adr/0027-crd-native-control-plane.md) decision 6). Review of the authored document *is* the
agent safety mechanism — one thing to build, one thing to reason about.

Built as of [ADR-0025](adr/0025-agent-policy.md): the block is read from the *stored* spec and enforced
in the API server, so a modified client changes nothing, and blast-radius limits (`maxReplicas`,
`protect`, `forbid`) sit beside `agents:`. What is not built yet is the last step of the sentence
above — `propose-only` today refuses the mutation and points at the proposal (`dry_run=RENDER`, or
`Diff`) rather than opening the pull request itself. Opening it needs a forge credential, which is a
class of secret [ADR-0009](adr/0009-secrets.md) deliberately keeps out of the control plane; ADR-0028
removes the git writer that would otherwise have carried it.

**Observation, not polling.** A watch/SSE event stream lets agents react to outcomes. Plus structured
`explain` endpoints — "why is this component degraded?" returns causal, machine-readable data
(failing probes, recent revision, events, resource pressure), not a log dump for an LLM to guess at.
That one is built: `ExplainService.Explain`, `kelson explain` and the `WHY` section of
`diagnose_component` are three surfaces over one capability in `internal/explain`, and every cause it
returns carries a stable code, a confidence, the evidence behind it and — where the recorded history
shows one — the revision that introduced the change being blamed
([ADR-0023](adr/0023-explain-structured-causes.md)).

## Secrets

The spec carries **references, never values**, and the goal is that a literal fails validation
everywhere. Enforcing it in the shared model validation means one rule covers CLI, UI, API and agents —
there is no second path to secure. Today's enforcement is honest-but-heuristic: a name pattern plus
URL-credential detection, which catches the common shapes and misses creatively named literals; making
the guarantee structural is tracked in [#82](https://github.com/dafrie/kelson/issues/82). Full reasoning
in [ADR-0009](adr/0009-secrets.md).

Three backends, chosen per Environment. The schema accommodates all three from day one so adding the later
two is not a breaking change.

| Backend | Where the value lives | Ships |
|---|---|---|
| `cluster` | Kubernetes Secret written by kelson via the API | v0.1 |
| `externalSecrets` | Vault, AWS/GCP/Azure secret manager — kelson renders an `ExternalSecret` and never holds the value ([ADR-0020](adr/0020-external-secrets.md)) | v0.1 |
| `sops` | Encrypted with age **inside the published artifact**, decrypted in-cluster by kustomize-controller — kelson encrypts in memory and holds no private key ([ADR-0022](adr/0022-sops-age.md), transport amended by [ADR-0028](adr/0028-delivery-spine.md) §7) | v0.1 |

Rendered manifests contain only `secretKeyRef` under all three, and **kelson does not persist secret
values** — the store is the cluster, the artifact or your secret manager, and kelson reads back masked
for display. No prerequisites for the default: `kelson secret set checkout-db url=…`.

The cost of the `cluster` backend is that secrets are not part of the reproducible artifact, so a cluster
rebuilt from the artifact alone will not have them. `sops` closes that — the encrypted Secret ships in
the artifact beside the workloads that reference it, the `Kustomization` kelson writes carries the
decryption block, and the only thing that has to survive outside is one age identity. See
[Secrets](secrets.md).

Two failures not repeated from the category: build-time secrets go through BuildKit secret mounts rather
than build arguments or image layers, and no secret value is ever written to a log, event, error or diff.

## Data services

Delegated to CloudNativePG for `kind: postgres` and to
[valkey-io/valkey-operator](https://github.com/valkey-io/valkey-operator) for `kind: valkey`, with
kelson owning only the component-facing abstraction. Full reasoning in
[ADR-0007](adr/0007-data-services.md) and, for the choice of Valkey operator and what it costs,
[ADR-0015](adr/0015-valkey-operator.md). What each preset actually renders is
[docs/data-services.md](data-services.md).

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
| `kelson-controller` | Go, controller-runtime | **The spine.** Reconciles `Project` and `Environment`: validate, detect, render, publish, ensure the Flux pair, observe ([ADR-0028](adr/0028-delivery-spine.md)). Being scaffolded now ([#224](https://github.com/dafrie/kelson/issues/224)) |
| `kelson-server` | Go | **A stateless façade over the CRs.** Same ConnectRPC wire surface; reads and writes custom resources with server-side apply, reads `Environment.status` instead of computing it ([ADR-0027](adr/0027-crd-native-control-plane.md) decision 6). Also serves the web UI, builds, secrets, agent identities and the audit trail |
| `kelson` (CLI) | Go | Local render/diff/deploy, cluster-direct verbs; single static binary |
| `kelson-mcp` | Go | MCP server; task-shaped surface with capability parity to the API ([ADR-0008](adr/0008-mcp-surface.md)) |
| `kelson-ui` | TypeScript / React | Web UI. Shipped — served from the same listener as the API |

The controller is built on controller-runtime as a **library** and not on kubebuilder: the repository
already has three generated-artifact pipelines sharing one convention (committed output, drift test,
`make` target), and the CRD schemas come out of the same `internal/schemagen` reflection that produces
`schema/*.json` rather than from a second marker-driven generator. Spec structs stay in
`internal/model`; `api/kelson/v1alpha1` holds only the CR wrappers, the status types and generated
deepcopy ([ADR-0027](adr/0027-crd-native-control-plane.md) decisions 2–4).

API transport: ConnectRPC (gRPC and HTTP/JSON from one schema definition, giving the CLI, UI and MCP
server generated clients from a single source). Auth via OIDC, mapping to Kubernetes RBAC through
impersonation where the deployment model allows, so kelson does not become a privilege-escalation
bypass around the cluster's own authorization.

## Tracked, not built

Recorded so the next design conversation starts from the record rather than rediscovering it. None of
these exist, and each has an issue rather than a stub in the schema.

| Direction | Why it is not here | Tracked |
|---|---|---|
| Workspace / Team, tenancy and `policy.deployers` | Tenancy needs a human subject kelson does not model; built on "holds the shared password" it would be authorization theatre ([ADR-0031](adr/0031-single-cluster-single-tenant.md), [#84](https://github.com/dafrie/kelson/issues/84)) | [#231](https://github.com/dafrie/kelson/issues/231) |
| Multi-cluster placement | Not a field: it changes where credentials live, what a `ClusterProfile` is and where the controller runs. `Environment.spec.cluster` is deleted rather than kept as a guessed spelling | [#232](https://github.com/dafrie/kelson/issues/232) |
| Admission webhook for the CRDs | Costs a serving certificate, a `CABundle` to rotate and a failure mode where broken kelson rejects unrelated applies. Generated CEL rules on the CRD schema are what make deferring it tolerable | [#229](https://github.com/dafrie/kelson/issues/229) |
| `kind: timoni` | Reserved, mirroring `kind: helm`. Timoni is a packaging layer *below* kelson's authoring layer, and there is no GA in-cluster timoni controller to delegate to ([ADR-0029](adr/0029-renderer-stays-go.md)) | [#230](https://github.com/dafrie/kelson/issues/230) |
| `ExternalArtifact` (Flux ≥2.7) | The registry-less path. An addition to the spine, not a replacement — and an unused second publishing path is a second publishing path to test | [#228](https://github.com/dafrie/kelson/issues/228) |
| Flux-native release hooks | Two `Kustomization`s with `dependsOn`. Possible only now that kelson owns the Kustomization; `components[].release` is a validated refusal until it is built | [#227](https://github.com/dafrie/kelson/issues/227) |

## What we deliberately do not build

Postgres failover (CloudNativePG). Kafka operations (Strimzi). Certificate issuance (cert-manager).
Secret storage (external-secrets / Vault). A metrics TSDB (Prometheus). A CD reconciler (Flux, Argo CD).
Progressive delivery mechanics (Argo Rollouts, Flagger). Cluster lifecycle (k3s, Talos).

kelson's contribution is the **coherent application model across all of them**, the pure renderer, and the
agent-grade API. Every one of those is a place where an incumbent chose to reimplement and later regretted
it — most visibly Kubero, whose vendored Bitnami charts broke working installations when the upstream
catalog was withdrawn.

This constrains **what kelson promises**, not what you can run. Any workload or Helm chart is installable;
kelson runs it, routes to it and binds components to it. It just makes no durability claim about
anything it did not provision as a managed type. See [ADR-0005](adr/0005-delegate-to-operators.md).
