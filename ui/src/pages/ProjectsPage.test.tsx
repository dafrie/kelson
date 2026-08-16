import { describe, expect, it } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
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
import { ProjectsPage } from "./ProjectsPage";

/**
 * Home's contract (#260): the unit is a component in an environment, grouped by
 * project, and the rows come out of the verdicts the same one Status per
 * (project, environment) already carried.
 *
 * Three environments, deliberately different: one that reports per-component
 * verdicts, one that reports none (a build with no observation client — the
 * floor this page degrades to), and one whose cluster cannot be reached at all.
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
          namespace: "checkout-production",
          detail: { resources: "4", live: "4", degraded: "0" },
          verdicts: [
            {
              resource: "Deployment/checkout-production/web",
              code: "healthy",
              healthy: true,
              degraded: false,
              message: "Deployment/checkout-production/web healthy",
              remediation: "",
            },
            {
              resource: "Deployment/checkout-production/worker",
              code: "crash-loop-back-off",
              healthy: false,
              degraded: true,
              message: "worker is restarting repeatedly (7 restarts)",
              remediation: "kelson logs worker --at-termination",
            },
          ],
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

function renderProjects() {
  return renderAt(transport, "/projects", "/projects", <ProjectsPage />);
}

/** The block one environment's rows live in, found by its chip. */
function environmentBlock(name: string): HTMLElement {
  const chip = screen.getByText(name, { selector: ".k-chip" });
  const block = chip.closest(".k-envblock");
  if (block === null) throw new Error(`no block for ${name}`);
  return block as HTMLElement;
}

describe("ProjectsPage", () => {
  it("groups environments under their project and names each one", async () => {
    renderProjects();

    expect(await screen.findByText("production")).toBeTruthy();
    expect(screen.getByText("staging")).toBeTruthy();
    expect(screen.getByText("dev")).toBeTruthy();
    // One heading per project now, not one card per pair: the environments sit
    // under the name rather than repeating it.
    const links = screen.getAllByRole("link", { name: "checkout" });
    expect(links).toHaveLength(1);
    expect(links[0]?.getAttribute("href")).toBe("/projects/checkout");
    // The counts are mono and the words beside them are not (#260), so each
    // one is a span around a span and the assertion is about the line.
    const counted = (text: string) =>
      screen.getByText((_, el) => el?.tagName === "SPAN" && el.textContent === text);
    expect(counted("2 projects")).toBeTruthy();
    expect(counted("3 environments")).toBeTruthy();
    // The one way into the create flow (#63).
    expect(
      screen.getByRole("link", { name: "New project" }).getAttribute("href"),
    ).toBe("/projects/new");
  });

  it("lists one row per component, out of the verdicts Status already carried", async () => {
    renderProjects();

    await screen.findByRole("link", { name: "web" });
    const production = environmentBlock("production");
    const rows = [...production.querySelectorAll(".k-row")];
    expect(rows.map((r) => r.querySelector(".k-row__name")?.textContent)).toEqual([
      "web",
      "worker",
    ]);
    // Each row is addressable: the component in that environment, which is the
    // page the row is a summary of.
    expect(
      within(production).getByRole("link", { name: "web" }).getAttribute("href"),
    ).toBe("/projects/checkout/production/components/web");
    // The component's own word, not its environment's: production is Healthy
    // and one of its components is not.
    expect(
      rows[1]?.querySelector(".k-pill")?.textContent,
    ).toBe("unhealthy");
    expect(rows[0]?.querySelector(".k-pill")?.textContent).toBe("live");
  });

  it("says so when an environment reports nothing per component", async () => {
    renderProjects();

    await waitFor(() =>
      within(environmentBlock("staging")).getByText(
        "no per-component readings here",
      ),
    );
    const staging = environmentBlock("staging");
    // The honest floor: no verdicts means no rows, and the absence is stated
    // rather than filled with the environment's word repeated per component.
    expect(staging.querySelectorAll(".k-row")).toHaveLength(0);
    expect(
      within(staging).getByText("no per-component readings here"),
    ).toBeTruthy();
  });

  it("keeps the environment's own facts on the environment", async () => {
    renderProjects();

    await screen.findByText("8f2c1ad");
    const production = environmentBlock("production");
    expect(within(production).getByText("8f2c1ad")).toBeTruthy();
    expect(within(production).getByText("4/4 live")).toBeTruthy();
    const staging = environmentBlock("staging");
    expect(within(staging).getByText("2/3 live · 1 degraded")).toBeTruthy();
    expect(
      within(staging).getByText("web: 1 of 3 replicas are not ready"),
    ).toBeTruthy();
    expect(
      within(staging).getByText("unhealthy", { selector: ".k-pill" }),
    ).toBeTruthy();
  });

  it("degrades a failing status to an honest pill with the server's reason", async () => {
    const { container } = renderProjects();

    const pill = await screen.findByText("status unavailable");
    expect(pill.dataset.status).toBe("unknown");
    // The reason is the structured error, on the tooltip and in the block.
    expect(pill.parentElement?.getAttribute("title")).toBe(
      "delivery/unavailable: the cluster could not be reached",
    );
    expect(
      screen.getAllByText(
        "delivery/unavailable: the cluster could not be reached",
      ).length,
    ).toBeGreaterThan(0);

    // The tally above the page counts environments — the one unit that exists
    // whether or not anything reports per component — converging as each Status
    // call settles, and claiming no green for the one nothing could be read for.
    await waitFor(() => {
      const counts = [...container.querySelectorAll(".k-count-group")].map(
        (el) => el.textContent,
      );
      // Worst first: the tally is a to-do list, not an inventory.
      expect(counts).toEqual(["1 unhealthy", "1 live", "1 unknown"]);
    });
    // The healthy environment is unaffected by its neighbour's failure: its own
    // word and its healthy component's are both there.
    expect(
      within(environmentBlock("production")).getAllByText("live", {
        selector: ".k-pill",
      }),
    ).toHaveLength(2);
  });
});

describe("ProjectsPage attention band", () => {
  it("raises what needs attention, once, above everything else", async () => {
    renderProjects();

    const band = await screen.findByLabelText("Needs attention");
    const rows = [...band.querySelectorAll(".k-attention__row")].map(
      (row) => row.querySelector(".k-attention__what")?.textContent,
    );
    // The crash-looping component by name, and the environment nothing could be
    // read for as itself. The healthy component and the environment whose
    // trouble a component already owns are not repeated here.
    expect(rows).toEqual([
      "checkout · production · worker",
      "checkout · staging",
      "hello · dev",
    ]);
    expect(
      within(band).getByText("worker is restarting repeatedly (7 restarts)"),
    ).toBeTruthy();
    expect(
      within(band)
        .getByRole("link", { name: "checkout · production · worker" })
        .getAttribute("href"),
    ).toBe("/projects/checkout/production/components/worker");
  });

  it("is absent — not empty — when everything is live", async () => {
    const healthy = createRouterTransport((router) => {
      router.service(SpecService, {
        listSpecs: () => ({
          specs: [
            { project: "checkout", version: "7", environments: ["production"] },
          ],
        }),
      });
      router.service(DeployService, {
        status: () => ({
          phase: "Healthy",
          revision: "8f2c1ad",
          namespace: "checkout-production",
          verdicts: [
            {
              resource: "Deployment/checkout-production/web",
              code: "healthy",
              healthy: true,
              degraded: false,
              message: "Deployment/checkout-production/web healthy",
              remediation: "",
            },
          ],
        }),
      });
    });
    renderAt(healthy, "/projects", "/projects", <ProjectsPage />);

    expect(await screen.findByText("web")).toBeTruthy();
    // Quiet when healthy: no band, and no sentence reassuring anybody either.
    await waitFor(() => {
      expect(screen.queryByLabelText("Needs attention")).toBeNull();
    });
    expect(screen.queryByText(/Needs attention/)).toBeNull();
  });

  it("keeps work in flight out of the band", async () => {
    const deploying = createRouterTransport((router) => {
      router.service(SpecService, {
        listSpecs: () => ({
          specs: [
            { project: "checkout", version: "7", environments: ["production"] },
          ],
        }),
      });
      router.service(DeployService, {
        status: () => ({
          phase: "Reconciling",
          revision: "8f2c1ad",
          namespace: "checkout-production",
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
    });
    renderAt(deploying, "/projects", "/projects", <ProjectsPage />);

    // A rollout is not a problem: a band that fills up during every deploy is
    // a band people stop reading.
    expect(
      await screen.findAllByText("deploying", { selector: ".k-pill" }),
    ).toHaveLength(2);
    await waitFor(() => {
      expect(screen.queryByLabelText("Needs attention")).toBeNull();
    });
  });

  it("keeps a drifted-but-live environment out of the band, and still says it drifted", async () => {
    // Stale is a statement about revisions and not about health (#260): this
    // environment is on revision 44, which is up and well and simply is not
    // what the stored spec would publish. That is worth stating and is not a
    // to-do, so the line says it and the band stays absent.
    const drifted = createRouterTransport((router) => {
      router.service(SpecService, {
        listSpecs: () => ({
          specs: [
            { project: "checkout", version: "7", environments: ["production"] },
          ],
        }),
      });
      router.service(DeployService, {
        status: () => ({
          phase: "Healthy",
          answer: "live",
          revision: "44-1a2b3c4d",
          observedRevision: "44-1a2b3c4d",
          stale: true,
          namespace: "checkout-production",
          verdicts: [
            {
              resource: "Deployment/checkout-production/web",
              code: "healthy",
              healthy: true,
              degraded: false,
              stuck: false,
              message: "Deployment/checkout-production/web healthy",
              remediation: "",
            },
          ],
        }),
      });
    });
    renderAt(drifted, "/projects", "/projects", <ProjectsPage />);

    expect(
      await screen.findAllByText("live", { selector: ".k-pill" }),
    ).toHaveLength(2);
    expect(screen.getByText("older than the spec")).toBeTruthy();
    await waitFor(() => {
      expect(screen.queryByLabelText("Needs attention")).toBeNull();
    });
  });
});

/**
 * The live half (#76): one watch stream for the whole page, applied in place.
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
          namespace: "checkout-production",
          detail: { resources: "4", live: "4", degraded: "0" },
          verdicts: [
            {
              resource: "Deployment/checkout-production/web",
              code: "healthy",
              healthy: true,
              degraded: false,
              message: "Deployment/checkout-production/web healthy",
              remediation: "",
            },
          ],
        };
      },
    });
    events.install(router);
  });
  return { transport, statusCalls: () => statusCalls };
}

describe("ProjectsPage live updates", () => {
  it("applies a status transition to the environment it names, in place", async () => {
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
    const { container } = renderAt(transport, "/projects", "/projects", <ProjectsPage />);

    // The fetched answer first, then the streamed one over the top of it — on
    // the environment and on the component row that borrows its word.
    expect(
      await screen.findAllByText("deploying", { selector: ".k-pill" }),
    ).toBeTruthy();
    const pills = await screen.findAllByText("live", { selector: ".k-pill" });
    expect(pills).toHaveLength(2);
    // The environment's own meta line. The same revision is also on the
    // ticker's row for this transition (#260) — the meta line is what is
    // running, the row is what happened — so the assertion names which.
    expect(
      screen.getByText("9d3f0aa", { selector: ".k-card__rev .k-copy__value" }),
    ).toBeTruthy();
    expect(screen.getByText("3/3 replicas ready")).toBeTruthy();
    // In place: the environment was updated, not refetched.
    expect(statusCalls()).toBe(1);
    // And the tally follows what it counts.
    await waitFor(() => {
      const counts = [...container.querySelectorAll(".k-count-group")].map(
        (el) => el.textContent,
      );
      expect(counts).toEqual(["1 live"]);
    });
  });

  it("moves the component's own row on a health change, and clears it on recovery", async () => {
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
    renderAt(transport, "/projects", "/projects", <ProjectsPage />);

    // The row the event names, and the band it now belongs in.
    const band = await screen.findByLabelText("Needs attention");
    expect(
      within(band).getByRole("link", { name: "checkout · production · web" }),
    ).toBeTruthy();
    expect(
      screen.getAllByText(/web is restarting repeatedly \(7 restarts\)/).length,
    ).toBeGreaterThan(0);

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
    // The recovery leaves no note behind: nothing is wrong now, so the band
    // goes away entirely rather than emptying out.
    await waitFor(() => {
      expect(screen.queryByLabelText("Needs attention")).toBeNull();
    });
  });

  it("relists on a resync instead of trusting the deltas it has", async () => {
    const events = watchStub([
      resyncEvent("the cursor is older than the retained event window"),
    ]);
    const { transport, statusCalls } = liveServer(events);
    renderAt(transport, "/projects", "/projects", <ProjectsPage />);

    expect(
      await screen.findAllByText("deploying", { selector: ".k-pill" }),
    ).toBeTruthy();
    // The second Status answer is the one the page ends up showing.
    expect(await screen.findByText("relisted")).toBeTruthy();
    expect(statusCalls()).toBeGreaterThan(1);
  });

  it("shows the live indicator while the stream is connected", async () => {
    const events = watchStub([]);
    const { transport } = liveServer(events);
    renderAt(transport, "/projects", "/projects", <ProjectsPage />);

    expect(await screen.findByText("streaming")).toBeTruthy();
    expect(screen.getByText("streaming").dataset.state).toBe("live");
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
    renderAt(transport, "/projects", "/projects", <ProjectsPage />);

    expect(
      await screen.findByText("live", { selector: ".k-pill" }),
    ).toBeTruthy();
    await waitFor(() => {
      expect(screen.queryByText("streaming")).toBeNull();
      expect(screen.queryByText("reconnecting…")).toBeNull();
    });
  });

  it("aborts the stream on unmount", async () => {
    const events = watchStub([]);
    const { transport } = liveServer(events);
    const { unmount } = renderAt(transport, "/projects", "/projects", <ProjectsPage />);

    await screen.findByText("streaming");
    expect(events.opened()).toBe(1);
    unmount();
    // Resolves only when the client hung up; a stream left running would time
    // this test out rather than pass quietly.
    await events.aborted;
  });
});
