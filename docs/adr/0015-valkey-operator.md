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

**kelson renders no ACL user, and therefore no `password` binding.** The operator reads application
user passwords from a Secret it never creates; a pure renderer cannot create one either. A cache
therefore runs with Valkey's own `default` user — the operator's own default, and what its quickstart
documents — and the `password` key of `model.ServiceKeys[valkey]` is a structured refusal that says
so. `uri`, `host` and `port` render as plain env values, because a Service name and a port are not
credentials.

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
- **A kelson cache has no password.** Anything that can reach its Service in the namespace can read
  and write it. kelson renders no NetworkPolicy, so the namespace boundary is the whole boundary. This
  is documented in `docs/data-services.md` rather than left to be assumed either way, and it is the
  single largest reason to revisit this decision.
- **HA costs six pods.** The operator always runs Valkey in cluster mode, and cluster-mode failover is
  a vote among primaries — a single-shard "replicated" cache would have a replica and no failover. The
  replicated presets are therefore three shards with one replica each, and applications binding to
  them need a cluster-aware client. The `small` preset is one shard and works with any Redis client.
- **A cache is not a durable store, and kelson will not pretend otherwise.** Queues and sessions that
  must survive a restart need `spec.persistence`, which kelson does not render; the honest options are
  an overlay patch against the rendered `ValkeyCluster`, or `kind: postgres`.

## Revisit when

- The operator generates an application user credential, or kelson's secrets plane
  ([ADR-0009](0009-secrets.md), M8) can supply one. Either makes the ACL user renderable and turns the
  `password` binding from a refusal into a `secretKeyRef` — `valkeyWithheldKeys` empties and nothing
  else in the renderer moves.
- The operator declares `v1beta1` or `v1`, or drops the not-production-ready notice. That is a floor
  bump and a re-read of this ADR's negatives, not a new decision.
- The operator gains a non-cluster (standalone or replication) mode, which would let the replicated
  presets cost two pods instead of six and drop the cluster-aware-client requirement.
- Anyone asks for a Valkey component as a durable store. That is a different type with a different
  ADR, not a flag on this one.
