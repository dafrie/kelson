import { PhaseRail } from "kelson-ui";

/** Mid-deploy: the reconciler is working and nothing is wrong. */
export function Reconciling() {
  return (
    <PhaseRail
      input={{ phase: "Reconciling", mode: "flux", adapter: "flux" }}
      project="checkout"
      environment="production"
    />
  );
}

/** Settled healthy: every stage done. */
export function Healthy() {
  return (
    <PhaseRail
      input={{ phase: "Healthy", mode: "flux", adapter: "flux" }}
      project="checkout"
      environment="production"
    />
  );
}

/** A rejection with the diagnosis panel and its one next step. */
export function Rejected() {
  return (
    <PhaseRail
      input={{
        phase: "Rejected",
        answer: "rejected",
        reachedPhase: "Reconciling",
        mode: "flux",
        adapter: "flux",
        cause: {
          component: "cache",
          reason: "unknown-component",
          message: 'no data service named "redis-7" is available in this cluster',
        },
      }}
      project="checkout"
      environment="production"
    />
  );
}

/** The compact one-line form a project page embeds. */
export function Compact() {
  return (
    <PhaseRail
      input={{ phase: "Applied", mode: "flux", adapter: "flux" }}
      project="checkout"
      environment="staging"
      compact
    />
  );
}
