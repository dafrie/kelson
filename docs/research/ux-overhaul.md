# Comparative UI/UX research for a kelson ground-up overhaul

**canine.sh · Coolify · Vercel · Heroku → kelson**

Research date 2026-08-15. Written against kelson's authoring model
(`docs/model.md`), delivery state machine (`docs/statemachine.md`) and the
shipped UI (`ui/README.md`, `ui/src/`).

---

## 0. Evidence grades — read this first

The session's egress proxy blocks `vercel.com`, `devcenter.heroku.com`,
`coolify.io`, `canine.sh` and `docs.canine.sh` (403 at CONNECT, org policy —
verified by `curl` and by `WebFetch`). `github.com` and web search were
reachable. So every claim below carries a grade:

| Grade | Meaning |
|---|---|
| **[W]** | Web-verified this session (search result summaries + GitHub-hosted documents). Source named inline. |
| **[R]** | Repo-verified this session — either kelson's own files, or a competitor's source on GitHub. |
| **[P]** | **Prior knowledge only.** Could not be re-verified because the vendor's own site is blocked. Treat as "probably true as of mid-2026, check before quoting to anyone." |

A design recommendation built on a **[P]** fact is still safe when the fact is
about *shape* ("Heroku's pipeline is columns of app cards") rather than *detail*
("the button is 32px"). Where a **[P]** claim is load-bearing I say so.

**Everything in §3 (synthesis), §4 (expert mode), §5 (rankings) and §6
(identity) is my own recommendation**, not a report of anyone's product.

---

## 1. kelson's model, compressed to what the UI must show

[R] from `docs/model.md`, `docs/statemachine.md`, `ui/README.md`.

**Nouns**

| Noun | What it is | Cardinality |
|---|---|---|
| `Project` | The versioned document. Holds `sources`, `build`, `image`, `env` defaults, and **`components[]`**. | 1 |
| `Component` | The deployable leaf. Kinds: `service`, `worker`, `cron`, `agent`, `postgres`, `valkey`, `helm`. Kind is *derived from shape* (`port:` → service, `schedule:` → cron, neither → worker). Owns its own ServiceAccount and its own spec-hash. **Deploys independently.** | n per project |
| `Environment` | Binds to a Project. Carries `namespace`, `routing`, `secrets.backend`, `policy`, `previews`, `autoDeploy`, and **per-component overrides** (`image`, `imageTracked`, `replicas`, `resources`, `env`, `preset`, `autoDeploy`). | n per project |
| `Source` | `{name, git, ref, connection}`. Declared on the Project (or globally as a `GitSource`); **bound one-to-one by a component's `source:`**. Project-local shadows global. | n |
| `GitConnection` | Forge credential (token or GitHub App). Never named by a Project. | n, instance-scoped |

**Flows**: deploy · diff · history · rollback · promote · logs · previews · build.
All addressed by **(project, environment)** — that pair is what the mutating
RPCs take [R `ui/README.md`].

**Facts the UI uniquely holds and mostly doesn't show today**

1. **Precedence.** Every effective env var, image, replica count and preset is
   the winner of a 2–3 level merge (P1–P5). The resolver knows which scope won.
   The UI shows the raw document instead.
2. **The delivery state machine.** Seven phases
   (`Proposed → Committed → Reconciling → Applied → Healthy`, plus `Rejected`,
   `Degraded`) and — more importantly — **six *answers*** that already exist in
   Go and in TS: `waiting · progressing · live · stuck · rejected · degraded`
   [R `docs/statemachine.md`, `ui/src/components/phase.ts`]. The answer is the
   thing a human needs; the phase is the thing an operator needs.
3. **Correlation / staleness.** The engine knows "the cluster is healthy, but on
   the *previous* revision" and carries `State.Stale` + `State.ObservedRevision`.
   That is drift, computed, sitting unused by any screen.
4. **`autoDeploy` effective value per component**, already resolved
   (`Resolved.AutoDeploy`), plus `StaleComponents(repo, ref)` — "what a push to
   this repo would move."
5. **`imageTracked`** — the difference between "a person pinned this" and "a
   trigger put this here and will move it again."
6. **Provenance stamps**: `kelson.dev/revision`, `kelson.dev/spec-hash`,
   `kelson.dev/promoted-from: <env>@<revision>`.
7. **Structured errors**: `{code, resource, field, message, remediation, docsUrl}`
   with 1-based line/column. The existing `ErrorPanel` is the single best piece
   of UI in the app and should be the template for everything else.

**What is currently wrong, concretely** [R, sampled from `ui/src/`]

- User-visible field notes cite internal records:
  `note="shared by every component (rule P1); a value is a plain string, a reference is a mapping — never a credential (ADR-0009, ADR-0018)"`
  (`pages/EditSpecPage.tsx:635`);
  `note="explicit FQDNs. A cluster with no Gateway API cannot serve them, and the check below says so rather than falling back to Ingress (#140)."`
  (`pages/NewProjectPage.tsx:480`).
- **Stale copy shipping today**: `pages/EditSpecPage.tsx:784` still tells users
  *"previews render in flux delivery mode only (ADR-0017): set the mode above,
  or Check will refuse this document with `render/previews-require-flux`"*, and
  `:697` still asks for `delivery.gitRepo` ("where kelson commits rendered
  manifests — required in flux mode"). `docs/model.md` records that the
  `delivery:` block, delivery modes, and `render/previews-require-flux` are all
  **deleted** (ADR-0028 / #234). The UI is teaching a vocabulary the product no
  longer has.
- `EditSpecPage.tsx` is 1,838 lines; `NewProjectPage.tsx` is 1,199. Two screens
  are 20% of the UI.
- Status vocabulary is operator-facing: the pills are
  `synced · reconciling · degraded · failed · suspended · unknown`
  [R `ui/src/components/phase.ts`]. Three of those six words are Flux's, not a
  developer's.

The "AI sloppy" diagnosis is precise and I'd sharpen it: **the UI is written in
the register of the design record.** It explains *why* at the point of *what*,
it cites its sources, and it uses the repo's rhetorical tic ("X, not Y";
"never a Z"; em-dash chains) in places where a person just needs a label.

---

## 2. Per-product analysis

### 2.1 Vercel

**What it is.** Git-connected build-and-serve platform. The home object is the
**Project** (≈ one repo ≈ one deployable), and the atom of the whole product is
the **Deployment** — an immutable, addressable, permanently-URL'd build.

**Information architecture** [W: `vercel.com/changelog/dashboard-navigation-redesign-rollout`,
`vercel.com/blog/dashboard-redesign` via search; full pages blocked]
The 2026 redesign moved horizontal tabs into a **resizable, hideable sidebar**,
made **tabs consistent across team level and project level**, reordered nav by
"most common developer workflows", and introduced **projects as filters** — one
click switches between the team version and the project version of the same
page. The project overview foregrounds three things: **production deployment
status, the latest deployment, and the git repo connection**, with a screenshot
of the current production deployment.

**Deployments** [W: search on `vercel.com/docs/deployments`, `/deployments/logs`,
`/instant-rollback`, `/deployments/promote-preview-to-production`]
- Statuses: **Queued · Building · Error · Ready**. Status is mirrored into the
  browser tab icon on the deployment inspector.
- Deployment Details page has a **Deployment Summary** section (resources, build
  time, detected framework), **Build Logs**, **Runtime Logs**, and actions
  **Redeploy**, **Assign a Custom Domain**, **Inspect**.
- **Instant Rollback** reassigns domains to an already-built deployment — no
  rebuild — and the docs are explicit that env vars are therefore *not*
  rebuilt. **Promote preview → production** does the opposite: a full rebuild
  with production env vars.
- Env vars are **scoped** to Production / Preview / Development or a custom
  environment.

**Comments** [W: `vercel.com/docs/comments`, `/blog/introducing-commenting-on-preview-deployments`]
Enabled by default on all preview deployments, all plans, free. Comments are
placed on the running UI via the Vercel Toolbar, open threads, **appear in the
associated GitHub PR**, and are collected in an **Inbox** with an unread badge
and a **filter-by-branch** control.

**Where it hides vs exposes.** Hides: the build machine, the CDN, the region
topology, the container. Exposes: **every artifact that has ever served
traffic**, forever, at a stable URL — and the exact diff between "what is
live" and "what a branch would do." Vercel's bet is that *build outputs are the
thing worth being honest about* and infrastructure is not.

#### 5–8 patterns worth stealing → kelson

| # | Pattern | Maps to |
|---|---|---|
| V1 | **The deployment is an object with a permanent URL, not an event in a log.** Every build is inspectable forever. | kelson **already has this and hides it**: a revision is `<generation>-<hash8>`, an immutable OCI artifact, recorded in `Environment.status.history[]`. Give it a route — `/projects/:p/:env/r/:revision` — showing what it rendered, what images it resolved, its diff vs. now, and Rollback/Compare. Today `history` is a five-string table [R `ui/README.md`]. |
| V2 | **Four-word status vocabulary, used identically in every surface** (list, detail, tab favicon). | Replace `synced/reconciling/degraded/failed/suspended/unknown` with the **statemachine's own six answers**, relabelled for developers: **Live · Deploying · Waiting · Failed · Unhealthy · Stuck**. `answerToStatus` already exists in `components/phase.ts`; the phase set moves behind expert mode. |
| V3 | **Rollback is domain reassignment, and the UI says what rollback does *not* do.** | kelson's rollback repoints Flux at an existing revision and **does not re-run release commands** and **does not undo migrations** [R `docs/model.md`]. Put that sentence *in the irreversibility preview* on `/rollback` as a two-line consequence list, the way Vercel states the env-var consequence. |
| V4 | **Promote is a distinct verb from deploy, with different semantics stated on the button.** | kelson's `promote` **writes a pin and never deploys** [R]. That is exactly Vercel's split, and the existing promote screen already honours it. Steal the *labelling*: the button should read "Pin staging's images into production" and the follow-up should be a visibly separate act. |
| V5 | **Env vars are scoped, and the scope is a first-class column** — not a flat KV blob. | kelson's P1 merge is a 3-level chain (Project.env < Component.env < Environment.components[].env). Render an **effective env table** per (component, environment): `KEY · value-or-reference · set at: Project / Component / staging · overridden here?`. This is the single highest-value screen kelson doesn't have. |
| V6 | **Preview comments live where the change is being reviewed** (the PR), not only in the platform. | kelson previews already publish per-PR namespaces with hostnames and phases. Post a **PR comment / commit status** carrying the preview URL, its phase, and a deep link to `/projects/:p/:env/previews/:pr` — the route already exists and ADR-0017 stage 3 anticipates it. Don't build a comment system; **borrow the forge's**. |
| V7 | **Latest-production-deployment screenshot on the overview.** A visual "this is what's live." | kelson can't screenshot, but it has a better answer: the overview cell should show **the resolved image tag + the source ref + how long it's been live**, which is the k8s-native equivalent of "here's what's serving." |
| V8 | **"Projects as filters"** — the same page, scoped up or down, one click. | kelson's flows are all (project, env)-addressed. Make **logs, history, activity and diff work at three scopes** — instance / project / environment — with the same page and a scope chip, instead of only the deepest one. |

#### Do NOT copy

1. **Vercel's "one project = one deployable" collapse.** It works because a
   Vercel project is a repo with one build output. kelson projects hold *n*
   heterogeneous components including databases and Helm charts. Copying the
   collapse would make the project page a lie and would bury `worker`, `cron`
   and `postgres` components. (This is the single most tempting wrong move.)
2. **Deployment-centric history as the only history.** Vercel's deployment list
   is the app's memory because a deployment is atomic. kelson's revisions are
   *environment*-scoped while components deploy independently and carry their
   own spec-hash — a pure revision list will therefore under-report ("nothing
   changed for `worker`" is invisible). History needs a per-component lane.
3. **Marketing-grade motion and the screenshot-heavy card wall.** Vercel can
   afford visual richness because the product is visual output. A self-hosted
   k8s console rendering big cards with hero imagery reads as filler; it is
   also the fastest route back to "AI sloppy."

---

### 2.2 Heroku

**What it is.** The original PaaS. Home object is the **App**. Apps are grouped
into **Pipelines**, whose stages are environments.

**Information architecture** [W: `blog.heroku.com/heroku-dashboard-tour`,
`devcenter.heroku.com/articles/heroku-dashboard`, `/articles/pipelines` via
search; pages blocked]
- App tabs: **Overview · Resources · Deploy · Metrics · Activity · Access
  (Collaborators) · Settings**. Overview is "at-a-glance information about an
  app's resources (dynos and add-ons), metrics, collaborator activity, and
  deployment activity", with detail one tab away.
- **Pipelines** organise apps sharing one codebase into **review /
  development / staging / production** stages; the pipeline overview page
  "tracks the real-time progress of code and features from development to
  production" and gives "meta-information about the status of each stage."
- **Promote to Production** is a button on the *staging app card* in the
  pipeline view, followed by a review-and-confirm; promotion **copies the
  compiled slug without rebuilding**.
- **Review Apps**: a temporary, shareable app per pull request, via the GitHub
  integration, enabled per pipeline.
- **Activity** shows "all code, config, and resource changes by every developer
  working on an app", who made them and when; **rollback is performed from
  Activity**; with a GitHub repo configured, each deploy row carries a
  **clickable diff link**.
- **Config Vars** live under Settings behind a **"Reveal Config Vars"** button —
  hidden by default, then an editable KV grid.

[P] Not re-verifiable this session, but well established: releases are numbered
(`v43`), a release is created by *any* change — code, config var, add-on — so
config changes are first-class history rows; `release phase` runs migrations
before a release goes live; the dyno formation UI is a per-process-type row with
a size selector and a quantity control.

**Where it hides vs exposes.** Hides: the machine, the slug compiler, the
routing mesh, the filesystem. Exposes: **the sequence of changes** — the
activity feed is the product's spine and is why Heroku still feels trustworthy.
It also exposes *what a change costs* (dyno counts and add-on tiers with prices
on the same page as the toggle).

#### 5–8 patterns worth stealing → kelson

| # | Pattern | Maps to |
|---|---|---|
| H1 | **The pipeline view: columns are stages, cards are apps.** One screen shows the whole delivery topology and the promote arrows between stages. | **This is kelson's project page.** Rows = **Components**, columns = **Environments**, cell = phase pill + revision + relative time. It transplants exactly: Heroku pipeline ≈ kelson Project; Heroku stage ≈ kelson Environment; Heroku app ≈ kelson **Component**. It also satisfies the owner's "the component is the lifecycle-bearing thing" without breaking the (project, env) API addressing. |
| H2 | **Promote is a button *on the source card*, aimed at the next stage.** Direction is visible in the layout. | kelson's promote screen is addressed by the **target** and asks for a source [R `ui/README.md`]. Keep the target-addressed route (it's the right reading), but add the **source-side entry point** on the matrix: a "→ production" affordance on staging's cell, which lands on the same target-addressed screen with `?from=staging`. Both readings, one screen. |
| H3 | **Activity feed as the app's spine: code, config *and* resource changes in one stream, with attribution and a diff link per row.** | kelson's `History` is publish-only [R], and `message` is free prose. Build a **Deliveries** feed at three scopes (instance / project / environment) fed by `History` + `Environment.status` + the `kelson.dev/promoted-from` annotation. Each row: *what changed* (image / env / preset / promotion / rollback), *which components moved*, *by what* (person, `autoDeploy` push, `ci report-build`), *when*, and **Compare to now** + **Roll back** actions. The "who" gap (#74) is real — say "unattributed" once, in the header, not on every row. |
| H4 | **Rollback lives in history, not in a separate destination.** | kelson has `/history` and `/rollback` as sibling routes. Make rollback an **action on a history row** that opens the irreversibility preview inline. Keep `/rollback?to=` as the deep link. |
| H5 | **Config vars are hidden until revealed, then fully editable in place.** Secrecy is a posture, not a wall. | kelson never has values (by construction — `ListSecrets` has no value field [R]). Turn the constraint into the feature: the Secrets panel shows **name · keys · age · which components reference each key**, and the "reveal" affordance is replaced by **"used by"** — the thing kelson *can* answer and Heroku can't. |
| H6 | **Review Apps are a pipeline-level toggle with an explicit enablement step**, not magic. | kelson's `previews:` block needs *two halves* (the block + something publishing artifacts) and the failure mode is silent waiting [R `docs/model.md`]. Model previews as a **checklist with three named preconditions** — flux-operator present, `previews:` configured, artifacts being published — each with its own state and its own fix. The `awaiting-artifact` phase already exists; make it a first-class "you're missing half of this" state, not a row in a table. |
| H7 | **Tabs are a *stable* set across every app**, so muscle memory transfers. | Adopt a fixed component-page tab set: **Overview · Config · Logs · History · Settings**. Do not vary tabs by component kind — vary the *content*; a `postgres` component's Config tab shows preset + capacity, a `service`'s shows env + replicas + domains. |
| H8 | **Every change creates a numbered release, including a config-only change.** | kelson already does this structurally: an env edit bumps `.metadata.generation` and produces a new `<gen>-<hash8>` artifact. **Say so in the UI**: after saving a spec edit, show "this creates revision 47" *before* the save, not after. |

#### Do NOT copy

1. **The 7-tab app page as the default depth.** Heroku's tab set is a filing
   cabinet from 2013; Overview is a digest of the other six and most users
   bounce between Resources and Settings. kelson should ship **five tabs, one of
   which (Settings) is expert-leaning**, and put "Overview" content on the page
   itself rather than in a tab.
2. **Prose-only activity rows.** Heroku's activity messages are human sentences
   ("Deploy 3f2a1b by dave@…"), which is exactly the trap kelson's `History` is
   already in: `message` carries the outcome, the digest and the images as free
   text with no seam to parse [R `ui/README.md`]. Fix the **wire format**
   (add fields to `HistoryEntry`) rather than teaching the UI to regex prose.
   Otherwise every improvement to the feed is a parser.
3. **Add-on marketplace framing for data services.** Heroku add-ons are
   third-party SaaS with tiers and prices. kelson's `postgres`/`valkey` are
   *your* operators in *your* cluster. A marketplace shell would imply a vendor
   relationship that doesn't exist and would push toward the exact "container
   with a PVC" framing kelson's competitive analysis calls out as the
   category's failure [R `docs/competitive-analysis.md`].

---

### 2.3 Coolify

**What it is.** The self-hosted incumbent (60k+ stars [R
`docs/competitive-analysis.md`]). Docker/Swarm-centric. Home is a
**Server/Project tree**.

**Information architecture** [W: DeepWiki on `coollabsio/coolify`,
`coolify.io/docs/get-started/concepts` via search]
**Team → Project → Environment → Resource**, with **Servers** owned by teams and
resources placed on servers. Environment variables inherit across **four
levels** (team, project, environment, resource) with override.

**Design system** [W: `github.com/coollabsio/coolify/blob/main/DESIGN.md` —
directly readable]. This is the most useful document any of the four has
published, and it is worth reading in full:
- "Compact, product-focused aesthetic centered on restraint and density":
  **13–14px typography, 32px controls, 8px radius**, "near-neutral layered
  surfaces instead of large bordered boxes", **hairline rings replace heavy
  borders**, full-width data tables for collections.
- **Three-layer navigation**: global sidebar (32px rows, outline glyphs, pill
  active state) → **layer-2 fixed bar** with resource route tabs and contextual
  actions → settings sidebar (210px, grouped, sticky). Rule: *"layer-2 tabs
  should only appear for genuine sibling routes; collections don't require a tab
  purely to fill the bar."*
- Dense tables: "Cloudflare-inspired" — toolbar with left search, right
  filters/sort, **40px headers, ~48px rows**, footer pagination. **"Status uses
  compact badges rather than large colored chips."**
- **Unsaved changes = a floating bottom-center pill** with reset + save, "not
  full-width footers."
- Nesting rule: **"outer radius = inner radius + visible inset."**
- Theme-aware accent: purple in light, yellow in dark, "for sufficient
  contrast"; active nav uses **solid neutral fills, not accent gradients**.

**The honest failure record** [W: discussions
[#2508](https://github.com/coollabsio/coolify/discussions/2508) and
[#5687](https://github.com/coollabsio/coolify/discussions/5687)]:
- "You need to navigate between 2–3 pages before arriving at configuration for
  any given service"; multiple pages before start/stop/restart.
- "The dashboard barely displays information and barely serves a purpose apart
  from being a projects page." Users can't act from it.
- **Inconsistent save behaviour**: some fields autosave, some need a Save
  button.
- Domain and exposed port share one field → "many people accidentally remove the
  port when editing the domain … which leads to problems for the proxy."
- Asked for: health/status at a glance, server metrics, recent activity, **Cmd+K
  search**, "sort by", error highlighting, **card/list density toggle**, and a
  4th breadcrumb to switch config/deployments/logs/terminal.
- [R `docs/competitive-analysis.md`] the v5 announcement is candid that the
  codebase "has few tests, lacks strict rules, and lacks proper structure."

**Where it hides vs exposes.** Hides very little — that is its brand and its
problem. Exposes: raw Docker labels, proxy config, SSH-level server state, the
full option surface per resource. Coolify is the category's cautionary tale
about what happens when *nothing* is behind a disclosure.

#### 5–8 patterns worth stealing → kelson

| # | Pattern | Maps to |
|---|---|---|
| C1 | **Adopt DESIGN.md's density numbers wholesale**: 13–14px type, 32px controls, 8px radius, hairline rings, near-neutral layered surfaces, full-width tables for collections. | kelson's `ui/src/styles/tokens.css` + `pages.css`. This is a free, tested calibration for exactly kelson's genre (self-hosted console, dark-first, hand-rolled CSS, no component library) and it is the fastest cure for "AI sloppy": generated UIs default to big cards, big radii, big padding. Dense and hairlined reads as *designed*. |
| C2 | **"Status uses compact badges rather than large colored chips."** | kelson's `StatusPill` is currently the loudest object on the projects grid. Shrink it; let **position and repetition** carry scanning, and reserve saturated colour for the ~5% of rows that are not `Live`. |
| C3 | **The floating bottom-center unsaved-changes pill.** | `/projects/:p/edit` currently guards navigation twice (`useBlocker` + `beforeunload`) [R] but has no persistent affordance. A pill with **Reset · Diff · Save** — where **Diff is the primary** and Save is behind it — makes kelson's "a diff before every save, for every environment" rule *visible* instead of modal. |
| C4 | **Layer-2 tab discipline**: tabs only for genuine sibling routes; a collection doesn't get a tab to fill the bar. | Directly resolves kelson's flow sprawl. `deploy`, `diff`, `history`, `rollback`, `promote`, `logs` are currently six sibling routes under `(project, env)` [R]. Under this rule: **Logs and History are tabs** (sibling views of the same subject); **Deploy, Promote, Rollback are actions** that open a focused task surface; **Diff is a mode of Deploy and of History**, not a destination. Six routes → two tabs + three actions. |
| C5 | **Multi-level variable inheritance shown as inheritance.** Coolify has four levels and users still ask for a clearer scope switcher — the lesson is that the levels must be *rendered*, not just *implemented*. | kelson's P1–P5 chain, rendered as the effective-env table from **V5** with a `set at:` column and a strikethrough on the shadowed value. Same treatment for image (P3: Project → Component → Environment pin, plus `--image` and `imageTracked`), replicas/resources (P2), and preset (P5). |
| C6 | **Cmd+K over everything** (their most-requested missing feature). | kelson's namespace is small and typed: projects, components, environments, revisions, secrets, previews, log queries. A palette that resolves `web@staging` → the component page, and `47` → revision 47, is a day of work and removes most of the "2–3 pages to reach config" complaint before it can happen. |
| C7 | **A dashboard you can act from.** Coolify's #1 complaint is a home screen that only links onward. | kelson's `/projects` grid is exactly this today: cards that link. Every home row should carry **one primary action** appropriate to its state (`Deploying` → View stream; `Failed` → See the error; `Stuck` → Check Flux; `Live` + spec ahead → **Deploy**; `Live` → nothing, and *show* nothing). |
| C8 | **Separate fields for separable values** (their domain+port bug). | The general rule: **never let two independent facts share one input.** In kelson: `source` name vs. `ref`; `image` repo vs. tag/digest; `domains` vs. `domainSuffix` defaulting; `secret:` name vs. `key:`. The existing `EnvValueFields` component already gets this right for the three env forms — make it the pattern, not the exception. |

#### Do NOT copy

1. **Exposing everything by default.** Coolify's option sprawl is the
   documented cause of "powerful, but we found the sheer number of configuration
   options overwhelming" [R `docs/competitive-analysis.md`]. kelson's answer is
   the expert-mode line in §4 — and, unlike Coolify, kelson has a *principled*
   place to put the overflow: the YAML tab, which is always complete.
2. **Inconsistent save semantics.** Coolify's mixed autosave/explicit-save is
   named as a top-4 problem by its own maintainers. kelson must pick **one**:
   *nothing saves until you press Save, and Save always shows a diff first.*
   That rule is already the design [R `ui/README.md`]; it just needs to be
   visibly true everywhere including the Secrets panel and the connections form.
3. **The server/infrastructure tree as the primary spine.** Coolify's IA leads
   with Servers because Coolify manages machines. kelson is single-cluster
   single-tenant by decision (ADR-0031) and delegates the cluster to Flux and
   operators. `/cluster` is a **utility page**, not a top-level peer of
   projects. Promoting infrastructure into the spine would import Coolify's
   navigation depth for no benefit.

---

### 2.4 canine.sh

**What it is.** "Coolify is to a VPS as Canine is to Kubernetes." Rails,
Apache-2.0, ~2.9k stars, Postgres-backed, generates and applies Helm charts
directly [R `docs/competitive-analysis.md`]. kelson's most direct competitor.

**Information architecture** [R: `config/routes.rb` read from GitHub — the most
reliable evidence available for this product]. Resource nesting, verbatim shape:

```
clusters              → metrics, build_cloud, cluster_packages
                        member: transfer_ownership, download_kubeconfig,
                                download_yaml, logs, test_connection, retry_install
projects              → services → resource_constraint, jobs, domains (domains: check_dns)
                        environment_variables
                        deployments  (collection: deploy; member: redeploy, kill, kill_deploy)
                        project_add_ons, volumes, metrics, notifiers
                        processes (member: shell)
                        project_forks, development_environments, workbench
                        cluster_migration, development_environment_configuration
                        member: restart
add_ons               → cluster_migration, metrics, endpoints, processes
                        collection: search, metadata, fetch_helm_repository_index
                        member: restart, download_values
onboarding            (index, create; collection: account_select)
webhooks              github | gitlab | bitbucket
api/v1                me, projects (deploy, restart, processes), builds (kill),
                      clusters (download_kubeconfig), add_ons (restart)
mcp                   handler + OAuth dynamic client registration
```

[W: README + search] Advertised features: **"Automated Deployments"**,
**"Built-in Image Building"** (Dockerfile or buildpacks), **"Service
Management"** (web services, background workers, scheduled jobs), **"Resource
Constraints"** (CPU/memory/**GPU**), **"Domain & SSL"**, **"Secrets & Config"**,
**"Persistent Storage"**, **"Multi-tenancy"**, **"Custom Pod Templates"**,
**"Enterprise SSO"** (SAML/OIDC/LDAP, free). One-click add-ons "to deploy
databases and other services with pre-configured settings".

**Where it hides vs exposes.** Hides: the Helm chart it generates, the manifests,
the apply. Exposes: `download_kubeconfig`, `download_yaml`, `download_values`,
per-process **`shell`**, cluster `logs`, `test_connection`, `retry_install`. The
posture is *"we abstract it, and here's the escape hatch, labelled"* — which is
the right instinct and worth beating rather than avoiding.

#### 5–8 patterns worth stealing → kelson

| # | Pattern | Maps to |
|---|---|---|
| N1 | **The vocabulary is *jobs to be done*, not Kubernetes**: "background worker", "scheduled job", "add-on", "process", "volume" — and Kubernetes appears only in escape hatches. | kelson's model **already derives kind from shape** (`port:` → service, `schedule:` → cron, neither → worker) [R `docs/model.md`], which is a stronger version of the same idea. The UI should *speak the derived kind*: a component page header reads **"web service"**, **"background worker"**, **"scheduled job — every day at 03:00"**, **"Postgres database"** — never `kind: service`, never `Deployment`. Kubernetes nouns appear only under expert mode. |
| N2 | **`processes` with a per-process `shell` action.** A live list of what is actually running, with a terminal one click away. | kelson has replicas and pods but no pod list. Add a **Processes** section on the component page: pod name, phase, restarts, age, node — with the per-pod colour token already used by the log tail (`--kelson-pod-1…8`) [R `ui/README.md`], so a pod's colour is consistent between the process list and its log lines. (A shell is out of scope for now; say "use `kubectl exec`" and *give the exact command with the names filled in* — a copyable command is a legitimate escape hatch.) |
| N3 | **Explicit `download_kubeconfig` / `download_yaml` / `download_values` actions.** The escape hatch is a labelled button, not a hidden mode. | kelson's equivalent is far stronger and is currently invisible: **the rendered manifest set is a real artifact**. Add **"Download rendered manifests"** and **"Copy `flux`/`kubectl` command"** to the environment page and to every revision page. This is the honesty move Coolify gets credit for and canine ships as a button. |
| N4 | **A first-class `onboarding` controller with `account_select`** — onboarding is a *route*, not a modal. | kelson already has `/setup` [R]. Make it the **post-install default landing page** until the first successful deploy, and make it a **checklist with a finish line**: cluster reachable → platform components present → git connection → first project → **first deploy**. The current `/setup` stops at platform components; the value is in carrying the user to a running URL. |
| N5 | **`project_forks` + `development_environments`** — cloning a project into a throwaway environment is a modelled operation. | kelson's `Environment` is already a first-class document bound to a Project. Ship **"Duplicate environment"**: copy an Environment document, change name + namespace + `domainSuffix`, show the diff, store. Two RPCs kelson already has (`GetSpec`, `PutSpec`), one screen, and it makes environments feel cheap — which is the whole point of having them. |
| N6 | **Add-on catalog with `search` / `metadata` / `fetch_helm_repository_index`.** Browsing charts is part of the product. | kelson has `kind: helm` and refuses to template Helm itself [R `docs/model.md`]. A **chart picker** on "Add component" that reads a repo's `index.yaml`, offers chart + version, and writes the `chart` / `chartVersion` / `source.repository` / `values` block is entirely renderer-safe (it's authoring, not rendering) and closes the biggest gap in `appendComponent`, which today can't write `helm` components at all [R `ui/README.md`]. |
| N7 | **`check_dns` as an action on a domain.** Verify the thing the user just typed, right there. | kelson's `domains:` are explicit FQDNs and the current field note explains Gateway API in 25 words [R `NewProjectPage.tsx:480`]. Replace the explanation with a **check**: resolve the name, compare against the gateway's address, show ✓/✗ and the one fix. A check is worth ten sentences. |
| N8 | **Deployments as a nested collection with `deploy`, `redeploy`, `kill`, `kill_deploy`** — including *stopping* an in-flight deploy. | kelson's `Deploy` streams and settles [R]; there is no cancel. The deploy stream screen should carry **Stop watching** (client-side, honest) distinctly from any future **Cancel deploy** (server-side) — and until the latter exists, say plainly what the button does. canine's four verbs are a good reminder that a deploy has a *middle*, not just a start and an end. |

#### Do NOT copy

1. **State in Postgres, UI as source of truth.** canine's deployment path is
   `helm_deployment_service.rb` generating and applying charts, with state in
   Rails models; zero hits for `argocd`, `flux`, `gitops`, `kustomize`,
   `server_side_apply`; three incidental hits for `dry_run` [R
   `docs/competitive-analysis.md`]. kelson's entire differentiation is the
   opposite. **Do not import any UI pattern that implies the database knows
   best** — especially "edit here and it's live", inline autosave on spec
   fields, or a settings page with no diff.
2. **The flat `add_ons` top-level collection.** canine has both
   `project_add_ons` (nested) and `add_ons` (global), which duplicates the
   concept at two scopes. kelson's `postgres`/`valkey`/`helm` are **components
   in the same list as everything else** by explicit decision (ADR-0014) [R].
   Do not grow a second "Services" or "Add-ons" navigation peer — render data
   components *in the component matrix*, visually distinguished by kind.
3. **`cluster_migration` on every resource.** Moving a resource between clusters
   is modelled per-project and per-add-on. kelson is single-cluster by decision
   (ADR-0031, with multi-cluster deliberately unspecified) [R]. Adding a
   migration affordance now would prejudge #232 in the UI — the exact mistake
   ADR-0031 avoided in the schema.

---

## 3. Synthesis: the recommended information architecture

### 3.1 The home screen's object

**Recommendation: the home object is the Component-in-an-Environment, presented
as a matrix, grouped by Project.**

Reasoning, in one line each:

- The owner is right that the **Component** is the lifecycle-bearing thing, and
  the model agrees: components deploy independently, each carries its own
  spec-hash, an unchanged component produces no rollout [R `docs/model.md` §6].
- But a component alone has no phase, no image and no URL — those are all
  *per-environment* (P2/P3/P5 overrides, namespace, routing). So the atom with a
  status is **(component, environment)**.
- The API's addressable unit is **(project, environment)** [R `ui/README.md`],
  and `?component=` already exists as a sub-selector on the logs route. So a
  component page is `(project, env)` + a component name — **no API change
  required**.
- Heroku's pipeline proves the layout: stages across, apps down. kelson's
  version is **components down, environments across**, which is the transpose
  and reads better because kelson has more components than environments.

Current `/projects` shows **one card per (project, environment)** [R
`ui/src/pages/ProjectsPage.tsx`] — a project with 4 components × 3 environments
is 3 cards that each summarise 4 things. The matrix shows all 12 cells.

### 3.2 Navigation

```
/                          Home
                           ├ "Needs attention" band (only non-Live cells; empty when all good)
                           └ one compact matrix strip per project
/p/:project                Project — the full matrix (components × environments)
                           + sources & their bindings, + connections in use
/p/:project/:env           Environment — one column expanded: every component's
                           row, plus namespace, domains, secrets, previews, policy
/p/:project/:env/:component        THE page. Tabs: Overview · Config · Logs · History · Settings
/p/:project/:env/r/:revision       A revision: what it rendered, images, diff vs now, roll back
/p/:project/:env/previews/:pr      One preview (route exists)
/deliveries                Activity feed, instance-wide; ?project= / ?env= narrow it
/cluster                   Nodes, ClusterProfile, platform components  (utility, not a peer)
/connections               Git connections + global GitSources          (utility, not a peer)
/setup                     Onboarding; default landing until first successful deploy
```

Keep every current route as a **redirect** into the new shape
(`/projects/:p/:env/logs?component=web` → `/p/:p/:env/web/logs`). The
`ui/README.md` promise that "a link into a deploy or a log tail is a link that
keeps working" must survive the overhaul.

**Flows become actions, not destinations** (Coolify's layer-2 rule, C4):

| Flow | New home |
|---|---|
| Logs | **Tab** on the component page (component preselected — the query param becomes the route) |
| History | **Tab** on the component page (component's lane) and on the environment page (all lanes) |
| Diff | **Not a destination.** It is the body of the Deploy action, of the pre-Save guard, and of a history row's "Compare to now". |
| Deploy | **Action** from a cell / the environment header → focused task surface (diff → confirm → live stream) |
| Promote | **Action** from a source cell (`→ production`) and from the target environment header; one target-addressed screen |
| Rollback | **Action** on a history row → irreversibility preview inline |
| Previews | **Section** on the environment page + a per-PR route |
| Build | **Never its own screen.** It's a phase inside Deploy and a row in Deliveries. |

Six sibling routes → two tabs and four actions. That alone removes most of
Coolify's documented "2–3 pages to reach anything" failure before kelson can
inherit it.

### 3.3 What each level shows

**Home.** A band of cells that need attention, then one strip per project. A
cell is: component name · phase glyph · relative age · one action. Nothing else
by default. When everything is `Live`, the attention band is **absent**, not
empty-with-a-message.

**Project.** The matrix. Row header = component name + derived kind label
("web service", "background worker", "Postgres database"). Column header =
environment name + `autoDeploy` state + namespace (expert). Cell = phase + the
short revision + relative age. Below the matrix: **Sources**, showing each
declared source, its ref, its connection, which components bind to it, and —
critically — **which sources are shadowing a global `GitSource`** (the model
explicitly asks the UI to say so [R `docs/model.md`]).

**Environment.** One column, expanded: every component as a row with its
effective image, replicas, and phase; then Domains, Secrets (with "used by"),
Previews (with the three-precondition checklist), and Policy. Expert reveals
namespace, gatewayClass, secrets backend and the ClusterProfile findings that
matter here.

**Component.** The page the owner wants to exist. Above the tabs, a persistent
**current-revision card** with exactly five facts and one action:

```
web · web service · staging

  Live   2 hours ago                                    [ Deploy ]
  ghcr.io/acme/checkout:v1.4.2   ←  app @ main (3f2a1b)
  revision 47 · auto-deploys on push to main
```

Everything else is a tab. The card is the same object on every component, in
every environment, at every phase — which is what makes a console feel
designed.

---

## 4. Where the expert-mode line sits

**The control.** One global toggle, persisted like the theme toggle already is
(`localStorage`, three states is overkill here — two is right). Label it for
what it reveals, not for who the user is: **"Kubernetes detail"**, not "Expert
mode" or "Advanced". It never gates an *action*, only *information* — any task
must be completable with it off.

**The escape valve that makes the simple view survivable.** Every simplified
statement gets a **"why"** affordance: a small caret that expands the evidence
*in place*. Expert mode is then just "expand all whys, permanently." This is
what keeps the default from feeling like it's hiding things, and it is the
direct replacement for the current habit of writing the explanation inline.

| Screen | Simple default shows | "Kubernetes detail" adds |
|---|---|---|
| **Home** | Component · environment · phase word · relative age · one action | Namespace, revision tag `<gen>-<hash8>`, spec-hash, whether the observation is stale |
| **Project matrix** | Cell: phase + age. Row: derived kind in plain words | Per-cell revision + image digest (elided), Flux `Kustomization` / `OCIRepository` names, generation |
| **Component · Overview** | Current-revision card (5 facts), replicas as "3 running", recent deliveries (3 rows), the one action | ServiceAccount name, pod list with node/restarts, resource requests/limits as quantities, `kelson.dev/*` labels, the resolved manifest list |
| **Component · Config** | Effective env table (`KEY · value-or-🔒reference · set at`), image source (built-from vs pinned), replicas, domains | The three-scope merge shown as a chain with shadowed values struck through; `imageTracked` state and what it means for the next push; the raw override block; `secretKeyRef` targets |
| **Component · Logs** | Tail, this component, pod colours, find-in-page | Pod/container selector, timestamps, `since`/`until`, the server-side `LogMatch` query, replica attribution detail |
| **Component · History** | Timeline: *what changed* · when · by what · Live badge on one row | Revision tag, spec-hash, artifact digest, `promoted-from` annotation, the raw `message` string |
| **Deploy** | Plain-language change list ("2 env vars, 1 image, 1 replica count") + Deploy | Full manifest diff, per-resource server-side dry-run verdicts, render errors with codes and JSONPaths |
| **Rollback** | Target revision, what it will restore, **the two consequences** (migrations aren't reversed; release commands don't re-run) | The manifest diff between now and the target, the artifact tag being repointed |
| **Promote** | Source → target, the component table (pinned / unchanged / skipped, with reasons), the resulting change | Full digests, the `Environment` patch that will be written, `resourceVersion` |
| **Environment** | Name, domain suffix, autoDeploy, "secrets: from the cluster", previews on/off | Namespace, gatewayClass, TLS, secrets backend + store + refreshInterval, the whole `policy:` block, ClusterProfile findings |
| **Secrets** | Name · keys · age · **used by which components** | Namespace, backend, `ExternalSecret` Ready condition + last sync, sops recipients, the ready-to-paste reference (keep — it's good) |
| **Data services** | Preset in numbers ("3 instances · 4 vCPU · 100 GiB · 1 sync replica") and health | CNPG `Cluster` name, storage class + snapshot capability + confidence, the generated Secret name and its keys |
| **Previews** | PR number · title · phase · URL · age | Namespace, the commit the `OCIRepository` actually pins vs. the PR head, `ResourceSet` / `ResourceSetInputProvider` conditions |
| **Edit spec** | Form | YAML tab (**already exists and is already right** — keep the byte-identical rebuild guard exactly as is) |
| **Cluster** | Ready/not per platform component, node count, "you can run X" | Full ClusterProfile YAML, per-finding confidence, versions, install plans |

**Two things that must never be behind the toggle**, because hiding them would
be dishonest rather than simplifying:

1. **"We could not tell."** The `unknown` state and the `Status`-failed card
   must render at both levels. `ui/README.md` already commits to this ("a status
   that could not be read is never rendered as green") — it is the best
   principle in the current UI and it survives the overhaul untouched.
2. **Structured error codes.** `ErrorPanel`'s code chip + message + `fix:` +
   docs link stays visible always. A user pasting `render/external-secrets-store-ambiguous`
   into a search box is the fastest support path the product has.

---

## 5. The ten highest-leverage changes, ranked

Ranked by (impact on the "AI sloppy" complaint × breadth × how much it uses
machinery kelson already has).

| # | Change | Why it's here | Cost |
|---|---|---|---|
| **1** | **Rewrite user-facing copy against a written standard.** No ADR numbers, no issue numbers, no "rule P1", no file paths, no `render/*` codes in *prose* (they stay in the code chip). Field notes ≤ 10 words. One sentence per empty state, plus one action. Kill the house rhetorical tics ("X, not Y"; "never a Z"; em-dash chains) in UI strings — they're right in docs and wrong on a label. Move every *why* into a "why" caret or a docs link. | This **is** the complaint. It's also the cheapest change on the list and it makes every other change look better. Start with `EditSpecPage.tsx` and `NewProjectPage.tsx` — 3,000 lines, 20% of the UI, and where every cited example lives. | S |
| **2** | **One status vocabulary, developer-facing, everywhere.** Surface the statemachine's six **answers** (`live · waiting · progressing · stuck · rejected · degraded`) as **Live · Waiting · Deploying · Stuck · Failed · Unhealthy**, plus **Unknown**. Phases move behind expert mode. `answerToStatus` already exists. | The current pills (`synced`, `reconciling`, `suspended`) speak Flux, not developer. One vocabulary used identically in list, detail, stream and tab title is the strongest single signal of a designed product (Vercel's whole status system is four words). | S |
| **3** | **The project page becomes the component × environment matrix**, and `(project, env, component)` becomes an addressable page with the current-revision card. | Transplants Heroku's pipeline view onto kelson's actual model, and makes the component the visible lifecycle unit — the owner's stated requirement — without touching the API. Replaces a card grid that summarises 4 things into 1 card. | L |
| **4** | **The effective-config table** (env vars first, then image / replicas / resources / preset), with a `set at:` column, shadowed values struck through, and references rendered as references. | kelson computes P1–P5 and shows none of it; users currently reverse-engineer a 3-level merge by reading two YAML documents. Nothing else on this list turns as much hidden machinery into visible value. Also fixes the worst wall of text (`EditSpecPage`'s env section). | M |
| **5** | **Delete the stale delivery-mode vocabulary from the UI.** `EditSpecPage.tsx:697` (`delivery.gitRepo`) and `:784` (`render/previews-require-flux`, "flux delivery mode") describe a model ADR-0028/#234 removed. | The UI is actively teaching users a concept that no longer exists. This is a correctness bug wearing a copy bug's clothes, and it will make any user who reads the docs distrust the screen. | S |
| **6** | **Deliveries feed** (Heroku Activity) at instance / project / environment scope: what changed, which components moved, triggered by what (person · push · CI report · promotion), when — with **Compare to now** and **Roll back** per row. Requires adding fields to `HistoryEntry` rather than parsing `message`. | Gives the product a spine and a memory. It is also the only screen that makes `autoDeploy` legible: right now a push silently rewrites `Environment.spec.components[].image` and nothing narrates it. | M (+ proto) |
| **7** | **Density and chrome pass** against Coolify's DESIGN.md numbers: 13–14px, 32px controls, 8px radius, hairline rings, layered near-neutral surfaces, full-width dense tables (40px headers / 48px rows), compact status badges, one elevation. Plus the floating bottom-center unsaved-changes pill with **Diff** as the primary. | This is what makes it *look* designed rather than generated. Generated UIs converge on big cards, big radii, generous padding, loud pills — precisely the current look. kelson already has hand-rolled CSS on tokens with no framework, so this is a token-and-primitives edit, not a rewrite. | M |
| **8** | **Drift and reconciliation made visible** — the signature Kubernetes-native surface (see §6). The engine already computes `State.Stale` and `State.ObservedRevision`: "the cluster is healthy, on revision 44; you deployed 47." | **No competitor can show this.** Vercel and Heroku own the applier so drift is impossible; Coolify and canine apply directly and don't model it. This is the one screen that could only exist in kelson, and it currently isn't drawn. | M |
| **9** | **Flow consolidation**: six sibling routes → Logs/History as tabs; Deploy/Promote/Rollback as actions; Diff as a *mode* of Deploy, Save and History. Old routes redirect. | Prevents kelson from shipping Coolify's documented #1 complaint. Also removes three top-level nav concepts a new user must learn before deploying anything. | M |
| **10** | **Onboarding that ends at a running URL.** `/setup` becomes the default landing until the first successful deploy, and becomes a 5-step checklist: cluster reachable → platform components → git connection → project → **first deploy**. `/projects/new`'s three-question form stays; its "More options" drawer gets the §1 copy treatment. | Time-to-first-deploy is the price of entry in this category [R `docs/competitive-analysis.md`], and kelson's current onboarding stops at "platform components installed", which is not value. | M |

**Runners-up** (real value, below the line): Cmd+K palette (C6); "Duplicate
environment" (N5); the chart picker for `kind: helm` (N6); domain DNS check
(N7); process/pod list with shared pod colours (N2); PR comment + commit status
carrying the preview link (V6); "Download rendered manifests" (N3).

---

## 6. Identity: three directions

The brief: make kelson feel *designed* rather than generated, grounded in what a
Kubernetes-native tool can uniquely show — **live reconciliation, drift, and the
delivery state machine**. Each direction below is concrete about type, density,
colour posture, and one signature interaction. All three assume the existing
dark-first, two-theme token system and the `#0FA36B` / `#0b724b` mark
[R `ui/README.md`].

### Direction A — "The Console"

*The product is an instrument panel for a system that is always moving.*

- **Type.** Two faces, one rule: **every machine-generated value is mono, every
  human-written value is not.** Revisions, digests, namespaces, pod names,
  codes, hostnames → mono. Component names, environment names, prose → the UI
  sans. Applied without exception this is a legibility system, not decoration —
  a reader learns in ten seconds that mono means "this came from the cluster."
- **Density.** High, per Coolify's numbers. 13px body, 32px controls, 40/48px
  table rows, hairlines instead of borders, no card gallery. The whole matrix
  for a 6-component × 3-environment project fits above the fold.
- **Colour.** Near-monochrome. Status is carried first by **glyph and position**,
  second by hue, and saturated colour is reserved for the minority of cells that
  are not Live — so an all-green console is *quiet*, and one red cell is the only
  thing on screen. Single accent for interaction only (links, primary buttons,
  `fix:`). No gradients anywhere.
- **Signature interaction — the reconciliation ticker.** A single-line strip
  beneath the header, always present. When nothing is in flight it reads
  `in sync · 4m ago`. When a revision is moving it becomes a **segmented bar of
  the five phases** — Proposed · Committed · Reconciling · Applied · Healthy —
  with the current segment lit and the elapsed-in-segment time counting, driven
  by the existing `EventService.Watch` stream and `statemachine.State`. Segments
  that were *skipped* (the engine allows forward skips) render as passed-through
  rather than lit, which quietly teaches the model. The **only** use of the
  existing `kelson-pulse` animation. Stuck turns the segment amber and the strip
  gains one link: *"nothing has picked this up — check Flux."*

**Why it's kelson.** Vercel shows you a finished artifact; Heroku shows you a
list of past releases. Only kelson can show you a change *in transit through a
reconciler it does not control* — and the ticker makes the product's hardest
technical honesty (we publish, Flux applies) into its most reassuring feature.

### Direction B — "The Ledger"

*The product is an append-only record of what ran, and delivery is a movement
along it.*

- **Type.** Editorial contrast: a real text face at a genuinely large size for
  the one heading per page, mono for every revision id, and small caps for
  section eyebrows. Fewer, larger type steps than Direction A.
- **Density.** Medium, with generous vertical rhythm. Everything is a timeline
  or a row on one; horizontal chrome is minimal because the vertical axis *means
  time*.
- **Colour.** Paper-first — light is the primary theme here, dark is the
  supported alternate (an inversion of today's posture, and a real decision to
  weigh). Hairlines, no fills except status dots. The accent appears only on the
  live marker.
- **Signature interaction — the revision rail.** A persistent vertical rail on
  every project, environment and component page: one notch per revision, newest
  at top, with a filled marker on the one that is **live**. Hovering a notch
  previews what that revision changed (a two-line summary, expandable to the
  diff). **Dragging the live marker to an older notch is the rollback gesture** —
  it opens the irreversibility preview and requires the confirm; it never
  applies on drop. **Dragging a notch sideways from staging's rail into
  production's rail is the promotion gesture** — it opens the promote plan with
  that revision's images preselected. Both gestures have plain button
  equivalents; the rail is the discoverable face of them.

**Why it's kelson.** kelson's revisions are genuinely immutable OCI artifacts
with content-addressed tags. The rail is the only UI in the category that could
be literally true — Coolify and canine have no artifact to point at, and Heroku's
releases are opaque slugs. Risk: drag-to-rollback needs very careful confirm
design, and gestures are a discoverability tax.

### Direction C — "The Blueprint"

*The product knows the whole resolved graph, and draws it.*

- **Type.** Technical-neutral sans, tight tracking, one weight for structure and
  one for emphasis. Labels are small and set in small caps; values are large.
  Mono only inside the inspector.
- **Density.** Deliberately mixed: a large, calm canvas on the left, a **dense
  inspector panel** on the right (the Figma / Cloudflare posture). The canvas is
  low-density and the inspector is high-density, and nothing lives between them.
- **Colour.** **Structural encoding**: component *kind* is carried by **shape**
  (service = rounded rect with a port notch; worker = plain rect; cron = rect
  with a clock notch; postgres/valkey = cylinder; helm = hexagon) and **colour is
  reserved entirely for status.** This is the rule that makes a graph readable at
  a glance and it is the one thing most graph UIs get wrong.
- **Signature interaction — the live wiring diagram.** The environment page
  draws the resolved component graph: nodes are components, **edges are the
  things kelson actually resolves** — `from: {service: db, key: uri}` bindings,
  `{secret: …, key: …}` references, `source:` bindings from components to
  repositories, and domain edges out to hostnames. Clicking an edge opens the
  inspector on the resolved `secretKeyRef` and **which scope won the merge**.
  During a reconcile the edges of the moving components animate; **drift renders
  as a dashed node with its observed revision printed on it**, so "healthy, but
  on revision 44" is a picture rather than a sentence.

**Why it's kelson.** The binding graph is data kelson already computes and no
competitor models at all (canine has no bindings; Coolify has string env vars).
Risk: graph UIs are the classic over-build — they demo beautifully and get used
twice. Mitigate by making the graph a **tab on the environment page**, never the
default view, and by making the inspector the real product.

### Recommendation

**Ship A as the system, borrow B's rail as the signature interaction, keep C as
a later tab.**

Direction A is the correct *posture* for a self-hosted console — dense,
monochrome, quiet-when-healthy — and it is the fastest cure for the generated
look. Its ticker and B's rail are complements, not alternatives: the ticker is
*the present tense* (one revision, moving now) and the rail is *the past tense*
(every revision, addressable). Adopt the rail as a **click** interaction first
and only add the drag gestures once the confirm flows are proven. C's graph is
genuinely differentiated and genuinely expensive; put it behind the environment
page's tab bar in a later milestone and let the effective-config table (change
#4) deliver the same information in a cheaper form first.

---

## 7. Copy standard (the direct fix for "AI sloppy")

Adopt as a lint-able rule set; several of these can be enforced by a test over
the string literals in `ui/src/`.

1. **No internal citations in user-facing text.** No `ADR-nnnn`, no `#nnn`, no
   "rule P1/P3/P5", no `docs/*.md`, no Go package or file names. The *why* goes
   behind a "why" caret or a `kelson.dev/...` link. (Enforceable: grep the JSX
   string literals and `note=` props.)
2. **Field notes ≤ 10 words, no parentheses, no subordinate clauses.**
   Before: *"explicit FQDNs. A cluster with no Gateway API cannot serve them,
   and the check below says so rather than falling back to Ingress (#140)."*
   After: *"Full hostnames, one per line."* — with a DNS check beside it.
3. **Prefer a check over an explanation.** Anything the product can verify
   (DNS, reachability, capability, whether a name resolves) should be verified
   rather than described. Most long notes in the current UI are compensating for
   a check that doesn't exist.
4. **One sentence per empty state, and one action.** Never explain an absence in
   a paragraph; the three-cause preview-empty case is the honourable exception
   and should be **three named states with three fixes**, not three sentences.
5. **Numbers over adjectives.** "3 instances · 4 vCPU · 100 GiB" beats
   "a high-availability configuration suitable for production."
6. **Ban the house rhetorical tics in UI strings**: "X, not Y"; "never a Z";
   "which is why"; "the whole mechanism"; stacked em-dashes. They are good prose
   in `docs/` and they read as filler on a label.
7. **Keep `ErrorPanel` exactly as it is.** Code chip + message + `fix:` +
   docs link is the right shape, it is already consistent, and it should be the
   template for warnings and confirmations too.
8. **Say what a button does before it does it, in the button.** "Deploy revision
   48 to staging", "Pin staging's 3 images into production" — not "Confirm".

---

## 8. Open questions for the owner

1. **Light or dark as the primary theme?** Direction B implies flipping the
   default. Today dark is the shipped design and light was added on 2026-08-13
   [R `ui/README.md`]. Direction A works either way; B does not.
2. **Is `HistoryEntry` open for new fields?** Change #6 (the Deliveries feed) is
   badly constrained by outcome/digest/images travelling as free text inside
   `message`. Everything downstream is a parser until that's fixed.
3. **How much of `EditSpecPage` should survive?** The byte-identical rebuild
   guard is genuinely excellent design. But if change #4 (the effective-config
   table) lands, most day-to-day editing moves to targeted edits (set an env
   var, change replicas, pin an image) and the giant form's remaining job is
   "author a new document" — which `/projects/new` already does.
4. **Does the owner want the CLI and the UI to share a vocabulary?** If yes,
   change #2's relabelling (`synced` → `Live`) needs to reach `kelson status`
   and the deploy stream output too, or the two surfaces will disagree in front
   of the same user.
