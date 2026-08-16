import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { Ticker } from "./Ticker";
import { entryFor, type TickerEntry } from "./ticker";
import type { WatchState } from "../api/watch";
import { create } from "@bufbuild/protobuf";
import { WatchResponse_EventSchema } from "../gen/kelson/v1alpha1/events_pb";

/**
 * The ticker as it is drawn (#260). `ticker.test.ts` covers what a row *is*;
 * these pin the four rules the strip is held to — absent when empty, records
 * rather than statuses, one coloured word at most, and a stream that says when
 * it is not carrying everything.
 */

/**
 * The strip prints an age, so the browser's clock is pinned: without this every
 * assertion about a row's time would be a different string on the day it ran.
 */
const NOW = 1_760_000_000_000;

beforeEach(() => vi.useFakeTimers({ now: NOW }));
afterEach(() => vi.useRealTimers());

function row(fields: {
  key: string;
  phase: string;
  previousPhase?: string;
  revision?: string;
  project?: string;
  environment?: string;
  agoMs?: number;
}): TickerEntry {
  const made = entryFor(
    create(WatchResponse_EventSchema, {
      cursor: fields.key,
      atUnixMs: BigInt(NOW - (fields.agoMs ?? 0)),
      project: fields.project ?? "checkout",
      environment: fields.environment ?? "production",
      payload: {
        case: "statusTransition",
        value: {
          phase: fields.phase,
          previousPhase: fields.previousPhase ?? "",
          revision: fields.revision ?? "",
        },
      },
    }),
    NOW,
  );
  if (made === undefined) throw new Error("not a transition");
  return made;
}

function draw(entries: TickerEntry[], state: WatchState = "live", props = {}) {
  return render(
    <MemoryRouter>
      <Ticker entries={entries} state={state} {...props} />
    </MemoryRouter>,
  );
}

/** Every row on screen, as the strings a reader sees on it. */
function rowTexts(): string[][] {
  return screen
    .getAllByRole("listitem")
    .map((li) =>
      [...li.children].map((el) => el.textContent ?? "").filter((s) => s !== ""),
    );
}

describe("the ticker's rows", () => {
  it("renders a transition as time, subject, word, phases and revision", () => {
    draw([
      row({
        key: "nonce.1",
        phase: "Reconciling",
        previousPhase: "Committed",
        revision: "45-9e8d7c6b",
        agoMs: 12_000,
      }),
    ]);

    expect(rowTexts()).toEqual([
      [
        "12s",
        "checkout · production",
        "deploying",
        "Committed → Reconciling",
        "45-9e8d7c6b",
      ],
    ]);
  });

  it("puts the newest at the top, in the order it was given", () => {
    draw([
      row({ key: "c", phase: "Healthy", previousPhase: "Applied" }),
      row({ key: "b", phase: "Applied", previousPhase: "Reconciling" }),
      row({ key: "a", phase: "Reconciling", previousPhase: "Committed" }),
    ]);

    expect(rowTexts().map((cells) => cells[3])).toEqual([
      "Applied → Healthy",
      "Reconciling → Applied",
      "Committed → Reconciling",
    ]);
  });

  it("draws no more rows than the limit it was given", () => {
    draw(
      ["e", "d", "c", "b", "a"].map((key) => row({ key, phase: "Healthy" })),
      "live",
      { limit: 2 },
    );

    expect(screen.getAllByRole("listitem")).toHaveLength(2);
  });

  it("drops the subject where the page already names it", () => {
    draw([row({ key: "a", phase: "Healthy" })], "live", { subject: false });

    expect(screen.queryByText("checkout · production")).toBeNull();
    expect(rowTexts()).toEqual([["now", "live", "Healthy"]]);
  });

  it("links the subject to the environment it names", () => {
    draw([row({ key: "a", phase: "Healthy", environment: "staging" })]);

    expect(
      screen.getByRole("link", { name: "checkout · staging" }).getAttribute("href"),
    ).toBe("/projects/checkout/staging");
  });

  it("keeps the machine's values in the mono face and the names out of it", () => {
    const { container } = draw([
      row({
        key: "a",
        phase: "Rejected",
        previousPhase: "Committed",
        revision: "46-0badcafe",
      }),
    ]);

    const mono = [...container.querySelectorAll(".k-mono")].map(
      (el) => el.textContent,
    );
    // The age, the word, the phases and the revision are the machine's. The
    // project and environment are names somebody chose and are not in the list.
    expect(mono).toEqual(["now", "failed", "Committed → Rejected", "46-0badcafe"]);
    expect(container.querySelector(".k-tick__subject")?.className).not.toContain(
      "k-mono",
    );
  });

  it("keeps the wall-clock instant reachable without printing it", () => {
    const { container } = draw([
      row({ key: "a", phase: "Healthy", agoMs: 90_000 }),
    ]);

    const at = container.querySelector(".k-tick__at");
    expect(at?.textContent).toBe("1m");
    expect(at?.getAttribute("title")).toBe(
      new Date(NOW - 90_000).toISOString().replace("T", " ").replace(/\.\d+Z$/, "Z"),
    );
  });
});

describe("the ticker's colour", () => {
  it("tones a settled failure and leaves every other word as ink", () => {
    const { container } = draw([
      row({ key: "a", phase: "Rejected" }),
      row({ key: "b", phase: "Degraded" }),
      row({ key: "c", phase: "Reconciling" }),
      row({ key: "d", phase: "Committed" }),
      row({ key: "e", phase: "Healthy" }),
    ]);

    const tones = [...container.querySelectorAll(".k-tick__word")].map(
      (el) => (el as HTMLElement).dataset.tone,
    );
    expect(tones).toEqual(["failed", "degraded", undefined, undefined, undefined]);
  });

  it("draws no status pill at all", () => {
    // A pill claims "this is the state now". A row four minutes down the strip
    // is a statement about an instant that has passed, and exactly one object
    // on the screen — the pill above — is allowed to make the present-tense
    // claim.
    const { container } = draw([row({ key: "a", phase: "Rejected" })]);
    expect(container.querySelector(".k-pill")).toBeNull();
  });
});

describe("the ticker's absence and its transport", () => {
  it("renders nothing at all when nothing has happened", () => {
    const { container } = draw([]);
    // Not an empty box with a reassuring sentence: the ring starts empty on
    // every load, so the box would be the permanent state of a quiet instance.
    expect(container.innerHTML).toBe("");
  });

  it("draws no transport indicator of its own", () => {
    // Both screens that carry a ticker already have one in their header, and
    // the indicator is deliberately the smallest thing on a page: two of them
    // saying the same word about the same stream would make the transport the
    // loudest object on it.
    draw([row({ key: "a", phase: "Healthy" })]);

    const strip = screen.getByLabelText("Activity");
    expect(within(strip).queryByText("streaming")).toBeNull();
    expect(screen.queryByText("reconnecting — rows may be missing")).toBeNull();
  });

  it("says the strip may be incomplete while the stream is down", () => {
    draw([row({ key: "a", phase: "Healthy" })], "reconnecting");

    // The one thing the header's indicator cannot say: the rows are not merely
    // stale, they are missing some.
    expect(screen.getByText("reconnecting — rows may be missing")).toBeTruthy();
  });

  it("says nothing about a transport that cannot watch", () => {
    draw([row({ key: "a", phase: "Healthy" })], "off");

    expect(screen.queryByText("reconnecting — rows may be missing")).toBeNull();
    // The rows are still real: they arrived before the stream went away.
    expect(screen.getAllByRole("listitem")).toHaveLength(1);
  });

  it("names whose activity it is without letterspacing a name", () => {
    const { container } = draw([row({ key: "a", phase: "Healthy" })], "live", {
      name: "Activity in production",
    });

    expect(screen.getByLabelText("Activity in production")).toBeTruthy();
    // The eyebrow is uppercased and letterspaced by the stylesheet, which is a
    // thing to do to a label and not to a name somebody chose.
    expect(container.querySelector(".k-eyebrow")?.textContent).toBe("Activity");
  });
});
