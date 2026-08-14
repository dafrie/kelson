# Roadmap

Four phases, seventeen milestones plus the v0.2 data split (M9b). Each milestone has a tracking epic
issue; the [issue tracker](https://github.com/dafrie/kelson/issues) is the live view.

This is a long project. The renderer came first because everything else depends on its properties. The
2026-08-12 architecture review added a hard rule to that: **the spine before the leaves.** A spec
deployed end to end, proven on kind (#86), gates everything visible, because the review found finished,
well-tested subsystems (~10k LOC) that nothing had ever called.

**The rebuild below is that rule applied to itself.** ADRs 0027–0031 replace the spine — not a leaf —
and the phases are ordered so that nothing visible is rebuilt before the thing underneath it works on a
kind cluster.

---

## Now: the CRD-native rebuild (epic [#223](https://github.com/dafrie/kelson/issues/223))

Five ADRs taken as one decision on 2026-08-14, pre-release and therefore cheap:
[ADR-0027](adr/0027-crd-native-control-plane.md) puts state in `Project` and `Environment` custom
resources reconciled by a controller; [ADR-0028](adr/0028-delivery-spine.md) replaces delivery modes
with one spine — render, push an immutable OCI artifact, let Flux reconcile;
[ADR-0029](adr/0029-renderer-stays-go.md) keeps the renderer pure Go after evaluating CUE and timoni;
[ADR-0030](adr/0030-flux-aio-install.md) makes Flux installable on a small cluster;
[ADR-0031](adr/0031-single-cluster-single-tenant.md) draws the scope line at one cluster, one tenant.

| Phase | Scope | Exit |
|---|---|---|
| **R1 · The spine** ([#224](https://github.com/dafrie/kelson/issues/224)) | `api/kelson/v1alpha1`, CRD generation out of `internal/schemagen`, `cmd/kelson-controller` on controller-runtime, the OCI publisher, the `OCIRepository` + `Kustomization` pair, status and history mirror. Deletes `internal/delivery/direct`, `git`, `rollback`, the adapter seam and the ConfigMap spec/history stores. **Status:** the six steps are built — publisher extracted to `internal/artifact` and shared with previews, spec hash and artifact ref, SSA'd Flux pair with `wait: true`, phase readback, rollback annotation, bounded history, the delivery error taxonomy and the `kelson.dev/environment` finalizer. The chart's controller RBAC has landed too: `environments/finalizers` on the cluster-scoped ClusterRole, a namespaced Role in the flux namespace for CRUD on `OCIRepository`/`Kustomization` (delete included, for the finalizer teardown), leader election on by default now that the controller writes rather than only reads (`deploy/chart/kelson`). Deliberately still no workload-reading RBAC — R1 delegates health to Flux's own `wait: true` and tracks the finer-grained readback separately ([#240](https://github.com/dafrie/kelson/issues/240)). The exit gate is now written as a test: `TestDeliverySpine` (`test/e2e/spine_test.go`, provisioned by `hack/e2e/spine.sh`, run by `.github/workflows/e2e.yml`) applies a Project and an Environment as custom resources and asserts the artifact, its provenance annotations, the `OCIRepository`/`Kustomization` pair, the live workload, a second revision, the rollback reverting the deployed bytes, and the resume — see [docs/e2e.md](e2e.md). **Outstanding: its first green CI run**, which needs a cluster that can pull images; one finding is already recorded there (an inert rollback re-arms, because `EnvironmentReconciler` discards the generation `rollbackFor` computes for it) | **a spec deployed end to end on a kind cluster** — apply a Project and an Environment, artifact published, Flux reconciles, `Environment.status` reaches Healthy |
| **R2 · Façade and verbs** ([#225](https://github.com/dafrie/kelson/issues/225)) | `kelson-server` over CRs with SSA and `resourceVersion` concurrency; the agent and audit stores to `internal/controlstore`; deploy, history, rollback (the annotation), promote (the CR patch) reshaped in the CLI, the UI and MCP | every verb works over the new spine with the wire surface unchanged |
| **R3 · Install path** ([#226](https://github.com/dafrie/kelson/issues/226)) | the flux-aio catalog row and its release-time render, kelson's own CRDs as a catalog entry, chart and RBAC for the controller, previews re-verified against the shared publisher | a cluster with nothing gets a working kelson in one install path |

Follow-ups, deliberately out of the three phases:
[#227](https://github.com/dafrie/kelson/issues/227) Flux-native release hooks ·
[#228](https://github.com/dafrie/kelson/issues/228) `ExternalArtifact` for registry-less clusters ·
[#229](https://github.com/dafrie/kelson/issues/229) admission webhook ·
[#230](https://github.com/dafrie/kelson/issues/230) `kind: timoni` ·
[#231](https://github.com/dafrie/kelson/issues/231) tenancy ·
[#232](https://github.com/dafrie/kelson/issues/232) multi-cluster ·
[#233](https://github.com/dafrie/kelson/issues/233) agent/audit stores as CRDs ·
[#234](https://github.com/dafrie/kelson/issues/234) the label rename and the `delivery:` block removal,
kept as one behaviour-change PR.

The milestone tables below are kept as written, with the rows the rebuild absorbs annotated rather than
deleted: what was built and why it was replaced is the part worth keeping.

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
| Argo CD adapter | Removed entirely, not deferred ([ADR-0012](adr/0012-flux-only-gitops.md)). [ADR-0028](adr/0028-delivery-spine.md) then removed the seam it would have returned through: one reconciler, no adapters. A second one would be a new ADR answering ADR-0012's parity question with evidence. |
| ~~Buildpacks (#49)~~ | *Reversed by shipping early:* the driver was written, tested and unused, so the deferral was of the wiring rather than of the work. Both strategies now build; what stays deferred is in-cluster detection (#50), which is why `auto` still needs a local checkout. |
| Release history UI (#67) | The API has it; the UI can wait. |
| ~~external-secrets (#80)~~ | *Reversed by shipping early:* it turned out to need no per-backend code at all — kelson renders an `ExternalSecret` and delegates every provider to the operator's own SecretStore ([ADR-0020](adr/0020-external-secrets.md)). |
| ~~SOPS + age (#81)~~ | *Reversed by shipping early:* it closes the one gap ADR-0009 documented against the `cluster` backend — a cluster rebuilt from the delivered artifact now comes back with its secrets. kelson encrypts in memory and holds no private key ([ADR-0022](adr/0022-sops-age.md)), and since [ADR-0028](adr/0028-delivery-spine.md) §7 it also writes the decryption block, closing that ADR's worst failure mode. |
| MySQL | The operator landscape is materially weaker than CNPG. Two engines done properly beats three half-supported. |
| M10–M16 | See the phase tables below. |

**Two deferrals were reversed by shipping early** — `kelson eject` (#41) fell out of the
rendered-history store and landed with M2, and the Score question (#31) was answered rather than
postponed: [an importer, not an input format](research/score-as-input-format.md), scheduled as #131 in
M15. (The Argo adapter also shipped early with M2, and was then removed by ADR-0012 — shipping early
is not the same as being right. `eject` is the second case: it shipped, and
[ADR-0028](adr/0028-delivery-spine.md) deleted it because every revision is now already an immutable
artifact of standard manifests, so the export had nothing left to export.)

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
| **M1 · Model & renderer** ✓ | Project/Component/Environment schemas (ADR-0006's leaf, as amended by [ADR-0014](adr/0014-components.md) and [ADR-0032](adr/0032-finish-the-component-rename.md)), pure renderer, golden-file harness, `kelson render` |
| **M2 · Delivery adapters** ✓ | `direct` / `flux`, provenance, status correlation, eject-to-git (the `argocd` adapter it also delivered is removed per ADR-0012). *Superseded by R1:* [ADR-0028](adr/0028-delivery-spine.md) collapses the adapter seam to one spine and deletes `direct`, the git writer, rollback-by-replay and eject. Provenance and the state machine survive unchanged |
| **M3 · Preview, diff & dry-run** | Rendered diff, server-side dry-run diff, structured diff output |
| **M4 · Build & deploy — the spine** | Source to image, registry, **wiring delivery/observation into `kelson deploy`/`status`/`rollback` (#135)**, logs, rollback, Gateway-API-only cleanup (#140), honest-spec gating (#141). *Delivery half absorbed by R1/R2:* the same verbs, over the artifact spine instead of over an adapter. Build, logs and the gating cleanups are unaffected |

Exit: **a spec deployed end to end on a kind cluster in CI (#86)** — render → deliver → observe →
Healthy, with byte-identical rendered output proven by golden tests. R1 inherits this gate verbatim,
with "through both adapters" struck: there is one path, and it is the one under test.

## Phase 2 — Product

*Drivable by agents, then usable by people — the API precedes the UI, because the UI, CLI and MCP
server are peers over one schema and an API designed around the UI first ends up UI-shaped.*

| Milestone | Scope |
|---|---|
| **M5 · ClusterProfile & install** | Detection and adoption, Helm chart, non-destructive uninstall, minimal k3s bootstrap, storage capability. *Install half superseded by R3:* [ADR-0030](adr/0030-flux-aio-install.md) makes flux-aio the default offer on a Flux-less cluster and flux-operator optional (previews only), and the chart gains kelson's CRDs and the controller. Detection, adoption and uninstall stand |
| **M7 · Agent surface & MCP** | ConnectRPC schema (#69), `kelson-server` v0 (#139), dry-run everywhere, idempotency, structured errors, MCP server, agent identities, policy. **Runs before M6.** |
| **M6 · Web UI** | App list and detail, deploy flow with preview, live logs, diff view, rollback — built against the M7 schema |
| **M8 · Secrets** | ~~SOPS/age (#81)~~ *(landed early, [ADR-0022](adr/0022-sops-age.md))*, structural no-plaintext guarantee (#82), binding injection, cluster backend |
| **M9 · Data services (v0.1 half)** | CNPG presets (shared/small/ha), backups once per environment, PITR-capable archiving, verified restore, `kelson db`, preset rename (#146) |

## Phase 3 — Platform

*A team of ten runs production on it.*

| Milestone | Scope |
|---|---|
| **M9b · Branching, Valkey & advanced data (v0.2)** | Snapshot/fallback branching, branch policy and lifecycle, preview databases, migration testing against a production branch, Valkey |
| **M10 · Environments & promotion** | Environment model, PR previews via flux-operator `ResourceSet`/`ResourceSetInputProvider`, vcluster ephemeral preview, promotion, Kargo interop. *Partly absorbed:* the Environment becomes a CRD in R1 and promotion becomes a CR patch in R2; multi-cluster targeting, once M10's, is now [#232](https://github.com/dafrie/kelson/issues/232) and `Environment.spec.cluster` is deleted ([ADR-0031](adr/0031-single-cluster-single-tenant.md)) |
| **M11 · Teams, RBAC & tenancy** | OIDC SSO, teams, Kubernetes RBAC mapping, quotas, audit log. Out of the rebuild's scope by [ADR-0031](adr/0031-single-cluster-single-tenant.md), which records the shape and the prior art without building any of it ([#231](https://github.com/dafrie/kelson/issues/231)); the order is forced — real human identity (#84), then RBAC, then tenancy |
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
- **Testing** — golden files for the renderer, envtest for the controller, [e2e on kind](https://github.com/dafrie/kelson/issues/86) as the exit gate for M4 and now for R1
- **Docs** — every feature ships with docs; an ADR for every load-bearing decision

## Sequencing notes

**The renderer came first** because deletability, trustworthy preview and cheap testing all derive from
its purity ([ADR-0001](adr/0001-hybrid-state-model.md)) — and it is the one layer the rebuild did not
touch, deliberately and on the record ([ADR-0029](adr/0029-renderer-stays-go.md)). Hybrid delivery was
the fourth thing that property bought, and [ADR-0028](adr/0028-delivery-spine.md) gave it back: the
purity now buys an artifact whose digest *is* the revision.

**The spine gates the visible parts.** The review found the delivery, build and observation planes
finished but never assembled — high-quality parts with zero callers. The kind E2E test is the exit gate
no later milestone starts without, and the rebuild inherits it: **R1 does not end until a spec deploys
end to end on kind through the artifact spine**, and R2 does not begin on the strength of a design
document.

**The API precedes the UI** ([M7](https://github.com/dafrie/kelson/issues/8) before
[M6](https://github.com/dafrie/kelson/issues/7)). The UI, CLI and MCP server are peers over one
schema ([ADR-0002](adr/0002-tech-stack.md)); kelson's identity is agent-native, so the schema is the
product surface and the UI is its first big client.

**Delegate to Flux; flux-operator only where it is the one that can.** kustomize-controller reconciles
everything kelson delivers ([ADR-0028](adr/0028-delivery-spine.md)), and the PR-preview lifecycle
(`ResourceSet` + `ResourceSetInputProvider`) is flux-operator's — which is now the *only* reason to
install it ([ADR-0030](adr/0030-flux-aio-install.md)). CR-only integration either way: flux-operator is
AGPL-3.0, kelson is MIT.

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
database, so #104 stayed in M9 (v0.1) rather than waiting for M9b or day-2 operations in M14. The hook
shipped, and [ADR-0028](adr/0028-delivery-spine.md) then took its implementation away with the mode it
depended on: `release:` is a validated refusal until the two-`Kustomization` `dependsOn` split is built
([#227](https://github.com/dafrie/kelson/issues/227)). That is a regression, recorded as one.
