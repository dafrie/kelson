# Roadmap

Four phases, seventeen milestones plus the v0.2 data split (M9b). Each milestone has a tracking epic
issue; the [issue tracker](https://github.com/dafrie/kelson/issues) is the live view.

This is a long project. The renderer and delivery adapters came first because everything else depends
on their properties. The 2026-08-12 architecture review added a hard rule to that: **the spine before
the leaves.** M4's exit criterion — a spec deployed end to end, proven on kind (#86) — gates
everything visible, because the review found finished, well-tested subsystems (~10k LOC) that nothing
had ever called.

---

## The v0.1 cut

Issues tagged [`v0.1`](https://github.com/dafrie/kelson/issues?q=is%3Aissue+label%3Av0.1).

Someone with a bare VPS or an existing cluster gets a Git repo to a URL over TLS, through a web UI,
with a production-grade database behind it — and the manifests kelson wrote are ones they would
approve in review.

It spans M0–M8, the v0.1 half of M9, and the minimal bootstrap. Deliberately trimmed:

| Deferred from v0.1 | Why |
|---|---|
| Database branching, Valkey (M9b) | Branching is the flagship — which is exactly why it does not ship on top of an unproven deploy path. Basic Postgres (shared/small presets, backups, verified restore) stays. |
| Argo CD adapter | Removed entirely, not deferred ([ADR-0012](adr/0012-flux-only-gitops.md)): Flux is the only GitOps mode; the adapter seam stays pluggable for a possible return. |
| Buildpacks (#49) | Dockerfile covers most repos. [ADR-0010](adr/0010-build-strategy.md) makes Buildpacks the eventual *default*, so this defers the default, not the decision. |
| Release history UI (#67) | The API has it; the UI can wait. |
| ~~external-secrets (#80)~~ | *Reversed by shipping early:* it turned out to need no per-backend code at all — kelson renders an `ExternalSecret` and delegates every provider to the operator's own SecretStore ([ADR-0020](adr/0020-external-secrets.md)). |
| ~~SOPS + age (#81)~~ | *Reversed by shipping early:* it closes the one gap ADR-0009 documented against the `cluster` backend — a cluster rebuilt from Git alone now comes back with its secrets. kelson encrypts in memory and holds no private key ([ADR-0022](adr/0022-sops-age.md)). |
| MySQL | The operator landscape is materially weaker than CNPG. Two engines done properly beats three half-supported. |
| M10–M16 | See the phase tables below. |

**Two deferrals were reversed by shipping early** — `kelson eject` (#41) fell out of the
rendered-history store and landed with M2, and the Score question (#31) was answered rather than
postponed: [an importer, not an input format](research/score-as-input-format.md), scheduled as #131 in
M15. (The Argo adapter also shipped early with M2, and was then removed by ADR-0012 — shipping early
is not the same as being right.)

**M9 moved into v0.1 and then split.** Managed Postgres is the feature people ask about first, and
shipping without it would make v0.1 hard to use for a real application. Branching grew on top of it
and was split back out to M9b (v0.2) by the review: highest-dependency feature, last to build.

The known weak spot: `kelson up` defaults to k3s with local-path, which has **no snapshot driver**,
and a bare VPS has no object store either — so that path gets logical dump/restore or
empty-plus-migrations branching (when M9b lands) until the user configures better storage. The
mitigation is detection plus an explicit nudge at the point of use rather than a silent degradation
(#108).

---

## Phase 1 — Core

*The renderer and its properties, then the spine. Everything downstream inherits both.*

| Milestone | Scope |
|---|---|
| **M0 · Foundations** ✓ | Repo scaffolding, CI, release tooling, docs site, governance. Includes the CI rule enforcing renderer purity. |
| **M1 · Model & renderer** ✓ | Project/Application/Environment schemas, pure renderer, golden-file harness, `kelson render` |
| **M2 · Delivery adapters** ✓ | `direct` / `flux`, provenance, status correlation, eject-to-git (the `argocd` adapter it also delivered is removed per ADR-0012) |
| **M3 · Preview, diff & dry-run** | Rendered diff, server-side dry-run diff, structured diff output |
| **M4 · Build & deploy — the spine** | Source to image, registry, **wiring delivery/observation into `kelson deploy`/`status`/`rollback` (#135)**, logs, rollback, Gateway-API-only cleanup (#140), honest-spec gating (#141) |

Exit: **a spec deployed end to end on a kind cluster in CI (#86)** — render → deliver → observe →
Healthy, through both adapters, with byte-identical rendered output proven by golden tests.

## Phase 2 — Product

*Drivable by agents, then usable by people — the API precedes the UI, because the UI, CLI and MCP
server are peers over one schema and an API designed around the UI first ends up UI-shaped.*

| Milestone | Scope |
|---|---|
| **M5 · ClusterProfile & install** | Detection and adoption, Helm chart, non-destructive uninstall, minimal k3s bootstrap, storage capability. Installing Flux means flux-operator + `FluxInstance` (#60). |
| **M7 · Agent surface & MCP** | ConnectRPC schema (#69), `kelson-server` v0 (#139), dry-run everywhere, idempotency, structured errors, MCP server, agent identities, policy. **Runs before M6.** |
| **M6 · Web UI** | App list and detail, deploy flow with preview, live logs, diff view, rollback — built against the M7 schema |
| **M8 · Secrets** | ~~SOPS/age (#81)~~ *(landed early, [ADR-0022](adr/0022-sops-age.md))*, structural no-plaintext guarantee (#82), binding injection, cluster backend |
| **M9 · Data services (v0.1 half)** | CNPG presets (shared/small/ha), backups once per environment, PITR-capable archiving, verified restore, `kelson db`, preset rename (#146) |

## Phase 3 — Platform

*A team of ten runs production on it.*

| Milestone | Scope |
|---|---|
| **M9b · Branching, Valkey & advanced data (v0.2)** | Snapshot/fallback branching, branch policy and lifecycle, preview databases, migration testing against a production branch, Valkey |
| **M10 · Environments & promotion** | Environment model, PR previews via flux-operator `ResourceSet`/`ResourceSetInputProvider`, vcluster ephemeral preview, promotion, Kargo interop |
| **M11 · Teams, RBAC & tenancy** | OIDC SSO, teams, Kubernetes RBAC mapping, quotas, audit log |
| **M12 · Networking & TLS** | Gateway API (only — no Ingress, [#140](https://github.com/dafrie/kelson/issues/140)), custom domains, cert-manager, DNS |
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
- **Testing** — golden files for the renderer, envtest for controllers, [e2e on kind](https://github.com/dafrie/kelson/issues/86) as the M4 exit gate
- **Docs** — every feature ships with docs; an ADR for every load-bearing decision

## Sequencing notes

**The renderer came first** because deletability, trustworthy preview, cheap testing and hybrid
delivery all derive from its purity ([ADR-0001](adr/0001-hybrid-state-model.md)).

**The spine gates the visible parts.** The review found the delivery, build and observation planes
finished but never assembled — high-quality parts with zero callers. M4 now owns the assembly, and
its kind E2E test is the exit gate no later milestone starts without.

**The API precedes the UI** ([M7](https://github.com/dafrie/kelson/issues/8) before
[M6](https://github.com/dafrie/kelson/issues/7)). The UI, CLI and MCP server are peers over one
schema ([ADR-0002](adr/0002-tech-stack.md)); kelson's identity is agent-native, so the schema is the
product surface and the UI is its first big client.

**Delegate to flux-operator.** Flux install/upgrade (`FluxInstance`), PR-preview lifecycle
(`ResourceSet` + `ResourceSetInputProvider`) and Flux health (`FluxReport`) are flux-operator's job;
kelson authors templates and reads status. CR-only integration — flux-operator is AGPL-3.0, kelson is
MIT.

**Minimal bootstrap moved from Phase 4 to M5.** [ADR-0003](adr/0003-install-model.md) was revised:
starting on Kubernetes at day zero is the premise, not a concession, so "I have nothing" needs an
answer in v0.1. An empty cluster is the degenerate ClusterProfile, so bootstrap is mostly filling in
the gaps detection already reports. The expensive parts — Talos, multi-node, upgrades, DR — stay in
M16.

**The catalog stays late.** Coolify's ~300 templates cannot be matched quickly and racing them is a
losing game. The plugin API matters more than the initial catalog size; the goal is for the community
to add templates.

**M9 sits in Phase 2 despite its number, and M9b in Phase 3.** Milestone numbers are identifiers, not
ordering. Basic data services moved forward because v0.1 is unusable for a real application without a
database; branching moved back because it is the highest-dependency feature in the plan and deserves
a proven substrate.

**Branching still pulls the release-command hook forward.** Migrations matter for any app with a
database, so #104 stays in M9 (v0.1) rather than waiting for M9b or day-2 operations in M14.
