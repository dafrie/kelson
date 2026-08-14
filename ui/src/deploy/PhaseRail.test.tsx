import { describe, expect, it } from "vitest";
import { render, screen, within } from "@testing-library/react";
import { create } from "@bufbuild/protobuf";
import { createMemoryRouter, RouterProvider } from "react-router-dom";

import { ErrorSchema } from "../gen/kelson/v1alpha1/common_pb";
import { PhaseRail } from "./PhaseRail";
import type { RailInput } from "./rail";

/**
 * What the screen must show, as opposed to what the mapper computes: the five
 * stages with their actors, one diagnosis panel that is visibly a different
 * thing for each of the three failures, and an action that goes somewhere.
 */

function renderRail(input: RailInput, props: Partial<Parameters<typeof PhaseRail>[0]> = {}) {
  const router = createMemoryRouter(
    [
      {
        path: "/",
        element: (
          <PhaseRail
            input={input}
            project="checkout"
            environment="production"
            {...props}
          />
        ),
      },
      { path: "/projects/checkout/edit", element: <p>edit</p> },
      { path: "/projects/checkout/production/logs", element: <p>logs</p> },
    ],
    { initialEntries: ["/"] },
  );
  return render(<RouterProvider router={router} />);
}

function stage(phase: string): HTMLElement {
  const el = document.querySelector(`[data-phase="${phase}"]`);
  if (el === null) throw new Error(`no stage for ${phase}`);
  return el as HTMLElement;
}

function diagnosis(): HTMLElement {
  const el = document.querySelector("[data-diagnosis]");
  if (el === null) throw new Error("no diagnosis rendered");
  return el as HTMLElement;
}

describe("PhaseRail", () => {
  it("draws the five phases with the component responsible for each", () => {
    renderRail({ phase: "Reconciling", adapter: "flux" });

    for (const phase of ["Proposed", "Committed", "Reconciling", "Applied", "Healthy"]) {
      expect(stage(phase)).toBeTruthy();
    }
    expect(within(stage("Proposed")).getByText("kelson server")).toBeTruthy();
    expect(
      within(stage("Reconciling")).getByText("Flux (kustomize-controller)"),
    ).toBeTruthy();
    expect(
      within(stage("Applied")).getByText("Kubernetes (the cluster)"),
    ).toBeTruthy();
    expect(stage("Reconciling").dataset.state).toBe("current");
    expect(stage("Committed").dataset.state).toBe("done");
    expect(stage("Healthy").dataset.state).toBe("pending");
    // Nothing is wrong, so there is no diagnosis to answer.
    expect(document.querySelector("[data-diagnosis]")).toBeNull();
  });

  it("says the reconciler was not reported rather than naming a plausible one", () => {
    renderRail({ phase: "Reconciling" });
    expect(
      within(stage("Reconciling")).getByText(/not reported/),
    ).toBeTruthy();
    expect(screen.queryByText(/Flux/)).toBeNull();
  });

  it("shows a commit nothing picked up as its own state, with the config step", () => {
    renderRail({
      phase: "Committed",
      stuck: true,
      adapter: "flux",
      cause: {
        component: "flux",
        reason: "NotPickedUp",
        message: "revision abc1234 was committed but flux has not picked it up within 5m",
      },
    });

    // Not a terminal failure: the stage is wedged, not failed.
    expect(stage("Committed").dataset.state).toBe("stuck");
    expect(diagnosis().dataset.diagnosis).toBe("not-picked-up");
    expect(screen.getByText(/has not picked this commit up/)).toBeTruthy();
    expect(
      screen.getByText(/Check the delivery configuration/),
    ).toBeTruthy();
    expect(
      screen.getByText(
        /revision abc1234 was committed but flux has not picked it up within 5m/,
      ),
    ).toBeTruthy();
    expect(screen.getByRole("link", { name: "Review this environment" })).toBeTruthy();
  });

  it("shows a rejection with the manifest step and the structured error", () => {
    const error = create(ErrorSchema, {
      code: "delivery/apply-failed",
      message: "the reconciler rejected this revision",
      remediation: "fix the cause and redeploy; the change is not live",
      docsUrl: "https://kelson.dev/delivery/errors/delivery/apply-failed",
    });
    renderRail(
      {
        phase: "Rejected",
        adapter: "flux",
        cause: {
          component: "flux",
          reason: "BuildFailed",
          message: "kustomize build failed: missing deployment.yaml",
        },
      },
      { errors: [error] },
    );

    expect(stage("Reconciling").dataset.state).toBe("failed");
    expect(stage("Applied").dataset.state).toBe("pending");
    expect(diagnosis().dataset.diagnosis).toBe("rejected");
    expect(screen.getByText(/Fix the manifest/)).toBeTruthy();
    // The shared ErrorPanel renders the wire error next to the diagnosis.
    expect(screen.getByText("delivery/apply-failed")).toBeTruthy();
    expect(
      screen.getByText("fix the cause and redeploy; the change is not live"),
    ).toBeTruthy();
    expect(screen.getByRole("link", { name: "Edit configuration" })).toBeTruthy();
  });

  it("shows an applied-but-unhealthy revision with the workload step and a logs link", () => {
    renderRail({
      phase: "Degraded",
      adapter: "direct",
      cause: {
        component: "kubernetes",
        reason: "ProgressDeadlineExceeded",
        message: "Deployment/checkout has 1/3 replicas ready",
      },
    });

    expect(stage("Applied").dataset.state).toBe("done");
    expect(stage("Healthy").dataset.state).toBe("failed");
    expect(diagnosis().dataset.diagnosis).toBe("unhealthy");
    expect(screen.getByText(/Debug the workload/)).toBeTruthy();
    expect(screen.getByText(/Deployment\/checkout has 1\/3 replicas ready/)).toBeTruthy();
    const link = screen.getByRole("link", { name: "Open logs" });
    expect(link.getAttribute("href")).toBe("/projects/checkout/production/logs");
  });

  it("never renders the same panel for two different failures", () => {
    const kinds = new Set<string>();
    const steps = new Set<string>();
    for (const input of [
      { phase: "Committed", stuck: true },
      { phase: "Rejected" },
      { phase: "Degraded" },
    ] satisfies RailInput[]) {
      const view = renderRail(input);
      kinds.add(diagnosis().dataset.diagnosis ?? "");
      steps.add(diagnosis().textContent ?? "");
      view.unmount();
    }
    expect(kinds.size).toBe(3);
    expect(steps.size).toBe(3);
  });

  it("keeps the diagnosis and the actors in the compact form", () => {
    renderRail(
      {
        phase: "Degraded",
        adapter: "direct",
        cause: { component: "kubernetes", reason: "NotReady", message: "1/3 ready" },
      },
      { compact: true },
    );

    expect(document.querySelector(".k-rail--compact")).toBeTruthy();
    // Compact drops the per-stage state word and nothing else.
    expect(stage("Healthy").dataset.state).toBe("failed");
    expect(within(stage("Reconciling")).getByText("kelson (direct apply)")).toBeTruthy();
    expect(screen.getByText(/Debug the workload/)).toBeTruthy();
    expect(screen.getByRole("link", { name: "Open logs" })).toBeTruthy();
  });

  it("labels the rail for a screen reader and marks the current step", () => {
    renderRail({ phase: "Applied" }, { label: "Deployment phase for production" });
    expect(screen.getByLabelText("Deployment phase for production")).toBeTruthy();
    expect(stage("Applied").getAttribute("aria-current")).toBe("step");
    expect(stage("Healthy").getAttribute("aria-current")).toBeNull();
  });
});
