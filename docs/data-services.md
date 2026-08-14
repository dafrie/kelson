# Data services — preset topologies, sizing and capability rules

Implementation reference for M9 · Data services (epic
[#10](https://github.com/dafrie/kelson/issues/10)), completing the design decisions
[#89](https://github.com/dafrie/kelson/issues/89) left open, and for `kind: valkey`
([#98](https://github.com/dafrie/kelson/issues/98)).

[ADR-0007](adr/0007-data-services.md) is the decision record: it settled *that* presets
determine topology, that branching degrades rather than disappearing, and that retention is
asymmetric. [ADR-0015](adr/0015-valkey-operator.md) settles which Valkey operator
`kind: valkey` delegates to. This page settles the rest — which operator fields each preset
renders, what the sizing defaults are and why, which preset changes are safe, and where the
capability verdict is enforced. Where an ADR and this page disagree, the ADR wins and this
page is the bug.

Two kinds, two operators, one set of rules. `kind: postgres` renders CloudNativePG resources
and `kind: valkey` renders a `ValkeyCluster`; everything below that is shared — the preset
vocabulary, the naming rule, the provenance stamps, the render ordering, and the tri-state
capability check. The [Valkey](#valkey) section states only what differs, and it differs more
than the shared parts suggest.

**Not on this page: `kind: helm`.** It delegates to an operator the same way these do
([ADR-0005](adr/0005-delegate-to-operators.md)) and it is deliberately not a data service — it has no
preset, nothing binds to it, and what it installs is the chart's business rather than kelson's. It is
documented with the model ([helm components](model.md#helm-components-a-chart-delegated),
[ADR-0016](adr/0016-delivery-flows-v0.md)).

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

The preset vocabulary is shared by both data kinds, which is what makes `preset: small` mean
the same thing whichever engine reads it. Two of its five members are not topologies a cache
has, and those refuse for `kind: valkey` with a cache-specific reason.

| Preset | `kind: postgres` | `kind: valkey` |
|---|---|---|
| `shared` | **deferred** ([#93](https://github.com/dafrie/kelson/issues/93)) | **deferred**, same decision |
| `small` | dedicated `Cluster`, 1 instance | `ValkeyCluster`, 1 shard, no replica |
| `ha-small` | dedicated `Cluster`, 3 instances, synchronous | `ValkeyCluster`, 3 shards × 1 replica |
| `ha-medium` | dedicated `Cluster`, 3 instances, synchronous | `ValkeyCluster`, 3 shards × 1 replica, larger |
| `branch` | **nothing yet** ([#99](https://github.com/dafrie/kelson/issues/99)) | **refused**: a cache has no durable state to branch from |

`branch` validates and renders a structured not-implemented error naming
[#99](https://github.com/dafrie/kelson/issues/99). Branching needs a source cluster, a snapshot
mechanism chosen from the profile and a TTL; none of that exists, and rendering an ordinary
empty `small` cluster for it would be the silent-success failure
[#141](https://github.com/dafrie/kelson/issues/141) exists to prevent.

**`shared` is deferred (owner decision, 2026-08-13, [ADR-0007](adr/0007-data-services.md)).**
It rendered a `Database` CR into a kelson-owned shared cluster until the owner reconsidered:
the shared cluster was ADR-0007's cost optimization for the resource math of many small
databases, never something anyone asked for, and the dedicated presets are simpler, work
today including bindings, and cost one pod — acceptable at this stage. `preset: shared`
validates (the vocabulary survives) and renders the same structured not-implemented error
shape as `branch`, naming #93 and suggesting `preset: small`. [#93](https://github.com/dafrie/kelson/issues/93)
tracks any return of the preset; the rendering it used to do — the `Database` CR into
`kelson-data`, the namespace convention, the credential-distribution gap — is preserved in
git history rather than repeated here.

### The dedicated presets (`kind: postgres`)

One `postgresql.cnpg.io/v1` `Cluster` per data component, named `<project>-<environment>-<component>`
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
[#20](https://github.com/dafrie/kelson/issues/20)). So the owner role kelson binds workloads
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

### The shared preset, deferred — the design it would need if it returns

**This section describes what kelson no longer renders.** `preset: shared` is deferred (owner
decision, 2026-08-13, [ADR-0007](adr/0007-data-services.md)): the shared cluster was ADR-0007's
cost optimization, never something anyone asked for, and dedicated clusters per postgres
component are simpler, work today including bindings, and the pod cost is acceptable at this
stage. What follows is preserved as the design record for whoever picks
[#93](https://github.com/dafrie/kelson/issues/93) back up, not as current behaviour; the
rendering code itself is in git history.

`shared` would render one `Database` CR and no cluster: the shared cluster would be kelson-owned
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
- **`metadata.name`, `spec.name` and `spec.owner` are all `<project>-<environment>-<component>`.**
  In a cluster shared across projects, a database called `db` collides on the first collision;
  the qualified name is what makes the shared topology safe. The same name is used for the
  dedicated presets' `Cluster` so there is one naming rule rather than two.
- **`databaseReclaimPolicy: retain`** is CNPG's default, written explicitly because it *is*
  ADR-0007's asymmetric-retention promise: removing a service from a spec must never drop a
  database.

Provenance labels and annotations are stamped exactly as on every other resource, including
`kelson.dev/project` and `kelson.dev/environment` — which is how a human looking at
`kelson-data` can tell whose database is whose, and how a future uninstall can find them.

**What `shared` did not do even while it rendered: credentials.** The `Database` CRD creates a
database, not a role, and generates no Secret. The owner role and its password would live in the
shared cluster, and the Secret holding them would live in `kelson-data` — while the workload
that needs it runs in the project's namespace. Kubernetes Secrets are namespaced and a pod
cannot `secretKeyRef` across a namespace boundary, so binding to a `shared` service needed a
credential *distribution* step (a role provisioned in the shared cluster, its Secret projected
into the consuming namespace) that belonged to #93 and never existed — every binding refused.
Now the preset itself refuses before rendering anything, for the same reason and the same issue,
so this credential gap is moot until #93 revisits the preset, not only the binding.

---

## Sizing defaults

**`kind: postgres`.** The cache table is under [Cache sizing](#cache-sizing).

The preset **is** the sizing. There is no `resources:` override on a service in v0 — see
[Tuning](#tuning-without-leaving-the-preset-model). That holds for both kinds.

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

**`kind: postgres`.** Every cache transition is safe and none of them preserves anything: a
kelson cache holds nothing durable, so changing a preset re-tunes it in place where only
`maxmemory` moved, and otherwise reshards it and starts it cold. There is no table to write
because there is no data to lose.

Preset is an Environment-level override (ADR-0007, rule P5), so changing one is a two-character
spec edit. For a database these are not equally cheap.

| From → to | Verdict | What happens |
|---|---|---|
| `small` → `ha-small` | **safe, in place** | `instances: 1 → 3`; CNPG clones the primary into two standbys and turns on synchronous replication. No downtime, no data movement by kelson. |
| `ha-small` → `ha-medium` | **safe, in place** | Resource requests grow and the PVCs are resized upward. Rolling restart per instance. |
| `ha-medium` → `ha-small` | **safe, lossy of headroom** | Requests shrink; **storage does not** — `spec.storage.size` cannot be decreased, so the PVCs stay at the larger size and the cost stays with them. |
| `ha-*` → `small` | **safe, lossy of replicas** | `instances: 3 → 1`; the standbys and their PVCs are removed and synchronous replication stops. The data survives on the primary; the *availability* does not, and neither does the read capacity. |
| `shared` → any dedicated | **moot while deferred** ([#93](https://github.com/dafrie/kelson/issues/93)) | `shared` does not render, so there is nothing to migrate away from. Were it to return: different cluster, different storage, so the database would have to be dumped and restored, or replicated and cut over. [#105](https://github.com/dafrie/kelson/issues/105). |
| any dedicated → `shared` | **moot while deferred** ([#93](https://github.com/dafrie/kelson/issues/93)) | Same reason, in reverse: `shared` refuses to render, so nothing can transition onto it. Were it to return: same migration, plus a name change — the database becomes `<project>-<environment>-<component>` inside the shared cluster instead of `app` inside its own. |
| anything → `branch` | **not a transition** | A branch is a *new* service bootstrapped from a source, with a TTL and a lifecycle of its own (ADR-0007). Rewriting an existing service's preset to `branch` is not a topology change, it is a different object. [#99](https://github.com/dafrie/kelson/issues/99). |

**What is enforced today: nothing beyond validation and the capability check.** The renderer is
pure and stateless — it sees the spec it is asked to render, not the one that is already
deployed — so it cannot tell an in-place bump from a migration. The place that *can* is the
preview/diff plane, which compares rendered output against live state; a `shared` → `small`
edit shows up there as a `Database` deleted and a `Cluster` created, which is at least visible.
Turning that into a refusal with a named migration path is #105's job, and this table is the
specification it should implement.

---

## Backups

**`kind: postgres`.** A cache has nothing durable to back up, and that is a design decision
rather than a gap — see [Persistence is off](#persistence-is-off-and-what-a-durable-store-would-take).

Deferred — "coming soon" (owner decision, 2026-08-13, [ADR-0007](adr/0007-data-services.md)).
Nothing here renders yet: no `Backup`, no `ScheduledBackup`, no WAL archiving, no PITR.
[#94](https://github.com/dafrie/kelson/issues/94)-[#96](https://github.com/dafrie/kelson/issues/96)
track it.

The near path, when it lands, is CloudNativePG's declarative volumeSnapshot-based `Backup` and
`ScheduledBackup` resources on snapshot-capable storage — the same storage-class detection
`internal/clusterprofile/storage` already does for branching
([#91](https://github.com/dafrie/kelson/issues/91)) feeds directly into whether a cluster can be
offered scheduled snapshot backups, so that detection work is not duplicated. Object-store WAL
archiving and continuous PITR, the fuller promise ADR-0007's "Backups are configured once"
describes, is a later addition once the snapshot path is in place.

---

## Valkey

`kind: valkey` renders one `valkey.io/v1alpha1` `ValkeyCluster` per component, delegated to
[valkey-io/valkey-operator](https://github.com/valkey-io/valkey-operator) —
[ADR-0015](adr/0015-valkey-operator.md) records why that operator and not one of the four
alternatives, and states the negatives as plainly as this page does.

```yaml
apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: checkout-production-cache
  namespace: checkout-prod
spec:
  shards: 1
  replicas: 0
  resources:
    requests: {cpu: 250m, memory: 512Mi}
    limits: {memory: 512Mi}
  config:
    maxmemory: 384mb
    maxmemory-policy: allkeys-lru
```

That is the whole manifest. The Service, the ConfigMap, the per-pod `ValkeyNode` resources, the
PodDisruptionBudget and the ACL file are the operator's; kelson writes the topology request and
the two settings that decide what a full cache does.

### Cache sizing

| Preset | Shards | Replicas per shard | Pods | CPU request | Memory request | Memory limit | `maxmemory` |
|---|---|---|---|---|---|---|---|
| `small` | 1 | 0 | 1 | `250m` | `512Mi` | `512Mi` | `384mb` |
| `ha-small` | 3 | 1 | 6 | `250m` | `512Mi` | `512Mi` | `384mb` |
| `ha-medium` | 3 | 1 | 6 | `1` | `2Gi` | `2Gi` | `1536mb` |

**`maxmemory` is 75% of the container's memory limit, and the two numbers must never be the
same one.** `maxmemory` bounds the dataset. The process also needs room for replication
buffers, client output buffers, the copy-on-write pages a background save touches, and
allocator fragmentation, none of which that number counts. Setting `maxmemory` *at* the
container limit means the pod is OOM-killed exactly when the eviction policy was supposed to
start doing its job — the failure mode the setting exists to prevent, arriving on schedule.
`mb` in a Valkey config is binary, so `384mb` is 384 MiB and `1536mb` is 1.5 GiB.

**`maxmemory-policy: allkeys-lru`, not the default.** Valkey's default is `noeviction`: a full
cache starts returning errors on writes, which surfaces in the application as an outage caused
by a component whose entire promise was that losing it is cheap. `allkeys-lru` evicts the least
recently used key instead, across the whole keyspace rather than only the keys someone
remembered to set a TTL on. A cache that silently forgets is behaving correctly; a cache that
refuses writes is not.

Both keys are on the operator's live-settable allow-list, so a preset change re-tunes a running
cache with `CONFIG SET` rather than rolling its pods.

**Memory limit equals the request, and there is no CPU limit** — the same reasoning as the
Postgres presets, one step milder. An OOM-killed cache node is a cold cache rather than a
failover, but CPU throttling still turns a slow cache into slow *everything that waits on it*.

### Why an HA cache is six pods

This is the number that surprises people, and it is not a sizing choice.

The operator always runs Valkey in cluster mode (`cluster-enabled yes`, from its own base
config). In cluster mode a failover is decided by a **vote among primaries**. With a single
primary there is no quorum left to hold a vote once it dies, so a one-shard cache with one
replica would have a replica and no automatic failover — a promise kelson would be making on
the operator's behalf and the operator would not keep. Three shards is the smallest topology
where the vote can happen, so it is what `ha-small` and `ha-medium` render.

The consequence for applications: **`ha-*` needs a cluster-aware client.** Three shards means
the keyspace is split, and a plain client that does not follow `MOVED` redirects will fail on
two thirds of its keys. `small` is a single shard holding all 16384 slots, so no redirect is
ever issued and any Redis-compatible client works unchanged. Most mainstream clients
(`ioredis`, `go-redis`, `redis-py`, Lettuce) have a cluster mode; turning it on is the cost of
`ha-*`.

### Persistence is off, and what a durable store would take

**A kelson valkey component is a cache.** `spec.persistence` is not rendered at any preset, so
the operator gives each node an `emptyDir` instead of a PersistentVolumeClaim. A pod restart, a
node drain, a rollback, a preset change that rolls the pods — each of these starts the cache
empty and it refills from whatever it is a cache of. [#98](https://github.com/dafrie/kelson/issues/98)
frames that as the point rather than a limitation, and it is why the whole data-protection
apparatus around `kind: postgres` — backups, PITR, restore drills, branching — has no
counterpart here.

**If you are using it as a durable store, say so and do not use this.** Queues, sessions,
rate-limit counters that must survive a deploy, anything whose loss is an incident rather than
a latency spike — none of that is covered by a component kelson renders with persistence off,
and no amount of it working in staging changes that. The honest options are:

- **`kind: postgres`.** A queue or a session table in Postgres is durable, backed up, and
  already bound the same way. This is the right answer far more often than it looks.
- **An overlay patch against the rendered `ValkeyCluster`** ([model.md](model.md), rule P6),
  adding `spec.persistence` with a size and a storage class. That is visible and reviewable,
  and it also makes it explicit that you have left the preset model — kelson still renders
  `maxmemory-policy: allkeys-lru`, which for a store means keys are silently evicted under
  memory pressure, so a patch that adds persistence and does not also change the eviction
  policy has bought durability for data it is still allowed to throw away.

A first-class durable Valkey type is not a flag on this one; it is a different type with a
different ADR.

### Authentication: `auth:` and two commands

**A cache has no password unless you give it one.** Without `auth:`, anything that can reach the
cache's Service in its namespace can read and write it — kelson renders no `users:`, so Valkey's
own `default` user stays in place with `nopass`, and kelson renders no NetworkPolicy either, so
the namespace is the whole boundary. That was ADR-0015's largest negative and it is still the
behaviour of a component that says nothing.

`auth:` is how a component says something. It is two lines in the spec and one command before
the first apply:

```sh
kelson secret set cache-auth --project checkout --env staging --from-stdin password
```

```yaml
components:
  - name: cache
    kind: valkey
    preset: small
    auth: { secret: cache-auth, key: password }   # a name and a key, never a value
```

`--from-stdin password` reads the value from stdin so it never reaches your shell history;
`password=<value>` inline works too, and so does
`kubectl -n checkout-staging create secret generic cache-auth --from-literal=password=…` on a
machine with kubectl and no kelson. The Secret lives in the environment's namespace, so **the
same two spec lines are a different Secret in every environment** — write one per environment
under the same name and nothing in the Project changes.

That one name then reaches two places in the rendered set, and the password itself reaches
neither:

```yaml
spec:
  users:
    - name: default
      passwordSecret:
        name: cache-auth
        keys: [password]
      keys:     {readWrite: ["*"]}
      channels: {patterns:  ["*"]}
      commands: {allow:     ["@all"]}
```

```yaml
env:
  - name: CACHE_PASSWORD
    valueFrom:
      secretKeyRef: {name: cache-auth, key: password}
```

The operator reads the Secret, SHA-256-hashes whatever it finds and writes the hash into the ACL
file it mounts into every node; the kubelet projects the same key into the workload. kelson
reads it in neither direction — it cannot, being pure ([ADR-0001](adr/0001-hybrid-state-model.md))
— which is what makes this the reference model of [ADR-0018](adr/0018-secret-references.md) rather
than a second mechanism beside it. `auth:` is a `SecretRef`, the same type an env value's
`{secret, key}` decodes into, with the same validation.

Three consequences worth knowing before you use it:

- **It redefines `default`, it does not add a user.** An ACL file that never mentions `default`
  leaves the built-in `on nopass ~* &* +@all` exactly as it was, so a *new* user would add a
  password nobody is obliged to use and the cache would still be open. Redefining `default` is
  the edit that closes the hole. The permissions written are the ones `default` already had — all
  keys, all channels, all commands — restated because an ACL line replaces a user's rules rather
  than adding to them. The change is authentication and nothing else.
- **The value's format is free.** The operator hashes plaintext; a value that is already a
  `#`-prefixed 64-character SHA-256 digest is used as-is. `kelson secret set` needs no special
  shape, and nothing has to hash anything on kelson's side.
- **The Secret must exist before the cluster reconciles.** With `auth:` set and the Secret or the
  key missing, the operator refuses to build the ACL (`no password or reference found`, or
  `missing password key in secret`) and the cache does not become ready. kelson cannot check this
  — validation has no cluster and the renderer must not read one — so the order is: write the
  Secret, then apply. Adding `auth:` to a cache that is already running is applied live with
  `ACL LOAD` and does not roll its pods.

### Cache bindings

`model.ServiceKeys` declares four keys for `kind: valkey`. Three are **plain env values**; the
fourth is a `secretKeyRef` when the component declares `auth:` and a structured refusal when it
does not:

| kelson key | Rendered value |
|---|---|
| `host` | `valkey-<cluster>.<namespace>.svc` |
| `port` | `"6379"` |
| `uri` | `redis://valkey-<cluster>.<namespace>.svc:6379` — **no password, ever** |
| `password` | `secretKeyRef{name: <auth.secret>, key: <auth.key>}`, or a structured error naming `auth:` and `kelson secret set` when the component has none |

`valkey-<cluster>` is the headless Service the operator creates. Plain values rather than a
Secret because a Service name and a port are not credentials: ADR-0009 forbids a *secret* in a
spec, and minting a Secret to hold a hostname would obey the letter of that while making the
manifest harder to read.

**The URI never carries the password, and this does not change when `auth:` is set.**
`redis://:<password>@host:6379` would be a credential written into a container's `env[].value`,
in a manifest that is diffed, previewed, stored in the delivery repository and read back through
the API — precisely the thing ADR-0009 forbids and ADR-0018 makes structurally impossible. So
`uri` stays the address form at every setting, and **an authenticated client assembles its
connection from the parts**: bind `host`, `port` and `password` (or `uri` *and* `password`, which
every mainstream client accepts as a URL plus a separate credential option) and hand them to the
client constructor. There is no spelling for a URI with the password in it, which is the same
reason there is no string interpolation in an env value.

**The URI scheme is `redis://` on purpose.** Valkey is wire- and URL-compatible with Redis, and
`redis://` is what the client library an application already has will parse. Emitting
`valkey://` would name the product correctly and be rejected by most of them, which is the
wrong trade for a value whose only job is to be handed to a client constructor.

**There is no `username` key**, and `auth:` is why there does not have to be one: the user kelson
renders is `default`, so `AUTH <password>` authenticates it and no client needs to be told a name.

**`auth:` is `kind: valkey` only.** On `kind: postgres` it is refused
(`schema/mutually-exclusive`): CloudNativePG's `initdb` bootstrap *generates* the application
user and its password and publishes them as `<cluster>-app`, so a Secret an author wrote would be
a second credential the database never accepts. Bind `password` there and kelson points the
`secretKeyRef` at the one the operator made. On a workload or a `kind: helm` component it is
refused too, and the remediation names the spelling that does work in those places — an env value
written `{secret: <name>, key: <key>}`.

### Naming

The same `<project>-<environment>-<component>` rule as everything else, capped at **37**
characters rather than 53. The operator derives `internal-<cluster>-system-passwords` for its
own system users, which is 26 characters of prefix and suffix around the name kelson chose, and
the result still has to fit the 63-character DNS label limit. A longer name is a structured
render error, not a manifest the API server rejects with an arithmetic complaint.

---

## Validation versus capability

A preset is a request; whether a cluster can serve it is a separate question with **three**
answers, not two ([`clusterprofile.Outcome`](https://github.com/dafrie/kelson/issues/144)).
`postgres.SupportsPreset(profile, preset)` gives the verdict for a database and names the
capability that decided it; `valkey.SupportsPreset` is the same function against the other
operator, and the two are independent — a cluster can host a cache and not a database, or the
reverse, and each refusal names its own operator and its own install path.

The valkey capability table has two entries where the postgres one has four, and both of them
are a served CRD rather than a version floor: `valkeyclusters.valkey.io` is the API kelson
writes, and `valkeynodes.valkey.io` is what the operator materialises each pod through. They
are separate answers because a partially applied CRD set serves the first and not the second,
which means the manifest is accepted, no error is reported, and no pod is ever created — the
silent half-success a capability check exists to catch.

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
| **No** | Refuse: `render/postgres-unsupported` or `render/valkey-unsupported`, naming the blocking capability, the version floor, the detected version, and the remediation (always *upgrade the operator you have* — both operators are cluster-scoped and a second install fights the first). Two codes rather than one because the remediation is a different operator, and a caller switching on the code should not have to parse prose to tell which. |
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

A workload component binds to a data component by key: `env: {DATABASE_URL: {from: {service: db, key: uri}}}`.
Validation checks the key against `model.ServiceKeys`; the renderer resolves it against the
bound service, in one of three ways. A **credential** becomes a `secretKeyRef` against a Secret
the operator generated — for postgres, `<cluster>-app` (type `basic-auth`). A **connection
detail that is not a credential** becomes a plain value, which is how a cache's address binds. A
key the kind declares that the service genuinely cannot supply is
`render/binding-unavailable-key`, with the reason rather than a list of alternatives that does
not contain the answer — today that is exactly one key, and only in one configuration:
[a cache's `password` when the component declares no `auth:`](#authentication-auth-and-two-commands).
With `auth:` set it is a credential like any other, and the Secret it resolves against is one the
author named rather than one an operator generated — the first binding where those two differ.

**A binding is one of the two reference forms, not a separate mechanism.** The other is
`{secret: <name>, key: <key>}`, which names a Secret kelson does not manage
([ADR-0018](adr/0018-secret-references.md), [docs/model.md](model.md#secrets-references-never-literals)).
Both are mappings in an env value, both render into the same `valueFrom.secretKeyRef` through
the same function, and neither can carry a value. What differs is who names the Secret: a
binding names a *component* and kelson derives the Secret its operator generates; a reference
names the Secret directly. So a credential that is not a data service kelson manages — an API
key, an SMTP password — is written the same way, in the same map, beside the binding.

The postgres mapping follows.

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

For each data component, in spec order, **after the Namespace and before any workload** — a
workload that binds to a data component should not be applied before the resource that produces
its credentials. The one `spec.components` list of [ADR-0014](adr/0014-components.md) does not
change this: the data components render first whatever position the author gave them in the list,
because ordering is the only sequencing a rendered set can express.

```
Namespace
  postgresql.cnpg.io/v1 Cluster        (kind: postgres, small | ha-small | ha-medium)
  valkey.io/v1alpha1 ValkeyCluster     (kind: valkey,   small | ha-small | ha-medium)
  ServiceAccount / Service / Deployment / … per workload component
```

(`shared` would have rendered a `postgresql.cnpg.io/v1 Database` here; it is deferred — see
[Preset → topology](#preset--topology).)

Ordering is advisory rather than a guarantee — nothing waits for the cluster to be ready, and a
workload whose database is still bootstrapping will crash-loop until it is. That is the same
deal every other resource in the set gets, and it is visible in `kelson status`.

Names are `<project>-<environment>-<component>`, capped at 53 characters for `kind: postgres`
and 37 for `kind: valkey`. CloudNativePG derives its own object names from the cluster's —
`<cluster>-app`, `<cluster>-superuser`, `<cluster>-rw`, `<cluster>-1` — and the longest suffix
(`-superuser`, ten characters) has to fit inside the 63-character DNS label limit; the Valkey
operator's longest derivation is `internal-<cluster>-system-passwords`, which is 26. Two caps
rather than one because the operators derive different names, not because there are two
opinions about names. A longer name is a structured render error rather than a resource the API
server rejects with an arithmetic complaint.

Provenance is identical to every other rendered resource: `app.kubernetes.io/managed-by`,
`kelson.dev/project`, `kelson.dev/environment`, `kelson.dev/renderer-version` and a
`kelson.dev/spec-hash` computed over the data component's own resolved input, so an unrelated edit
elsewhere in the spec does not churn the database's annotation.

Data components carry no `kelson.dev/application` label and render no ServiceAccount of their own.
They are not owned by one workload — that is what makes them bindable by several — and the identity
their pods run under is the operator's to create, not kelson's
([ADR-0005](adr/0005-delegate-to-operators.md), [ADR-0014](adr/0014-components.md) decision D).

---

## Status

| Piece | State |
|---|---|
| `small`, `ha-small`, `ha-medium` → `Cluster` | rendered |
| bindings against a dedicated preset | rendered (`secretKeyRef` → `<cluster>-app`) |
| `preset: shared` | **deferred, structured error** on both kinds, [#93](https://github.com/dafrie/kelson/issues/93) (owner decision, 2026-08-13) |
| `preset: branch` | **structured error**, [#99](https://github.com/dafrie/kelson/issues/99); refused for `kind: valkey` as not a cache topology |
| `kind: valkey` `small`, `ha-small`, `ha-medium` → `ValkeyCluster` | rendered ([ADR-0015](adr/0015-valkey-operator.md)) |
| bindings against a cache | rendered as plain values (`uri`, `host`, `port`) |
| `auth: {secret, key}` on `kind: valkey` → `spec.users[]` | rendered ([ADR-0015 amendment](adr/0015-valkey-operator.md#amendment-2026-08-14--auth-a-cache-with-a-password), [#98](https://github.com/dafrie/kelson/issues/98)) |
| a cache's `password` binding | `secretKeyRef` against the `auth:` Secret; **structured error** naming `auth:` when the component declares none |
| `auth:` on postgres, on a workload, on a chart | **structured error** `schema/mutually-exclusive`, each naming what to write instead |
| a URI with the password in it | **not authorable and never rendered** — `uri` is the address form at every setting (ADR-0009, ADR-0018) |
| valkey persistence, TLS, per-application ACL users, external access | not authorable; the operator supports them, kelson renders only the `default` user and only when `auth:` is set |
| backups, WAL archiving, PITR | deferred, [#94](https://github.com/dafrie/kelson/issues/94)-[#96](https://github.com/dafrie/kelson/issues/96); see [Backups](#backups). No counterpart for `kind: valkey` — it has nothing durable to back up |
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

valkey-io/valkey-operator, read the same way at `main` and at tag `v0.5.0`:

- `api/v1alpha1/valkeycluster_types.go` — `shards`, `replicas` ("replicas for each shard
  group"), `resources`, `users`, `config` ("additional Valkey configuration parameters"),
  `persistence`, `exporter`, `podDisruptionBudget`, `networking`, in that field order.
- `api/v1alpha1/persistence_types.go` — `PersistenceSpec` is a pointer, so an omitted
  `persistence` is no PVC at all.
- `api/v1alpha1/valkeyacls_types.go` — `UserAclSpec` (`name`, `enabled` defaulting to true,
  `passwordSecret`, `nopass`, `resetpass`, `commands`, `keys`, `channels`, `permissions`) and
  `PasswordSecretSpec` (`name`, `keys []string`), with the one validation rule: a username may
  not start with `_`. `docs/valkeycluster.md#users` is the field reference for the same shape.
- `internal/controller/users.go` — `PasswordSecretSpec.Name` defaults to `<cluster>-users` and
  `Keys` to the username; a Secret's value is SHA-256-hashed unless it is already a
  `#`-prefixed 64-hex digest; a missing Secret or key fails the reconcile; the Secret is only
  ever *read*, and the only generated one is `internal-<cluster>-system-passwords`, for the
  operator's own `_`-prefixed system users. `buildUserAcl` is the exact ACL line each user
  becomes, which is how `keys`/`channels`/`commands` were checked to reproduce `default`'s own
  `~* &* +@all`.
- `internal/controller/valkeynode_resources.go` — the probes authenticate as `_operator` with
  `VALKEY_USER`/`VALKEYCLI_AUTH`, not as `default`, so giving `default` a password does not
  break the operator's own health checks.
- `internal/controller/config.go` — `cluster-enabled yes` in the base config, and the
  live-settable allow-list containing `maxmemory` and `maxmemory-policy`.
- `internal/controller/valkeycluster_controller.go` — the headless `valkey-<cluster>` Service
  on port 6379, and `getSystemPasswordSecretName`, which is where the 37-character name cap
  comes from.
- `docs/quickstart.md`, `docs/valkeynode-design.md`, `README.md` (the early-development
  notice).
