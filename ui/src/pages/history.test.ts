import { describe, expect, it } from "vitest";

import { formatWhen, railPlacements, shortHash } from "./history";

/**
 * These are the conservative half of the history screen: every function has a
 * "cannot tell" answer, and most of what is asserted here is that it returns
 * that answer rather than a plausible-looking guess.
 */

describe("shortHash", () => {
  it("keeps the algorithm and abbreviates the hex", () => {
    expect(shortHash(`sha256:${"a1b2c3d4e5f6".repeat(4)}`)).toBe(
      "sha256:a1b2c3d4e5f6",
    );
  });

  it("returns anything it does not recognise as it arrived", () => {
    expect(shortHash("")).toBe("");
    expect(shortHash("sha256:abc")).toBe("sha256:abc");
    expect(shortHash("not-a-hash")).toBe("not-a-hash");
    expect(shortHash("sha256:not-hex-at-all-here")).toBe(
      "sha256:not-hex-at-all-here",
    );
  });
});

describe("formatWhen", () => {
  it("renders an RFC 3339 instant as UTC wall clock", () => {
    expect(formatWhen("2026-08-13T10:04:05Z")).toBe("2026-08-13 10:04:05Z");
    expect(formatWhen("2026-08-13T12:04:05+02:00")).toBe(
      "2026-08-13 10:04:05Z",
    );
  });

  it("returns nothing for a value that is not a timestamp", () => {
    // committed_at is optional and free-form on the wire; "Invalid Date" on
    // screen would be a claim about when something happened.
    expect(formatWhen("")).toBe("");
    expect(formatWhen("whenever")).toBe("");
  });
});

describe("railPlacements", () => {
  const rail = [
    { revision: "4-d" },
    { revision: "3-c" },
    { revision: "2-b" },
  ];

  it("places every stop relative to the marker", () => {
    expect(railPlacements(rail, "3-c")).toEqual(["above", "live", "below"]);
    expect(railPlacements(rail, "4-d")).toEqual(["live", "below", "below"]);
    expect(railPlacements(rail, "2-b")).toEqual(["above", "above", "live"]);
  });

  it("marks nothing when there is no marker to place stops around", () => {
    // Two different absences with the same drawing: a server that named no
    // revision, and one that named a revision this list has no row for.
    expect(railPlacements(rail, "")).toEqual([
      "unmarked",
      "unmarked",
      "unmarked",
    ]);
    expect(railPlacements(rail, "9-z")).toEqual([
      "unmarked",
      "unmarked",
      "unmarked",
    ]);
  });

  it("fades the revisions only the registry remembers", () => {
    const withTail = [...rail, { revision: "1-a", beyondWindow: true }];
    expect(railPlacements(withTail, "3-c")).toEqual([
      "above",
      "live",
      "below",
      "tail",
    ]);
  });

  it("keeps the marker on a registry-only revision", () => {
    // A rollback can pin an environment to a revision the cluster's bounded
    // history has forgotten, and that is exactly the case this screen is opened
    // for: fading the live stop would hide the marker.
    const withTail = [...rail, { revision: "1-a", beyondWindow: true }];
    expect(railPlacements(withTail, "1-a")).toEqual([
      "above",
      "above",
      "above",
      "live",
    ]);
  });

  it("has nothing to place on an empty rail", () => {
    expect(railPlacements([], "3-c")).toEqual([]);
  });
});
