import { describe, expect, it } from "vitest";

import {
  classifyRecord,
  formatWhen,
  isCommitRevision,
  shortRevision,
  shortSpecHash,
} from "./history";

/**
 * These are the conservative half of the history screen: every function has a
 * "cannot tell" answer, and most of what is asserted here is that it returns
 * that answer rather than a plausible-looking guess.
 */

const COMMIT = "9f2c1a4b7e0d3f65a8b9c0d1e2f3a4b5c6d7e8f9";

describe("isCommitRevision", () => {
  it("accepts a full forty-character hex revision, in either case", () => {
    expect(isCommitRevision(COMMIT)).toBe(true);
    expect(isCommitRevision(COMMIT.toUpperCase())).toBe(true);
  });

  it("rejects anything that is not exactly a commit id", () => {
    // Direct mode's counter is the common case and must never be mistaken for
    // a sha — it is not one, and there is no repository it would name.
    expect(isCommitRevision("rev-00000007")).toBe(false);
    // An abbreviated sha is indistinguishable from any other hex token.
    expect(isCommitRevision(COMMIT.slice(0, 12))).toBe(false);
    // Off-by-one in both directions, and hex that is not.
    expect(isCommitRevision(COMMIT.slice(0, 39))).toBe(false);
    expect(isCommitRevision(`${COMMIT}0`)).toBe(false);
    expect(isCommitRevision("z".repeat(40))).toBe(false);
    expect(isCommitRevision("")).toBe(false);
  });
});

describe("shortRevision", () => {
  it("abbreviates a commit to twelve characters", () => {
    expect(shortRevision(COMMIT)).toBe("9f2c1a4b7e0d");
  });

  it("leaves a revision it cannot identify as a commit untouched", () => {
    expect(shortRevision("rev-00000007")).toBe("rev-00000007");
    // Long, but not a commit: truncating it would invent an abbreviation
    // nothing else in the system uses.
    expect(shortRevision("release-2026-08-13-candidate")).toBe(
      "release-2026-08-13-candidate",
    );
  });
});

describe("shortSpecHash", () => {
  it("keeps the algorithm and abbreviates the digest", () => {
    expect(shortSpecHash(`sha256:${"a1b2c3d4e5f6".repeat(4)}`)).toBe(
      "sha256:a1b2c3d4e5f6",
    );
  });

  it("returns anything it does not recognise as it arrived", () => {
    expect(shortSpecHash("")).toBe("");
    expect(shortSpecHash("sha256:abc")).toBe("sha256:abc");
    expect(shortSpecHash("not-a-hash")).toBe("not-a-hash");
    expect(shortSpecHash("sha256:not-hex-at-all-here")).toBe(
      "sha256:not-hex-at-all-here",
    );
  });
});

describe("classifyRecord", () => {
  it("reads the subjects direct mode writes", () => {
    expect(classifyRecord("deploy sha256:abcdef")).toBe("deploy");
    expect(classifyRecord("rollback to rev-00000003")).toBe("rollback");
  });

  it("reads the subjects the Git modes write", () => {
    // Mode parity is the acceptance criterion: the same two acts, recognised
    // through a different vocabulary.
    expect(classifyRecord("kelson: update checkout/production")).toBe("deploy");
    expect(
      classifyRecord("kelson: rollback checkout/production to 9f2c1a4b7e0d"),
    ).toBe("rollback");
    // The Git writer's fallback subject, used when no subject was supplied.
    expect(classifyRecord("kelson: update rendered manifests")).toBe("deploy");
  });

  it("says unknown for a subject it was not written by kelson to match", () => {
    expect(classifyRecord("")).toBe("unknown");
    // A hand-written commit on the manifests branch.
    expect(classifyRecord("Merge pull request #12 from acme/hotfix")).toBe(
      "unknown",
    );
    // Word-boundary anchored: these start with the right letters and mean
    // nothing of the kind.
    expect(classifyRecord("deployment tuning")).toBe("unknown");
    expect(classifyRecord("rollbacks are documented in the runbook")).toBe(
      "unknown",
    );
    // Not at the start: a subject that merely mentions the word.
    expect(classifyRecord("chore: describe the rollback procedure")).toBe(
      "unknown",
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
