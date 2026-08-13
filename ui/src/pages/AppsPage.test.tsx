import { describe, expect, it } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import { Code, ConnectError, createRouterTransport } from "@connectrpc/connect";

import { ErrorSchema } from "../gen/kelson/v1alpha1/common_pb";
import { DeployService } from "../gen/kelson/v1alpha1/deploy_pb";
import { SpecService } from "../gen/kelson/v1alpha1/spec_pb";
import { renderAt } from "../test/render";
import { AppsPage } from "./AppsPage";

/**
 * The list's contract: one card per (project, environment), each carrying the
 * status its own Status call returned — and a card whose call failed says so
 * instead of borrowing a neighbour's green.
 */
const transport = createRouterTransport((router) => {
  router.service(SpecService, {
    listSpecs: () => ({
      specs: [
        { project: "checkout", version: "7", environments: ["production", "staging"] },
        { project: "hello", version: "2", environments: ["dev"] },
      ],
    }),
  });

  router.service(DeployService, {
    status: (req) => {
      if (req.environment === "production") {
        return {
          phase: "Healthy",
          revision: "8f2c1ad",
          detail: { resources: "4", live: "4", degraded: "0" },
        };
      }
      if (req.environment === "staging") {
        return {
          phase: "Degraded",
          revision: "c41b90e",
          cause: "web: 1 of 3 replicas are not ready",
          detail: { resources: "3", live: "2", degraded: "1" },
        };
      }
      // hello/dev has no cluster behind it. The RPC fails, with the delivery
      // plane's own structured error attached.
      throw new ConnectError(
        "api: building the delivery plane: dial tcp 127.0.0.1:6443: connect: connection refused",
        Code.Unavailable,
        undefined,
        [
          {
            desc: ErrorSchema,
            value: {
              code: "delivery/unavailable",
              message: "the cluster could not be reached",
            },
          },
        ],
      );
    },
  });
});

function renderApps() {
  return renderAt(transport, "/apps", "/apps", <AppsPage />);
}

describe("AppsPage", () => {
  it("renders one card per project and environment", async () => {
    renderApps();

    expect(await screen.findByText("production")).toBeTruthy();
    expect(screen.getByText("staging")).toBeTruthy();
    expect(screen.getByText("dev")).toBeTruthy();
    // The project name links to its detail screen, twice for checkout.
    const links = screen.getAllByRole("link", { name: "checkout" });
    expect(links).toHaveLength(2);
    expect(links[0]?.getAttribute("href")).toBe("/apps/checkout");
    expect(screen.getByText("2 projects")).toBeTruthy();
    expect(screen.getByText("3 environments")).toBeTruthy();
  });

  it("maps phases onto pills and shows revision, counts and cause", async () => {
    renderApps();

    const healthy = await screen.findByText("healthy", { selector: ".k-pill" });
    expect(healthy.dataset.status).toBe("synced");
    expect(
      screen.getByText("degraded", { selector: ".k-pill" }).dataset.status,
    ).toBe("degraded");
    expect(screen.getByText("8f2c1ad")).toBeTruthy();
    expect(screen.getByText("4/4 live")).toBeTruthy();
    expect(screen.getByText("2/3 live · 1 degraded")).toBeTruthy();
    expect(screen.getByText("web: 1 of 3 replicas are not ready")).toBeTruthy();
  });

  it("degrades a failing status to an honest pill with the server's reason", async () => {
    const { container } = renderApps();

    const pill = await screen.findByText("status unavailable");
    expect(pill.dataset.status).toBe("unknown");
    // The reason is the structured error, on the tooltip and in the card body.
    expect(pill.parentElement?.getAttribute("title")).toBe(
      "delivery/unavailable: the cluster could not be reached",
    );
    expect(
      screen.getByText("delivery/unavailable: the cluster could not be reached"),
    ).toBeTruthy();

    // The tally above the grid counts what is on screen, converging as each
    // card's own Status call settles — one of each, and no green claimed for
    // the environment nothing could be read for.
    await waitFor(() => {
      const counts = [...container.querySelectorAll(".k-count-group")].map(
        (el) => el.textContent,
      );
      expect(counts).toEqual(["1 synced", "1 degraded", "1 unknown"]);
    });
    // The healthy card is unaffected by its neighbour's failure.
    expect(screen.getByText("healthy", { selector: ".k-pill" })).toBeTruthy();
  });
});
