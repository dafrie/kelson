# kelson-ui

The kelson web UI: TypeScript and React over Vite, talking to `kelson-server`
through generated ConnectRPC clients ([ADR-0002](../docs/adr/0002-tech-stack.md),
[ADR-0013](../docs/adr/0013-server-state-and-api-v0.md)). Part of milestone M6.

Node >= 20.19 (`engines` in `package.json` — the floor Vite 8 sets).
Dependencies are pinned to exact versions and `package-lock.json` is committed,
matching `website/`.

## Running it

The UI is a client for a server that binds loopback and holds all the state, so
start the server first:

```sh
go run ./cmd/kelson-server          # serves 127.0.0.1:8420 by default
```

Then, in `ui/`:

```sh
npm ci
npm run dev        # http://localhost:5173
```

Other scripts:

```sh
npm run typecheck  # tsc --noEmit; strict mode is the lint story (no ESLint in v0)
npm run test       # vitest; `npm run test -- --run` for a single pass
npm run build      # static output into dist/
npm run preview    # serve dist/ locally
```

## Signing in

A `kelson-server` started with `--password` requires a session; one started
without it — the default, and what `npm run dev` talks to unless you say
otherwise — requires nothing and the UI shows no login at all
([the server](../docs/server.md)).

`src/api/auth.tsx` is the whole of it. On boot it asks `GET /auth/session`,
whose three answers are three different facts:

| Answer | State | What the UI does |
| --- | --- | --- |
| `204 No Content` | authentication is disabled | nothing — today's behaviour, no login, no user chip |
| `200 {username}` | a session exists | renders the app and the username in the header |
| `401` | a login is required | redirects to `/login?next=…` |

A 401 from *any* RPC afterwards means the session went away mid-work — a server
restart mints a new signing key, so every open tab's cookie dies with the old
process. The transport interceptor in `src/api/clients.ts` reports it, the
provider flips to anonymous, and the login screen says the session expired and
returns to where the work was. Screens are untouched by any of this: they get
the same `unauthenticated` error they always would.

Components rendered outside the `AuthBoundary` see the context's default, which
is **disabled** rather than loading — that is what keeps the screen tests, and
any future embedding, from having to know authentication exists.

## The screens

Milestone M6 ([#7](https://github.com/dafrie/kelson/issues/7)). Every flow is
addressed by a project **and** an environment, because that pair is what the
API's mutating RPCs take — a `SpecRef` plus an environment name — so a link into
a deploy or a log tail is a link that keeps working. Two screens add a third
segment and no new RPC: a component's page is that same pair plus a name, read
out of what `GetSpec` and `Status` already answer, and an action is that pair
plus what is being done to it.

The pair is now a **screen** and not only an address
([#260](https://github.com/dafrie/kelson/issues/260)): `/projects/:project/:env`
is a layout with two tabs — Overview and the two views that used to be routes of
their own — and a bar of three actions. See "Flow consolidation" below for what
moved where, and for the redirects that keep the old paths working.

| Route | What it does | RPCs |
| --- | --- | --- |
| `/projects` | Every component in every environment, grouped by project, under a "needs attention" band that is absent when nothing does. Each environment keeps its own revision, counts and cause; each component row is a link to its page | `ListSpecs`, then one `DeployService.Status` per (project, environment) |
| `/projects/new` | Create a project and its first component: three fields, a rendered preview, then the store | `PutSpec` at `RENDER`, then with an idempotency key |
| `/projects/:project` | The component × environment matrix and the stored documents. A column header is the link into that environment | `GetSpec`, one `Status` per environment |
| `/projects/:project/:env` | **Overview**, the environment whole: status, workload verdicts, data services, its PR previews and Secrets. The index tab of the layout that carries Logs, History and the three actions | `GetSpec` (the layout's), `Status`, `Watch`, `GetProfile`, `ListPreviews`, `ListSecrets`, `Render` (deferred presets only), `SetSecret`/`DeleteSecret` on use |
| `/projects/:project/:env/components/:component` | One component in one environment: its shape, the image its documents resolve to and the scope that set it, the source it builds from, the environment's revision and namespace, the effective-config table, its own workload verdict — and links into its environment's tabs and actions, with itself preselected in the logs | `GetSpec`, `Status`, `GetEffectiveConfig` |
| `/projects/:project/edit` | Edit the stored spec: a form tab and a raw YAML tab, a diff before saving, an optimistic-concurrency save. The form reaches `spec.previews` (ADR-0017; the retired `delivery:` stanza is gone per ADR-0028), and appends a component to `spec.components` (`?add=component` opens on it) | `GetSpec`, `PutSpec` at `RENDER` then for real, `Diff` |
| `/projects/:project/edit`, git-owned | The same screen when `GetSpec` reports a Flux Kustomization owns these documents (#248): `GitOpsBanner` names the owner and the `autoDeploy` collision, Save is replaced by `ExportPanel` (the documents, copyable and downloadable, no server call) and `ProposePanel` (the same bytes as a pull request through a connection that can open one) — `src/pages/GitOpsPanel.tsx` | `GetSpec`, `Diff`, `ProposeSpec`, `ListConnections` |
| `/projects/:project/:env/actions/deploy` | **Action.** Preview (render dry-run) then a confirm that streams the deployment live. Its step one is the server-side comparison, which is why there is no diff screen | `Deploy` at `RENDER`, then at `NONE`; optional `Diff` at `SERVER` |
| `/projects/:project/:env/actions/promote` | **Action.** The environment in the path is the **target**: pick a source, read the plan and the diff it produces, then write the pins. It never deploys | `GetSpec`, `Promote` at `RENDER` then `NONE` |
| `/projects/:project/:env/actions/rollback` | **Action.** Revision picker, irreversibility preview, then the apply. `?to=<revision>` preselects and previews a target, never applies it | `History`, `Rollback` at `RENDER` then `NONE` |
| `/projects/:project/:env/history` | **Tab.** The revision rail: the recorded revisions newest first, one stop each, a single marker on the live one, and a faded tail where the record thins out into the registry. A stop opens on a click and offers the comparison and the rollback; the head above the newest offers the promotion. `?from=<revision>` opens the comparison against that revision in place; `?compare=1` opens it against the live cluster | `History`, `Status`, then `Diff` when the comparison is open |
| `/projects/:project/:env/logs` | **Tab.** Bounded Query, and a live tail that pauses, filters, reconnects and saves. `?component=<name>` opens on one component — the link a component's own page carries | `QueryLogs`, `FollowLogs`, `Status` for the namespace |
| `/projects/:project/:env/previews/:pr` | One change request's preview: phase, hosts, the pinned commit and applied revision, the two Ready conditions, when it appeared — everything `Preview` reports and nothing it doesn't. `:pr` is the change-request number, the identifier a human types (ADR-0017); there is no `GetPreview`, so the page reads the same `ListPreviews` the environment Overview's Previews section does and picks out the matching row, honestly reporting when none matches. The route a commit status and a PR comment link to (ADR-0017 stage 3, #248) | `ListPreviews` |
| `/cluster` | Server build, the node inventory (count, readiness, CPU/memory usage where metrics.k8s.io answers), the platform-component checklist with its install flow, and the detected ClusterProfile | `/healthz`, `GetProfile`, `GetNodes`, `ListComponents`, `PlanInstall`, `Install` |
| `/connections` | The forges this instance can pull from: provider, host, account, owner, health and the reported repository count per connection, a live probe and a delete that names the projects it breaks, plus the token-connection form | `ListConnections`, `CreateConnection`, `TestConnection`, `DeleteConnection` |
| `/setup` | The onboarding screen: the same component checklist framed for a first run — what is present, what is missing, an install flow per missing row, and where to go next | `ListComponents`, `PlanInstall`, `Install` |

## The component × environment matrix

The project page and the home page draw the same object
([#260](https://github.com/dafrie/kelson/issues/260)): a **component in an
environment**. The component is what deploys — components carry their own
spec-hash and an unchanged one produces no rollout (docs/model.md §6) — and the
environment is what gives it a namespace, an image pin and a phase. Neither
alone has a status, so the thing with one is the pair. The project page draws
every pair at once, components down and environments across; home draws the same
rows grouped by project. `src/pages/matrix.ts` is the shared logic and is tested
as logic.

Five things it is deliberate about:

- **A cell says which answer it is standing on.** `Status` reports per-component
  health only for what observation probes, which is Deployments
  (`internal/api`'s `observeWorkloads`). A `cron`, a chart and a database have
  no reading of their own, so their cells show the *environment's* word with a
  small `env` mark and a legend under the table. Borrowing silently would be the
  quiet lie the status vocabulary exists to prevent.
- **A component overrules its environment downward, never upward.** A degraded
  workload inside a Healthy environment reads `unhealthy`; a healthy workload
  inside a reconciling environment reads `deploying`, because the delivery
  answer is the wider claim. `WorkloadVerdict.stuck` is the fourth case and it
  reads `stuck`: the probe gave up waiting, `code` stays a *wait* code such as
  `workload/progressing`, and before that field existed "gave up waiting" and
  "still starting" were one word here. A verdict with all three flags false is
  the latter and is `deploying`, never a failure.
- **The revision is the environment's and the image is the component's.** One
  publish carries every component (ADR-0028), so the revision is stated once per
  column; the image differs per cell and is rule P3's winner across the three
  documents — *configuration*, not an observation, and labelled with the scope
  that set it on the component's own page.
- **Home costs what it always cost.** `ListSpecs` omits the stored documents, so
  a per-project component list would have meant a `GetSpec` per project. The
  rows come out of the verdicts each `Status` already carries instead. The floor
  that buys is real and stated on screen: an environment reporting no
  per-component verdicts says "no per-component readings here" and keeps its own
  status, which is what the whole page used to show.
- **The attention band is absent when it is empty.** It carries `unhealthy`,
  `stuck`, `failed` and `unknown` — not `deploying` or `waiting`, because a band
  that fills up during every deploy is a band people stop reading, and not
  `suspended`, because nothing is trying on purpose. `unknown` is in it: "we
  could not tell" is exactly the state somebody has to go and look at. **Drift
  is not in it and cannot be**: the band is keyed on the word, and `stale` never
  becomes one — a stale environment that is `live` stays out, and a stale one
  that is `stuck` is already in on the strength of `stuck`.

The project page issues one `Status` per environment — one per column — and that
is now all it issues. The environment's own panel used to sit below the grid,
selected by a strip of buttons, and reused the column it was already showing;
it is the Overview tab of the environment's route instead, so the environment a
reader is looking at is in the URL and the panel reads its own single `Status`.
A column header is the link into it.

## The effective-config table

`src/config/` is the component page's answer to *what is this actually running
with here, and which file put that there*
([#260](https://github.com/dafrie/kelson/issues/260)). Every environment
variable, image, replica count, resource quantity, hostname and preset a
component runs with is the winner of a two- or three-level merge across two
documents, and until this landed the only way to read it was to open both and
merge them in your head.

**The merge is not done here, and must never be.** `GetSpec` returns the
authored documents and `Render` returns the finished manifests; neither answers
the question, so `SpecService.GetEffectiveConfig` was added to. It carries the
winning value *and* the block that set it, computed in `internal/model` beside
the resolver and tested for agreement with it value-for-value on the fixture
that exercises every precedence rule at once. A copy of that merge in
TypeScript would drift, silently, in exactly the direction that makes a
provenance claim wrong — `src/config/effective.ts` therefore reads the answer
and computes nothing.

Five things the table is deliberate about:

- **A setting the answer did not mention is absent, not blank.** A component
  awaiting its first build has no `image` row, because nothing in either
  document names one — the resolver's own placeholder for that state is a
  sentinel that must not reach a manifest, and it must not reach a table of
  what runs either. A row with an em dash in it would claim the question was
  asked and answered.
- **A secret stays a reference.** The wire's value is a union whose two mapping
  arms carry a Secret's name and a key, and no message in the schema has a
  field a value could arrive in — kelson never reads the Secret. The row prints
  `{ secret: checkout-db, key: url }` through the same `envValueText` the spec
  builders write, so what a reader sees is what the editor would have produced.
- **The provenance is a sentence, not a code.** Five answers — "kelson's
  default", "set on the project", "set on the component", "set on production",
  "set on production, for this component" — said as facts about two files
  rather than as the rule numbers that govern them. The two environment answers
  are parallel to the two project ones so a reader learns the shape once, and a
  level this build cannot read says "not stated" rather than falling back to
  something plausible.
- **A row nobody wrote recedes.** kelson's own defaults are dimmed, because the
  rows a reader is looking for are the ones a file put there — and dimming is
  what makes them findable without colouring anything.
- **Rows are separated on the group, never on the name.** An environment
  variable is named by its author and `resources.requests.cpu` by the model, so
  a project is free to declare a variable called `image` and it is a different
  row from the workload setting. The wire says which group each row is in.

The JSONPath into the document — `$.spec.components[0].env.LOG_LEVEL`, the same
spelling a structured error's `field` uses — rides on the row's `title` rather
than taking a column, and the environment's own settings (its secret backend,
its agent policy) are the table's third group, because one of them changes what
a row above *means*: a `{secret, key}` reference is served by whichever backend
this environment names.

## Flow consolidation

Six flows used to hang off `(project, environment)` as sibling routes —
`deploy`, `diff`, `history`, `logs`, `rollback`, `promote` — and a reader had to
know which of the six answered the question they had. They were never six
siblings ([#260](https://github.com/dafrie/kelson/issues/260)), so they are no
longer routed as six:

| Was | Is | Why |
| --- | --- | --- |
| `/…/:env/logs` | **Tab**, same path | A view of the environment. Two views are siblings; a view and an action are not. |
| `/…/:env/history` | **Tab**, same path | The same. |
| `/…/:env/deploy` | **Action**, `/…/:env/actions/deploy` | Something done *to* the environment: entered from its bar, finished by returning to it. |
| `/…/:env/promote` | **Action**, `/…/:env/actions/promote` | The same. The environment in the path is still the target. |
| `/…/:env/rollback` | **Action**, `/…/:env/actions/rollback` | The same. A history row carries its revision in `?to=`. |
| `/…/:env/diff` | **Panel**, no route | Not a place. It is step one of the deploy, the editor's guard before a save, and what `?from=<revision>` opens on the History tab. |

`src/pages/flows.ts` holds the mapping as a pure function and
`src/pages/FlowRedirect.tsx` mounts it, so all four moved paths still resolve and
every query parameter travels with them: `?component=`, `?to=`, `?image=`, and
`?from=`, which becomes the History tab's open comparison. A `/diff` with no
revision becomes `?compare=1`, the mode that screen opened on. This is what keeps
the promise above — a link into a deploy is a link that keeps working — for the
URLs already sitting in commit statuses, pull-request comments and the docs.
`internal/api`'s auto-deploy commit status points at
`projects/:project/:env/history`, which is one of the two that did not move.

Three things it is deliberate about:

- **The tabs' paths did not change.** A consolidation that renamed every URL
  would have been a consolidation that broke every link. Logs and History became
  tabs by being mounted under a layout, not by being moved.
- **The layout reads the spec; the tabs read the cluster.** `EnvironmentPage`
  issues one `GetSpec` — it is what says this environment exists and what its
  siblings are, which is what the promote action needs — and hands the stored
  documents down, along with the sibling list itself: the logs tab reads the
  components out of the documents and the History tab reads the siblings to
  decide whether its rail has a head. Every cluster-touching call still belongs
  to the tab that wants it: the logs tab opens no `Status` for a phase rail it
  does not draw, and Overview reads the one `Status` the panel always read.
- **A tab is a link, an action is a link, and the log screen's two modes are
  buttons.** They look the same and they are not the same: the first two are
  routes worth pasting, the third is one screen with a switch on it.

The connections screen ([ADR-0033](../docs/adr/0033-git-connections.md),
[#248](https://github.com/dafrie/kelson/issues/248)) holds the one entry point
in this UI that is **not** an RPC. "Connect GitHub" runs GitHub's app-manifest
flow, whose credential is minted by GitHub and handed to the *server* — which is
why `CreateConnection` has no app variant. The button POSTs
`/forge/github/manifest/session` with the session's credential, then navigates
to the single-use `startUrl` the server answers with; the callback lands back on
`/connections` with `connected`/`install`/`error` query parameters the page
renders once and clears. The dev proxy in `vite.config.ts` forwards `/forge/`
alongside the RPC prefix, so the whole round trip works under `npm run dev`.

Ownership is displayed and enforced by nothing: ADR-0033 decision 6 fixes the
semantics now and gives them a subject when tenancy does (#231), so the screen
shows the recorded owner and states in words that every connection is visible
to — and deletable by — everyone who can reach this server. Health is read as
three states rather than two, because `ready` and `reachable` are both false
before the first probe as well as after a failed one and `message` is all that
separates them.

The history screen ([#67](https://github.com/dafrie/kelson/issues/67)) is bounded
by what `DeployService.History` actually returns, which is nine fields per
revision — `revision`, `spec_hash`, `committed_at`, `message`, `author`,
`digest`, `images`, `outcome` and `beyond_window` — and nothing else. Under the
rebuilt delivery spine ([ADR-0028](../docs/adr/0028-delivery-spine.md),
R2 [#225](https://github.com/dafrie/kelson/issues/225)), a revision id is
`<generation>-<hash8>`, the OCI artifact tag the controller published — never a
git commit sha, because there is no git writer left to commit one. The outcome,
the digest and the images used to travel as prose inside `message` because
`HistoryEntry` had no field for any of them; they have fields now, the screen
reads the fields, and `message` is not rendered at all. Every row is a publish:
a rollback repoints Flux at a revision that is already here and prepends no
history entry of its own. Two consequences are still visible on the screen
rather than hidden by it:

- **A recorded outcome is a snapshot, not a live answer.** It is what was true
  when that revision stopped being the current one, and nothing refreshes it
  afterwards, so it is labelled "recorded" in the muted meta line. The phase
  pill appears on exactly one row — the revision `Status` reports as live, the
  only one anything can currently answer for *right now* — and no other row
  carries a health claim.
- **No human-vs-agent attribution.** The spine records who deployed nothing yet,
  human or agent, so every entry reads "unattributed" rather than being
  attributed to anybody. Agent identity is [#74](https://github.com/dafrie/kelson/issues/74).

There is also no commit or pull-request link: a revision is an OCI artifact in a
registry, not a commit in a repository, so there is no forge to point at.

The same absence rules out an A-against-B revision diff: `RenderService.Diff`
compares the *current* spec against one recorded revision (`from_revision`) and
offers no A-vs-B call, and `HistoryEntry` carries no rendered manifests to do it
client-side either. So the per-revision action is named for what it does —
compare against what is deployed now — and opens the shared comparison panel
(`src/diff/ComparePanel.tsx`) in place, under `?from=<revision>`, rather than
growing a second copy of it. Rollback stays a link out to its action, because the
irreversibility preview must not be duplicated into a screen that might skip it.

## Promotion, which never deploys

The promote screen ([#11](https://github.com/dafrie/kelson/issues/11),
[ADR-0016](../docs/adr/0016-delivery-flows-v0.md) decision 2) is addressed by the
environment it writes **into**. That is the direction a reader arrives with: they
are looking at production and want what staging is running, so the screen is
named from production's side — "promote into this environment" — and the source
is the thing it asks for. It says it that way on the one bar that offers it: the
environment's action bar, beside Deploy and Rollback, on every one of its tabs.

What it refuses to do, in the order a reader meets the refusals:

- **It never deploys.** Promoting is editing one field (docs/model.md's
  Promotion section: the pin *is* the promotion), so a successful promotion
  leaves the target running exactly what it was running a moment earlier, with a
  new pin in the store. The success state says so and offers the deploy action
  as the next, separate act; it does not deploy on the reader's behalf, because
  that would merge the two acts ADR-0016 keeps apart.
- **It never plans and writes in one click.** Picking a source runs
  `Promote` at `dry_run=RENDER` — every pin computed, nothing stored — and the
  confirm re-sends the same promotion at `NONE`. There is no path from the
  picker to a written pin that does not pass through the table, and a plan that
  would pin nothing gets no confirm button at all, the way `kelson promote`
  stops with "nothing to write".
- **It never hides a component it did nothing to.** The response carries every
  component the promotion considered, pinned, unchanged and skipped alike
  (`internal/api`'s `wirePromoted`), and the table shows all of them with the
  server's own `promote/*` code and prose under each skip. A component missing
  from the table would be indistinguishable from one kelson forgot.
- **It never retries a stale plan.** A `store/version-conflict` means someone
  stored the spec between the plan and the confirm, so the offer is to *plan
  again*, not to retry and not to force. The pins on screen were computed from
  documents that no longer exist; unlike a half-typed edit, a promotion can be
  recomputed exactly, so it is.

The diff comes back on the promotion's own response and goes through the same
decoder and the same `DiffView` every other preview uses — the server computes
it because a dry run stores nothing, so there is no "after" for a follow-up
`Diff` to compare against. Image references are digests, so the table elides
their middles (`src/pages/promote.ts`) and keeps the whole value on the tooltip
and on the clipboard: the head and tail are what a reader compares, and a digest
is exactly the value that must not be retyped from a screenshot.

Five things the screens are deliberate about:

- **A status that could not be read is never rendered as green.** Each card's
  `Status` call is its own, so one unreachable cluster degrades one card to
  "status unavailable" with the server's structured reason on it, instead of
  blocking or blanking the grid.
- **Streams render incrementally and abort on navigation.** `Deploy`, `Rollback`
  and `FollowLogs` are consumed with `for await`, each event painted as it
  lands. There is no client-side timeout — the server owns the budget and
  answers by *sending* a settled event — and `src/api/stream.ts` aborts the
  `AbortController` on unmount.
- **A deployment that settles unhealthy is an outcome, not a broken
  connection.** The stream completes cleanly with the error on the `Settled`
  event (`statemachine.Run`'s contract), and the screen renders it as the
  deploy's answer.
- **One error component.** `ErrorPanel` renders `kelson.v1alpha1.Error` —
  decoded from ConnectRPC error details with `findDetails(ErrorSchema)`, or
  taken from the inline `errors` fields — as a mono code chip, the message, the
  remediation as a `fix:` line and `docs_url` as a link. Codes are never
  re-mapped; `delivery/unsupported` reaches the screen as the string the owning
  Go package defines.
- **Creating a project asks three questions.** `/projects/new` shows a name, an
  image and a port, and nothing else; an empty port is a worker, because the
  model derives the workload kind from the shape rather than asking for a type.
  Every other field the model can express sits behind one "More options"
  disclosure — progressive disclosure is the whole design (docs/model.md's own
  target shape), and the named failure mode is a first screen that asks forty
  questions to deploy one container.

## The live log tail

`src/logs/` is the machinery behind Follow mode ([#64](https://github.com/dafrie/kelson/issues/64)),
kept out of the component so the parts that are only logic can be tested as
logic. Five decisions:

- **The tail is a capped window, and it says so.** `LogBuffer` retains ten
  thousand lines in constant memory; past that the oldest are evicted and the
  count of what left is on screen, because a view that silently forgets is a
  view that lies. A second, smaller cap bounds what is in the *DOM* — ten
  thousand log rows is a layout cost no amount of batching makes free, and
  "remains responsive at high volume" is the issue's acceptance criterion. Copy,
  download and the filter read the buffer, not the DOM.
- **Lines are batched into frames.** Every event lands in a queue that one
  `requestAnimationFrame` drains, so a thousand lines a second is still one
  re-render per frame. The buffers are refs; a version counter is what tells
  React something changed. Making them state would copy the whole window on
  every batch, which is the cost the cap exists to avoid.
- **Pause buffers. It does not drop, and it does not close the stream.**
  Closing would make "pause" mean "lose whatever happens while you read the line
  you paused for"; dropping would mean the same thing while looking like it
  didn't. The arriving lines go to a second capped buffer and the pill says how
  many are waiting.
- **A dropped stream reconnects and fills its own gap.** The screen remembers
  the newest timestamp it saw, backfills with a bounded `QueryLogs` from that
  instant, then re-follows from it, with capped exponential backoff. Both ends
  replay the boundary instant — `since` is inclusive, and excluding it would
  lose every line sharing the last millisecond — so `GapGate` suppresses the
  overlap as a multiset of the keys already retained. A `LogLine` has no ID, so
  the key is the whole of it (timestamp, pod, container, message) and the two
  honest limits are written down where it is defined. Only the answers a second
  identical request cannot change (`Unimplemented`, `InvalidArgument`, a missing
  namespace, an expired session) stop the loop; those become the error panel
  instead of a permanent "reconnecting…".
- **The find box is client-side, unlike Query's.** A `LogMatch` sent upstream
  restarts the stream and discards non-matching lines permanently, so clearing
  the box later shows a gap rather than the lines that were always there.
  Filtering the retained buffer is instant, reversible, and applies to what has
  already arrived. It is therefore a find-in-page and case-insensitive; the
  case-sensitive contract is the server's, and bounded Query still sends it.

Pod attribution comes from `LogLine.pod`, which the merged stream fills in per
replica. The label's colour is hashed from the pod name into eight tokens
(`--kelson-pod-1` … `-8`, both themes) so a replica keeps its colour between
glances, and the name is always printed beside it — eight tones over an
arbitrary number of pods collide, and a colour alone would then be a lie.

## Data services

`src/dataservices/` is a section of the environment's Overview tab
([#107](https://github.com/dafrie/kelson/issues/107)): a `kind: postgres`
component is not a workload, so it is not listed among them. Four rules shape
it, and each is there because the alternative would mislead:

- **The preset is spelled out in numbers.** `src/dataservices/presets.ts`
  mirrors `dedicatedPresets` in `internal/renderer/dataservice.go` — instances,
  CPU, memory, storage, synchronous replicas — and it is the *only* copy of
  that table in the UI. The browser cannot derive those numbers; it quotes
  them, and the file says so. Change the Go table and this one is wrong until
  it is changed with it.
- **Health is reused, never invented.** The section reads the same
  `StatusResponse.verdicts` the workload list does and looks for one about the
  component's `Cluster/<namespace>/<project>-<environment>-<component>`.
  Observation probes Deployments today (`internal/observation/probe.go`), so
  usually there is none — and the section says there is none. A database
  nothing watches must not read as a healthy one.
- **A refusal comes from the server.** `preset: shared` is deferred
  ([#93](https://github.com/dafrie/kelson/issues/93)) and `preset: branch` is
  not implemented ([#99](https://github.com/dafrie/kelson/issues/99)). When the
  spec names one, the section calls `RenderService.Render` — the offline rung,
  no profile, no cluster — and renders the structured
  `render/service-not-implemented` error it answers with, code and remediation
  intact, through the same `ErrorPanel` as everything else.
- **Coming soon looks deliberate.** Backups
  ([#94](https://github.com/dafrie/kelson/issues/94)) and branching are muted
  badges with one sentence and a link to the issue, never disabled buttons.

`src/secrets/SecretsPanel.tsx` sits beside them
([#116](https://github.com/dafrie/kelson/issues/116)): the Secrets kelson
manages in this environment's namespace — what a `{secret: <name>, key: <key>}`
reference points at — listed by name, keys and age, with an inline form that
writes one. Four things it does not do, all of them the schema's decision rather
than the screen's: it never shows a value (no message in `secret.proto` has a
field one could arrive in), it never claims a write replaced a Secret (`SetSecret`
merges, and the response says which keys were kept), it never adopts a Secret
kelson did not label (`secret/not-managed` reaches the reader through the same
`ErrorPanel` as every other structured refusal), and it offers no dry-run rung —
the form submit is the confirm, because the person pressing it is looking at the
form. A delete still confirms, because what it destroys is not on screen to be
retyped. After a write it prints the ready-to-paste reference per key, from the
same `secretReference` the spec builders use, so what is pasted is what the
editor would have written.

`CapabilityPanel` sits under them and answers the question a database raises
about the cluster: does the storage have a snapshot driver, what does a clone
cost, and how confidently was that decided. It reads `GetProfile`'s YAML —
the profile travels as its canonical document, not as proto fields — and phrases
the answer at the point of use: *"Fast branching unavailable — your storage
class (local-path) has no snapshot driver."* The confidence is shown rather than
hidden, because `unknown` is not `none` and a reader told "we could not tell"
can go and look. It states two operator findings in the same shape for the same
reason — CloudNativePG, without which a rendered `Cluster` is just a manifest,
and Flux's helm-controller, without which a rendered `HelmRelease` installs
nothing ([ADR-0016](../docs/adr/0016-delivery-flows-v0.md)).

Three parsers (`parse.ts` for the spec's data components, `capability.ts` for
the profile, `src/spec/components.ts` for the matrix's rows — their kinds, their
images, and the source each binds to) sit
on `miniyaml.ts`, which reads block mappings, block sequences and one-line flow
mappings and nothing else. They are separate from `src/spec/edit.ts`'s parser on
purpose: that one feeds a form whose honesty rests on a byte-identical rebuild,
so a key it cannot write is a key it must not read, and a document outside its
grammar has no form at all. These only *describe* a document — a hand-written
Project is still a Project whose components a reader is entitled to see listed —
and a document they cannot follow yields nothing rather than something wrong.

## Building spec documents in the browser

`src/spec/documents.ts` writes the Project and Environment documents
`/projects/new` stores. Three things about it are load-bearing:

- **It emits only what was filled in.** The store keeps the authored bytes
  (ADR-0013 §1) — this is the file the user now owns, so a builder that wrote
  `health: ""` because the schema has the field would be handing them something
  to prune. The output is the shape of `examples/hello-single`.
- **It is a string builder, and `yamlScalar` is the whole risk.** The document
  is a fixed skeleton with scalars poured into it, so the only hard part is
  quoting, and quoting is where guessing wrong is *silent*: `PORT: 3000`
  decodes as an integer and `model.EnvValue` refuses it, and `DEBUG: on`
  resolves to a boolean. The rule is an allow-list of characters that can only
  be a string, with `JSON.stringify` as the escape function (a JSON string
  literal is a valid YAML double-quoted scalar), and `documents.test.ts` pins
  the cases.
- **It owns the map back from JSONPaths to inputs.** `PutSpec` at `RENDER`
  answers with `kelson.v1alpha1.Error`s carrying `field` paths, and the builder
  is what decided the port lands at `$.spec.components[0].port`, so
  `fieldForError` lives beside it. `resource` separates the two documents,
  which share paths. Anything unrecognised goes to `ErrorPanel` whole — a rule
  the form does not model still has to reach the reader with its code, its
  remediation and its line number.

The agreement with Go is a fixture kept on both sides: the minimal three-field
document pair in `src/spec/documents.test.ts` is byte-identical to the one in
`internal/api/uispec_test.go`, where the real model validates and renders it
through `PutSpec` at `RENDER`. Neither side can prove the other's half; changing
the builder without changing both fails the Go test. `src/spec/edit.ts` keeps a
second pair under the same rule (`EDITED_PROJECT` /`uiEditedProjectDoc`), for the
richer document the editor can write.

## Editing a stored spec

`/projects/:project/edit` (#65) is the other half: `documents.ts` writes a document
nobody has an opinion about yet, and `src/spec/edit.ts` reads one back that
somebody might. Those are different problems, because the store is byte-faithful
and the spec is the user's file.

Three tabs' worth of design sit on one decision — **the form is offered only for
a document the UI can rebuild byte-identically**:

1. Parse the stored document into edit state.
2. Rebuild a document from that state.
3. Byte-identical? The UI wrote this file and nobody has hand-edited it, so the
   form may edit it by rebuilding, and nothing can be lost.
4. Otherwise the form is read-only with a notice, and the YAML tab — which is
   always present, always complete, and opens first in this case — is where that
   document gets edited.

Step 3 is a **total** guard, not a heuristic, and that is what makes so simple a
strategy honest: anything the parser fails to capture — a comment, a key order,
a `preset:`, an anchor — is missing from the rebuild and shows up as a
byte difference. There is no path where the module drops something *and* still
claims the document is editable. The reader is never asked to trust the parser;
they are shown its output compared against their own bytes.

**Adding a component goes through the same guard**
([#214](https://github.com/dafrie/kelson/issues/214)). A Project is a container
of components (ADR-0014) and the UI could edit the ones a document already had
but never add one, so a second component meant hand-writing YAML nothing
advertised. `appendComponent` in `src/spec/edit.ts` is the whole mechanism, and
its contract is an equality rather than a promise: the rebuilt Project document
must be **the stored bytes, a blank line, and the new entry** — asserted in the
function and pinned in `edit.test.ts`, so "the original file is untouched" is a
checked fact and not a property of how the builder happens to order its output
today. When the stored document is not rebuildable there is no append at all:
the panel says so and hands over the exact block to paste into the YAML tab,
which is the same trade the read-only form makes.

The kinds it offers are the ones it can write whole: `service`, `worker` and
`cron` asked as the shape questions the model derives them from (a port, a
schedule, neither), plus `postgres` and `valkey`, which are a name and a
`kind:`. `helm` and `agent` are named on the panel rather than shown disabled —
a chart is a `chart:` plus `values:` these fields do not have, and an agent's
one distinguishing field is rejected until M7 ([#75](https://github.com/dafrie/kelson/issues/75)).

That data pair is the one place the parser's vocabulary grew with the builder's:
both learned `kind: postgres` / `kind: valkey` and nothing else, so a project
that gains a database stays rebuildable, while a `preset:` — or any other key a
data, agent or chart component can carry — still sends the document to the YAML
tab whole. The form *states* a data component instead of editing it: it has no
image to roll and no replicas to scale, and its preset is the data-services
section's subject ([#107](https://github.com/dafrie/kelson/issues/107)).

The parser is small and strict on purpose. It reads the grammar the builders
emit — two-space indentation, block mappings and sequences, flow mappings for
`replicas` and the env reference forms, plain and double-quoted scalars — and
gives up on everything else. It is not a YAML implementation and must not become
one: a document it cannot read costs the reader the form tab, which is the right
outcome.

**Env values are a union of three, and the guard is what makes that safe.** A
scalar is a value, `{secret, key}` is a reference to a Secret in the
environment's namespace, and `{from: {service, key}}` is a binding to a data
component ([ADR-0018](../docs/adr/0018-secret-references.md)). The form shows all
three as themselves — a form picker plus the right sub-fields, and no value input
on either reference, because the spec carries references and never credentials.
What it *writes* is the single-line flow styling this builder emits
(`DATABASE_URL: { secret: checkout-db, key: url }`), which is what ADR-0018,
docs/model.md and `kelson secret set` all show. A reference authored as a block
mapping, or with its keys the other way round, parses and displays correctly and
then fails the byte guard, so it opens on the YAML tab with its formatting
intact. A flow mapping with different spacing, a quoted inner scalar, or a
mapping that is neither reference form is refused outright — including a plain
value whose text begins with `{`, which is the one deliberate refusal in the
reader: a mapping it could not decode must never reach the form as a *string*
that looks like one.

Two more things the flow is deliberate about:

- **A diff before every save, for every environment.** `RenderService.Diff` with
  `from` = the currently stored documents and the edited ones supplied inline
  says what the change actually does. Every environment gets its own diff rather
  than the selected one, because a Project edit reaches all of them and a
  destructive change hidden behind an unselected tab is exactly what the preview
  exists to prevent.
- **A version conflict is a state, not an error panel.** The save carries the
  `version` from `GetSpec`, so a spec someone else wrote in the meantime is
  refused (`store/version-conflict`) rather than overwritten. No merge is
  offered — reapplying form edits onto bytes that moved underneath them is a
  three-way merge, and a wrong one produces a document nobody wrote. The two
  honest actions are: reload (fresh bytes, edits discarded and said to be) and
  force (labelled destructive, `force=true`). The edited text is one click from
  the clipboard in both, because "discarded" must never mean "gone".

Unsaved changes are guarded twice, because one guard cannot see both exits:
`useBlocker` catches client-side navigation and `beforeunload` catches a reload
or a closed tab. `useBlocker` exists only under a data router, which is why
`src/App.tsx` exports routes and `main.tsx` mounts them with
`createBrowserRouter`; nothing else in the app uses a loader or an action.

`src/diff/parse.ts` decodes `diff_json` against the Go types in
`internal/diff/diff.go`. Its fixture, `src/diff/testdata/server-diff.json`, is
real `diff.EncodeJSON` output captured from `internal/diff/format_test.go` —
which is what makes the test an agreement with Go rather than with itself.

## Why there is a dev proxy and no CORS

`kelson-server` serves ConnectRPC, `/auth/*` and `/healthz` on one mux with no
CORS middleware. It assumes the UI is served from the same origin it is, which in
production is true: the built assets ship behind the same listener. Same-origin
is also what makes the session cookie work at all — it is `SameSite=Lax`, so a
cross-site call would not carry it ([the server](../docs/server.md)).

In development Vite serves on `:5173` and the API is on `:8420`, which would be
cross-origin. Rather than add CORS handling to the Go server for the benefit of
development only, `vite.config.ts` proxies to it:

| Path | Forwarded to |
| --- | --- |
| `/kelson.v1alpha1.*` | `http://127.0.0.1:8420` |
| `/auth/*` | `http://127.0.0.1:8420` |
| `/healthz` | `http://127.0.0.1:8420` |

Connect RPC paths are `/<package>.<Service>/<Method>`, so the one
`/kelson.v1alpha1.` prefix covers `SpecService`, `RenderService`,
`ProfileService`, `DeployService` and `LogService` — and any service added
later — without touching the config. The transport in `src/api/clients.ts`
therefore uses `baseUrl: "/"` in both environments; there is no API host to
configure.

`DeployService.Deploy` and `LogService.FollowLogs` are server-streaming. The
proxy passes bytes through as they arrive (http-proxy's default), and nothing in
the config may start buffering responses without breaking live logs.

## Regenerating the clients

`src/gen/` is generated from `proto/` and committed, exactly like
`internal/api/gen`. Regenerate both together from the repository root:

```sh
cd ui && npm ci     # buf.gen.yaml invokes ui/node_modules/.bin/protoc-gen-es
cd .. && make proto
```

Connect-ES v2 needs only `protoc-gen-es`: `createClient` consumes the service
descriptors it emits, so there is no `protoc-gen-connect-es` counterpart to the
Go plugin. `src/api/clients.test.ts` builds a client per service
against an in-memory `createRouterTransport` — it fails the moment the generated
schemas and the client wiring stop agreeing, which is the check that catches a
`src/gen/` left stale by a `.proto` change.

## Design

The tokens in `src/styles/tokens.css` are the palette recorded in
[`docs/design/assets/README.md`](../docs/design/assets/README.md), named to match
`website/src/css/custom.css` so the docs site and the app share one vocabulary.
The shell follows the structure of `docs/design/Kelson Dashboard.dc.html`.

Those mockups are **reference, not specification**
([docs/design/README.md](../docs/design/README.md)), and this scaffold diverges
from them where they describe things that do not exist: the nav ships only the
destinations that are real, there is no environment selector and no user avatar,
and the fonts are self-hosted via `@fontsource` rather than pulled from the
Google Fonts CDN the mockups use.

One animation exists: `kelson-pulse`, reserved for reconciling status dots. If
something else starts pulsing, the signal stops meaning "work is in flight". It
honours `prefers-reduced-motion` in both themes.

### The Console

The visual system, shipped for
[#260](https://github.com/dafrie/kelson/issues/260) from direction A of
[`docs/research/ux-overhaul.md`](../docs/research/ux-overhaul.md) §6. Four rules,
and every one of them is enforceable somewhere:

**1. Monochrome surface; colour is status.** Backgrounds, borders, headings, nav
and controls are neutral. The six status ramps in `tokens.css` are the only
saturated colour on any screen. The consequence people notice first is that
`--kelson-accent` is *not the brand green any more*: it is the page's own ink
(`#e6ebf0` dark, `#1a2026` light), because green is what `live` is painted in and
one hue cannot mean both "healthy" and "clickable". A link is the ink with a
1px underline in `--kelson-line`, and the underline — not the colour — is what
changes on hover. The primary button is a solid neutral fill. `--kelson-green`
is untouched: it is the mark's colour and the mark is not chrome.
`tokens.test.ts` asserts the accent equals no tone and stays channel-neutral.

**2. Mono means the machine produced this exact string.** Revisions, digests,
image references, namespaces, hosts, pod and resource names, refs, structured
codes, kinds, presets, phases, byte counts and the numbers in a status line are
`.k-mono`. Names people chose — projects, components, environments — and every
word a person reads are the UI sans, and so is the server's own prose: a
remediation, a cause and a health message are sentences, and mono on a sentence
spends the signal on something that is not true of it. Two consequences that
look like exceptions and are not:

- the **eyebrow is sans**, because a section label is a label; and
- the **status pill is mono**, because its text is never a label — it is either
  the state machine's computed word or a structured code rendered verbatim, and
  those two must not look like different kinds of thing.

`.k-mono` sets the face and the size and deliberately **not** the colour, so a
value inside a sentence reads at the sentence's ink and a value that should
recede is dimmed by the row it sits in.

**3. Density, from Coolify's published `DESIGN.md`** (§2.3 of the research):
13.5px body, 12.5px facts, 11.5px micro, **32px controls**, 20/15/13.5px
headings, hairline rules instead of borders-around-boxes, and **no elevation at
all** — `--kelson-shadow` is gone from both palettes, along with the card
gallery it was holding up. Sizes, control heights and the two radii are tokens
(`--kelson-size-*`, `--kelson-control-h`, `--kelson-radius-*`) so a screen asks
for "a fact" rather than for "11.5px".

**4. Quiet when healthy.** The status badge has three loudness tiers and which
tier a tone is in *is* the design:

| Tier | Tones | How it paints |
| --- | --- | --- |
| quiet | `live`, `suspended`, `unknown` | no fill, hairline ring, neutral ink — the tone survives only in the 5px dot |
| working | `deploying`, `waiting` | no fill, a ring and the word in `--kelson-reconciling` |
| loud | `unhealthy`, `stuck`, `failed` | fill, ring and word all in the tone |

So a screen where everything is live carries no saturated colour larger than a
dot, and the first thing that goes wrong is the only coloured object in view.
The same bargain is made everywhere: the deploy outcome panel is neutral even
when the news is good, the `live` tally is ordinary text while the failed one
keeps its red, the stream indicator says `streaming` in grey with a green dot,
and the promote plan colours only the rows it is *skipping*. **The only two
filled objects left in the UI are the needs-attention band and the error
panel** — which is what makes them unmissable without being large.

### Copy

A screen states facts and offers actions; it does not explain kelson's
architecture. Three rules, and the tests pin the strings that carry them:

- **Nothing rendered cites a design record.** No `ADR-` in body text, `note=`
  props, `title` attributes, validation messages or computed status strings. The
  reasoning belongs in a code comment beside the string — most of the ones
  removed in the de-slop pass are still there, one line above the copy they used
  to be inside. `grep -rn "ADR-" src --include="*.tsx" --include="*.ts"` should
  therefore hit comments, test names and `src/gen/` only.
- **Kubernetes vocabulary stays out of the default line.** "Previews are not
  available on this cluster: flux-operator is not installed", not "`fluxcd.controlplane.io`
  is not served". Facts a reader acts on — a revision id, a namespace, a
  digest — are kept as short labelled values; it is the prose around them that
  goes.
- **Empty states name the absence and the next action**, errors name what broke
  and the one thing to do. "No deploys yet." beats a paragraph about why the
  record is empty.

The one deliberate exception is text the *server* wrote: structured errors,
remediations and probe messages are rendered verbatim, code intact, wherever
they land (`ErrorPanel`). A refusal is the owning Go package's sentence and this
UI does not paraphrase it. Which means the rule has a Go half:
`internal/renderer`'s remediations reach a reader through this panel, so they
follow the same standard, and the fixtures quoting them here
(`src/dataservices/DataServices.test.tsx`, `src/components/ErrorPanel.test.tsx`)
are byte-copies of the Go strings rather than plausible-looking inventions.

### One status vocabulary

Every status word a reader sees comes from `src/components/status.ts`
([#260](https://github.com/dafrie/kelson/issues/260)). Before it, the same fact
was said three ways on three screens — `synced` on a project card,
`Reconciling` on an environment panel, `awaiting-artifact` on a preview row —
which are Flux's word, Kubernetes' word and a poller's word, and none of them
answer what a developer is asking.

Eight words, and the first six are the delivery state machine's own answers
relabelled:

| Word | From | Tone |
| --- | --- | --- |
| `live` | answer `live`, phase `Healthy`, preview `ready` | synced |
| `deploying` | answer `progressing`, phases `Reconciling`/`Applied` | reconciling |
| `waiting` | answer `waiting`, phases `Proposed`/`Committed`, preview `awaiting-artifact` | reconciling |
| `stuck` | answer `stuck`, `WorkloadVerdict.stuck` | degraded |
| `unhealthy` | answer `degraded`, phase `Degraded` | degraded |
| `failed` | answer `rejected`, phase `Rejected`, preview `failed` | failed |
| `suspended` | `Preview.suspended` | suspended |
| `unknown` | anything the maps do not know | unknown |

Five things follow from it:

- **The word comes from `answer`, and only falls back to the phase.**
  `StatusResponse.answer` is the engine's own verdict for this environment, so
  `statusForDelivery` reads it and derives from `phase` only when it is empty.
  The phase cannot express all six — `stuck` is not a phase but a verdict about
  one — and re-deriving is a second opinion free to disagree with the CLI's. The
  one source that legitimately sends no answer is `EventService`'s
  `StatusTransition` (four fields, no answer), which is why the fallback exists;
  a *non-empty* token this build cannot read stays `unknown` rather than
  falling back to something cheerier. A transition also voids the fetched
  answer, because it was a verdict about the phase that has just been left
  (`src/pages/matrix.ts`'s `deliveryFacts`).
- **`StatusKind` is a tone, not a word.** `synced`, `reconciling` and the rest
  name the six colour ramps in `tokens.css` and stay as they are, because a
  colour does not change when the word painted in it does. `StatusPill`'s
  `label` is therefore *required*: there is no path by which a tone name
  reaches a screen as text.
- **The wire's own phase survives as a labelled fact, never as the pill.**
  `phase Reconciling` sits beside the word on the environment's Overview, the deploy
  stream's rows and the preview page — it is what an operator correlates with
  Flux — and the preview page's `artifact ready` / `applied ready` conditions
  are unchanged.
- **A workload verdict reaches the vocabulary through `statusForVerdict`.**
  Healthy keeps the environment's word, stuck is `stuck`, degraded is
  `unhealthy`, and all-three-false is `deploying` — the four cases the matrix's
  cells need, decided in the module rather than on each screen. `verdictTone`
  reads the same classification, so the colour on a workload line and the word
  in a cell cannot drift apart. On a verdict *row*, where the pill's label is
  observation's own code verbatim, a stuck verdict grows a second pill carrying
  the word: `code` stays a wait code, so nothing else on the row could say it.
- **`suspended` is a seventh state and not a flavour of `waiting`.** Waiting
  means something is expected to act; suspended means nothing is, deliberately,
  and what is running is the last thing that reconciled. It is only ever shown
  when the wire reported it.

### Drift, the one card only kelson holds

`StatusResponse.stale` answers "am I looking at what I asked for?", and until
[#260](https://github.com/dafrie/kelson/issues/260) no screen drew it. It is a
statement about **revisions and not about health** — a stale environment is very
often `live`, because revision 44 is up and well and simply is not revision 45 —
so it is rendered *beside* the word and never instead of it. `driftFor` in
`src/components/status.ts` decides whether there is drift and what the sentence
is; `DriftMark` is the whole of the drawing.

Four decisions, each of them the one that keeps the claim honest:

- **The tone is quiet, with one mark.** No fill, no amber, no red, and no place
  in the attention band. It takes the quiet tier's shape — hairline ring,
  neutral ink — and the only colour on it is a 5px dot in `--kelson-suspended`,
  the palette's single "nothing is tracking this, and it may be on purpose" hue,
  already pinned as a graphical object rather than as text. Painting drift amber
  would spend the whole colour budget on news that is not bad.
- **Two sentences, because the wire can tell them apart.** A stale environment
  reads **"older than the spec"**; one whose `cause` names a rollback pin reads
  **"pinned to an older revision"**. A pin is stale by construction and
  correctly so, and the pinned wording is what keeps a deliberate state from
  reading as neglect. The pin is recognised by the controller's own reason token
  (`RollbackPinned` / `RolledBack`) as a whole segment of `Cause.String()`, never
  by matching the message's prose.
- **The mark never repeats the revision.** Every site that draws it already has
  the revision on screen as a mono value — the matrix column's revision line,
  Overview's `revision` row, home's meta line, the component page's revision
  fact, the History row's own id — and the mark is the sans sentence about it.
  The revision it means is on the title attribute.
- **It says nothing about *how far* behind.** "The spec is at 45" is not a claim
  this message supports: `stale` is a boolean and `StatusResponse` carries no
  generation, so the revision's own generation is readable from its tag and the
  spec's current one is not on the wire at all. Non-stale renders exactly as
  before — currency is the norm and recedes, drift is the exception and
  advances.

The transport indicator says **streaming**, not "live", for the same reason: a
connected event stream and a running revision are different claims and must not
share a label on one screen.

### The reconciliation ticker

The Console's signature interaction
([#260](https://github.com/dafrie/kelson/issues/260), direction A of the UX
research): a bounded strip of what the cluster has been *doing*, newest first.
Every other live surface here is present tense — the pill, the phase rail, the
drift mark, the verdict rows — and each new answer erases the last one, so an
environment that went `Committed → Reconciling → Rejected` while a reader was on
another tab shows one red pill and no story. The ticker is the story.
`src/live/ticker.ts` is the logic, `src/live/Ticker.tsx` the whole of the
drawing.

One row, verbatim:

```
12s   checkout · production   deploying   Committed → Reconciling   45-9e8d7c6b
```

Left to right: how long ago, which pair, the vocabulary's word, the phases it
moved between, the revision it moved to. The pair is the two names somebody
chose and is therefore sans and a link; everything else is a value the machine
produced and is mono. The arrow appears only when the phase actually moved — a
transition is emitted for a change of phase *or* of revision, so both sides are
sometimes the same phase.

Six decisions, and each of them is what keeps a log from becoming a second
status display:

- **It reads the stream and does not re-interpret it.** The word is
  `statusForPhase(transition.phase)`, which is exactly where a pill on the same
  transition lands: `deliveryFacts` voids the fetched `answer` when a transition
  arrives, so `statusForDelivery` falls through to the phase derivation. The two
  are on screen together and a disagreement between them would be unarguable.
- **Rows are records, so no row carries a `StatusPill`.** A pill claims "this is
  the state now", and the third row down is a statement about an instant that has
  passed. Exactly one object on a screen makes the present-tense claim.
- **Only a settled failure is coloured.** `rowTone` hands a tone to `failed` and
  `unhealthy` and to nothing else, so a strip of twenty normal rows is grey and
  one red word in it is the only thing in view. The badge's *working* tier —
  blue `deploying` — is exactly wrong here: twenty coloured objects reporting
  that everything is fine.
- **Empty is absent, on both screens.** Not a box saying nothing has happened.
  The ring is memory only, nothing is persisted, and the server retains no
  history a page could backfill from, so a fresh load of a quiet instance would
  otherwise carry a permanent empty box.
- **No second transport indicator.** Both screens already have a
  `LiveIndicator` in their header. The strip adds only what that cannot say —
  `reconnecting — rows may be missing` — because while the stream is down the
  rows are not merely stale, they are incomplete. A Resync does *not* clear the
  ring: it says the client's view has gaps, not that what it already saw was
  false, and the strip has never claimed to be the last twenty transitions that
  *occurred*.
- **It opens no stream of its own.** It is a second reader of the `onEvent` the
  screen already hands to `useWatch`.

**Where they are.** The environment's Overview carries its own — that pair's
transitions only, no subject column, sitting below the workloads and *outside*
the `Status` block, because a transition about an environment whose `Status`
call failed is still a real phase and is then the only thing on the page that
can say anything. Home carries the instance-wide one, last on the page and cut
to the newest five of the twenty it holds: home answers "what is the state of
everything" and this is context for that, never the headline.

**The deploy stream is not duplicated.** `/actions/deploy` is a route of its own
and renders its own transitions, so a deploy screen and a ticker are never on
one screen. The ticker is the ambient telling of the same events, which is the
case the deploy screen cannot cover: a deploy someone else started, or one
started from `kelson deploy`, moves these rows exactly as one started here does.
There is deliberately no "a deploy is in progress" note — the rows are the
answer, and inferring one from a phase would be a second opinion beside the
phase rail that already draws it.

**What the stream could not tell it.** `StatusTransition` carries four fields,
so a row cannot say **who** caused it (there is no actor on the wire; a browser,
a `kelson deploy` and a controller re-reconciling are indistinguishable), **which
component** moved (a transition is an environment's; only `HealthChange` is per
workload), or **the engine's answer** (there is none on the message, which is
why the word is derived). Two things it *could* have been given and was not:
`HEALTH_CHANGE` events stay out, because the verdict row and the attention band
already move on one and `HealthChange` carries no remediation and no `stuck`, so
a row would be the same news twice and the poorer telling of it.

**Two clocks.** `WatchResponse.Event.at_unix_ms` is the *server's* stamp and
`Date.now()` is the browser's, so "12s ago" is an elapsed time computed across
two machines. It is still the right field: a client that reconnects inside the
retained window is replayed events that are genuinely minutes old, and printing
those against receipt time would date every one of them to now. The skew is
handled by clamping at zero — a server whose clock runs ahead prints `now`,
never a time in the future — and an event carrying no stamp at all falls back to
when this browser received it. The exact instant stays on the row's `title`.

### The revision rail

The borrowed interaction
([#260](https://github.com/dafrie/kelson/issues/260), direction B of the UX
research): the History tab's record is drawn as a **rail** — one vertical line,
one stop per revision, one filled marker on the revision that is live.
`src/pages/history.ts`'s `railPlacements` decides where every stop sits and
`.k-history__*` in `src/pages/pages.css` is the whole of the drawing.

The rail exists because the question this screen is asked is not "what happened"
but **"what is running, and is it the top one"** — and that is a question about a
position, not about a list. An environment a rollback pinned shows its marker two
stops down and the answer arrives before a word is read.

Five decisions:

- **Position is the statement, and it is the only one.** The five placements —
  `live`, `above`, `below`, `tail`, `unmarked` — are purely spatial, and `above`
  and `below` paint identically. There is deliberately **no count**: "two
  revisions behind" is a claim about the spec's generation and nothing on the
  wire carries one, which is the same reason `DriftMark` refuses it. The drift
  mark stays the sentence and renders on the marker's own stop exactly as
  before; the rail adds no second one.
- **The marker wins over the tail.** The rail runs past the cluster's bounded
  history into the registry's tag list ([#241](https://github.com/dafrie/kelson/issues/241))
  and fades there — dotted rail, dimmed ink — because the record thins out rather
  than ending. But a rollback can pin an environment to a revision the cluster
  has forgotten, and fading the one stop that is live would hide the marker on
  the rail it is the point of. `live` is therefore checked before `beyondWindow`.
- **Click-first, and no drag.** Direction B makes dragging the marker the
  rollback and dragging it sideways the promotion; its own recommendation is to
  ship the click first and add the gestures once the confirm flows are proven. So
  a stop **opens**: a quiet expansion of the row, no modal, no overlay, offering
  two links into flows that already exist — `?from=` for the comparison panel on
  this same page and `actions/rollback?to=` for the whole irreversibility-preview
  screen. The rail never applies anything. A rollback's preview must not be
  skippable, so there is one rollback flow in this UI and the rail only carries a
  revision to it.
- **The offers are collapsed, and that is what makes the rail readable.** Two
  standing buttons per row is twenty-four controls on a twelve-revision
  environment. Quiet-when-healthy applies to chrome as much as to colour. The
  whole summary is the click target — a stretched button under the row with the
  values lifted back above it — and the caret at the end of the head line is the
  visible half.
- **The head is where the next revision arrives, and the only place promotion
  belongs.** Above the newest stop is one more, which is not a revision; opening
  it offers `actions/promote` with the environment action bar's label unchanged,
  because it is the same action. It is drawn only when the project declares
  another environment to promote from — the same condition that bar uses. A
  *row* still offers no promotion: a row's revision is this environment's, and a
  promotion reads the source environment's latest, which no row here knows.

**Sideways promotion, noted and not built.** The research's second gesture —
dragging a notch from staging's rail into production's — has a cheap click-first
expression that this slice deliberately did not build, because it is
cross-environment UI and belongs where environments are already juxtaposed: the
project page's matrix draws one column per environment with each column's live
revision in it. A "promote this column into that one" entry there would need one
thing that does not exist yet — `PromotePage` reads no `?from=` and picks its
source on screen — so the whole of it is a query parameter, a preselected radio
and one link per adjacent column pair. No RPC, no proto, no second promote flow.
### Kubernetes detail, the display preference that adds facts

The second half of the Console's bargain
([#260](https://github.com/dafrie/kelson/issues/260)). The default screens
answer a developer's question — is my change live, is it moving, is something
wrong — and deliberately keep Kubernetes out of the line they answer it in. The
operator who needs the generation, the namespace and the object names was
therefore reading a screen that had those facts in hand and would not print
them. **Kubernetes detail** is the one control that changes that, and it is a
*display preference* in exactly the sense the theme is one: one localStorage key
(`kelson-detail`, absent means off, which is the default), one quiet control in
the header beside the theme, nothing sent to the server, no mode of operation.
`src/expert/` is all of it — `preference.ts` (the store), `DetailToggle.tsx`
(the control), `Detail.tsx` (`<Detail>` and `<KubeFact>`), `Why.tsx` (the caret
and its evidence grid) and `facts.ts` (the two parsers).

Six rules, and the first two are tested rather than described:

- **It gates information, never actions.** Every button, link and flow is on
  screen in both states. `src/pages/expert.test.tsx` asserts the component
  page's action bar is *byte-identical* markup with the preference on and off,
  and that the environment's link set is unchanged — so a screenshot from one
  reader is actionable by another and nobody is missing a control because of a
  display setting.
- **It never hides a could-not-check state or a structured code.** "Not read",
  "status unavailable", the `env` basis mark, "no reading for this component"
  and every `ErrorPanel` code are the states a reader most needs, and the
  honest-absence rule that produced them has no loudness setting. The same test
  file pins the unreachable-cluster case in both modes. Detail may *add* to such
  a state — it does, with a caret saying why the word is `unknown` — and may not
  subtract from it.
- **Off is exactly what shipped.** Nothing moves, nothing is re-worded, and no
  normal-mode screen gained a row. The tests assert `.k-why` and `.k-kfact` are
  absent from the overview, the history tab and the matrix when it is off.
- **Expert mode surfaces; it does not compute.** Every value it prints is
  something a response already said. The two derivations are string parsers in
  `facts.ts` and both are strict: `revisionParts` reads `<generation>-<hash8>`
  (the tag [ADR-0028](../docs/adr/0028-delivery-spine.md) decision 2 defines,
  which is why `stale` needs no second read) and `resourceParts` splits a
  verdict's `Kind/namespace/name`. A shape they do not recognise yields nothing
  and the screen draws nothing — a wrong generation number on a dense screen is
  worse than an absent one. **The resource kinds a component *would* render are
  deliberately not shown**: that mapping lives in `internal/renderer` and the
  browser would be quoting it from memory, so an object is named only where the
  cluster actually reported one.
- **The why-caret belongs to one statement.** Where the UI makes a
  plain-language claim — a status word, `stuck`, "older than the spec",
  "deployed now" — expert mode attaches a 14px caret that expands the fields
  that claim was computed from, verbatim, with one sentence saying which rule
  applied. It is absent in normal mode and **collapsed by default in expert
  mode**: the preference says the evidence should be *available*, not that
  fifteen panels should be open at once. A field the wire did not answer is
  dropped from the grid rather than rendered blank, because a key with nothing
  beside it reads as an empty string that was actually sent.
- **Kubernetes vocabulary is allowed here and only here.** The copy rule above
  keeps `generation`, `namespace` and `kind` out of the default line; this is
  the line the reader asked for them on.

Where it landed, and what each surface gained:

| Surface | Facts | Carets |
| --- | --- | --- |
| Environment Overview | the revision tag's `generation` and `spec hash`; each verdict's `kind` / `namespace` / `name` | the status word (answer vs. phase), the drift note, a `stuck` verdict |
| Component page | the same two revision halves; the verdict's three parts | the page's word (its own probe, or its environment's, or unread) |
| Project matrix | per column: `generation` and the resolved `namespace` | **none** — see below |
| History tab | each row's `generation` | "deployed now" |
| Previews section, preview detail page | a preview's `namespace` named again as the OCIRepository and the Kustomization it also is; `PreviewLifecycle.name` named as the ResourceSetInputProvider and the ResourceSet | a preview's status word (`Preview.phase` / `.suspended`, with `artifactReady` / `appliedReady` / `reason` / `message` as evidence); the lifecycle sentence (`providerReady` / `providerReason` and `setReady` / `setReason` as evidence) |

**What was deliberately not reached**, so the next slice starts from the truth:

- **No caret anywhere in the matrix.** A cell is a `<Link>` and a `<details>`
  cannot live inside an anchor; a column header sits inside
  `.k-matrix__scroll`, whose `overflow-x` clips a positioned panel on both axes.
  The environment's Overview is one press away and carries the same evidence
  with room to draw it. The column heads still gain their two facts.
- **Untouched surfaces:** the deploy / promote / rollback actions, the logs tab,
  the cluster and connections screens, the editor, and home. The previews
  section and the preview detail page came off this list (#268 item 3):
  `Preview.namespace` is named again as the OCIRepository and the Kustomization
  it also is, `PreviewLifecycle.name` is named as the ResourceSetInputProvider
  and the ResourceSet, and both are wrapped in the shared `LifecycleStatus`
  component (`src/previews/Previews.tsx`) so the environment's Previews section
  and the preview detail page's not-found state carry the same facts rather
  than two copies of the markup. `PreviewPill`'s why-caret shows a preview's two
  Ready conditions and its `reason` / `message`, and `LifecycleStatus`'s shows
  the provider and set conditions' reasons — the fields the lifecycle sentence
  is actually derived from (`lifecycleLine`).
- **Wire gaps hit.** Two facts wanted and not available: `StatusResponse` has
  a `detail` map documented for adapter counts (`resources` / `live` /
  `degraded`) that `internal/api` never fills, so there is nothing to print;
  and `stale` is a boolean and the *spec's* current generation is on no
  message, so "45 vs 47" cannot be shown, only "45, and behind". No response
  still carries the Flux `Kustomization` / `OCIRepository` names for a *normal*
  environment the way `Preview` does for a preview, so an environment's own
  Flux objects still cannot be named without inventing them from the model's
  conventions — that gap is now closed only on the previews surfaces, where the
  wire actually names the objects. `Preview` itself carries no per-condition
  reason for `artifactReady` / `appliedReady` — only one `reason` / `message`
  pair for the whole phase — so the why-caret shows the pair rather than a
  reason per condition.

### Two themes

The UI **follows the operating system by default** and can be pinned either way.
`docs/design/README.md` records the mockups as dark-only; the owner decided
otherwise on 2026-08-13, and dark remains the shipped design — not one value of
it moved to make room for the second theme.

The control is one button in the header, cycling **light → dark → system**. The
third state is the absence of the `kelson-theme` key in `localStorage`, which is
why a fresh browser and a browser that was reset behave identically; while it is
in that state a `matchMedia` listener applies OS changes live, and the button
carries a small `auto` mark so "dark because you asked" is distinguishable from
"dark because your machine is". `src/theme.ts` holds all of it; the toggle is
`src/components/ThemeToggle.tsx` and its two glyphs are inline SVG, because two
icons are not worth a dependency.

Selection is a `data-theme` attribute on `<html>`, and it is **never absent at
paint time**: a blocking inline script in `index.html` reads storage and
`prefers-color-scheme` before the bundle loads, so the page never flashes the
wrong theme. That script duplicates exactly three things from `src/theme.ts` —
the storage key, the media query and the two `theme-color` values — and the two
have to be kept in step.

`tokens.css` has three blocks: `:root` for type and layout (no colours, ever),
`:root, :root[data-theme="dark"]` for the dark palette, and
`:root[data-theme="light"]` for the light one. Components never learn which
theme is on; they use tokens, and a component that hardcodes a colour is a
component the light theme cannot reach. `src/styles/tokens.test.ts` parses the
file and fails if the two palettes stop defining the same token names — the
guard for the token someone adds to one block and forgets in the other.

**Where the light palette comes from.** The design project contains exactly one
light surface: the logo board in `docs/design/Kelson Logo.dc.html`. Its paper
(`#f4f5f3`) is the page, and its OKLCH chrome — heading `oklch(0.24 0.015 250)`,
body `0.42`, eyebrow `0.55`, borders `0.88` and `0.93` — converted to sRGB, is
the text ramp and the hairlines. Everything else moves along the board's own
axes: neutrals stay on hue 250, and each status hue keeps its dark-theme hue and
drops in lightness until it clears WCAG AA.

Two consequences worth knowing:

- **`--kelson-green` and `--kelson-accent` are different tokens**, and since
  #260 they are different *kinds* of thing. The mark's `#0FA36B` is
  theme-independent — a logo does not change hue because the page went white.
  The accent is interaction, and it is neutral in both themes; see "The Console"
  above for why.
- **There is no elevation.** Both themes separate surfaces with a hairline, and
  `--kelson-shadow` no longer exists. This is a calm console, not a card
  gallery.

**The contrast pins.** `tokens.test.ts` computes WCAG ratios and pins the pairs
the design rests on, each with its own floor because "you read this" and "you
notice this" are different jobs:

| Pinned | Floor | Why |
| --- | --- | --- |
| `text` and `accent` on page/panel/panel-dim | 7:1 | what a reader actually reads, and links are text first |
| `page` on `accent` | 7:1 | the primary button, inverted |
| `text-2`, `text-3` on the three surfaces | 4.5:1 | labels and facts are 11.5–12.5px, which is normal text by WCAG |
| `muted` on the three surfaces | 4:1 | see the disclosure below |
| `synced`/`reconciling`/`degraded`/`failed` on the three surfaces | 4.5:1 | **new**: badges lost their fills, so a tone that is text now sits on a bare surface |
| `suspended` on the three surfaces | 3:1 | it is only ever a dot — a graphical object under WCAG 1.4.11 |
| `text`/`text-2`/tone on `degraded-fill` and `failed-fill` | 7 / 4.5 / 4.5 | the band and the error panel are the last filled objects, so everything on them is pinned |
| `line` on panel | 1.4–3:1 | a link's underline: visible, and quieter than its ink |
| `hairline` vs `line` on panel | strictly quieter | a row separator must never read as a border |

One of those is a **disclosure, not a pass**: `--kelson-muted` is `#6f7b89` in
dark, which is 4.18:1 on a panel — under AA. It is one of the values pinned to
`docs/design/assets/README.md`, so raising it is a decision about the recorded
design and not a side effect of a visual pass. What #260 did do is stop using
the step *below* it (`--kelson-muted-deep`, 2.46:1) for text at all — every meta
line, gauge label, rail actor and log timestamp moved up to `--kelson-muted`,
and `--kelson-muted-deep` now paints only glyphs.

There are no CSS frameworks and no component library: the primitives in
`src/styles/base.css` and `src/pages/pages.css` are hand-rolled against the
tokens, which is the house style. Adding a dependency to this package is a
decision, not a convenience.
