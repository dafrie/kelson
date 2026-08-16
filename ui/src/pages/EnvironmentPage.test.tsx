import { describe, expect, it } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import { createRouterTransport } from "@connectrpc/connect";
import type { RouteObject } from "react-router-dom";

import { DeployService } from "../gen/kelson/v1alpha1/deploy_pb";
import { PreviewService } from "../gen/kelson/v1alpha1/preview_pb";
import { SecretService } from "../gen/kelson/v1alpha1/secret_pb";
import { SpecService } from "../gen/kelson/v1alpha1/spec_pb";
import { renderRoutes } from "../test/render";
import { environmentRoutes } from "../test/routes";
import { routes } from "../App";

const PROJECT_YAML = `kind: Project
metadata:
  name: checkout
spec:
  components:
    - name: web
      port: 8080
`;

const transport = createRouterTransport((router) => {
  router.service(SpecService, {
    getSpec: () => ({
      spec: {
        project: "checkout",
        version: "7",
        environments: ["production", "staging"],
        documents: {
          project: new TextEncoder().encode(PROJECT_YAML),
          environments: {},
        },
      },
    }),
  });
  router.service(DeployService, {
    status: () => ({
      phase: "Healthy",
      revision: "8f2c1ad",
      namespace: "checkout-production",
      verdicts: [],
    }),
    history: () => ({ entries: [] }),
  });
  router.service(SecretService, {
    listSecrets: () => ({ namespace: "checkout-production", secrets: [] }),
  });
  router.service(PreviewService, {
    listPreviews: () => ({
      project: "checkout",
      environment: "production",
      namespace: "checkout-production",
      mode: "direct",
    }),
  });
});

function renderEnv(path: string) {
  return renderRoutes(transport, path, environmentRoutes());
}

/**
 * The shape the six flows were consolidated into (#260).
 *
 * These read the route table as data — `createRoutesFromElements` returns plain
 * objects — so the claim being tested is the app's own table and not a copy of
 * it made for the test. That matters because the redirects below are the whole
 * reason the old paths still work, and a route quietly dropped from `App.tsx`
 * would otherwise only show up as a 404 in somebody's pull-request comment.
 */
describe("the environment's route table", () => {
  const paths = flatten(routes);

  it("mounts logs and history as children of the environment, at their old paths", () => {
    expect(paths).toContain("projects/:project/:env");
    expect(paths).toContain("projects/:project/:env/logs");
    expect(paths).toContain("projects/:project/:env/history");
    // The index is Overview: the environment's own path renders it.
    const env = find(routes, "projects/:project/:env");
    expect(env?.children?.some((c) => c.index === true)).toBe(true);
  });

  it("mounts the three actions under actions/, and no diff route anywhere", () => {
    expect(paths).toContain("projects/:project/:env/actions/deploy");
    expect(paths).toContain("projects/:project/:env/actions/promote");
    expect(paths).toContain("projects/:project/:env/actions/rollback");
    // Diff is a panel now. The only `diff` left in the table is the redirect
    // that keeps the old link alive.
    expect(paths.filter((p) => p.endsWith("/diff"))).toEqual([
      "projects/:project/:env/diff",
    ]);
  });

  it("keeps every old flow path mounted", () => {
    for (const flow of ["deploy", "diff", "promote", "rollback"]) {
      expect(paths).toContain(`projects/:project/:env/${flow}`);
    }
  });
});

describe("EnvironmentPage", () => {
  it("draws the environment's views as tabs and its actions as actions", async () => {
    renderEnv("/projects/checkout/production");

    expect(await screen.findByRole("heading", { name: "production" })).toBeTruthy();
    const tabs = screen.getByRole("navigation", { name: "production views" });
    expect(
      [...tabs.querySelectorAll("a")].map((a) => a.textContent),
    ).toEqual(["Overview", "Logs", "History"]);
    // Overview is the index, so the environment's own path is the one showing.
    expect(
      tabs.querySelector(".k-tab--active")?.textContent,
    ).toBe("Overview");

    // The three actions are reachable from wherever the reader is standing.
    expect(
      screen.getByRole("link", { name: "Deploy" }).getAttribute("href"),
    ).toBe("/projects/checkout/production/actions/deploy");
    expect(
      screen
        .getByRole("link", { name: "Promote into this environment" })
        .getAttribute("href"),
    ).toBe("/projects/checkout/production/actions/promote");
    expect(
      screen.getByRole("link", { name: "Rollback" }).getAttribute("href"),
    ).toBe("/projects/checkout/production/actions/rollback");
  });

  it("marks the tab that is showing, and keeps the actions on every one", async () => {
    renderEnv("/projects/checkout/production/logs");

    const tabs = await screen.findByRole("navigation", { name: "production views" });
    const active = tabs.querySelector(".k-tab--active");
    expect(active?.textContent).toBe("Logs");
    expect(active?.getAttribute("aria-current")).toBe("page");
    // Deploy is still one press away from a log tail — that is the point of
    // putting the actions on the bar rather than on the Overview tab.
    expect(screen.getByRole("link", { name: "Deploy" })).toBeTruthy();
    expect(screen.getByRole("heading", { name: "production" })).toBeTruthy();
  });

  it("reads the project once for the tab below it", async () => {
    const calls: string[] = [];
    const counted = createRouterTransport((router) => {
      router.service(SpecService, {
        getSpec: (req) => {
          calls.push(req.project);
          return {
            spec: {
              project: "checkout",
              version: "7",
              environments: ["production"],
              documents: {
                project: new TextEncoder().encode(PROJECT_YAML),
                environments: {},
              },
            },
          };
        },
      });
      router.service(DeployService, {
        status: () => ({ phase: "Healthy", revision: "8f2c1ad", verdicts: [] }),
      });
    });
    renderRoutes(counted, "/projects/checkout/production/logs", environmentRoutes());

    // The component picker is filled from the layout's read, not a second one.
    expect(await screen.findByDisplayValue("web")).toBeTruthy();
    await waitFor(() => expect(calls).toEqual(["checkout"]));
  });

  it("says so when the spec declares no such environment", async () => {
    renderEnv("/projects/checkout/qa");

    expect(
      await screen.findByText("checkout declares no environment called qa"),
    ).toBeTruthy();
    // The tabs stay: they are how a reader gets to an environment that exists.
    expect(screen.getByRole("navigation", { name: "qa views" })).toBeTruthy();
  });

  it("offers no promotion when the project declares nowhere to promote from", async () => {
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
    renderRoutes(alone, "/projects/checkout/production", environmentRoutes());

    const promote = await screen.findByRole("button", {
      name: "Promote into this environment",
    });
    expect((promote as HTMLButtonElement).disabled).toBe(true);
    expect(promote.getAttribute("title")).toContain(
      "declares no other environment to promote from",
    );
  });
});

/**
 * The four paths that moved, and the promise that they still work.
 *
 * `ui/README.md` says a link into a deploy is a link that keeps working, and
 * these are the links: a commit status, a pull-request comment, a page of the
 * docs. Each assertion is about the landing *and* the parameters, because a
 * redirect that drops `?to=` has lost the revision the reader clicked.
 */
describe("the old flow routes", () => {
  const landing = async (from: string) => {
    const { router } = renderEnv(from);
    await waitFor(() =>
      expect(router.state.location.pathname).not.toBe(from.split("?")[0]),
    );
    return router.state.location;
  };

  it("lands a deploy link on the deploy action", async () => {
    const at = await landing("/projects/checkout/production/deploy");
    expect(at.pathname).toBe("/projects/checkout/production/actions/deploy");
    expect(await screen.findByRole("heading", { name: "Deploy" })).toBeTruthy();
  });

  it("lands a rollback link on the rollback action, still holding ?to=", async () => {
    const at = await landing(
      "/projects/checkout/production/rollback?to=rev-00000041",
    );
    expect(at.pathname).toBe("/projects/checkout/production/actions/rollback");
    expect(at.search).toBe("?to=rev-00000041");
  });

  it("lands a promote link on the promote action", async () => {
    const at = await landing("/projects/checkout/production/promote");
    expect(at.pathname).toBe("/projects/checkout/production/actions/promote");
  });

  it("lands a diff-of-a-revision link on the history row's comparison", async () => {
    const at = await landing(
      "/projects/checkout/production/diff?from=rev-00000041",
    );
    expect(at.pathname).toBe("/projects/checkout/production/history");
    expect(at.search).toBe("?from=rev-00000041");
    expect(
      await screen.findByRole("button", { name: "Against deployed revision" }),
    ).toBeTruthy();
  });

  it("lands a bare diff link on the same panel, against the cluster", async () => {
    const at = await landing("/projects/checkout/production/diff");
    expect(at.pathname).toBe("/projects/checkout/production/history");
    expect(at.search).toBe("?compare=1");
    expect(
      (
        await screen.findByRole("button", { name: "Against live cluster" })
      ).getAttribute("aria-current"),
    ).toBe("true");
  });

  it("replaces the old entry rather than stacking it in the history", async () => {
    const { router } = renderEnv("/projects/checkout/production/deploy");
    await waitFor(() =>
      expect(router.state.location.pathname).toBe(
        "/projects/checkout/production/actions/deploy",
      ),
    );
    // Back from the new home must not bounce forward through the redirect.
    expect(router.state.historyAction).toBe("REPLACE");
  });
});

/** Every path the table declares, joined the way the router joins them. */
function flatten(objects: RouteObject[], prefix = ""): string[] {
  const out: string[] = [];
  for (const route of objects) {
    const path =
      route.path === undefined
        ? prefix
        : prefix === ""
          ? route.path
          : `${prefix}/${route.path}`;
    if (route.path !== undefined) out.push(path);
    if (route.children) out.push(...flatten(route.children, path));
  }
  return out;
}

/**
 * The route object declaring a path, for asserting on its children. The layers
 * above the screens (the auth boundary, the session gate, the shell) declare no
 * path of their own, so a screen's `path` is the whole path.
 */
function find(objects: RouteObject[], want: string): RouteObject | undefined {
  for (const route of objects) {
    if (route.path === want) return route;
    const inner = route.children ? find(route.children, want) : undefined;
    if (inner) return inner;
  }
  return undefined;
}
