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
