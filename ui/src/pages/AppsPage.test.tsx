import { describe, expect, it } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import { Code, ConnectError, createRouterTransport } from "@connectrpc/connect";

import { ErrorSchema } from "../gen/kelson/v1alpha1/common_pb";
import { DeployService } from "../gen/kelson/v1alpha1/deploy_pb";
import { EventService } from "../gen/kelson/v1alpha1/events_pb";
import { SpecService } from "../gen/kelson/v1alpha1/spec_pb";
import { renderAt } from "../test/render";
import {
  healthEvent,
  resyncEvent,
  transitionEvent,
  watchStub,
} from "../test/watch";
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
    // The one way into the create flow (#63).
    expect(
      screen.getByRole("link", { name: "New app" }).getAttribute("href"),
    ).toBe("/apps/new");
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

/**
 * The live half (#76): one watch stream for the whole grid, applied to the
 * cards in place.
 *
 * Each test builds its own server so the scripted events and the Status answers
 * belong together. The status counter is what proves a resync *relisted* rather
 * than merely being ignored.
 */
function liveServer(events: ReturnType<typeof watchStub>) {
  let statusCalls = 0;
  const transport = createRouterTransport((router) => {
    router.service(SpecService, {
      listSpecs: () => ({
        specs: [
          { project: "checkout", version: "7", environments: ["production"] },
        ],
      }),
    });
    router.service(DeployService, {
      status: () => {
        statusCalls += 1;
        return {
          phase: statusCalls > 1 ? "Healthy" : "Reconciling",
          revision: statusCalls > 1 ? "relisted" : "8f2c1ad",
          detail: { resources: "4", live: "4", degraded: "0" },
        };
      },
    });
    events.install(router);
  });
  return { transport, statusCalls: () => statusCalls };
}

describe("AppsPage live updates", () => {
  it("applies a status transition to the matching card in place", async () => {
    const events = watchStub([
      transitionEvent({
        project: "checkout",
        environment: "production",
        phase: "Healthy",
        previousPhase: "Reconciling",
        revision: "9d3f0aa",
        cause: "3/3 replicas ready",
      }),
    ]);
    const { transport, statusCalls } = liveServer(events);
    const { container } = renderAt(transport, "/apps", "/apps", <AppsPage />);

    // The fetched answer first, then the streamed one over the top of it.
    expect(
      await screen.findByText("reconciling", { selector: ".k-pill" }),
    ).toBeTruthy();
    const pill = await screen.findByText("healthy", { selector: ".k-pill" });
    expect(pill.dataset.status).toBe("synced");
    expect(screen.getByText("9d3f0aa")).toBeTruthy();
    expect(screen.getByText("3/3 replicas ready")).toBeTruthy();
    // In place: the card was updated, not refetched.
    expect(statusCalls()).toBe(1);
    // And the tally above the grid follows the card it counts.
    await waitFor(() => {
      const counts = [...container.querySelectorAll(".k-count-group")].map(
        (el) => el.textContent,
      );
      expect(counts).toEqual(["1 synced"]);
    });
  });

  it("shows an unhealthy workload from a health change, and clears it on recovery", async () => {
    const events = watchStub([
      healthEvent({
        project: "checkout",
        environment: "production",
        resource: "Deployment/checkout-production/web",
        code: "crash-loop-back-off",
        previousCode: "healthy",
        message: "web is restarting repeatedly (7 restarts)",
      }),
    ]);
    const { transport } = liveServer(events);
    renderAt(transport, "/apps", "/apps", <AppsPage />);

    expect(
      await screen.findByText(
        "Deployment/checkout-production/web: web is restarting repeatedly (7 restarts)",
      ),
    ).toBeTruthy();

    events.push(
      healthEvent({
        project: "checkout",
        environment: "production",
        resource: "Deployment/checkout-production/web",
        code: "healthy",
        previousCode: "crash-loop-back-off",
        healthy: true,
        cursor: "nonce.3",
      }),
    );
    // The recovery leaves no note behind: nothing is wrong now.
    await waitFor(() => {
      expect(
        screen.queryByText(
          "Deployment/checkout-production/web: web is restarting repeatedly (7 restarts)",
        ),
      ).toBeNull();
    });
  });

  it("relists on a resync instead of trusting the deltas it has", async () => {
    const events = watchStub([
      resyncEvent("the cursor is older than the retained event window"),
    ]);
    const { transport, statusCalls } = liveServer(events);
    renderAt(transport, "/apps", "/apps", <AppsPage />);

    expect(
      await screen.findByText("reconciling", { selector: ".k-pill" }),
    ).toBeTruthy();
    // The second Status answer is the one the card ends up showing.
    expect(await screen.findByText("relisted")).toBeTruthy();
    expect(statusCalls()).toBeGreaterThan(1);
  });

  it("shows the live indicator while the stream is connected", async () => {
    const events = watchStub([]);
    const { transport } = liveServer(events);
    renderAt(transport, "/apps", "/apps", <AppsPage />);

    expect(await screen.findByText("live")).toBeTruthy();
    expect(screen.getByText("live").dataset.state).toBe("live");
  });

  it("says nothing at all when the server cannot watch", async () => {
    const transport = createRouterTransport((router) => {
      router.service(SpecService, {
        listSpecs: () => ({
          specs: [
            { project: "checkout", version: "7", environments: ["production"] },
          ],
        }),
      });
      router.service(DeployService, {
        status: () => ({ phase: "Healthy", revision: "8f2c1ad" }),
      });
      router.service(EventService, {
        watch: async function* () {
          throw new ConnectError(
            "the delivery plane is not available in this server",
            Code.Unimplemented,
          );
        },
      });
    });
    renderAt(transport, "/apps", "/apps", <AppsPage />);

    expect(
      await screen.findByText("healthy", { selector: ".k-pill" }),
    ).toBeTruthy();
    await waitFor(() => {
      expect(screen.queryByText("live")).toBeNull();
      expect(screen.queryByText("reconnecting…")).toBeNull();
    });
  });

  it("aborts the stream on unmount", async () => {
    const events = watchStub([]);
    const { transport } = liveServer(events);
    const { unmount } = renderAt(transport, "/apps", "/apps", <AppsPage />);

    await screen.findByText("live");
    expect(events.opened()).toBe(1);
    unmount();
    // Resolves only when the client hung up; a stream left running would time
    // this test out rather than pass quietly.
    await events.aborted;
  });
});
