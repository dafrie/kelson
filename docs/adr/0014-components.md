# ADR-0014: One `components:` list, per-component identity

- **Status:** Accepted (2026-08-14: implemented — `spec.components` with the closed kind enum is the
  model's only leaf list and every component renders its own ServiceAccount. Decision B's Workspace
  shape is carried forward, still unbuilt, by [ADR-0031](0031-single-cluster-single-tenant.md).)
- **Date:** 2026-08-13

> Amends [ADR-0006](0006-project-application-environment.md), which named the leaf *Application* and
> put data services beside it in a second list. The three-level shape ADR-0006 chose — Project →
> leaf → Environment — is confirmed here, not replaced. The evidence base is
> [docs/research/hierarchy-prior-art.md](../research/hierarchy-prior-art.md).

## Context

The spec has two leaf lists. `spec.applications` holds deployables and `spec.services` holds managed
data services, and an Environment mirrors both with `spec.applications` overrides and
`spec.services` preset overrides. The split is not a modelling insight; it is an accident of the
order the features landed (ADR-0006 first, [ADR-0007](0007-data-services.md) after).

The cost was invisible while the two lists held unlike things. Three pressures made it visible.

**The lists are already converging.** A data service is bound to by name, overridden per environment
by name, and rendered into the same namespace beside the workloads. It differs from an application in
what it renders to, not in how it is addressed. Every surface that walks one list walks the other
immediately afterwards, with a parallel override lookup and a parallel duplicate-name check.

**Agents do not fit either list.** An agent is a long-running process with an image, so it is
application-shaped; but the thing that distinguishes it is a policy attachment point — which tools it
may call, under which identity — which is service-shaped. Adding `spec.agents` as a third list would
have been the same accident a third time.

**The hierarchy question was open.** A survey of resource-hierarchy prior art (2026-08-13) was
commissioned to answer whether kelson needs a level *above* Project — a Workspace or Team — and
whether the leaf should stay as it is. The findings pointed the other way: the level above is a
control-plane concern, and the pressure is on the leaf.

Three findings carried the decision.

1. **The composite-vs-leaf test.** A middle level earns its place only when it owns something no
   single workload can own. Radius's Application owns the *connection graph*: a connection generates
   both environment variables and access grants, which cannot live on either endpoint. Backstage's
   System owns an encapsulation contract. Where the middle level would only *group*, it lost — Argo CD
   kept Application a leaf and delegated composition to Helm/Kustomize; Score refuses to model above
   the workload. kelson's Project already owns its binding graph: `{from: {service, key}}` is an edge
   whose two endpoints are both leaves, and the Project is where it lives. By this test the existing
   composite qualifies and needs no help; the open question was only what the leaves are called.
2. **Humanitec removed its middle level in March 2026.** The company that popularised
   Org → App → Env ran per-level RBAC in production for years, then collapsed App into Project:
   Org → Project → Environment. What survived the reset was the *two-axis permission model* (project
   role × environment-type role), not the level. kelson's current shape is what the incumbent
   converged to, which is a reason to stop adding levels, not to add one.
3. **Hierarchical namespaces are dead.** HNC was archived 2025-04-17 and removed from the Kubernetes
   docs; WG Multi-Tenancy dissolved. What survived is flat, machine-generated namespaces plus a
   control-plane object fanning policy out to a labelled set of them (Capsule's Tenant). Hierarchy
   belongs in the control plane; the cluster never sees it.

And one finding created a new requirement rather than settling an old one:

4. **Agents are the first component type with a hard identity requirement.** kagent (CNCF Sandbox)
   gives every agent a dedicated ServiceAccount and calls it "the most important blast-radius
   control", subsets tools per agent even when the MCP server exposes more, and puts a kill-switch in
   the gateway. Dapr Agents 1.0 (GA 2026-03) gives each agent a SPIFFE identity with mTLS. CoSAI's
   agentic IAM guidance and the NIST AI Agent Standards Initiative both require first-class identity
   per agent and no standing privilege. Per-component identity is not a nice-to-have for this
   component type; it is the current practice.

## Decision

**A.** `spec.applications` and `spec.services` unify into one `spec.components` list. A component has
a `kind` from a closed set: `service`, `worker`, `cron`, `agent`, `postgres`, `valkey`.

- **Workload kinds stay derived.** `port:` → `service`, `schedule:` → `cron`, neither → `worker`,
  exactly as ADR-0006 decided. The happy path gains no vocabulary: the minimum viable Project is the
  same document with one key renamed.
- **`kind:` is the explicit form, and it wins.** When written it is validated against the enum *and*
  against the component's shape: `kind: service` needs a port, `kind: cron` needs a schedule, and a
  `kind:` that contradicts the shape is a validation error rather than a silent override.
- **Data kinds are always explicit.** `kind: postgres` replaces `type: postgres`; there is nothing to
  derive a database from, and no shape that could accidentally become one. `preset:` stays and is
  meaningful only on a data kind, as `port:`/`schedule:` are meaningful only on a workload kind —
  each is an error on the other side rather than a field that quietly does nothing
  ([#141](https://github.com/dafrie/kelson/issues/141)).
- **Environment overrides unify the same way.** One `spec.components` override list, matched by name,
  carrying the workload overrides (`replicas`, `resources`, `env`) or the data override (`preset`)
  according to the kind of the component it names. Precedence rules P1–P6 are unchanged in substance:
  P1/P2/P3 apply to workload components and P5 to data components, exactly as they did when the two
  lived in separate lists.

**B.** `Project` remains the composite. No level is added above it in the spec.

A Workspace or Team object is future *control-plane* direction, and this ADR records only its shape
so that later work does not have to relitigate the question: an object like Argo CD's AppProject — a
tuple allowlist (source repos, destinations as cluster × namespace, resource-kind allowlists) with
two-axis roles (project role × environment-class role, where deploying to production requires both,
per Humanitec's surviving half). It is never a YAML document in an application repo and never
cluster-side hierarchy. Nothing in this ADR builds it.

**C.** `kind: agent` lands thin. It carries one agent-specific field, `tools:`, and that field is
*validated but gated* `schema/not-implemented` against
[#75](https://github.com/dafrie/kelson/issues/75) until a policy engine exists to enforce it. An
agent component renders today as a worker-shaped Deployment with its own identity. No `model:`, no
`mcpServers:`, no `memory:` — the kinds of fields that would have to be guessed at now and lived with
later.

**D.** **Every component gets its own ServiceAccount.** Not only web services, as today.

- It is the blast-radius control the agent research names first, and an agent without its own
  identity has no attachment point for the tool policy of decision C.
- Uniformity beats a rule. "Services have a ServiceAccount, workers share `default`" is a fact a
  reader must learn and an author must remember; "every component has one, named after it" is a fact
  they can derive. The uniform rule also means the identity is already in place when policy lands —
  no pod gets re-keyed later, which is what the existing ServiceAccount comment already promised for
  web services.
- Data components get none: CloudNativePG creates and owns the identities its clusters run under
  ([ADR-0005](0005-delegate-to-operators.md) — the operator owns its topology, and that includes who
  it runs as).

**E.** The binding graph is the future NetworkPolicy source. `{from: {service, key}}` already states
which components talk to which; by the Radius test that edge is exactly what a composite owns, and it
is enough to generate a default-deny NetworkPolicy set. This ADR records the direction and builds
none of it: NetworkPolicy needs a CNI capability verdict from the ClusterProfile, and a default-deny
posture applied to an adopted cluster is a change that must be opt-in and separately designed.

## Rationale

**Why one list and not three.** A closed set of component kinds is what the OAM/KubeVela experience
says is the precondition for a composite to be worth having: KubeVela's own record is years of
component-vs-trait placement ambiguity and an escape hatch (`type: raw`) that makes the abstraction
buy nothing. kelson's set is closed, small and opinionated, and the escape hatch is deliberately
*outside* the component model (`overlays:`), so the pressure that collapsed OAM's abstraction has
somewhere else to go.

The alternative — leave the two lists and add `spec.agents` as a third — was rejected because it
scales by addition. Each new list is another override list on the Environment, another duplicate-name
check, another loop in every consumer, and another decision for an author about which list a thing
belongs in. One list with a kind scales by enumeration, and the enum is a place a reader can look.

**Why the kind stays derived for workloads.** ADR-0006's derivation rule is the reason the minimum
viable Project is six lines. Making `kind:` mandatory would have made every document more explicit and
no document more correct; making it *optional and checked* keeps the six-line case and gives the
explicit form to anyone who wants their spec to say what it is — including agents generating specs,
for whom an explicit enum is easier to emit correctly than a shape rule.

**Why identity is uniform rather than per-kind.** A per-kind rule optimises the manifest count; a
uniform rule optimises the thing that is actually scarce, which is the reader's model of the system.
The cost of the uniform rule is one extra four-line object per non-web component, which no cluster
notices.

**Why not a level above Project.** All three hierarchy findings point the same way: the incumbent
removed its middle level, the Kubernetes ecosystem retired cluster-side hierarchy entirely, and the
composite test is already satisfied by the Project's ownership of the binding graph. A Workspace that
only grouped Projects would fail the same test ADR-0006 applied to Application-as-composite, and a
hierarchy level, once in every API path and URL, is very hard to remove — a cost ADR-0006 already
accepted once, deliberately, and should not accept twice.

## Consequences

**Positive.**

- One list, one override list, one duplicate-name rule, one loop per consumer. Every surface that had
  parallel application and service handling — validation, resolution, the MCP spec summary, the UI's
  parser and builder — loses a copy.
- `agent` costs a kind, not a document type. The next component type does too.
- Identity is in place before policy needs it. When #74/#75 land, the attachment point exists on every
  component and no workload has to be re-keyed.
- The spec says what a component *is* when the author wants it to, without making anyone say it.

**Negative.**

- **This is a breaking rename of the spec's most-used key**, and kelson is pre-alpha with no
  compatibility promise, so nothing is translated: `applications:` and `services:` become
  `schema/unknown-field` errors whose remediation names `components:`. Every stored spec, every
  example, every fixture and every document in this repository is rewritten in the same change.
- **Rendered output changes for unchanged input**: worker and cron components now render a
  ServiceAccount and reference it from their pod template. That is a golden-file change and is
  reviewed as one.
- **The rename stops at the cluster boundary.** Rendered resources keep the
  `kelson.dev/application` label, and Deployments keep selecting on it. A Deployment's selector is
  immutable in Kubernetes, so renaming that label would orphan every running workload on upgrade —
  the spec's vocabulary changed, the cluster's identity did not. The same applies to the log
  selector and to `list_applications` and `diagnose_application` on the MCP surface: those name a
  domain an operator already knows, not a YAML key.
- One list means one type carrying fields that are meaningful for only some of its kinds. Validation
  therefore has to say so per field — `preset` on a worker, `port` on a postgres, `tools` on anything
  but an agent are all errors. That is more validation code than two narrow types needed, and it is
  the price of the single list; the alternative is silent no-op fields, which this project has already
  decided it will not ship (#141).
- `kind: agent` renders as a worker with an identity and nothing more until #74 and #75 land. It is
  honest — the `tools:` field is refused rather than ignored — but an author who writes
  `kind: agent` today gets a Deployment, not an agent runtime, and the vocabulary may promise more
  than the milestone delivers.
- ADR-0006 is now split across two documents. Its Status line carries the amendment note, but a
  reader who finds ADR-0006 alone will read the word *Application* and the two-list model.

## Revisit when

- **#74/#75 land.** `tools:` leaves the gate table and the ServiceAccounts of decision D acquire
  Roles and RoleBindings. If per-agent policy turns out to need fields this ADR refused to guess at,
  decision C's thinness is what gets revisited, not the single list.
- **NetworkPolicy is built.** Decision E is a direction; the ADR that implements it settles the
  default-deny posture, the ClusterProfile capability and the adopted-cluster opt-in.
- **A Workspace object is actually wanted.** Decision B records a shape and builds nothing. The ADR
  that builds it must show what the level *owns* — by the same composite test applied here — not what
  it groups.
- **A component kind is proposed that is neither a workload nor a data service.** The closed set is
  what makes the single list safe; the first kind that does not fit the enum is the signal that this
  decision is under strain.
