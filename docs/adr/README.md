# Architecture Decision Records

Load-bearing decisions, with the reasoning and the trade-offs accepted. If you think one of these is
wrong, open an issue — it is far cheaper to reverse a decision now than after it has been built on.

| ADR | Decision | Status |
|---|---|---|
| [0001](0001-hybrid-state-model.md) | Hybrid state model — one pure renderer, pluggable delivery | Accepted |
| [0002](0002-tech-stack.md) | Go control plane, TypeScript/React UI | Accepted |
| [0002](0002-tech-stack.md) | ConnectRPC as the API transport | **Proposed** |
| [0003](0003-install-model.md) | Adopt existing clusters, bootstrap empty ones | Accepted (revised) |
| [0004](0004-licensing.md) | MIT, fully open, no feature gating | Accepted |
| [0005](0005-delegate-to-operators.md) | Delegate stateful workloads to upstream operators | **Proposed** |
| [0006](0006-project-application-environment.md) | Project, Application, Environment | Accepted |
| [0007](0007-data-services.md) | Data services — plans, delegation and branching | Accepted |

## Not yet written up

Two decisions are being built on but have no ADR. Both were drafted without sign-off and need one before
they harden:

- **Secrets are references only, enforced by a hard render failure.** A strong constraint that users will
  feel. Currently described in [architecture.md](../architecture.md) and issues #79 and #82.
- **The MCP server is a thin adapter with no logic of its own.** Currently in
  [architecture.md](../architecture.md) and epic #8.

## Format

Short. Context, Decision, Rationale, Consequences — with the negative consequences stated as plainly as
the positive ones. An ADR that lists only benefits is marketing, not a record.

Statuses: `Proposed` · `Accepted` · `Superseded by ADR-XXXX` · `Deprecated`. ADRs are immutable once
accepted; to change a decision, write a new ADR that supersedes it.
