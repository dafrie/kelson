import { describe, expect, it } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import { Code, ConnectError, createRouterTransport } from "@connectrpc/connect";

import { ErrorSchema } from "../gen/kelson/v1alpha1/common_pb";
import { DeployService } from "../gen/kelson/v1alpha1/deploy_pb";
import { SpecService } from "../gen/kelson/v1alpha1/spec_pb";
import { renderAt } from "../test/render";
import { ComponentPage } from "./ComponentPage";

/**
 * The addressable component (#260): (project, environment, component) as a
 * page, built from the two calls that already exist.
 *
 * The fixture is the shape the model actually produces — a project image, a
 * per-environment pin over it, two declared sources and a database — because
 * what this page is for is stating which of those decided what.
 */
const PROJECT_YAML = `kind: Project
metadata:
  name: checkout

spec:
  image: ghcr.io/acme/checkout:1.4.2

  sources:
    - name: app
      git: https://github.com/acme/checkout
      ref: main
    - name: tools
      git: https://github.com/acme/build-tools
      ref: v2

  components:
    - name: web
      port: 8080
      source: app

    - name: nightly
      schedule: "0 3 * * *"
      source: tools

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

const ROUTE = "/projects/:project/:env/components/:component";

function server(status: () => unknown) {
  return createRouterTransport((router) => {
    router.service(SpecService, {
      getSpec: () => ({
        spec: {
          project: "checkout",
          version: "7",
          environments: ["production"],
          documents: {
            project: new TextEncoder().encode(PROJECT_YAML),
            environments: {
              production: new TextEncoder().encode(PRODUCTION_YAML),
            },
          },
        },
      }),
    });
    router.service(DeployService, {
      status: () => status() as never,
    });
  });
}

const healthy = server(() => ({
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
}));

function open(component: string, transport = healthy) {
  return renderAt(
    transport,
    `/projects/checkout/production/components/${component}`,
    ROUTE,
    <ComponentPage />,
  );
}

describe("ComponentPage", () => {
  it("states what the component is, what it runs, and where that was set", async () => {
    open("web");

    expect(await screen.findByText("live", { selector: ".k-pill" })).toBeTruthy();
    expect(screen.getByText("serves on port 8080")).toBeTruthy();
    // Rule P3: the environment's pin beat the project's image, and the page
    // says which document decided it rather than showing a bare value.
    expect(screen.getByText("ghcr.io/acme/checkout:1.4.3")).toBeTruthy();
    expect(screen.getByText("set on the environment")).toBeTruthy();
    // The source binding, from the same documents (ADR-0035).
    expect(screen.getByText("app")).toBeTruthy();
    expect(screen.getByText("https://github.com/acme/checkout")).toBeTruthy();
    expect(screen.getByText("main")).toBeTruthy();
    // The revision belongs to the environment and is labelled as such: one
    // publish carries every component.
    expect(screen.getByText("8f2c1ad")).toBeTruthy();
    expect(
      screen.getByText("the environment's, not this component's"),
    ).toBeTruthy();
    expect(screen.getByText("checkout-production")).toBeTruthy();
  });

  it("links to the tabs and actions that act on it, itself preselected in the logs", async () => {
    open("web");

    await screen.findByText("serves on port 8080");
    const href = (name: string) =>
      screen.getByRole("link", { name }).getAttribute("href");
    // Two views of this component's environment...
    expect(href("Logs")).toBe(
      "/projects/checkout/production/logs?component=web",
    );
    expect(href("History")).toBe("/projects/checkout/production/history");
    // ...and two things that can be done to it (#260).
    expect(href("Deploy")).toBe(
      "/projects/checkout/production/actions/deploy",
    );
    expect(href("Rollback")).toBe(
      "/projects/checkout/production/actions/rollback",
    );
    // Diff is not among them: it is not a destination any more, it is what the
    // deploy shows before it writes and what a history row opens.
    expect(screen.queryByRole("link", { name: "Diff" })).toBeNull();
    // Back to the matrix it came from, and into the environment it runs in.
    expect(href("← checkout")).toBe("/projects/checkout");
    expect(href("production")).toBe("/projects/checkout/production");
  });

  it("shows the component's own verdict when observation has one", async () => {
    const crashing = server(() => ({
      phase: "Healthy",
      revision: "8f2c1ad",
      namespace: "checkout-production",
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
    }));
    open("web", crashing);

    // The component's word overrules a healthy environment's, and the code,
    // message and fix are observation's own.
    expect(
      await screen.findByText("unhealthy", { selector: ".k-pill" }),
    ).toBeTruthy();
    expect(screen.getByText("crash-loop-back-off")).toBeTruthy();
    expect(
      screen.getByText("web is restarting repeatedly (7 restarts)"),
    ).toBeTruthy();
    expect(screen.getByText("kelson logs web --at-termination")).toBeTruthy();
  });

  it("says there is no reading rather than borrowing green for a cron", async () => {
    open("nightly");

    expect(await screen.findByText("runs at 0 3 * * *")).toBeTruthy();
    // Observation probes Deployments; a CronJob has no verdict, and the page
    // must not present the environment's health as this component's.
    expect(
      screen.getByText(
        "no reading for nightly here — the word above is production's own",
      ),
    ).toBeTruthy();
    // Its own source binding is still a fact the documents answer.
    expect(screen.getByText("tools")).toBeTruthy();
    expect(screen.getByText("v2")).toBeTruthy();
  });

  it("sends a database to the section that can size it, and offers no log tail", async () => {
    open("db");

    expect(await screen.findByText("small")).toBeTruthy();
    expect(screen.queryByRole("link", { name: "Logs" })).toBeNull();
    expect(
      screen.getByText(
        "no reading for this database — its section on the project page has what there is",
      ),
    ).toBeTruthy();
    // A database has no image and no source: neither is asked for.
    expect(screen.queryByText("source")).toBeNull();
  });

  it("names a component the project does not declare", async () => {
    open("ghost");

    expect(
      await screen.findByText("checkout declares no component called ghost"),
    ).toBeTruthy();
    // Nothing is claimed about a component that does not exist — no status
    // pill, no facts.
    expect(screen.queryByText("live", { selector: ".k-pill" })).toBeNull();
  });

  it("keeps the documents' facts when the cluster cannot be read", async () => {
    const unreachable = server(() => {
      throw new ConnectError(
        "api: building the delivery plane: connection refused",
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
    });
    open("web", unreachable);

    // What the spec says needs no cluster, so it is still on screen…
    expect(await screen.findByText("serves on port 8080")).toBeTruthy();
    expect(screen.getByText("ghcr.io/acme/checkout:1.4.3")).toBeTruthy();
    // …and the half that did need one says so, with the server's own code.
    await waitFor(() => {
      expect(screen.getByText("delivery/unavailable")).toBeTruthy();
    });
    expect(screen.getByText("the cluster could not be reached")).toBeTruthy();
    expect(screen.getByText("unknown", { selector: ".k-pill" })).toBeTruthy();
    expect(screen.getByText("not read")).toBeTruthy();
  });

  it("reports a spec that could not be read as the spec's failure", async () => {
    const noSpec = createRouterTransport((router) => {
      router.service(SpecService, {
        getSpec: () => {
          throw new ConnectError("no such project", Code.NotFound);
        },
      });
      router.service(DeployService, {
        status: () => ({ phase: "Healthy", revision: "8f2c1ad" }),
      });
    });
    open("web", noSpec);

    expect(
      await screen.findByText("Cannot read the spec for checkout"),
    ).toBeTruthy();
  });
});
