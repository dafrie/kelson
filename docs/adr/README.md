# Architecture Decision Records

Load-bearing decisions, with the reasoning and the trade-offs accepted. If you think one of these is
wrong, open an issue — it is far cheaper to reverse a decision now than after it has been built on.

| ADR | Decision | Status |
|---|---|---|
| [0001](0001-hybrid-state-model.md) | Hybrid state model — one pure renderer, pluggable delivery | Accepted |
| [0002](0002-tech-stack.md) | Go control plane, TypeScript/React UI, ConnectRPC | Accepted |
| [0003](0003-install-model.md) | Adopt existing clusters, bootstrap empty ones | Accepted (revised) |
| [0004](0004-licensing.md) | MIT, fully open, no feature gating | Accepted |
| [0005](0005-delegate-to-operators.md) | Managed service types delegate to operators | Accepted (revised) |
| [0006](0006-project-application-environment.md) | Project, Application, Environment | Accepted (leaf amended by 0014) |
| [0007](0007-data-services.md) | Data services — presets, delegation and branching | Accepted |
| [0008](0008-mcp-surface.md) | MCP tools are task-shaped, not endpoint-shaped | Accepted |
| [0009](0009-secrets.md) | Secrets are references; values never enter Git | Accepted |
| [0010](0010-build-strategy.md) | Dockerfile when present, Cloud Native Buildpacks otherwise | Accepted |
| [0011](0011-build-cache.md) | Build cache is a registry cache, scoped per application | Deferred |
| [0012](0012-flux-only-gitops.md) | Flux is the only GitOps delivery mode; Argo CD deferred | Accepted |
| [0013](0013-server-state-and-api-v0.md) | Server state lives in the cluster; API v0 shape | Proposed |
| [0014](0014-components.md) | One `components` list, per-component identity (amends 0006) | Proposed |

*Process note (2026-08-12):* ADR-0003 and ADR-0005 were revised in place before the immutability rule
below hardened; their in-place revisions stand as recorded. From ADR-0012 on, changes supersede.

## Format

Short. Context, Decision, Rationale, Consequences — with the negative consequences stated as plainly as
the positive ones. An ADR that lists only benefits is marketing, not a record.

Statuses: `Proposed` · `Accepted` · `Superseded by ADR-XXXX` · `Deprecated`. ADRs are immutable once
accepted; to change a decision, write a new ADR that supersedes it.
