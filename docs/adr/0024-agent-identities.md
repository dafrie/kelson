# ADR-0024: Agent identities — scoped, expiring credentials that are principals

**Status:** Accepted
**Date:** 2026-08-14
**Issue:** [#74](https://github.com/dafrie/kelson/issues/74)
**Refines:** [ADR-0013](0013-server-state-and-api-v0.md) §3 (the interim auth of #84)

## Context

kelson-server's only credential is a shared password ([ADR-0013](0013-server-state-and-api-v0.md) §3,
amended 2026-08-13). It answers one question — may this caller reach the server — and it cannot
answer any of the three that matter once agents are the callers:

- **Attribution.** Every action taken with the password looks the same in every log. "Who deployed
  to production at 03:41?" has no answer, which makes the audit trail of
  [#78](https://github.com/dafrie/kelson/issues/78) impossible to build on top of it.
- **Scope.** An agent gets exactly the human's access, because it *is* the human's access. There is
  nothing to narrow.
- **Revocation.** Cutting off one agent means changing the password, which cuts off every human and
  every other agent at the same time.

An agent is a principal. It needs a credential of its own.

## Decision

**An agent identity is a first-class principal, stored in the cluster, with a scoped, expiring bearer
credential; scope is enforced server-side in a ConnectRPC interceptor driven by one exhaustive
method-to-scope table.**

### 1. The principal model

`serverstate.Agent` is `{name, created, expires, revoked, revokedAt, scope, limit}`. One Kubernetes
Secret per identity in the state namespace, labelled `kelson.dev/state=agent`, typed
`kelson.dev/agent-identity`.

A Secret rather than the ConfigMap the specs and history use. What is stored is *not* a credential —
it is a salted HMAC and no token can be recovered from it — but identity material belongs behind
whatever RBAC an operator puts on Secrets, and the server already needs Secret permissions for
[ADR-0009](0009-secrets.md)'s cluster backend, so the tighter home costs nothing. The grant is
`get, list, create, update` in one namespace. No `delete`: a revoked identity is kept, because an
audit trail that cannot name the principal behind a past action is not one.

Everything about an identity is cluster state, so ADR-0013 §1 still holds: the process forgets
nothing on restart and a second replica forks nothing.

### 2. The credential

The token is `kagt.<name>.<base64url(32 random bytes)>`, returned exactly once, by the call that
minted it. The server stores `HMAC-SHA256(per-identity random salt, secret)` and compares in constant
time. A lost token is replaced, never recovered.

**Why HMAC-SHA256 and not argon2 or bcrypt.** A password KDF buys resistance to offline guessing. A
token is 256 bits from `crypto/rand`, so there is no dictionary and no guessing attack to resist.
What the KDF's cost *would* buy is a deliberate CPU burn on every single request — revocation has to
be immediate, so every request re-reads the identity and re-verifies the credential with no cached
allow. A per-identity random salt gives no shared precomputation across identities, and the compare
is `hmac.Equal`. The rejected alternative (argon2id at any honest parameter set) would have made a
per-request check cost milliseconds of CPU, which is a denial-of-service surface handed to anyone who
can send a wrong token.

The name travels in the token so authentication is one `Get` rather than an HMAC against every stored
identity. A forged name buys nothing — the secret is what proves the claim — and every authentication
failure returns one identical error, because telling "no such identity" from "wrong secret" is an
oracle for enumerating identities.

The value is registered with `internal/redact` the moment it is minted
([#117](https://github.com/dafrie/kelson/issues/117)), so from before any caller sees it, it cannot
reach a log line, an error message or an error detail.

### 3. Scope: project, environment, operation class

`{projects[], environments[], operations[]}`. An empty `projects` or `environments` list means every
one. An empty `operations` list is **refused at issuance** rather than read the same way: two of the
three fields widen by omission and the third must not.

Operations are two classes, not a permission per RPC:

| Class | What it covers |
|---|---|
| `read` | status, history, logs, events, render, diff, preview listing, secret **listing** (names and keys; no RPC in the schema can return a value) |
| `mutate` | deploy, rollback, promote, build, spec writes, secret writes. Implies `read` |

A third class, `admin`, exists only as the label the AgentService methods carry. It cannot be granted
(see §5).

Two classes rather than a per-method matrix because an agent's blast radius is "can it change
anything", and a matrix is a policy language nobody could audit at a glance. Finer grain is the policy
engine of [#75](https://github.com/dafrie/kelson/issues/75).

### 4. Enforcement: one table, one interceptor, fail closed

`internal/api/scope.go` maps **every** registered RPC method to a row: its operation class, how far it
reaches, and — where it reaches a target — how to read the (project, environment) out of the request.
`TestEveryRegisteredMethodHasAScopeRow` walks the generated protobuf descriptors and fails by name for
any method without a row. This is the [#141](https://github.com/dafrie/kelson/issues/141) gate-table
discipline applied to authorization, for the same reason: the failure mode of an authorization table
is silence, and the natural implementation — a lookup with a permissive default — ships a new RPC wide
open. **A method with no row is refused to every caller.**

`internal/api/authz.go` is a ConnectRPC interceptor that `Server.Register` mounts itself, so no caller
can serve the handlers without it. Order: row exists → credential live (not revoked, not expired) →
not administrative → operation class granted → inside the request budget → target inside the scope.

The target lives in the request message. For a server-streaming RPC that message is not decoded until
the handler receives it, so the stream's connection is wrapped and the check runs on the first
`Receive`, before the handler has done anything with it. `Deploy` is server-streaming and is exactly
the method the acceptance criterion names, so this is the main path, not an edge case.

**Where the target is not knowable, the answer is no.** Three shapes cannot say what they act on:

| Shape | Example | Rule for a *restricted* credential |
|---|---|---|
| Inline spec | `Render`, `Diff`, `Deploy`, `Build`, `ListPreviews` with `SpecRef.documents` | Refused: the project is inside the YAML |
| No project field at all | `PutSpec` | Refused: same reason |
| Spans every project | `ListSpecs`, `Watch` with no scopes | Refused: serving it would leak other projects |

Refused, not filtered. A scope that silently narrowed a response would be indistinguishable from one
that did not apply. An *unrestricted* agent credential (naming no project and no environment) still
reaches all of them, because there is nothing left to check it against. `GetProfile` is cluster-wide —
it names no project in the question or the answer — and is allowed to any credential with `read`.

**Log queries are a documented approximation.** `LogService` addresses a namespace, and the mapping
from namespace to (project, environment) belongs to the resolved spec, which the interceptor
deliberately does not load. A namespace is therefore matched against the renderer's own convention,
`<project>-<environment>`, over every possible split. Consequences, stated plainly:

- An environment that overrides `spec.namespace` cannot be read by a scoped credential at all. That
  fails closed, which is the right direction.
- A namespace that *collides* with the convention of an allowed pair would be allowed. This is a real
  gap. The fix is to resolve the namespace through the spec store; it is not done here because it
  would put spec resolution on the authorization path of every log call.

An environment a targeted request did not name is refused for an environment-restricted credential
rather than read as "all of them" — the difference between a scope and a suggestion.

### 5. Issuance is human-only

`AgentService.CreateAgent/ListAgents/RevokeAgent` and `kelson agent create|list|revoke` are the two
surfaces. **Both refuse an agent credential**, enforced by the `admin` row in the scope table, and
`admin` cannot be granted at issuance either. An agent that could mint an agent could mint one wider
than itself, which would make every scope in this ADR advisory. There is no scoped delegation and no
"may create identities no wider than mine" — that is a second, subtler authorization system, and it
would be the only place in kelson where a credential could grow the credential set.

The CLI talks to the **cluster**, not to kelson-server. Its authority is the operator's kube context
and the RBAC on the state namespace, which is the same authority `kelson deploy` and `kelson secret`
already rely on. That makes issuance the one operation whose authority does not come from kelson at
all: someone holding only the server password cannot mint an identity, and the cluster's own audit log
records who did.

**Rotation is create-then-revoke.** There is no `rotate` verb: revoking before the successor is in
place breaks the agent, and revoking after needs to know when "after" is, which is the operator's
knowledge. TTL defaults to 24h and is capped at 30d. A request above the cap is **refused, not
clamped** — an operator who asked for a year must learn they did not get one.

### 6. Rate limiting: per identity, in memory, honestly

A token bucket per identity name, `requests_per_minute` and `burst` set at issuance (defaults 120 and
30). A breach is ConnectRPC `resource_exhausted`, never `permission_denied`: an agent that read a
budget refusal as permanent would give up on work it could do a second later, and one that read a
scope refusal as retryable would spin.

**The buckets live in this process.** Two replicas would each grant the full budget, so the effective
limit is per replica. That is the truth for a v0 server and it is written down rather than papered
over; a shared limiter is a store round trip per request in exchange for an exactness nothing yet
needs. A restart refills every bucket at once, which is the safe direction — a restart never makes a
limit *stricter* than the operator asked for.

### 7. Attribution

Every authenticated request leaves one structured line naming the principal (`agent:deploybot`,
`human:ada`, `human`, `anonymous`), the principal type, the method and the outcome. kelson-server
writes it as JSON to stderr. It carries no request payload: the one RPC that receives secret values
would put them there.

This is the **seam** [#78](https://github.com/dafrie/kelson/issues/78) builds on, not the audit trail.
There is no queryable store, no retention and no tamper evidence here.

### 8. The human path is untouched

An agent token is a different header value (`kagt.` prefix), a different lookup and a different
principal type. A password caller is not scoped, not rate-limited and not attributed to an identity —
[#84](https://github.com/dafrie/kelson/issues/84)'s posture, unchanged. Revoking an agent writes one
object and cannot lock a person out.

Agent tokens are honoured even on a server started without a password. That is not a security boundary
— a caller there can simply omit the header and be anonymous — but it means an agent's scope behaves
identically in a local dev server and in production, which is where scope bugs would otherwise hide.

## Consequences

**Positive**

- The issue's acceptance criterion is a test: a credential scoped to `development` cannot mutate
  `production`, refused server-side before the handler runs, and revocation takes effect on the very
  next request with no cache to wait out and no human affected.
- A new RPC cannot ship unauthorized. It fails the coverage test and, if merged anyway, is refused.
- The MCP server can act as a principal: issue a token, set `KELSON_AGENT_TOKEN`, and every call it
  makes is attributed and bounded.
- Identity material never leaves the cluster in a readable form, and a token cannot be logged.

**Negative**

- **A scoped credential loses real capability.** `ListSpecs`, inline specs and `PutSpec` are all
  refused to it. An agent that wants to write a spec must be unrestricted by project, which is a
  blunter grant than anyone would like. Narrowing this needs the request shapes to carry a project
  name — a schema change, deliberately not made here.
- **The namespace approximation has a collision case** (§4), and it is a real one.
- **Two operation classes are coarse.** "May deploy but not write secrets" is not expressible.
- **Rate limits are per replica**, so the number an operator sets is not the number the system
  enforces once there are two.
- **Attribution goes to stderr and nowhere else.** Without #78 there is no way to query it.
- **The UI does not know about any of this.** No issuance screen, no identity list; the TypeScript
  client is generated and unused. Follow-up.
- **A `kagt.`-prefixed token that is not one is refused with a distinct message** from a wrong
  password. That is deliberate — an operator debugging must not be sent to the wrong credential — and
  it does tell an attacker that agent identities exist on this server. Which the docs say anyway.

**Neutral**

- ADR-0013 §3 is refined, not superseded: the shared password remains the human credential and #84
  still owns replacing it.
- The `admin` operation class exists in the Go vocabulary but not in the wire enum, because nothing
  may ever request it.
