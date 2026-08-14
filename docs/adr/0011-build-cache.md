# ADR-0011: Build cache is a registry cache, scoped per application

- **Status:** Deferred
- **Date:** 2026-08-12

!!! warning "Deferred — not a decision yet"

    Caching is an optimisation, and kelson does not yet work end to end. Building it now
    adds a second registry repository, a second credential scope and a new failure surface
    to a path that has never run against a real cluster, in exchange for speed nobody can
    measure yet. The cache code has been removed from the buildpacks driver and every build
    is cold; see [#52](https://github.com/dafrie/kelson/issues/52).

    Still deferred now that the buildpacks driver actually runs
    ([#49](https://github.com/dafrie/kelson/issues/49)). What changed is only that the cost
    is now paid rather than hypothetical: a cold lifecycle build pulls a builder image and
    every dependency layer on every run, and the `-launch-cache` the driver mounts is an
    `emptyDir` that dies with the pod, which is the lifecycle's scratch space and not a
    cache that survives anything.

    This ADR also needs correcting before it can be accepted. It argues its decision almost
    entirely in BuildKit's vocabulary, and [ADR-0010](0010-build-strategy.md) makes Cloud
    Native Buildpacks the eventual *default*. The two have different caching mechanisms:
    BuildKit exports build steps with `--export-cache type=registry`, while the CNB
    lifecycle caches buildpack-declared dependency layers with `-cache-image`. **`mode=min`
    and `mode=max` are BuildKit concepts with no CNB equivalent** — the "Cache export uses
    `mode=min` by default" decision below cannot be expressed under Buildpacks at all, and a
    `CacheMode` knob written from this ADR sat in the buildpacks driver validated, defaulted
    and tested while never reaching the lifecycle.

    The document also omits CNB's largest speed-up, which is not a cache in this ADR's sense:
    the lifecycle's `analyze`/`restore` phases reuse layers from the *previous app image*.
    That strengthens the isolation argument, since image repositories are already scoped per
    application, and it makes the "a registry outage degrades builds to cold" consequence
    more severe than stated.

    What survives review is the general part: cache belongs in a registry, scoped per
    `(project, application)`, never shared across projects, evicted by registry retention.
    The per-strategy mechanism has to be stated separately.

## Context

Build time is a primary DX metric and the most common complaint about zero-config builders
([#52](https://github.com/dafrie/kelson/issues/52)). Today there is no cache at all: the build Job mounts
`buildkit-state` as an `emptyDir`, so every build is cold and the state dies with the pod.

Caching is also where multi-tenancy gets sharp. A shared cache across tenants is a cross-tenant
information leak; a per-tenant cache multiplies storage. The two acceptance criteria pull against each
other on purpose.

Three backends were considered.

**Registry cache** (`--export-cache type=registry`). BuildKit pushes cache blobs to a registry
repository. Build pods stay ephemeral and rootless, because the cache lives outside them. kelson already
has registry references, digest handling and credential resolution
([#51](https://github.com/dafrie/kelson/issues/51)), so this backend adds no new infrastructure and no
new secret path. Isolation becomes a naming-plus-authentication property, which is auditable. It is
slower than a local disk cache, because blobs cross the network.

**PVC cache.** A persistent volume mounted into the build pod. The fastest option, and the worst fit.
`ReadWriteOnce` serialises builds per volume; storage multiplies per tenant; eviction has to be built
from nothing; and it reintroduces a long-lived writable volume shared across builds — the exact shape
that makes a cross-tenant leak possible. It is also weakest on the default bootstrap path, where k3s
local-path has no snapshot driver ([#108](https://github.com/dafrie/kelson/issues/108)).

**S3-compatible cache** (`type=s3`). Scales well and evicts by lifecycle policy, but requires an object
store that a self-hosted user may not have, plus a second credential path. It is the right answer at a
scale kelson does not have yet.

## Decision

**Registry cache, scoped per `(project, application)`, is the default.** The cache reference is derived
from the same identity that owns the image repository, and a build receives credentials for its own
cache reference only. There is no global or shared cache reference, ever — not as an option, not as an
optimisation.

**Cache is shared across environments of one application, and never across projects.** A preview
environment therefore starts warm from the parent project's cache, which is where cache warming
([#103](https://github.com/dafrie/kelson/issues/103), M10) comes from for free. Crossing environments is
not a tenancy boundary; crossing projects is.

**Cache export uses `mode=min` by default.** `mode=max` exports intermediate layers, which can contain
build-time file contents and source that never reach the final image. That makes a `max` cache as
sensitive as the source tree while looking like a build artifact. `max` stays available per application
for users who want the faster rebuild and understand what they are publishing.

**Cache repositories carry the same access control as image repositories,** and the docs must say
plainly that cache contents are as sensitive as the images they build.

**Eviction is registry retention**, handled by the same mechanism as image retention (#51), not by a
second bespoke system.

## Rationale

The decision follows from taking the leak criterion as binding rather than aspirational. A shared cache
is the single largest available speed-up and it is refused, because "cache contents cannot leak between
tenants" cannot be satisfied by a cache that is shared and then filtered — filtering is a mitigation, and
mitigations fail.

Choosing the backend kelson already has credentials and references for means cache isolation reuses an
access-control path that is already tested, instead of inventing a second one. Reusing registry retention
for eviction means there is one retention story rather than two.

## Consequences

**Negative, stated plainly:**

- **A registry cache is slower than a local disk cache.** Every cache hit is a network fetch. Users
  comparing kelson against a single-tenant builder with a warm local disk will see kelson lose, and that
  comparison is fair.
- **Cache storage costs registry storage**, and it grows per application. Retention is therefore not
  optional; without it this quietly becomes a bill.
- **`mode=min` caches less than `mode=max`**, so the default is the slower of the two. It is chosen
  because the faster default would publish intermediate layers most users would not expect to publish.
- **A registry outage degrades builds to cold** rather than failing them. That is the right behaviour,
  but it makes build times bimodal and confusing at exactly the wrong moment.
- Users with no external registry get no cache until the in-cluster registry option (#51) exists.

**Positive:**

- Build pods stay ephemeral, rootless and stateless — no long-lived writable volume shared across
  builds, which removes the leak vector rather than guarding it.
- Preview environments start warm with no separate warming mechanism.
- No new infrastructure, no new credential path, no second retention system.

**Verification.** *"A no-op rebuild completes substantially faster than a cold build"* cannot be proven
by a unit test; it needs a real registry and a real build, so it belongs to the end-to-end harness
([#86](https://github.com/dafrie/kelson/issues/86)). *"Cache contents cannot leak between tenants"* is
testable earlier and should be: the cache reference derivation is a pure function, and a test must assert
that two different projects can never produce the same cache reference.

**Not decided here:** cache hit-rate metrics. Hit rates are read from buildctl's output, which is
scraped text and therefore brittle; that belongs with the log-streaming work
([#54](https://github.com/dafrie/kelson/issues/54)) where output parsing already lives.
