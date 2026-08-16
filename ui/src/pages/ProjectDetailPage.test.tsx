import { describe, expect, it } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import { Code, ConnectError, createRouterTransport } from "@connectrpc/connect";

import { DeployService } from "../gen/kelson/v1alpha1/deploy_pb";
import { ProfileService } from "../gen/kelson/v1alpha1/profile_pb";
import { SpecService } from "../gen/kelson/v1alpha1/spec_pb";
import { renderAt } from "../test/render";
import { healthEvent, transitionEvent, watchStub } from "../test/watch";
import { ProjectDetailPage } from "./ProjectDetailPage";

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
        // A build started without a workload observation client answers
        // Unimplemented for every environment it is asked about
        // (internal/api's Status). It says nothing about Rollback, which
        // reads a different seam (the Environment store).
        throw new ConnectError(
          "the observation plane is not available in this server: it was started without the backing seam",
          Code.Unimplemented,
        );
      }
      return {
        phase: "Healthy",
        revision: "8f2c1ad",
        namespace: "checkout-production",
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
  return renderAt(transport, "/projects/checkout", "/projects/:project", <ProjectDetailPage />);
}

/**
 * The matrix draws as soon as the documents arrive and fills in as each
 * column's Status lands, so a test that reads a cell waits for the columns to
 * stop saying they are reading.
 */
async function settled() {
  await screen.findByText("Components × environments");
  await waitFor(() => {
    expect(screen.queryAllByText("reading…")).toHaveLength(0);
  });
}

/**
 * The project page after the flow consolidation (#260): the matrix, and the
 * documents it is drawn from. The environment's own panel — status, verdicts,
 * data services, previews, Secrets — moved to the environment's route and is
 * tested there (`EnvironmentOverview.test.tsx`); what stays here is the grid
 * and the way into it.
 */
describe("ProjectDetailPage", () => {
  it("renders the stored documents byte-faithfully", async () => {
    renderDetail();

    expect(await screen.findByText(/kind: Project/)).toBeTruthy();
    expect(screen.getByText(/kind: Environment/)).toBeTruthy();
    expect(screen.getByText("Spec documents (2)")).toBeTruthy();
    // Editing is reached from the documents, next to the bytes it changes.
    expect(
      screen.getByRole("link", { name: "Edit configuration" }).getAttribute("href"),
    ).toBe("/projects/checkout/edit");
  });
});

/**
 * The matrix (#260): components down, environments across, one cell per pair.
 *
 * The fixture is deliberately mixed — a service and a worker that observation
 * reports on, a cron and a database it does not — because the interesting
 * property is that a cell says which of the two answers it is standing on.
 */
describe("ProjectDetailPage matrix", () => {
  const MULTI_PROJECT = `kind: Project
metadata:
  name: checkout

spec:
  image: ghcr.io/acme/checkout:1.4.2

  sources:
    - name: app
      git: https://github.com/acme/checkout
      ref: main

  components:
    - name: web
      port: 8080
      source: app

    - name: worker

    - name: nightly
      schedule: "0 3 * * *"

    - name: db
      kind: postgres
      preset: small
`;

  const PRODUCTION_YAML = `kind: Environment
metadata:
  name: production
spec:
  components:
    - name: web
      image: ghcr.io/acme/checkout:1.4.3
`;

  const multi = createRouterTransport((router) => {
    router.service(SpecService, {
      getSpec: () => ({
        spec: {
          project: "checkout",
          version: "7",
          environments: ["production", "staging"],
          documents: {
            project: new TextEncoder().encode(MULTI_PROJECT),
            environments: {
              production: new TextEncoder().encode(PRODUCTION_YAML),
              staging: new TextEncoder().encode(ENV_YAML),
            },
          },
        },
      }),
    });
    router.service(DeployService, {
      status: (req) => {
        if (req.environment === "staging") {
          return {
            phase: "Reconciling",
            revision: "c41b90e",
            namespace: "checkout-staging",
            verdicts: [],
          };
        }
        return {
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
      },
    });
  });

  function renderMulti() {
    return renderAt(multi, "/projects/checkout", "/projects/:project", <ProjectDetailPage />);
  }

  it("draws a row per component and a column per environment", async () => {
    const { container } = renderMulti();

    await settled();
    const rows = [...container.querySelectorAll("tbody tr")];
    expect(
      rows.map((row) => row.querySelector(".k-matrix__name")?.textContent),
    ).toEqual(["web", "worker", "nightly", "db"]);
    // The kind is a fact about the project and does not change per environment,
    // so it stays on the row header: the table of docs/model.md, a port being a
    // service, a schedule a cron, neither a worker, and a written kind read.
    expect(
      rows.map((row) => row.querySelector(".k-chip")?.textContent),
    ).toEqual(["service", "worker", "cron", "postgres"]);
    const columns = [...container.querySelectorAll("thead .k-matrix__env")];
    expect(columns.map((c) => c.textContent)).toEqual([
      "productionlive",
      "stagingdeploying",
    ]);
    // The revision is an environment's fact and is stated once per column.
    expect(
      [...container.querySelectorAll(".k-matrix__rev")].map((r) => r.textContent),
    ).toEqual(["8f2c1ad", "c41b90e"]);
  });

  it("gives each cell the component's own word, and links to the pair's page", async () => {
    const { container } = renderMulti();

    await settled();
    const cells = [...container.querySelectorAll(".k-cell")];
    const byKey = new Map(
      cells.map((c) => [c.getAttribute("href"), c] as const),
    );
    const web = byKey.get("/projects/checkout/production/components/web");
    const worker = byKey.get("/projects/checkout/production/components/worker");
    expect(web?.querySelector(".k-pill")?.textContent).toBe("live");
    // Production's phase is Healthy; this component is not, and the cell says
    // the component's answer rather than its environment's.
    expect(worker?.querySelector(".k-pill")?.textContent).toBe("unhealthy");
    expect(worker?.getAttribute("data-basis")).toBe("component");
    // The observation code, verbatim, is the fact under a cell in trouble.
    expect(worker?.querySelector(".k-cell__fact")?.textContent).toBe(
      "crash-loop-back-off",
    );
    // And the effective image (rule P3: the environment's pin wins) is the fact
    // under one that is not.
    expect(web?.querySelector(".k-cell__fact")?.textContent).toBe(
      "checkout:1.4.3",
    );
  });

  it("marks a cell that is showing the environment's word, not the component's", async () => {
    const { container } = renderMulti();

    await settled();
    const nightly = container.querySelector(
      '[href="/projects/checkout/production/components/nightly"]',
    );
    // Nothing observes a CronJob (internal/api's observeWorkloads probes
    // Deployments), so the cell borrows production's word and says so.
    expect(nightly?.getAttribute("data-basis")).toBe("environment");
    expect(nightly?.querySelector(".k-pill")?.textContent).toBe("live");
    expect(nightly?.querySelector(".k-cell__basis")?.textContent).toBe("env");
    expect(
      screen.getByText(
        /the environment's own status: nothing reports on this component separately/,
      ),
    ).toBeTruthy();
    // A database is in the same position and shows what it is sized as.
    const db = container.querySelector(
      '[href="/projects/checkout/staging/components/db"]',
    );
    expect(db?.querySelector(".k-cell__fact")?.textContent).toBe("small");
  });

  it("reads exactly one Status per environment — one per column", async () => {
    const calls: string[] = [];
    const counted = createRouterTransport((router) => {
      router.service(SpecService, {
        getSpec: () => ({
          spec: {
            project: "checkout",
            version: "7",
            environments: ["production", "staging"],
            documents: {
              project: new TextEncoder().encode(MULTI_PROJECT),
              environments: {},
            },
          },
        }),
      });
      router.service(DeployService, {
        status: (req) => {
          calls.push(req.environment);
          return { phase: "Healthy", revision: "8f2c1ad", verdicts: [] };
        },
      });
    });
    renderAt(counted, "/projects/checkout", "/projects/:project", <ProjectDetailPage />);

    await settled();
    await waitFor(() => {
      expect(calls.length).toBe(2);
    });
    // One per column and no more. The environment's own panel used to sit
    // below the grid and read the column it was already showing; it is a route
    // of its own now, so this page's calls are the columns' and nothing else.
    expect([...calls].sort()).toEqual(["production", "staging"]);
  });

  it("darkens only the column whose Status failed", async () => {
    const partial = createRouterTransport((router) => {
      router.service(SpecService, {
        getSpec: () => ({
          spec: {
            project: "checkout",
            version: "7",
            environments: ["production", "staging"],
            documents: {
              project: new TextEncoder().encode(MULTI_PROJECT),
              environments: {},
            },
          },
        }),
      });
      router.service(DeployService, {
        status: (req) => {
          if (req.environment === "staging") {
            throw new ConnectError(
              "api: building the delivery plane: connection refused",
              Code.Unavailable,
            );
          }
          return { phase: "Healthy", revision: "8f2c1ad", verdicts: [] };
        },
      });
    });
    const { container } = renderAt(
      partial,
      "/projects/checkout",
      "/projects/:project",
      <ProjectDetailPage />,
    );

    // One column is unreadable and the other is not: the calls are per column
    // precisely so the second one still answers.
    await settled();
    const columns = [...container.querySelectorAll("thead .k-matrix__env")];
    expect(columns[0]?.textContent).toBe("productionlive");
    expect(columns[1]?.textContent).toBe("stagingstatus unavailable");
    // And the cells under the dark column claim nothing.
    const cell = container.querySelector(
      '[href="/projects/checkout/staging/components/web"]',
    );
    expect(cell?.getAttribute("data-basis")).toBe("unread");
    expect(cell?.querySelector(".k-cell__fact")?.textContent).toBe("not read");
  });

  it("makes each column the way into that environment", async () => {
    const { container } = renderMulti();

    // The column header used to be a button that opened a panel below the grid.
    // The environment has a route now, so the header is a link and where a
    // reader ends up is in the address bar: from there the Overview, the logs,
    // the history and the three actions are all one strip away.
    await settled();
    expect(
      [...container.querySelectorAll("thead .k-matrix__env")].map((a) =>
        a.getAttribute("href"),
      ),
    ).toEqual(["/projects/checkout/production", "/projects/checkout/staging"]);
    // And no flow hangs off this page any more: they are the environment's.
    expect(screen.queryByRole("link", { name: "Deploy" })).toBeNull();
    expect(screen.queryByRole("link", { name: "History" })).toBeNull();
  });

  it("offers adding a component, and sends it to the editor that writes the spec", async () => {
    renderMulti();

    expect(
      (await screen.findByRole("link", { name: "Add component" })).getAttribute("href"),
    ).toBe("/projects/checkout/edit?add=component");
  });

  it("says so when the stored document declares no components", async () => {
    renderDetail();

    expect(
      await screen.findByText(/No components were read from the stored Project document/),
    ).toBeTruthy();
  });
});

describe("ProjectDetailPage data services", () => {
  const DATA_PROJECT = `kind: Project
metadata:
  name: checkout

spec:
  components:
    - name: db
      kind: postgres
      preset: small
    - name: web
      port: 8080
`;

  const withData = createRouterTransport((router) => {
    router.service(SpecService, {
      getSpec: () => ({
        spec: {
          project: "checkout",
          version: "7",
          environments: ["production"],
          documents: {
            project: new TextEncoder().encode(DATA_PROJECT),
            environments: { production: new TextEncoder().encode(ENV_YAML) },
          },
        },
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
            message: "web is available",
            remediation: "",
          },
          {
            resource: "Cluster/checkout-production/checkout-production-db",
            code: "healthy",
            healthy: true,
            degraded: false,
            message: "3/3 instances ready",
            remediation: "",
          },
        ],
      }),
    });
    router.service(ProfileService, {
      getProfile: () => ({
        yaml: new TextEncoder().encode(
          "storageClasses:\n    - name: local-path\n      provisioner: rancher.io/local-path\n      default: true\n      cloneCapability: none\n      cloneConfidence: observed\n",
        ),
      }),
    });
  });

  it("reads a data component's cell from the verdict about its own resource", async () => {
    const { container } = renderAt(
      withData,
      "/projects/checkout",
      "/projects/:project",
      <ProjectDetailPage />,
    );

    await settled();
    const db = container.querySelector(
      '[href="/projects/checkout/production/components/db"]',
    );
    // A CloudNativePG Cluster is named `<project>-<environment>-<component>`,
    // which is how a verdict about it is recognised as this component's.
    await waitFor(() => {
      expect(db?.getAttribute("data-basis")).toBe("component");
    });
    expect(db?.querySelector(".k-pill")?.textContent).toBe("live");
  });
});

describe("ProjectDetailPage live updates", () => {
  const WEB_PROJECT = `kind: Project
metadata:
  name: checkout

spec:
  components:
    - name: web
      port: 8080
`;

  it("moves the column a transition names, and the cell under it (#76)", async () => {
    const events = watchStub([]);
    const live = createRouterTransport((router) => {
      router.service(SpecService, {
        getSpec: () => ({
          spec: {
            project: "checkout",
            version: "7",
            environments: ["production"],
            documents: {
              project: new TextEncoder().encode(WEB_PROJECT),
              environments: {},
            },
          },
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
      events.install(router);
    });
    const { container } = renderAt(
      live,
      "/projects/checkout",
      "/projects/:project",
      <ProjectDetailPage />,
    );
    const web = () =>
      container.querySelector(
        '[href="/projects/checkout/production/components/web"]',
      );

    expect(
      await screen.findAllByText("deploying", { selector: ".k-pill" }),
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

    // The transition carries the revision, and the revision is the column's
    // fact: it is stated once, in the header, for every cell beneath it.
    expect(await screen.findByText("9d3f0aa")).toBeTruthy();

    // A health change is the *cell's* fact, and it overrules the column
    // downward: production stays Healthy, and web does not.
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

    await waitFor(() => {
      expect(web()?.querySelector(".k-pill")?.textContent).toBe("unhealthy");
    });
    // The code, verbatim, is what a cell in trouble shows instead of its image.
    expect(web()?.querySelector(".k-cell__fact")?.textContent).toBe(
      "crash-loop-back-off",
    );
    expect(
      [...container.querySelectorAll("thead .k-matrix__env")][0]?.textContent,
    ).toBe("productionlive");
  });
});
