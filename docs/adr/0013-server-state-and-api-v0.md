# ADR-0013: Server state lives in the cluster; API v0 shape

- **Status:** Proposed (amended 2026-08-13: `EventService` added as the sixth service — the watch
  stream of [#76](https://github.com/dafrie/kelson/issues/76). It adds no new state and no new seam;
  it observes through the same DeliveryConnector and health evaluator the Status RPC uses, so §1's
  "the process holds no state" is unchanged: the retained event window is a cache a restart is
  allowed to lose, and says so with a Resync.)
- **Date:** 2026-08-13

## Context

[ADR-0002](0002-tech-stack.md) decided the transport (ConnectRPC, one schema producing gRPC and
HTTP/JSON) and [ADR-0008](0008-mcp-surface.md) decided the parity rule (capability parity, task-shaped
MCP tools on top). Neither decided two things #139 cannot proceed without:

1. **Where server state lives.** The direct adapter's history journal is local-filesystem JSONL
   (`internal/delivery/direct/history.go`) — right for a CLI, wrong for a multi-client server. The
   review that produced #139 flagged this as needs-ADR. The same question applies to specs: a server
   that "serves existing capabilities" could stay stateless and take specs in every request (the CLI's
   `-f` model over HTTP), or own a spec store and become the source of truth clients converge on.

2. **The v0 surface.** Which capabilities the first server release exposes, and what its trust model is
   while the threat model (#84) and agent identities (M7) do not exist yet.

Decisions taken with the project owner, 2026-08-13: the server **stores specs** (not stateless),
history is **in-cluster from day one** (no local server state), the v0 surface is the **full working
set** (render, diff, profile, deploy, status, rollback, log streaming), and v0 auth is **loopback-only,
no authentication**.

## Decision

### 1. All server state is cluster state; the server itself is stateless

Specs and deployment history are stored as **ConfigMaps in the namespace the server runs against**
(default `kelson-system`, flag-configurable), carrying the standard kelson provenance labels. The
`kelson-server` process holds no state a restart or a second replica would lose or fork.

**Not a CRD.** [ADR-0003](0003-install-model.md)'s additive-install doctrine means the server must work
against a cluster where kelson has installed nothing: ConfigMaps need no CRD registration, no
admission wiring, and plain namespaced RBAC (`get/list/watch/create/update/delete configmaps` in one
namespace) is the entire permission footprint. A CRD-based store is the natural successor **when the
controller exists** (M13+) and is explicitly the migration target recorded here — the store is behind
an interface precisely so that swap does not touch handlers.

**Optimistic concurrency is Kubernetes resourceVersion.** #69 requires optimistic concurrency on spec
version. Rather than inventing a version counter and a store to keep it in, spec writes carry the
ConfigMap's `resourceVersion` through the API as an opaque `version` string; a conflicting write fails
server-side apply the same way any Kubernetes conflict does and surfaces as a structured
`store/version-conflict` error. What Kubernetes already guarantees, kelson does not reimplement.

**Layout.**

- Spec store: one ConfigMap per project — `kelson-spec-<project>` — holding the authored YAML
  documents verbatim (`project.yaml`, one key per environment). The server stores what the user wrote,
  not a normalized form: the spec is the user's document, round-trips must be byte-faithful, and
  validation happens on write with the full structured-error taxonomy (`schema/*` codes, line/column
  positions).
- History store: one ConfigMap per revision — `kelson-hist-<project>-<env>-<revision>` — containing
  the journal record (JSON) and the rendered manifests (gzipped, base64). Labels
  `kelson.dev/project`, `kelson.dev/environment`, `kelson.dev/revision` make listing a label query.
  Retention mirrors the JSONL store (`DefaultKeep = 20`, newest kept, pruned on append). A rendered
  set that cannot fit a ConfigMap after compression (1 MiB etcd limit) fails the deploy with a
  structured error rather than storing a truncated history — a history that lies is worse than none.

**The seam.** `internal/delivery/direct` gains a `History` interface (the six methods of today's
`*Store`: Append/List/Latest/Get/Rendered/NextRevision); the JSONL `Store` keeps implementing it for
the CLI, and the cluster-backed implementation lives with the other cluster-facing code. The CLI
changes not at all: local JSONL remains correct for a single-user tool. The CLI and the server
therefore have **different history stores by design** — deploys made through one are not visible in
the other's history. That is honest (they are different actors) but worth revisiting when agent
identities (M7) give deploys an attributable author regardless of entry point.

### 2. v0 API surface: the full working set, one schema, structured errors on the wire

Six services under `kelson.v1alpha1` (details in `proto/kelson/v1alpha1/`):

| Service | RPCs | Notes |
|---|---|---|
| `SpecService` | PutSpec, GetSpec, ListSpecs, DeleteSpec | The store; Put validates and returns structured errors; optimistic concurrency via `version` |
| `RenderService` | Render, Diff | Pure calls; spec by store reference **or** inline documents (the CLI's `-f` mode remains a peer) |
| `ProfileService` | GetProfile | Live capture against the server's cluster; gaps are data, not warnings on a side channel |
| `DeployService` | Deploy, Status, Rollback, History | Deploy/Rollback are server-streaming: each state-machine transition is an event; the final event carries the settled state |
| `LogService` | Query, Follow | First consumer of `observation/logquery`; Query is bounded (the engine's `requireBound`), Follow is the deliberate unbounded stream |
| `EventService` | Watch | Server-streaming watch over (project, environment) scopes (#76). Cursors resume within a bounded in-memory window; past it the server sends `Resync` (relist via Status) rather than pretending to durable history |

**Every mutating RPC** (PutSpec, DeleteSpec, Deploy, Rollback) carries `dry_run`
(`DRY_RUN_UNSPECIFIED | NONE | RENDER | SERVER`) and `idempotency_key`, per #69. Idempotency in v0 is
scoped honestly: keys are recorded in the state ConfigMaps' annotations and a replayed key returns the
recorded outcome; there is no distributed dedup beyond what one namespace's ConfigMaps provide.

**One wire error shape.** The three plane vocabularies (`model.Error`, `renderer.Error`,
`delivery.Error`) map onto a single `kelson.v1alpha1.Error` message — the union of their fields
(code, resource, field, application, overlay, target, message, remediation, docs_url, line, column,
cause) — attached as ConnectRPC error details. Codes pass through verbatim (`schema/not-implemented`,
`image/unresolved`, `delivery/apply-failed`, …) plus a small `store/*` vocabulary for the state layer
(`store/version-conflict`, `store/not-found`, `store/too-large`). Agents branch on codes; the wire
must not invent a second taxonomy.

**Streaming is ConnectRPC server-streaming.** Deploy events are `statemachine.State` snapshots
(phase, stuck, cause, answer) — the same transitions the CLI prints, so `kelson deploy` and a future
UI cannot disagree about what a phase means (issue #37's rule, now enforced at the API boundary).

### 3. v0 trust model: loopback, no auth, no pretense

The server binds `127.0.0.1` by default and refuses a non-loopback `--listen` without an explicit
`--insecure-bind` flag. No authentication, no TLS, in v0. This is not a security posture; it is the
absence of one, stated loudly, gated to localhost, and tracked by the threat model (#84) which owns
deciding the real answer (mTLS, tokens, or K8s TokenReview). The schema reserves nothing for auth —
retrofitting an auth header does not break a Protobuf contract.

### 4. Codegen follows the repo's committed-and-drift-tested convention

`buf` generates Go (`protoc-gen-go` + `protoc-gen-connect-go`) into `internal/api/gen/`; generated
code is **committed**, like `schema/*.json` and `docs/reference/*`, with a drift test guarding it.
`buf breaking` joins CI when the schema first ships in a release (the compatibility promise starts at
the first tag that serves it, not before). The TypeScript client is deliberately **not** generated in
v0 — `buf.gen.yaml` carries the commented config so M6 turns it on rather than designs it.

The depguard fence extends rather than loosens: a new `api` rule for `**/internal/api/**` and
`**/cmd/kelson-server/**` allows `connectrpc.com/connect`, `google.golang.org/protobuf` and
`net/http` alongside stdlib and kelson itself; the cluster-backed stores live under the existing
cluster-facing rule's paths. The `main` rule — and with it the renderer's and model's isolation —
is untouched.

## Rationale

**Stateless server over stateful.** Every alternative for history (embedded DB, server-local JSONL)
makes the server a single point of state that backup, HA and multi-replica stories then have to
solve. The cluster is already the system of record for everything kelson deploys; making it the
system of record for what kelson *did* means `kubectl get configmaps -l kelson.dev/project=x` is the
audit trail, RBAC is the access control, and killing the pod loses nothing.

**ConfigMaps over CRD now, CRD later.** The strongest argument for a CRD — typed schema, watchable,
kubectl-native — is also an argument for the controller that does not exist yet (#133 was deferred on
exactly this ground). Choosing ConfigMaps now and CRD-behind-the-same-interface later costs one
interface; choosing CRD now costs an install step that [ADR-0003](0003-install-model.md) promises not
to require.

**Spec store returns what you stored.** Storing raw documents (not parsed state) keeps Git mode
coherent: in Git mode the repository stays the source of truth (ADR-0001), and a server-stored spec
is a working copy the API validated — the delivery adapter still commits to Git. The two-homes
problem ADR-0002 rejected for CRDs would reappear if the server normalized specs into its own model.

## Consequences

**Positive.**
- A `kelson-server` restart or second replica needs no migration, no volume, no leader election for v0.
- Optimistic concurrency, listing, retention and audit come from Kubernetes primitives, not new code.
- The CLI keeps working offline-ish (local JSONL, no server required) — the server is additive.
- One error taxonomy end to end; agents branch on the same codes at every entry point.

**Negative.**
- CLI-made and server-made deploy history are separate stores; nothing merges them in v0.
- ConfigMap size bounds rendered-history retention; very large rendered sets degrade to
  fail-loudly-on-append. The CRD successor inherits the same etcd limit — the real fix (external blob
  store) is deliberately out of scope until something needs it.
- Idempotency keys are namespace-scoped annotations, not a real dedup store; two servers pointed at
  different namespaces do not share them.
- No auth in v0 means the server must not be exposed beyond loopback until #84 lands — a documented
  sharp edge.

## Revisit when

The controller exists (CRD store supersedes ConfigMaps behind the same interface); #84 decides authn/z;
M6 turns on TypeScript client generation; or real usage shows spec round-tripping through ConfigMaps
hits the size or update-frequency limits of etcd.
