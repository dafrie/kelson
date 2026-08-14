# ADR-0031: One cluster, one tenant — for now, and recorded as such

- **Status:** Accepted
- **Date:** 2026-08-14

> Settles the scope question the rebuild of [ADR-0027](0027-crd-native-control-plane.md) and
> [ADR-0028](0028-delivery-spine.md) kept running into. Carries forward, without building,
> [ADR-0014](0014-components.md) decision B's Workspace/Team shape and the evidence in
> [docs/research/hierarchy-prior-art.md](../research/hierarchy-prior-art.md).

## Context

A rebuild is where scope creep is cheapest to commit and most expensive to discover later. Three
questions kept surfacing while ADR-0027 and ADR-0028 were being settled, and each of them, answered
generously, would have changed the design:

1. **Does an `Environment` name a cluster?** `Environment.spec.cluster` exists in the model today,
   validated and then refused by `internal/model/notimplemented.go` as *"multi-cluster targeting"*
   against milestone M10. If it were real, the controller would need cluster credentials, the
   `ClusterProfile` would need to be per-target rather than per-install, and
   [ADR-0028](0028-delivery-spine.md)'s two Flux objects would have to be created somewhere other than
   where the controller runs.
2. **Does a Project belong to a Team?** [ADR-0014](0014-components.md) decision B recorded a
   Workspace/Team *shape* and deliberately built none of it. With CRDs arriving
   ([ADR-0027](0027-crd-native-control-plane.md)), the temptation to add a third CRD "while we are
   here" is at its maximum.
3. **Who may deploy to production?** `policy.deployers` is in the spec, validated, and gated against
   milestone M11 · Teams, RBAC & multi-tenancy — because, as `notimplemented.go` puts it, kelson *"has
   no notion of a human subject beyond 'holds the password'"* ([ADR-0013](0013-server-state-and-api-v0.md)
   §3, [ADR-0024](0024-agent-identities.md)).

They are one question in three costumes, and this ADR answers it once.

## Decision

**kelson targets a platform team, or several teams, sharing one Kubernetes cluster. Multi-cluster and
multi-tenancy are out of scope for the rebuild.**

Concretely:

### 1. `Environment.spec.cluster` is deleted outright

Not gated — deleted. The field, its schema entry, its resolver handling and its
`notimplemented.go` row all go. It was already refused at validation, so no document that works today
stops working, and no rendered output changes.

A gate is the right mechanism for a field whose *shape is known and whose implementation is pending*;
that is what `notimplemented.go` exists for and it says so. `cluster:` is not that. Multi-cluster
placement is a design with several plausible shapes — a cluster reference, a placement policy, a label
selector over a fleet, a per-environment kubeconfig — and keeping one guessed spelling in the schema
prejudges the design while delivering nothing. Deleting it costs a line in the changelog and buys the
freedom to get the shape right when it is designed.

### 2. `policy.deployers` stays gated, unchanged

It keeps its rows in the gate table (`$.spec.defaults.policy.deployers`,
`$.spec.policy.deployers`) against the tenancy milestone. The difference from decision 1 is that its
shape *is* known — a list of principals, evaluated where [ADR-0025](0025-agent-policy.md) already
evaluates the rest of `policy:` — and the missing piece is a notion of a human principal, not a design.
It is a field waiting for a subject, which is exactly what a gate is for.

### 3. No Workspace, no Team, no tenancy object

[ADR-0014](0014-components.md) decision B stands as written: `Project` remains the composite, no level
is added above it, and the Workspace shape it recorded is recorded and not built. ADR-0027 adds two
CRDs and not a third.

### 4. The two directions are recorded, with their prior art, and tracked

Neither is abandoned. Both are tracked in the issue tracker — GitHub issues and milestones are this
project's source of truth for state, per AGENTS.md, and the issue numbers may be added to this document
later.

**Direction A — the control-plane tenancy object.** The shape is
[ADR-0014](0014-components.md) decision B's, and the evidence is
[docs/research/hierarchy-prior-art.md](../research/hierarchy-prior-art.md). What survives from that
research and must survive into any implementation:

- **Two-axis RBAC.** Humanitec ran per-level RBAC for years, collapsed App into Project in March 2026,
  and what survived the reset was the *permission model* — project role × environment-class role, where
  deploying to production requires both — not the hierarchy level. Any kelson tenancy design starts
  from the two axes, not from a tree.
- **AppProject-style tuple allowlists.** Argo CD's AppProject is the working shape: allowed sources,
  allowed destinations as cluster × namespace, allowed resource kinds. It is a flat control-plane object
  fanning policy out to a labelled set of namespaces, which is also what Capsule's Tenant does.
- **Hierarchy stays out of the cluster.** HNC was archived 2025-04-17 and WG Multi-Tenancy dissolved.
  Whatever kelson builds, the cluster sees flat, machine-generated namespaces.
- **It must pass the composite test.** ADR-0014's rule: a level earns its place by owning something no
  single member can own. A Workspace that only groups Projects fails it.

**Direction B — multi-cluster environment placement.** An `Environment` reconciled against a cluster
other than the controller's. The open questions are recorded so the design starts from them rather than
rediscovering them: where credentials for the target cluster live and who holds them; whether the
`ClusterProfile` becomes per-target (it must, since capability detection is what the renderer reads);
whether [ADR-0028](0028-delivery-spine.md)'s `OCIRepository` + `Kustomization` pair is created by a
remote Flux reconciling a shared registry — which is the cheap answer, because the artifact is already
the interface and a remote cluster's Flux can pull it without kelson reaching into that cluster at all —
or by a controller with fleet credentials.

## Rationale

**The target user is one cluster.** A platform team, or a handful of product teams, running one
Kubernetes cluster they own. That user has no multi-cluster problem and no hard tenancy boundary;
namespaces, the existing provenance labels and the cluster's own RBAC are what they use. Building for
the user after that one, before serving this one, is how a pre-release project ships nothing.

**Multi-cluster is not a feature; it is a different product's spine.** It changes where credentials
live, what a `ClusterProfile` is, where the controller runs, what "the cluster" means in every
capability check, and what an install even is. It cannot be added as a field, which is precisely why
decision 1 deletes the field that pretended it could.

**Tenancy needs a subject kelson does not have.** [ADR-0013](0013-server-state-and-api-v0.md) §3 is
explicit that usernames are not identities — *"the login accepts any username and verifies only the
password… nothing checks it, nothing authorizes on it"* — and #84 owns the real answer.
[ADR-0024](0024-agent-identities.md) made *agents* principals, deliberately and only agents. Building
tenancy on top of "holds the shared password" would produce authorization theatre: a policy that reads
well in a spec and enforces nothing. The order is forced — real human identity, then RBAC, then
tenancy — and skipping to the end produces a feature that lies.

**Recording a direction is not the same as deferring it, and both are better than a stub.** ADR-0014
established this pattern: record the shape and the evidence so later work does not relitigate the
question, and build none of it. What this ADR adds is the corollary — a *field* is not a way to record
a direction. A gated field is a promise about shape, and `cluster:` had no shape to promise.

**Doing it now costs one line; doing it later costs a migration.** The moment kelson has users with
stored specs, deleting a spec field is a breaking change with a deprecation window. Pre-release it is a
schema regeneration.

## Consequences

**Positive.**

- The rebuild has a boundary, and the boundary is written down where the next design conversation will
  find it.
- `internal/model` loses a field, a gate row and a resolver branch; the schema and the reference docs
  shrink with it.
- [ADR-0028](0028-delivery-spine.md)'s controller has exactly one cluster to reason about, so
  `ClusterProfile`, RBAC, the registry credential and the Flux objects all have one home each.
- ADR-0027 adds two CRDs rather than three, and the two it adds are the two the delivery loop needs.
- The prior art that would otherwise have to be re-derived — two-axis RBAC, tuple allowlists, the
  composite test, the HNC outcome — is cited from one place.

**Negative — stated as plainly as the positives.**

- **kelson does not serve teams with a cluster per environment**, which is a common and reasonable
  topology — staging and production as separate clusters is the default advice in much of the
  ecosystem. Those users can run one kelson per cluster, which means one control plane per cluster, no
  cross-cluster promotion, and no single view. That is a real limitation and not a small one.
- **Multi-tenancy is namespaces and cluster RBAC, and kelson adds nothing.** Anyone needing hard tenant
  isolation gets no help from kelson beyond the provenance labels it already writes.
- **`policy.deployers` remains a field that refuses**, so a spec can express an intention kelson will
  not honour, and the author is told so at validation time rather than discovering it. Honest, and
  still friction.
- **A deleted field is a decision that has to be re-taken.** When multi-cluster is designed, `cluster:`
  may well come back with the same name and a different meaning, and this ADR will read as though
  nothing was gained. What was gained is that the meaning is chosen with the design rather than
  inherited from a guess.
- **"For now" is doing work in the title.** Recording a direction without a milestone is how directions
  become permanent, and nothing in this ADR forces the question to be revisited on any schedule.

## Revisit when

- **Real usage produces a cluster-per-environment demand with enough weight to move it.** That is a new
  ADR for direction B, and it starts from the questions listed there — credentials, per-target
  `ClusterProfile`, and whether the OCI artifact is already the multi-cluster interface.
- **#84 lands real human identity.** That is the precondition for tenancy, and it unblocks
  `policy.deployers` before it unblocks anything else. A Workspace object is the *third* step, not the
  first.
- **A second axis of grouping appears that Project cannot own**, by ADR-0014's composite test. Until
  something fails that test, the answer to "should there be a level above Project" stays no.
