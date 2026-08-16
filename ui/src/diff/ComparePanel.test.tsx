import { describe, expect, it } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import { Code, ConnectError, createRouterTransport } from "@connectrpc/connect";

import { DeployService } from "../gen/kelson/v1alpha1/deploy_pb";
import { RenderService } from "../gen/kelson/v1alpha1/render_pb";
import { DryRun } from "../gen/kelson/v1alpha1/common_pb";
import { renderAt } from "../test/render";
import { ComparePanel } from "./ComparePanel";

/** One modified Deployment, the shape diff.EncodeJSON produces. */
function diffJson(image: string): Uint8Array {
  return new TextEncoder().encode(
    JSON.stringify({
      level: "rendered",
      project: "checkout",
      environment: "production",
      resources: [
        {
          apiVersion: "apps/v1",
          kind: "Deployment",
          name: "web",
          namespace: "checkout-production",
          op: "modified",
          risk: "restart-required",
          fields: [
            {
              path: "spec.template.spec.containers[0].image",
              before: image,
              after: "ghcr.io/acme/checkout:v3",
              origin: "spec",
              risk: "restart-required",
            },
          ],
        },
      ],
      summary: { added: 0, modified: 1, removed: 0, maxRisk: "restart-required" },
    }),
  );
}

const transport = createRouterTransport((router) => {
  router.service(DeployService, {
    history: () => ({
      entries: [
        { revision: "rev-00000002", specHash: "sha256:b", committedAt: "2026-08-13T10:00:00Z" },
        { revision: "rev-00000001", specHash: "sha256:a", committedAt: "2026-08-12T10:00:00Z" },
      ],
    }),
  });

  router.service(RenderService, {
    diff: (req) => {
      if (req.dryRun === DryRun.SERVER) {
        // The live cluster is the other mode's business, and this transport is
        // not one: an unreachable cluster is the honest answer here.
        throw new ConnectError("no usable cluster credentials", Code.Unavailable);
      }
      if (req.fromRevision === "") {
        throw new ConnectError("expected a revision", Code.InvalidArgument);
      }
      return {
        diffJson: diffJson(
          req.fromRevision === "rev-00000001"
            ? "ghcr.io/acme/checkout:v1"
            : "ghcr.io/acme/checkout:v2",
        ),
        exitSemantics: 2,
      };
    },
  });
});

/**
 * The panel where the history tab opens it (#260). It was a screen of its own
 * until the flow consolidation; the subject of these tests is unchanged, only
 * the mounting is — the panel takes the pair and the revision as props now,
 * where it used to read them off the URL.
 */
function renderCompare() {
  return renderAt(
    transport,
    "/projects/checkout/production/history?compare=1",
    "/projects/:project/:env/history",
    <ComparePanel project="checkout" env="production" />,
  );
}

/** The panel as a history row opens it: a revision already chosen. */
function renderCompareFrom(revision: string) {
  return renderAt(
    transport,
    `/projects/checkout/production/history?from=${revision}`,
    "/projects/:project/:env/history",
    <ComparePanel project="checkout" env="production" from={revision} />,
  );
}

describe("ComparePanel", () => {
  it("opens against the live cluster and reports its failure as one", async () => {
    renderCompare();
    expect(
      screen.getByRole("button", { name: "Against live cluster" }).getAttribute("aria-current"),
    ).toBe("true");
    expect(await screen.findByText(/no usable cluster credentials/)).toBeTruthy();
  });

  it("diffs against a deployed revision once one is picked (#162)", async () => {
    renderCompare();
    fireEvent.click(screen.getByRole("button", { name: "Against deployed revision" }));

    // The revision list is the History RPC's, newest first, and nothing is
    // requested until one is chosen.
    expect(await screen.findByText("rev-00000002")).toBeTruthy();
    expect(screen.getByText("rev-00000001")).toBeTruthy();
    expect(screen.queryByText("Deployment")).toBeNull();

    fireEvent.click(screen.getAllByRole("radio")[1]!);

    await waitFor(() =>
      expect(screen.getByText(/checkout:v1/)).toBeTruthy(),
    );
    expect(screen.getByText(/checkout:v3/)).toBeTruthy();
  });

  it("re-reads the diff when another revision is picked", async () => {
    renderCompare();
    fireEvent.click(screen.getByRole("button", { name: "Against deployed revision" }));
    await screen.findByText("rev-00000002");

    fireEvent.click(screen.getAllByRole("radio")[1]!);
    await waitFor(() =>
      expect(screen.getByText(/checkout:v1/)).toBeTruthy(),
    );

    fireEvent.click(screen.getAllByRole("radio")[0]!);
    await waitFor(() =>
      expect(screen.getByText(/checkout:v2/)).toBeTruthy(),
    );
  });

  it("opens on the revision it was given, which is how a history row opens it (#67)", async () => {
    renderCompareFrom("rev-00000001");

    // A revision only means something in the rendered mode, so it selects the
    // mode as well as the revision — and the diff is requested without a click,
    // because the row that opened the panel already made the choice.
    expect(
      screen
        .getByRole("button", { name: "Against deployed revision" })
        .getAttribute("aria-current"),
    ).toBe("true");
    await waitFor(() => expect(screen.getByText(/checkout:v1/)).toBeTruthy());
    expect(screen.getByText(/checkout:v3/)).toBeTruthy();

    const picked = await screen.findByDisplayValue("rev-00000001");
    expect((picked as HTMLInputElement).checked).toBe(true);
  });
});
