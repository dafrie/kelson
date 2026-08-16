import { describe, expect, it } from "vitest";
import { screen } from "@testing-library/react";
import { createRouterTransport } from "@connectrpc/connect";

import { DeployService } from "../gen/kelson/v1alpha1/deploy_pb";
import { PreviewService } from "../gen/kelson/v1alpha1/preview_pb";
import { SecretService } from "../gen/kelson/v1alpha1/secret_pb";
import { SpecService } from "../gen/kelson/v1alpha1/spec_pb";
import { ProjectDetailPage } from "./ProjectDetailPage";
import { renderAt, renderRoutes } from "../test/render";
import { environmentRoutes } from "../test/routes";

/**
 * Drift, on the screens that draw it (#260).
 *
 * The engine has computed `State.Stale` for a while and no screen drew it. What
 * these pin is not that a chip exists but the two claims it must never blur:
 *
 *  1. **Stale is not unhealthy.** A drifted environment keeps its own word, and
 *     `live` beside "older than the spec" is the sentence the pair makes. If a
 *     future change paints drift as degraded or folds it into the word, the
 *     first assertion in each test here fails.
 *  2. **A rollback pin is deliberate.** The environment is *meant* to be on an
 *     older revision and `cause` names the pin, so the line says so rather than
 *     implying somebody forgot to deploy.
 *
 * The status word itself comes from `answer` on every one of these surfaces,
 * which is why the stubs answer `live` under a phase that is not `Healthy`
 * wherever the two can be told apart.
 */

const PROJECT_YAML = `kind: Project
metadata:
  name: checkout

spec:
  components:
    - name: web
      port: 8080
`;

const ENV_YAML = "kind: Environment\nmetadata:\n  name: production\n";

/** The pinned message the controller writes, verbatim (internal/controller). */
const PINNED_CAUSE =
  "kelson: RollbackPinned: pinned to revision 44-1a2b3c4d: re-rendering is suspended while " +
  "kelson.dev/rollback-to is set, so the current spec cannot be republished over it. Two things " +
  "resume tracking: remove the annotation, or edit the spec.";

interface Drifted {
  stale: boolean;
  cause?: string;
  answer?: string;
  phase?: string;
}

function transportFor({ stale, cause = "", answer = "live", phase = "Healthy" }: Drifted) {
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
    router.service(SecretService, { listSecrets: () => ({ secrets: [] }) });
    router.service(PreviewService, {
      listPreviews: () => ({ project: "checkout", environment: "production" }),
    });
    router.service(DeployService, {
      status: () => ({
        phase,
        answer,
        revision: "44-1a2b3c4d",
        observedRevision: "44-1a2b3c4d",
        stale,
        cause,
        namespace: "checkout-production",
        verdicts: [
          {
            resource: "Deployment/checkout-production/web",
            code: "healthy",
            healthy: true,
            degraded: false,
            stuck: false,
            message: "1/1 replicas ready",
          },
        ],
      }),
      history: () => ({
        entries: [
          { revision: "45-9e8d7c6b", outcome: "Healthy", specHash: "abc" },
          { revision: "44-1a2b3c4d", outcome: "Healthy", specHash: "def" },
        ],
      }),
    });
  });
}

/** Every drift mark on screen, as the sentences they read. */
function driftNotes(): string[] {
  return screen
    .queryAllByText((_, el) => el?.className === "k-drift")
    .map((el) => el.textContent ?? "");
}

describe("drift on the environment's Overview", () => {
  it("renders beside the word and never instead of it", async () => {
    renderRoutes(
      transportFor({ stale: true }),
      "/projects/checkout/production",
      environmentRoutes(),
    );

    // Both facts, and this is the whole point: revision 44 is up and well, and
    // it is simply not the revision the stored spec would publish.
    expect(
      await screen.findByText("live", { selector: ".k-pill" }),
    ).toBeTruthy();
    expect(driftNotes()).toEqual(["older than the spec"]);
    // The revision it is about is still on the page as its own copyable value.
    expect(screen.getAllByText("44-1a2b3c4d").length).toBeGreaterThan(0);
  });

  it("says a rollback pin was chosen rather than neglected", async () => {
    renderRoutes(
      transportFor({ stale: true, cause: PINNED_CAUSE }),
      "/projects/checkout/production",
      environmentRoutes(),
    );

    expect(
      await screen.findByText("live", { selector: ".k-pill" }),
    ).toBeTruthy();
    expect(driftNotes()).toEqual(["pinned to an older revision"]);
  });

  it("is silent when the environment is current", async () => {
    renderRoutes(
      transportFor({ stale: false }),
      "/projects/checkout/production",
      environmentRoutes(),
    );

    expect(
      await screen.findByText("live", { selector: ".k-pill" }),
    ).toBeTruthy();
    // Currency is the norm and recedes; drift is the exception and advances.
    expect(driftNotes()).toEqual([]);
  });

  it("takes the word from the answer, not from the phase", async () => {
    // `stuck` is not a phase: this environment is wedged in Committed, and a
    // screen deriving from the phase alone would call it `waiting` and report
    // that nothing is wrong.
    renderRoutes(
      transportFor({ stale: false, answer: "stuck", phase: "Committed" }),
      "/projects/checkout/production",
      environmentRoutes(),
    );

    expect(
      await screen.findByText("stuck", { selector: ".k-pill" }),
    ).toBeTruthy();
    expect(screen.queryByText("waiting", { selector: ".k-pill" })).toBeNull();
    // The phase survives as the labelled fact an operator correlates with Flux.
    expect(
      screen.getByText(
        (_, el) =>
          el?.className === "k-env__phase" && el.textContent === "phase Committed",
      ),
    ).toBeTruthy();
  });

  it("falls back to the phase when the delivery half sent no answer", async () => {
    renderRoutes(
      transportFor({ stale: false, answer: "", phase: "Reconciling" }),
      "/projects/checkout/production",
      environmentRoutes(),
    );

    // Empty never means "fine"; it means nothing was reported, and the phase is
    // the older claim this UI has always been able to make.
    expect(
      await screen.findByText("deploying", { selector: ".k-pill" }),
    ).toBeTruthy();
  });
});

describe("drift on the History tab", () => {
  it("marks the live row, which is not the newest one", async () => {
    renderRoutes(
      transportFor({ stale: true, cause: PINNED_CAUSE }),
      "/projects/checkout/production/history",
      environmentRoutes(),
    );

    expect(await screen.findByText("deployed now")).toBeTruthy();
    // The answer to "why is the marker not on the top row", on the row itself.
    expect(driftNotes()).toEqual(["pinned to an older revision"]);
    expect(screen.getByText("live", { selector: ".k-pill" })).toBeTruthy();
  });
});

describe("drift on the project matrix", () => {
  it("puts the sentence on the column's revision line", async () => {
    renderAt(
      transportFor({ stale: true }),
      "/projects/checkout",
      "/projects/:project",
      <ProjectDetailPage />,
    );

    // The column head's word and the cell's, both `live` and both untouched by
    // the drift the column reports underneath them.
    expect(
      (await screen.findAllByText("live", { selector: ".k-pill" })).length,
    ).toBe(2);
    expect(driftNotes()).toEqual(["older than the spec"]);
  });
});
