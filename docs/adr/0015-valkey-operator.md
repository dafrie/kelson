# ADR-0015: `kind: valkey` delegates to valkey-io/valkey-operator

- **Status:** Accepted
- **Date:** 2026-08-13

## Context

[ADR-0005](0005-delegate-to-operators.md) makes every managed data type operator-backed with no
degraded mode: kelson owns the application-facing abstraction and an upstream operator owns the
topology. It names "a Valkey operator" for `kind: valkey` and leaves the choice open.
[#98](https://github.com/dafrie/kelson/issues/98) is where the choice has to be made, and it adds
requirements of its own — persistence off by default, memory limits and an eviction policy made
explicit, and *"adding a Valkey service and referencing it yields a working connection with no manual
credential handling"*.

Three constraints narrow the field before any operator is looked at:

- **The renderer is pure** ([#20](https://github.com/dafrie/kelson/issues/20)). It has no random
  source, so it cannot invent a password. Credentials must come from the operator, exactly as
  CloudNativePG's `initdb` bootstrap publishes `<cluster>-app` for `kind: postgres`.
- **No vendored charts** (ADR-0005). kelson writes a CR; it does not template an engine.
- **A cache is not a database.** Losing one refills it. That changes what a risk is worth here.

The 2026 landscape was surveyed directly from the repositories, not from memory.

| Operator | API | License | State as of 2026-08 |
|---|---|---|---|
| [valkey-io/valkey-operator](https://github.com/valkey-io/valkey-operator) | `valkey.io/v1alpha1` `ValkeyCluster` | Apache-2.0 | The Valkey project's own, under Linux Foundation governance. Commits daily, weekly public tech call, latest tag v0.5.0. README says **not ready for production**, API may change. |
| [hyperspike/valkey-operator](https://github.com/hyperspike/valkey-operator) | `hyperspike.io/v1` `Valkey` | Apache-2.0 | Stable v1 CRD and the only operator that **generates the credential Secret itself**. But `main` last moved 2026-01-27, `spec.replicas` is documented upstream as creating extra primaries (their #186), and there is no config passthrough at all: `valkey.conf` is baked from an embedded template that sets `save 900 1` and no `maxmemory`. |
| [SAP/valkey-operator](https://github.com/SAP/valkey-operator) | `cache.cs.sap.com/v1alpha1` `Valkey` | Apache-2.0 | Actively maintained and writes a ready-made binding Secret (`host`, `port`, `password`). Wraps a vendored Bitnami chart whose images it now pulls from `bitnamilegacy/`, the archive Broadcom created when the free Bitnami catalog was withdrawn in 2025 and which receives no further updates or patches. |
| [chideat/valkey-operator](https://github.com/chideat/valkey-operator) | `rds.valkey.buf.red/v1alpha1` | Apache-2.0 | Standalone/sentinel/cluster and a `customConfigs` map. One maintainer, ~20 stars. |
| others (`smoketurner`, `littlered`, `wellcake`, `KeiaiLab`, `halter`, `InditexTech`, `alauda`) | various | mixed | All under a year old and/or under 30 stars. Not a base to make a managed type out of. |

## Decision

**`kind: valkey` renders one `valkey.io/v1alpha1` `ValkeyCluster` per component, delegated to
[valkey-io/valkey-operator](https://github.com/valkey-io/valkey-operator).** The declared support
floor is **0.5.0** (`internal/clusterprofile/support`), the release whose CRD shape kelson writes.

Detection records the operator as its own profile finding — version, namespace, and the resources the
API server serves — and `internal/clusterprofile/valkey` turns that into a per-capability verdict, the
same shape `internal/clusterprofile/postgres` has for CNPG. An absent operator is a structured render
refusal naming the operator and the install path, never a degraded render.

~~**kelson renders no ACL user, and therefore no `password` binding.** The operator reads application
user passwords from a Secret it never creates; a pure renderer cannot create one either. A cache
therefore runs with Valkey's own `default` user — the operator's own default, and what its quickstart
documents — and the `password` key of `model.ServiceKeys[valkey]` is a structured refusal that says
so.~~ **Superseded by the [2026-08-14 amendment](#amendment-2026-08-14--auth-a-cache-with-a-password):
a component that declares `auth: {secret, key}` renders the ACL user against that Secret and the
`password` binding resolves. A component that declares none still behaves exactly as struck through
above, which is why the text is struck rather than deleted.** `uri`, `host` and `port` render as
plain env values at every setting, because a Service name and a port are not credentials.

## Rationale

- **`spec.config` is the deciding feature.** It is the only surface among the mature candidates that
  lets kelson state `maxmemory` and `maxmemory-policy` per preset, which #98 asks for explicitly. Both
  keys are on the operator's live-settable allow-list, so changing a preset re-tunes a running cache
  instead of rolling it. hyperspike cannot express either at any price.
- **`spec.persistence` omitted means an emptyDir, not a PVC.** Persistence-off-by-default is a field
  kelson does not write, rather than a setting it has to fight. hyperspike's baked config has RDB
  snapshots on and no way to turn them off.
- **It is the project's own operator.** Every alternative is one vendor or one person; this one has
  Linux Foundation governance, a public weekly call, and the people who write Valkey. Over the life of
  a managed type that outweighs a version number.
- **Its licence is Apache-2.0 and nothing of it is imported.** kelson writes YAML against a documented
  CRD; no Go module of theirs enters this repository, so the licence question stays simple.
- **SAP's binding Secret was the closest fit and its supply chain disqualified it.** Pointing kelson's
  users at an operator whose data-plane images come from an explicitly unmaintained archive is a worse
  bargain than an alpha API on a cache.

## Consequences

**Positive.** One CR, no chart, no credential distribution. Memory limit and eviction policy are
explicit in the rendered manifest and reviewable. Persistence is off by default and its absence is
visible. The capability judgement, the tri-state outcome and the refusal shape are all reused from the
CNPG slice rather than reinvented.

**Negative — stated as plainly as the positives.**

- **The API is `v1alpha1` and upstream says it is not production-ready.** kelson accepts that *for this
  type only*, because a cache's failure mode is that it refills. It would not be an acceptable trade
  for `kind: postgres`, and this ADR is not a precedent for one. The support floor will move with the
  operator, and a floor bump is a refusal for clusters below it.
- ~~**A kelson cache has no password.** Anything that can reach its Service in the namespace can read
  and write it. kelson renders no NetworkPolicy, so the namespace boundary is the whole boundary. This
  is documented in `docs/data-services.md` rather than left to be assumed either way, and it is the
  single largest reason to revisit this decision.~~ **Struck 2026-08-14** — a cache can have a
  password now; see the amendment below for what remains true (no NetworkPolicy; a cache without
  `auth:` is still open).
- **HA costs six pods.** The operator always runs Valkey in cluster mode, and cluster-mode failover is
  a vote among primaries — a single-shard "replicated" cache would have a replica and no failover. The
  replicated presets are therefore three shards with one replica each, and applications binding to
  them need a cluster-aware client. The `small` preset is one shard and works with any Redis client.
- **A cache is not a durable store, and kelson will not pretend otherwise.** Queues and sessions that
  must survive a restart need `spec.persistence`, which kelson does not render; the honest options are
  an overlay patch against the rendered `ValkeyCluster`, or `kind: postgres`.

## Amendment (2026-08-14) — `auth:`, a cache with a password

This ADR's second *Revisit when* has happened, from the direction it named second: the operator
still generates no application credential, but kelson's secrets plane can now supply one.
[ADR-0018](0018-secret-references.md) standardised the reference shape `{secret, key}` and
[#116](https://github.com/dafrie/kelson/issues/116) shipped `kelson secret set`, the command that
writes the Secret a reference names. What was missing was the piece in between, and it is
[#98](https://github.com/dafrie/kelson/issues/98)'s remaining acceptance criterion: *"adding a Valkey
service and referencing it yields a working connection with no manual credential handling."*

### The field

A `kind: valkey` component gains one optional field:

```yaml
- name: cache
  kind: valkey
  preset: small
  auth: { secret: cache-auth, key: password }
```

It is a `model.SecretRef` — the same Go type, the same two keys, the same validation as the env
value `{secret: <name>, key: <key>}` — because it is the same promise: a name kelson resolves for
nobody, pointing at a Secret it does not create, read, diff or own. Absent, nothing changes and the
struck-through paragraphs above are still the behaviour. Present, three things follow.

**One.** The rendered `ValkeyCluster` gains `spec.users`, verified against the operator's API at
[`api/v1alpha1/valkeyacls_types.go`](https://github.com/valkey-io/valkey-operator/blob/main/api/v1alpha1/valkeyacls_types.go)
and documented at
[`docs/valkeycluster.md#users`](https://github.com/valkey-io/valkey-operator/blob/main/docs/valkeycluster.md#users):

```yaml
users:
  - name: default
    passwordSecret: {name: cache-auth, keys: [password]}
    keys:     {readWrite: ["*"]}
    channels: {patterns:  ["*"]}
    commands: {allow:     ["@all"]}
```

**Two.** The `password` key of `model.ServiceKeys[valkey]` stops being a refusal and resolves to
`secretKeyRef{name: cache-auth, key: password}` — through the *existing* binding path, with no new
branch: the component simply gains a `secret` and a `keys` entry in `boundService`, which is what
`valkeyWithheldKeys` was written as data to allow.

**Three.** One Secret has two consumers and the value has none. The operator reads it and hashes it
into the ACL file; the kubelet projects it into the workload. kelson is on neither path and could not
be — the renderer is pure — so ADR-0018's guarantee is not weakened by this: the typed path still
cannot leak, because there is still no stage that resolves a reference into a value.

### Why `default` and not a new user

An ACL file that never mentions `default` leaves Valkey's built-in `default` exactly as it is:
`on nopass ~* &* +@all`. So rendering a *new* named user would have added a password nobody is
obliged to use, and the cache would have stayed open to anything that skips the AUTH — the negative
this amendment exists to strike would have survived it, quietly, behind a manifest that looked
authenticated. **Redefining `default` is the only edit that closes it.**

It also keeps the binding vocabulary honest. `model.ServiceKeys[valkey]` is `uri`, `host`, `port`,
`password` — there is no `username`, and adding one to carry an invented name would be a model change
in service of a decision nobody asked for. `AUTH <password>` against `default` is what every
Redis-compatible client can do without being told a name.

The permissions written are the ones `default` already had, restated because an ACL line replaces a
user's rules rather than adding to them. The change is authentication and nothing else: the same
cache, with a password. The operator's own probes authenticate as its `_operator` system user
(`VALKEY_USER`/`VALKEYCLI_AUTH` in `internal/controller/valkeynode_resources.go`), not as `default`,
so nothing of the operator's breaks when `default` acquires one.

### The `uri` binding stays passwordless — decided, not overlooked

`redis://:<password>@host:6379` was considered and **refused**. It would put a credential into a
container's `env[].value`: a literal secret value in a rendered manifest, which is diffed, previewed,
written to the delivery repository and read back through the API. That is exactly what
[ADR-0009](0009-secrets.md) exists to forbid and what ADR-0018 §4 claims is structurally impossible,
and the claim would have to be withdrawn to render it. It is not worth a shorter connection string.

So `uri` is the address form at every setting — `redis://host:port`, no userinfo — and an
authenticated client assembles its connection from parts: `uri` (or `host` + `port`) plus `password`,
which every mainstream client accepts as a URL plus a separate credential option. This is the same
answer, for the same reason, as ADR-0018's refusal of string interpolation: there is no syntax for
half a value, so a workload assembles the string or a Secret holds the whole of it.

### Scope, and what stays true

`auth:` is `kind: valkey` only. On `kind: postgres` it is refused with `schema/mutually-exclusive`:
CloudNativePG's `initdb` bootstrap *generates* the application user and its password and publishes
them as `<cluster>-app`, so an author-written Secret there would be a second credential the database
never accepts, sitting beside a binding that keeps resolving against the operator's. On a workload or
a `kind: helm` component it is refused too, and the remediation names the spelling that works there.

Three of this ADR's negatives are untouched by this amendment and are worth restating so the strike
above is not read as wider than it is:

- **kelson still renders no NetworkPolicy.** Authentication is not network isolation; what changes is
  that reaching the Service is no longer sufficient.
- **A cache without `auth:` is still open**, and that is a deliberate default rather than an
  oversight: turning authentication on for every existing cache would break every workload already
  bound to one, on an upgrade, with a `NOAUTH` the author did not ask for. The refusal on the
  `password` binding now names the two commands that turn it on, so the default is discoverable
  rather than silent.
- **The Secret must exist before the cluster reconciles.** With `auth:` set and the Secret or key
  missing, the operator refuses to build the ACL and the cache never becomes ready. kelson cannot
  check it — validation has no cluster, the renderer must not read one — which is ADR-0018's second
  negative consequence arriving in a new place.

## Revisit when

- ~~The operator generates an application user credential, or kelson's secrets plane
  ([ADR-0009](0009-secrets.md), M8) can supply one. Either makes the ACL user renderable and turns the
  `password` binding from a refusal into a `secretKeyRef` — `valkeyWithheldKeys` empties and nothing
  else in the renderer moves.~~ **Done 2026-08-14** by the second of the two, and it landed as
  predicted: the refusal moved into `keys` and no other part of the renderer changed. See the
  amendment above.
- **Somebody needs more than one cache user.** `auth:` renders exactly one, `default`, because that
  is what closes the hole and what the binding vocabulary can express. Per-application ACL users with
  key-pattern scoping are a thing the operator supports and kelson does not model, and asking for one
  is a spec-surface decision (a list, a name per entry, a `username` binding key), not a flag on this
  field.
- **The operator learns to generate the user Secret itself.** Then `auth:` becomes optional again in
  a second sense — a cache could be authenticated by default with no field at all — and the question
  is whether the field stays as the way to name a Secret you already have.
- The operator declares `v1beta1` or `v1`, or drops the not-production-ready notice. That is a floor
  bump and a re-read of this ADR's negatives, not a new decision.
- The operator gains a non-cluster (standalone or replication) mode, which would let the replicated
  presets cost two pods instead of six and drop the cluster-aware-client requirement.
- Anyone asks for a Valkey component as a durable store. That is a different type with a different
  ADR, not a flag on this one.
