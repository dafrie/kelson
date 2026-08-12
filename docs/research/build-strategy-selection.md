# Build strategy selection: Dockerfile, Buildpacks, Railpack

Evidence for [#47](https://github.com/dafrie/kelson/issues/47). The decision it supports is
[ADR-0010](../adr/0010-build-strategy.md). Researched 2026-08-12.

## The landscape moved twice, not once

The issue framed the trap correctly: Railway put **Nixpacks into maintenance** and shipped **Railpack**
as its replacement, while Coolify and Dokploy still default to Nixpacks — a deprecated dependency
inherited by inertia.

Since that framing, a second and larger move happened. **Cloud Native Buildpacks graduated from the
CNCF on 17 July 2026**, the foundation's highest maturity level. The issue listed "maintenance risk and
governance" as an open question; for CNB that question is now answered about as definitively as this
industry answers it.

## The four options

### Dockerfile via BuildKit

Unambiguous. The user has already said exactly what they want, and there is no detection to get wrong.
It covers most real repositories, has no governance risk worth discussing, and is the only option that
imposes nothing.

Its weakness is the reason the others exist: every application owns its own base image, so patching a
base-image CVE across a fleet means editing and rebuilding every repository.

### Cloud Native Buildpacks

- **Governance:** CNCF **Graduated** (17 July 2026). Multi-vendor. The lowest-risk option on this axis by
  a wide margin.
- **Reproducibility:** the strongest story of the four.
- **Rebase:** the differentiator. Base-image choice moves out of per-service Dockerfiles into a single
  builder owned by the platform. `rebase` detects a newer run image and rewrites the OCI manifest and
  config **without rebuilding application layers**, so a fleet is patched in seconds rather than in
  forty rebuilds.
- **Honest limit:** rebase only replaces compatible run-image layers. Vulnerabilities in *application*
  dependencies still require a real rebuild. Rebase is not a universal CVE answer and should never be
  described as one.
- **Cost:** builds are slower and images larger than Railpack's, and the builder/lifecycle model is more
  machinery than a Dockerfile.

### Railpack

- Go + BuildKit, MIT, the successor to Nixpacks, written by Railway from several years of running
  Nixpacks in production.
- **Image size:** 38% smaller for Node, up to 77% for Python versus Nixpacks. Real and substantial.
- **Build times:** materially better than Nixpacks, especially cold.
- **Standalone use is supported:** a BuildKit frontend ships as an image per release and is the
  recommended production path, with a documented guide for platforms embedding it.
- **Risks:** single-vendor (Railway is the steward), and it still carries a beta designation. Its
  roadmap is driven by one company's platform needs.

### Nixpacks

In maintenance. Not a candidate. Its only relevance is as the mistake to avoid repeating.

## Answering the issue's questions

**Language coverage.** Dockerfile is universal by construction. CNB and Railpack both cover the
mainstream set (Node, Python, Go, Ruby, PHP, Java). Neither is a coverage bottleneck for a first
release; the Dockerfile escape hatch covers whatever they miss.

**Image size and build time.** Railpack wins, clearly. CNB is the slowest and largest of the three.

**Reproducibility and rebase.** CNB wins, and rebase has no equivalent in the others.

**Maintenance risk and governance.** CNB (graduated, multi-vendor) ≫ Railpack (single-vendor, beta) >
Nixpacks (maintenance). This axis moved decisively since the issue was written.

**Can we support more than one without doubling maintenance?** Yes, on one condition: the build strategy
must be an interface with a deliberately narrow contract — *(source, environment) → image reference +
streamed logs* — and every strategy must be a thin driver over BuildKit, which all three already are.
The maintenance cost is proportional to how much of each tool's configuration surface leaks into
kelson's spec. It must not leak: the spec says `strategy: buildpacks`, not a builder DSL.

**The right default for a user with no build configuration.** Buildpacks — see the ADR.

## What this means for the spec

`model.BuildStrategy` already has the enum `auto | dockerfile | buildpacks | none`. This research
confirms that enum was right and fixes what `auto` resolves to. Railpack is deliberately absent; adding
it later is a backward-compatible enum extension, which is exactly the property that makes deferring it
cheap.
