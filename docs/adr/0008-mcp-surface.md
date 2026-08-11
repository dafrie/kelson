# ADR-0008: MCP tools are task-shaped, not endpoint-shaped

- **Status:** Accepted
- **Date:** 2026-08-12

## Context

An earlier draft of the agent design said the MCP server is "a thin adapter over the same typed API the
UI uses", with the test: *if it needs an endpoint the UI does not have, the API is wrong.*

That was written to prevent a real failure — an agent surface that grows its own business logic, its own
bugs, and capabilities nobody else can audit. But it prescribed the wrong thing, because it conflated two
separate concerns.

A one-to-one mapping from API endpoints to MCP tools produces a bad agent surface:

- **Tool count.** A sixty-endpoint API becomes sixty tools. Model tool-selection accuracy degrades well
  before that.
- **Wrong granularity.** APIs are resource-shaped because programmatic clients know what they want.
  Agents are task-shaped and have to discover it. "Why is checkout broken" becomes eight calls —
  application, deployments, pods, events, logs, metrics, recent revision, policy status — each costing a
  round trip and context.
- **Unbounded responses.** A log-streaming endpoint is correct for a CLI and unusable in a context window.
- **No affordance.** Tools need descriptions written for models: preconditions, side effects, cost. An API
  has no equivalent and needs none.

## Decision

**Capability parity, not surface parity.**

The MCP server must not expose any capability the API does not have. It may — and should — expose a
different shape.

The test becomes: *if MCP needs a **capability** the API lacks, the API is wrong. If it needs a different
**shape**, that is the point.*

Concretely, the MCP surface is designed rather than generated, and adds four things:

### 1. Task-shaped composition
Tools correspond to what an agent is trying to do, not to resources. `diagnose_application` composes
status, events, a bounded log window, the recent revision and policy state into one structured answer.
`deploy` renders, dry-runs and returns a risk-classified diff. Neither is a new capability; both are
compositions of API calls that would otherwise cost an agent six round trips.

### 2. Bounded, shaped responses
The API streams. Tools return windows. "The last N lines around this failure" is a first-class query at
the tool layer, sized for a context window rather than for a terminal.

### 3. Model-facing descriptions
Every tool states its preconditions, whether it mutates, and what it costs. Read-only tools are clearly
separated from mutating ones. This is documentation for a model and has no API equivalent.

### 4. Policy-aware exposure
Tools an agent's credentials cannot use are **not exposed**, rather than exposed and erroring. An agent
scoped propose-only in production should not see production-mutating tools at all — it will otherwise
spend attempts discovering the boundary. Enforcement still happens server-side; this is ergonomics on top
of enforcement, never instead of it.

## Rationale

The original concern was sound and is preserved by capability parity. What made the "thin adapter" framing
wrong was assuming that auditability requires an identical surface. It does not. It requires that every
tool decomposes into API calls that any other client could make, which is a much weaker and more useful
constraint.

Generating tools from the Protobuf schema ([ADR-0002](0002-tech-stack.md)) remains useful as a **starting
point and a consistency check** — it can verify that every tool maps to real API operations. It is not the
design.

## Consequences

**Positive.**
- An agent surface designed for how agents actually work, rather than one inherited from a UI's needs.
- Far fewer tools, each meaningful, which measurably helps tool selection.
- Context economy: one call where a naive mapping would need six.
- Capability parity still prevents a privileged backdoor. Every tool is auditable as a composition of
  ordinary API calls.

**Negative.**
- The MCP surface is now a designed artifact with its own maintenance cost, its own tests, and its own
  review burden. It is no longer free.
- Composition can drift from the API beneath it. Tools must be tested against the API rather than
  reimplementing logic — a rule that needs enforcing, because the shortcut is always available.
- Policy-aware exposure means tool availability varies by caller, which is harder to document and to
  debug. "Why can't the agent see that tool" becomes a support question.
- Deciding tool granularity is genuine design work with no obvious right answer, and getting it wrong is
  only visible once agents are using it in anger.

## Revisit when

Real agent usage shows the chosen granularity is wrong, or MCP itself gains a mechanism that changes how
tool count and discovery interact.
