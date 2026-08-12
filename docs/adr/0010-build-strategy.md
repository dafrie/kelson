# ADR-0010: Dockerfile when present, Cloud Native Buildpacks otherwise

- **Status:** Accepted
- **Date:** 2026-08-12

## Context

kelson must turn a repository into an image without asking the user to write build configuration, and
must choose the zero-config path deliberately rather than by inheritance. Evidence in
[docs/research/build-strategy-selection.md](../research/build-strategy-selection.md); the question is
[#47](https://github.com/dafrie/kelson/issues/47).

Three candidates are live. **Dockerfile via BuildKit** is unambiguous and covers most real repositories.
**Cloud Native Buildpacks** graduated from the CNCF on 17 July 2026 and offers `rebase`, which patches a
base image across a fleet without rebuilding application layers. **Railpack** — Railway's Go + BuildKit
successor to Nixpacks — produces markedly smaller images (38% for Node, up to 77% for Python versus
Nixpacks) and faster cold builds, but is single-vendor and still carries a beta designation.

Nixpacks is in maintenance. Coolify and Dokploy default to it. Repeating that choice by inertia is the
specific trap this decision exists to avoid.

## Decision

**Selection precedence, in order:**

1. An explicit `build.strategy` in the spec always wins. `none` means "use `image:`, build nothing".
2. A `Dockerfile` in the repository (or at `build.dockerfile`) selects **Dockerfile via BuildKit**.
3. Otherwise, **Cloud Native Buildpacks**.

**Buildpacks is the zero-config default.** Base-image choice belongs to the platform, not to forty
application repositories, and `rebase` is what makes that ownership pay: a run-image CVE is patched
fleet-wide by rewriting OCI manifests, not by forty rebuilds. For a platform that will be asked "how do
I patch this across every service," no other option has an answer.

**Railpack is not adopted now, and is not rejected.** Its efficiency advantage is real, but governance
and stability matter more for the default path than image size, and its strengths do not include the
capability that decides this ADR. Revisit when it leaves beta or gains multi-vendor governance.

**Strategies are drivers behind one narrow interface:** *(source, environment) → image reference +
streamed logs*. Every strategy is a thin driver over BuildKit. No strategy's configuration surface
enters the kelson spec: the spec says `strategy: buildpacks`, never a builder DSL.

## Rationale

The three axes that decided it:

- **Governance.** CNCF Graduated and multi-vendor versus single-vendor and beta. The default build path
  is not where a platform should carry avoidable dependency risk — the Kubero/Bitnami episode in
  [ADR-0005](0005-delegate-to-operators.md) is the same failure mode.
- **Fleet patching.** Rebase is a genuine platform capability with no equivalent in the alternatives.
- **Consistency with ADR-0005.** kelson delegates to well-governed upstreams rather than reimplementing
  them. Buildpacks is that principle applied to builds.

Dockerfile takes precedence over the default because a Dockerfile is an explicit statement of intent.
Silently ignoring one in favour of detection would be the kind of magic this project exists to avoid.

## Consequences

**Negative, stated plainly:**

- **The default path is the slowest and produces the largest images of the three.** Users who care most
  about image size get a worse result by default than Railpack would give them. That is a real cost paid
  for governance and rebase, and it will draw comparisons.
- **Buildpacks is the most machinery.** Builders, lifecycle and run images are concepts a user may meet
  when a build fails, and the failure modes are less legible than a Dockerfile's.
- **Rebase is not a complete CVE story.** It replaces compatible run-image layers only; application
  dependency vulnerabilities still need a rebuild. Describing rebase as fleet-wide patching without that
  qualifier would be dishonest, and the UI and docs must carry the qualifier.
- **Deferring Railpack means users asking for it will be told "not yet."** Mitigated, not removed, by
  the strategy interface.

**Positive:**

- Zero-config users get reproducible builds and a platform-owned base image.
- Users with a Dockerfile get exactly what they wrote, with no detection to misfire.
- Adding Railpack later is a backward-compatible enum extension plus one driver, because
  `model.BuildStrategy` is already `auto | dockerfile | buildpacks | none` and strategies are drivers.

**Not decided here:** build caching ([#52](https://github.com/dafrie/kelson/issues/52)), registry
integration ([#51](https://github.com/dafrie/kelson/issues/51)), and the detection implementation
([#50](https://github.com/dafrie/kelson/issues/50)). Dockerfile builds
([#48](https://github.com/dafrie/kelson/issues/48)) proceed in parallel regardless of this ADR, as #47
anticipated.
