import { describe, expect, it } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import { Code, ConnectError, createRouterTransport } from "@connectrpc/connect";

import { DeployService } from "../gen/kelson/v1alpha1/deploy_pb";
import { PreviewService } from "../gen/kelson/v1alpha1/preview_pb";
import { ProfileService } from "../gen/kelson/v1alpha1/profile_pb";
import { SecretService } from "../gen/kelson/v1alpha1/secret_pb";
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

  // The environment's Secrets panel (#116) reads this as soon as the page
  // opens, so the stub answers it rather than leaving a failed list between the
  // assertions below.
  router.service(SecretService, {
    listSecrets: () => ({
      namespace: "checkout-production",
      secrets: [
        {
          name: "checkout-db",
          namespace: "checkout-production",
          keys: ["url"],
          ageSeconds: 3600n,
        },
      ],
    }),
  });

  // The previews section (ADR-0017) reads this per environment, like the
  // Secrets panel above. This project declares none, which is the ordinary
  // answer and the one that draws the empty state.
  router.service(PreviewService, {
    listPreviews: (req) => ({
      project: "checkout",
      environment: req.environment,
      namespace: `checkout-${req.environment}`,
      mode: "direct",
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

describe("ProjectDetailPage", () => {
  it("shows the phase and the workload verdicts, which are different answers", async () => {
    renderDetail();

    expect(
      await screen.findByText("phase Healthy"),
    ).toBeTruthy();
    expect(screen.getAllByText("live", { selector: ".k-pill" }).length).toBeGreaterThan(0);
    expect(screen.getByText("8f2c1ad")).toBeTruthy();
    // A Healthy phase with a crash-looping workload underneath is exactly the
    // pair the two signals exist to tell apart.
    expect(screen.getAllByText("crash-loop-back-off").length).toBeGreaterThan(0);
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
    ).toBe("/projects/checkout/edit");
  });

  it("lists the environment's kelson-managed Secrets beside it (#116)", async () => {
    renderDetail();

    // Per environment, because a Secret's lifecycle is the environment's: the
    // panel addresses the namespace this tab names, not the project's.
    expect(await screen.findByText("Secrets (1)")).toBeTruthy();
    expect(screen.getByText("checkout-db")).toBeTruthy();
    expect(screen.getByText("url")).toBeTruthy();
    expect(
      screen.getByRole("button", { name: "Write the Secret" }),
    ).toBeTruthy();
  });

  it("links each environment to its release history (#67)", async () => {
    renderDetail();

    expect(
      (await screen.findByRole("link", { name: "History" })).getAttribute("href"),
    ).toBe("/projects/checkout/production/history");
  });

  it("offers promotion into the environment on screen, named from its side (#11)", async () => {
    renderDetail();

    // The label is the target's perspective: this environment is where the
    // pins land, and the source is picked on the promote screen.
    expect(
      (
        await screen.findByRole("link", {
          name: "Promote into this environment",
        })
      ).getAttribute("href"),
    ).toBe("/projects/checkout/production/promote");
  });

  it("disables promotion when the project declares nowhere to promote from", async () => {
    const alone = createRouterTransport((router) => {
      router.service(SpecService, {
        getSpec: () => ({
          spec: { project: "checkout", version: "7", environments: ["production"] },
        }),
      });
      router.service(DeployService, {
        status: () => ({ phase: "Healthy", revision: "8f2c1ad", verdicts: [] }),
      });
    });
    renderAt(alone, "/projects/checkout", "/projects/:project", <ProjectDetailPage />);

    const promote = await screen.findByRole("button", {
      name: "Promote into this environment",
    });
    expect((promote as HTMLButtonElement).disabled).toBe(true);
    expect(promote.getAttribute("title")).toContain(
      "declares no other environment to promote from",
    );
  });

  it("keeps rollback offered when Status fails, like every other action", async () => {
    // Status.Unimplemented now means only that this server has no workload
    // observation client (internal/api's Status, R2 #225) — it says nothing
    // about whether the Environment store Rollback needs is configured, so
    // this page must not read it as "rollback cannot work". Rollback's own
    // screen has the real precondition and its own honest error if it fails.
    renderDetail();

    // The tab, not the matrix column header: the column button's accessible
    // name carries its status word too.
    fireEvent.click(await screen.findByRole("button", { name: "staging" }));

    const rollback = await screen.findByRole("link", { name: "Rollback" });
    expect(rollback.getAttribute("href")).toBe(
      "/projects/checkout/staging/rollback",
    );
    // Deploy stays available too: a render dry-run needs no cluster at all.
    expect(screen.getByRole("link", { name: "Deploy" })).toBeTruthy();
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

  it("reads one Status per environment and reuses it for the panel below", async () => {
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
    // One per column, and the panel below the matrix is the column already
    // read rather than a second call for an environment on screen twice.
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

  it("gives a database its own section and keeps it out of the workload list", async () => {
    renderAt(withData, "/projects/checkout", "/projects/:project", <ProjectDetailPage />);

    expect(await screen.findByText("Data services (1)")).toBeTruthy();
    expect(screen.getByText("1 instance")).toBeTruthy();
    // The spec is readable before the status is; the health arrives with it.
    expect(await screen.findByText("3/3 instances ready")).toBeTruthy();
    // The Cluster verdict belongs to the data section; the workload count is
    // the Deployment alone.
    expect(screen.getByText("Workloads (1)")).toBeTruthy();
    expect(
      screen.getByText(
        "Fast branching unavailable — your storage class (local-path) has no snapshot driver.",
      ),
    ).toBeTruthy();
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
    renderAt(live, "/projects/checkout", "/projects/:project", <ProjectDetailPage />);

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
    expect(
      await screen.findAllByText("live", { selector: ".k-pill" }),
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

  /**
   * The rail on this screen is fed by Status plus the stream's deltas, so it
   * has to move when the stream says something moved — and it has to keep the
   * two failures it can be in apart while doing it (#68).
   */
  it("moves the compact rail as the stream reports transitions", async () => {
    const events = watchStub([]);
    const live = createRouterTransport((router) => {
      router.service(SpecService, {
        getSpec: () => ({
          spec: { project: "checkout", version: "7", environments: ["production"] },
        }),
      });
      router.service(DeployService, {
        status: () => ({ phase: "Committed", revision: "8f2c1ad", verdicts: [] }),
      });
      events.install(router);
    });
    const { container } = renderAt(
      live,
      "/projects/checkout",
      "/projects/:project",
      <ProjectDetailPage />,
    );

    // Compact, and honest about what it cannot know: StatusResponse names no
    // adapter, so the reconciler stage stays unnamed.
    await waitFor(() => {
      expect(container.querySelector(".k-rail--compact")).toBeTruthy();
    });
    const stageState = (phase: string) =>
      container.querySelector(`[data-phase="${phase}"]`)?.getAttribute("data-state");
    expect(stageState("Committed")).toBe("current");
    expect(screen.getByText(/not reported/)).toBeTruthy();
    expect(container.querySelector("[data-diagnosis]")).toBeNull();

    // The engine's own stuck reason arrives inside the flattened cause string:
    // the rail must read it as stuck-in-Committed, not as a generic failure.
    events.push(
      transitionEvent({
        project: "checkout",
        environment: "production",
        phase: "Committed",
        previousPhase: "Committed",
        revision: "8f2c1ad",
        cause:
          "flux: NotPickedUp: revision 8f2c1ad was committed but flux has not picked it up within 5m",
      }),
    );

    await waitFor(() => {
      expect(stageState("Committed")).toBe("stuck");
    });
    expect(
      container.querySelector('[data-diagnosis="not-picked-up"]'),
    ).toBeTruthy();
    expect(
      screen.getByText(/Check this environment's configuration/),
    ).toBeTruthy();
    // The cause named a component, so the reconciler stage can be named now.
    expect(screen.getByText("Flux (kustomize-controller)")).toBeTruthy();

    // A rejection is a different failure and gets a different answer.
    events.push(
      transitionEvent({
        project: "checkout",
        environment: "production",
        phase: "Rejected",
        previousPhase: "Committed",
        revision: "8f2c1ad",
        cause: "flux: BuildFailed: kustomize build failed: missing deployment.yaml",
      }),
    );

    await waitFor(() => {
      expect(container.querySelector('[data-diagnosis="rejected"]')).toBeTruthy();
    });
    expect(screen.getByText(/Fix the manifest/)).toBeTruthy();
    expect(stageState("Committed")).toBe("failed");
    expect(stageState("Applied")).toBe("pending");
  });
});
