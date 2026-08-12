# ADR-0012: Flux is the only GitOps delivery mode; Argo CD deferred

- **Status:** Accepted
- **Date:** 2026-08-12

## Context

M2 delivered three adapters over the pluggable delivery seam of
[ADR-0001](0001-hybrid-state-model.md): `direct`, `flux` and `argocd`. The Argo adapter was a
reasonable adopt-only design — it talks Argo's REST API, refuses to create or own `Application`
resources, verifies that an existing Application's source actually covers the repo/path/branch kelson
writes to, and reads Argo's own sync and health verdicts rather than recomputing them.

The 2026-08-12 architecture review surfaced what parity actually costs. The rendered-output claim
("all adapters consume identical manifests") is cheap and true; the last mile is not. Flux and Argo
have different health models feeding one kelson state machine, different secrets stories (SOPS
integration differs materially), and different preview/promotion ecosystems. Every future feature —
secrets (M8), preview environments (M10), promotion — would need to be designed, implemented and
tested three ways, forever, pre-release, by a project that does not yet have a working deploy path.
The M2 exit criterion only ever proved byte-identical rendered output, which tests none of this.

The project also intends to lean on [flux-operator](https://fluxoperator.dev/) for Flux lifecycle,
PR-preview environments and Flux health readback, which deepens the Flux path and widens the parity
gap further.

## Decision

**Flux is the only supported GitOps delivery mode.** The `internal/delivery/argocd` adapter is removed
([#138](https://github.com/dafrie/kelson/issues/138)).

**The delivery seam stays pluggable.** `Adapter`, `Registry` and `Capabilities` are unchanged; the
feature × mode matrix is `direct` and `flux`, both fully supported and both in every feature's test
matrix.

## Rationale

- The cost of GitOps-engine parity is permanent and multiplicative; the benefit before a first release
  is zero users served.
- Two external health vocabularies mapped onto one state machine is a correctness liability that would
  need continuous reconciliation work.
- Flux composes with flux-operator, which kelson adopts for Flux install ([#60](https://github.com/dafrie/kelson/issues/60)),
  PR previews (M10) and health readback ([#137](https://github.com/dafrie/kelson/issues/137)). There is
  no equivalent consolidation available on the Argo side at the layer kelson needs.

## Consequences

**Positive.** Every delivery feature is designed once against one GitOps engine; the state machine has
one external health model to map; M8/M10 shrink materially.

**Negative.**
- Teams standardized on Argo CD cannot adopt kelson's GitOps mode. They can still use direct mode, and
  `kelson eject` still emits a plain Kustomize repo an Argo `Application` can point at — but kelson will
  not read Argo's status back.
- ~1,900 LOC of working, tested adapter code is deleted. It remains in git history
  (`git log -- internal/delivery/argocd`) as the starting point for any return.

## Return condition

An Argo adapter returns, if demand proves out, at **"compose with existing Argo" scope** — adopt,
verify coverage, sync, read health — explicitly outside the every-feature test matrix, and as a
follow-up after v0.1. Anything more than that scope requires superseding this ADR.

## Supersedes

The Argo-parity aspects of [ADR-0001](0001-hybrid-state-model.md) (delivery mode list) and
[ADR-0002](0002-tech-stack.md) (Argo CD client libraries as a Go-choice rationale). The hybrid state
model itself — one pure renderer, pluggable delivery, direct mode as Git mode with an implicit
repository — is unchanged.
