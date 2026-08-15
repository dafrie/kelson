# Architecture Decision Records

Load-bearing decisions, with the reasoning and the trade-offs accepted. If you think one of these is
wrong, open an issue — it is far cheaper to reverse a decision now than after it has been built on.

| ADR | Decision | Status |
|---|---|---|
| [0001](0001-hybrid-state-model.md) | Hybrid state model — one pure renderer, pluggable delivery | Accepted (hybrid delivery superseded by 0028) |
| [0002](0002-tech-stack.md) | Go control plane, TypeScript/React UI, ConnectRPC | Accepted |
| [0003](0003-install-model.md) | Adopt existing clusters, bootstrap empty ones | Accepted (revised) |
| [0004](0004-licensing.md) | MIT, fully open, no feature gating | Accepted |
| [0005](0005-delegate-to-operators.md) | Managed service types delegate to operators | Accepted (revised) |
| [0006](0006-project-application-environment.md) | Project, Application, Environment | Accepted (leaf amended by 0014) |
| [0007](0007-data-services.md) | Data services — presets, delegation and branching | Accepted |
| [0008](0008-mcp-surface.md) | MCP tools are task-shaped, not endpoint-shaped | Accepted (tool names amended by 0032) |
| [0009](0009-secrets.md) | Secrets are references; values never enter Git | Accepted |
| [0010](0010-build-strategy.md) | Dockerfile when present, Cloud Native Buildpacks otherwise | Accepted |
| [0011](0011-build-cache.md) | Build cache is a registry cache, scoped per application | Deferred |
| [0012](0012-flux-only-gitops.md) | Flux is the only GitOps delivery mode; Argo CD deferred | Accepted (conclusion carried into 0028) |
| [0013](0013-server-state-and-api-v0.md) | Server state lives in the cluster; API v0 shape | Superseded by 0027/0028 (§2–§4 live on) |
| [0014](0014-components.md) | One `components` list, per-component identity (amends 0006) | Accepted (carve-out reversed by 0032) |
| [0015](0015-valkey-operator.md) | `kind: valkey` delegates to valkey-io/valkey-operator | Accepted |
| [0016](0016-delivery-flows-v0.md) | The four M10 delivery flows, and what each one is in v0 | Accepted (mode gates superseded by 0028) |
| [0017](0017-pr-previews.md) | PR previews — `previews:`, the artifact tag, and what stage 1 does not do | Accepted (publisher adopted by 0028) |
| [0018](0018-secret-references.md) | The secret reference schema — `{secret, key}` in an env value (refines 0009) | Accepted |
| [0019](0019-release-command-hook.md) | The release command hook — a component field, direct mode only, no new phase | Accepted, implementation gated (0028) |
| [0020](0020-external-secrets.md) | The `externalSecrets` backend — an ExternalSecret per referenced Secret (refines 0018) | Accepted |
| [0021](0021-installing-missing-components.md) | Installing missing platform components — pinned upstream manifests, per-object provenance | Accepted (extended by 0030) |
| [0022](0022-sops-age.md) | The `sops` backend — encrypt on write, decrypt in-cluster, kelson holds no key (refines 0018) | Accepted (transport amended by 0028) |
| [0023](0023-explain-structured-causes.md) | `explain` — structured causes with confidence, evidence and revision provenance | Accepted |
| [0024](0024-agent-identities.md) | Agent identities — scoped, expiring credentials that are principals (refines 0013 §3) | Accepted |
| [0025](0025-agent-policy.md) | Per-environment agent policy — guardrails in the spec, enforced server-side (builds on 0024) | Accepted |
| [0026](0026-agent-audit-trail.md) | The audit trail — one principal-typed record per mutation, in a bounded cluster ring | Accepted |
| [0027](0027-crd-native-control-plane.md) | CRD-native control plane — `Project` and `Environment` are custom resources (supersedes 0013 §1) | Accepted |
| [0028](0028-delivery-spine.md) | The delivery spine — render, push an OCI artifact, let Flux reconcile | Accepted |
| [0029](0029-renderer-stays-go.md) | The renderer stays pure Go; CUE and timoni rejected as the rendering engine | Accepted |
| [0030](0030-flux-aio-install.md) | Install substrate — flux-aio, pre-rendered at release time and pinned | Accepted |
| [0031](0031-single-cluster-single-tenant.md) | One cluster, one tenant — for now, and recorded as such | Accepted |
| [0032](0032-finish-the-component-rename.md) | Finish the component rename — the label, the selector and the MCP tools (amends 0014, 0008) | Accepted |
| [0033](0033-git-connections.md) | Git connections — the forge credentials kelson holds, and how they are scoped (amends 0009) | Proposed |
| [0034](0034-forge-driven-delivery.md) | Forge-driven delivery — webhooks, the CI hand-off, server-side preview publishing (amends 0017) | Proposed |
| [0035](0035-sources.md) | Sources are declared, then bound — per-component sources and the global tier | Proposed |

*Rebuild note (2026-08-14):* ADRs 0027–0031 are the pre-release rebuild taken as one decision. Read in
order they are: where state lives, how it is delivered, what renders it, what runs it, and how far it
reaches. ADR-0032 is not part of it: it landed on main in parallel and finishes the ADR-0014 rename.

*Process note (2026-08-12):* ADR-0003 and ADR-0005 were revised in place before the immutability rule
below hardened; their in-place revisions stand as recorded. From ADR-0012 on, changes supersede.

## Format

Short. Context, Decision, Rationale, Consequences — with the negative consequences stated as plainly as
the positive ones. An ADR that lists only benefits is marketing, not a record.

Statuses: `Proposed` · `Accepted` · `Superseded by ADR-XXXX` · `Deprecated`. ADRs are immutable once
accepted; to change a decision, write a new ADR that supersedes it.
