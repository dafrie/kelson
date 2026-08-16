import { describe, expect, it } from "vitest";

import {
  componentsFromVerdicts,
  deliveryFacts,
  mergeVerdicts,
  needsAttention,
  NO_DELIVERY,
  readCell,
  shortImage,
  verdictFor,
  type EnvironmentRead,
  type VerdictRow,
} from "./matrix";

function verdict(partial: Partial<VerdictRow> & { resource: string }): VerdictRow {
  return {
    code: "healthy",
    healthy: true,
    degraded: false,
    stuck: false,
    message: "",
    remediation: "",
    ...partial,
  };
}

const PRODUCTION: EnvironmentRead = {
  ...NO_DELIVERY,
  environment: "production",
  phase: "Healthy",
  answer: "live",
  revision: "45-8f2c1ad0",
  observedRevision: "45-8f2c1ad0",
  cause: "",
  namespace: "checkout-production",
  verdicts: [
    verdict({ resource: "Deployment/checkout-production/web" }),
    verdict({
      resource: "Deployment/checkout-production/worker",
      code: "crash-loop-back-off",
      healthy: false,
      degraded: true,
      message: "worker is restarting repeatedly (7 restarts)",
    }),
    verdict({
      resource: "Cluster/checkout-production/checkout-production-db",
      message: "3/3 instances ready",
    }),
    verdict({ resource: "ExternalSecret/checkout-production/checkout-db" }),
  ],
  read: true,
};

describe("verdictFor", () => {
  it("finds a workload by the name the renderer gives its Deployment", () => {
    // internal/renderer/workload.go names a component's Deployment after the
    // component; that is the whole of the correlation.
    expect(
      verdictFor(PRODUCTION, {
        project: "checkout",
        component: "web",
        kind: "service",
      })?.code,
    ).toBe("healthy");
  });

  it("finds a database by the compound name its Cluster carries", () => {
    expect(
      verdictFor(PRODUCTION, {
        project: "checkout",
        component: "db",
        kind: "postgres",
      })?.message,
    ).toBe("3/3 instances ready");
  });

  it("does not mistake another namespace's workload for this one's", () => {
    const elsewhere: EnvironmentRead = {
      ...PRODUCTION,
      verdicts: [verdict({ resource: "Deployment/other-namespace/web" })],
    };
    expect(
      verdictFor(elsewhere, {
        project: "checkout",
        component: "web",
        kind: "service",
      }),
    ).toBeUndefined();
  });

  it("has nothing to say about a kind nothing observes", () => {
    // A CronJob and a HelmRelease are not probed, so there is no verdict and
    // the cell falls back to the environment rather than inventing one.
    expect(
      verdictFor(PRODUCTION, {
        project: "checkout",
        component: "nightly",
        kind: "cron",
      }),
    ).toBeUndefined();
  });
});

describe("readCell", () => {
  it("keeps the environment's word for a healthy component", () => {
    const cell = readCell(
      PRODUCTION,
      verdictFor(PRODUCTION, {
        project: "checkout",
        component: "web",
        kind: "service",
      }),
    );
    expect(cell.status.word).toBe("live");
    expect(cell.basis).toBe("component");
  });

  it("does not call a green Deployment live inside an environment that is not", () => {
    const reconciling: EnvironmentRead = {
      ...PRODUCTION,
      phase: "Reconciling",
      answer: "progressing",
    };
    const cell = readCell(
      reconciling,
      verdictFor(reconciling, {
        project: "checkout",
        component: "web",
        kind: "service",
      }),
    );
    // The delivery answer is the wider claim: the pods are up, the change is
    // still arriving.
    expect(cell.status.word).toBe("deploying");
  });

  it("lets a failing component overrule its environment", () => {
    const cell = readCell(
      PRODUCTION,
      verdictFor(PRODUCTION, {
        project: "checkout",
        component: "worker",
        kind: "worker",
      }),
    );
    expect(cell.status.word).toBe("unhealthy");
    expect(cell.code).toBe("crash-loop-back-off");
    expect(cell.detail).toBe("worker is restarting repeatedly (7 restarts)");
  });

  it("reads a wait state as work in flight and never as a failure", () => {
    // All three false is a rollout still in flight.
    const cell = readCell(
      PRODUCTION,
      verdict({
        resource: "Deployment/checkout-production/web",
        code: "workload/progressing",
        healthy: false,
        degraded: false,
        message: "web is rolling out",
      }),
    );
    expect(cell.status.word).toBe("deploying");
  });

  it("says stuck when the probe gave up, and it is not the wait state", () => {
    // The fourth state the wire can finally report. `code` stays a WAIT code —
    // `stuck` is a timeout verdict and deliberately not a failure — so the only
    // thing that separates it from the row above is the flag.
    const stuck = verdict({
      resource: "Deployment/checkout-production/web",
      code: "workload/progressing",
      healthy: false,
      degraded: false,
      stuck: true,
      message: "no progress for 10m0s",
    });
    const cell = readCell(PRODUCTION, stuck);
    expect(cell.status.word).toBe("stuck");
    expect(cell.code).toBe("workload/progressing");

    const waiting = readCell(PRODUCTION, { ...stuck, stuck: false });
    expect(waiting.status.word).toBe("deploying");
    const failing = readCell(PRODUCTION, { ...stuck, stuck: false, degraded: true });
    expect(failing.status.word).toBe("unhealthy");
    // Three verdicts, one code, three words: the flag is what tells them apart.
    expect(
      new Set([cell.status.word, waiting.status.word, failing.status.word]).size,
    ).toBe(3);
  });

  it("takes the environment's word from the answer, not from the phase", () => {
    // `stuck` is not a phase — a deployment that gave up waiting is wedged in
    // whatever phase it reached — so a cell deriving from the phase alone would
    // read `waiting` and say nothing is wrong.
    const wedged: EnvironmentRead = {
      ...PRODUCTION,
      phase: "Committed",
      answer: "stuck",
      verdicts: [],
    };
    expect(readCell(wedged, undefined).status.word).toBe("stuck");
    // The same read with no answer is the honest fallback, and it is the older
    // claim: the phase can only say that something is expected to act.
    expect(readCell({ ...wedged, answer: "" }, undefined).status.word).toBe(
      "waiting",
    );
  });

  it("borrows the environment's word, and says that is what it did", () => {
    const cell = readCell(PRODUCTION, undefined);
    expect(cell.status.word).toBe("live");
    expect(cell.basis).toBe("environment");
  });

  it("claims nothing at all when the status was not read", () => {
    const cell = readCell(
      { ...PRODUCTION, read: false, phase: "Healthy" },
      undefined,
    );
    expect(cell.status.word).toBe("unknown");
    expect(cell.basis).toBe("unread");
  });
});

describe("componentsFromVerdicts", () => {
  it("names the components a status reported on, and nothing else", () => {
    expect(
      componentsFromVerdicts(PRODUCTION, "checkout").map((r) => r.component),
    ).toEqual(["web", "worker", "db"]);
    // The ExternalSecret is the environment's resource, not a component's.
  });

  it("is empty when the server has no observation client", () => {
    expect(
      componentsFromVerdicts({ ...PRODUCTION, verdicts: [] }, "checkout"),
    ).toEqual([]);
  });
});

describe("mergeVerdicts", () => {
  it("applies an event over the fetched row and drops the stale remediation", () => {
    const rows = mergeVerdicts(
      [
        verdict({
          resource: "Deployment/checkout-production/web",
          code: "progressing",
          healthy: false,
          remediation: "wait for the rollout to finish",
        }),
      ],
      {
        "Deployment/checkout-production/web": {
          code: "crash-loop-back-off",
          healthy: false,
          message: "web is restarting repeatedly (7 restarts)",
        },
      },
    );
    expect(rows).toHaveLength(1);
    expect(rows[0]?.code).toBe("crash-loop-back-off");
    // The remediation was the fix for the code it replaced.
    expect(rows[0]?.remediation).toBe("");
  });

  it("drops a stuck flag the event cannot re-state", () => {
    // HealthChange carries a code, a verdict and a message. `stuck` was the
    // probe's timeout verdict about the code this event just replaced, so
    // keeping it would attach a finding to a reading that never produced one.
    const rows = mergeVerdicts(
      [
        verdict({
          resource: "Deployment/checkout-production/web",
          code: "workload/progressing",
          healthy: false,
          stuck: true,
        }),
      ],
      {
        "Deployment/checkout-production/web": {
          code: "healthy",
          healthy: true,
          message: "",
        },
      },
    );
    expect(rows[0]?.stuck).toBe(false);
  });

  it("appends a workload the event names and the fetch never saw", () => {
    const rows = mergeVerdicts([], {
      "Deployment/checkout-production/new": {
        code: "healthy",
        healthy: true,
        message: "",
      },
    });
    expect(rows.map((r) => r.resource)).toEqual([
      "Deployment/checkout-production/new",
    ]);
  });
});

describe("deliveryFacts", () => {
  const READ = {
    phase: "Healthy",
    answer: "live",
    revision: "44-1a2b3c4d",
    observedRevision: "44-1a2b3c4d",
    cause: "",
    stale: true,
  };

  it("keeps the poll's own answer and drift when nothing streamed", () => {
    expect(deliveryFacts(READ, undefined)).toEqual(READ);
  });

  it("voids the answer a transition did not re-state", () => {
    // The answer was the engine's verdict about the phase that was polled. A
    // transition to another phase is not a verdict about it, so the word falls
    // back to being derived — which is what this UI did before the field.
    const next = deliveryFacts(READ, {
      phase: "Reconciling",
      revision: "45-9e8d7c6b",
      cause: "",
    });
    expect(next.answer).toBe("");
    expect(next.phase).toBe("Reconciling");
  });

  it("voids the drift when the transition names another revision", () => {
    // `stale` was computed against the revision that was polled, and there is
    // nothing here to compare the new one with. The wire's own false means
    // exactly that, and this UI renders it as no claim rather than "current".
    expect(
      deliveryFacts(READ, {
        phase: "Reconciling",
        revision: "45-9e8d7c6b",
        cause: "",
      }).stale,
    ).toBe(false);
  });

  it("keeps the drift when the transition is about the same revision", () => {
    expect(
      deliveryFacts(READ, {
        phase: "Healthy",
        revision: "44-1a2b3c4d",
        cause: "",
      }).stale,
    ).toBe(true);
  });

  it("is empty for an environment nothing answered for", () => {
    expect(deliveryFacts(undefined, undefined)).toEqual(NO_DELIVERY);
  });
});

describe("needsAttention", () => {
  it("raises trouble and the unreadable, and leaves work in flight alone", () => {
    expect(needsAttention("unhealthy")).toBe(true);
    expect(needsAttention("failed")).toBe(true);
    expect(needsAttention("stuck")).toBe(true);
    // "we could not tell" is exactly the state a person has to go and look at.
    expect(needsAttention("unknown")).toBe(true);
    expect(needsAttention("deploying")).toBe(false);
    expect(needsAttention("waiting")).toBe(false);
    expect(needsAttention("live")).toBe(false);
    // Nothing is trying, on purpose: that is not a to-do.
    expect(needsAttention("suspended")).toBe(false);
  });

  it("does not raise an environment for having drifted", () => {
    // Stale is a statement about revisions, not about health: revision 44 is up
    // and well and simply is not 45, and a rollback pin is stale on purpose.
    // The band is keyed on the word, and drift never becomes one.
    const drifted: EnvironmentRead = {
      ...PRODUCTION,
      stale: true,
      verdicts: [],
    };
    const cell = readCell(drifted, undefined);
    expect(cell.status.word).toBe("live");
    expect(needsAttention(cell.status.word)).toBe(false);

    // Stale AND wedged is in the band, and it is `stuck` that puts it there.
    const wedged = readCell({ ...drifted, answer: "stuck" }, undefined);
    expect(needsAttention(wedged.status.word)).toBe(true);
  });
});

describe("shortImage", () => {
  it("keeps the end of a tagged reference, which is what differs per cell", () => {
    expect(shortImage("ghcr.io/acme/checkout:1.4.2")).toBe("checkout:1.4.2");
    expect(shortImage("checkout:1.4.2")).toBe("checkout:1.4.2");
    expect(shortImage("")).toBe("");
  });

  it("elides a digest's middle rather than printing 71 characters", () => {
    expect(
      shortImage(
        "ghcr.io/acme/checkout@sha256:0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0",
      ),
    ).toBe("checkout@sha256:0f1e2d3…");
  });
});
