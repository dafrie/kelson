import { describe, expect, it } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import { Code, ConnectError, createRouterTransport } from "@connectrpc/connect";

import { DeployService } from "../gen/kelson/v1alpha1/deploy_pb";
import { PreviewService } from "../gen/kelson/v1alpha1/preview_pb";
import { ProfileService } from "../gen/kelson/v1alpha1/profile_pb";
import { SecretService } from "../gen/kelson/v1alpha1/secret_pb";
import { SpecService } from "../gen/kelson/v1alpha1/spec_pb";
import { renderRoutes } from "../test/render";
import { environmentRoutes } from "../test/routes";
import { healthEvent, transitionEvent, watchStub } from "../test/watch";

/**
 * The Overview tab (#260).
 *
 * This panel was the block under the project page's matrix, selected by a strip
 * of environment buttons; it is the environment's index route now. The subjects
 * are the ones it always had — the phase and the workload verdicts are different
 * answers, a database is not a workload, the Secrets belong to the environment,
 * the rail moves when the stream says something moved — and the mounting is the
 * only thing that changed.
 */

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

  // The environment's Secrets panel (#116) reads this as soon as the tab opens,
  // so the stub answers it rather than leaving a failed list between the
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

function renderOverview(environment = "production") {
  return renderRoutes(
    transport,
    `/projects/checkout/${environment}`,
    environmentRoutes(),
  );
}

describe("EnvironmentOverview", () => {
  it("shows the phase and the workload verdicts, which are different answers", async () => {
    renderOverview();

    // The label is sans and the phase itself is mono (#260), so the line is
    // two elements and the assertion is about what it reads as.
    expect(
      await screen.findByText(
        (_, el) => el?.className === "k-env__phase" && el.textContent === "phase Healthy",
      ),
    ).toBeTruthy();
    expect(
      screen.getAllByText("live", { selector: ".k-pill" }).length,
    ).toBeGreaterThan(0);
    expect(screen.getByText("8f2c1ad")).toBeTruthy();
    // A Healthy phase with a crash-looping workload underneath is exactly the
    // pair the two signals exist to tell apart.
    expect(screen.getAllByText("crash-loop-back-off").length).toBeGreaterThan(0);
    expect(
      screen.getByText("web is restarting repeatedly (7 restarts)"),
    ).toBeTruthy();
    expect(screen.getByText("fix:")).toBeTruthy();
  });

  it("lists the environment's kelson-managed Secrets beside it (#116)", async () => {
    renderOverview();

    // Per environment, because a Secret's lifecycle is the environment's: the
    // panel addresses the namespace this route names, not the project's.
    expect(await screen.findByText("Secrets (1)")).toBeTruthy();
    expect(screen.getByText("checkout-db")).toBeTruthy();
    expect(screen.getByText("url")).toBeTruthy();
    expect(
      screen.getByRole("button", { name: "Write the Secret" }),
    ).toBeTruthy();
  });

  it("keeps rollback offered when Status fails, like every other action", async () => {
    // Status.Unimplemented now means only that this server has no workload
    // observation client (internal/api's Status, R2 #225) — it says nothing
    // about whether the Environment store Rollback needs is configured. The
    // actions live on the layout's bar, which reads no status at all, so this
    // is now true by construction; the assertion stays because the behaviour is
    // what was promised, not the mechanism.
    renderOverview("staging");

    expect(
      await screen.findByText(/Could not read this environment's status/),
    ).toBeTruthy();
    expect(
      screen.getByRole("link", { name: "Rollback" }).getAttribute("href"),
    ).toBe("/projects/checkout/staging/actions/rollback");
    // Deploy stays available too: a render dry-run needs no cluster at all.
    expect(screen.getByRole("link", { name: "Deploy" })).toBeTruthy();
  });

  it("says stuck on a workload that gave up waiting, and only on that one", async () => {
    // The fourth state `WorkloadVerdict` can finally report (#260). `code`
    // stays a WAIT code on a stuck verdict — stuck is a timeout verdict and
    // deliberately not a failure — so the code chip alone cannot tell a
    // workload that gave up from one that is still starting, and the word
    // beside it is what does.
    const stalled = createRouterTransport((router) => {
      router.service(SpecService, {
        getSpec: () => ({
          spec: { project: "checkout", version: "7", environments: ["production"] },
        }),
      });
      router.service(DeployService, {
        status: () => ({
          phase: "Applied",
          answer: "progressing",
          revision: "8f2c1ad",
          namespace: "checkout-production",
          verdicts: [
            {
              resource: "Deployment/checkout-production/web",
              code: "workload/progressing",
              healthy: false,
              degraded: false,
              stuck: true,
              message: "no progress for 10m0s",
            },
            {
              resource: "Deployment/checkout-production/worker",
              code: "workload/progressing",
              healthy: false,
              degraded: false,
              stuck: false,
              message: "1 of 3 replicas updated",
            },
            {
              resource: "Deployment/checkout-production/api",
              code: "crash-loop-back-off",
              healthy: false,
              degraded: true,
              stuck: false,
              message: "api is restarting repeatedly",
            },
          ],
        }),
      });
    });
    renderRoutes(stalled, "/projects/checkout/production", environmentRoutes());

    // Two rows carry the same code and only one of them is stuck.
    expect(
      (await screen.findAllByText("workload/progressing")).length,
    ).toBe(2);
    const stuck = screen.getAllByText("stuck", { selector: ".k-pill" });
    expect(stuck).toHaveLength(1);
    expect(
      stuck[0]?.closest(".k-verdict")?.textContent?.includes("no progress"),
    ).toBe(true);
    // And it is not the degraded row either: that one keeps its own code and
    // grows no word.
    const degraded = screen.getByText("crash-loop-back-off");
    expect(
      degraded.closest(".k-verdict")?.querySelectorAll(".k-pill"),
    ).toHaveLength(1);
  });

  it("reads Status once for the environment in the URL", async () => {
    const calls: string[] = [];
    const counted = createRouterTransport((router) => {
      router.service(SpecService, {
        getSpec: () => ({
          spec: {
            project: "checkout",
            version: "7",
            environments: ["production", "staging"],
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
    renderRoutes(counted, "/projects/checkout/staging", environmentRoutes());

    // One call, for one environment: the project page used to read every
    // environment because every column was on screen. Here there is one.
    expect(await screen.findByText("8f2c1ad")).toBeTruthy();
    await waitFor(() => expect(calls).toEqual(["staging"]));
  });
});

describe("EnvironmentOverview data services", () => {
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
    renderRoutes(withData, "/projects/checkout/production", environmentRoutes());

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
});

describe("EnvironmentOverview live updates", () => {
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
    renderRoutes(live, "/projects/checkout/production", environmentRoutes());

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
   * The rail on this tab is fed by Status plus the stream's deltas, so it has to
   * move when the stream says something moved — and it has to keep the two
   * failures it can be in apart while doing it (#68).
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
    const { container } = renderRoutes(
      live,
      "/projects/checkout/production",
      environmentRoutes(),
    );

    // Compact, and honest about what it cannot know: StatusResponse names no
    // adapter, so the reconciler stage stays unnamed.
    await waitFor(() => {
      expect(container.querySelector(".k-rail--compact")).toBeTruthy();
    });
    const stageState = (phase: string) =>
      container
        .querySelector(`[data-phase="${phase}"]`)
        ?.getAttribute("data-state");
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
