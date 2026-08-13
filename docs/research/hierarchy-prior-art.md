# Resource-hierarchy prior art: Project / Application / Component (2025–2026)

Research input for the hierarchy discussion (Projects → Applications → components, with
security and team orchestration first-class). Facts surveyed 2026-08-13; sources at the end
of each section. This is a research note, not a decision — the decision, when taken, becomes
an ADR amending [ADR-0006](../adr/0006-project-application-environment.md).

## The headline findings

1. **Humanitec deleted its Application level in March 2026.** The company that popularized
   Org → App → Env ran per-level RBAC in production for years, then collapsed App and Project
   into one: the new model is Org → **Project** → Environment. kelson's current shape is what
   the incumbent converged *to*. What they kept: the **two-axis permission model** — project
   role × environment-type role, where deploying to production requires both.
2. **Hierarchical namespaces are dead.** HNC archived 2025-04-17 and removed from
   kubernetes.io docs; WG Multi-Tenancy dissolved, repos moved to `kubernetes-retired`. What
   survived is flat, machine-generated namespaces plus a control-plane object fanning policy
   out to a labelled set of them (Capsule's Tenant). Hierarchy belongs in the control plane;
   the cluster never sees it.
3. **The composite-vs-leaf test:** a middle "Application" level is justified only when it owns
   something irreducible to any single workload — Radius's Application owns the **connection
   graph** (connections generate env vars *and* IAM); Backstage's System owns an
   **encapsulation contract**. Where the middle level would only group, it lost: Argo CD kept
   Application a leaf, Score refuses to model above the workload, Humanitec removed the level.
4. **Agents are the one genuine pressure toward addressable components.** kagent (CNCF
   Sandbox): dedicated ServiceAccount per agent ("the most important blast-radius control"),
   per-agent tool subsetting, gateway kill-switch. Dapr Agents 1.0 (GA 2026-03): SPIFFE
   identity + mTLS per agent. CoSAI/NIST guidance: first-class identity per agent, no standing
   privilege. Per-component identity + capability policy is now mandatory practice for this
   component type.

## Per-system notes

### OAM / KubeVela — the cautionary composite
Application → components[] → traits[], plus app-level policies/workflow. CNCF Incubating;
alive but slow (v1.11.0 2026-07; the OAM spec repo itself frozen at v0.3.1 — the "open
standard" decayed into KubeVela's internal model). Criticisms with years of evidence:
escape-hatch collapse (`type: raw` embeds YAML and the abstraction buys nothing), permanent
component-vs-trait placement ambiguity, debugging traverses three CRs, revision objects have
real memory cost at scale. **Lesson: a composite is only worth it with a small, opinionated,
closed set of component types; the moment a generic escape hatch is needed, the level is pure
overhead — and can never be removed.**

### Radius — the composite that earns its keep
Resource Group → Environment (binds a namespace + recipes) → Application → resources with
**connections**. Namespace per (environment × application), generated. A connection drives
env-var injection *and* access grants (IAM roles on the target; SA/Role/RoleBinding on K8s);
the application graph is queryable (`rad app graph`). Active, monthly releases, still CNCF
Sandbox, Microsoft-driven. **Lesson: composite is justified when the middle level owns the
edges — connections generating config + permissions can't live on one workload.**

### Humanitec / Score
Old: Org → App → Env(type) → workloads, with app roles (Viewer/Developer/Owner) ×
environment-type roles (Deployer) — deploy = intersection of both axes; service users as
first-class principals. New (2026-03): Org → Project → Environment + resource graph; App
merged into Project; "scoped roles per project and environment; service users for agents with
least-privilege access." Score stays deliberately workload-scoped (CNCF Sandbox). Company is
small (~33 people 2026-03) but the model reset is instructive independent of scale.
**Lesson: the two-axis RBAC survived; the middle level did not.**

### Argo CD AppProject — the reference project-as-security-boundary
AppProject → Application (leaf; composition delegated to Helm/Kustomize). The project is an
**allowlist of tuples**: sourceRepos, destinations (cluster × namespace), cluster/namespaced
resource kind allowlists, destinationServiceAccounts (sync impersonation), project roles with
JWT, sync windows. Every RBAC string is literally `<project>/<app>`. Caveat: a boundary
enforced only inside Argo's control plane is theatre unless it lands as real K8s RBAC — hence
sync impersonation. **Lesson: steal the tuple-allowlist shape for any team/workspace object;
and note Argo never needed a composite Application.**

### Kubernetes multi-tenancy, 2026 state
Official guidance: namespaces + RBAC + ResourceQuota (control plane); NetworkPolicy, storage
isolation, sandboxing (data plane); exactly two models — namespace-per-tenant and virtual
control plane per tenant. HNC dead; WG dissolved. In practice: **Capsule** for soft tenancy
(Tenant CRD fans RBAC/NetPol/quota out to member namespaces; tenant-wide Resource Pools solve
quota aggregation), **vCluster** for hard tenancy. ResourceQuota is namespace-scoped, so any
project-wide budget needs an aggregating layer. **Lesson: model hierarchy in the control
plane and project it onto flat generated namespaces; a new spec level must cost zero
cluster-side hierarchy machinery.**

### Backstage System/Component/Domain (ownership reference)
Domain → System → {Component, Resource, API}; ownership is `spec.owner` → Group per entity,
never inherited by the data model; the System level is defined as an encapsulation boundary
("exposes one or several public APIs", hides internals) — and even Backstage still has open
RFCs re-litigating the middle level. **Lesson: a middle level needs a contract justification
(what does it encapsulate?), not an organisational one.**

### Agents as components
kagent (CNCF Sandbox 2025-05): `Agent`/`ModelConfig`/`MCPServer` CRDs; per-agent SA + minimal
Role; per-agent tool subsetting even when the MCP server exposes more; agentgateway enforces
per-tool authz, A2A RBAC, kill-switch. Dapr Agents 1.0 (2026-03): durable execution +
SPIFFE-based identity, mTLS between agents. NIST AI Agent Standards Initiative (2026-02) and
CoSAI agentic IAM guidance (2026-03): first-class identity per agent, abolish standing
privilege. **Lesson: agent components require their own identity and their own policy
attachment point — a functional (not organisational) reason for components to be addressable.**

## Synthesis

- **RBAC boundary:** project/tenant level, crossed with environment class — two axes, not one
  level. Nobody puts primary RBAC on the individual component, except agents (per-component
  identity is mandatory there).
- **Namespace boundary:** one namespace per (ownership-unit × environment), flat, generated.
  Quota aggregation above it needs a controller (Capsule Resource Pools) — a cost any per-app
  namespace split inherits.
- **Composite vs leaf:** composite only when the middle level owns the edges (connection
  graph → config + permissions) or an encapsulation contract. kelson's Project already owns
  its binding graph — by the Radius test the existing composite qualifies; the open question
  is unifying the leaf (`applications` + `services` → one `components:` list with kinds,
  including `agent` with per-component identity/tool policy), not adding a level above it in
  spec. A team/workspace level, if added, is a control-plane object shaped like Argo's
  AppProject (tuple allowlists, two-axis roles), never a YAML document in an app repo and
  never cluster-side hierarchy.

Sources: kubevela.io / github.com/kubevela/kubevela / github.com/oam-dev/spec ·
docs.radapp.io / blog.radapp.io · developer.humanitec.com (RBAC, service users, "The all new
Platform Orchestrator", new-orchestrator core concepts) · score-spec/spec ·
argo-cd.readthedocs.io (projects, RBAC, sync impersonation) · kubernetes.io multi-tenancy ·
github.com/kubernetes-retired/{multi-tenancy,hierarchical-namespaces} ·
projectcapsule/capsule · vcluster.com · backstage.io system model ·
cncf.io/projects/kagent · kagent.dev · CNCF Dapr Agents 1.0 GA announcement ·
Microsoft Security blog (least privilege for AI agents, 2026-07).
