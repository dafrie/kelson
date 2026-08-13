import { describe, expect, it } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import { Code, ConnectError, createRouterTransport } from "@connectrpc/connect";

import { DeployService } from "../gen/kelson/v1alpha1/deploy_pb";
import { SpecService } from "../gen/kelson/v1alpha1/spec_pb";
import { renderAt } from "../test/render";
import { healthEvent, transitionEvent, watchStub } from "../test/watch";
import { AppDetailPage } from "./AppDetailPage";

const PROJECT_YAML = "kind: Project\nmetadata:\n  name: checkout\n";
const ENV_YAML = "kind: Environment\nmetadata:\n  name: production\n";

const transport = createRouterTransport((router) => {
  router.service(SpecService, {
    getSpec: () => ({
      spec: {
        project: "checkout",
        version: "7",
        environments: ["production", "staging"],
        documents: {
          project: new TextEncoder().encode(PROJECT_YAML),
          environments: { production: new TextEncoder().encode(ENV_YAML) },
        },
      },
    }),
  });

  router.service(DeployService, {
    status: (req) => {
      if (req.environment === "staging") {
        // A build started without a delivery plane answers Unimplemented for
        // every environment it is asked about.
        throw new ConnectError(
          "the delivery plane is not available in this server: it was started without the backing seam",
          Code.Unimplemented,
        );
      }
      return {
        phase: "Healthy",
        revision: "8f2c1ad",
        detail: { resources: "4", live: "4" },
        verdicts: [
          {
            resource: "Deployment/checkout-production/web",
            code: "crash-loop-back-off",
            healthy: false,
            degraded: true,
            message: "web is restarting repeatedly (7 restarts)",
            remediation: "kelson logs web --at-termination",
          },
        ],
      };
    },
  });
});

function renderDetail() {
  return renderAt(transport, "/apps/checkout", "/apps/:project", <AppDetailPage />);
}

describe("AppDetailPage", () => {
  it("shows the phase and the workload verdicts, which are different answers", async () => {
    renderDetail();

    expect(await screen.findByText("healthy", { selector: ".k-pill" })).toBeTruthy();
    expect(screen.getByText("8f2c1ad")).toBeTruthy();
    // A Healthy phase with a crash-looping workload underneath is exactly the
    // pair the two signals exist to tell apart.
    expect(screen.getByText("crash-loop-back-off")).toBeTruthy();
    expect(screen.getByText("web is restarting repeatedly (7 restarts)")).toBeTruthy();
    expect(screen.getByText("fix:")).toBeTruthy();
  });

  it("renders the stored documents byte-faithfully", async () => {
    renderDetail();

    expect(await screen.findByText(/kind: Project/)).toBeTruthy();
    expect(screen.getByText(/kind: Environment/)).toBeTruthy();
    expect(screen.getByText("Spec documents (2)")).toBeTruthy();
    // Editing is reached from the documents, next to the bytes it changes.
    expect(
      screen.getByRole("link", { name: "Edit configuration" }).getAttribute("href"),
    ).toBe("/apps/checkout/edit");
  });

  it("disables rollback with the server's own reason when there is no delivery plane", async () => {
    renderDetail();

    fireEvent.click(await screen.findByRole("button", { name: "staging" }));

    const rollback = await screen.findByRole("button", { name: "Rollback" });
    expect((rollback as HTMLButtonElement).disabled).toBe(true);
    expect(rollback.getAttribute("title")).toContain(
      "it was started without the backing seam",
    );
    // Deploy stays available: a render dry-run needs no cluster at all.
    expect(screen.getByRole("link", { name: "Deploy" })).toBeTruthy();
  });
});

describe("AppDetailPage live updates", () => {
  it("updates the status block and the verdict the event names (#76)", async () => {
    const events = watchStub([]);
    const live = createRouterTransport((router) => {
      router.service(SpecService, {
        getSpec: () => ({
          spec: {
            project: "checkout",
            version: "7",
            environments: ["production"],
          },
        }),
      });
      router.service(DeployService, {
        status: () => ({
          phase: "Reconciling",
          revision: "8f2c1ad",
          verdicts: [
            {
              resource: "Deployment/checkout-production/web",
              code: "progressing",
              healthy: false,
              degraded: false,
              message: "web is rolling out",
              remediation: "wait for the rollout to finish",
            },
          ],
        }),
      });
      events.install(router);
    });
    renderAt(live, "/apps/checkout", "/apps/:project", <AppDetailPage />);

    expect(
      await screen.findByText("reconciling", { selector: ".k-pill" }),
    ).toBeTruthy();

    events.push(
      transitionEvent({
        project: "checkout",
        environment: "production",
        phase: "Healthy",
        previousPhase: "Reconciling",
        revision: "9d3f0aa",
        cause: "3/3 replicas ready",
      }),
    );
    expect(
      await screen.findByText("healthy", { selector: ".k-pill" }),
    ).toBeTruthy();
    expect(screen.getByText("9d3f0aa")).toBeTruthy();
    expect(screen.getByText("3/3 replicas ready")).toBeTruthy();

    events.push(
      healthEvent({
        project: "checkout",
        environment: "production",
        resource: "Deployment/checkout-production/web",
        code: "crash-loop-back-off",
        previousCode: "progressing",
        message: "web is restarting repeatedly (7 restarts)",
      }),
    );
    expect(await screen.findByText("crash-loop-back-off")).toBeTruthy();
    expect(
      screen.getByText("web is restarting repeatedly (7 restarts)"),
    ).toBeTruthy();
    // The remediation belonged to the code that was replaced, so it goes with
    // it rather than staying on screen pointing at the wrong problem.
    await waitFor(() => {
      expect(screen.queryByText("fix:")).toBeNull();
    });
  });
});
