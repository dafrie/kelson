# Roadmap

Four phases, seventeen milestones. Each milestone has a tracking epic issue; the
[issue tracker](https://github.com/dafrie/kelson/issues) is the live view.

This is a long project. The renderer and delivery adapters come first because everything else depends on
their properties. The visible parts come after the foundation can support them.

---

## The v0.1 cut

**50 issues**, tagged [`v0.1`](https://github.com/dafrie/kelson/issues?q=is%3Aissue+label%3Av0.1).

Someone with a bare VPS or an existing cluster gets a Git repo to a URL over TLS, through a web UI, and the
manifests kelson wrote are ones they would approve in review.

It spans M0–M6 plus the minimal bootstrap and the parts of M8 that Git mode makes mandatory. Deliberately
trimmed:

| Deferred from v0.1 | Why |
|---|---|
| Argo CD adapter (#35) | Flux proves the Git path. Argo is the same shape. |
| Buildpacks (#49) | Dockerfile covers most repos. Zero-config builds are the least differentiating work on the critical path. |
| `kelson eject` (#41) | The non-destructive uninstall test (#59) is the stronger deletability proof and it ships. |
| Release history UI (#67) | The API has it; the UI can wait. |
| external-secrets (#80) | SOPS + age (#81) is more self-contained for a bootstrapped cluster. ESO follows. |
| Score research (#31) | Interop, not core. |
| All of M9–M16 | See below. |

**The known gap: v0.1 ships without managed data services.** M9 is Phase 3, so v0.1 users bring their own
database and reference it through the secret model. That is defensible for an early release and it is worth
being honest about, because "managed Postgres" is the feature people ask about first. If early feedback
says it blocks adoption, M9 moves ahead of M6.

---

## Phase 1 — Core

*The renderer and its properties. Everything downstream inherits them.*

| Milestone | Scope |
|---|---|
| **M0 · Foundations** | Repo scaffolding, CI, release tooling, docs site, governance. Includes the CI rule enforcing renderer purity. |
| **M1 · Model & renderer** | Project/Application/Environment schemas, pure renderer, golden-file harness, `kelson render` |
| **M2 · Delivery adapters** | `direct` / `flux` / `argocd`, provenance, status correlation, eject-to-git |
| **M3 · Preview, diff & dry-run** | Rendered diff, server-side dry-run diff, structured diff output |
| **M4 · Build & deploy** | Source to image, registry, rollout, logs, rollback |

Exit: one application deployed through every delivery adapter with byte-identical rendered output, proven
by golden tests.

## Phase 2 — Product

*Usable by people, drivable by agents.*

| Milestone | Scope |
|---|---|
| **M5 · ClusterProfile & install** | Detection and adoption, Helm chart, non-destructive uninstall, minimal k3s bootstrap |
| **M6 · Web UI** | App list and detail, deploy flow with preview, live logs, diff view, rollback |
| **M7 · Agent surface & MCP** | ConnectRPC schema, dry-run everywhere, idempotency, structured errors, MCP server, agent identities, policy |
| **M8 · Secrets** | external-secrets, SOPS/age, renderer-enforced no-plaintext, binding injection |

## Phase 3 — Platform

*A team of ten runs production on it.*

| Milestone | Scope |
|---|---|
| **M9 · Data services** | CloudNativePG, Valkey, object storage, backup and PITR, connection binding |
| **M10 · Environments & promotion** | Environment model, PR previews, vcluster ephemeral preview, promotion, Kargo interop |
| **M11 · Teams, RBAC & tenancy** | OIDC SSO, teams, Kubernetes RBAC mapping, quotas, audit log |
| **M12 · Networking & TLS** | Gateway API, Ingress, custom domains, cert-manager, DNS |
| **M13 · Observability** | Prometheus/Loki/Tempo adoption, per-app dashboards, alerts, cost |

## Phase 4 — Scale

| Milestone | Scope |
|---|---|
| **M14 · Day-2 & scaling** | HPA/KEDA, cron, workers, progressive delivery, resource recommendations |
| **M15 · Catalog & ecosystem** | Service catalog, plugin API, Score importer, `kelson eject` |
| **M16 · Full bootstrap** | Talos, multi-node, node addition, upgrades, control-plane DR |

---

## Cross-cutting

Ongoing, tracked by label rather than milestone.

- **Security** — [threat model](https://github.com/dafrie/kelson/issues/84), [supply chain](https://github.com/dafrie/kelson/issues/85), least-privilege RBAC
- **Testing** — golden files for the renderer, envtest for controllers, [e2e on kind](https://github.com/dafrie/kelson/issues/86)
- **Docs** — every feature ships with docs; an ADR for every load-bearing decision

## Sequencing notes

**The renderer comes first** because deletability, trustworthy preview, cheap testing and hybrid delivery
all derive from its purity ([ADR-0001](adr/0001-hybrid-state-model.md)). Building on top before that is
locked in and CI-enforced means rebuilding later.

**The API is designed alongside the UI, not after it.** The UI, CLI and MCP server are peers over one
schema ([ADR-0002](adr/0002-tech-stack.md)). Designing around the UI's needs first is how an API ends up
UI-shaped and unusable by agents, which is the state of the rest of the category.

**Minimal bootstrap moved from Phase 4 to M5.** [ADR-0003](adr/0003-install-model.md) was revised: starting
on Kubernetes at day zero is the premise, not a concession, so "I have nothing" needs an answer in v0.1. An
empty cluster is the degenerate ClusterProfile, so bootstrap is mostly filling in the gaps detection
already reports. The expensive parts — Talos, multi-node, upgrades, DR — stay in M16.

**The catalog stays late.** Coolify's ~300 templates cannot be matched quickly and racing them is a losing
game. The plugin API matters more than the initial catalog size; the goal is for the community to add
templates.
