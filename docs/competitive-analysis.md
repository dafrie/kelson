# Competitive analysis

*Researched August 2026. Figures from the GitHub API; qualitative findings from project docs, issue
trackers and Hacker News discussion rather than SEO comparison blogs.*

## The field

| Project | Stars | License | Stack | Delivery model |
|---|---|---|---|---|
| [Coolify](https://github.com/coollabsio/coolify) | 60.4k | Apache-2.0 | PHP / Laravel | Docker + Swarm; Kubernetes added late |
| [Dokploy](https://github.com/Dokploy/dokploy) | 36.5k | Apache-2.0 + `/proprietary` | TypeScript | Docker Swarm + Traefik |
| [Devtron](https://github.com/devtron-labs/devtron) | 5.6k | Apache-2.0 | Go | Kubernetes CI/CD + GitOps |
| [Kubero](https://github.com/kubero-dev/kubero) | 4.4k | GPL-3.0 | TypeScript / NestJS | Kubernetes-native, state in CRDs |
| [Canine](https://github.com/CanineHQ/canine) | 2.9k | Apache-2.0 | Ruby / Rails | Kubernetes via generated Helm |
| [KubeVela](https://github.com/kubevela/kubevela) | 7.9k | Apache-2.0 | Go | OAM abstraction engine |
| [Radius](https://github.com/radius-project/radius) | 1.7k | Apache-2.0 | Go | App graph + IaC "Recipes" |
| CapRover | — | Apache-2.0 | TypeScript | Docker Swarm |
| [Score](https://score.dev) | — | Apache-2.0 | Go | Workload spec + CLI, no runtime |

## Per-project findings

### Coolify — the incumbent

**Does well.** Best-in-class time to first deploy; a single install script and you are running. The
~300-item one-click service catalog is the real moat and the most-copied feature in the category. Owns the
full stack coherently: proxy, TLS, backups, terminal, logs. Genuinely free with no feature gating, and
the project is profitable and well-run.

**Does badly.** Docker and Swarm centric — Kubernetes arrived late and is not the model. The UI is the
source of truth, so platform state is a database blob with no reviewable history and no reproducibility.
The option surface has sprawled; a representative user report calls it "powerful, but we found the sheer
number of configuration options overwhelming." Single-server bias means multi-server is SSH fan-out. No
real RBAC or multi-tenancy, no built-in observability, and databases are plain containers without HA or
PITR.

The v5 announcement ([issue #5685](https://github.com/coollabsio/coolify/issues/5685)) is unusually candid
about the cause: the codebase "has few tests, lacks strict rules, and lacks proper structure … sometimes
updates break existing functionality — which is a no-go."

**Lesson.** Time-to-first-deploy is the price of entry. Feature velocity without architectural discipline
produces a UX that collapses under its own option count.

### Dokploy — the UX leader

**Does well.** Widely regarded as the cleanest UX in the category. Notably lighter than Coolify at idle.
Swarm-native with good Docker Compose support and a coherent multi-server story.

**Does badly.** The license is Apache-2.0 with a carved-out `/proprietary` directory, and OIDC plus audit
logs sit behind the paid enterprise tier. Users notice: *"It's too bad about the licensing (e.g. OIDC +
audit logs behind a paid enterprise license)."*

**Lesson.** This is the anti-pattern that most directly informs [ADR-0004](adr/0004-licensing.md).
Paywalling authentication and audit is paywalling security.

### Kubero — the closest architectural relative

**Does well.** Made the single best structural call in the category: two containers, all state in CRDs and
etcd, no separate database. Genuinely Kubernetes-native. Good feature coverage for its size — pipelines
with staging environments, review apps on pull requests, add-ons, 160+ templates, CLI, and an
Operator SDK based operator.

**Does badly.** No team RBAC. Built-in add-ons are not HA-ready. It vendored Bitnami Helm charts and was
badly disrupted when Broadcom removed the free Bitnami image catalog in September 2025 — a dependency
failure that broke working installations. It also implements its own pipeline and CD engine rather than
delegating to Argo or Flux, so it does not compose with an existing GitOps setup. Small community, real
bus-factor risk.

**Lesson.** CRDs-over-database is correct and kelson adopts it — literally, since
[ADR-0027](adr/0027-crd-native-control-plane.md): `Project` and `Environment` are custom resources with
status subresources. Vendoring charts for stateful software is a liability — see
[ADR-0005](adr/0005-delegate-to-operators.md), and [ADR-0030](adr/0030-flux-aio-install.md) for the one
carefully fenced exception and why it is not the same thing. And a PaaS that brings its own CD engine
cannot coexist with the GitOps tooling its target users already run, which is why kelson brings none:
Flux does the reconciling ([ADR-0028](adr/0028-delivery-spine.md)).

### Canine — the most direct competitor

Positioned as "Coolify is to a VPS as Canine is to Kubernetes." Apache-2.0, Rails, started August 2024,
~2.9k stars, actively developed.

**It is further along than its star count suggests, and further along than most comparisons state.**
Reading the source rather than the marketing:

- **13 MCP tools already shipped** (`app/mcp/tools/`): create/deploy project, create service, create add-on,
  project and add-on logs, restart, environment variable read and update, cluster kubeconfig, add-on search
- **OIDC, SAML and LDAP** SSO — all free
- **Teams** with membership and per-resource scoping
- API tokens, buildpacks, build clouds, cron schedules, volumes, domains, metrics, events
- `project_fork` and `development_environment` models, suggesting preview environments

Anyone claiming Canine lacks SSO, teams or agent support has not looked.

**What it does not have**, confirmed by code search across the repository:

| Search term | Hits |
|---|---|
| `argocd`, `flux`, `gitops`, `kustomize` | **0** |
| `server_side_apply` | 0 |
| `dry_run` | 3 (incidental) |

State lives in Postgres via Rails models. Deployment is `helm_deployment_service.rb` — it generates Helm
charts and applies them directly. `cluster_package/installer` installs its own platform components rather
than adopting what is present. There is no rendered-artifact model, no preview, and no dry-run.

**This is the project kelson must be meaningfully different from, not merely better than.** The
differentiation is architectural rather than feature-count: standard manifests delivered as immutable
artifacts instead of database state, composition with the GitOps tooling users already run instead of a
private CD engine, adoption instead of installation, and agent *safety* rather than agent *tooling*.

### Devtron — the enterprise option

Comprehensive: CI/CD, GitOps, observability and security in one dashboard, Go, Apache-2.0. But it is heavy,
complex to operate, and aimed at platform teams rather than application developers. 760 open issues
suggests a large surface under strain.

### KubeVela and Radius — the abstraction layers

KubeVela implements OAM: Applications composed of Components, Traits, Policies and Workflow. Powerful and
genuinely well-designed, but the abstraction tower is heavy and adoption has been slower than hoped.
Radius models the whole application graph including infrastructure, with platform-engineer-supplied
"Recipes" backed by IaC — well-funded, CNCF-submitted, but enterprise-shaped and not a PaaS.

**Lesson.** Both are *engines for building a platform*, not a platform. kelson should borrow the good idea
(a small declarative application model, separated from environment-specific concerns) without importing
the full OAM concept hierarchy that developers must learn before deploying anything.

### Score — the spec worth being compatible with

A CNCF sandbox workload specification, not a tool. No runtime component, no CRDs — implementations are
CLIs that translate a Score file into platform primitives. Adoption grew through 2025–2026.

**Lesson.** Not a competitor. kelson should ship a Score importer; it costs little and buys interop.

## Cross-cutting findings

### 1. Nobody has solved platform state
Every PaaS here except Kubero keeps its state in a database. Consequences: no reviewable change history,
no reproducibility, disaster recovery means restoring a DB, and uninstalling is destructive. Kubero moved
state to CRDs, which is better, but cluster state still is not a reviewable artifact.

**This is the largest unclaimed differentiator in the category** and the basis of
[ADR-0001](adr/0001-hybrid-state-model.md).

### 2. Everyone assumes greenfield
None of these adopt an existing cert-manager, Prometheus, external-secrets or Argo CD install. For
Group B users — teams already running Kubernetes properly — this makes every option a non-starter, because
adopting one means running a parallel platform stack.

### 3. Stateful workloads are systematically underserved
"Managed Postgres" almost always means a container with a PVC. No HA, no PITR, no failover. A recurring
user complaint: even on Coolify, *"I still had to spend a lot of effort maintaining PostgreSQL and Redis."*
Meanwhile CloudNativePG, Strimzi and the Valkey operators solve this properly and are free.

### 4. Agent tooling is arriving; agent safety is not
From Hacker News, March 2026: *"Currently Coolify & Dokploy are not designed for AI Agent auto deploy."*
That was true of Coolify and Dokploy, and is no longer a general claim — **Canine ships 13 MCP tools
today**, and others will follow within a year. MCP tool coverage is not a durable differentiator and
should not be treated as one.

What none of them have is what makes an agent safe to point at production: dry-run on every mutation,
idempotency keys, a structured error taxonomy an agent can branch on without parsing prose, non-human
identities with scoped and expiring credentials, and per-environment policy governing unsupervised action.
Canine's MCP tools authenticate with the same API token a human would use, so an agent's actions are
neither separately scoped nor separately attributable.

kagent (CNCF sandbox) established the pattern for agents operating *on* Kubernetes. Nothing connects that
to application delivery with guardrails.

### 5. Observability is universally absent
Every project in this table punts on metrics, logs and traces. Users bolt on Prometheus, Grafana, Loki and
Uptime Kuma by hand and wire dashboards themselves.

## Where kelson lands

| Dimension | Incumbents | kelson |
|---|---|---|
| Source of truth | Platform database | `Project` / `Environment` custom resources; the deployed state is an immutable OCI artifact of rendered manifests |
| Delivery | Platform applies | Standard manifests, published as an artifact, reconciled by Flux — one path, no adapters |
| Existing cluster components | Ignored, duplicated | Detected and adopted |
| Preview before deploy | None, or a text diff | Rendered diff → server-side dry-run → ephemeral live |
| Stateful workloads | Containers with PVCs | Upstream operators (CNPG, Strimzi, Valkey) |
| Agent tooling | Absent, or MCP tools (Canine) | Same, and not the differentiator |
| Agent safety | Human tokens, no dry-run, no policy | Scoped identities, dry-run everywhere, per-env policy |
| Uninstall | Destructive | Leaves running apps, and every revision is already a plain manifest set anyone can pull |
| SSO / RBAC / audit | Missing or paywalled | Free, always |
