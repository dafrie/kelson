# ADR-0006: Project, Application, Environment

- **Status:** Accepted (leaf shape amended by [ADR-0014](0014-components.md), 2026-08-13: `applications`
  and `services` unify into one `components` list with kinds, and every component gets its own
  ServiceAccount. The Project → leaf → Environment shape decided here is confirmed, not replaced.)
- **Date:** 2026-08-11

## Context

A real service is usually several deployables that ship together: a web process, a background worker, a
cron job. They share an image, environment configuration and database bindings, and they generally need to
move as a unit.

Three models were considered.

1. **One Application containing components.** Web, worker and cron nested inside a single Application.
2. **One Application equals one deployable.** Worker and cron are separate Applications referencing shared
   configuration.
3. **Project contains Applications.** A grouping level above the deployable, holding shared configuration.

## Decision

**Project → Application → Environment.**

- **Project** — a grouping with shared configuration. Shared image, environment variables, service
  bindings and team ownership live here. A product, or a team's surface area.
- **Application** — one deployable, rendering to one workload. A web service, a worker, a cron job.
- **Environment** — where an Application runs and what differs there: cluster, namespace, domain,
  replica and resource overrides, delivery mode, policy.

A typical service is one Project with three Applications, deployed into two or three Environments.

## Rationale

Option 1 pushes complexity into the Application schema and the renderer. Every field then needs a
per-component override story, which is the nesting that makes a spec hard to read and hard for an agent to
generate correctly. It also conflicts with [ADR-0001](0001-hybrid-state-model.md): a renderer emitting
several workloads from one spec makes diffs harder to attribute to a specific deployable.

Option 2 is the simplest schema but leaves nowhere for shared configuration. Users end up duplicating
environment blocks across specs, and those copies drift. That drift is the failure users hit in month
three, and it is exactly what the platform should prevent.

Option 3 puts shared configuration where it belongs without complicating the deployable. Each Application
still renders to one workload, which keeps the renderer simple and makes diffs precise. It also matches
how Canine and Coolify users already think, so the model needs no explanation.

The thin-abstraction principle applies here as much as anywhere. Three concepts is close to the minimum
that expresses a real system, and adding a fourth should require a strong argument.

## Consequences

**Positive.**
- Shared configuration has one home, so there is nothing to keep in sync by hand.
- Each Application renders to one workload, keeping the renderer and its diffs simple.
- Applications can be deployed and rolled back independently when that is what is wanted.
- The model is already familiar to users coming from Canine or Coolify.

**Negative.**
- A hierarchy level lands in every API path, permission check and URL, and hierarchies are very hard to
  remove later. This is the main cost and it was accepted deliberately.
- Atomic rollback across a whole Project is not free. Coordinated multi-Application rollback needs
  explicit design rather than falling out of the model.
- The Project/Environment relationship needs care: environments are scoped to a Project, but delivery
  mode and policy may reasonably be set at either level. The precedence rules must be written down before
  implementation.
- Single-Application projects carry a level of ceremony they do not need. The CLI and UI should hide the
  Project when there is only one Application, so simple cases stay simple.

## Open questions for implementation

Resolved by [docs/model.md](../model.md) with the implementation in `internal/model` (#24, #25, #28):

- Precedence when Project and Environment both set a value → rules P1–P6: innermost scope
  wins; environment-scoped concerns (delivery, policy, secrets) are taken whole, never merged.
- Whether an Application can belong to more than one Project → no; identified by
  (project, application).
- How Project-level shared configuration is expressed without becoming a second spec format →
  shared fields on the Project act as per-key defaults merged into each Application.
