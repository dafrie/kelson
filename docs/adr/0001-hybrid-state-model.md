# ADR-0001: Hybrid state model — one renderer, pluggable delivery

- **Status:** Accepted (Argo CD adapter scope superseded by [ADR-0012](0012-flux-only-gitops.md), 2026-08-12; hybrid delivery — the mode list, per-environment mode selection and `direct` — superseded by [ADR-0028](0028-delivery-spine.md), 2026-08-14. The pure renderer and the pluggable-seam insight survive, and the seam's last implementation is the Flux spine.)
- **Date:** 2026-08-11

## Context

Every self-hosted PaaS in this category keeps platform state in its own database. The consequences are
consistent and severe: no reviewable change history, no reproducibility, disaster recovery that means
restoring a database, and an uninstall path that is destructive to running workloads. Kubero improved on
this by storing state in CRDs, but cluster state still is not a reviewable artifact.

Meanwhile, the users we most want — teams already running Flux or Argo CD — cannot adopt a platform that
insists on being the thing that applies manifests.

Three options were considered:

1. **Git as the sole source of truth.** kelson renders and commits; Flux or Argo reconciles.
2. **CRDs plus a controller that applies directly.** Kubero's model. Git export secondary.
3. **Hybrid** — both modes first-class.

The concern with (3) is the obvious one: doubled surface area, and two code paths that drift apart.

## Decision

**Hybrid, implemented as a single path.**

The renderer is a pure function `(spec, ClusterProfile) → manifests`, with no cluster access, no network,
no clock and no database. Delivery is a pluggable adapter consuming identical rendered output:

- `direct` — kelson server-side applies
- `flux` — commit, then trigger reconciliation
- `argocd` — commit, then trigger sync

Delivery mode is configured **per environment**, not per install.

Crucially, **direct mode is Git mode with an implicit repository.** Direct mode still versions its
rendered output, so those users retain diffs, history and rollback — and `kelson eject --to-git` replays
that history into a real repository, switching adapters without re-modelling anything.

## Consequences

**Positive.**
- The doubled-surface concern largely dissolves: modes differ only in who calls `apply`, which is an
  adapter, not a second codebase.
- kelson becomes deletable. Uninstalling leaves running apps and a plain Kustomize repo. This is the
  strongest possible anti-lock-in guarantee, and it is provable rather than promised.
- Preview becomes trustworthy, because rendering the same spec twice produces the same bytes.
- Most correctness lives in golden-file tests instead of an integration suite needing a live cluster.
- Both target audiences are served without forking the product.

**Negative.**
- Purity is a discipline that must be defended. Any convenience cluster lookup inside the renderer breaks
  golden tests, previews and hybrid delivery simultaneously. This needs enforcement in CI, not just review.
- Git mode has an inherent feedback-latency risk. Mitigated by triggering reconciliation on commit rather
  than waiting for the poll interval, and by provenance-based status correlation.
- Git mode forbids plaintext secrets, forcing external-secrets or SOPS/age from the start. This is more
  up-front work than writing `Secret` resources, and is genuinely the right constraint.
- Concurrent edits need optimistic concurrency on spec version. Read-modify-write against latest HEAD;
  conflicts must surface loudly rather than last-write-wins.

## Enforcement

The renderer package must not import any Kubernetes client, HTTP client, or `time.Now`. This should be
checked by a CI lint rule, because the failure mode is silent and expensive.
