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
| [0006](0006-project-application-environment.md) | Project, Application, Environment | Accepted |
| [0007](0007-data-services.md) | Data services — plans, delegation and branching | Accepted |
| [0008](0008-mcp-surface.md) | MCP tools are task-shaped, not endpoint-shaped | Accepted |
| [0009](0009-secrets.md) | Secrets are references; values never enter Git | Accepted |
| [0010](0010-build-strategy.md) | Dockerfile when present, Cloud Native Buildpacks otherwise | Proposed |
| [0011](0011-build-cache.md) | Build cache is a registry cache, scoped per application | Proposed |

Every load-bearing decision is now recorded and reviewed.

## Format

Short. Context, Decision, Rationale, Consequences — with the negative consequences stated as plainly as
the positive ones. An ADR that lists only benefits is marketing, not a record.

Statuses: `Proposed` · `Accepted` · `Superseded by ADR-XXXX` · `Deprecated`. ADRs are immutable once
accepted; to change a decision, write a new ADR that supersedes it.
