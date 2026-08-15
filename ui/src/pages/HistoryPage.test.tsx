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
 * nothing can answer for, no author (the spine records none yet, #74), no
 * rollback link to the revision already deployed.
 *
 * The fixtures use the shapes the rebuilt delivery spine actually sends
 * (ADR-0028, R2 #225): a revision id of `<generation>-<hash8>`, and a message
 * that carries the outcome, the digest and the images inline
 * (`internal/api`'s `revisionSummary`), because `HistoryEntry` has no fields
 * of its own for any of them yet.
 */

const ENTRIES = [
  {
    revision: "4-b2c3d4e5",
    specHash: `sha256:${"c".repeat(64)}`,
    committedAt: "2026-08-13T10:04:05Z",
    message: "Healthy · serving · sha256:deadbeef · ghcr.io/acme/hello:1.4.2",
    author: "",
  },
  {
    revision: "3-9f0a1b2c",
    specHash: `sha256:${"b".repeat(64)}`,
    committedAt: "2026-08-12T09:00:00Z",
    message: "Healthy · sha256:cafefeed · ghcr.io/acme/hello:1.4.1",
    author: "",
  },
  {
    revision: "2-1a2b3c4d",
    specHash: `sha256:${"a".repeat(64)}`,
    committedAt: "2026-08-11T08:00:00Z",
    message: "Rejected · sha256:aaaabbbb · ghcr.io/acme/hello:1.4.0",
    author: "",
  },
];

interface Stub {
  entries?: typeof ENTRIES;
  /** What Status reports as live; "" is "the server names no revision". */
  live?: string;
  phase?: string;
  /** When set, Status fails with this code instead of answering. */
  statusError?: Code;
}

function transportFor({
  entries = ENTRIES,
  live = "3-9f0a1b2c",
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
    "/projects/checkout/production/history",
    "/projects/:project/:env/history",
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
    expect(items[0]?.textContent).toContain("4-b2c3d4e5");
    expect(items[2]?.textContent).toContain("2-1a2b3c4d");

    const newest = await row("4-b2c3d4e5");
    expect(newest.textContent).toContain("2026-08-13 10:04:05Z");
    expect(newest.textContent).toContain(`spec sha256:${"c".repeat(12)}`);
    // The outcome, digest and images ride in `message` and are shown verbatim.
    expect(newest.textContent).toContain(
      "Healthy · serving · sha256:deadbeef · ghcr.io/acme/hello:1.4.2",
    );
  });

  it("puts the phase pill only on the revision Status reports as live", async () => {
    // The live revision is deliberately not the newest: the marker follows the
    // server's answer, not a position in the list.
    const { container } = renderHistory({
      live: "3-9f0a1b2c",
      phase: "Healthy",
    });

    const live = await row("3-9f0a1b2c");
    await waitFor(() =>
      expect(within(live).getByText("deployed now")).toBeTruthy(),
    );
    expect(within(live).getByText("healthy")).toBeTruthy();

    // Every other revision gets no health claim of any kind, even though its
    // own message happens to carry an outcome word too — that word is a
    // snapshot from when it was captured, not a live answer.
    expect(within(await row("4-b2c3d4e5")).queryByText("deployed now"))
      .toBeNull();
    expect(screen.getAllByText("deployed now")).toHaveLength(1);
    expect(container.querySelectorAll(".k-pill")).toHaveLength(1);
  });

  it("carries an unhealthy live phase through the same pill vocabulary", async () => {
    renderHistory({ live: "4-b2c3d4e5", phase: "Degraded" });

    const live = await row("4-b2c3d4e5");
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

  it("reads authorship honestly: the spine records none yet", async () => {
    renderHistory();

    const unattributed = await screen.findAllByText("unattributed");
    expect(unattributed).toHaveLength(3);
    expect(unattributed[0]?.getAttribute("title")).toContain("#74");
  });

  it("links each revision to the diff against what is deployed now", async () => {
    renderHistory();

    const item = await row("2-1a2b3c4d");
    expect(
      within(item)
        .getByRole("link", { name: "Diff against current" })
        .getAttribute("href"),
    ).toBe("/projects/checkout/production/diff?from=2-1a2b3c4d");
  });

  it("links a rollback that carries the revision, except for the newest", async () => {
    renderHistory();

    const older = await row("3-9f0a1b2c");
    expect(
      within(older)
        .getByRole("link", { name: "Roll back to this" })
        .getAttribute("href"),
    ).toBe("/projects/checkout/production/rollback?to=3-9f0a1b2c");

    // Restoring the newest revision is not a rollback, and the rollback screen
    // disables that target, so the link is not offered at all.
    const newest = await row("4-b2c3d4e5");
    expect(within(newest).queryByRole("link", { name: "Roll back to this" }))
      .toBeNull();
  });

  it("offers promotion once, above the list, and not per revision (#11)", async () => {
    renderHistory();

    const promote = await screen.findByRole("link", {
      name: "Promote into this environment",
    });
    expect(promote.getAttribute("href")).toBe(
      "/projects/checkout/production/promote",
    );
    // It reads the *source* environment's latest revision, so no row offers it:
    // a per-revision button would promise a promotion the RPC does not make.
    const newest = await row("4-b2c3d4e5");
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
      /kelson does not record who deployed yet/,
    );
    expect(note.textContent).toContain("no commit or pull-request link");
  });

  it("reports an empty history as never deployed, not as a lost record", async () => {
    renderHistory({ entries: [] });

    expect(await screen.findByText("No recorded history")).toBeTruthy();
    expect(screen.queryByRole("listitem")).toBeNull();
    // A rollback prepends no history entry of its own (ADR-0028 decision 5),
    // so the empty state must not imply one would appear here.
    expect(
      screen.getByText(/a rollback repoints Flux at a revision that is already here/),
    ).toBeTruthy();
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
      "/projects/checkout/production/history",
      "/projects/:project/:env/history",
      <HistoryPage />,
    );

    expect(
      await screen.findByText("Cannot read the recorded history"),
    ).toBeTruthy();
  });
});
