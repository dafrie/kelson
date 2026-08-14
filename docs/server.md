# The server

`kelson-server` serves the `kelson.v1alpha1` schema over ConnectRPC — the same API the web UI, the
MCP surface and any other client talk to ([ADR-0013](adr/0013-server-state-and-api-v0.md),
issue [#139](https://github.com/dafrie/kelson/issues/139)).

**It is a stateless façade over Kubernetes.** The wire surface is unchanged; what is underneath it is
not ([ADR-0027](adr/0027-crd-native-control-plane.md) decision 6). `SpecService` reads and writes
`Project` and `Environment` **custom resources** with server-side apply under the field manager
`kelson-server`; `DeployService.Status` reads `Environment.status` instead of computing it, because the
controller already wrote what it observed there. ADR-0013 §1's rule — the process holds no state a
restart or a second replica would lose or fork — is more true after this change than before it, and
`kubectl get projects,environments -A` is now the inventory.

Optimistic concurrency is `resourceVersion`, carried through the API as the same opaque `version`
string, and a conflicting write is still `store/version-conflict`. The store vocabulary
(`store/version-conflict`, `store/not-found`, `store/too-large`) is unchanged; only what it is a
vocabulary *about* changed.

**The loss, stated plainly: the authored document no longer round-trips byte for byte.** ADR-0013 §1
promised the store returned what you stored — your YAML, comments and key order intact. A custom
resource is a decoded, re-serialized object, so `PutSpec` followed by `GetSpec` returns an *equivalent*
document, not the same bytes. Comments do not survive. This is real, and it is accepted because the
document store the project recommends is your own git repository, where byte fidelity is git's job and
always was. `kelson-server` becomes what it should have been: an API over cluster state, not a home for
a file.

> **Transition (R1/R2, [#224](https://github.com/dafrie/kelson/issues/224) /
> [#225](https://github.com/dafrie/kelson/issues/225)).** Today the specs and the deployment history
> are still ConfigMaps in `internal/serverstate`, and the server still calls a delivery adapter to
> apply. Both are deleted by [ADR-0027](adr/0027-crd-native-control-plane.md) decision 7 and
> [ADR-0028](adr/0028-delivery-spine.md) decision 9. The **agent identity and audit records are not** —
> they are control-plane records rather than delivery state, and they relocate unchanged to
> `internal/controlstore` so the package name stops implying they are the server's memory of a
> deployment. Whether they should also become custom resources is
> [#233](https://github.com/dafrie/kelson/issues/233), not a decision either ADR took.

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
| `--namespace` | — | `kelson-system` | Where kelson's custom resources and control-plane records live. |
| `--keep` | — | 20 | Deployment revisions retained per environment — the bound on the history mirror in `Environment.status`. |
| `--audit-retention` | — | 30 | Days of audit records retained (maximum 120). `0` turns the trail off. See [The audit trail](#the-audit-trail). |
| `--registry` | `KELSON_REGISTRY` | unset | Destination registry for builds, e.g. `ghcr.io/acme`. See [build](build.md). |
| `--push-secret` | — | unset | Name of an existing `kubernetes.io/dockerconfigjson` Secret authenticating the push. |
| `--build-namespace` | — | the environment's namespace | Where build Jobs run. |
| `--insecure-registries` | `KELSON_INSECURE_REGISTRIES` | unset | Comma-separated registry hosts served over plain HTTP, e.g. `localhost:5000`. Exactly these; never a request's. See [build](build.md#plain-http-registries). |

`/healthz` reports liveness plus the build (`version`, `commit`) and never requires authentication —
a probe holds no credential, and a server that failed its liveness check because nobody logged in
would be restarted forever.

## It also serves the web UI

The same listener serves the API and the web UI (`ui/`). Open `http://127.0.0.1:8420/` and you get
the application; in a cluster, `kubectl port-forward` at the Service is the whole of "install it and
click around" — see [installing kelson](install.md) and `deploy/chart/kelson/README.md`.

One origin is the point. The UI's base URL is `/` and its session is a cookie, so serving it from
the process that answers `/kelson.v1alpha1.*` means there is no CORS to configure, no second
hostname and no cross-site cookie — in development `ui/vite.config.ts` proxies the API to
manufacture the same property, and in production it is simply true.

**The API keeps precedence.** `/kelson.v1alpha1.*`, `/auth/*` and `/healthz` are answered by their
own handlers, and a path under those prefixes that no handler claims is a 404 rather than a page —
a ConnectRPC client must never be handed HTML to decode. Everything else that is not a file resolves
to `index.html`, so reloading a deep link like `/projects/shop/environments/production` hands the
browser the application instead of a 404 it cannot route. Content-addressed assets are cached
immutably; `index.html` is not, because its name outlives its contents.

**It is served unauthenticated**, exactly as the Vite dev server serves it: the login form is part
of the application, so putting it behind the login would be a loop. The assets are public; every
route that can touch the cluster is not.

### Where the UI comes from, and the page that says it is missing

`go:embed` needs a directory that exists when the compiler runs, `ui/dist` is a build artifact this
repository does not commit, and `go build ./...` must work on a clone with no Node installed. So
`make ui` builds `ui/` and copies the result into `internal/webui/static/` before compilation, and
the release workflow does the same before goreleaser — released binaries and the
`ghcr.io/dafrie/kelson-server` images carry the real UI.

A binary compiled without that step — `go build ./cmd/kelson-server` on a fresh clone — carries a
committed placeholder page instead. It serves at `/`, says the UI was not built into this binary
and how to get one, and the startup banner says the same thing:

```
kelson-server v0.1.0 serving the kelson.v1alpha1 schema on http://127.0.0.1:8420 (namespace
kelson-system, authentication: shared password, plus agent identities, audit trail: 30 days,
web UI: not built into this binary (`make ui`))
```

`make server` builds both halves. The placeholder is deliberately not named `index.html`: the UI
build writes an `index.html` into that directory, and a tracked one would be overwritten on every
build and committed by accident.

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
No request payload is logged. This line is the *volatile* half of attribution; the durable half is
[the audit trail](#the-audit-trail) below.

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

**It is enforced on the server, from the stored spec.** kelson reads the policy off the stored
`Environment` a request acts on — never out of the request. An agent that sends its own
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

It does **not** open a pull request yet, and after [ADR-0028](adr/0028-delivery-spine.md) it has no
half-built path to one: the git writer is deleted with the rest of the git transport, and opening a pull
request needs a forge credential the control plane deliberately does not hold
([ADR-0009](adr/0009-secrets.md)). The proposal a human reviews is the change to the Project or
Environment document in your own repository. ADR-0025 §7 records the gap.

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

## The audit trail

`kelson audit` answers "what did it actually do?" — the question that makes agent operation
reviewable after the fact ([#78](https://github.com/dafrie/kelson/issues/78),
[ADR-0026](adr/0026-agent-audit-trail.md)). It is free and always on, per
[ADR-0004](adr/0004-licensing.md).

```console
$ kelson audit --since 24h --project shop
2026-08-14T09:41:02Z  allowed  agent:deploybot  DeployService.Deploy
  target=shop/production  revision=rev-00000007  4 resources applied  kinds=Deployment,Service
  scope: projects=shop environments=production operations=mutate
  reason: rolling out the checkout fix from PR 412

2026-08-14T09:38:55Z  refused  agent:reporter   SecretService.SetSecret
  target=shop/production  code=auth/out-of-scope
  scope: projects=shop operations=read
  agent identity "reporter" is not granted the mutate operation class

window: the store retains 30 days, back to 2026-07-16; oldest record in range 2026-08-13T06:02:11Z
this window is complete: nothing inside the range queried was dropped.
```

Filters: `--project`, `--env`, `--agent`, `--principal`, `--procedure`, `--outcome`, `--since`.
`--export jsonl` writes every matching record as one JSON object per line, for a pipeline.

### What it records

**Every mutation and every refusal.** Allowed *reads* are not recorded: a status poll changes
nothing, and an agent polling every two seconds would fill the store's daily bound in about an hour,
evicting the mutations the trail exists for. If you need read auditing, the Kubernetes API server's
own audit log already sees every read kelson makes on your behalf.

Each record names the principal and its type, the credential's scope *as it was when it acted*, the
procedure, the target project and environment, the outcome (`allowed`, `refused` or `failed` — the
third is "allowed, and then it broke"), the refusal or failure code, the dry-run rung, the revision
the change produced, a bounded summary of what it touched, and the reason the caller stated.

**The revision is the pointer to the diff**, not a copy of it: `kelson rollback --dry-run` and
`DeployService.History` reconstruct the exact manifests from it. A second unbounded copy per record
would exhaust the store in one deploy.

**Nothing secret is in a record.** There is no request payload, and the two free-text fields go
through kelson's redaction registry twice on the way in
([#117](https://github.com/dafrie/kelson/issues/117)).

### Where it lives, and what that costs

ConfigMaps in the state namespace, one per UTC day, and
`kubectl get configmap -l kelson.dev/state=audit -o yaml` reads it when kelson-server itself is what
you are investigating. `kelson audit` talks to the cluster directly for the same reason `kelson
agent` does: the authority is your kube context, and it still works when the server is down.

These records **stay ConfigMaps** through the CRD rebuild, and move with the agent identity store to
`internal/controlstore` ([ADR-0027](adr/0027-crd-native-control-plane.md) decision 7). They are
control-plane records — who a principal is, what a principal did — not delivery state, and neither the
controller nor Flux has any interest in them; an audit ring also wants a fixed-size buffer more than it
wants a typed API. Making them custom resources is
[#233](https://github.com/dafrie/kelson/issues/233), and it has to argue that case first.

A ConfigMap is a poor append log and kelson does not pretend otherwise. **The day is a ring**: past
2000 records or 768 KiB, the oldest of that day are dropped, and the count of what was dropped
travels with every query result. **Every answer states its window** — the retention boundary and
whether anything inside the range queried was lost — whether or not anything was, because a reader
who never sees the horizon cannot know it exists. Longer retention is an external sink, which does
not exist yet; a longer ring would be a promise the storage cannot keep.

An audit write that fails does **not** fail the request it was recording — an audit trail that could
take a deployment down would be a new way to take a deployment down. It is counted and logged at
`ERROR` on stderr with a running total, so a hole in the trail is loud even though it is not fatal.

### Reading it is administrative

`AuditService.QueryAudit` is refused to every agent credential whatever its scope. An agent that could
read the trail could read what its reviewer is about to see, and the trail spans every project by
construction, so there is no scoped version of the answer that would be safe to serve. The way an
agent explains itself is the `reason` it supplies on the way in — the mutating MCP tools all take
one, and it lands in the record beside the action.

### What it is not

Not tamper-evident: anyone who can write ConfigMaps in the state namespace can edit it, so the RBAC
on that namespace is the boundary. Not a record of unauthenticated attempts — those are refused by
the HTTP gate before any interceptor runs, and there is no principal to attribute them to. Not in
the web UI yet.

## The delivery grant is opt-in, and cluster-wide

> **Transition (R2/R3, [#225](https://github.com/dafrie/kelson/issues/225) /
> [#226](https://github.com/dafrie/kelson/issues/226)).** This whole section describes the grant the
> **direct adapter** needs, and the direct adapter is being deleted
> ([ADR-0028](adr/0028-delivery-spine.md) decision 9). Under the spine the server applies no workload
> at all: the controller publishes an artifact and writes two Flux objects in `kelson-system`, and
> kustomize-controller applies your manifests under **Flux's** RBAC. What the server and the controller
> each need afterwards — and what shrinks — is install work tracked with R3. Until then, everything
> below is what a chart-installed server actually needs to deploy.

A default `helm install` gives the server no way to apply anything. That is deliberate — the
installer does not hand itself broad write on your say-so — but it means a server installed the
documented way and then asked to deploy fails on its first click:

```
not permitted to apply this resource: namespaces "podinfo-development" is forbidden:
User "system:serviceaccount:kelson-system:kelson" cannot patch resource "namespaces"
```

The fix is one value, `rbac.createDeployClusterRole=true`, which renders a `ClusterRole` and a
`ClusterRoleBinding` for the server's service account.

### Why it cannot be a Role per namespace

That was the earlier answer and it does not work, for two reasons that are not going away:

- **The renderer emits the environment's `Namespace`**
  ([#150](https://github.com/dafrie/kelson/issues/150)), and `internal/delivery/direct` reads it
  immediately before the apply and PATCHes it with `kelson.dev/namespace-ownership` — the record
  of whether kelson created the namespace or adopted one that predates it, which is the only thing
  `kelson uninstall` accepts as licence to delete it. A namespace is cluster-scoped, and no
  namespaced Role can grant a verb on a cluster-scoped object.
- **The web UI deploys into a namespace that does not exist yet.** Creating an application targets
  `<project>-<environment>`, brought into existence by that first apply. `rbac.targetNamespaces`
  is read when the chart is installed, so it cannot name it. The same argument covers everything
  one step later: the pod and Deployment reads behind the first status verdict, the log stream the
  UI opens, and the Secrets `SecretService` writes all happen in that same new namespace.

### What it grants, and what bounds it

Named apiGroups, resources and verbs only — no wildcards. Every kind `internal/renderer` emits
gets `get, list, create, patch, delete`: `patch` plus `create` is the server-side apply, `get` is
the status read-back, `list` plus `delete` is the prune. `namespaces` get everything but `delete`,
because pruning excludes them and no RPC removes one. `update` and `watch` appear nowhere — the
delivery plane server-side applies and polls. `deploy/chart/kelson/templates/clusterrole-deploy.yaml`
gives the reason for each rule, and a test in `deploy/chart` derives the kind list from the
renderer's own golden files so a new rendered kind fails the build until its rule exists.

What bounds it is **not RBAC**. It is write access to every namespace in the cluster, so anything
that can authenticate to the server can deploy anywhere: the cluster sees one service account with
one grant and cannot tell one caller from another. The fences that do exist are the ones above in
this document — the shared password, the per-identity agent scopes ([#74](https://github.com/dafrie/kelson/issues/74),
[ADR-0024](adr/0024-agent-identities.md)), and the per-environment agent policy
([ADR-0025](adr/0025-agent-policy.md)). Least-privilege RBAC for this path, including what a
per-project or per-environment identity would look like, belongs to
[#84](https://github.com/dafrie/kelson/issues/84) with the rest of the threat model.

Two honest limits beyond that. `spec.overlays` can emit **any** kind, so a spec that overlays a
`PodDisruptionBudget` fails with a plain `forbidden` naming it — bind a ClusterRole of your own
alongside this one for those. And a Flux-backed environment does not need this value at all: there
kelson publishes an artifact and Flux does the applying, under Flux's RBAC — which is what every
environment becomes once R1 lands.

`hack/local/up.sh` (`make kind-up`) sets the value, because a throwaway kind cluster on your own
machine is the one place where that trade is obviously right.

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
lands, run the server with a service account scoped to the namespaces it is meant to serve —
`rbac.targetNamespaces` is how the chart does that. Note that the delivery grant above supersedes
the scoping: `rbac.createDeployClusterRole=true` carries the same Secret verbs cluster-wide,
because the namespace a deploy creates cannot be listed in advance. Setting it means accepting the
Secret grant in every namespace too.

## PreviewService reads flux-operator's objects, and one read it cannot be granted

`PreviewService.ListPreviews` ([ADR-0017](adr/0017-pr-previews.md)) reports which pull requests of an
environment are running. It reads four kinds in the **environment's own** namespace — the
`ResourceSetInputProvider` and `ResourceSet` kelson renders, and the per-change-request `OCIRepository`
and `Kustomization` flux-operator instantiates from them — and the chart grants `get` and `list` on all
four in every namespace it serves. Nothing is granted for writing: previews are published by CI and
torn down by flux-operator, and no RPC in this schema creates or deletes one.

The exception is hostnames. A preview's HTTPRoutes live in the preview's own namespace,
`<project>-<environment>-pr<id>`, which is created at reconcile time and cannot be listed in
`rbac.targetNamespaces` when the chart is installed. So that read needs cluster scope. Without it
the preview list is complete except for its hostnames — the read fails soft rather than failing the
call.

`rbac.createDeployClusterRole=true` covers it, because that grant is cluster-wide and HTTPRoute is
a kind the renderer emits. If you do not want the delivery grant, bind just this instead:

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
- `internal/webui` — the embedded web UI, the SPA fallback and the placeholder.
- `cmd/kelson` — `kelson agent create|list|revoke` (`agent.go`), which writes to the cluster.
- `internal/api` — the ConnectRPC handlers (`api.go`), the credential gate (`auth.go`), the principal
  (`principal.go`), the RPC-to-scope table (`scope.go`), the authorization interceptor (`authz.go`)
  and the per-identity budgets (`ratelimit.go`).
- `api/kelson/v1alpha1` — the public CR types (`Project`, `Environment`, their status structs and
  generated deepcopy) the façade reads and writes. The spec structs themselves stay in
  `internal/model` ([ADR-0027](adr/0027-crd-native-control-plane.md) decision 3).
- `internal/controlstore` — the audit ring (`audit.go`) and the Secret-backed agent identity store
  (`agent.go`). *Transitional:* both still live in `internal/serverstate` beside the ConfigMap spec and
  history stores that ADR-0027 deletes ([#224](https://github.com/dafrie/kelson/issues/224)).
- `internal/api/audit.go` — the audit capture points and `AuditService`; `cmd/kelson/audit.go` — the
  `kelson audit` command and its JSONL export.
- `internal/secret` — the cluster secret backend behind `SecretService`.
- `internal/delivery/flux` — the preview read behind `PreviewService` (`previews.go`).
