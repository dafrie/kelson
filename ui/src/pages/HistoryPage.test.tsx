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
 * (ADR-0028, R2 #225): a revision id of `<generation>-<hash8>`, and the
 * outcome, the digest and the images in fields of their own. Every entry also
 * carries the deprecated `message` the server still fills for older clients,
 * spelled as something the screen must never show — reading it back would be
 * re-introducing the prose parsing the fields removed.
 */

const ENTRIES = [
  {
    revision: "4-b2c3d4e5",
    specHash: `sha256:${"c".repeat(64)}`,
    committedAt: "2026-08-13T10:04:05Z",
    message: "prose the screen must not render",
    author: "",
    outcome: "Healthy",
    digest: `sha256:${"d".repeat(64)}`,
    images: [{ component: "web", image: "ghcr.io/acme/hello:1.4.2" }],
  },
  {
    revision: "3-9f0a1b2c",
    specHash: `sha256:${"b".repeat(64)}`,
    committedAt: "2026-08-12T09:00:00Z",
    message: "prose the screen must not render",
    author: "",
    outcome: "Healthy",
    digest: `sha256:${"e".repeat(64)}`,
    images: [
      { component: "web", image: "ghcr.io/acme/hello:1.4.1" },
      { component: "worker", image: "ghcr.io/acme/worker:1.4.1" },
    ],
  },
  {
    // An entry the controller recorded before it attributed images: the image
    // arrives with no component claimed for it.
    revision: "2-1a2b3c4d",
    specHash: `sha256:${"a".repeat(64)}`,
    committedAt: "2026-08-11T08:00:00Z",
    message: "prose the screen must not render",
    author: "",
    outcome: "Rejected",
    digest: "",
    images: [{ component: "", image: "ghcr.io/acme/hello:1.4.0" }],
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
    // The outcome, the digest and the images are fields, each shown as itself.
    expect(newest.textContent).toContain("recorded healthy");
    expect(newest.textContent).toContain(`artifact sha256:${"d".repeat(12)}`);
    expect(newest.textContent).toContain("ghcr.io/acme/hello:1.4.2");
    // And `message` is not a source for any of it.
    expect(screen.queryByText(/prose the screen must not render/)).toBeNull();
  });

  it("names the component each image belongs to, and claims none when the record does not", async () => {
    renderHistory();

    // Two components, each under its own name: the controller recorded the
    // pair, so the screen does not have to infer one.
    const attributed = await row("3-9f0a1b2c");
    expect(within(attributed).getByText("web")).toBeTruthy();
    expect(within(attributed).getByText("worker")).toBeTruthy();
    expect(attributed.textContent).toContain("ghcr.io/acme/worker:1.4.1");

    // A pre-attribution entry shows the image alone rather than under a
    // fabricated label.
    const unattributed = await row("2-1a2b3c4d");
    expect(unattributed.textContent).toContain("ghcr.io/acme/hello:1.4.0");
    expect(within(unattributed).queryByText("web")).toBeNull();
  });

  it("shows a recorded outcome on every row without claiming it is live", async () => {
    renderHistory({ live: "3-9f0a1b2c", phase: "Healthy" });

    // Every row says what was recorded, including one that ended badly...
    expect((await row("2-1a2b3c4d")).textContent).toContain(
      "recorded rejected",
    );
    // ...but only the live row carries a phase pill, because only that one has
    // an answer for right now.
    await waitFor(() => expect(screen.getAllByText("deployed now")).toHaveLength(1));
    expect(document.querySelectorAll(".k-pill")).toHaveLength(1);
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

    // Every other revision gets no live claim of any kind, even though it
    // carries a recorded outcome of its own — that is a snapshot from when it
    // was captured, not a live answer.
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
