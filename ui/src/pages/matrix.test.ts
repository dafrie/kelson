import { describe, expect, it } from "vitest";

import {
  componentsFromVerdicts,
  mergeVerdicts,
  needsAttention,
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
    message: "",
    remediation: "",
    ...partial,
  };
}

const PRODUCTION: EnvironmentRead = {
  environment: "production",
  phase: "Healthy",
  revision: "8f2c1ad",
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
    const reconciling: EnvironmentRead = { ...PRODUCTION, phase: "Reconciling" };
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
    // The wire's `degraded` is `!healthy && !stuck && IsFailure(code)`, so a
    // verdict that is merely not-ready-yet arrives with both false.
    const cell = readCell(
      PRODUCTION,
      verdict({
        resource: "Deployment/checkout-production/web",
        code: "progressing",
        healthy: false,
        degraded: false,
        message: "web is rolling out",
      }),
    );
    expect(cell.status.word).toBe("deploying");
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
