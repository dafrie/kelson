import { describe, expect, it } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { Code, ConnectError, createRouterTransport } from "@connectrpc/connect";
import type { Transport } from "@connectrpc/connect";

import { DeployService } from "../gen/kelson/v1alpha1/deploy_pb";
import { SpecService } from "../gen/kelson/v1alpha1/spec_pb";
import { renderRoutes } from "../test/render";
import { environmentRoutes } from "../test/routes";

/**
 * The screen's contract is that it shows what History returned and nothing
 * more, so most of these tests assert an absence: no phase pill on a revision
 * nothing can answer for, no author (the spine records none yet, #74), no
 * rollback link to the newest revision.
 *
 * Since #260's rail the placement of each stop is asserted too, off `data-rail`
 * — the marker's position on the rail is the screen's main statement and it
 * would otherwise be untested drawing — and the per-stop actions are asserted
 * after a click, because they are collapsed until the stop is opened.
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
  /** The environment has drifted, with this `cause` on the answer. */
  stale?: boolean;
  cause?: string;
  /**
   * The project's environments. The rail's head offers the promotion only when
   * there is another one to promote from, so a one-environment project is a
   * fixture in its own right.
   */
  environments?: string[];
}

function transportFor({
  entries = ENTRIES,
  live = "3-9f0a1b2c",
  phase = "Healthy",
  statusError,
  stale = false,
  cause = "",
  environments = ["staging", "production"],
}: Stub): Transport {
  return createRouterTransport((router) => {
    router.service(SpecService, {
      getSpec: () => ({
        spec: {
          project: "checkout",
          version: "7",
          environments,
          documents: { environments: {} },
        },
      }),
    });
    router.service(DeployService, {
      history: () => ({ entries }),
      status: () => {
        if (statusError !== undefined) {
          throw new ConnectError("no delivery plane", statusError);
        }
        return { phase, revision: live, cause, stale, detail: {} };
      },
    });
  });
}

/**
 * The History tab, mounted under the environment layout it is a tab of (#260).
 * The tab reads the layout's spec for the promotion's possible sources, so a
 * test of the tab alone would be testing a screen the app does not ship.
 */
function renderHistory(stub: Stub = {}, search = "") {
  return renderRoutes(
    transportFor(stub),
    `/projects/checkout/production/history${search}`,
    environmentRoutes(),
  );
}

/** The <li> for one revision, found by the revision id it renders. */
async function row(text: string): Promise<HTMLElement> {
  const node = await screen.findByText(text);
  const item = node.closest("li");
  if (item === null) throw new Error(`no rail stop for ${text}`);
  return item;
}

/** Open one stop's offer, the way a reader does. */
async function open(revision: string): Promise<HTMLElement> {
  fireEvent.click(
    await screen.findByRole("button", {
      name: `Actions for revision ${revision}`,
    }),
  );
  return row(revision);
}

/** Every stop's placement on the rail, top to bottom. */
function rail(): (string | null)[] {
  return Array.from(document.querySelectorAll(".k-history__rail > li")).map(
    (li) => li.getAttribute("data-rail"),
  );
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

  it("shows a recorded outcome on every stop without claiming it is live", async () => {
    renderHistory({ live: "3-9f0a1b2c", phase: "Healthy" });

    // Every stop says what was recorded, including one that ended badly...
    expect((await row("2-1a2b3c4d")).textContent).toContain(
      "recorded rejected",
    );
    // ...but only the marker carries a phase pill, because only that one has
    // an answer for right now.
    await waitFor(() =>
      expect(screen.getAllByText("deployed now")).toHaveLength(1),
    );
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
    expect(within(live).getByText("live")).toBeTruthy();

    // Every other revision gets no live claim of any kind, even though it
    // carries a recorded outcome of its own — that is a snapshot from when it
    // was captured, not a live answer.
    expect(
      within(await row("4-b2c3d4e5")).queryByText("deployed now"),
    ).toBeNull();
    expect(screen.getAllByText("deployed now")).toHaveLength(1);
    expect(container.querySelectorAll(".k-pill")).toHaveLength(1);
  });

  it("carries an unhealthy live phase through the same pill vocabulary", async () => {
    renderHistory({ live: "4-b2c3d4e5", phase: "Degraded" });

    const live = await row("4-b2c3d4e5");
    await waitFor(() =>
      expect(within(live).getByText("unhealthy")).toBeTruthy(),
    );
    expect(
      within(live)
        .getByText("unhealthy")
        .closest(".k-pill")
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
    // And no stop is placed relative to a marker that is not there.
    expect(rail()).toEqual(["unmarked", "unmarked", "unmarked"]);
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

  it("says what the record does not carry", async () => {
    renderHistory();

    await screen.findByText("Revisions (3)");
    // One muted line, not a banner, and it names the issue rather than
    // implying the attribution exists somewhere on this screen.
    const note = screen.getByText(/kelson does not record who deployed yet/);
    expect(note.textContent).toContain("no commit or pull-request link");
  });

  it("reports an empty history as never deployed, not as a lost record", async () => {
    renderHistory({ entries: [] });

    expect(await screen.findByText("No deploys yet")).toBeTruthy();
    expect(screen.queryByRole("listitem")).toBeNull();
    // A rollback prepends no history entry of its own (ADR-0028 decision 5),
    // so the empty state must not imply one would appear here.
    expect(
      screen.getByText(/repoints Flux at a revision that is already here/),
    ).toBeTruthy();
  });

  it("surfaces a history the server could not read", async () => {
    const transport = createRouterTransport((router) => {
      router.service(SpecService, {
        getSpec: () => ({
          spec: {
            project: "checkout",
            version: "7",
            environments: ["production"],
            documents: { environments: {} },
          },
        }),
      });
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
    renderRoutes(
      transport,
      "/projects/checkout/production/history",
      environmentRoutes(),
    );

    expect(
      await screen.findByText("Cannot read the recorded history"),
    ).toBeTruthy();
  });

  describe("the rail", () => {
    it("places the marker where Status put it and every other stop around it", async () => {
      renderHistory({ live: "3-9f0a1b2c" });

      // The marker is the middle stop, so one revision sits above it and one
      // below. Position is the screen's statement and this is the assertion of
      // it.
      await waitFor(() => expect(rail()).toEqual(["above", "live", "below"]));
      expect((await row("3-9f0a1b2c")).getAttribute("data-rail")).toBe("live");
    });

    it("puts the marker at the top when the newest revision is the live one", async () => {
      renderHistory({ live: "4-b2c3d4e5" });

      await waitFor(() => expect(rail()).toEqual(["live", "below", "below"]));
    });

    it("sits the marker below the top when the environment has drifted, and says why once", async () => {
      // The case the drift field exists to make visible: healthy, and not on
      // the revision the spec asks for, because a rollback pinned it.
      renderHistory({
        live: "2-1a2b3c4d",
        stale: true,
        cause: "RollbackPinned: restored 2-1a2b3c4d",
      });

      await waitFor(() => expect(rail()).toEqual(["above", "above", "live"]));

      // The sentence is the drift mark's, on the marker's own stop, and there
      // is exactly one of it — the rail shows the position, the mark says what
      // the position means, and neither repeats the other.
      const live = await row("2-1a2b3c4d");
      expect(
        within(live).getByText("pinned to an older revision"),
      ).toBeTruthy();
      expect(document.querySelectorAll(".k-drift")).toHaveLength(1);
      // No count: "two revisions behind" is a claim about the spec's
      // generation and nothing on the wire carries one.
      expect(live.textContent).not.toMatch(/\d+ revisions? behind/);
    });

    it("fades the registry-only revisions into the rail's tail, and keeps the marker out of it", async () => {
      const aged = {
        revision: "1-0badc0de",
        specHash: "",
        committedAt: "",
        message: "prose the screen must not render",
        author: "",
        outcome: "",
        digest: "",
        images: [],
        beyondWindow: true,
      };
      renderHistory({
        entries: [...ENTRIES, aged] as typeof ENTRIES,
        live: "3-9f0a1b2c",
      });

      await waitFor(() =>
        expect(rail()).toEqual(["above", "live", "below", "tail"]),
      );

      const tail = await row("1-0badc0de");
      expect(within(tail).getByText("registry only")).toBeTruthy();
      expect(tail.textContent).toContain("only the registry remembers");
      // "unattributed" says the record has an author-shaped hole in it. This
      // record has nothing in it at all, and saying both would be saying the
      // wrong one.
      expect(within(tail).queryByText("unattributed")).toBeNull();
    });

    it("keeps the marker on a registry-only revision a rollback pinned it to", async () => {
      // The marker wins over the tail: fading the one stop that is live would
      // hide the marker on the rail it is the point of.
      renderHistory({
        entries: [
          ...ENTRIES,
          {
            revision: "1-0badc0de",
            specHash: "",
            committedAt: "",
            message: "prose the screen must not render",
            author: "",
            outcome: "",
            digest: "",
            images: [],
            beyondWindow: true,
          },
        ] as typeof ENTRIES,
        live: "1-0badc0de",
      });

      await waitFor(() =>
        expect(rail()).toEqual(["above", "above", "above", "live"]),
      );
      expect(
        within(await row("1-0badc0de")).getByText("deployed now"),
      ).toBeTruthy();
    });
  });

  describe("opening a stop", () => {
    it("offers nothing until a stop is opened", async () => {
      renderHistory();

      await screen.findByText("Revisions (3)");
      // Two standing buttons per row is twenty-four controls on a twelve
      // revision environment; the rail is quiet until a stop is pressed.
      expect(
        screen.queryByRole("link", { name: "Diff against current" }),
      ).toBeNull();
      expect(
        screen.queryByRole("link", { name: "Roll back to this" }),
      ).toBeNull();
    });

    it("offers the comparison and the rollback, each carrying the revision", async () => {
      renderHistory();
      const older = await open("3-9f0a1b2c");

      // The comparison is a panel of this tab (#260) and not a screen of its
      // own, but it is still a URL: `?from=` on this same path is what the
      // retired diff route redirects into, so a link out of a pull request and
      // a press on this stop land on the same thing.
      expect(
        within(older)
          .getByRole("link", { name: "Diff against current" })
          .getAttribute("href"),
      ).toBe("/projects/checkout/production/history?from=3-9f0a1b2c");
      // Rollback stays whole: the stop carries its revision to the flow's own
      // screen, which previews before it applies.
      expect(
        within(older)
          .getByRole("link", { name: "Roll back to this" })
          .getAttribute("href"),
      ).toBe("/projects/checkout/production/actions/rollback?to=3-9f0a1b2c");
    });

    it("opens the comparison panel when the stop's link is followed", async () => {
      const { router } = renderHistory();
      const older = await open("3-9f0a1b2c");

      fireEvent.click(
        within(older).getByRole("link", { name: "Diff against current" }),
      );
      await waitFor(() =>
        expect(router.state.location.search).toBe("?from=3-9f0a1b2c"),
      );
      expect(
        await screen.findByRole("button", { name: "Against deployed revision" }),
      ).toBeTruthy();
    });

    it("keeps one stop open at a time", async () => {
      renderHistory();
      await open("3-9f0a1b2c");
      const other = await open("2-1a2b3c4d");

      expect(
        within(other).getByRole("link", { name: "Roll back to this" }),
      ).toBeTruthy();
      expect(
        within(await row("3-9f0a1b2c")).queryByRole("link", {
          name: "Roll back to this",
        }),
      ).toBeNull();
    });

    it("closes a stop that is pressed again", async () => {
      renderHistory();
      await open("3-9f0a1b2c");
      const again = await open("3-9f0a1b2c");

      expect(
        within(again).queryByRole("link", { name: "Diff against current" }),
      ).toBeNull();
      expect(
        within(again)
          .getByRole("button", { name: "Actions for revision 3-9f0a1b2c" })
          .getAttribute("aria-expanded"),
      ).toBe("false");
    });

    it("offers no rollback to the newest revision, and says why", async () => {
      renderHistory();
      const newest = await open("4-b2c3d4e5");

      // The rollback screen disables its newest entry, so the offer does not
      // carry a target that would arrive disabled.
      expect(
        within(newest).queryByRole("link", { name: "Roll back to this" }),
      ).toBeNull();
      expect(newest.textContent).toContain(
        "The newest revision is not a rollback target.",
      );
      // The comparison is still offered: it is a question about the record,
      // not an action on it.
      expect(
        within(newest).getByRole("link", { name: "Diff against current" }),
      ).toBeTruthy();
    });

    it("offers a registry-only revision as a rollback target anyway", async () => {
      renderHistory({
        entries: [
          ...ENTRIES,
          {
            revision: "1-0badc0de",
            specHash: "",
            committedAt: "",
            message: "prose the screen must not render",
            author: "",
            outcome: "",
            digest: "",
            images: [],
            beyondWindow: true,
          },
        ] as typeof ENTRIES,
      });

      // The artifact is immutable, so the restore is exact even though nothing
      // else about the revision was recorded — which is the reason for listing
      // it at all.
      const tail = await open("1-0badc0de");
      expect(
        within(tail)
          .getByRole("link", { name: "Roll back to this" })
          .getAttribute("href"),
      ).toContain("to=1-0badc0de");
    });
  });

  describe("the rail's head", () => {
    it("offers the promotion above the newest revision", async () => {
      renderHistory();

      // Nothing stands there until it is opened, like every other stop.
      const head = await screen.findByRole("button", {
        name: "what comes next",
      });
      expect(
        within(head.parentElement as HTMLElement).queryByRole("link", {
          name: "Promote into this environment",
        }),
      ).toBeNull();

      fireEvent.click(head);
      const stop = head.closest(".k-history__stop") as HTMLElement;
      expect(
        within(stop)
          .getByRole("link", { name: "Promote into this environment" })
          .getAttribute("href"),
      ).toBe("/projects/checkout/production/actions/promote");
      // Promotion writes pins and publishes nothing, so the head says what
      // still has to happen for a revision to arrive there.
      expect(stop.textContent).toContain("A deploy publishes them.");
    });

    it("draws no head when the project declares nowhere to promote from", async () => {
      renderHistory({ environments: ["production"] });

      await screen.findByText("Revisions (3)");
      // The same condition the environment's action bar uses: an offer that
      // could only open an empty picker is not made.
      expect(
        screen.queryByRole("button", { name: "what comes next" }),
      ).toBeNull();
      expect(document.querySelector(".k-history__stop--head")).toBeNull();
    });

    it("offers no promotion on a stop: a revision here is not a source", async () => {
      // Promotion reads the *source* environment's latest revision, which no
      // stop on this rail knows. It belongs at the head, where it promises
      // nothing the RPC does not do.
      renderHistory();
      const newest = await open("4-b2c3d4e5");
      expect(
        within(newest).queryByRole("link", {
          name: "Promote into this environment",
        }),
      ).toBeNull();
    });
  });

  describe("the comparison against the cluster", () => {
    it("opens in place when a revision is named in the URL", async () => {
      renderHistory({}, "?from=3-9f0a1b2c");

      // The panel opens on the mode the revision belongs to, and the record it
      // was opened from is still on screen underneath it.
      expect(
        (
          await screen.findByRole("button", {
            name: "Against deployed revision",
          })
        ).getAttribute("aria-current"),
      ).toBe("true");
      expect(screen.getByText("Revisions (3)")).toBeTruthy();
    });

    it("is offered once, above the rail", async () => {
      const { router } = renderHistory();

      // The mode that is about no particular revision: it is offered once,
      // above the stops, which is what the bare diff route used to be.
      fireEvent.click(
        await screen.findByRole("button", { name: "Diff against the cluster" }),
      );
      await waitFor(() =>
        expect(router.state.location.search).toBe("?compare=1"),
      );
      expect(
        screen.getByRole("button", { name: "Against live cluster" }),
      ).toBeTruthy();
    });
  });
});
