# Vision

## The bet

Kubernetes is the substrate. Not because it's pleasant, but because it's the only one that doesn't run out.

Every PaaS built on something else is a bet that its users won't need what its authors didn't anticipate. That bet has a predictable expiry. You need a sidecar, or an operator, or a network policy, or a second region, and the platform has no answer. Leaving it means rewriting everything, usually under time pressure, usually at the worst moment.

Kubernetes doesn't have that cliff. The same substrate runs a single node and a large fleet. Someone who starts on it at day zero never has to migrate off it.

What Kubernetes lacks is a good front door. That's a solvable problem and it's the whole of kelson's job.

## Why this matters more now

LLMs are good at Kubernetes. The manifests, the operators, the controller patterns and years of documentation are all in the training data. An agent reasoning about a Deployment, an HTTPRoute or a CloudNativePG Cluster is working with something it has seen thousands of times.

An agent reasoning about a PaaS's private abstraction is not. It's guessing at an internal model it has never seen, and it will guess confidently and wrongly.

This inverts the usual instinct. The conventional PaaS move is to invent concepts that hide Kubernetes. Once agents are operating the system, each invented concept is a cost paid by every human and every agent that touches it.

So kelson generates standard Kubernetes and stays out of the way. The abstraction is thin on purpose.

And kelson is itself Kubernetes-shaped: you author a `Project` and an `Environment` as **custom
resources** in your cluster ([ADR-0027](adr/0027-crd-native-control-plane.md)), a controller renders
them to plain manifests, publishes those as an immutable OCI artifact, and Flux applies it
([ADR-0028](adr/0028-delivery-spine.md)). `kubectl` works on kelson's own objects the way it works on
everything else, and what lands in your cluster is YAML you could have written.

## Principles

### 1. Deletable by design
Removing kelson should change nothing about running workloads. This is only true if the output is plain Kubernetes YAML that stands alone, so every other decision defers to it. It is now the delivery mechanism rather than an export of it: every revision kelson ever deployed is an immutable artifact of standard manifests you can pull with `flux pull artifact` and apply with `kubectl` ([ADR-0028](adr/0028-delivery-spine.md)) — which is why `kelson eject` was deleted rather than kept.

### 2. The renderer is a pure function
`(spec, ClusterProfile) → manifests` touches no cluster, no network, no clock, no database. Deterministic and testable with golden files. Trustworthy previews, cheap tests and an artifact whose digest *is* the revision all follow from this one property. It stays pure Go: CUE and timoni were evaluated as the engine and rejected, because the error taxonomy and the golden corpus are the assets ([ADR-0029](adr/0029-renderer-stays-go.md)).

### 3. Thin abstraction, real escape hatch
Three fields to deploy. Every generated resource inspectable. Raw patches and arbitrary manifests available inside the app model, so nobody has to leave the platform to do something it didn't anticipate.

The escape hatch also protects the spec. Long-tail requests get a good answer that isn't "add another field", which is how Coolify's option surface grew until its own users called it overwhelming.

### 4. Adopt, don't install
If the cluster has a Gateway API implementation, route through it. If it has cert-manager, emit `Certificate` resources instead of running an ACME client. Detection is a subsystem, not a config flag. (Routing is Gateway API only — a missing implementation is a reported gap plus an offer to install one, never a parallel stack.)

### 5. Delegate stateful workloads
kelson will not implement Postgres failover. CloudNativePG has spent years on it. kelson's job is making a database a three-line spec entry and wiring the connection in correctly.

### 6. One API, four clients
CLI, web UI, HTTP API and MCP server are peers. No capability belongs to one of them. If the UI can do it, an agent can, and the reverse.

### 7. Agents are principals, not proxies
An agent gets its own identity, scoped and expiring credentials, its own audit trail, and per-environment policy about what it may do unsupervised. Free in development, propose-only in production.

Tools are not the hard part here, and won't be a differentiator for long. Canine already ships MCP tools. The hard part is an agent acting in production without a human regretting it.

### 8. Open, permanently
MIT. Every feature. SSO, RBAC and audit logs are security basics, not upsells. Paywalling them ships a product that is insecure by default for anyone who won't pay. kelson has no monetization plan and no commercial intent — no paid tiers, ever.

## Vocabulary

Three concepts, and no more without a strong argument. See [ADR-0006](adr/0006-project-application-environment.md).

- **Project** — a grouping with shared configuration. A product or a team's surface area.
- **Component** — one deployable. A web service, a worker, a cron job, a managed database. Renders to one workload ([ADR-0014](adr/0014-components.md) unified the leaf; ADR-0006 called it an Application).
- **Environment** — where a Component runs and what differs there. Namespace, domain, secrets backend, policy.

A typical service is one Project containing three Components, deployed into two or three Environments.
Project and Environment are custom resources; a Component is an entry in the Project's `components:`
list.

## Non-goals

- **Not a Kubernetes distribution.** kelson can provision k3s or Talos for someone starting from nothing, but it delegates to them and doesn't manage node pools, upgrades or CNI.
- **Not a general-purpose dashboard.** Headlamp and k9s exist. kelson shows applications, not every resource in the cluster.
- **Not a CI system.** kelson builds images and deploys them. It doesn't replace GitHub Actions or run your test matrix.
- **Not a replacement for Flux.** Flux does the reconciling, always and only ([ADR-0028](adr/0028-delivery-spine.md)) — composing with an existing installation is the design point, and a cluster with none is offered flux-aio: every Flux controller in one pod, sized for a small cluster ([ADR-0030](adr/0030-flux-aio-install.md)). There is no second delivery path and no adapter seam; a second reconciler would be a new decision, argued on evidence.
- **Not a monitoring stack.** kelson adopts your Prometheus, Loki and Grafana and renders per-application views. It doesn't ship a TSDB.
- **Not cloud infrastructure provisioning.** No Crossplane-style resource graph. If you need an RDS instance, provision it with Crossplane or Terraform and reference the result.
- **Not multi-cluster, and not multi-tenant.** One cluster, one tenant, for now and on the record ([ADR-0031](adr/0031-single-cluster-single-tenant.md)). Teams with a cluster per environment can run one kelson per cluster, which means no cross-cluster promotion and no single view — a real limitation, recorded with the prior art the design will start from rather than pretended away with a spec field.

## What success looks like

**v0.1** — Someone with an empty VPS or an existing cluster gets a Git repo to a URL over TLS, and the manifests kelson wrote are ones they'd approve in review.

**v0.5** — A team of ten runs staging and production through kelson, with preview environments on pull requests, HA Postgres with point-in-time recovery, SSO, and an audit log they'd show an auditor.

**v1.0** — An agent takes "checkout is throwing 500s in production", investigates through kelson's API under real guardrails, and opens a reviewable pull request. A platform engineer can read exactly what it did afterwards.
