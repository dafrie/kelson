import { describe, expect, it } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import { createRouterTransport } from "@connectrpc/connect";
import { create } from "@bufbuild/protobuf";

import { DryRun } from "../gen/kelson/v1alpha1/common_pb";
import {
  DeployResponseSchema,
  DeployService,
} from "../gen/kelson/v1alpha1/deploy_pb";
import type { DeployRequest } from "../gen/kelson/v1alpha1/deploy_pb";
import { renderAt } from "../test/render";
import { DeployPage } from "./DeployPage";

/**
 * Two behaviours, both of them the schema's:
 *
 *   - dry_run=RENDER ends after Proposed, and the manifests ride that event.
 *   - dry_run=NONE tells the whole story in order, and a deployment that
 *     settles unhealthy completes the stream *cleanly* with the error on the
 *     Settled event. The screen must render that as the deploy's outcome, not
 *     as a transport failure.
 */

const yaml = (text: string) => new TextEncoder().encode(text);

function transportFor(events: () => AsyncIterable<unknown>, seen?: DeployRequest[]) {
  return createRouterTransport((router) => {
    router.service(DeployService, {
      deploy: async function* (req) {
        seen?.push(req);
        if (req.dryRun === DryRun.RENDER) {
          yield create(DeployResponseSchema, {
            event: {
              case: "proposed",
              value: {
                project: "checkout",
                environment: "production",
                resources: 2,
                mode: "direct",
                manifests: [
                  {
                    apiVersion: "v1",
                    kind: "Namespace",
                    name: "checkout-production",
                    yaml: yaml("apiVersion: v1\nkind: Namespace\n"),
                  },
                  {
                    apiVersion: "apps/v1",
                    kind: "Deployment",
                    name: "web",
                    namespace: "checkout-production",
                    yaml: yaml("apiVersion: apps/v1\nkind: Deployment\n"),
                  },
                ],
              },
            },
          });
          return;
        }
        yield* events() as AsyncIterable<never>;
      },
    });
  });
}

async function* unhealthyDeploy() {
  yield create(DeployResponseSchema, {
    event: {
      case: "proposed",
      value: { project: "checkout", environment: "production", resources: 2, mode: "direct" },
    },
  });
  yield create(DeployResponseSchema, {
    event: { case: "committed", value: { revision: "rev-9", adapter: "direct" } },
  });
  yield create(DeployResponseSchema, {
    event: {
      case: "transition",
      value: { phase: "Committed", answer: "waiting" },
    },
  });
  yield create(DeployResponseSchema, {
    event: {
      case: "transition",
      value: {
        phase: "Reconciling",
        answer: "progressing",
        cause: { component: "direct", reason: "rolling-out", message: "1 of 3 replicas updated" },
      },
    },
  });
  yield create(DeployResponseSchema, {
    event: {
      case: "settled",
      value: {
        final: { phase: "Degraded", answer: "degraded", stuck: true },
        error: {
          code: "delivery/unhealthy",
          resource: "Deployment/checkout-production/web",
          message: "web did not become healthy within the budget",
          remediation: "kelson logs web --at-termination to see why the container exits",
        },
      },
    },
  });
}

function renderDeploy(
  events: () => AsyncIterable<unknown>,
  opts: { search?: string; seen?: DeployRequest[] } = {},
) {
  return renderAt(
    transportFor(events, opts.seen),
    "/projects/checkout/production/deploy" + (opts.search ?? ""),
    "/projects/:project/:env/deploy",
    <DeployPage />,
  );
}

describe("DeployPage", () => {
  it("previews with a render dry-run: resource count, mode and manifests", async () => {
    renderDeploy(unhealthyDeploy);

    expect(await screen.findByText("direct")).toBeTruthy();
    expect(screen.getByText("Namespace/checkout-production")).toBeTruthy();
    expect(screen.getByText("Deployment/web")).toBeTruthy();
    // The byte-faithful YAML is there, collapsed behind its disclosure.
    expect(screen.getByText(/kind: Deployment/)).toBeTruthy();
    // The confirm button names exactly what it would do.
    expect(
      screen.getByRole("button", {
        name: "Apply 2 resources to checkout/production",
      }),
    ).toBeTruthy();
  });

  it("appends transitions as they arrive and renders a settled error", async () => {
    const { container } = renderDeploy(unhealthyDeploy);

    const confirm = await screen.findByRole("button", {
      name: "Apply 2 resources to checkout/production",
    });
    fireEvent.click(confirm);

    // Committed carries the revision and the adapter that assigned it.
    expect(await screen.findByText(/rev-9/)).toBeTruthy();
    await waitFor(() => {
      const rows = [...container.querySelectorAll(".k-stream__row")];
      expect(rows.length).toBeGreaterThanOrEqual(4);
    });

    // Every transition is a row, in arrival order, with its cause.
    expect(screen.getByText("committed", { selector: ".k-pill" })).toBeTruthy();
    expect(screen.getByText("reconciling", { selector: ".k-pill" })).toBeTruthy();
    expect(
      screen.getByText("direct/rolling-out: 1 of 3 replicas updated"),
    ).toBeTruthy();

    // The terminal state is the deploy's answer, structured error and all —
    // and it is the rail that answers it, naming which of the three failures
    // this is rather than settling for a red badge (#68).
    await waitFor(() => {
      expect(
        container.querySelector('[data-diagnosis="unhealthy"]'),
      ).toBeTruthy();
    });
    expect(screen.getByText(/Debug the workload/)).toBeTruthy();
    expect(screen.getByText("delivery/unhealthy")).toBeTruthy();
    expect(
      screen.getByText("web did not become healthy within the budget"),
    ).toBeTruthy();
    expect(screen.getByText("fix:")).toBeTruthy();
    // The raw event log keeps the terminal row, secondary to the rail.
    expect(screen.getByText("Deployment settled")).toBeTruthy();
  });

  it("names the reconciler on the rail from the adapter the server reported", async () => {
    renderDeploy(unhealthyDeploy);

    fireEvent.click(
      await screen.findByRole("button", {
        name: "Apply 2 resources to checkout/production",
      }),
    );

    // Committed.adapter is "direct": the reconciling stage is kelson itself,
    // and saying so is what tells a reader there is no Flux to go look at.
    expect(await screen.findByText("kelson (direct apply)")).toBeTruthy();
    expect(screen.queryByText(/not reported/)).toBeNull();
  });

  it("renders a healthy deployment as a success, not as an error", async () => {
    renderDeploy(async function* () {
      yield create(DeployResponseSchema, {
        event: { case: "committed", value: { revision: "rev-10", adapter: "direct" } },
      });
      yield create(DeployResponseSchema, {
        event: {
          case: "settled",
          value: {
            final: { phase: "Healthy", answer: "live", observedRevision: "rev-10" },
          },
        },
      });
    });

    fireEvent.click(
      await screen.findByRole("button", {
        name: "Apply 2 resources to checkout/production",
      }),
    );

    expect(await screen.findByText("Deployment settled")).toBeTruthy();
    expect(screen.getByText("healthy", { selector: ".k-pill" })).toBeTruthy();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  // ?image= is how the build screen hands a freshly built reference over
  // (#63). It is DeployRequest.image — the same field the CLI's --image fills —
  // and it is what lets a project that builds from source be deployed at all.
  it("carries an image override into both the preview and the deploy", async () => {
    const image = "ghcr.io/acme/checkout@sha256:" + "b".repeat(64);
    const seen: DeployRequest[] = [];
    renderDeploy(
      async function* () {
        yield create(DeployResponseSchema, {
          event: { case: "committed", value: { revision: "rev-11", adapter: "direct" } },
        });
        yield create(DeployResponseSchema, {
          event: { case: "settled", value: { final: { phase: "Healthy", answer: "live" } } },
        });
      },
      { search: `?image=${encodeURIComponent(image)}`, seen },
    );

    // Shown, because deploying something other than what the spec says is not
    // a fact the reader should have to infer.
    expect(await screen.findByText("Image override")).toBeTruthy();

    fireEvent.click(
      await screen.findByRole("button", {
        name: "Apply 2 resources to checkout/production",
      }),
    );
    await screen.findByText("Deployment settled");

    // Both calls carry it: a preview that rendered a different image would be
    // a preview of something else.
    expect(seen.map((req) => req.image)).toEqual([image, image]);
  });
});
