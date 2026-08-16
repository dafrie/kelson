import { describe, expect, it } from "vitest";

import { STATUS_KINDS } from "./StatusPill";
import {
  STATUS_WORDS,
  statusFor,
  statusForAnswer,
  statusForPhase,
  statusForPreview,
  verdictTone,
} from "./status";

/**
 * The vocabulary is only worth having if it is exhaustive and if every screen
 * that asks a question gets the same answer, so that is what is asserted here:
 * the four wire vocabularies land on the same eight words, and nothing lands
 * outside the palette.
 */

describe("the status vocabulary", () => {
  it("paints every word in a tone the stylesheet defines", () => {
    for (const word of STATUS_WORDS) {
      expect(STATUS_KINDS).toContain(statusFor(word).tone);
    }
  });

  it("translates all six of the engine's answers, and nothing else", () => {
    const words = ["waiting", "progressing", "live", "stuck", "rejected", "degraded"].map(
      (a) => statusForAnswer(a).word,
    );
    expect(words).toEqual([
      "waiting",
      "deploying",
      "live",
      "stuck",
      "failed",
      "unhealthy",
    ]);
    expect(statusForAnswer("teleported").word).toBe("unknown");
    expect(statusForAnswer("").word).toBe("unknown");
  });

  it("gives a phase the word its answer would have got", () => {
    // The rail derives an answer from a phase; this map is that derivation
    // composed with the one above, and the two must not drift.
    expect(statusForPhase("Proposed").word).toBe("waiting");
    expect(statusForPhase("Committed").word).toBe("waiting");
    expect(statusForPhase("Reconciling").word).toBe("deploying");
    expect(statusForPhase("Applied").word).toBe("deploying");
    expect(statusForPhase("Healthy").word).toBe("live");
    expect(statusForPhase("Degraded").word).toBe("unhealthy");
    expect(statusForPhase("Rejected").word).toBe("failed");
    expect(statusForPhase("Teleported").word).toBe("unknown");
  });

  it("says the same words about a preview as about an environment", () => {
    expect(statusForPreview("ready", false)).toEqual(statusForPhase("Healthy"));
    expect(statusForPreview("applying", false)).toEqual(statusForPhase("Reconciling"));
    expect(statusForPreview("failed", false)).toEqual(statusForPhase("Rejected"));
  });

  it("keeps suspended out of waiting, because nothing is trying", () => {
    // Waiting means something is expected to act. Suspended means nothing is,
    // deliberately — a different fact, so a different word and a different
    // colour, and it is only ever shown when the wire reported it.
    expect(statusForPreview("ready", true).word).toBe("suspended");
    expect(statusFor("suspended").tone).not.toBe(statusFor("waiting").tone);
    expect(STATUS_WORDS).toContain("suspended");
  });

  it("colours a workload verdict from the same three words", () => {
    expect(verdictTone(true, false)).toBe(statusFor("live").tone);
    expect(verdictTone(false, true)).toBe(statusFor("unhealthy").tone);
    expect(verdictTone(false, false)).toBe(statusFor("failed").tone);
  });
});
