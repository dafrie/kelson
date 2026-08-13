import { describe, expect, it } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import { Code, ConnectError, createRouterTransport } from "@connectrpc/connect";

import { DeployService } from "../gen/kelson/v1alpha1/deploy_pb";
import { RenderService } from "../gen/kelson/v1alpha1/render_pb";
import { DryRun } from "../gen/kelson/v1alpha1/common_pb";
import { renderAt } from "../test/render";
import { DiffPage } from "./DiffPage";

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

function renderDiff() {
  return renderAt(
    transport,
    "/apps/checkout/production/diff",
    "/apps/:project/:env/diff",
    <DiffPage />,
  );
}

/** The screen as the history timeline links to it: a revision in the URL. */
function renderDiffFrom(revision: string) {
  return renderAt(
    transport,
    `/apps/checkout/production/diff?from=${revision}`,
    "/apps/:project/:env/diff",
    <DiffPage />,
  );
}

describe("DiffPage", () => {
  it("opens against the live cluster and reports its failure as one", async () => {
    renderDiff();
    expect(
      screen.getByRole("button", { name: "Against live cluster" }).getAttribute("aria-current"),
    ).toBe("true");
    expect(await screen.findByText(/no usable cluster credentials/)).toBeTruthy();
  });

  it("diffs against a deployed revision once one is picked (#162)", async () => {
    renderDiff();
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
    renderDiff();
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

  it("opens on a revision named in the URL, which is how history links here (#67)", async () => {
    renderDiffFrom("rev-00000001");

    // `?from=` only means something in the rendered mode, so it selects the tab
    // as well as the revision — and the diff is requested without a click,
    // because the link already made the choice this screen would ask for.
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
