# ADR-0025: Per-environment agent policy — guardrails that live in the spec

**Status:** Accepted
**Date:** 2026-08-14
**Issue:** [#75](https://github.com/dafrie/kelson/issues/75)
**Builds on:** [ADR-0024](0024-agent-identities.md) (agent identities), [ADR-0001](0001-hybrid-state-model.md) (GitOps), [ADR-0013](0013-server-state-and-api-v0.md) §2 (structured errors)

## Context

[ADR-0024](0024-agent-identities.md) made an agent a principal with a scoped, expiring credential. A
scope answers *may this credential reach this (project, environment, operation class)*. It cannot
answer the question an operator actually asks about production:

> The agent is allowed to deploy. Is it allowed to deploy **here**, **unsupervised**, **at that
> size**, and **to that database**?

Those are properties of the environment, not of the credential. They must be the same for every
agent that reaches it — including one issued next month by someone who never read this ADR — and
they must be reviewable in the same place the rest of the environment is described. A credential
cannot carry them: it is minted once, by one person, and it is invisible to everyone reading the
spec afterwards.

`spec.policy` has existed in the model since [ADR-0006](0006-project-application-environment.md) and
has rendered nothing since [#141](https://github.com/dafrie/kelson/issues/141) gated it. This ADR
makes it real.

## Decision

**Agent policy is a declarative block on the Environment, resolved from the stored spec at
enforcement time, applied in the API handlers, and reported as structured errors that name the rule
that refused and the escalation path out.**

### 1. The schema

```yaml
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: production}
spec:
  project: shop
  policy:
    agents: propose-only        # allow | propose-only
    require: [dry-run]          # guards that must hold before an agent mutation applies
    maxReplicas: 5              # blast radius: the largest an agent may scale a workload
    protect: [db, sessions]     # components an agent may not remove or scale to zero
    forbid: [secret-set]        # operations refused to agents here
```

It also exists as `spec.defaults.policy` on the Project, under precedence rule P4 — the
Environment's block wins **whole**, and the two do not merge. `deployers` stays gated
([#141](https://github.com/dafrie/kelson/issues/141), milestone M11): it is about humans, and kelson
has no notion of a human subject beyond "holds the password".

**Why the spec and not server configuration.** Everything else about an environment is spec-shaped,
versioned with it and reviewable in the same pull request. A guardrail kept in a server flag would
be invisible to the person reading the environment, would not travel with a promotion, and would be
changed by whoever can restart the process rather than by whoever can approve a change to
production. Policy in the spec also means the GitOps mechanism of ADR-0001 already governs *changes
to the policy itself*.

### 2. Unset means "no narrowing" — and that is a change

The built-in default was `propose-only` while the field was inert. It is now `allow`, at both
levels: an environment with no `policy:` block, and a block that leaves `agents:` unset, narrow
nothing.

The reasoning is ADR-0024's. The credential *is* the grant: an operator has to run `kelson agent
create --allow mutate` and scope it before an agent can change anything at all. Policy narrows that
grant where an environment says so. Had the default gone the other way, upgrading kelson would have
silently revoked authority that was deliberately issued, in environments whose specs say nothing
about agents — and the refusal would have pointed the operator at a field they had never written.
Every restriction here is opt-in and therefore visible in the spec, which is the property that makes
it auditable.

The cost is stated plainly: **a production environment is not protected until someone writes the
block.** The mitigations that exist before it is written are the credential's own scope, its TTL,
and the fact that `mutate` must be granted explicitly.

### 3. Enforcement: in the handlers, against the stored spec

The scope check runs first, in the interceptor (ADR-0024 §4). Policy runs second, in the handler,
because three of the rules need something the interceptor does not have: the resolved spec
(`maxReplicas`, `protect`), the render L1 already performed (`require: dry-run`), and — for a request
carrying inline documents — the project name, which is inside the YAML that ADR-0024 deliberately
refuses to parse on the authorization path.

**The stored spec decides, never the request.** `Server.guard` resolves the effective policy for the
(project, environment) the request acts on by reading the spec store, whatever documents the request
carried. An agent that presents an inline spec declaring `agents: allow` for an environment kelson
holds as `propose-only` is refused: naming an environment is asking about the one kelson stores.
There is no field in any request that policy is read from, which is what makes "cannot be bypassed
by a modified client" a structural fact rather than a promise.

Against forgetting, the same discipline as everywhere else in kelson: `agentOperations` in
`internal/api/policy.go` is a table of every mutating RPC and the operation name `forbid:` knows it
by, `TestEveryMutatingMethodHasAPolicyOperation` pins it against the `mutate` rows of the scope
table, and `policyDrivers` in the test file drives every one of them through the real HTTP stack as
a propose-only agent. A mutating RPC that lands unguarded fails by name.

Two ordering decisions worth recording. A policy refusal is returned **before** the "this server was
not wired with that seam" answer, so a refusal never depends on how a particular server happens to be
configured. And a dry run is never refused: `RENDER` and `SERVER` change nothing and are exactly what
a propose-only agent is told to send instead.

### 4. The rules and their codes

| Rule | Code | Refuses |
|---|---|---|
| `agents: propose-only` | `agent-policy/propose-only` | every live mutation of the environment |
| `forbid: [op]` | `agent-policy/forbidden-operation` | the named operation |
| `maxReplicas: N` | `agent-policy/max-replicas` | a deploy whose resolved spec exceeds N |
| `protect: [c]` | `agent-policy/protected-resource` | removing c, or scaling it to zero |
| `require: [dry-run]` | `agent-policy/dry-run-required` | a deploy kelson could not dry-run, or whose dry-run says it would be rejected |
| (addressing) | `agent-policy/unaddressed` | a mutation that names no project and environment |
| (fail closed) | `agent-policy/unreadable` | a mutation whose stored policy could not be read |

Operations are one name per mutating RPC: `deploy`, `rollback`, `promote`, `build`, `secret-set`,
`secret-delete`, `spec-write`, `spec-delete`.

**The prefix is `agent-policy/` and not `policy/`** because `internal/diff` already owns `policy/*`
for admission-control findings ([#45](https://github.com/dafrie/kelson/issues/45)). Two taxonomies
under one prefix would make "policy refused it" ambiguous exactly where an agent is deciding what to
do next: a Kyverno rejection is retryable after fixing the manifest, an `agent-policy` refusal never
is.

Every refusal carries `field` — the spec path of the rule (`$.spec.policy.maxReplicas`) — plus the
value and the limit in the message. They travel as `kelson.v1alpha1.Error` details like every other
plane's errors, so `internal/explain` and any agent read them the same way they read
`store/not-found`.

### 5. `require: [dry-run]` is run by the server

kelson runs the dry-run itself, in the same request, on the same rendered set it is about to apply:
L1 is the render that already succeeded, L2 is the same preview engine `--dry-run=server` uses. A
dry-run that reports the change would be rejected refuses the deploy.

There is deliberately **no request field claiming a dry-run was performed**. Believing one would be
client-side enforcement wearing a server-side hat, which is the thing the issue's acceptance
criterion rules out. A server with no preview engine refuses rather than waving the deploy through:
an unsatisfiable requirement is not a satisfied one.

### 6. Escalation is the error

There is no approval queue, no request-for-approval object and no notification. The refusal names
what a human must do — run the same operation with their own credential, or relax the rule on the
stored Environment — and, for `propose-only`, how to produce the proposal a human would review:
re-send with `dry_run=RENDER`, or call `Diff`.

Approval machinery is a genuine feature with a queue, a lifetime, a notification path and its own
authorization questions. Shipping a half-built one would be worse than shipping none: an agent
waiting on an approval nobody can see is stuck in a way that looks like a bug.

### 7. What `propose-only` concretely does today, per delivery mode

The issue's insight is that an agent proposing a change and a human opening a pull request travel the
identical path, so `propose-only` is pull-request mode with a different trigger. That is the design;
this is the honest state of it:

| Mode | What `propose-only` does today |
|---|---|
| `direct` | Refuses the live mutation. Nothing is applied to the cluster. The proposal is the `dry_run=RENDER` manifests or the `Diff`, which the agent can produce and a human can read. There is no pull request in this mode because there is no repository — direct mode applies from memory. |
| `flux` (git) | The same refusal. `internal/delivery/git` **does** implement pull-request mode (`ModePullRequest`, branch + `Provider.CreatePullRequest`, used by nothing on the server path yet), but `cmd/kelson-server` constructs its git writer with `git.ModeCommit` and has no forge-credential configuration at all. |

So v0 `propose-only` is **refusal plus a pointer to the proposal**, not an opened pull request. This
is written down rather than glossed because the alternative — reporting "proposal opened" when
nothing was opened — is precisely the silent-success failure this project treats as the worst shape
a bug can take.

The follow-up is small and the seam is already there: give kelson-server forge configuration, select
`git.ModePullRequest` for a propose-only agent's write, and the refusal becomes a `Committed` event
carrying a pull-request URL. Nothing in this ADR has to change for that; the policy block already
says which environments would use it.

## Consequences

**Positive**

- The acceptance criterion is a test: `TestProposeOnlyRefusesEveryMutation` drives every mutating RPC
  in the schema as an agent in a propose-only environment, and `TestAHumanIsNeverRestrictedByAgentPolicy`
  drives the same set as a human and requires all of them to pass.
- Policy cannot be escalated by the thing it governs. A spec write is itself guarded, so an agent
  cannot rewrite `agents: propose-only` to `allow` and then deploy — the refusal that matters most,
  since without it every other one is advisory.
- A blast-radius limit is checked against the *resolved* spec, so an environment override, a project
  default and an inline document are all caught by the same rule.
- Guardrails are reviewable: they are lines in the environment's YAML, they travel with the spec, and
  changing them is a change to the spec like any other.

**Negative, and the honest gaps**

- **Rollback is not blast-radius checked.** A rollback replays recorded manifests rather than a
  render, so `maxReplicas` and `protect` have nothing to read: restoring a revision that ran fifty
  replicas is not caught. `propose-only` and `forbid: [rollback]` do cover it, and both are one line.
  Parsing replica counts out of recorded manifests is the fix and it is not done here.
- **Policy is keyed on (project, environment), and a namespace is not.** An agent with an
  unrestricted credential could deploy an inline spec under a *different* project name whose
  `spec.namespace` points at a protected environment's namespace. This is the same shape as
  ADR-0024's log-namespace gap and the same fix applies — resolving namespace ownership server-side.
  Until then the mitigation is the credential's project scope.
- **A spec write is a whole-project write.** Because PutSpec replaces every document (and omitting an
  environment deletes it), an agent cannot store a spec for a project that has *any* propose-only
  environment, even to change a different one. That is the correct reading of what the operation
  does, and it is coarse.
- **Anonymous callers get agent strictness.** On a server started without a password kelson cannot
  tell a person from an agent, so it applies the stricter reading. A solo operator running an open
  server will meet their own production policy. The fix is to set a password and authenticate as a
  human, which is what such a server should do anyway.
- **A server with no spec store enforces nothing**, because there is no stored policy to read. That
  is a partially-wired build rather than a bypass — `cmd/kelson-server` always wires the store — but
  it means the enforcement point is only as good as the state plane behind it.
- `maxReplicas` reads the resolved replica *floor and ceiling* of workload components only. A data
  service's topology is its preset and a chart's is the chart's business, so neither is capped by
  this rule.
