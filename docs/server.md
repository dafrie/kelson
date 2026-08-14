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

In a cluster it is a Helm install, and every flag below is a values knob — see
[installing kelson](install.md).

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

It is not TLS, not per-user identity for humans, and not a defence against anyone who can read the
process's environment or command line. It raises the floor from *anything that can reach the port is
the operator* to *a caller must hold the shared secret*. Issue
[#84](https://github.com/dafrie/kelson/issues/84) still owns the human half of the real answer —
project-level team auth, OIDC and TLS. The agent half is below.

## Agent identities

An agent is a principal, not a human with a borrowed token
([#74](https://github.com/dafrie/kelson/issues/74),
[ADR-0024](adr/0024-agent-identities.md)). Beside the shared password the server accepts **agent
tokens**: named credentials with a scope, an expiry and a request budget, each revocable on its own.

### Issue one

`kelson agent` talks to the cluster, not to the server. Its authority is your kube context and the
RBAC on the state namespace — so someone holding only the server password cannot mint an identity,
and your cluster's audit log records who did.

```sh
kelson agent create deploybot --project shop --env development --allow mutate --ttl 24h
```

```
created agent identity deploybot
  expires:      2026-08-15T09:00:00Z
  projects:     shop
  environments: development
  operations:   mutate
  budget:       120 requests/minute, burst 30

token (shown once — the server keeps only a hash of it):
kagt.deploybot.…
```

The token is returned **once**. The server stores a salted HMAC of it and nothing else, so it cannot
be recovered — a lost token is replaced, not found. `kelson agent list` reports every identity,
including revoked and expired ones, and never a credential.

`AgentService.CreateAgent/ListAgents/RevokeAgent` is the same three operations over the API, for a
human logged in with the password. **Both surfaces refuse an agent credential**: an agent that could
mint an agent could mint one wider than itself.

### Use one

The token goes in the same header the password does:

```sh
curl -H "Authorization: Bearer $KELSON_AGENT_TOKEN" \
  -H 'Content-Type: application/json' -d '{}' \
  http://127.0.0.1:8420/kelson.v1alpha1.SpecService/ListSpecs
```

`kelson-mcp` reads `--token` / `KELSON_AGENT_TOKEN` and presents it on every call
([the MCP server](mcp.md)).

### What a scope means

| Dimension | Empty means | Enforced as |
|---|---|---|
| `--project` | every project | the project the request names |
| `--env` | every environment | the environment the request names |
| `--allow` | *refused* — a grant must be explicit | `read`, or `mutate` (which implies `read`) |

`read` covers status, history, logs, events, render, diff, preview listing and secret *listing*
(names and keys — no RPC in the schema can return a value). `mutate` covers deploy, rollback,
promote, build, spec writes and secret writes.

Enforcement is server-side, in a ConnectRPC interceptor driven by a table that maps **every**
registered method to a scope, with a coverage test over the generated descriptors. A method with no
row is refused to everyone — a new RPC fails closed rather than shipping open.

Two consequences of that table are worth knowing before you scope a credential:

- **A credential restricted by project or environment is refused the calls whose target the request
  cannot state.** That is `ListSpecs` (it spans every project), `PutSpec` (the project name is inside
  the YAML), a `Watch` with no scopes, and any call carrying an inline spec instead of a project name.
  Refused, not filtered: a scope that silently narrowed a response would be indistinguishable from one
  that did not apply. An identity with no `--project` and no `--env` still reaches them.
- **Log queries address a namespace**, so a scoped credential reaches one only through the renderer's
  `<project>-<environment>` convention. An environment that sets `spec.namespace` is therefore not
  readable by a scoped credential at all. ADR-0024 §4 records the collision case this leaves open.

### Expiry, rotation and revocation

`--ttl` defaults to 24h and is capped at 30 days; a longer request is refused rather than clamped.
Expiry is checked on every request.

Rotation is create-then-revoke — create the successor, configure the agent with its token, then
revoke the predecessor. There is no `rotate` verb, because revoking before the successor is in place
breaks the agent and revoking after needs to know when "after" is.

```sh
kelson agent revoke deploybot
```

Revocation takes effect on the **next request** that credential makes: the server reads the identity
from cluster state on every request and caches no allow decision, so there is no window and no cache
to wait out. It writes one object — no human session, no other identity and no password is affected.

### Budgets

`--rate` (requests per minute, default 120) and `--burst` (default 30) are a token bucket per
identity. A breach is ConnectRPC `resource_exhausted`, not `permission_denied`, so a client knows to
back off rather than to give up. **The buckets are in-memory and therefore per replica**: two
replicas each grant the full budget. That is honest for a single-replica v0 and it is the reason to
treat the number as a runaway-loop cap rather than a quota.

### Attribution

Every authenticated request leaves one JSON line on stderr:

```json
{"level":"INFO","msg":"rpc","principal":"agent:deploybot","principal_type":"agent",
 "procedure":"/kelson.v1alpha1.DeployService/Deploy","outcome":"allowed"}
```

`principal` is `agent:<name>`, `human:<name>`, `human` for a bearer-password caller, or `anonymous`.
This is the seam the audit trail of [#78](https://github.com/dafrie/kelson/issues/78) builds on — it
is not the audit trail: there is no queryable store, no retention and no tamper evidence. No request
payload is logged.

### What this is not

Not authorization for humans: the password is still one shared secret. Not visible in the UI yet.
And an agent's scope does not change which MCP tools are offered, only which calls succeed.

## Agent policy: what agents may do in *this* environment

A scope says what one credential may reach. Policy says what **any** agent may do to one
environment, and it lives in the environment's own spec
([ADR-0025](adr/0025-agent-policy.md), [#75](https://github.com/dafrie/kelson/issues/75)):

```yaml
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: production}
spec:
  project: shop
  policy:
    agents: propose-only        # allow | propose-only
    require: [dry-run]          # kelson must dry-run the change and it must pass
    maxReplicas: 5              # the largest an agent may scale a workload here
    protect: [db]               # components an agent may not remove or scale to zero
    forbid: [secret-set]        # operations refused to agents here
```

The same block exists as `spec.defaults.policy` on the Project. The Environment's wins **whole** —
the two do not merge (rule P4).

**Nothing is restricted until you write it.** An environment with no `policy:` block narrows nothing,
because the credential is already the grant: an agent cannot change anything unless someone ran
`kelson agent create --allow mutate`. Write the block on the environments that need guarding, which
is usually production.

**It is enforced on the server, from the stored spec.** kelson reads the policy out of the spec store
for the environment a request acts on — never out of the request. An agent that sends its own
documents saying `agents: allow` for an environment kelson holds as `propose-only` is refused, so a
modified client buys nothing. Humans are never subject to any of this; a caller authenticated with
the password is not an agent.

### The operations `forbid:` knows

`deploy`, `rollback`, `promote`, `build`, `secret-set`, `secret-delete`, `spec-write`, `spec-delete`
— one name per mutating RPC.

`spec-write` and `spec-delete` are guarded for a reason worth spelling out: an environment's desired
state includes the line saying it is `propose-only`. Without that, an agent could rewrite the policy
and then deploy. Since a `PutSpec` replaces the project's whole document set (and omitting an
environment deletes it), every stored environment of the project has a say — so an agent cannot store
a spec for a project that has *any* propose-only environment.

### What `propose-only` does today

It refuses every live mutation of the environment and tells the agent how to propose instead: re-send
the deploy with `dry_run=RENDER` for the manifests, or call `Diff`. A human then applies the change.

It does **not** open a pull request yet. The git writer implements pull-request mode
(`internal/delivery/git`), but the server commits directly and has no forge credentials, so claiming
"proposal opened" would be a lie. ADR-0025 §7 records the gap and the seam that closes it.

`build` is deliberately not refused by `propose-only`: a build produces an artifact in a registry and
changes no environment. Use `forbid: [build]` to stop it.

### Refusals name the rule, and the escalation

```json
{"code": "agent-policy/max-replicas",
 "resource": "environment/shop/production",
 "field": "$.spec.policy.maxReplicas",
 "message": "component \"web\" would run 9 replicas in shop/production, and policy caps an agent at 5",
 "remediation": "lower replicas for \"web\" to 5 or fewer, or ask a human to deploy the larger count. escalate: …"}
```

| Code | Rule |
|---|---|
| `agent-policy/propose-only` | `agents: propose-only` |
| `agent-policy/forbidden-operation` | `forbid:` |
| `agent-policy/max-replicas` | `maxReplicas:` |
| `agent-policy/protected-resource` | `protect:` |
| `agent-policy/dry-run-required` | `require: [dry-run]` |
| `agent-policy/unaddressed` | a mutation naming no project and environment |
| `agent-policy/unreadable` | the stored policy could not be read — fails closed |

All of them are ConnectRPC `permission_denied`: retrying never helps, a human or a spec change does.
The prefix is `agent-policy/` rather than `policy/` because `policy/*` already means an admission
webhook or a Kyverno policy rejected the manifest — a different problem with a different fix.

**Escalation is the error.** There is no approval queue: the refusal names what a human must do —
run the operation themselves, or relax the rule on the stored Environment. An agent that hits one
should surface it to a person rather than retry.

### Two things to know before you rely on it

- A **rollback** is not checked against `maxReplicas` or `protect`: it replays recorded manifests, so
  there is no resolved spec to read. `forbid: [rollback]` and `propose-only` do cover it.
- On a server started **without a password**, every caller is anonymous and kelson cannot tell a
  person from an agent — so it applies the agent rules to everyone. Set a password.

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

## PreviewService reads flux-operator's objects, and one read it cannot be granted

`PreviewService.ListPreviews` ([ADR-0017](adr/0017-pr-previews.md)) reports which pull requests of an
environment are running. It reads four kinds in the **environment's own** namespace — the
`ResourceSetInputProvider` and `ResourceSet` kelson renders, and the per-change-request `OCIRepository`
and `Kustomization` flux-operator instantiates from them — and the chart grants `get` and `list` on all
four in every namespace it serves. Nothing is granted for writing: previews are published by CI and
torn down by flux-operator, and no RPC in this schema creates or deletes one.

The exception is hostnames. A preview's HTTPRoutes live in the preview's own namespace,
`<project>-<environment>-pr<id>`, which is created at reconcile time and cannot be listed in
`rbac.targetNamespaces` when the chart is installed. So that read needs cluster scope, the chart does
not grant it, and without it the preview list is complete except for its hostnames — the read fails
soft rather than failing the call. Bind this yourself if you want them:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: kelson-preview-routes
rules:
  - apiGroups: ["gateway.networking.k8s.io"]
    resources: ["httproutes"]
    verbs: ["get", "list"]
```

It is a cluster-wide read of routing objects and nothing else; kelson narrows the query to the
`app.kubernetes.io/managed-by: kelson` provenance labels and to namespaces named for this
environment's previews.

## Where the code lives

- `cmd/kelson-server` — flags, the mux, the bind check.
- `cmd/kelson` — `kelson agent create|list|revoke` (`agent.go`), which writes to the cluster.
- `internal/api` — the ConnectRPC handlers (`api.go`), the credential gate (`auth.go`), the principal
  (`principal.go`), the RPC-to-scope table (`scope.go`), the authorization interceptor (`authz.go`)
  and the per-identity budgets (`ratelimit.go`).
- `internal/serverstate` — the ConfigMap-backed spec and history stores, and the Secret-backed agent
  identity store (`agent.go`).
- `internal/secret` — the cluster secret backend behind `SecretService`.
- `internal/delivery/flux` — the preview read behind `PreviewService` (`previews.go`).
