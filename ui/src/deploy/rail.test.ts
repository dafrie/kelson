import { describe, expect, it } from "vitest";

import {
  buildRail,
  deriveAnswer,
  isStuckReason,
  parseCause,
  reconcilerActor,
  RAIL_PHASES,
  type RailInput,
  type StageState,
} from "./rail";

/**
 * The rail's whole job is to keep three failures apart, so most of what is
 * asserted here is *difference*: that not-picked-up, rejected and unhealthy
 * produce three different kinds, three different next steps and three different
 * actions. A test that only checked each one in isolation would still pass on
 * the day someone collapses them into one red badge.
 */

function states(input: RailInput): StageState[] {
  return buildRail(input).stages.map((s) => s.state);
}

describe("phase mapping", () => {
  it("walks the rail one stage at a time", () => {
    expect(states({ phase: "Proposed" })).toEqual([
      "current",
      "pending",
      "pending",
      "pending",
      "pending",
    ]);
    expect(states({ phase: "Committed" })).toEqual([
      "done",
      "current",
      "pending",
      "pending",
      "pending",
    ]);
    expect(states({ phase: "Reconciling" })).toEqual([
      "done",
      "done",
      "current",
      "pending",
      "pending",
    ]);
    expect(states({ phase: "Applied" })).toEqual([
      "done",
      "done",
      "done",
      "current",
      "pending",
    ]);
    expect(states({ phase: "Healthy" })).toEqual([
      "done",
      "done",
      "done",
      "done",
      "done",
    ]);
  });

  it("lands a rejection on the stage it happened at, defaulting to Reconciling", () => {
    // Rejection is pre-apply (the engine's transition table), so the stages
    // after it are pending and never done.
    expect(states({ phase: "Rejected" })).toEqual([
      "done",
      "done",
      "failed",
      "pending",
      "pending",
    ]);
    expect(states({ phase: "Rejected", reachedPhase: "Committed" })).toEqual([
      "done",
      "failed",
      "pending",
      "pending",
      "pending",
    ]);
    // A reachedPhase past the apply cannot be right for a rejection; it clamps
    // rather than drawing a rejection after the manifests landed.
    expect(states({ phase: "Rejected", reachedPhase: "Healthy" })).toEqual([
      "done",
      "done",
      "failed",
      "pending",
      "pending",
    ]);
  });

  it("puts Degraded on the health stage with the apply behind it", () => {
    expect(states({ phase: "Degraded" })).toEqual([
      "done",
      "done",
      "done",
      "done",
      "failed",
    ]);
  });

  it("claims nothing for a phase it does not know", () => {
    expect(states({ phase: "" })).toEqual([
      "pending",
      "pending",
      "pending",
      "pending",
      "pending",
    ]);
    expect(states({ phase: "Reticulating" })).toEqual([
      "pending",
      "pending",
      "pending",
      "pending",
      "pending",
    ]);
    expect(buildRail({ phase: "" }).answer).toBe("unknown");
    expect(buildRail({ phase: "" }).diagnosis).toBeUndefined();
  });

  it("marks Healthy and Rejected settled, and nothing else", () => {
    expect(buildRail({ phase: "Healthy" }).settled).toBe(true);
    expect(buildRail({ phase: "Rejected" }).settled).toBe(true);
    expect(buildRail({ phase: "Degraded" }).settled).toBe(false);
    expect(buildRail({ phase: "Reconciling" }).settled).toBe(false);
  });

  it("mirrors State.Answer, failures before the timeout", () => {
    expect(deriveAnswer("Proposed", false)).toBe("waiting");
    expect(deriveAnswer("Committed", false)).toBe("waiting");
    expect(deriveAnswer("Reconciling", false)).toBe("progressing");
    expect(deriveAnswer("Applied", false)).toBe("progressing");
    expect(deriveAnswer("Healthy", false)).toBe("live");
    expect(deriveAnswer("Rejected", false)).toBe("rejected");
    expect(deriveAnswer("Committed", true)).toBe("stuck");
    // Degraded then timing out is still degraded, not "stuck waiting".
    expect(deriveAnswer("Degraded", true)).toBe("degraded");
  });

  it("prefers the answer the stream sent over one derived from the phase", () => {
    expect(buildRail({ phase: "Committed", answer: "stuck" }).answer).toBe("stuck");
  });
});

describe("the three failure modes", () => {
  const notPickedUp = buildRail({
    phase: "Committed",
    stuck: true,
    adapter: "flux",
    cause: {
      component: "flux",
      reason: "NotPickedUp",
      message: "revision abc1234 was committed but flux has not picked it up within 5m",
    },
  });

  const rejected = buildRail({
    phase: "Rejected",
    adapter: "flux",
    cause: {
      component: "flux",
      reason: "BuildFailed",
      message: "kustomize build failed: accumulating resources: missing deployment.yaml",
    },
  });

  const unhealthy = buildRail({
    phase: "Degraded",
    adapter: "flux",
    cause: {
      component: "kubernetes",
      reason: "ProgressDeadlineExceeded",
      message: "Deployment/checkout has 1/3 replicas ready",
    },
  });

  it("classifies each one as its own kind", () => {
    expect(notPickedUp.diagnosis?.kind).toBe("not-picked-up");
    expect(rejected.diagnosis?.kind).toBe("rejected");
    expect(unhealthy.diagnosis?.kind).toBe("unhealthy");
  });

  it("gives each one a different next step and a different action", () => {
    const steps = [notPickedUp, rejected, unhealthy].map(
      (r) => r.diagnosis?.nextStep,
    );
    expect(new Set(steps).size).toBe(3);
    const actions = [notPickedUp, rejected, unhealthy].map(
      (r) => r.diagnosis?.action.kind,
    );
    expect(actions).toEqual(["config", "manifest", "logs"]);
  });

  it("says check the configuration for a revision nothing picked up", () => {
    expect(notPickedUp.diagnosis?.nextStep).toContain(
      "Check this environment's configuration",
    );
    expect(notPickedUp.diagnosis?.title).toContain("has not picked this revision up");
    // Which stage is wedged IS the diagnosis, so it is carried, not flattened.
    expect(notPickedUp.diagnosis?.stage).toBe("Committed");
    expect(notPickedUp.diagnosis?.component).toBe("flux");
  });

  it("says fix the manifest for a rejection, and that nothing is live", () => {
    expect(rejected.diagnosis?.nextStep).toContain("Fix the manifest");
    expect(rejected.diagnosis?.nextStep).toContain("nothing is live");
    expect(rejected.diagnosis?.detail).toContain("kustomize build failed");
  });

  it("says debug the workload for an unhealthy revision, and that it IS live", () => {
    expect(unhealthy.diagnosis?.nextStep).toContain("Debug the workload");
    expect(unhealthy.diagnosis?.nextStep).toContain("IS live");
    expect(unhealthy.diagnosis?.stage).toBe("Healthy");
  });

  it("keeps the server's own words as the detail, and falls back honestly", () => {
    expect(notPickedUp.diagnosis?.detail).toBe(
      "revision abc1234 was committed but flux has not picked it up within 5m",
    );
    const silent = buildRail({ phase: "Rejected" });
    expect(silent.diagnosis?.kind).toBe("rejected");
    expect(silent.diagnosis?.detail).toContain("reported no reason");
  });

  it("treats applied-but-never-healthy as a workload problem, not a wiring one", () => {
    const rail = buildRail({
      phase: "Applied",
      stuck: true,
      adapter: "flux",
      cause: {
        component: "flux",
        reason: "HealthUnknown",
        message: "revision abc1234 was applied but never reported healthy within 5m",
      },
    });
    expect(rail.diagnosis?.kind).toBe("unhealthy");
    expect(rail.diagnosis?.nextStep).toContain("Debug the workload");
    expect(rail.diagnosis?.action.kind).toBe("logs");
  });

  it("does not let a Healthy phase hide unhealthy verdicts", () => {
    const rail = buildRail({ phase: "Healthy", unhealthyWorkloads: 2 });
    expect(rail.diagnosis?.kind).toBe("unhealthy");
    expect(rail.diagnosis?.title).toContain("2 workloads");
    expect(rail.stages[4]?.state).toBe("failed");
    // Everything up to the apply really did happen, and still reads that way.
    expect(rail.stages[3]?.state).toBe("done");
  });

  it("reports nothing wrong when nothing is wrong", () => {
    expect(buildRail({ phase: "Healthy" }).diagnosis).toBeUndefined();
    expect(buildRail({ phase: "Reconciling" }).diagnosis).toBeUndefined();
    expect(buildRail({ phase: "Healthy" }).headline).toContain("Live");
  });
});

describe("stuck", () => {
  it("wedges the current stage rather than failing it", () => {
    expect(states({ phase: "Reconciling", stuck: true })).toEqual([
      "done",
      "done",
      "stuck",
      "pending",
      "pending",
    ]);
    expect(states({ phase: "Committed", stuck: true })).toEqual([
      "done",
      "stuck",
      "pending",
      "pending",
      "pending",
    ]);
  });

  it("distinguishes a stalled reconciler from one that never started", () => {
    const never = buildRail({ phase: "Committed", stuck: true, adapter: "flux" });
    const stalled = buildRail({ phase: "Reconciling", stuck: true, adapter: "flux" });
    expect(never.diagnosis?.kind).toBe("not-picked-up");
    expect(stalled.diagnosis?.kind).toBe("not-picked-up");
    expect(never.diagnosis?.title).not.toBe(stalled.diagnosis?.title);
    expect(stalled.diagnosis?.nextStep).toContain("its own logs and conditions");
  });

  it("blames kelson when the commit itself never happened", () => {
    const rail = buildRail({ phase: "Proposed", stuck: true });
    expect(rail.diagnosis?.title).toContain("kelson never committed");
    expect(rail.diagnosis?.stage).toBe("Proposed");
  });

  it("recovers a stuck verdict from the engine's cause reasons", () => {
    // DeployService.Status has no `stuck` field — only the flattened cause.
    for (const reason of [
      "NotCommitted",
      "NotPickedUp",
      "StalledReconciling",
      "HealthUnknown",
      "NoProgress",
    ]) {
      expect(isStuckReason(reason)).toBe(true);
    }
    // An adapter's own condition reason must not read as the engine's verdict.
    expect(isStuckReason("ProgressDeadlineExceeded")).toBe(false);
    expect(isStuckReason("")).toBe(false);

    const rail = buildRail({
      phase: "Committed",
      cause: parseCause(
        "flux: NotPickedUp: revision abc1234 was committed but flux has not picked it up",
      ),
    });
    expect(rail.answer).toBe("stuck");
    expect(rail.stages[1]?.state).toBe("stuck");
    expect(rail.diagnosis?.kind).toBe("not-picked-up");
  });
});

describe("who is responsible", () => {
  it("names the same actors for every stage regardless of mode", () => {
    const stages = buildRail({ phase: "Healthy", adapter: "flux" }).stages;
    expect(stages.map((s) => s.actor)).toEqual([
      "kelson server",
      "kelson server",
      "Flux (kustomize-controller)",
      "Kubernetes (the cluster)",
      "kelson observation",
    ]);
    expect(stages.every((s) => s.actorKnown)).toBe(true);
  });

  it("names the reconciler from the adapter or the blamed component", () => {
    expect(reconcilerActor({ phase: "", adapter: "flux" }).name).toBe(
      "Flux (kustomize-controller)",
    );
    expect(
      reconcilerActor({
        phase: "",
        cause: { component: "flux", reason: "", message: "" },
      }).name,
    ).toBe("Flux (kustomize-controller)");
    // The Committed event's adapter is what actually took the revision, so it
    // wins over a component a later failure blames.
    expect(
      reconcilerActor({
        phase: "",
        adapter: "flux",
        cause: { component: "kustomize", reason: "", message: "" },
      }).name,
    ).toBe("Flux (kustomize-controller)");
  });

  it("prints a name outside the table verbatim, never hiding it", () => {
    // Flux is the only reconciler this table knows (ADR-0028), but the wire's
    // own word must still reach the reader if a future or older server ever
    // names something else — a translation table is not licence to discard
    // what it does not recognise.
    expect(reconcilerActor({ phase: "", adapter: "widget-operator" }).name).toBe(
      "widget-operator",
    );
    expect(reconcilerActor({ phase: "", adapter: "widget-operator" }).known).toBe(
      true,
    );
  });

  it("passes an unknown adapter through verbatim instead of dropping it", () => {
    const actor = reconcilerActor({ phase: "", adapter: "spinnaker" });
    expect(actor).toEqual({ name: "spinnaker", known: true });
  });

  it("refuses to name a reconciler the wire never named", () => {
    // The ProjectDetailPage case: Status carries no mode and no adapter.
    const stage = buildRail({ phase: "Reconciling" }).stages[2];
    expect(stage?.actorKnown).toBe(false);
    expect(stage?.actor).toBe("reconciler");
    // The engine's own placeholder component is not a name either.
    expect(
      reconcilerActor({
        phase: "",
        cause: { component: "reconciler", reason: "", message: "" },
      }).known,
    ).toBe(false);
  });
});

describe("parseCause", () => {
  it("takes apart a Cause.String() with all three parts", () => {
    expect(
      parseCause("flux: NotReady: Kustomization ./apps is not ready"),
    ).toEqual({
      component: "flux",
      reason: "NotReady",
      message: "Kustomization ./apps is not ready",
    });
  });

  it("keeps colons that belong to the message", () => {
    expect(
      parseCause("flux: BuildFailed: kustomize build failed: missing resource"),
    ).toEqual({
      component: "flux",
      reason: "BuildFailed",
      message: "kustomize build failed: missing resource",
    });
  });

  it("handles a cause with no reason and one with neither", () => {
    expect(parseCause("kelson: something went wrong")).toEqual({
      component: "kelson",
      reason: "",
      message: "something went wrong",
    });
    expect(parseCause("")).toEqual({ component: "", reason: "", message: "" });
  });

  it("does not invent a component out of prose", () => {
    // "the reconciler refused this" is a sentence, not component/reason/message.
    const cause = parseCause("the reconciler refused this: no such namespace");
    expect(cause.component).toBe("");
    expect(cause.reason).toBe("");
    expect(cause.message).toBe("the reconciler refused this: no such namespace");
  });
});

describe("the rail's shape", () => {
  it("is the five phases, in order, always", () => {
    for (const phase of [...RAIL_PHASES, "Rejected", "Degraded", ""]) {
      expect(buildRail({ phase }).stages.map((s) => s.phase)).toEqual([
        ...RAIL_PHASES,
      ]);
    }
  });

  it("gives every stage a status the pill vocabulary knows", () => {
    const rail = buildRail({ phase: "Reconciling", stuck: true });
    expect(rail.stages.map((s) => s.status)).toEqual([
      "synced",
      "synced",
      "degraded",
      "unknown",
      "unknown",
    ]);
  });
});
