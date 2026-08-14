import { describe, expect, it } from "vitest";
import { fireEvent, screen } from "@testing-library/react";
import { createRouterTransport } from "@connectrpc/connect";
import { create } from "@bufbuild/protobuf";

import { DryRun } from "../gen/kelson/v1alpha1/common_pb";
import {
  DeployService,
  RollbackResponseSchema,
} from "../gen/kelson/v1alpha1/deploy_pb";
import { renderAt } from "../test/render";
import { RollbackPage } from "./RollbackPage";

/**
 * The preview is the point. RollbackResponse streams Preview first and always
 * (internal/api/deploy.go), and a finding flagged unrecoverable names something
 * the restore will NOT undo — the one thing a person about to roll back has to
 * read before they press the button.
 */
const DIFF_JSON = JSON.stringify({
  level: "rendered",
  project: "checkout",
  environment: "production",
  resources: [
    {
      apiVersion: "apps/v1",
      kind: "Deployment",
      name: "web",
      op: "modified",
      risk: "restart-required",
    },
  ],
  summary: { added: 0, modified: 1, removed: 0, maxRisk: "restart-required" },
});

const transport = createRouterTransport((router) => {
  router.service(DeployService, {
    history: () => ({
      entries: [
        { revision: "rev-9", committedAt: "2026-08-13T09:00:00Z", author: "ci", message: "raise replicas" },
        { revision: "rev-8", committedAt: "2026-08-12T17:31:00Z", author: "ci", message: "bump image" },
        { revision: "rev-7", committedAt: "2026-08-12T09:02:00Z", author: "hb" },
      ],
    }),
    rollback: async function* (req) {
      yield create(RollbackResponseSchema, {
        event: {
          case: "preview",
          value: {
            toRevision: req.toRevision,
            diffJson: new TextEncoder().encode(DIFF_JSON),
            findings: [
              {
                resource: "PersistentVolumeClaim/checkout-production/data",
                path: "spec.resources.requests.storage",
                cause: "storage-expanded",
                message:
                  "the volume was expanded to 50Gi; a rollback cannot shrink it back to 20Gi",
                unrecoverable: true,
              },
              {
                resource: "Deployment/checkout-production/web",
                path: "spec.replicas",
                cause: "scaled",
                message: "replicas will return to 3",
                unrecoverable: false,
              },
            ],
          },
        },
      });
      if (req.dryRun === DryRun.RENDER) return;
      yield create(RollbackResponseSchema, {
        event: {
          case: "committed",
          value: { restoredRevision: req.toRevision, asRevision: "rev-10" },
        },
      });
      yield create(RollbackResponseSchema, {
        event: { case: "settled", value: {} },
      });
    },
  });
});

function renderRollback() {
  return renderAt(
    transport,
    "/projects/checkout/production/rollback",
    "/projects/:project/:env/rollback",
    <RollbackPage />,
  );
}

/** The screen as the history timeline links to it: a target in the URL. */
function renderRollbackTo(revision: string) {
  return renderAt(
    transport,
    `/projects/checkout/production/rollback?to=${revision}`,
    "/projects/:project/:env/rollback",
    <RollbackPage />,
  );
}

describe("RollbackPage", () => {
  it("offers the recorded revisions newest first, with the current one marked", async () => {
    renderRollback();

    expect(await screen.findByText("rev-9")).toBeTruthy();
    const radios = screen.getAllByRole("radio") as HTMLInputElement[];
    expect(radios.map((r) => r.value)).toEqual(["rev-9", "rev-8", "rev-7"]);
    // The live revision is not a rollback target.
    expect(radios[0]?.disabled).toBe(true);
    expect(screen.getByText("current")).toBeTruthy();
    expect(radios[1]?.disabled).toBe(false);
  });

  it("previews before applying, flagging what cannot be reverted", async () => {
    renderRollback();

    const radios = await screen.findAllByRole("radio");
    fireEvent.click(radios[1] as HTMLElement);

    expect(
      await screen.findByText(
        "the volume was expanded to 50Gi; a rollback cannot shrink it back to 20Gi",
      ),
    ).toBeTruthy();
    expect(screen.getByText("cannot revert")).toBeTruthy();
    expect(screen.getByText("storage-expanded")).toBeTruthy();
    // The recoverable finding is present but not flagged.
    expect(screen.getByText("replicas will return to 3")).toBeTruthy();
    expect(screen.getAllByText("cannot revert")).toHaveLength(1);
    // The heading counts the unrecoverable ones out of the total.
    expect(screen.getByText(/What this rollback cannot revert \(1 of 2\)/)).toBeTruthy();
    // The preview's diff_json is decoded and rendered alongside.
    expect(screen.getByText("Deployment/web")).toBeTruthy();
  });

  it("applies only on confirmation and reports the restored revision", async () => {
    renderRollback();

    fireEvent.click((await screen.findAllByRole("radio"))[1] as HTMLElement);
    const confirm = await screen.findByRole("button", {
      name: "Restore rev-8 to checkout/production",
    });
    fireEvent.click(confirm);

    expect(await screen.findByText("rev-8")).toBeTruthy();
    expect(await screen.findByText("rev-10")).toBeTruthy();
    expect(await screen.findByText("Rollback settled")).toBeTruthy();
  });

  it("preselects and previews a revision named in the URL, but never applies it (#67)", async () => {
    renderRollbackTo("rev-8");

    // The history screen's link lands on the preview, not on a fresh picker.
    expect(
      await screen.findByText(
        "the volume was expanded to 50Gi; a rollback cannot shrink it back to 20Gi",
      ),
    ).toBeTruthy();
    const radios = (await screen.findAllByRole("radio")) as HTMLInputElement[];
    expect(radios[1]?.checked).toBe(true);

    // Nothing was written: the apply is still the button it always was. A link
    // that deployed on arrival is a link someone can be handed.
    expect(screen.queryByText("Rollback settled")).toBeNull();
    expect(
      screen.getByRole("button", {
        name: "Restore rev-8 to checkout/production",
      }),
    ).toBeTruthy();
  });

  it("ignores a URL target the history does not offer, and says why", async () => {
    renderRollbackTo("rev-9");

    // rev-9 is what is deployed now, so the picker disables it; preselecting it
    // would preview a rollback to where the environment already is.
    expect(
      await screen.findByText(/rev-9 is what is deployed now/),
    ).toBeTruthy();
    const radios = (await screen.findAllByRole("radio")) as HTMLInputElement[];
    expect(radios.some((r) => r.checked)).toBe(false);
    expect(screen.queryByText("cannot revert")).toBeNull();
  });

  it("says when a URL target is not among the recorded revisions", async () => {
    renderRollbackTo("rev-404");

    expect(
      await screen.findByText(/rev-404 is not among the recorded revisions/),
    ).toBeTruthy();
    const radios = (await screen.findAllByRole("radio")) as HTMLInputElement[];
    expect(radios.some((r) => r.checked)).toBe(false);
  });
});
