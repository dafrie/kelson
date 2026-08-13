# Data services — preset topologies, sizing and capability rules

Implementation reference for M9 · Data services (epic
[#10](https://github.com/dafrie/kelson/issues/10)), completing the design decisions
[#89](https://github.com/dafrie/kelson/issues/89) left open.

[ADR-0007](adr/0007-data-services.md) is the decision record: it settled *that* presets
determine topology, that branching degrades rather than disappearing, and that retention is
asymmetric. This page settles the rest — which CloudNativePG fields each preset renders, what
the sizing defaults are and why, which preset changes are safe, and where the capability
verdict is enforced. Where the two disagree, the ADR wins and this page is the bug.

**Baseline.** kelson targets the latest CloudNativePG release (owner decision, 2026-08-13,
recorded in `internal/clusterprofile/postgres`). The declarative surface — `Cluster
.spec.managed.roles`, the `Database` CRD, declarative schemas and extensions — is relied on
with no imperative fallback. An older operator is not a global refusal but a per-capability
answer; see [Validation versus capability](#validation-versus-capability).

Every CloudNativePG field named below was checked against the `release-1.30` API types
(`api/v1/cluster_types.go`, `api/v1/database_types.go`) and the matching `docs/src` pages, not
against memory. Field names that surprised us are called out where they appear.

---

## Preset → topology

| Preset | Topology | Rendered by kelson | Instances |
|---|---|---|---|
| `shared` | a `Database` in a shared cluster | `Database` CR only | — (no pod) |
| `small` | dedicated cluster | `Cluster` CR | 1 |
| `ha-small` | dedicated, synchronous | `Cluster` CR | 3 |
| `ha-medium` | dedicated, synchronous | `Cluster` CR | 3 |
| `branch` | dedicated, bootstrapped from a source | **nothing yet** | — |

`branch` validates and renders a structured not-implemented error naming
[#99](https://github.com/dafrie/kelson/issues/99). Branching needs a source cluster, a snapshot
mechanism chosen from the profile and a TTL; none of that exists, and rendering an ordinary
empty `small` cluster for it would be the silent-success failure
[#141](https://github.com/dafrie/kelson/issues/141) exists to prevent.

`type: valkey` is the same story against [#98](https://github.com/dafrie/kelson/issues/98).

### The dedicated presets

One `postgresql.cnpg.io/v1` `Cluster` per service, named `<project>-<environment>-<service>`
in the environment's namespace.

```yaml
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: checkout-production-db
  namespace: checkout-prod
spec:
  instances: 3
  minSyncReplicas: 1
  maxSyncReplicas: 1
  storage:
    size: 5Gi
  resources:
    requests: {cpu: 500m, memory: 1Gi}
    limits: {memory: 1Gi}
  bootstrap:
    initdb:
      database: app
      owner: app
```

**Why `bootstrap.initdb` and not `managed.roles`.** CloudNativePG's `initdb` bootstrap creates
the application database and its owning role *and generates their credentials*, publishing them
as a `basic-auth` Secret named `<cluster>-app`. `.spec.managed.roles` does not: a managed role
with a password requires a `passwordSecret` the author supplies, and the renderer cannot invent
a password — it is a pure function with no random source ([ADR-0001](adr/0001-hybrid-state-model.md),
[#20](https://github.com/dafrie/kelson/issues/20)). So the owner role kelson binds applications
to is the `initdb` owner, and `managed.roles` enters only when a spec needs *more* than the
owner role — a read-only reporting user, a migration role with different privileges. Nothing in
the spec model expresses that yet, so nothing renders it yet; the capability is still required
of the cluster (`postgres.CapabilityManagedRoles`) because the presets that will use it are the
same presets.

`database` and `owner` are both `app`, CNPG's own defaults, written explicitly rather than left
to the defaulting webhook: a manifest that states the database name is one a reviewer can read.
The database is named per cluster, not per project, because the cluster is already
project-scoped by name and namespace — one database per cluster is CNPG's guidance and the
dedicated presets follow it exactly.

**Synchronous replication** on the HA presets uses `minSyncReplicas: 1` / `maxSyncReplicas: 1`.
CloudNativePG 1.24 introduced a newer stanza, `.spec.postgresql.synchronous` with
`method`/`number`/`dataDurability`, and calls the two older fields legacy — but does *not*
remove them, and they remain fully supported in 1.30. kelson's declared support floor for
`cnpg` is 1.23.0 (`internal/clusterprofile/support`, degradation `refuse`), which is below the
release that added the newer stanza, so writing it would refuse clusters the support matrix
promises to serve. The legacy fields are the ones that work across kelson's whole declared
range. Moving to `.spec.postgresql.synchronous` is a follow-up for whenever that floor rises
past 1.24, not a v0 decision.

`maxSyncReplicas` must stay below `instances`; 1 of 3 means one standby must confirm each
commit and the cluster still accepts writes after losing one replica. `minSyncReplicas: 1`
keeps that guarantee from silently degrading to asynchronous.

**Storage class is deliberately unset.** `spec.storage.storageClass` is optional and CNPG falls
back to the cluster default. kelson could read `ClusterProfile.StorageClasses` and name the
default explicitly, but that would bake a detection snapshot into a manifest that outlives it,
and it is the *branching* verdict — not provisioning — that actually depends on which class was
used ([#91](https://github.com/dafrie/kelson/issues/91), [#108](https://github.com/dafrie/kelson/issues/108)).

### The shared preset, and the constraint that shapes it

`shared` renders one `Database` CR and no cluster: the shared cluster is kelson-owned
infrastructure provisioned once per environment tier by
[#93](https://github.com/dafrie/kelson/issues/93), not per project. ADR-0007's resource
arithmetic (10 apps × 3 environments → 12 clusters) only works if `shared` means *one cluster
for every development database in the installation*, so the shared cluster cannot live in a
project's namespace.

**CloudNativePG's `Database.spec.cluster` is a `corev1.LocalObjectReference`** — verified in
`api/v1/database_types.go`, where it carries only a `name` and is additionally marked immutable
after creation. There is no namespace field and no cross-namespace form. A `Database` must
therefore live in the same namespace as the `Cluster` it targets.

So the `Database` CR renders into the shared cluster's namespace, not the project's:

```yaml
apiVersion: postgresql.cnpg.io/v1
kind: Database
metadata:
  name: checkout-development-db
  namespace: kelson-data
spec:
  cluster:
    name: kelson-shared-development
  name: checkout-development-db
  owner: checkout-development-db
  databaseReclaimPolicy: retain
```

- **Namespace `kelson-data`, cluster `kelson-shared-<environment>`.** The environment name is
  the only pure input that identifies the tier, so it is what the convention is built from; two
  projects' `development` environments share one cluster, which is the point. #93 owns making
  both real and may make them configurable.
- **`metadata.name`, `spec.name` and `spec.owner` are all `<project>-<environment>-<service>`.**
  In a cluster shared across projects, a database called `db` collides on the first collision;
  the qualified name is what makes the shared topology safe. The same name is used for the
  dedicated presets' `Cluster` so there is one naming rule rather than two.
- **`databaseReclaimPolicy: retain`** is CNPG's default, written explicitly because it *is*
  ADR-0007's asymmetric-retention promise: removing a service from a spec must never drop a
  database.

Provenance labels and annotations are stamped exactly as on every other resource, including
`kelson.dev/project` and `kelson.dev/environment` — which is how a human looking at
`kelson-data` can tell whose database is whose, and how a future uninstall can find them.

**What `shared` does not do yet: credentials.** The `Database` CRD creates a database, not a
role, and generates no Secret. The owner role and its password live in the shared cluster, and
the Secret holding them would live in `kelson-data` — while the workload that needs it runs in
the project's namespace. Kubernetes Secrets are namespaced and a pod cannot `secretKeyRef`
across a namespace boundary, so binding to a `shared` service needs a credential *distribution*
step (a role provisioned in the shared cluster, its Secret projected into the consuming
namespace) that belongs to #93 and does not exist. Until it does, `env: {from: {service: …}}`
against a `shared`-preset service is a structured render error naming #93 rather than a
`secretKeyRef` to a Secret nothing creates. The database is still created; only the automatic
binding is missing.

---

## Sizing defaults

The preset **is** the sizing. There is no `resources:` override on a service in v0 — see
[Tuning](#tuning-without-leaving-the-preset-model).

| Preset | Instances | CPU request | Memory request | Memory limit | Storage | Sync replicas |
|---|---|---|---|---|---|---|
| `shared` | — | — | — | — | — | — |
| `small` | 1 | `500m` | `1Gi` | `1Gi` | `5Gi` | none |
| `ha-small` | 3 | `500m` | `1Gi` | `1Gi` | `5Gi` | min 1, max 1 |
| `ha-medium` | 3 | `2` | `4Gi` | `4Gi` | `20Gi` | min 1, max 1 |

**Memory: 1Gi for the small tier.** ADR-0007 measured a CNPG instance at roughly 512Mi in
practice. Requesting exactly that leaves nothing for `shared_buffers`, page cache or a vacuum
that has work to do, and the first real query pattern evicts everything. 1Gi is the smallest
number that is not immediately a support ticket, and it keeps the ADR's arithmetic honest: five
`small` databases still fit in 5Gi on a bare VPS.

**Memory limit equals the request; there is no CPU limit.** A Postgres process that exceeds its
memory limit is OOM-killed, which for a primary is a failover — so the limit is set where the
request is, and the pod is not allowed to grow into memory the node cannot honour. CPU is the
opposite case: throttling a database at a limit turns a slow query into a slow *cluster*,
including the replication and health traffic that decides whether a failover happens. Requests
guarantee the floor; the burst above it is free and worth having.

**Storage: 5Gi small, 20Gi medium.** `spec.storage.size` cannot be decreased — CNPG says so in
the field's own documentation and enforces it — so a default that is too large is a permanent
cost and one that is too small is a resize. 5Gi is roughly a year of a small application's data
plus WAL headroom, and ten of them is 50Gi, which a modest VPS has. Growing is a one-line spec
change CNPG reapplies to the PVCs.

**`ha-medium` at 2 CPU / 4Gi / 20Gi** is the "this is the production database" tier: enough
shared buffers to hold a working set of a few gigabytes, and CPU headroom for a rebuild or a
bulk migration without starving the replicas.

### Tuning without leaving the preset model

**v0 answer: there is none.** The preset is the sizing, and a service has exactly three
authored fields (`name`, `type`, `preset`). This is a deliberate refusal to invent spec surface
before the shape of the requests is known.

The reasoning is the same one that made presets a good idea. A `resources:` block on a service
recreates the decision the preset exists to make, and it recreates it in the place where the
user has the least information — the number that matters for Postgres is rarely the container's
memory limit but `shared_buffers`, `work_mem`, `max_connections` and the relationship between
them. A partial override (`memory` set, `instances` not) also silently turns a named topology
into an unnamed one, which breaks the transition table below.

A future `resources:` override, or additional presets, is a spec change with its own issue and
its own migration story. Until then, an application that has genuinely outgrown `ha-medium` uses
the escape hatch of record: an overlay patch against the rendered `Cluster`
([docs/model.md](model.md), rule P6). That is visible, reviewable, and does not pretend the
preset still describes what is running.

---

## Safe preset transitions

Preset is an Environment-level override (ADR-0007, rule P5), so changing one is a two-character
spec edit. These are not equally cheap.

| From → to | Verdict | What happens |
|---|---|---|
| `small` → `ha-small` | **safe, in place** | `instances: 1 → 3`; CNPG clones the primary into two standbys and turns on synchronous replication. No downtime, no data movement by kelson. |
| `ha-small` → `ha-medium` | **safe, in place** | Resource requests grow and the PVCs are resized upward. Rolling restart per instance. |
| `ha-medium` → `ha-small` | **safe, lossy of headroom** | Requests shrink; **storage does not** — `spec.storage.size` cannot be decreased, so the PVCs stay at the larger size and the cost stays with them. |
| `ha-*` → `small` | **safe, lossy of replicas** | `instances: 3 → 1`; the standbys and their PVCs are removed and synchronous replication stops. The data survives on the primary; the *availability* does not, and neither does the read capacity. |
| `shared` → any dedicated | **migration, not in place** | Different cluster, different storage. The database has to be dumped and restored, or replicated and cut over. [#105](https://github.com/dafrie/kelson/issues/105). |
| any dedicated → `shared` | **migration, not in place** | Same, in reverse, plus a name change: the database becomes `<project>-<environment>-<service>` inside the shared cluster instead of `app` inside its own. |
| anything → `branch` | **not a transition** | A branch is a *new* service bootstrapped from a source, with a TTL and a lifecycle of its own (ADR-0007). Rewriting an existing service's preset to `branch` is not a topology change, it is a different object. [#99](https://github.com/dafrie/kelson/issues/99). |

**What is enforced today: nothing beyond validation and the capability check.** The renderer is
pure and stateless — it sees the spec it is asked to render, not the one that is already
deployed — so it cannot tell an in-place bump from a migration. The place that *can* is the
preview/diff plane, which compares rendered output against live state; a `shared` → `small`
edit shows up there as a `Database` deleted and a `Cluster` created, which is at least visible.
Turning that into a refusal with a named migration path is #105's job, and this table is the
specification it should implement.

---

## Validation versus capability

A preset is a request; whether a cluster can serve it is a separate question with **three**
answers, not two ([`clusterprofile.Outcome`](https://github.com/dafrie/kelson/issues/144)).
`postgres.SupportsPreset(profile, preset)` gives the verdict and names the capability that
decided it.

### The check runs in the renderer, not in validation

`model.Validate*` has no `ClusterProfile` — deliberately, because a spec is valid or invalid on
its own terms and the same spec is meant to target several clusters. The capability question
cannot be answered there without handing the authoring plane a cluster, which is the boundary
[ADR-0001](adr/0001-hybrid-state-model.md) draws.

So the check lives in `internal/renderer`, which already takes a `ClusterProfile`, and fails
with a structured render error in the same family as `render/gateway-api-missing`. #89's
acceptance criterion — *"requesting a plan the cluster cannot support fails at validation with
the reason"* — is read as **fails before anything is applied, with the reason**: every surface
that can reach a cluster (`kelson render`, `diff`, `deploy`, the ConnectRPC API, the MCP tools)
goes through `Render`, so no path exists that accepts an unsupportable preset and finds out
later. What the reading gives up is a profile-free `kelson validate` catching it, which it could
never honestly have done.

### Outcome → behaviour

| Verdict | Renderer behaviour |
|---|---|
| **Yes** | Render. The operator is present and meets every capability the preset needs. |
| **No** | Refuse: `render/postgres-unsupported`, naming the blocking capability, the version floor, the detected version, and the remediation (always *upgrade the operator you have* — CNPG is cluster-scoped and a second install fights the first). |
| **Unknown** | **Render.** |

**Unknown renders, and that is the whole point of having three outcomes.** Unknown means the
operator's version could not be read, or CNPG sat behind a detection gap — a probe without RBAC
to list deployments, a profile written by hand. It does not mean the capability is absent. A
zero `ClusterProfile` is the extreme case: nobody looked at anything.

Refusing on Unknown would make every hand-written profile and every under-permissioned probe
into a refusal for a cluster that very likely works, and would teach people to write
capabilities into profiles to get past kelson rather than because they checked. Rendering on
Unknown costs an apply-time failure from the API server — `no matches for kind "Cluster"`, or a
field the operator does not know — which is a real, specific error from the component that
actually knows the answer, arriving before anything is running.

The asymmetry is the tri-state discipline: **Unknown is not No.** A No is a fact we established
(no CNPG at all, a version below the floor, a CRD the API server does not serve) and refusing on
it is honest. An Unknown is an absence of evidence and refusing on it would be a claim we cannot
make.

The renderer has no side channel for a warning — it returns manifests or errors, and adding a
third return value would put advisory prose in the path of every caller including the golden
harness. Surfacing "rendered, but the profile could not confirm CNPG" belongs to the callers
that already report profile findings: `kelson diff`'s preview, which reports unvalidated
resources, and the profile report itself, which lists gaps with the permission that would close
them. Nothing rendered is hidden; nothing rendered is claimed to be verified either.

### Binding keys

An application binds to a service by key: `env: {DATABASE_URL: {from: {service: db, key: uri}}}`.
Validation checks the key against `model.ServiceKeys`; the renderer turns it into a
`secretKeyRef` against the Secret CloudNativePG generates, `<cluster>-app` (type `basic-auth`).

CNPG's key names are not kelson's. The mapping, read from `pkg/specs/secrets.go` in
`release-1.30`:

| kelson key | CNPG key in `<cluster>-app` |
|---|---|
| `uri` | `uri` |
| `host` | `host` |
| `port` | `port` |
| `database` | **`dbname`** |
| `username` | `username` |
| `password` | `password` |

CNPG also writes `user`, `pgpass`, `jdbc-uri`, `fqdn-uri` and `fqdn-jdbc-uri`. These are not
exposed as kelson keys: `user` duplicates `username`, `pgpass` is a file format rather than a
value, and the JDBC and FQDN forms are worth adding when something asks for them rather than
guessed at now. A key kelson does not map is a structured render error listing the keys that
exist, so the failure names the fix.

`uri` and `fqdn-uri` differ in the host they embed — `<cluster>-rw.<namespace>:5432` versus the
fully qualified `…svc.<cluster-domain>`. `uri` is the right default inside one cluster and the
one kelson maps.

---

## What renders, in what order

For each service, in spec order, **after the Namespace and before any application** — a
workload that binds to a service should not be applied before the resource that produces its
credentials:

```
Namespace
  postgresql.cnpg.io/v1 Cluster    (small | ha-small | ha-medium)
  postgresql.cnpg.io/v1 Database   (shared)
  ServiceAccount / Service / Deployment / … per application
```

Ordering is advisory rather than a guarantee — nothing waits for the cluster to be ready, and a
workload whose database is still bootstrapping will crash-loop until it is. That is the same
deal every other resource in the set gets, and it is visible in `kelson status`.

Names are `<project>-<environment>-<service>`, capped at 53 characters. CloudNativePG derives
its own object names from the cluster's — `<cluster>-app`, `<cluster>-superuser`,
`<cluster>-rw`, `<cluster>-1` — and the longest suffix (`-superuser`, ten characters) has to
fit inside the 63-character DNS label limit. A longer name is a structured render error rather
than a resource the API server rejects with an arithmetic complaint.

Provenance is identical to every other rendered resource: `app.kubernetes.io/managed-by`,
`kelson.dev/project`, `kelson.dev/environment`, `kelson.dev/renderer-version` and a
`kelson.dev/spec-hash` computed over the service's own resolved input, so an unrelated edit
elsewhere in the spec does not churn the database's annotation.

Services carry no `kelson.dev/application` label. They are not owned by one application — that
is what makes them bindable by several.

---

## Status

| Piece | State |
|---|---|
| `small`, `ha-small`, `ha-medium` → `Cluster` | rendered |
| bindings against a dedicated preset | rendered (`secretKeyRef` → `<cluster>-app`) |
| `shared` → `Database` | rendered |
| bindings against `shared` | **structured error**, [#93](https://github.com/dafrie/kelson/issues/93) |
| the shared cluster itself | not rendered, [#93](https://github.com/dafrie/kelson/issues/93) |
| `preset: branch` | **structured error**, [#99](https://github.com/dafrie/kelson/issues/99) |
| `type: valkey` | **structured error**, [#98](https://github.com/dafrie/kelson/issues/98) |
| backups, WAL archiving, PITR | not rendered, [#94](https://github.com/dafrie/kelson/issues/94) |
| declarative schemas and extensions | not authorable; the capability is judged, nothing consumes it |
| preset transitions beyond rendering | not enforced, [#105](https://github.com/dafrie/kelson/issues/105) |

## Sources

CloudNativePG `release-1.30`, read directly rather than from memory:

- `api/v1/cluster_types.go` — `instances`, `minSyncReplicas`, `maxSyncReplicas`, `storage`
  (`size`, `storageClass`, "Size cannot be decreased"), `resources`, `bootstrap`, `managed`.
- `api/v1/database_types.go` — `spec.cluster` as `corev1.LocalObjectReference` and immutable,
  `spec.name`, `spec.owner`, `spec.ensure`, `spec.databaseReclaimPolicy` (default `retain`),
  `spec.schemas`, `spec.extensions`.
- `pkg/specs/secrets.go` — the exact `StringData` keys of the generated `<cluster>-app` Secret.
- `docs/src/declarative_database_management.md`, `docs/src/bootstrap.md`,
  `docs/src/applications.md`, `docs/src/replication.md`.
