# ADR-0026: The audit trail — one principal-typed record per mutation, in a bounded cluster ring

**Status:** Accepted
**Date:** 2026-08-14
**Issue:** [#78](https://github.com/dafrie/kelson/issues/78), [#12](https://github.com/dafrie/kelson/issues/12)
**Builds on:** [ADR-0024](0024-agent-identities.md) (the attribution seam), [ADR-0013](0013-server-state-and-api-v0.md) §1 (all server state is cluster state)

## Context

[ADR-0024](0024-agent-identities.md) made every request attributable: a principal, a scope, a
procedure, an outcome, one `slog` line per call. That is the seam, and it is not an audit trail.

A log line is lost on restart unless something is collecting stderr, cannot be queried, cannot be
exported, and carries nothing about what the request *did* — the interceptor writes its line before
the handler runs, so it knows the deploy was allowed and not that it produced `rev-00000007`
touching four resources.

[#78](https://github.com/dafrie/kelson/issues/78)'s acceptance criterion is sharper than "log
things": *every agent-initiated change is traceable to an identity, a scope and a resulting diff*.
And [#12](https://github.com/dafrie/kelson/issues/12) wants a general audit log for humans too. Two
trails maintained separately would disagree within a month.

This is also, per [ADR-0004](0004-licensing.md), free and always on. An audit trail behind a paywall
is a threat, not a feature.

## Decision

**One durable, principal-typed record per mutation and per refusal, captured in the ADR-0024
interceptor and enriched by the handler, stored as a bounded per-day ring of ConfigMaps in the state
namespace, queried and exported through one paged API whose answer always states what it could not
have covered.**

### 1. One trail, principal-typed — not an agent trail beside a human one

`AuditRecord.principal` is `{type, name}` where type is `agent`, `human` or `anonymous`. "What did
the agents do" is `--agent deploybot`; "what happened to production last night" is `--env
production`. One store, one query, one set of bounds.

The alternative — an agent trail and a general log — was rejected because they would drift. The
first mutation recorded in one and not the other makes both untrustworthy, and there is no test that
can catch it.

### 2. Storage: a bounded ring of ConfigMaps, one per UTC day

ADR-0013 §1 says the process holds no state a restart or a second replica would lose or fork, and an
audit trail that a restart forgot would be worse than none. So the records live where the specs and
history live: ConfigMaps in the state namespace, under the same provenance labels, readable with
`kubectl get configmap -l kelson.dev/state=audit -o yaml` — which matters precisely when kelson
itself is what is being investigated.

**A ConfigMap is a poor append log and this does not pretend otherwise.** etcd caps a value near
1 MiB, every append is a read-modify-write of the whole day, and a busy day churns one object. What
makes the shape honest:

- **One object per UTC day.** The write set is one object, retention is a delete of whole days, and a
  query reads only the days its range covers.
- **The day is a ring with a stated bound.** Past 2000 records or 768 KiB, the oldest records of that
  day are dropped and the count is written onto the object as an annotation, surviving the drop that
  produced it.
- **Every field is bounded on the way in and marked when shortened** (`…[truncated]`). That is what
  makes "2000 records a day" a bound on anything: a record of maximally oversized fields still cannot
  evict a day.
- **Retention is 30 days by default, 120 maximum**, `--audit-retention`, `0` to disable.

**Rejected: a PVC-backed JSONL file.** It is a better append log, and it costs the property
ADR-0013 §1 is built on — a second replica would fork the trail, a restart on a new node would lose
it unless the volume followed, and the chart would grow a PersistentVolumeClaim and a storage class
question for every installation, including the ones that never look at the trail. It is the recorded
migration target *behind an external sink*, not instead of one.

**Rejected: Secrets.** Nothing here is secret, and the size cap and churn are the same.

**The in-memory fallback is that there is none.** A server started with `--audit-retention 0` keeps
no trail and says so in its banner. There is no silent degradation to a process-local buffer that a
restart would drop, because a trail that exists sometimes is a trail nobody can reason about.

**The recorded successor**, when the ring stops being enough, is an operator-supplied external sink
behind the same two-method `AuditSink` interface. Longer retention is that, not a longer ring.

### 3. What is recorded: mutations and refusals, not allowed reads

- Every **mutation** (`OpMutate`) and every **administrative** call, whatever its outcome.
- Every **refusal**, whatever the operation class. A refusal is a security event and there are few of
  them.
- **Allowed reads are not recorded.** A status poll changes nothing, and an agent polling a
  deployment every two seconds would fill the ring's daily bound in about an hour — evicting exactly
  the mutations the trail exists for. Recording reads at lower detail was considered and rejected:
  the volume, not the size, is the problem.

The way out for an operator who genuinely needs read auditing is the external sink of §2, or the
Kubernetes API server's own audit log, which already sees every read kelson makes on their behalf.
This is stated rather than papered over.

**Outcomes are `allowed`, `refused` or `failed`.** Three rather than the two an authorization
decision has, because "the caller was allowed and the thing failed anyway" is the answer to "what did
it actually do?" as often as either of the others, and an `allowed` record that quietly meant "we let
it try" would be the most misleading value in the schema.

### 4. The record: what it carries, and the diff it points at

```
{time, id, principal{type,name}, scope, procedure, operation, target{project,environment},
 outcome, code, message, dryRun, dryRunSummary,
 change{revision, from, source, added, modified, removed, resources, kinds, maxRisk},
 reason, idempotencyKey}
```

- **`scope`** is the credential's scope *as it was when it acted*, as the same one-line summary the
  refusal messages use. Recorded rather than looked up later: an identity can be re-issued, and a
  record must say what was true then.
- **`change.revision` is the pointer to the resulting diff**, not a copy of it. It names the entry in
  the environment's rendered history, from which the exact manifests and any comparison against them
  can be reconstructed (`DeployService.History`, the rollback preview). Manifests are already stored
  once, under a name; a second unbounded copy per record would exhaust the ring in one deploy. For a
  spec write (`PutSpec`, `Promote`) the same field is the spec store's version, which is what a later
  `Diff` would compare against.
- **`change.source` says how to read the counts.** `diff` means they come from a computed comparison
  (a server-side dry run, a promotion's diff, a rollback's preview); `rendered` means no comparison
  was computed and the numbers describe the set that was applied — `resources` and `kinds` only.
  Reporting `0 added, 0 modified, 0 removed` for a real deploy because nothing computed a diff would
  be the worst kind of wrong, so the two claims are kept apart.
- **`reason` is the caller's own words** and is absent when none were given. kelson never invents
  one.

### 5. Reading the trail is administrative — an agent may not

`AuditService.QueryAudit` is filed under `OpAdmin` in the ADR-0024 scope table, beside `AgentService`,
and is therefore refused to every agent credential whatever its scope.

Two reasons, and the second is the load-bearing one. An agent that could read the trail could read
what its reviewer is about to see. And the trail spans every project by construction, so there is no
scoped version of the answer that would be safe to serve — a project-scoped credential asking for
"its own" records would still be asking a question whose honest answer requires reading everyone
else's.

The way an agent legitimately explains itself is the `reason` it supplies on the way *in*, not a read
on the way out.

The scope table's coverage test now carries a closed list of services that may be classed admin, with
the reason for each, so filing a new service that way stays a deliberate edit rather than something a
name match allows.

### 6. Capture: the interceptor opens the record, the handler enriches it, one write at the end

The ADR-0024 interceptor is the only place every request passes through, so that is where an
`auditEntry` is parked in the request's context. Authorization stamps a refusal onto it; the handler
enriches it with the revision, the diff summary, the dry-run rung and the target it alone can derive;
exactly one write happens when the call returns.

**One request is one record.** A handler that forgets to enrich produces a thinner record — never a
second one, and never none. Every authorization refusal funnels through a single exit
(`authorizer.refuse`), so a refusal cannot reach a caller without also reaching the trail, and the
record's `code` is the same code the caller received.

The write runs on a context detached from the request's (`context.WithoutCancel`) with a 5-second
budget, because the two cases where a record matters most — a cancelled deploy stream, a client that
hung up mid-call — are exactly the cases where the request's context is already dead.

**`reason` is a request header (`Kelson-Reason`), not a field on every mutating message.** It belongs
to the call rather than to any one operation; adding it to the schema would mean adding it to eight
messages and to every message added later, and the one that got forgotten would be the one that
mattered. The mutating MCP tools expose it as an optional `reason` parameter.

**A failed audit write never fails the request, and is never silent.** Failing a deploy because a
ConfigMap update conflicted would make the audit trail a new way to take a deployment down. So a
failure is counted (`auditor.Failures()`) and logged at ERROR with the call it belonged to and the
running total. A trail with a silent hole in it is worse than none; a trail with a loud hole in it is
a trail you can still reason about.

### 7. Nothing secret reaches a record

The record carries no request payload. Its two free-text fields — the caller's `reason` and a failure
`message` — go through `internal/redact` twice: once in the API layer at write time (after the
handler has run, so `SetSecret`'s values are already registered), and again in the store on the way
in. That is the same deliberate duplication `internal/api/secret.go` and `internal/secret` keep
between them, and it is what stops a future sink from being the one place that forgot (#117).

`SetSecret`'s record says which keys were written and how many. It cannot say what they were: there
is no field for a value, and the MCP tool's `reason` parameter says out loud never to include one.

## Consequences

**Good.**

- "What did it actually do?" has an answer that survives a restart, and it names an identity, a
  scope, a target and the revision the change produced.
- Agent and human actions are one queryable trail, so #12 and #78 are one implementation.
- No new RBAC, no new storage dependency, no chart surgery: it is the same verbs on the same resource
  in the same namespace the state stores already use.
- `kelson audit` reads the cluster directly, so the trail is readable when kelson-server is down —
  which is one of the moments you most want it.
- Truncation is impossible to miss: every answer states the retention window and whether anything
  inside the queried range was dropped, whether or not anything was.

**Bad, and accepted.**

- **The ring is small.** 2000 records a day, 30 days. A very busy installation will drop records, and
  will be told it did. The fix is an external sink, which does not exist yet.
- **Allowed reads are invisible.** "Which agent read production's secrets list?" is not answerable
  from this trail. It is answerable from the Kubernetes audit log, and that is the honest pointer.
- **An unauthenticated request leaves no record.** The HTTP gate refuses it before any interceptor
  runs, and it has no principal to attribute the attempt to. A brute-force attempt against the shared
  password is visible in the gate's own output and nowhere in the trail. Closing this needs the gate
  to record attempts, which is #84's territory.
- **Every append is a read-modify-write of one object.** Two replicas mutating concurrently will
  conflict and retry, bounded at three attempts, after which the record is lost — loudly. This is the
  cost of the storage choice and it is why the retry is bounded rather than infinite.
- **`change` for a plain apply is not a diff.** It is the applied set's shape plus the revision that
  points at the real one. Computing a true diff on every deploy would double the work of every
  deploy; the pointer is what the acceptance criterion actually needs and it says which kind of
  number it is carrying.
- **No UI.** The web UI shows nothing of this yet; the generated TypeScript client exists and is
  unused. That is follow-up work, not a decision.
