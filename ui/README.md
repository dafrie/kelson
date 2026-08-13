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
a deploy or a log tail is a link that keeps working.

| Route | What it does | RPCs |
| --- | --- | --- |
| `/apps` | One card per (project, environment): phase pill, revision, cause, live/degraded counts | `ListSpecs`, then one `DeployService.Status` per card |
| `/apps/new` | Create a component: three fields, a rendered preview, then the store | `PutSpec` at `RENDER`, then with an idempotency key |
| `/apps/:project` | Environment tabs with status, workload verdicts and the stored documents; buttons into the four flows | `GetSpec`, `Status` |
| `/apps/:project/edit` | Edit the stored spec: a form tab and a raw YAML tab, a diff before saving, an optimistic-concurrency save | `GetSpec`, `PutSpec` at `RENDER` then for real, `Diff` |
| `/apps/:project/:env/deploy` | Preview (render dry-run) then a confirm that streams the deployment live | `Deploy` at `RENDER`, then at `NONE`; optional `Diff` at `SERVER` |
| `/apps/:project/:env/diff` | The live cluster's own dry-run verdict, rendered from `diff_json` | `Diff` at `SERVER` |
| `/apps/:project/:env/logs` | Bounded Query and unbounded Follow, with a dropped-lines banner | `QueryLogs`, `FollowLogs` |
| `/apps/:project/:env/rollback` | Revision picker, irreversibility preview, then the apply | `History`, `Rollback` at `RENDER` then `NONE` |
| `/cluster` | Server build and the detected ClusterProfile | `/healthz`, `GetProfile` |

There is **no history screen**: [#67](https://github.com/dafrie/kelson/issues/67)
defers it. Rollback calls the History RPC to offer target revisions, which is a
picker for an action and not a screen about the past.

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
- **Creating an app asks three questions.** `/apps/new` shows a name, an image
  and a port, and nothing else; an empty port is a worker, because the model
  derives the workload kind from the shape rather than asking for a type. Every
  other field the model can express sits behind one "More options" disclosure —
  progressive disclosure is the whole design (docs/model.md's own target
  shape), and the named failure mode is a first screen that asks forty
  questions to deploy one container.

## Building spec documents in the browser

`src/spec/documents.ts` writes the Project and Environment documents `/apps/new`
stores. Three things about it are load-bearing:

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

`/apps/:project/edit` (#65) is the other half: `documents.ts` writes a document
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
a data component, an anchor — is missing from the rebuild and shows up as a
byte difference. There is no path where the module drops something *and* still
claims the document is editable. The reader is never asked to trust the parser;
they are shown its output compared against their own bytes.

The parser is small and strict on purpose. It reads the grammar the builders
emit — two-space indentation, block mappings and sequences, one flow mapping for
`replicas`, plain and double-quoted scalars — and gives up on everything else. It
is not a YAML implementation and must not become one: a document it cannot read
costs the reader the form tab, which is the right outcome.

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
Go plugin. `src/api/clients.test.ts` builds a client for all five services
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
drops in lightness until its 11.5px mono pill text clears WCAG AA against its own
fill (synced 5.4:1, reconciling 5.4:1, degraded 4.9:1, failed 5.8:1, suspended
4.9:1).

Two consequences worth knowing:

- **`--kelson-green` and `--kelson-accent` are different tokens.** The mark's
  `#0FA36B` is theme-independent — a logo does not change hue because the page
  went white. Brand-as-*text* (links, `fix:` labels, primary buttons) is
  `--kelson-accent`, and on light it drops to `#0b724b`, the darkest step of the
  ramp in `website/src/css/custom.css`, because `#0FA36B` on white is 3.2:1 and
  fails AA.
- **`--kelson-shadow` is the only elevation in the system**, `none` under dark
  and a 1px whisper under light, where a white panel on near-white paper needs
  more than a border to read as raised. It is on `.k-panel` and `.k-env` and
  nowhere else; this is a calm console, not a card gallery.

There are no CSS frameworks and no component library: the primitives in
`src/styles/base.css` and `src/pages/pages.css` are hand-rolled against the
tokens, which is the house style. Adding a dependency to this package is a
decision, not a convenience.
