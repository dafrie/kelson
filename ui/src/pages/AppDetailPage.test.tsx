import { describe, expect, it } from "vitest";
import { fireEvent, screen } from "@testing-library/react";
import { Code, ConnectError, createRouterTransport } from "@connectrpc/connect";

import { DeployService } from "../gen/kelson/v1alpha1/deploy_pb";
import { SpecService } from "../gen/kelson/v1alpha1/spec_pb";
import { renderAt } from "../test/render";
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
