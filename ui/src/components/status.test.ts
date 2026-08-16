import { describe, expect, it } from "vitest";

import { STATUS_KINDS } from "./StatusPill";
import {
  driftFor,
  STATUS_WORDS,
  statusFor,
  statusForAnswer,
  statusForDelivery,
  statusForPhase,
  statusForPreview,
  statusForVerdict,
  verdictTone,
  wordForVerdict,
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

  it("colours a workload verdict from the same four words", () => {
    const v = { healthy: false, degraded: false, stuck: false };
    expect(verdictTone({ ...v, healthy: true })).toBe(statusFor("live").tone);
    expect(verdictTone({ ...v, stuck: true })).toBe(statusFor("stuck").tone);
    expect(verdictTone({ ...v, degraded: true })).toBe(
      statusFor("unhealthy").tone,
    );
    // Behaviour change (#260): all three false is a rollout still in flight,
    // and this used to paint it in the `failed` tone while `statusForVerdict`
    // called the same verdict `deploying` — one screen showed a starting pod in
    // red and another in blue. Both now read the one classification.
    expect(verdictTone(v)).toBe(statusFor("deploying").tone);
  });

  it("keeps stuck apart from degraded and from the wait state", () => {
    // The fourth state, exclusive and in the wire's own order of precedence.
    // `code` stays a wait code on a stuck verdict, so the word is the only
    // thing that separates it from one that is merely still starting.
    const stuck = { healthy: false, degraded: false, stuck: true };
    expect(wordForVerdict(stuck)).toBe("stuck");
    expect(wordForVerdict({ ...stuck, stuck: false })).toBe("deploying");
    expect(wordForVerdict({ healthy: false, degraded: true, stuck: false })).toBe(
      "unhealthy",
    );
    // Healthy defers to the environment; nothing else does.
    const environment = statusFor("deploying");
    expect(
      statusForVerdict({ healthy: true, degraded: false, stuck: false }, environment),
    ).toBe(environment);
    expect(statusForVerdict(stuck, environment).word).toBe("stuck");
  });

  it("reads the engine's answer over the phase, and derives only when there is none", () => {
    // `stuck` is not a phase — a deployment that gave up waiting is wedged in
    // whatever phase it reached — so an answer that disagrees with the phase is
    // the answer that is right.
    expect(statusForDelivery("stuck", "Committed").word).toBe("stuck");
    expect(statusForDelivery("degraded", "Healthy").word).toBe("unhealthy");
    // Empty is the only fallback, and it is what a StatusTransition delivers:
    // a phase, and no answer at all.
    expect(statusForDelivery("", "Reconciling")).toEqual(
      statusForPhase("Reconciling"),
    );
    expect(statusForDelivery("", "").word).toBe("unknown");
    // A token this build cannot read does NOT fall back. The engine said
    // something; deriving a cheerier word from the phase would bury that.
    expect(statusForDelivery("teleported", "Healthy").word).toBe("unknown");
  });
});

/**
 * Drift is a statement about revisions and never about health, so what these
 * pin is that it stays a fact beside the word rather than becoming one.
 */
describe("drift", () => {
  const LIVE_AND_STALE = {
    stale: true,
    revision: "44-1a2b3c4d",
    observedRevision: "44-1a2b3c4d",
    cause: "",
  };

  it("says nothing at all when the environment has not drifted", () => {
    expect(driftFor({ ...LIVE_AND_STALE, stale: false })).toBeUndefined();
  });

  it("names the revision it is serving and says the spec is ahead of it", () => {
    const drift = driftFor(LIVE_AND_STALE);
    expect(drift?.revision).toBe("44-1a2b3c4d");
    expect(drift?.pinned).toBe(false);
    expect(drift?.note).toBe("older than the spec");
    // Not a word, and never one: a stale environment keeps whatever the engine
    // answered, which for this one is `live`.
    expect(STATUS_WORDS).not.toContain(drift?.note);
  });

  it("reads a rollback pin as deliberate rather than as neglect", () => {
    // `cause` is statemachine.Cause.String() — component, reason and message
    // joined with ": " — and RollbackPinned is the controller's own reason for
    // an environment that is serving an older revision on purpose.
    const drift = driftFor({
      ...LIVE_AND_STALE,
      cause:
        "kelson: RollbackPinned: pinned to revision 44-1a2b3c4d: re-rendering is suspended while " +
        "kelson.dev/rollback-to is set, so the current spec cannot be republished over it.",
    });
    expect(drift?.pinned).toBe(true);
    expect(drift?.note).toBe("pinned to an older revision");
  });

  it("does not read a pin out of prose that merely mentions one", () => {
    // Reasons are matched as whole segments. A message is the server's own
    // sentence and is never matched on.
    const drift = driftFor({
      ...LIVE_AND_STALE,
      cause: "kelson: Reconciling: the last RollbackPinned annotation was removed",
    });
    expect(drift?.pinned).toBe(false);
  });

  it("prefers the revision observed serving to the recorded one", () => {
    // The two agree on this spine; reading observed_revision first keeps the
    // seam open for a source that can tell them apart.
    expect(
      driftFor({ ...LIVE_AND_STALE, observedRevision: "43-0f0e0d0c" })?.revision,
    ).toBe("43-0f0e0d0c");
    expect(
      driftFor({ ...LIVE_AND_STALE, observedRevision: "" })?.revision,
    ).toBe("44-1a2b3c4d");
  });
});
