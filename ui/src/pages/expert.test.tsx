import { beforeEach, describe, expect, it } from "vitest";
import { cleanup, screen, waitFor } from "@testing-library/react";
import { Code, ConnectError, createRouterTransport } from "@connectrpc/connect";

import { ErrorSchema } from "../gen/kelson/v1alpha1/common_pb";
import { DeployService } from "../gen/kelson/v1alpha1/deploy_pb";
import { PreviewService } from "../gen/kelson/v1alpha1/preview_pb";
import { SecretService } from "../gen/kelson/v1alpha1/secret_pb";
import { SpecService } from "../gen/kelson/v1alpha1/spec_pb";
import { setDetail } from "../expert/preference";
import { renderAt, renderRoutes } from "../test/render";
import { environmentRoutes } from "../test/routes";
import { ComponentPage } from "./ComponentPage";
import { ProjectDetailPage } from "./ProjectDetailPage";

/**
 * Expert mode across the surfaces it reaches (#260).
 *
 * The three claims this file exists to pin are the ones the preference would be
 * dangerous without, and none of them is about a particular fact being on a
 * particular row:
 *
 *  1. **It gates information, never actions.** The action bar is byte-identical
 *     with the preference on and off, so a screenshot from one reader is
 *     actionable by another and nobody is missing a button because of a
 *     display setting.
 *  2. **It never hides a could-not-check state or a structured code.** The
 *     honest-absence rule that produced "not read", "status unavailable" and
 *     the server's own error codes has no loudness setting; expert mode may add
 *     to those and must not subtract.
 *  3. **Normal mode gained no density.** Every fact and caret added here is
 *     absent when the preference is off, which is the half of the owner's ask
 *     that is easiest to lose.
 */

const PROJECT_YAML = `kind: Project
metadata:
  name: checkout
spec:
  image: ghcr.io/acme/checkout:1.4.2
  components:
    - name: web
      port: 8080
    - name: worker
`;

const ENV_YAML = "kind: Environment\nmetadata:\n  name: production\n";

/** A revision on the rebuilt spine: `<generation>-<hash8>`. */
const REVISION = "45-9e8d7c6b";

const STATUS = {
  phase: "Healthy",
  answer: "live",
  revision: REVISION,
  observedRevision: REVISION,
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
    {
      resource: "Deployment/checkout-production/worker",
      code: "workload/progressing",
      healthy: false,
      degraded: false,
      stuck: true,
      message: "no progress for 5m",
      remediation: "kelson logs worker",
    },
  ],
};

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
            environments: { production: new TextEncoder().encode(ENV_YAML) },
          },
        },
      }),
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
    router.service(DeployService, {
      status: () => status() as never,
      history: () => ({
        entries: [
          {
            revision: REVISION,
            specHash: "sha256:0123456789abcdef",
            committedAt: "2026-08-01T10:00:00Z",
            outcome: "Healthy",
            author: "",
            digest: "sha256:fedcba9876543210",
            images: [],
            beyondWindow: false,
          },
          {
            revision: "44-1a2b3c4d",
            specHash: "sha256:aaaaaaaaaaaaaaaa",
            committedAt: "2026-07-31T09:00:00Z",
            outcome: "Healthy",
            author: "",
            digest: "",
            images: [],
            beyondWindow: false,
          },
        ],
      }),
    });
  });
}

const healthy = server(() => STATUS);

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

const COMPONENT_ROUTE = "/projects/:project/:env/components/:component";

function openComponent(transport = healthy) {
  return renderAt(
    transport,
    "/projects/checkout/production/components/web",
    COMPONENT_ROUTE,
    <ComponentPage />,
  );
}

function openOverview(transport = healthy) {
  return renderRoutes(
    transport,
    "/projects/checkout/production",
    environmentRoutes(),
  );
}

function openHistory(transport = healthy) {
  return renderRoutes(
    transport,
    "/projects/checkout/production/history",
    environmentRoutes(),
  );
}

function openProject(transport = healthy) {
  return renderAt(
    transport,
    "/projects/checkout",
    "/projects/:project",
    <ProjectDetailPage />,
  );
}

beforeEach(() => setDetail(false));

describe("the toggle gates information and not actions", () => {
  it("renders the component page's action bar identically in both modes", async () => {
    openComponent();
    await screen.findByText("serves on port 8080");
    const normalActions = document.querySelector(".k-actions")?.innerHTML;
    const normalLinks = screen
      .getAllByRole("link")
      .map((a) => `${a.textContent} → ${a.getAttribute("href")}`);
    expect(normalActions).toBeTruthy();
    cleanup();

    setDetail(true);
    openComponent();
    await screen.findByText("serves on port 8080");
    const expertActions = document.querySelector(".k-actions")?.innerHTML;
    const expertLinks = screen
      .getAllByRole("link")
      .map((a) => `${a.textContent} → ${a.getAttribute("href")}`);

    // Not "the same buttons" — the same markup. A preference that moved,
    // relabelled or reordered a control would make two products out of one.
    expect(expertActions).toBe(normalActions);
    expect(expertLinks).toEqual(normalLinks);
    // And the actions are the ones this page has always offered.
    expect(normalLinks).toContain(
      "Deploy → /projects/checkout/production/actions/deploy",
    );
  });

  it("keeps every action on the environment's overview in both modes", async () => {
    openOverview();
    await screen.findByText("live", { selector: ".k-pill" });
    const normal = screen
      .getAllByRole("link")
      .map((a) => a.getAttribute("href"))
      .sort();
    cleanup();

    setDetail(true);
    openOverview();
    await screen.findByText("live", { selector: ".k-pill" });
    const expert = screen
      .getAllByRole("link")
      .map((a) => a.getAttribute("href"))
      .sort();

    expect(expert).toEqual(normal);
  });
});

describe("could-not-check states survive both modes", () => {
  it("shows the server's structured code and the unread facts with detail off", async () => {
    openComponent(unreachable);

    await waitFor(() => {
      expect(screen.getByText("delivery/unavailable")).toBeTruthy();
    });
    expect(screen.getByText("the cluster could not be reached")).toBeTruthy();
    expect(screen.getByText("unknown", { selector: ".k-pill" })).toBeTruthy();
    expect(screen.getByText("not read")).toBeTruthy();
  });

  it("shows exactly the same with detail on, plus nothing removed", async () => {
    setDetail(true);
    openComponent(unreachable);

    await waitFor(() => {
      expect(screen.getByText("delivery/unavailable")).toBeTruthy();
    });
    expect(screen.getByText("the cluster could not be reached")).toBeTruthy();
    expect(screen.getByText("unknown", { selector: ".k-pill" })).toBeTruthy();
    expect(screen.getByText("not read")).toBeTruthy();
    // The caret on the word says why it is unknown rather than replacing the
    // sentence that already did.
    expect(screen.getByLabelText("Evidence for “unknown”")).toBeTruthy();
    expect(
      screen.getByText(
        "The status call did not answer, so nothing is claimed about this component.",
      ),
    ).toBeTruthy();
  });

  it("keeps the environment's error panel in both modes", async () => {
    setDetail(true);
    openOverview(unreachable);

    expect(
      await screen.findByText("Could not read this environment's status"),
    ).toBeTruthy();
    expect(
      screen.getByText("status unavailable", { selector: ".k-pill" }),
    ).toBeTruthy();
  });
});

describe("normal mode gains no density", () => {
  it("carries no carets and no Kubernetes facts on the overview", async () => {
    openOverview();
    await screen.findByText("live", { selector: ".k-pill" });

    expect(document.querySelector(".k-why")).toBeNull();
    expect(document.querySelector(".k-kfact")).toBeNull();
    expect(screen.queryByText(/^generation$/)).toBeNull();
    // The revision itself is on screen, as it always was.
    expect(screen.getByText(REVISION)).toBeTruthy();
  });

  it("carries no carets and no Kubernetes facts on the history tab", async () => {
    openHistory();
    await screen.findAllByText(REVISION);

    expect(document.querySelector(".k-why")).toBeNull();
    expect(document.querySelector(".k-kfact")).toBeNull();
  });
});

describe("expert mode surfaces what the wire already carried", () => {
  it("names the revision tag's two halves on the overview", async () => {
    setDetail(true);
    openOverview();
    await screen.findByText("live", { selector: ".k-pill" });

    // `<generation>-<hash8>`: the generation is the environment object's own
    // and was previously on screen only as an unexplained prefix.
    expect(screen.getAllByText("45").length).toBeGreaterThan(0);
    expect(screen.getAllByText("9e8d7c6b").length).toBeGreaterThan(0);
  });

  it("names a verdict's kind, namespace and object name", async () => {
    setDetail(true);
    openOverview();
    await screen.findByText("live", { selector: ".k-pill" });

    // The whole resource string was always here; the three parts were not.
    expect(screen.getAllByText("Deployment").length).toBeGreaterThan(0);
    expect(screen.getAllByText("checkout-production").length).toBeGreaterThan(0);
    expect(screen.getAllByText("worker").length).toBeGreaterThan(0);
  });

  it("attaches the evidence for the status word to that word", async () => {
    setDetail(true);
    openOverview();
    await screen.findByText("live", { selector: ".k-pill" });

    expect(screen.getByLabelText("Evidence for “live”")).toBeTruthy();
    // The two fields `statusForDelivery` actually reads, verbatim.
    expect(screen.getByText("answer")).toBeTruthy();
    // "phase" is also the label of the wire's phase beside the word, so the
    // evidence key is one of two rather than the only one.
    expect(screen.getAllByText("phase").length).toBeGreaterThan(1);
    expect(
      screen.getByText(
        "The word is the engine's own answer, translated. The phase is what the controller last wrote and is kept beside it rather than re-read.",
      ),
    ).toBeTruthy();
  });

  it("attaches the evidence for a stuck workload to the word stuck", async () => {
    setDetail(true);
    openOverview();
    await screen.findByText("stuck", { selector: ".k-pill" });

    expect(screen.getByLabelText("Evidence for “stuck”")).toBeTruthy();
    // The three booleans are what says the probe gave up rather than that the
    // rollout is slow — the code beside it is still a wait code.
    expect(screen.getByText("degraded")).toBeTruthy();
    expect(screen.getAllByText("workload/progressing").length).toBeGreaterThan(
      0,
    );
  });

  it("carries the generation and the deployed-now evidence on the history tab", async () => {
    setDetail(true);
    openHistory();
    await screen.findAllByText(REVISION);

    // Both rows' generations, and the caret on the one live claim.
    expect(screen.getAllByText("45").length).toBeGreaterThan(0);
    expect(screen.getAllByText("44").length).toBeGreaterThan(0);
    expect(screen.getByLabelText("Evidence for “deployed now”")).toBeTruthy();
    expect(
      screen.getByText(
        "The status call reports this revision as the one serving. Every other row carries a recorded outcome instead, frozen when it stopped being current.",
      ),
    ).toBeTruthy();
  });

  it("carries the generation and namespace on a matrix column head", async () => {
    setDetail(true);
    openProject();
    await screen.findByText("production", { selector: ".k-matrix__env span" });

    await waitFor(() => {
      expect(screen.getAllByText("45").length).toBeGreaterThan(0);
    });
    expect(screen.getAllByText("checkout-production").length).toBeGreaterThan(0);
    // No caret anywhere in the grid: a cell is a link and a header sits in a
    // scroll container. The evidence lives on the environment's own page.
    expect(document.querySelector(".k-why")).toBeNull();
  });

  it("adds no column-head detail to a matrix drawn in normal mode", async () => {
    openProject();
    await screen.findByText("production", { selector: ".k-matrix__env span" });

    expect(document.querySelector(".k-why")).toBeNull();
    expect(document.querySelector(".k-kfact")).toBeNull();
  });
});
