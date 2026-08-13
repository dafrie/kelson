import { describe, expect, it } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import { createRouterTransport } from "@connectrpc/connect";
import { create } from "@bufbuild/protobuf";

import {
  FollowLogsResponseSchema,
  LogService,
} from "../gen/kelson/v1alpha1/logs_pb";
import { SpecService } from "../gen/kelson/v1alpha1/spec_pb";
import { renderAt } from "../test/render";
import { applicationNames, LogsPage } from "./LogsPage";

const PROJECT_YAML = `apiVersion: kelson.dev/v1alpha1
kind: Project
metadata:
  name: checkout
spec:
  image: ghcr.io/acme/checkout:v1
  applications:
    - name: web
      port: 8080
    - name: worker
`;

const transport = createRouterTransport((router) => {
  router.service(SpecService, {
    getSpec: () => ({
      spec: {
        project: "checkout",
        version: "7",
        environments: ["production"],
        documents: {
          project: new TextEncoder().encode(PROJECT_YAML),
          environments: {},
        },
      },
    }),
  });

  router.service(LogService, {
    followLogs: async function* () {
      yield create(FollowLogsResponseSchema, {
        event: {
          case: "line",
          value: {
            timestampUnixMs: 1_700_000_000_000n,
            pod: "web-6c9",
            container: "web",
            message: "listening on :8080",
          },
        },
      });
      yield create(FollowLogsResponseSchema, {
        event: { case: "dropped", value: 12n },
      });
      yield create(FollowLogsResponseSchema, {
        event: {
          case: "line",
          value: {
            timestampUnixMs: 0n,
            pod: "web-6c9",
            container: "web",
            message: "a line the engine could not timestamp",
          },
        },
      });
    },
  });
});

function renderLogs() {
  return renderAt(
    transport,
    "/apps/checkout/production/logs",
    "/apps/:project/:env/logs",
    <LogsPage />,
  );
}

describe("applicationNames", () => {
  it("reads the names out of the stored Project document", () => {
    expect(applicationNames(PROJECT_YAML)).toEqual(["web", "worker"]);
  });

  it("returns nothing rather than guessing when there is no list", () => {
    expect(applicationNames("kind: Project\nspec:\n  image: x\n")).toEqual([]);
  });
});

describe("LogsPage", () => {
  it("prefills the namespace with the model's default for the pair", async () => {
    renderLogs();
    const namespace = screen.getByPlaceholderText("checkout-production");
    expect((namespace as HTMLInputElement).value).toBe("checkout-production");
    // The application picker fills in from the parsed spec once it arrives.
    await waitFor(() =>
      expect(screen.getByText(/from the stored Project document/)).toBeTruthy(),
    );
  });

  it("streams lines and reports dropped ones as loss, not silence", async () => {
    renderLogs();

    // The mode tab, then the action: the screen opens in the bounded mode and
    // an unbounded stream is never something a page start does by itself.
    fireEvent.click(screen.getByRole("button", { name: "Follow" }));
    fireEvent.click(screen.getByRole("button", { name: "Start following" }));

    expect(await screen.findByText("listening on :8080")).toBeTruthy();
    expect(
      await screen.findByText("a line the engine could not timestamp"),
    ).toBeTruthy();
    expect(screen.getAllByText("web-6c9")).toHaveLength(2);

    // A zero timestamp is "no parseable timestamp", never the Unix epoch.
    expect(screen.getByText("--:--:--")).toBeTruthy();

    const banner = await screen.findByText("12 lines dropped");
    expect(banner).toBeTruthy();
    expect(
      screen.getByText(/this gap is loss, not\s+silence/),
    ).toBeTruthy();
  });
});
