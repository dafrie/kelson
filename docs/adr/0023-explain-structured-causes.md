# ADR-0023 — `explain`: structured causes with confidence and evidence

Status: Accepted
Date: 2026-08-14
Issue: [#77](https://github.com/dafrie/kelson/issues/77) · relates to [ADR-0008](0008-mcp-surface.md), [ADR-0013](0013-server-state-and-api-v0.md)

## Context

"Why is this application degraded?" is the question every agent debugging loop actually asks, and the
loop kelson shipped before this ADR answered it the expensive way: fetch all logs, all events, all
resource state, and let a model guess. That is a context-window bill paid for a correlation kelson can
already make. kelson knows the application model, the deployment state machine, the rendered manifests
of every retained revision and the observation plane's verdicts. It can name the cause.

Two things were missing. The first is a *shape*: a cause with a stable code, a confidence, the evidence
behind it and — where determinable — the revision that introduced it. The second is a *place*: the
diagnosis machinery that existed lived in `internal/mcp/diagnose.go`, which is a client of the API. A
capability implemented in the agent surface is a capability the CLI and the UI cannot have, and
ADR-0008 is explicit that the MCP surface composes API capabilities rather than owning them.

## Decision

**1. The capability is a package, `internal/explain`, and it takes computed inputs — never a cluster.**

`explain.Explain(ctx, Input) Explanation` consumes what the other planes have already decided: the
adapter's `delivery.Status`, the observation plane's `[]observation.Verdict`, the delivery history
`[]delivery.Entry`, optional `[]diff.PolicyViolation` from an L2 dry-run, and two function seams — one
for a bounded log window, one for the recorded manifests of a revision.

It holds no Kubernetes client and cannot: under `.golangci.yml` a package outside the cluster-facing
planes is allowed stdlib, kelson, jsonschema, cobra and yaml.v3 only. That fence is the enforcement of
the rule this ADR most wants to keep — **explain never re-derives what observation already knows**. A
crash loop is a crash loop because the observation plane said so; explain adds the evidence, the
correlation and the confidence.

**2. `Explanation` is the shape.**

```
Explanation { Subject, Phase, Summary, Causes[], RecentChange?, Notes[], Truncated[] }
Cause       { Code, Message, Confidence, Resource, Evidence[], Remediation?, IntroducedBy? }
Evidence    { Kind: field|log|condition|revision, Source, Detail }
```

`Notes` carries degradation ("history was not readable, so no change correlation") and `Truncated`
carries every bound that fired. Both exist so a partial answer is never mistaken for a complete one —
the same discipline the tri-state of [#144](https://github.com/dafrie/kelson/issues/144) applies to
detection: *unknown is not no*.

**3. Confidence is a rule, not a feeling.**

| Confidence | Rule |
|---|---|
| `high` | A controller named the reason: an exact kubelet waiting reason (`CrashLoopBackOff`, `ImagePullBackOff`, `OOMKilled`), a scheduler `Unschedulable` message, an external-secrets `Ready=False` reason, an API-server enforcement rejection, a Flux failure condition. Or: two independent signals agree (the container's own output names a variable *and* a revision changed it). |
| `medium` | One signal only, or a heuristic the producing plane already calls one — a running-but-not-ready container inferred as a failing probe, a terminated container inferred as a loop, a revision change correlated with a failure whose output does not name it. The correlation is stated as a correlation in the message. |
| `low` | An audit-mode policy finding: real, recorded, and not what vetoed anything. |

A cause with no direct evidence is never `high`. A candidate with neither signal is not emitted at all —
kelson says nothing rather than guessing.

**4. Bounds are hard, stated in the proto, and enforced in one place.**

6 causes, 4 evidence items each, 8 log lines per excerpt, 200 bytes per line, 12 KiB for the whole
`Explanation`. `bound()` trims in a fixed order (line bytes, lines, evidence, causes, then the byte
budget from the least severe cause backwards) and records every trim in `Truncated`. Log excerpts pass
through `redact.Scrub` ([#117](https://github.com/dafrie/kelson/issues/117)) on the way in.

**5. The surface is a new `ExplainService`, not a method on `DeployService`.**

ADR-0013 §2 organises services by capability rather than by resource. Delivery *control* (deploy,
status, rollback, history, promote) and causal *analysis* are different capabilities with different
readers, and `DeployService` is already the largest service in the schema. A separate service also
leaves room for the subjects explanation will grow — a build, a rejected policy, a preview — without
each one widening the deploy contract.

**6. The CLI is `kelson explain -f spec.yaml --env <name>`, not `kelson explain <project>`.**

`cmd/kelson` is denied the ConnectRPC libraries by the same depguard rule that keeps the renderer pure,
and every existing verb (`status`, `diff`, `rollback`) is spec-file shaped. So the CLI composes the
same `internal/explain` capability locally, against the direct adapter, its probe and its rendered
history — the identical code path the server runs, which is what keeps the two from disagreeing.
A server-backed `kelson explain <project>` is a change to what the CLI *is* and belongs with the
broader client work, not here.

**7. `diagnose_application` composes it.**

The MCP tool gains a `WHY` section fed by `ExplainService.Explain` and keeps every section it had. It
still classifies nothing: the causes, their confidences and their evidence are relayed exactly as the
server produced them, which is ADR-0008's composition rule.

## Cause taxonomy

Every code is a compatibility promise, in the `<domain>/<class>` shape the delivery and policy
taxonomies already use.

| Code | Detected from | Confidence rule |
|---|---|---|
| `explain/missing-env-var` | a crash-looping container plus an environment-variable change between the live revision and its predecessor, and/or the container's own output naming a variable | `high` when both signals agree; `medium` with one |
| `explain/crash-loop` | verdict `crash-loop-back-off` | `high` on the kubelet's own `CrashLoopBackOff`; `medium` when observation inferred it from a terminated container |
| `explain/oom-killed` | verdict `crash-loop-back-off` whose reason names `OOMKilled` | `high` |
| `explain/evicted` | verdict reason names `Evicted` | `high` |
| `explain/image-pull` | verdict `image-pull-back-off` | `high` |
| `explain/failing-probe` | verdict `failing-probe`, with the probe's path and port read from the rendered manifest | `medium` — observation names it a heuristic |
| `explain/insufficient-resources` | verdict `insufficient-resources` | `high` |
| `explain/unschedulable` | verdict `scheduling-failed` | `high` |
| `explain/secret-sync-failed` | verdict `secret-sync-failed` ([#80](https://github.com/dafrie/kelson/issues/80)) | `high` |
| `explain/workload-missing` | verdict `missing` | `high` |
| `explain/policy-rejected` | a `diff.PolicyViolation` from the L2 dry-run ([#45](https://github.com/dafrie/kelson/issues/45)) | `high` when enforced; `low` for an audit finding |
| `explain/decryption-failed` | a delivery status cause carrying kustomize-controller's decryption failure (ADR-0022) | `high` |
| `explain/reconciler-not-ready` | a `Rejected`/`Degraded` phase whose cause names the reconciler | `high` |
| `explain/release-job-failed` | `detail[releaseState]` is `failed` or `timeout` (ADR-0019, [#104](https://github.com/dafrie/kelson/issues/104)) | `high` |
| `explain/release-job-pending` | `detail[releaseState]` is `running` | `high` |

## The acceptance case

A `CrashLoopBackOff` caused by a missing environment variable is explained with the variable and the
revision that introduced it. The detection is two independent signals, and the confidence says which
of them fired:

1. **The change.** The recorded rendered manifests of the live revision and its predecessor are parsed
   into workloads → containers → environment variables (name, literal value, or `secretKeyRef`
   name/key), and diffed per container. A variable removed, renamed, or repointed at a Secret key is a
   candidate, and the history entry for the live revision is the `introducedBy`.
2. **The symptom.** The crash-looping container's bounded output is scanned for each candidate name,
   and — independently — for the phrasings a runtime uses when a variable is absent (`KeyError: 'X'`,
   `missing required environment variable X`, `X is not set`, …).

Both → `high`, the field reference and the log line as evidence. One → `medium`, with the message
saying plainly which half is a correlation. Neither → nothing is emitted.

## Consequences

**Positive.** One capability, three surfaces, no second implementation. Every cause carries the
evidence a human can check and the confidence an agent can gate on. The bound is a number, tested
against pathological input, not a hope. `internal/mcp` lost its only piece of judgement.

**Negative, plainly.**

- Explain sees what its callers hand it. It reads no Events, so an eviction is only detected when the
  reason reaches a verdict, and `CreateContainerConfigError` — the symptom of a missing Secret key —
  is not classified by the observation plane today and therefore not a cause here. The env-var
  correlation catches the common shape of it; the general case waits on a verdict code.
- The change correlation needs two retained revisions of *recorded rendered manifests*. A first deploy,
  a pruned history, or a Flux-mode environment whose manifests kelson does not keep gets a `Note` and
  no `introducedBy`. That is a degradation, and it is stated rather than papered over.
- The log-phrase patterns are a fixed, small list. They will miss runtimes that phrase it differently.
  A missed pattern costs confidence, never correctness: the revision correlation still fires at
  `medium` on its own.
- A new service is a new client to wire in every consumer, and `ui/src/gen` grows another module the
  UI does not use yet.
