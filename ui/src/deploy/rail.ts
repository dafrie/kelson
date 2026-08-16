import type { StatusKind } from "../components/StatusPill";

/**
 * The deployment state machine, as something a person can read (issue #68).
 *
 * `Proposed → Committed → Reconciling → Applied → Healthy` is a rail, not a
 * spinner, because the three ways it fails demand three different actions and a
 * single red badge would hide which one you are in:
 *
 *   1. nothing picked the revision up    → check this environment's configuration
 *   2. the reconciler refused it         → fix the manifest
 *   3. it is applied and unhealthy       → debug the workload
 *
 * That distinction is the engine's already (internal/delivery/statemachine:
 * State.Answer keeps `stuck`, `rejected` and `degraded` apart by construction,
 * and MarkStuck picks the cause from the phase the machine is wedged in). This
 * module does no judging of its own — it maps what the wire says onto stage
 * states, an actor per stage, and one diagnosis with one next step. It is pure
 * so it can be tested without a DOM, and so both screens that render the rail
 * are guaranteed to say the same thing.
 *
 * Rejected and Degraded are deliberately NOT rail stages. They are failures OF
 * a stage: a rejection is the reconciling stage failing (rejection is pre-apply
 * — see the engine's transition table), a Degraded revision is the health stage
 * failing with everything before it done. Giving them their own boxes would
 * draw a happy path that does not exist.
 */

export const RAIL_PHASES = [
  "Proposed",
  "Committed",
  "Reconciling",
  "Applied",
  "Healthy",
] as const;

export type RailPhase = (typeof RAIL_PHASES)[number];

/** Where a stage sits: `stuck` is its own state, never a flavour of `failed`. */
export type StageState = "done" | "current" | "stuck" | "failed" | "pending";

/** statemachine.Cause, field for field. */
export interface RailCause {
  component: string;
  reason: string;
  message: string;
}

const NO_CAUSE: RailCause = { component: "", reason: "", message: "" };

export interface RailInput {
  /** delivery.Phase off the wire. Empty or unrecognised means "we cannot tell". */
  phase: string;
  /** statemachine.Answer, when the source carries one (the deploy stream does). */
  answer?: string | undefined;
  /** The engine's progress-timeout verdict, when the source carries one. */
  stuck?: boolean | undefined;
  cause?: RailCause | undefined;
  /**
   * The furthest rail stage proven reached, used to place a rejection. The
   * deploy stream knows it from its transition list and EventService's
   * StatusTransition carries `previous_phase`; without it a rejection lands on
   * Reconciling, which is where reconcilers reject things.
   */
  reachedPhase?: string | undefined;
  /**
   * The reconciler's name as the SERVER reported it —
   * DeployResponse.Committed.adapter. Never a guess made in the browser: an
   * unnamed reconciler is rendered as "reconciler" with the honest note that
   * the wire did not say which one.
   *
   * `Proposed.mode` used to be a second source. It carried the delivery mode,
   * the server stopped setting it when the mode vocabulary was deleted
   * (ADR-0028, #234), and reading a field nothing writes is how a dead concept
   * survives.
   */
  adapter?: string | undefined;
  /** Workloads whose health verdict is not healthy (StatusResponse.verdicts). */
  unhealthyWorkloads?: number | undefined;
}

export interface RailStage {
  phase: RailPhase;
  state: StageState;
  /** Who is responsible for this stage moving. */
  actor: string;
  /**
   * False when the wire did not name the reconciler, so the UI can say so
   * instead of printing a plausible name nobody verified.
   */
  actorKnown: boolean;
  status: StatusKind;
}

/** The three failure modes the issue refuses to collapse into one. */
export type DiagnosisKind = "not-picked-up" | "rejected" | "unhealthy";

/** What the next step actually opens. Routes are the caller's business. */
export type RailActionKind = "config" | "manifest" | "logs";

export interface RailAction {
  kind: RailActionKind;
  label: string;
}

export interface Diagnosis {
  kind: DiagnosisKind;
  /** The stage the failure belongs to. */
  stage: RailPhase;
  title: string;
  /** The cause detail, verbatim from the server when it sent one. */
  detail: string;
  /** One imperative sentence. Different for every kind, by construction. */
  nextStep: string;
  action: RailAction;
  /** The responsible component the server named, or "" when it named none. */
  component: string;
}

export interface Rail {
  stages: RailStage[];
  /** The engine's answer: waiting|progressing|live|stuck|rejected|degraded|unknown. */
  answer: string;
  headline: string;
  diagnosis: Diagnosis | undefined;
  /** Healthy or Rejected: the question is answered for this revision. */
  settled: boolean;
}

const STAGE_STATUS: Record<StageState, StatusKind> = {
  done: "synced",
  current: "reconciling",
  stuck: "degraded",
  failed: "failed",
  pending: "unknown",
};

function indexOfPhase(phase: string): number {
  return (RAIL_PHASES as readonly string[]).indexOf(phase);
}

/**
 * Reconciler names we can print because the server named the adapter.
 *
 * Flux is the only reconciler kelson has (ADR-0028), and `Committed.adapter` is
 * hardcoded to `"flux"` server-side (`internal/api`'s `adapterName`) — so this
 * table has one entry. It stays a table, and a name outside it is still printed
 * verbatim rather than hidden,
 * because the wire's word must never be silently discarded: an older server or
 * a value this build has not seen yet is still worth showing, just without a
 * friendly translation. Only an EMPTY mode degrades to the unnamed
 * "reconciler".
 */
const RECONCILERS: Record<string, string> = {
  flux: "Flux (kustomize-controller)",
};

/**
 * Who reconciles, and whether we actually know.
 *
 * Two sources, most authoritative first: the adapter the Committed event named,
 * and the component a failure cause blamed. DeployService.StatusResponse
 * carries neither — it has phase, revision, cause, detail, verdicts and
 * namespace — so on a screen fed by Status alone the reconciler is unknown
 * until something fails and names a component. Naming it would need an
 * `adapter` field on StatusResponse, mirroring
 * DeployResponse.Committed.adapter; until that exists this returns known=false
 * and the UI says "not reported" rather than guessing from the spec, which is
 * what would be deployed and not what did deploy.
 */
export function reconcilerActor(input: RailInput): {
  name: string;
  known: boolean;
} {
  const cause = input.cause ?? NO_CAUSE;
  const named = [input.adapter, cause.component]
    .map((s) => (s ?? "").trim())
    .find((s) => s !== "" && s !== "reconciler");
  if (named === undefined) return { name: "reconciler", known: false };
  return { name: RECONCILERS[named.toLowerCase()] ?? named, known: true };
}

function actorFor(phase: RailPhase, reconciler: { name: string; known: boolean }): {
  actor: string;
  actorKnown: boolean;
} {
  switch (phase) {
    case "Proposed":
    case "Committed":
      // kelson renders and writes the revision; nothing else can reach here.
      return { actor: "kelson server", actorKnown: true };
    case "Reconciling":
      return { actor: reconciler.name, actorKnown: reconciler.known };
    case "Applied":
      return { actor: "Kubernetes (the cluster)", actorKnown: true };
    case "Healthy":
      // The verdicts are internal/observation's, whoever applied the manifests.
      return { actor: "kelson observation", actorKnown: true };
  }
}

/**
 * statemachine.State.Answer, for sources that do not send the answer itself
 * (StatusResponse has a phase and a cause and nothing else). The order matters
 * and is the Go one: a Degraded deployment that then times out is "degraded",
 * not "stuck waiting".
 */
export function deriveAnswer(phase: string, stuck: boolean): string {
  switch (phase) {
    case "Rejected":
      return "rejected";
    case "Degraded":
      return "degraded";
    case "Healthy":
      return "live";
  }
  if (indexOfPhase(phase) < 0) return "unknown";
  if (stuck) return "stuck";
  if (phase === "Reconciling" || phase === "Applied") return "progressing";
  return "waiting";
}

/**
 * MarkStuck's reason vocabulary — the tokens the ENGINE writes, never an
 * adapter's (adapters send Kubernetes/Flux condition reasons like
 * `ProgressDeadlineExceeded`). DeployService.StatusResponse has no `stuck`
 * field, only the cause string the engine projected into it, so on that path
 * these tokens are how a stuck verdict is recovered. The deploy stream carries
 * the flag outright and never needs this.
 */
const STUCK_REASONS = new Set([
  "NotCommitted",
  "NotPickedUp",
  "StalledReconciling",
  "HealthUnknown",
  "NoProgress",
]);

export function isStuckReason(reason: string): boolean {
  return STUCK_REASONS.has(reason);
}

/**
 * A `statemachine.Cause` that has been flattened to a string and has to be
 * taken apart again — DeployService.Status sends `Cause.String()`, which is the
 * non-empty parts of component/reason/message joined with ": ".
 *
 * The split is conservative because the message may contain colons of its own:
 * the first segment counts as a component only if it looks like an adapter name
 * (a lowercase token, no spaces), the next counts as a reason only if it looks
 * like the engine's CamelCase token, and everything left is the message. A
 * string that matches neither shape stays entirely in `message`, which renders
 * as the server's own words — the failure mode of being too eager here is
 * inventing a component that was never blamed.
 */
export function parseCause(text: string): RailCause {
  const trimmed = text.trim();
  if (trimmed === "") return { ...NO_CAUSE };
  const parts = trimmed.split(": ");
  let at = 0;
  let component = "";
  let reason = "";
  if (parts.length > 1 && /^[a-z][a-z0-9._-]*$/.test(parts[0] ?? "")) {
    component = parts[0] ?? "";
    at = 1;
  }
  if (parts.length > at + 1 && /^[A-Z][A-Za-z0-9]*$/.test(parts[at] ?? "")) {
    reason = parts[at] ?? "";
    at += 1;
  }
  return { component, reason, message: parts.slice(at).join(": ") };
}

const HEADLINES: Record<string, string> = {
  waiting: "Committed. Waiting for a reconciler to pick it up.",
  progressing: "In flight.",
  live: "Live and healthy.",
  stuck: "No progress.",
  rejected: "Rejected before anything was applied.",
  degraded: "Applied, but not healthy.",
  unknown: "The delivery state could not be read.",
};

/** Builds the rail. Pure: same input, same rail, no clock and no routing. */
export function buildRail(input: RailInput): Rail {
  const cause = input.cause ?? NO_CAUSE;
  const stuck = input.stuck ?? isStuckReason(cause.reason);
  const phase = input.phase;
  const answer = input.answer && input.answer !== "" ? input.answer : deriveAnswer(phase, stuck);
  const reconciler = reconcilerActor(input);
  const unhealthy = input.unhealthyWorkloads ?? 0;

  const states = stageStates({ phase, stuck, reachedPhase: input.reachedPhase, unhealthy });
  const diagnosis = diagnose({ phase, stuck, cause, reconciler, unhealthy, states });

  const stages: RailStage[] = RAIL_PHASES.map((p, i) => {
    const state = states[i] ?? "pending";
    return {
      phase: p,
      state,
      status: STAGE_STATUS[state],
      ...actorFor(p, reconciler),
    };
  });

  return {
    stages,
    answer,
    headline: diagnosis?.title ?? HEADLINES[answer] ?? HEADLINES.unknown ?? "",
    diagnosis,
    settled: phase === "Healthy" || phase === "Rejected",
  };
}

/**
 * The per-stage states.
 *
 * Everything before the current phase is done, everything after is pending, and
 * the phase itself carries whatever went wrong. The two off-rail phases resolve
 * to a stage: Rejected lands on the furthest stage proven reached (defaulting
 * to Reconciling, and clamped to pre-apply because the engine forbids rejecting
 * an applied revision), Degraded lands on Healthy with the apply behind it.
 */
function stageStates({
  phase,
  stuck,
  reachedPhase,
  unhealthy,
}: {
  phase: string;
  stuck: boolean;
  reachedPhase: string | undefined;
  unhealthy: number;
}): StageState[] {
  const states: StageState[] = ["pending", "pending", "pending", "pending", "pending"];

  if (phase === "Rejected") {
    const reached = indexOfPhase(reachedPhase ?? "");
    const at = Math.min(reached < 0 ? 2 : reached, 2);
    for (let i = 0; i < at; i++) states[i] = "done";
    states[at] = "failed";
    return states;
  }

  if (phase === "Degraded") {
    for (let i = 0; i < 4; i++) states[i] = "done";
    states[4] = "failed";
    return states;
  }

  const at = indexOfPhase(phase);
  if (at < 0) return states; // Unknown phase: claim nothing.

  for (let i = 0; i < at; i++) states[i] = "done";
  if (at === 4) {
    // Healthy. A live revision with a bad verdict underneath is a real and
    // important thing to see, so the health stage reports the verdicts rather
    // than the phase (ProjectDetailPage makes the same point about its two halves).
    states[4] = unhealthy > 0 ? "failed" : "done";
    return states;
  }
  if (at === 3 && unhealthy > 0) {
    states[3] = "done";
    states[4] = "failed";
    return states;
  }
  states[at] = stuck ? "stuck" : "current";
  return states;
}

function diagnose({
  phase,
  stuck,
  cause,
  reconciler,
  unhealthy,
  states,
}: {
  phase: string;
  stuck: boolean;
  cause: RailCause;
  reconciler: { name: string; known: boolean };
  unhealthy: number;
  states: StageState[];
}): Diagnosis | undefined {
  const who = reconciler.known ? reconciler.name : "the reconciler";
  const workloads = `${unhealthy} ${unhealthy === 1 ? "workload" : "workloads"}`;

  // Order mirrors State.Answer: settled failures outrank a timeout.
  if (phase === "Rejected") {
    const at = states.indexOf("failed");
    return {
      kind: "rejected",
      stage: RAIL_PHASES[at < 0 ? 2 : at] ?? "Reconciling",
      title: `${who} rejected this revision`,
      detail:
        cause.message ||
        "The reconciler processed the change and refused it, but reported no reason.",
      nextStep:
        "Fix the manifest and deploy again — the change was refused before anything was applied, so nothing is live.",
      action: { kind: "manifest", label: "Edit configuration" },
      component: cause.component,
    };
  }

  if (phase === "Degraded") {
    return {
      kind: "unhealthy",
      stage: "Healthy",
      title: "Applied, but the workload is not healthy",
      detail:
        cause.message ||
        (unhealthy > 0
          ? `${workloads} are reporting an unhealthy verdict.`
          : "The revision is applied and the health check is failing."),
      nextStep:
        "Debug the workload — this revision IS live, so read its logs and events; the manifest was accepted.",
      action: { kind: "logs", label: "Open logs" },
      component: cause.component,
    };
  }

  if (stuck && phase === "Applied") {
    // Applied is behind us, health never arrived: a workload question, not a
    // wiring one, so it gets the workload's next step.
    return {
      kind: "unhealthy",
      stage: "Healthy",
      title: "Applied, but no health verdict arrived",
      detail:
        cause.message ||
        "The revision was applied and nothing has reported it healthy since.",
      nextStep:
        "Debug the workload — the manifests landed, so look at whether the pods are starting.",
      action: { kind: "logs", label: "Open logs" },
      component: cause.component,
    };
  }

  if (stuck) {
    const at = indexOfPhase(phase);
    return {
      kind: "not-picked-up",
      stage: RAIL_PHASES[at < 0 ? 1 : at] ?? "Committed",
      title: stuckTitle(phase, who),
      detail:
        cause.message ||
        "The progress timeout expired without a phase change and no cause was reported.",
      nextStep: stuckNextStep(phase, who),
      action: { kind: "config", label: "Review this environment" },
      component: cause.component,
    };
  }

  if (unhealthy > 0 && (phase === "Applied" || phase === "Healthy")) {
    // The phase says the change arrived; the verdicts say it does not work.
    // Both are true and the rail must not let the first hide the second.
    return {
      kind: "unhealthy",
      stage: "Healthy",
      title: `Live, but ${workloads} are unhealthy`,
      detail:
        "The revision arrived and was applied. The health verdicts below are what is failing.",
      nextStep:
        "Debug the workload — delivery is done, so this is the component's own problem.",
      action: { kind: "logs", label: "Open logs" },
      component: cause.component,
    };
  }

  return undefined;
}

function stuckTitle(phase: string, who: string): string {
  switch (phase) {
    case "Proposed":
      return "kelson never committed this revision";
    case "Reconciling":
      return `${who} started, then stopped making progress`;
    default:
      return `${who} has not picked this revision up`;
  }
}

/**
 * All three read "check this environment's configuration", because that IS the
 * action for every flavour of "nothing is moving" — the classic cause is
 * nothing watching what kelson published. The sentences differ because what to
 * check differs.
 */
function stuckNextStep(phase: string, who: string): string {
  switch (phase) {
    case "Proposed":
      return "Check this environment's configuration — the publish step never completed, so nothing was handed to a reconciler.";
    case "Reconciling":
      return `Check this environment's configuration, then ${who} itself — it took the revision and stalled, so its own logs and conditions hold the reason.`;
    default:
      return `Check this environment's configuration — ${who} must be watching what kelson published, and its source must be syncing.`;
  }
}
