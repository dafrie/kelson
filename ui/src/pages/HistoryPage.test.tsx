import { describe, expect, it } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import { Code, ConnectError, createRouterTransport } from "@connectrpc/connect";
import type { Transport } from "@connectrpc/connect";

import { DeployService } from "../gen/kelson/v1alpha1/deploy_pb";
import { renderAt } from "../test/render";
import { HistoryPage } from "./HistoryPage";

/**
 * The screen's contract is that it shows what History returned and nothing
 * more, so most of these tests assert an absence: no phase pill on a revision
 * nothing can answer for, no author where the mode records none, no rollback
 * link to the revision already deployed.
 *
 * Both delivery modes are exercised, because #67's acceptance is that they read
 * identically: direct mode's counter revisions with no author, and the Git
 * modes' commit shas with one.
 */

const COMMIT = "9f2c1a4b7e0d3f65a8b9c0d1e2f3a4b5c6d7e8f9";
const OLDER_COMMIT = "1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d";

/** Direct mode: a counter, a spec hash, no author at all. */
const DIRECT = [
  {
    revision: "rev-00000003",
    specHash: `sha256:${"c".repeat(64)}`,
    committedAt: "2026-08-13T10:04:05Z",
    message: "deploy sha256:cccc",
    author: "",
  },
  {
    revision: "rev-00000002",
    specHash: `sha256:${"b".repeat(64)}`,
    committedAt: "2026-08-12T09:00:00Z",
    message: "rollback to rev-00000001",
    author: "",
  },
  {
    revision: "rev-00000001",
    specHash: `sha256:${"a".repeat(64)}`,
    committedAt: "2026-08-11T08:00:00Z",
    message: "deploy sha256:aaaa",
    author: "",
  },
];

/** Git mode: commit shas and the commit signature as the author. */
const GIT = [
  {
    revision: COMMIT,
    specHash: `sha256:${"c".repeat(64)}`,
    committedAt: "2026-08-13T10:04:05Z",
    message: "kelson: update checkout/production",
    author: "Ada Lovelace <ada@example.com>",
  },
  {
    revision: OLDER_COMMIT,
    specHash: `sha256:${"b".repeat(64)}`,
    committedAt: "2026-08-12T09:00:00Z",
    message: "kelson: rollback checkout/production to 1a2b3c4d5e6f",
    author: "kelson-bot <bot@example.com>",
  },
];

interface Stub {
  entries?: typeof DIRECT;
  /** What Status reports as live; "" is "the server names no revision". */
  live?: string;
  phase?: string;
  /** When set, Status fails with this code instead of answering. */
  statusError?: Code;
}

function transportFor({
  entries = DIRECT,
  live = "rev-00000002",
  phase = "Healthy",
  statusError,
}: Stub): Transport {
  return createRouterTransport((router) => {
    router.service(DeployService, {
      history: () => ({ entries }),
      status: () => {
        if (statusError !== undefined) {
          throw new ConnectError("no delivery plane", statusError);
        }
        return { phase, revision: live, cause: "", detail: {} };
      },
    });
  });
}

function renderHistory(stub: Stub = {}) {
  return renderAt(
    transportFor(stub),
    "/apps/checkout/production/history",
    "/apps/:project/:env/history",
    <HistoryPage />,
  );
}

/** The <li> for one revision, found by the revision id it renders. */
async function row(text: string): Promise<HTMLElement> {
  const node = await screen.findByText(text);
  const item = node.closest("li");
  if (item === null) throw new Error(`no timeline item for ${text}`);
  return item;
}

describe("HistoryPage", () => {
  it("renders the recorded revisions newest first, with what each record said", async () => {
    renderHistory();

    expect(await screen.findByText("Revisions (3)")).toBeTruthy();

    const items = screen.getAllByRole("listitem");
    expect(items).toHaveLength(3);
    // The RPC returns newest first and the screen preserves that order rather
    // than re-sorting on a timestamp it does not own.
    expect(items[0]?.textContent).toContain("rev-00000003");
    expect(items[2]?.textContent).toContain("rev-00000001");

    const newest = await row("rev-00000003");
    expect(newest.textContent).toContain("2026-08-13 10:04:05Z");
    expect(newest.textContent).toContain(`spec sha256:${"c".repeat(12)}`);
    expect(newest.textContent).toContain("deploy sha256:cccc");
  });

  it("chips each record as the act the mode wrote down, in both modes", async () => {
    const { unmount } = renderHistory();
    expect(within(await row("rev-00000003")).getByText("deploy")).toBeTruthy();
    expect(within(await row("rev-00000002")).getByText("rollback")).toBeTruthy();
    unmount();

    renderHistory({ entries: GIT, live: COMMIT });
    expect(within(await row("9f2c1a4b7e0d")).getByText("deploy")).toBeTruthy();
    expect(within(await row("1a2b3c4d5e6f")).getByText("rollback")).toBeTruthy();
  });

  it("puts the phase pill only on the revision Status reports as live", async () => {
    // The live revision is deliberately not the newest: the marker follows the
    // server's answer, not a position in the list.
    const { container } = renderHistory({
      live: "rev-00000002",
      phase: "Healthy",
    });

    const live = await row("rev-00000002");
    await waitFor(() =>
      expect(within(live).getByText("deployed now")).toBeTruthy(),
    );
    expect(within(live).getByText("healthy")).toBeTruthy();

    // Every other revision gets no health claim of any kind.
    expect(within(await row("rev-00000003")).queryByText("deployed now"))
      .toBeNull();
    expect(screen.getAllByText("deployed now")).toHaveLength(1);
    expect(container.querySelectorAll(".k-pill")).toHaveLength(1);
  });

  it("carries an unhealthy live phase through the same pill vocabulary", async () => {
    renderHistory({ live: "rev-00000003", phase: "Degraded" });

    const live = await row("rev-00000003");
    await waitFor(() =>
      expect(within(live).getByText("degraded")).toBeTruthy(),
    );
    expect(
      within(live).getByText("degraded").closest(".k-pill")
        ?.getAttribute("data-status"),
    ).toBe("degraded");
  });

  it("says so when the live revision cannot be read, and marks nothing", async () => {
    renderHistory({ statusError: Code.Unimplemented });

    expect(await screen.findByText("Revisions (3)")).toBeTruthy();
    await waitFor(() =>
      expect(
        screen.getByText(/no “deployed now” marker/).textContent,
      ).toContain("unimplemented"),
    );
    expect(screen.queryByText("deployed now")).toBeNull();
    // The history itself is unaffected: an unreadable present is not an
    // unreadable past.
    expect(screen.getAllByRole("listitem")).toHaveLength(3);
  });

  it("distinguishes a server that names no live revision from one that failed", async () => {
    // A readable Status that reports no revision is a different answer from an
    // unreadable one, and it is about this environment rather than the server.
    renderHistory({ live: "" });

    await waitFor(() =>
      expect(
        screen.getByText(/no “deployed now” marker/).textContent,
      ).toContain("reports no live revision"),
    );
    expect(screen.queryByText("deployed now")).toBeNull();
  });

  it("reads authorship as the mode records it, and never invents it", async () => {
    const { unmount } = renderHistory();

    // Direct mode records no author.
    const unattributed = await screen.findAllByText("unattributed");
    expect(unattributed).toHaveLength(3);
    expect(unattributed[0]?.getAttribute("title")).toContain("#74");
    unmount();

    // The Git modes record the commit signature — and the title says what that
    // is and is not, because a signature does not say human or agent.
    renderHistory({ entries: GIT, live: COMMIT });
    const author = await screen.findByText("Ada Lovelace <ada@example.com>");
    expect(author.getAttribute("title")).toContain(
      "does not say whether a human or an agent",
    );
    expect(screen.queryByText("unattributed")).toBeNull();
  });

  it("names a commit revision as the manifests commit, and only a commit", async () => {
    const { unmount } = renderHistory({ entries: GIT, live: COMMIT });

    const item = await row("9f2c1a4b7e0d");
    expect(item.textContent).toContain("manifests commit 9f2c1a4b7e0d");
    unmount();

    // A direct-mode counter is not a commit and is never labelled as one.
    renderHistory();
    await screen.findByText("rev-00000003");
    expect(screen.queryByText(/manifests commit/)).toBeNull();
  });

  it("links each revision to the diff against what is deployed now", async () => {
    renderHistory();

    const item = await row("rev-00000001");
    expect(
      within(item)
        .getByRole("link", { name: "Diff against current" })
        .getAttribute("href"),
    ).toBe("/apps/checkout/production/diff?from=rev-00000001");
  });

  it("links a rollback that carries the revision, except for the newest", async () => {
    renderHistory();

    const older = await row("rev-00000002");
    expect(
      within(older)
        .getByRole("link", { name: "Roll back to this" })
        .getAttribute("href"),
    ).toBe("/apps/checkout/production/rollback?to=rev-00000002");

    // Restoring the newest revision is not a rollback, and the rollback screen
    // disables that target, so the link is not offered at all.
    const newest = await row("rev-00000003");
    expect(within(newest).queryByRole("link", { name: "Roll back to this" }))
      .toBeNull();
  });

  it("offers promotion once, above the list, and not per revision (#11)", async () => {
    renderHistory();

    const promote = await screen.findByRole("link", {
      name: "Promote into this environment",
    });
    expect(promote.getAttribute("href")).toBe(
      "/apps/checkout/production/promote",
    );
    // It reads the *source* environment's latest revision, so no row offers it:
    // a per-revision button would promise a promotion the RPC does not make.
    const newest = await row("rev-00000003");
    expect(
      within(newest).queryByRole("link", {
        name: "Promote into this environment",
      }),
    ).toBeNull();
    expect(
      screen.getByText(/it writes the spec and deploys nothing/),
    ).toBeTruthy();
  });

  it("says what the record does not carry", async () => {
    renderHistory();

    await screen.findByText("Revisions (3)");
    // One muted line, not a banner, and it names the issue rather than
    // implying the attribution exists somewhere on this screen.
    const note = screen.getByText(
      /authorship is recorded by the delivery mode/,
    );
    expect(note.textContent).toContain(
      "neither distinguishes a human from an agent",
    );
    expect(note.textContent).toContain("History carries no repository URL");
  });

  it("reports an empty history as never deployed, not as a lost record", async () => {
    renderHistory({ entries: [] });

    expect(await screen.findByText("No recorded history")).toBeTruthy();
    expect(screen.queryByRole("listitem")).toBeNull();
  });

  it("surfaces a history the server could not read", async () => {
    const transport = createRouterTransport((router) => {
      router.service(DeployService, {
        history: () => {
          throw new ConnectError("no delivery plane", Code.Unimplemented);
        },
        status: () => ({
          phase: "Healthy",
          revision: "",
          cause: "",
          detail: {},
        }),
      });
    });
    renderAt(
      transport,
      "/apps/checkout/production/history",
      "/apps/:project/:env/history",
      <HistoryPage />,
    );

    expect(
      await screen.findByText("Cannot read the recorded history"),
    ).toBeTruthy();
  });
});
