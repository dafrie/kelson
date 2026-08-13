# ADR-0007: Data services — presets, delegation and branching

- **Status:** Accepted (amended 2026-08-12: "plan" is renamed to "preset" — it is a topology preset, not a paid tier; rename tracked in [#146](https://github.com/dafrie/kelson/issues/146). Branching and Valkey moved to v0.2/M9b. Amended 2026-08-13, owner decision: the `shared` preset is deferred — it was this ADR's cost optimization, never an ask, and dedicated clusters per postgres component are simpler, work today including bindings, and the pod cost is acceptable at this stage; dedicated-per-component is the model for now, and [#93](https://github.com/dafrie/kelson/issues/93) tracks any return of `shared`. Backups are also deferred — "Backups are configured once" below does not render yet; when they land, the near path is CloudNativePG's declarative volumeSnapshot-based `Backup`/`ScheduledBackup` on snapshot-capable storage, which kelson already detects ([#91](https://github.com/dafrie/kelson/issues/91)), with object-store WAL archiving as a later addition ([#94](https://github.com/dafrie/kelson/issues/94)-[#96](https://github.com/dafrie/kelson/issues/96)).)
- **Date:** 2026-08-11

> **Implementation reference:** [docs/data-services.md](../data-services.md) carries the decisions
> this ADR left to implementation — the CloudNativePG fields each preset renders, the sizing
> defaults and their rationale, the safe-transition table, and where the capability verdict is
> enforced ([#89](https://github.com/dafrie/kelson/issues/89)). This ADR stays the decision record.

## Context

Managed databases are the feature people ask about first, and the one this category serves worst.

Coolify runs a standalone Postgres container with a PVC and backs it up by `docker exec` into the
container and piping `pg_dump` to S3. No replication, no HA, no point-in-time recovery — the recovery point
is however long since the last dump. Backups are configured per database, so three databases means setting
it up three times. Kubero's add-ons are explicitly not HA-ready, and its vendored Bitnami charts broke when
the catalog was withdrawn.

CloudNativePG solves the hard parts properly, and [ADR-0005](0005-delegate-to-operators.md) already commits
to delegating rather than reimplementing. Two questions remain: what topology a database service gets, and
how preview environments get data.

### The topology problem

CNPG's own guidance is one database per cluster. From the maintainers: *"Not supporting multiple databases
was a design decision taken to push a '1 database per cluster' approach."* The reasoning is microservice
data ownership and blast radius.

That is right for production and unaffordable everywhere else. A CNPG instance needs roughly 512Mi in
practice; HA is three of them.

| Setup | 10 apps × 3 envs | Pods | RAM |
|---|---|---|---|
| Dedicated cluster each | 30 clusters | 30 | ~15 GB |
| Dedicated, HA in prod | 30 clusters | 50 | ~25 GB |
| Shared dev/staging, dedicated prod | 12 clusters | 12 | ~6 GB |

[ADR-0003](0003-install-model.md) commits to someone starting on a bare VPS. Five apps with dedicated
clusters is 2.5 GB of Postgres before any application code runs.

CNPG 1.25 shipped a `Database` CRD for declarative database creation, with schemas and extensions added in
1.26. So multiple databases per cluster is supported, just not the recommended shape.

### The branching problem

CSI snapshots are PVC-level, and a CNPG cluster's PVC is the whole cluster. **A single database cannot be
branched out of a shared cluster.** Branching means snapshotting a dedicated source cluster and
bootstrapping a new dedicated cluster from it.

Neon branches in about a second regardless of size because its storage is log-structured and
LSN-addressed. That is a different storage engine, not Postgres on a PVC, and it is not reproducible here.

Whether a snapshot is cheap depends entirely on the driver. Ceph RBD, ZFS and LVM-thin give thin
copy-on-write clones. EBS, GCE PD and Azure Disk give a full-size volume restored lazily. **k3s ships
local-path, which has no snapshot driver at all.**

## Decision

### Presets determine topology

| Preset | Topology | Branchable | Cost |
|---|---|---|---|
| `shared` | `Database` CRD in a shared cluster | no | no pod |
| `small` | dedicated cluster, 1 instance | yes | 1 pod + PVC |
| `ha-small`, `ha-medium` | dedicated, 3 instances, synchronous | as source | 3 pods |
| `branch` | dedicated, bootstrapped from a source | is a branch | 1 pod + PVC |

Preset is an **Environment-level override**. One Project spec, `shared` in development and `ha-small` in
production. This falls out of [ADR-0006](0006-project-application-environment.md).

We follow CNPG's guidance where it matters and deviate knowingly where it doesn't, documenting why.

### Branching degrades rather than disappearing

kelson picks the best available mechanism from `ClusterProfile` and **reports which one it used**.

| Mechanism | Requires | Speed | Storage |
|---|---|---|---|
| Thin snapshot clone | Ceph RBD, ZFS, LVM-thin | seconds | thin |
| Full snapshot clone | EBS, GCE PD, Azure Disk | minutes | full size |
| Backup restore with PITR | object store configured | minutes to hours | full size |
| Logical dump and restore | nothing | slow | full size |
| Empty plus migrations | nothing | seconds | minimal |

Backup-restore as the universal fallback is what makes this shippable rather than a Ceph-only feature.
PITR also means branching from a point in time, not only from now.

### Branching production is allowed, opt-in and audited

A production database is marked branchable by an operator. Branching one is an audited action with a TTL
and automatic destruction. This enables the use case that justifies the feature: **branch production as of
ten minutes ago, run the migration against it, see what breaks.**

kelson does not attempt to anonymise data and will not claim to. Teams that need anonymisation supply a
post-bootstrap SQL transform.

### Preview databases default by capability

Branch from staging where fast branching is available, empty plus migrations where it is not. An explicit
project setting always wins. This keeps twenty open pull requests from becoming twenty full-size restores
on a cluster whose storage cannot snapshot cheaply.

### Backups are configured once

The object store destination is set per environment, not per database. Every non-preview database gets
continuous WAL archiving and PITR through CNPG automatically. This is the direct answer to Coolify's
configure-it-three-times model.

### Retention is asymmetric

Persistent environments **retain** data when a service is removed from a spec, and require an explicit
destructive action to delete. Preview environments **destroy** on close.

Accidentally dropping a production database because someone edited a YAML file is the worst failure this
feature can have, and the default must make it impossible rather than merely unlikely.

### Engines

PostgreSQL and Valkey are the managed types in the first cut. MySQL is deferred: the operator landscape is
materially weaker than CNPG, and three half-supported engines is worse than two done properly.

Per [ADR-0005](0005-delegate-to-operators.md), "deferred" means no *managed* MySQL type. Anyone can install
a MySQL chart and bind to it today — they just get no presets, backups or branching, and kelson makes no
durability claim about it.

## Consequences

**Positive.**
- Production-grade Postgres from the first release: HA, failover, PITR, tested restore.
- Usable on a single small node, which the dedicated-cluster-only model would rule out.
- Branching is a real capability across all storage backends, fast where storage allows.
- Migration testing against production-shaped data is something nothing else in this category offers.
- Removing kelson leaves working CNPG clusters, consistent with [ADR-0001](0001-hybrid-state-model.md).

**Negative.**
- Two provisioning and binding paths, shared and dedicated. Real complexity, accepted for the resource
  math above.
- kelson inherits CNPG's upgrade cadence and CRD version skew.
- Branching quality varies by storage class, so the same feature behaves differently across clusters. This
  must be surfaced explicitly, never silently.
- `kelson up` defaults to local-path, so the bootstrap path gets slow branching until a user opts into a
  snapshot-capable storage class. This is a known weak spot in the first-run experience.
- Branching production is a genuine data-protection risk that we mitigate with policy and audit rather
  than eliminate.
- Preview databases need migrations to run before the application starts, which pulls the release-command
  hook earlier than planned.

## Revisit when

Storage-class variance in branching produces enough support load to justify kelson installing a
snapshot-capable storage class by default, or MySQL demand outweighs the cost of a weaker operator.
