# ADR-0005: Managed service types delegate to operators

- **Status:** Accepted (revised 2026-08-11 — scope narrowed, see below)
- **Date:** 2026-08-11

## Context

"Managed database" in this category almost universally means a container with a PersistentVolumeClaim.
No high availability, no point-in-time recovery, no automated failover. Users notice — a recurring
complaint even from satisfied Coolify users is that they *"still had to spend a lot of effort maintaining
PostgreSQL and Redis."*

There is also a cautionary tale. Kubero vendored Bitnami Helm charts for its add-ons. When Broadcom
withdrew the free Bitnami image catalog in September 2025, working installations broke and the add-on
system had to be reworked under time pressure.

CloudNativePG, Strimzi and the Valkey operators solve these problems properly, are actively maintained,
and are free.

### The scope mistake in the first draft

The original version of this ADR said "do not vendor third-party Helm charts for stateful components",
which was too broad and collided with the catalog work in M15. Read literally it would have blocked
Umami, Plausible, Vaultwarden, Gitea and most of what a catalog is for — all stateful, none with an
operator, none needing one.

That conflated two different things, and the distinction is the whole point of this decision.

## Decision

**This ADR governs only the service types kelson offers as managed, where it makes a durability promise.
It places no restriction on what users can run.**

### Managed service types delegate to operators

When kelson offers something as a first-class service type, it is backed by an operator. No exceptions and
no degraded mode, because the type name is a promise.

| Managed type | Delegated to |
|---|---|
| `postgres` | CloudNativePG |
| `valkey` | Valkey operator |
| `kafka` (later) | Strimzi |
| object storage (later) | MinIO operator, or external S3 |

`type: postgres` always means CloudNativePG, with plans, HA, point-in-time recovery, backups and
branching as described in [ADR-0007](0007-data-services.md). There is no `plan: container` and no
operator-free mode. A field that sometimes means "HA Postgres with PITR" and sometimes means "a container
that will lose your data" is a trap, and someone will find it in production.

### Anything else is installable, and kelson does not judge

**Any workload or Helm chart can be installed.** Third-party applications, databases you would rather run
yourself, anything at all. kelson runs it, routes to it, and binds applications to it.

What it does not do is claim anything about durability. No plans, no managed backups, no branching, no
failover. It is your workload and yours to look after — the same deal you get from `kubectl apply`, with
better ergonomics.

This is the fallback for anyone who does not want CNPG: install a Postgres chart like any other workload.
One managed path with a real promise, one escape hatch with no promise, and no ambiguity about which one
you are using.

### Platform components are adopted, not owned

cert-manager, external-secrets, Prometheus, Flux, Argo CD and Argo Rollouts are detected and adopted per
[ADR-0003](0003-install-model.md), and optionally installed when absent by referencing upstream charts at
a pinned version. These were never candidates for reimplementation and are listed only for completeness.

### Operator prerequisites are visible, never silent

A managed type whose operator is absent appears in the UI **disabled, with the reason and a one-click
install**. Not hidden, not silently installed during bootstrap, and not installed invisibly on first use.

Pre-installing CNPG during `kelson up` would cost every user the operator's footprint whether or not they
ever create a database. Installing silently on first use turns someone's first real interaction with the
feature into a multi-minute stall with a novel failure mode. Showing the option, disabled, with a button
is honest about what is about to happen and costs one click.

## Rationale

Database high availability is a multi-year problem that a PaaS will always do worse than a dedicated
operator, and doing it badly is worse than not doing it — users trust the abstraction with their data.

The Bitnami incident is the specific lesson: depending on someone else's packaging of someone else's
software puts your users' running systems at the mercy of a vendor decision you have no influence over.
Depending on an operator's CRD API is a much smaller and more stable surface than a chart's values schema
and image registry. Note this argument applies to **what kelson promises**, not to what users install
themselves — a user who installs a chart has chosen that dependency knowingly.

This also keeps kelson deletable per [ADR-0001](0001-hybrid-state-model.md). If kelson renders a
CloudNativePG `Cluster`, removing kelson leaves a working CloudNativePG cluster. If kelson supervised
Postgres itself, removing it would orphan the database.

## Consequences

**Positive.**
- Production-grade data services from day one, rather than a container with a volume.
- A much smaller surface for kelson to maintain and support.
- Operator CRDs are more stable than chart values schemas.
- Removing kelson leaves working, standard, independently manageable infrastructure.
- The catalog is unblocked: any chart, any workload, no restriction.
- One clear promise. `type: postgres` means one thing.

**Negative.**
- Managed types carry a prerequisite. Postgres needs CNPG present or installed, which is more moving parts
  than `docker run postgres`. Mitigated by detection and one-click install, not eliminated.
- kelson inherits each operator's upgrade cadence and breaking changes, and must track CRD version skew
  across CNPG, the Valkey operator, cert-manager, external-secrets and Prometheus.
- Less control over the end-to-end experience. Some operator behaviour will be awkward to abstract, and
  the abstraction must leak deliberately rather than pretend.
- Users wanting one-click Postgres with zero prerequisites are told to install a chart and manage it
  themselves. That is a worse experience than Coolify offers, and it is the deliberate price of not
  shipping a database that loses data.

## Revisit when

A managed type's operator becomes unmaintained, or the prerequisite friction on the bootstrap path proves
to cost more adoption than the durability guarantee earns.
