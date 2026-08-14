import { describe, expect, it } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import {
  Code,
  ConnectError,
  createRouterTransport,
  type Transport,
} from "@connectrpc/connect";
import { create } from "@bufbuild/protobuf";

import {
  FollowLogsResponseSchema,
  LogService,
} from "../gen/kelson/v1alpha1/logs_pb";
import { DeployService } from "../gen/kelson/v1alpha1/deploy_pb";
import { SpecService } from "../gen/kelson/v1alpha1/spec_pb";
import { renderAt } from "../test/render";
import { componentNames, LogsPage } from "./LogsPage";

const PROJECT_YAML = `apiVersion: kelson.dev/v1alpha1
kind: Project
metadata:
  name: checkout
spec:
  image: ghcr.io/acme/checkout:v1
  components:
    - name: web
      port: 8080
    - name: worker
`;

/** `status` stands in for the delivery plane: `undefined` is a build without one. */
function logsTransport(namespace: string | undefined) {
  return createRouterTransport((router) => {
    router.service(DeployService, {
      status: () => {
        if (namespace === undefined) {
          throw new ConnectError(
            "the delivery plane is not available in this server",
            Code.Unimplemented,
          );
        }
        return { phase: "healthy", namespace };
      },
    });
    routes(router);
  });
}

const transport = createRouterTransport((router) => {
  router.service(DeployService, {
    status: () => ({ phase: "healthy", namespace: "checkout-production" }),
  });
  routes(router);
});

function routes(router: Parameters<Parameters<typeof createRouterTransport>[0]>[0]) {
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
    // A stream that ends is a server that went away, so the screen reconnects
    // and asks for the gap. Every follow fixture here needs an answer to that.
    queryLogs: () => ({ lines: [] }),
    followLogs: async function* () {
      yield lineEvent(1_700_000_000_000n, "listening on :8080");
      yield create(FollowLogsResponseSchema, {
        event: { case: "dropped", value: 12n },
      });
      yield lineEvent(0n, "a line the engine could not timestamp");
    },
  });
}

function lineEvent(timestampUnixMs: bigint, message: string, pod = "web-6c9") {
  return create(FollowLogsResponseSchema, {
    event: {
      case: "line",
      value: { timestampUnixMs, pod, container: "web", message },
    },
  });
}

/** A follow whose second half is released by the test, not by the clock. */
function gatedTransport() {
  let release: () => void = () => {};
  const held = new Promise<void>((resolve) => {
    release = resolve;
  });
  const transport = createRouterTransport((router) => {
    router.service(DeployService, {
      status: () => ({ phase: "healthy", namespace: "checkout-production" }),
    });
    routes(router);
    router.service(LogService, {
      queryLogs: () => ({ lines: [] }),
      followLogs: async function* () {
        yield lineEvent(1_000n, "before the pause");
        await held;
        yield lineEvent(2_000n, "while paused");
        // Stay open: pausing must not be a reason for the stream to end, and a
        // stream that ended would send the screen into its reconnect loop.
        await new Promise<void>(() => {});
      },
    });
  });
  return { transport, release: () => release() };
}

/**
 * A follow that drops after one line, and a Query that answers the gap with
 * that line plus the ones missed. The second follow replays from the resume
 * point too — both overlaps are the reconnect's real shape.
 */
function droppingTransport() {
  const queries: { tail: number; since: bigint }[] = [];
  let follows = 0;
  const transport = createRouterTransport((router) => {
    router.service(DeployService, {
      status: () => ({ phase: "healthy", namespace: "checkout-production" }),
    });
    routes(router);
    router.service(LogService, {
      queryLogs: (req) => {
        queries.push({ tail: req.tail, since: req.sinceUnixMs });
        return {
          lines: [
            { timestampUnixMs: 1_000n, pod: "web-6c9", container: "web", message: "one" },
            { timestampUnixMs: 2_000n, pod: "web-6c9", container: "web", message: "two" },
          ],
        };
      },
      followLogs: async function* () {
        follows += 1;
        if (follows === 1) {
          yield lineEvent(1_000n, "one");
          throw new ConnectError("connection reset", Code.Unavailable);
        }
        yield lineEvent(1_000n, "one");
        yield lineEvent(2_000n, "two");
        yield lineEvent(3_000n, "three");
        await new Promise<void>(() => {});
      },
    });
  });
  return { transport, queries };
}

function renderLogs(t: Transport = transport) {
  return renderAt(
    t,
    "/projects/checkout/production/logs",
    "/projects/:project/:env/logs",
    <LogsPage />,
  );
}

describe("componentNames", () => {
  it("reads the names out of the stored Project document", () => {
    expect(componentNames(PROJECT_YAML)).toEqual(["web", "worker"]);
  });

  it("returns nothing rather than guessing when there is no list", () => {
    expect(componentNames("kind: Project\nspec:\n  image: x\n")).toEqual([]);
  });

  // One list means the picker can now see components that have no pods at all.
  // Offering a database in a log picker would promise a stream nothing can
  // produce (ADR-0014), so the data kinds are dropped.
  it("leaves out data components, which have no logs", () => {
    const yaml = `apiVersion: kelson.dev/v1alpha1
kind: Project
metadata:
  name: checkout
spec:
  image: ghcr.io/acme/checkout:v1
  components:
    - name: db
      kind: postgres
      preset: small
    - name: cache
      kind: valkey
    - name: web
      port: 8080
`;
    expect(componentNames(yaml)).toEqual(["web"]);
  });
});

describe("LogsPage", () => {
  it("prefills the namespace with the one Status resolved (#161)", async () => {
    renderLogs(logsTransport("checkout-sandbox"));
    const namespace = screen.getByPlaceholderText("checkout-production");
    // The guess is what the field opens with; the server's answer replaces it.
    expect((namespace as HTMLInputElement).value).toBe("checkout-production");
    await waitFor(() =>
      expect((namespace as HTMLInputElement).value).toBe("checkout-sandbox"),
    );
    expect(screen.getByText(/resolved by the server/)).toBeTruthy();
    // The workload picker fills in from the parsed spec once it arrives.
    await waitFor(() =>
      expect(screen.getByText(/from the stored Project document/)).toBeTruthy(),
    );
  });

  it("keeps the model's default when the server cannot resolve one", async () => {
    renderLogs(logsTransport(undefined));
    const namespace = screen.getByPlaceholderText("checkout-production");
    await waitFor(() =>
      expect(screen.getByText(/the model's default for this pair/)).toBeTruthy(),
    );
    expect((namespace as HTMLInputElement).value).toBe("checkout-production");
  });

  it("leaves an edited namespace alone once it has been resolved", async () => {
    renderLogs(logsTransport("checkout-sandbox"));
    const namespace = screen.getByPlaceholderText("checkout-production");
    await waitFor(() =>
      expect((namespace as HTMLInputElement).value).toBe("checkout-sandbox"),
    );
    fireEvent.change(namespace, { target: { value: "checkout-other" } });
    expect((namespace as HTMLInputElement).value).toBe("checkout-other");
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

  it("filters the retained lines as you type, and marks the hits", async () => {
    renderLogs();
    fireEvent.click(screen.getByRole("button", { name: "Follow" }));
    fireEvent.click(screen.getByRole("button", { name: "Start following" }));
    expect(await screen.findByText("listening on :8080")).toBeTruthy();
    expect(
      await screen.findByText("a line the engine could not timestamp"),
    ).toBeTruthy();

    // Client-side and instant: the query is not sent upstream, so it applies to
    // lines that already arrived and clearing it brings them back.
    fireEvent.change(screen.getByLabelText("Find in the retained lines"), {
      target: { value: "listening" },
    });
    await waitFor(() =>
      expect(screen.queryByText("a line the engine could not timestamp")).toBeNull(),
    );
    expect(screen.getByText("listening").tagName).toBe("MARK");
    expect(screen.getByText(/1 of 2 retained lines shown/)).toBeTruthy();

    fireEvent.change(screen.getByLabelText("Find in the retained lines"), {
      target: { value: "" },
    });
    expect(
      await screen.findByText("a line the engine could not timestamp"),
    ).toBeTruthy();
  });

  // Pause buffers. It does not drop the lines and it does not close the stream:
  // both would make "pause" mean "lose what happens while you read".
  it("holds new lines while paused and releases them on resume", async () => {
    const { transport: t, release } = gatedTransport();
    renderLogs(t);
    fireEvent.click(screen.getByRole("button", { name: "Follow" }));
    fireEvent.click(screen.getByRole("button", { name: "Start following" }));
    expect(await screen.findByText("before the pause")).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Pause" }));
    release();

    const pill = await screen.findByRole("button", { name: /1 new line/ });
    expect(screen.queryByText("while paused")).toBeNull();
    // The line is held, not lost — and the stream was never torn down, so
    // stopping is still the only thing that ends it.
    expect(screen.getByRole("button", { name: "Stop following" })).toBeTruthy();

    fireEvent.click(pill);
    expect(await screen.findByText("while paused")).toBeTruthy();
    expect(screen.queryByRole("button", { name: /new line/ })).toBeNull();
  });

  it("reconnects, backfills the gap and shows nothing twice", async () => {
    const { transport: t, queries } = droppingTransport();
    renderLogs(t);
    fireEvent.click(screen.getByRole("button", { name: "Follow" }));
    fireEvent.click(screen.getByRole("button", { name: "Start following" }));
    expect(await screen.findByText("one")).toBeTruthy();

    // The backoff is a second, so this waits for a real one.
    expect(await screen.findByText("three", undefined, { timeout: 5_000 })).toBeTruthy();

    // The gap was asked for as a bounded query from the last instant seen…
    expect(queries).toHaveLength(1);
    expect(queries[0]?.since).toBe(1_000n);
    expect(queries[0]?.tail).toBeGreaterThan(0);
    // …and neither the query's overlap nor the new follow's replay doubled.
    expect(screen.getAllByText("one")).toHaveLength(1);
    expect(screen.getAllByText("two")).toHaveLength(1);
    expect(screen.getByText(/3 lines retained/)).toBeTruthy();
  }, 10_000);

  it("stops rather than retrying when the server cannot follow at all", async () => {
    const t = createRouterTransport((router) => {
      router.service(DeployService, {
        status: () => ({ phase: "healthy", namespace: "checkout-production" }),
      });
      routes(router);
      router.service(LogService, {
        followLogs: async function* () {
          throw new ConnectError("log following", Code.Unimplemented);
        },
      });
    });
    renderLogs(t);
    fireEvent.click(screen.getByRole("button", { name: "Follow" }));
    fireEvent.click(screen.getByRole("button", { name: "Start following" }));

    expect(await screen.findByText("The log stream failed")).toBeTruthy();
    // The reason replaces the stream, rather than a spinner hiding it.
    expect(screen.getByRole("button", { name: "Start following" })).toBeTruthy();
  });
});
