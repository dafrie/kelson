# The server

`kelson-server` serves the `kelson.v1alpha1` schema over ConnectRPC — the same API the web UI, the
MCP surface and any other client talk to ([ADR-0013](adr/0013-server-state-and-api-v0.md),
issue [#139](https://github.com/dafrie/kelson/issues/139)).

It holds no state. Specs and deployment history live as ConfigMaps in the namespace it runs against,
so a restart or a second replica loses and forks nothing, and `kubectl get configmaps -l
kelson.dev/project=x` is the audit trail.

```sh
kelson-server                                   # 127.0.0.1:8420, namespace kelson-system
kelson-server --namespace kelson-system --registry ghcr.io/acme
```

## Flags

| Flag | Environment | Default | What it does |
|---|---|---|---|
| `--listen` | — | `127.0.0.1:8420` | Address to serve on. See [Who may reach it](#who-may-reach-it). |
| `--insecure-bind` | — | off | Allow a non-loopback bind with no password. |
| `--password` | `KELSON_PASSWORD` | unset | The shared password clients authenticate with. Unset means no authentication. |
| `--kubeconfig` | `KUBECONFIG` | in-cluster, then `~/.kube/config` | Which cluster it works against. |
| `--namespace` | — | `kelson-system` | Where the spec and history ConfigMaps live. |
| `--keep` | — | 20 | Deployment revisions retained per environment. |
| `--registry` | `KELSON_REGISTRY` | unset | Destination registry for builds, e.g. `ghcr.io/acme`. See [build](build.md). |
| `--push-secret` | — | unset | Name of an existing `kubernetes.io/dockerconfigjson` Secret authenticating the push. |
| `--build-namespace` | — | the environment's namespace | Where build Jobs run. |

`/healthz` reports liveness plus the build (`version`, `commit`) and never requires authentication —
a probe holds no credential, and a server that failed its liveness check because nobody logged in
would be restarted forever.

## Who may reach it

This is the **interim** answer, taken with the project owner on 2026-08-13, and it is deliberately
smaller than the design issue [#84](https://github.com/dafrie/kelson/issues/84) owns. Three sentences
first, then the detail:

- **CLI users are already authenticated, by Kubernetes.** `kelson` talks to the cluster directly with
  your kube context, so the cluster's RBAC is the access control and there is nothing to configure.
  The server is not in that path at all.
- **Web and other API clients get one shared password.** Set `--password` (or `KELSON_PASSWORD`) and
  every `kelson.v1alpha1.*` route requires it.
- **A username is a display name, not an identity.** The login form asks for one and the UI shows it,
  but nothing checks it and nothing authorizes on it. Per-project team auth over OIDC is the real
  design, and it is later.

### With no password: loopback, and it says so

The default posture is v0's. Nothing authenticates, so the server binds `127.0.0.1` and **refuses** a
non-loopback `--listen`:

```
error: --listen 0.0.0.0:8420 would expose kelson-server beyond loopback with no authentication and no
TLS (ADR-0013 §3; the threat model is issue #84). Set --password (or $KELSON_PASSWORD) so clients must
authenticate, bind 127.0.0.1 and forward a port, or pass --insecure-bind to accept the risk
deliberately
```

`--insecure-bind` still overrides that refusal — for a private network you have reasoned about — and
still warns on every start. It has not become quieter; it has stopped being the only way out.

### With a password: a non-loopback bind is allowed, with a warning

A password changes the fact on the ground, so it is what lifts the refusal:

```sh
KELSON_PASSWORD=… kelson-server --listen 0.0.0.0:8420
warning: --listen 0.0.0.0:8420 serves beyond loopback. Every kelson.v1alpha1 route requires the shared
password, but there is no TLS: put a TLS-terminating proxy in front, or the password crosses the
network in clear (issue #84)
```

**Put a TLS-terminating proxy in front.** There is still no TLS in kelson-server itself; a password
sent in clear is a password anyone on the path has. Behind a proxy that sets `X-Forwarded-Proto:
https` the session cookie is marked `Secure` automatically.

Prefer the environment variable to the flag: a flag value is visible in every `ps` on the machine and
in shell history.

### Sessions

A session is a signed token, not a row in a table. ADR-0013 §1 says the process holds no state a
restart or a second replica would lose or fork, and a session store would be exactly that. So:

- The signing key is HMAC-SHA256 over a **per-process random salt** and the password. Every restart
  mints a new key, so **every session dies when the server restarts** and the UI asks for a login
  again. That is honest for this cut rather than a bug — there is no revocation list because there is
  nothing to revoke in.
- The token carries the display name and an expiry (12 hours) and is verified by signature alone.
- Browsers hold it in a cookie named `kelson_session`: `HttpOnly` (script cannot read it),
  `SameSite=Lax` (ordinary navigation and the Vite dev proxy keep it; nothing here is meant to be
  embedded cross-site), `Path=/`, and `Secure` when the request arrived over TLS.

There is no CORS handling and none is needed: the UI is served from the same origin as the API in
production, and in development `ui/vite.config.ts` proxies `/kelson.v1alpha1.`, `/healthz` and
`/auth/` to the server, which makes the browser's origin and the API's the same one. The proxy
forwards cookies unchanged.

### The three session endpoints

| Endpoint | Answer |
|---|---|
| `POST /auth/login` | `{username, password}` → **200** `{username}` and a `Set-Cookie`; **401** wrong password; **400** malformed or missing username; **204** authentication is disabled. |
| `POST /auth/logout` | **204**, cookie expired. |
| `GET /auth/session` | **204** authentication is disabled · **200** `{username}` you have a session · **401** log in. |

The tri-state on `/auth/session` is the contract the UI boots on, and it is three states rather than
two on purpose: "no password is configured" and "you are not logged in" are different facts, and a UI
that conflated them would show a login form no password could satisfy. A server with no password
answers 204 and the UI skips login entirely — today's behaviour, unchanged.

### Non-browser clients: one secret, two transports

Anything without a cookie jar — `kelson-mcp`, `curl`, a script — sends the same shared password as a
bearer token instead:

```sh
curl -H "Authorization: Bearer $KELSON_PASSWORD" \
  -H 'Content-Type: application/json' -d '{}' \
  http://127.0.0.1:8420/kelson.v1alpha1.SpecService/ListSpecs
```

`kelson-mcp` does this for you: it reads `--password` / `KELSON_PASSWORD` and sets the header on every
call, unary and streaming ([the MCP server](mcp.md)).

A rejected request comes back as ConnectRPC `unauthenticated`, so a client can tell "log in again"
from "the server broke" without reading prose.

### What this is not

It is not TLS, not per-user identity, not authorization, and not a defence against anyone who can read
the process's environment or command line. It raises the floor from *anything that can reach the port
is the operator* to *a caller must hold the shared secret*. Issue
[#84](https://github.com/dafrie/kelson/issues/84) still owns the real answer — project-level team auth,
OIDC, agent identities ([#74](https://github.com/dafrie/kelson/issues/74)) and TLS.

## SecretService needs Secret permissions, and that is a real grant

`SecretService` ([#116](https://github.com/dafrie/kelson/issues/116)) writes the Kubernetes Secrets a
spec's `{secret: <name>, key: <key>}` references point at, so the server's service account needs
`get`, `list`, `patch`, `create` and `delete` on `secrets` in the namespaces it serves. ADR-0009 names
this as a cost rather than a detail: it is what makes masked read-back possible and it is a meaningful
privilege, and today's single shared password does not scope it per project or per environment.

Two things bound it, and neither is authorization:

- kelson writes and deletes only Secrets carrying `kelson.dev/managed-secret: "true"`, and listing is
  a label query over it, so a namespace's TLS material and service-account tokens are neither
  enumerated nor removable through this API.
- No RPC returns a secret value. The grant lets a caller *write* credentials into namespaces the
  server can reach; it does not turn the API into a way to read them out.

Least-privilege RBAC for this path belongs to #84 along with the rest of the threat model. Until it
lands, run the server with a service account scoped to the namespaces it is meant to serve.

## Where the code lives

- `cmd/kelson-server` — flags, the mux, the bind check.
- `internal/api` — the ConnectRPC handlers (`api.go`) and the auth gate (`auth.go`).
- `internal/serverstate` — the ConfigMap-backed spec and history stores.
- `internal/secret` — the cluster secret backend behind `SecretService`.
