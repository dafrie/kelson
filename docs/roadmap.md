# Roadmap

Four phases, seventeen milestones. Each milestone has a tracking epic issue; the
[issue tracker](https://github.com/dafrie/kelson/issues) is the live view.

This is a long project. The renderer and delivery adapters come first because everything else depends on
their properties. The visible parts come after the foundation can support them.

---

## The v0.1 cut

**70 issues**, tagged [`v0.1`](https://github.com/dafrie/kelson/issues?q=is%3Aissue+label%3Av0.1).

Someone with a bare VPS or an existing cluster gets a Git repo to a URL over TLS, through a web UI, with a
production-grade database behind it — and the manifests kelson wrote are ones they would approve in review.

It spans M0–M6, all of M9, the minimal bootstrap, and the parts of M8 that Git mode makes mandatory.
Deliberately trimmed:

| Deferred from v0.1 | Why |
|---|---|
| Buildpacks (#49) | Dockerfile covers most repos. Zero-config builds are the least differentiating work on the critical path. Note [ADR-0010](adr/0010-build-strategy.md) makes Buildpacks the eventual *default*, so this defers the default, not the decision. |
| Release history UI (#67) | The API has it; the UI can wait. |
| external-secrets (#80) | SOPS + age (#81) is more self-contained for a bootstrapped cluster. ESO follows. |
| MySQL | The operator landscape is materially weaker than CNPG. Two engines done properly beats three half-supported. |
| M10–M16 | See the phase tables below. |

**Three deferrals were reversed by shipping early.** The Argo CD adapter (#35) and `kelson eject` (#41)
both landed with M2, because the adapter interface made the third adapter cheap and eject fell out of the
rendered-history store. The Score question (#31) was answered rather than postponed:
[an importer, not an input format](research/score-as-input-format.md), scheduled as #131 in M15.

**M9 moved into v0.1 and grew.** Managed Postgres is the feature people ask about first, and shipping
without it would have made v0.1 hard to use for a real application. It now also carries database branching,
which is the most differentiating capability in the plan — see [ADR-0007](adr/0007-data-services.md).

The known weak spot: `kelson up` defaults to k3s with local-path, which has **no snapshot driver at all**,
so the bootstrap path gets restore-based branching until a user opts into snapshot-capable storage. The
mitigation is detection plus an explicit nudge at the point of use rather than a silent degradation
(#108).

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
| **M5 · ClusterProfile & install** | Detection and adoption, Helm chart, non-destructive uninstall, minimal k3s bootstrap, storage capability |
| **M6 · Web UI** | App list and detail, deploy flow with preview, live logs, diff view, rollback |
| **M7 · Agent surface & MCP** | ConnectRPC schema, dry-run everywhere, idempotency, structured errors, MCP server, agent identities, policy |
| **M8 · Secrets** | external-secrets, SOPS/age, renderer-enforced no-plaintext, binding injection |
| **M9 · Data services & branching** | CNPG plans, HA, backup and PITR, Valkey, branching, preview databases, migration testing |

## Phase 3 — Platform

*A team of ten runs production on it.*

| Milestone | Scope |
|---|---|
| **M10 · Environments & promotion** | Environment model, PR previews, vcluster ephemeral preview, promotion, Kargo interop |
| **M11 · Teams, RBAC & tenancy** | OIDC SSO, teams, Kubernetes RBAC mapping, quotas, audit log |
| **M12 · Networking & TLS** | Gateway API, Ingress, custom domains, cert-manager, DNS |
| **M13 · Observability** | Prometheus/Loki/Tempo adoption, per-app dashboards, alerts, cost |

## Phase 4 — Scale

| Milestone | Scope |
|---|---|
| **M14 · Day-2 & scaling** | HPA/KEDA, cron, workers, progressive delivery, resource recommendations |
| **M15 · Catalog & ecosystem** | Service catalog, plugin API, Score importer (#131) |
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

**M9 sits in Phase 2 despite its number.** Milestone numbers are identifiers, not ordering. Data services
moved forward because v0.1 is unusable for a real application without a database, and because branching
turned out to be the most differentiating capability in the plan rather than a nice-to-have.

**Branching pulls the release-command hook forward.** Empty preview databases are useless without
migrations, so #104 lands in M9 rather than with the rest of day-2 operations in M14.
