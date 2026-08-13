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

## The screens

Milestone M6 ([#7](https://github.com/dafrie/kelson/issues/7)). Every flow is
addressed by a project **and** an environment, because that pair is what the
API's mutating RPCs take — a `SpecRef` plus an environment name — so a link into
a deploy or a log tail is a link that keeps working.

| Route | What it does | RPCs |
| --- | --- | --- |
| `/apps` | One card per (project, environment): phase pill, revision, cause, live/degraded counts | `ListSpecs`, then one `DeployService.Status` per card |
| `/apps/:project` | Environment tabs with status, workload verdicts and the stored documents; buttons into the four flows | `GetSpec`, `Status` |
| `/apps/:project/:env/deploy` | Preview (render dry-run) then a confirm that streams the deployment live | `Deploy` at `RENDER`, then at `NONE`; optional `Diff` at `SERVER` |
| `/apps/:project/:env/diff` | The live cluster's own dry-run verdict, rendered from `diff_json` | `Diff` at `SERVER` |
| `/apps/:project/:env/logs` | Bounded Query and unbounded Follow, with a dropped-lines banner | `QueryLogs`, `FollowLogs` |
| `/apps/:project/:env/rollback` | Revision picker, irreversibility preview, then the apply | `History`, `Rollback` at `RENDER` then `NONE` |
| `/cluster` | Server build and the detected ClusterProfile | `/healthz`, `GetProfile` |

There is **no history screen**: [#67](https://github.com/dafrie/kelson/issues/67)
defers it. Rollback calls the History RPC to offer target revisions, which is a
picker for an action and not a screen about the past.

Four things the screens are deliberate about:

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

`src/diff/parse.ts` decodes `diff_json` against the Go types in
`internal/diff/diff.go`. Its fixture, `src/diff/testdata/server-diff.json`, is
real `diff.EncodeJSON` output captured from `internal/diff/format_test.go` —
which is what makes the test an agreement with Go rather than with itself.

## Why there is a dev proxy and no CORS

`kelson-server` serves ConnectRPC and `/healthz` on one mux with no CORS
middleware and, in v0, no authentication at all (ADR-0013 §3 — which is why it
refuses to bind beyond loopback without `--insecure-bind`). It assumes the UI is
served from the same origin it is, which in production is true: the built assets
ship behind the same listener.

In development Vite serves on `:5173` and the API is on `:8420`, which would be
cross-origin. Rather than add CORS handling to the Go server for the benefit of
development only, `vite.config.ts` proxies to it:

| Path | Forwarded to |
| --- | --- |
| `/kelson.v1alpha1.*` | `http://127.0.0.1:8420` |
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

**The UI is dark-only by design.** The mockups explore no light palette and
`docs/design/README.md` records that as the decision, so there is no light theme
and no theme toggle — one palette that is right beats two that are half-done.

One animation exists: `kelson-pulse`, reserved for reconciling status dots. If
something else starts pulsing, the signal stops meaning "work is in flight".

There are no CSS frameworks and no component library: the primitives in
`src/styles/base.css` and `src/pages/pages.css` are hand-rolled against the
tokens, which is the house style. Adding a dependency to this package is a
decision, not a convenience.
