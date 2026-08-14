import { describe, expect, it } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import { Code, ConnectError, createRouterTransport } from "@connectrpc/connect";
import { create } from "@bufbuild/protobuf";

import { DryRun, ErrorSchema } from "../gen/kelson/v1alpha1/common_pb";
import {
  DeployService,
  PromotionStatus,
  type PromoteRequest,
} from "../gen/kelson/v1alpha1/deploy_pb";
import { SpecService } from "../gen/kelson/v1alpha1/spec_pb";
import { renderAt } from "../test/render";
import { PromotePage } from "./PromotePage";

/**
 * The promotion flow, against a stub server.
 *
 * Two properties are load-bearing and are asserted on the *requests* rather
 * than on the screen, because they are the ones a screenshot cannot show:
 * nothing but `dry_run=RENDER` leaves the browser until the confirm is pressed,
 * and the confirm carries the version the plan was computed against.
 */

const STAGING_DIGEST =
  "ghcr.io/acme/checkout@sha256:9f6ad2c1e4b5a70d3c8f1e2b4a6d8c0e2f4a6b8d0c2e4f6a8b0d2c4e6f8a0b2c4";
const PRODUCTION_DIGEST =
  "ghcr.io/acme/checkout@sha256:11223344556677889900aabbccddeeff11223344556677889900aabbccddeeff";
const WORKER_DIGEST = "ghcr.io/acme/worker:2.1.0";

const DIFF_JSON = new TextEncoder().encode(
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
            before: PRODUCTION_DIGEST,
            after: STAGING_DIGEST,
            origin: "spec",
            risk: "restart-required",
          },
        ],
      },
    ],
    summary: { added: 0, modified: 1, removed: 0, maxRisk: "restart-required" },
  }),
);

/** The plan the stub answers a dry run with: one of each decision. */
const COMPONENTS = [
  {
    component: "web",
    fromImage: PRODUCTION_DIGEST,
    toImage: STAGING_DIGEST,
    status: PromotionStatus.PINNED,
  },
  {
    component: "worker",
    fromImage: WORKER_DIGEST,
    toImage: WORKER_DIGEST,
    status: PromotionStatus.UNCHANGED,
  },
  {
    component: "reports",
    fromImage: "",
    toImage: "",
    status: PromotionStatus.SKIPPED,
    code: "promote/not-in-revision",
    reason:
      "the source environment's latest revision records no image for this component",
  },
];

interface Options {
  /** How many real writes fail with store/version-conflict before one lands. */
  conflicts?: number;
  /** Structured findings the dry run answers with; nothing is written then. */
  refuse?: boolean;
}

function stub(options: Options = {}) {
  const promotes: PromoteRequest[] = [];
  let conflicts = options.conflicts ?? 0;
  let version = "7";

  const transport = createRouterTransport((router) => {
    router.service(SpecService, {
      getSpec: () => ({
        spec: {
          project: "checkout",
          version,
          environments: ["development", "production", "staging"],
        },
      }),
    });

    router.service(DeployService, {
      promote: (req) => {
        promotes.push(req);
        if (options.refuse === true) {
          return {
            components: COMPONENTS,
            fromRevision: "rev-00000009",
            errors: [
              create(ErrorSchema, {
                code: "render/unresolved-image",
                resource: "Environment/production",
                message:
                  "component reports has no image after the promotion, so the environment cannot render",
                remediation:
                  "promote a revision that carries reports, or give reports an image in the spec",
              }),
            ],
          };
        }
        if (req.dryRun === DryRun.RENDER) {
          return {
            components: COMPONENTS,
            fromRevision: "rev-00000009",
            diffJson: DIFF_JSON,
            exitSemantics: 2,
          };
        }
        if (conflicts > 0) {
          conflicts -= 1;
          // The store's own answer, details and all: the spec moved under the
          // plan, so the write was refused rather than applied.
          version = "8";
          throw new ConnectError(
            'project "checkout" changed while this write was in flight',
            Code.FailedPrecondition,
            undefined,
            [
              {
                desc: ErrorSchema,
                value: create(ErrorSchema, {
                  code: "store/version-conflict",
                  resource: "Spec/checkout",
                  message:
                    'project "checkout" changed while this write was in flight',
                  remediation:
                    "re-read the spec with GetSpec and retry the write with the version it returns",
                }),
              },
            ],
          );
        }
        return {
          components: COMPONENTS,
          fromRevision: "rev-00000009",
          version: "9",
          diffJson: DIFF_JSON,
          exitSemantics: 2,
        };
      },
    });
  });

  return { promotes, transport };
}

function renderPromote(options: Options = {}) {
  const { promotes, transport } = stub(options);
  const view = renderAt(
    transport,
    "/projects/checkout/production/promote",
    "/projects/:project/:env/promote",
    <PromotePage />,
  );
  return { promotes, ...view };
}

/** Picks a source and waits for the dry run to come back. */
async function planFrom(source: string) {
  fireEvent.click(await screen.findByDisplayValue(source));
  await screen.findByText(/What would be pinned/);
}

describe("PromotePage", () => {
  it("offers the project's other environments as sources, and asks for nothing until one is picked", async () => {
    const { promotes } = renderPromote();

    const radios = (await screen.findAllByRole("radio")) as HTMLInputElement[];
    // The environment in the URL is the target, so it is not among the
    // sources: promoting an environment to itself is refused by the server and
    // is not offered here either.
    expect(radios.map((r) => r.value)).toEqual(["development", "staging"]);
    expect(promotes).toHaveLength(0);
    // The screen says which end of the promotion this environment is.
    expect(
      screen.getByRole("heading", { name: "Promote into production" }),
    ).toBeTruthy();
  });

  it("plans on picking a source, and renders every decision the server made", async () => {
    const { promotes } = renderPromote();
    await planFrom("staging");

    expect(promotes).toHaveLength(1);
    expect(promotes[0]?.fromEnvironment).toBe("staging");
    expect(promotes[0]?.toEnvironment).toBe("production");
    expect(promotes[0]?.dryRun).toBe(DryRun.RENDER);

    // All three statuses, all three components — including the one nothing
    // happens to, which must not be indistinguishable from one kelson forgot.
    expect(screen.getByText("What would be pinned (3)")).toBeTruthy();
    expect(screen.getByText("pinned")).toBeTruthy();
    expect(screen.getByText("unchanged")).toBeTruthy();
    expect(screen.getByText("skipped")).toBeTruthy();
    expect(screen.getByText("1 pinned, 1 unchanged, 1 skipped")).toBeTruthy();
    expect(screen.getByText("web")).toBeTruthy();
    expect(screen.getByText("worker")).toBeTruthy();
    expect(screen.getByText("reports")).toBeTruthy();

    // A skip carries the server's own code and its reason in prose.
    expect(screen.getByText("promote/not-in-revision")).toBeTruthy();
    expect(
      screen.getByText(
        "the source environment's latest revision records no image for this component",
      ),
    ).toBeTruthy();
    // The component with no pin today says that, rather than showing nothing.
    expect(screen.getByText("unpinned")).toBeTruthy();

    // The revision the images were read from is named and copyable.
    expect(screen.getByText("rev-00000009")).toBeTruthy();

    // The response's diff_json goes through the existing diff presentation.
    expect(screen.getByText("Deployment/web")).toBeTruthy();
    expect(
      screen.getByText("spec.template.spec.containers[0].image"),
    ).toBeTruthy();
  });

  it("shows a long digest middle-truncated, with the whole of it on the title", async () => {
    renderPromote();
    await planFrom("staging");

    const pinned = screen.getByRole("button", { name: `copy ${STAGING_DIGEST}` });
    expect(pinned.getAttribute("title")).toBe(STAGING_DIGEST);
    expect(pinned.textContent).toContain("…");
    expect(pinned.textContent).not.toBe(STAGING_DIGEST);
    // The repository and the leading hex still read at a glance.
    expect(pinned.textContent).toContain("ghcr.io/acme/checkout@sha256:9f6ad2c1");
  });

  it("writes nothing until the confirm, then sends the version and an idempotency key", async () => {
    const { promotes } = renderPromote();
    await planFrom("staging");

    // Everything so far has been a dry run. A screen that had already written
    // would look exactly like this one.
    expect(promotes.every((p) => p.dryRun === DryRun.RENDER)).toBe(true);
    expect(
      screen.getByText(/nothing has been written yet/),
    ).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Pin these images" }));
    await waitFor(() => expect(promotes).toHaveLength(2));

    const write = promotes[1];
    expect(write?.dryRun).toBe(DryRun.NONE);
    // The version GetSpec answered when the plan was computed — the optimistic
    // concurrency PutSpec carries.
    expect(write?.version).toBe("7");
    expect(write?.idempotencyKey).not.toBe("");
    expect(write?.fromEnvironment).toBe("staging");
    expect(write?.toEnvironment).toBe("production");
  });

  it("reports what was pinned, says nothing was deployed, and leads to the deploy screen", async () => {
    renderPromote();
    await planFrom("staging");
    fireEvent.click(screen.getByRole("button", { name: "Pin these images" }));

    expect(
      await screen.findByText("1 image pin written to checkout"),
    ).toBeTruthy();
    // The new spec version, and the revision the image came from.
    expect(screen.getByText(/spec version 9/)).toBeTruthy();
    // The one thing promotion does not do is stated, not implied.
    expect(
      screen.getByText(
        /nothing has been deployed — production is still running what it was running before this promotion/,
      ),
    ).toBeTruthy();
    expect(
      screen
        .getByRole("link", { name: "Next: deploy production" })
        .getAttribute("href"),
    ).toBe("/projects/checkout/production/deploy");
  });

  it("renders a structured refusal through the shared error panel and offers no confirm", async () => {
    const { promotes } = renderPromote({ refuse: true });
    fireEvent.click(await screen.findByDisplayValue("staging"));

    expect(
      await screen.findByText(
        "This promotion was refused — nothing would be written",
      ),
    ).toBeTruthy();
    // The server's own code and remediation, unmapped.
    expect(screen.getByText("render/unresolved-image")).toBeTruthy();
    expect(
      screen.getByText(
        "promote a revision that carries reports, or give reports an image in the spec",
      ),
    ).toBeTruthy();
    // Findings mean nothing was written, so there is nothing to confirm.
    expect(screen.queryByRole("button", { name: "Pin these images" })).toBeNull();
    expect(promotes).toHaveLength(1);
  });

  it("answers a version conflict with a re-plan, not a blind retry", async () => {
    const { promotes } = renderPromote({ conflicts: 1 });
    await planFrom("staging");
    fireEvent.click(screen.getByRole("button", { name: "Pin these images" }));

    expect(await screen.findByText("store/version-conflict")).toBeTruthy();
    expect(
      screen.getByText("The spec changed while this plan was on screen"),
    ).toBeTruthy();
    // Nothing was written, and the confirm is not left armed with a plan that
    // is about bytes the store no longer holds.
    expect(promotes).toHaveLength(2);
    expect(
      (
        screen.getByRole("button", {
          name: "Pin these images",
        }) as HTMLButtonElement
      ).disabled,
    ).toBe(true);

    fireEvent.click(
      screen.getByRole("button", { name: "Plan again from the stored spec" }),
    );

    // The re-plan is a fresh dry run against the spec as it is now — and the
    // confirm that follows carries the version that re-read returned.
    await waitFor(() => expect(promotes).toHaveLength(3));
    expect(promotes[2]?.dryRun).toBe(DryRun.RENDER);

    fireEvent.click(
      await screen.findByRole("button", { name: "Pin these images" }),
    );
    await waitFor(() => expect(promotes).toHaveLength(4));
    expect(promotes[3]?.dryRun).toBe(DryRun.NONE);
    expect(promotes[3]?.version).toBe("8");
    expect(
      await screen.findByText("1 image pin written to checkout"),
    ).toBeTruthy();
  });

  it("offers no confirmation when the plan would pin nothing", async () => {
    const unchangedOnly = createRouterTransport((router) => {
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
        promote: () => ({
          components: [
            {
              component: "web",
              fromImage: STAGING_DIGEST,
              toImage: STAGING_DIGEST,
              status: PromotionStatus.UNCHANGED,
            },
          ],
          fromRevision: "rev-00000009",
          diffJson: DIFF_JSON,
          exitSemantics: 0,
        }),
      });
    });

    renderAt(
      unchangedOnly,
      "/projects/checkout/production/promote",
      "/projects/:project/:env/promote",
      <PromotePage />,
    );
    fireEvent.click(await screen.findByDisplayValue("staging"));

    expect(await screen.findByText(/nothing to pin/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Pin these images" })).toBeNull();
  });

  it("says so when the project declares no other environment to promote from", async () => {
    const alone = createRouterTransport((router) => {
      router.service(SpecService, {
        getSpec: () => ({
          spec: {
            project: "checkout",
            version: "7",
            environments: ["production"],
          },
        }),
      });
    });

    renderAt(
      alone,
      "/projects/checkout/production/promote",
      "/projects/:project/:env/promote",
      <PromotePage />,
    );

    expect(
      await screen.findByText("There is no environment to promote from"),
    ).toBeTruthy();
    expect(screen.queryAllByRole("radio")).toHaveLength(0);
  });
});
