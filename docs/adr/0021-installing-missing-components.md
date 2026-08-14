# ADR-0021: Installing missing platform components — pinned upstream manifests, per-object provenance

- **Status:** Accepted (2026-08-14: [ADR-0030](0030-flux-aio-install.md) adds one catalog row whose
  bytes kelson renders at release time rather than fetches, and states the trade against "nothing is
  vendored" explicitly; the detection-first, digest-pinned, per-object-provenance mechanism is
  unchanged.)
- **Date:** 2026-08-14

## Context

[ADR-0003](0003-install-model.md) states the rule that makes kelson adoptable: **never install what is
already there.** [ADR-0005](0005-delegate-to-operators.md) states the other half in a single sentence
and then leaves it: platform components "are detected and adopted per ADR-0003, and **optionally
installed when absent** by referencing upstream charts at a pinned version."

Detection has existed for a while. The offer has not. Until now a cluster without cert-manager was told
it could not have TLS and left to work out the rest, and `docs/install.md` said in as many words that
installing missing components was "a separate, opt-in story:
[#60](https://github.com/dafrie/kelson/issues/60)". This is that story.

Four things make it a design decision rather than a scripting exercise.

**The Bitnami lesson.** Kubero vendored Bitnami Helm charts for its add-ons. When Broadcom withdrew the
free catalog in September 2025, working installations broke and the add-on system had to be reworked
under time pressure. ADR-0005 records the conclusion: depending on someone else's packaging of someone
else's software puts your users' running systems at the mercy of a vendor decision you have no
influence over. Whatever kelson does here must not take custody of anybody's packaging.

**flux-operator is AGPL-3.0 and kelson is MIT.** Installing Flux means installing flux-operator and
creating a `FluxInstance`, and the integration may only ever be CR-only — no Go module of theirs is
imported anywhere in this repository (`docs/architecture.md`, "Living with flux-operator").

**Uninstall just landed** ([#59](https://github.com/dafrie/kelson/issues/59)) and set the standard for
what an ownership claim looks like: `kelson.dev/namespace-ownership: created|adopted`, written by the
plane that performs the apply, at the one moment the question is answerable, and treated as the sole
licence to delete. Issue #60's acceptance criterion is the same claim one layer down — *uninstall
removes only components kelson installed, never pre-existing ones* — and it must be honest in the same
way, not merely careful.

**Detection is a tri-state** ([#144](https://github.com/dafrie/kelson/issues/144)). "It is absent" and
"we could not look" are different answers, and an installer that treats the second as the first will
install a second CloudNativePG beside the one it was not allowed to see.

## Decision

### 1. `kelson install <component>` — detection first, and a refusal is the normal outcome

```
kelson install cert-manager
kelson install flux cnpg --yes
kelson install --all-missing --dry-run
```

The verb takes component names from one table, never a spec: the platform layer is shared by every
project in the cluster and no spec describes it. Preview, confirm, apply — the shape `kelson uninstall`
established, including that the preview prints whether or not `--yes` was passed and that a
non-terminal stdin with no `--yes` is a refusal rather than an assumed yes. Installing writes
cluster-scoped RBAC, CRDs and webhook configurations; nothing that wide should be able to happen
because a pipe answered a question by accident.

Eligibility is detection's answer and only detection's answer, computed **before a byte is fetched**:

| Detection says | What install does |
|---|---|
| Yes — the component is present | **Refuse**, naming the version detected. Never upgrade, never install alongside. |
| No — it looked and it is absent | Install. |
| Unknown — a profile `Gap` hid it | **Refuse**, naming the RBAC permission that would settle it. |

The Unknown row is the one that needed deciding. Installing on an unknown is exactly the guess the
tri-state exists to prevent, and the failure it produces — a second operator whose cluster-scoped CRDs
fight the first — is among the worst things a PaaS can do to a cluster it was invited into.

`--all-missing` drops an already-present component silently (nobody asked about it) and still reports
every Unknown, because "we could not tell" is precisely what a sweep must not swallow.

The declarative, non-interactive path is `--yes` with an explicit component list or `--all-missing`;
`--dry-run` prints the preview and stops. There is no config file and no way to override a pin from
the command line — see §2.

### 2. Pinned upstream install manifests, fetched at install time, verified by digest

Each component installs from the install manifest **its own project publishes**, at a version pinned in
`internal/delivery/install/pins.go`, fetched over HTTPS at install time and checked against a recorded
SHA-256 before anything is applied. Nothing is vendored into this repository.

A row is three facts that must agree — version, URL, digest — plus the profile field whose detection
Gap hides the component. A drift test fails when they disagree, so a half-updated row cannot merge, and
the update procedure is two shell commands recorded in the table's own doc comment. This is the pattern
`internal/clusterprofile/support/matrix.go` uses for version floors and `hack/e2e/lib.sh` uses for the
kind binary and node image: the repository holds the coordinates and the checksum, never the artifact.

**Server-side apply over a Helm engine.** The alternative was to embed Helm and render upstream charts.
Rejected:

- A chart's values schema and image registry are a far larger and less stable surface than a published
  install manifest, and it is the surface the Bitnami incident destroyed.
- Embedding Helm means embedding Helm's release state — a second, invisible source of truth about what
  kelson installed, next to the labels this ADR makes authoritative in §3.
- kelson already server-side applies for a living (`internal/delivery/direct`), with field-manager
  conflict detection and no forcing. Applying a manifest reuses that whole discipline. A field another
  manager owns comes back as a conflict, which is the cluster saying *something else installed part of
  this*, and that is exactly the situation "never modify what kelson did not install" exists for.
- SSA is idempotent, so re-running after a partial failure converges rather than compounding.

The cost is that some components publish no plain manifest — external-secrets is Helm-chart-only, which
is why §5 defers it rather than pretending otherwise.

**No pin overrides on the command line.** A `--version` flag would turn the pins table from the source
of truth into a default, and the digest check into theatre: kelson has no digest for a version nobody
recorded. Installing a different version is a pull request against the table, which is where the change
gets reviewed.

**Air-gap honesty.** Fetching at install time means an install needs network access to the upstream
release host. Without it, kelson refuses, names the URL it could not reach, and applies nothing — every
component is fetched and verified before any component is applied, so a two-component run cannot end
with one installed and one unfetchable. The documented alternative is to mirror the manifest and apply
it yourself; detection then finds the component and kelson adopts it, which is the ADR-0003 path and
needs no new mechanism.

### 3. Provenance is per object, decided at apply time, and it is the only licence to delete

```
kelson.dev/installed-component: <name>       label — the selector handle
kelson.dev/installed-version:   <version>    label — the pin kelson applied
kelson.dev/component-ownership: created|adopted   annotation — the licence
```

Every object in the fetched manifest is read **immediately before its apply**. Absent means this apply
creates it, and it is stamped `created`. Present means it was already there — a `flux-system` namespace
somebody made by hand, a `ClusterRole` left by an earlier install — and it is stamped `adopted`. A read
that fails is fatal for that component: "could not tell whether this existed" must never resolve to
`created`, because `created` is the licence to delete it later. A re-run never downgrades a `created`
object to `adopted`; the annotation records who brought the object into the world, not who wrote to it
last.

Component-level tracking was rejected. "kelson installed cert-manager" is not true of the namespace a
user created last year, and a removal that took it would be exactly the over-deletion the acceptance
criterion forbids. Per-object is more bookkeeping and it is the only version that is not a lie.

`app.kubernetes.io/managed-by: kelson` is **deliberately not stamped**. That label is half of the
project-scoped uninstall selector (`internal/delivery/provenance.go`), and a platform component is not
a project deployment: claiming it there would put cert-manager's ClusterRoles in the same query as an
application's Deployments. Upstream sets that label for its own purposes too, and taking the field from
its owner is the silent overwrite [ADR-0001](0001-hybrid-state-model.md) forbids.

### 4. `kelson uninstall --component <name>` — the same verb, the same promise

Removal shares the uninstall verb because it is the same promise at a different layer: kelson removes
what kelson put there and nothing else. It refuses the project flags (`--project`, `--env`,
`--keep-data`, `--keep-history`, …) rather than ignoring them, because a component has no environment,
no namespace to sweep and no local history, and silently dropping a flag leaves a wrong belief intact.

The sweep is by label, discovery-driven over **both** scopes — a platform component is mostly
cluster-scoped, and a sweep that skipped CRDs, ClusterRoles and webhook configurations would leave a
component's most durable pieces behind while reporting it removed. It reads the cluster and never the
pinned manifest: a removal must work without network access, and the question is what is live and
labelled, not what upstream's manifest said at the version kelson happens to pin today.

Every candidate is re-read immediately before its delete and re-checked against its live label **and**
its live ownership annotation, with a UID precondition on the delete itself. Anything `adopted`,
anything with no annotation, and anything that stopped carrying the label is reported and left standing.

Order is the reverse of the dependency order, and the last two tiers are the ones that needed thought:

1. Custom resources of the component (the `FluxInstance`), so the operator that owns them is still
   running to finalize them.
2. Workloads. 3. Configuration and RBAC.
4. **CustomResourceDefinitions**, and the preview names every live custom resource each one takes with
   it. Deleting CloudNativePG's CRDs deletes every `Cluster` in the cluster — every managed database,
   including ones kelson never rendered. A preview that listed the CRD without saying that would be
   technically complete and practically a trap, which is the same reasoning behind uninstall's `DATA`
   section.
5. The namespace, and only when kelson created it.

### 5. Three components install; two refuse by name

| Component | Status | Why |
|---|---|---|
| `flux` | installs | flux-operator's pinned `install.yaml`, then one `FluxInstance` |
| `cert-manager` | installs | pinned `cert-manager.yaml` |
| `cnpg` | installs | pinned `cnpg-<version>.yaml` |
| `envoy-gateway` | refuses, names the follow-up | claiming a `GatewayClass` beside a cluster's existing routing has a blast radius detection cannot yet rule out |
| `external-secrets` | refuses, names the follow-up | upstream publishes a Helm chart and no plain install manifest, and §2 rejected embedding a Helm engine |

A deferred row is a refusal **with a reason and a follow-up**, not an "unknown component" error that
leaves the user wondering whether they typed it wrong.

**Installing Flux means installing flux-operator.** kelson applies the operator's own pinned manifest
and creates one `FluxInstance` named `flux` (the CRD's own CEL rule requires that name) through the
unstructured dynamic client. No Flux manifests are vendored and no install-or-upgrade lifecycle is
reimplemented — that lifecycle is what flux-operator is for ([ADR-0016](0016-delivery-flows-v0.md)).
The distribution version is pinned to a **minor** (`2.9.x`): patch upgrades within it are the
operator's, which kelson explicitly delegates, and a minor bump is an API-surface decision the support
matrix owns. The controller set includes `helm-controller`, because `kind: helm` renders a `HelmRelease`
and a FluxInstance may legally install a subset that leaves it out.

The `FluxInstance` sets **no `spec.sync`**. Pointing a fresh Flux at a Git repository is a delivery
decision belonging to `kelson deploy --mode flux` and to the user's repository layout, not to the act of
installing Flux. An install that silently started reconciling a repository would be doing something
nobody asked for.

Nothing else is configured either: no `ClusterIssuer` (which ACME account or CA to trust is not
kelson's decision), no `SecretStore`, no databases. The command says so on the way out.

## Rationale

The through-line is that every claim kelson makes here is checkable by the person running it. The
pinned URL and digest are `curl` and `sha256sum`. What kelson installed is
`kubectl get -l kelson.dev/installed-component=<name>`. What kelson may delete is that query narrowed by
one annotation, and the command prints the selector. None of it requires trusting a description of what
kelson did.

Detection-first is what keeps ADR-0003's additive doctrine intact under a verb whose whole job is to add
things. The refusal on Yes is obvious; the refusal on Unknown is the one that matters, because it is the
only place the tri-state can be quietly collapsed into a destructive default.

Per-object provenance costs a read per object and buys the acceptance criterion outright. It is the same
trade `internal/delivery/direct` already makes for namespaces, and the reason it is worth making twice
is that both answer a question that stops being answerable one microsecond after the apply.

## Consequences

**Positive.**
- A cluster with nothing can become a cluster kelson renders fully against, in one command, without
  kelson taking custody of anyone's packaging.
- Uninstall is honest about ownership at object granularity, so removing a component kelson installed
  cannot take a namespace the user made.
- The pins table is one reviewable place with one documented update procedure and a drift test.
- No Helm engine, no chart values schema, no second source of truth about what is installed.
- The AGPL boundary around flux-operator is structural: the only thing kelson knows about it is a URL
  and a CR shape.

**Negative.**
- **Installing requires network access to the upstream release host.** Air-gapped clusters cannot use
  this verb at all, and the documented answer — mirror it yourself, kelson will adopt it — is a worse
  experience than a vendored manifest would give. That is the deliberate price of not repeating Kubero's
  September 2025.
- **kelson now owns an upgrade cadence it did not before.** Every pinned version ages, and a stale pin
  installs a component older than the one the docs describe. Nothing here upgrades a component kelson
  installed: bumping the pin and re-running applies the new manifest, and whether that is a safe upgrade
  path for every component is untested and unclaimed.
- **A digest mismatch is a hard refusal**, including when upstream legitimately re-publishes a release
  artifact. The user is stuck until the pin is updated in kelson. Stated plainly in the error, with the
  `curl | sha256sum` command that shows them what changed.
- **Two of the five named components do not install**, so a cluster missing a Gateway API implementation
  still needs manual work before kelson can route anything.
- **Removing a component's CRDs deletes custom resources kelson never created.** The preview names each
  one and the prompt counts them, but the capability exists and a `--yes` in a script skips the reading.
- The removal sweep lists every kind in the cluster's discovery document, which is more API calls than a
  fixed list would be. Accepted for the reason `internal/delivery/uninstall` accepts it: a fixed list
  silently misses whatever the next upstream release adds.

## Revisit when

Upstream stops publishing a plain install manifest for a component that installs today — at which point
the choice is between embedding a chart renderer (re-opening §2) and dropping the component to a
deferred row. Or when someone needs an air-gapped install badly enough to design a local artifact source
that keeps the digest check meaningful.
