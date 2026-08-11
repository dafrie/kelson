# ADR-0005: Delegate stateful workloads to upstream operators

- **Status:** **Proposed** — not yet reviewed or accepted
- **Date:** 2026-08-11

> ⚠️ This decision was drafted without sign-off. It raises the prerequisite burden for users
> meaningfully, and it is being built on by [ADR-0007](0007-data-services.md) and the M9 issues. Review
> before it hardens.

## Context

"Managed databases" in this category almost universally means a container with a PersistentVolumeClaim.
No high availability, no point-in-time recovery, no automated failover. Users notice — a recurring
complaint even from satisfied Coolify users is that they *"still had to spend a lot of effort maintaining
PostgreSQL and Redis."*

There is also a cautionary tale. Kubero vendored Bitnami Helm charts for its add-ons. When Broadcom
withdrew the free Bitnami image catalog in September 2025, working installations broke and the add-on
system had to be reworked under time pressure.

Meanwhile CloudNativePG, Strimzi and the Valkey/Redis operators solve these problems properly, are
actively maintained, and are free.

## Decision

**kelson does not implement stateful workload operations. It delegates to upstream operators and owns
only the application-facing abstraction over them.**

| Need | Delegated to |
|---|---|
| PostgreSQL (HA, PITR, failover) | CloudNativePG |
| Kafka | Strimzi |
| Redis-compatible cache | Valkey operator |
| Object storage | MinIO operator, or an external S3 endpoint |
| TLS certificates | cert-manager |
| Secret storage | external-secrets, Vault, SOPS |
| Metrics | Prometheus |
| Reconciliation | Flux, Argo CD |
| Progressive delivery | Argo Rollouts, Flagger |

kelson's contribution is the coherent layer above: a `services:` entry in the Application spec, plan-based
sizing (`ha-small`), correct connection binding into the workload, and a unified backup and restore view.

Corollary: **do not vendor third-party Helm charts for stateful components.** Reference upstream operators
by version, detect them via `ClusterProfile`, and install them only when absent.

## Rationale

Database high availability is a multi-year problem that a PaaS will always do worse than a dedicated
operator, and doing it badly is worse than not doing it — users trust the abstraction with their data.

The Bitnami incident is the specific lesson: taking a hard dependency on someone else's packaging of
someone else's software puts your users' running systems at the mercy of a vendor decision you have no
influence over. Depending on an operator's CRD API is a much smaller and more stable surface than
depending on a chart's values schema and image registry.

This also keeps kelson deletable per [ADR-0001](0001-hybrid-state-model.md). If kelson renders a
CloudNativePG `Cluster` resource, removing kelson leaves a perfectly good CloudNativePG cluster behind.
If kelson ran its own Postgres supervision, removing it would orphan the database.

## Consequences

**Positive.**
- Genuinely production-grade data services from day one, rather than a container with a volume.
- A much smaller surface for kelson to maintain and support.
- Operator CRDs are more stable than chart values schemas.
- Removing kelson leaves working, standard, independently-manageable infrastructure.

**Negative.**
- More prerequisites. Postgres requires CloudNativePG present or installed, which is more moving parts
  than `docker run postgres`. Mitigated by detection plus optional installation.
- kelson inherits each operator's upgrade cadence and breaking changes, and must track CRD version skew.
- Less control over the end-to-end experience; some operator behaviour will be awkward to abstract, and
  the abstraction must leak deliberately rather than pretend.
- A smaller service catalog at launch than Coolify's ~300 one-click templates. Accepted: the catalog is a
  Phase 4 concern, and quality over breadth is the right trade for stateful software specifically.
