import { describe, expect, it } from "vitest";
import { create } from "@bufbuild/protobuf";

import {
  WatchResponse_EventSchema,
  type WatchResponse_Event,
} from "../gen/kelson/v1alpha1/events_pb";
import { statusForDelivery } from "../components/status";
import {
  entryFor,
  inScope,
  phaseMove,
  pushEntry,
  rowTone,
  sinceLabel,
  TICKER_LIMIT,
  type TickerEntry,
} from "./ring";

/**
 * The ticker's logic (#260), tested as functions rather than through a DOM —
 * `Ticker.test.tsx` covers the drawing.
 *
 * The claim these exist to protect is the one in the module doc: the ticker
 * *displays* transitions and does not re-interpret them. A row's word has to be
 * the word a pill lands on for the same transition, forever, because the two
 * are on screen together and a disagreement between them would be unarguable
 * and invisible.
 */

const RECEIVED = 1_760_000_000_000;

function transition(fields: {
  cursor?: string;
  atUnixMs?: number;
  project?: string;
  environment?: string;
  phase: string;
  previousPhase?: string;
  revision?: string;
  cause?: string;
}): WatchResponse_Event {
  return create(WatchResponse_EventSchema, {
    cursor: fields.cursor ?? "nonce.1",
    atUnixMs: BigInt(fields.atUnixMs ?? RECEIVED),
    project: fields.project ?? "checkout",
    environment: fields.environment ?? "production",
    payload: {
      case: "statusTransition",
      value: {
        phase: fields.phase,
        previousPhase: fields.previousPhase ?? "",
        revision: fields.revision ?? "",
        cause: fields.cause ?? "",
      },
    },
  });
}

function health(): WatchResponse_Event {
  return create(WatchResponse_EventSchema, {
    cursor: "nonce.9",
    atUnixMs: BigInt(RECEIVED),
    project: "checkout",
    environment: "production",
    payload: {
      case: "healthChange",
      value: {
        resource: "Deployment/checkout-production/web",
        code: "crash-loop-back-off",
        healthy: false,
        message: "web is restarting repeatedly",
      },
    },
  });
}

function entry(fields: Partial<TickerEntry> & { key: string }): TickerEntry {
  const made = entryFor(
    transition({ cursor: fields.key, phase: fields.phase ?? "Healthy" }),
    RECEIVED,
  );
  if (made === undefined) throw new Error("not a transition");
  return { ...made, ...fields };
}

describe("entryFor", () => {
  it("takes a row from a status transition", () => {
    const row = entryFor(
      transition({
        cursor: "nonce.4",
        phase: "Reconciling",
        previousPhase: "Committed",
        revision: "45-9e8d7c6b",
      }),
      RECEIVED,
    );

    expect(row).toEqual({
      key: "nonce.4",
      at: RECEIVED,
      project: "checkout",
      environment: "production",
      from: "Committed",
      phase: "Reconciling",
      revision: "45-9e8d7c6b",
      status: { word: "deploying", tone: "reconciling" },
    });
  });

  it("ignores a health change", () => {
    // Deliberate, and the module doc says why: the verdict row and the
    // attention band already move on one, so a ticker row would be the same
    // news twice and the poorer telling of it.
    expect(entryFor(health(), RECEIVED)).toBeUndefined();
  });

  it("says the same word about a transition that the pill does", () => {
    // `deliveryFacts` voids the fetched answer when a transition lands, so a
    // pill on this transition derives from the phase. The ticker must land on
    // exactly that, on every phase the engine has.
    for (const phase of [
      "Proposed",
      "Committed",
      "Reconciling",
      "Applied",
      "Healthy",
      "Degraded",
      "Rejected",
      "SomethingThisBuildHasNeverHeardOf",
    ]) {
      const row = entryFor(transition({ phase }), RECEIVED);
      expect(row?.status).toEqual(statusForDelivery("", phase));
    }
  });

  it("falls back to receipt time when the server stamped nothing", () => {
    const row = entryFor(transition({ phase: "Healthy", atUnixMs: 0 }), RECEIVED);
    expect(row?.at).toBe(RECEIVED);
  });
});

describe("the ring", () => {
  it("puts the newest row first", () => {
    let ring: TickerEntry[] = [];
    ring = pushEntry(ring, entry({ key: "a", phase: "Committed" }));
    ring = pushEntry(ring, entry({ key: "b", phase: "Reconciling" }));
    ring = pushEntry(ring, entry({ key: "c", phase: "Healthy" }));

    expect(ring.map((row) => row.phase)).toEqual([
      "Healthy",
      "Reconciling",
      "Committed",
    ]);
  });

  it("is bounded, and drops the oldest to stay so", () => {
    let ring: TickerEntry[] = [];
    for (let n = 0; n < TICKER_LIMIT + 7; n += 1) {
      ring = pushEntry(ring, entry({ key: `nonce.${n}` }));
    }

    expect(ring).toHaveLength(TICKER_LIMIT);
    expect(ring[0]?.key).toBe(`nonce.${TICKER_LIMIT + 6}`);
    expect(ring[ring.length - 1]?.key).toBe("nonce.7");
  });

  it("never prints one transition twice", () => {
    // A reconnect resumes *after* the last cursor, so this should not fire
    // against kelson-server. It costs one lookup and it means a stream that
    // redelivers cannot double a row.
    const one = entry({ key: "nonce.1" });
    expect(pushEntry(pushEntry([], one), one)).toHaveLength(1);
  });
});

describe("scope", () => {
  const row = entry({ key: "a" });

  it("keeps every pair when there is none", () => {
    expect(inScope(row, undefined)).toBe(true);
  });

  it("keeps only its own pair when there is one", () => {
    expect(inScope(row, { project: "checkout", environment: "production" })).toBe(
      true,
    );
    expect(inScope(row, { project: "checkout", environment: "staging" })).toBe(
      false,
    );
    expect(inScope(row, { project: "hello", environment: "production" })).toBe(
      false,
    );
  });
});

describe("phaseMove", () => {
  it("draws the arrow when the phase moved", () => {
    expect(
      phaseMove(entry({ key: "a", from: "Committed", phase: "Reconciling" })),
    ).toBe("Committed → Reconciling");
  });

  it("claims no movement when only the revision changed", () => {
    // A transition is emitted for a change of phase *or* of revision, so both
    // sides are sometimes the same phase and `Healthy → Healthy` would be a
    // movement that did not happen.
    expect(phaseMove(entry({ key: "a", from: "Healthy", phase: "Healthy" }))).toBe(
      "Healthy",
    );
  });

  it("says the phase alone when the stream sent no previous one", () => {
    expect(phaseMove(entry({ key: "a", from: "", phase: "Applied" }))).toBe(
      "Applied",
    );
  });
});

describe("rowTone", () => {
  it("tones the settled failures and nothing else", () => {
    const toneOf = (phase: string) => {
      const row = entryFor(transition({ phase }), RECEIVED);
      return row === undefined ? "?" : rowTone(row.status);
    };

    expect(toneOf("Rejected")).toBe("failed");
    expect(toneOf("Degraded")).toBe("degraded");
    // Twenty rows of blue `deploying` is twenty coloured objects saying that
    // everything is normal, which is the bargain the Console does not make.
    expect(toneOf("Reconciling")).toBeUndefined();
    expect(toneOf("Committed")).toBeUndefined();
    expect(toneOf("Healthy")).toBeUndefined();
    expect(toneOf("NotAPhase")).toBeUndefined();
  });
});

describe("sinceLabel", () => {
  it("prints the CLI's age vocabulary", () => {
    expect(sinceLabel(12_000)).toBe("12s");
    expect(sinceLabel(4 * 60_000)).toBe("4m");
    expect(sinceLabel(3 * 3_600_000)).toBe("3h");
    expect(sinceLabel(2 * 86_400_000)).toBe("2d");
  });

  it("says `now` for the seconds nobody is counting", () => {
    expect(sinceLabel(0)).toBe("now");
    expect(sinceLabel(4_999)).toBe("now");
  });

  it("clamps a server clock that runs ahead of the browser's", () => {
    // `at_unix_ms` is the server's stamp and `Date.now()` is this machine's, so
    // the elapsed time is computed across two clocks. The floor for "how long
    // ago" is zero; a row must never report a time in the future.
    expect(sinceLabel(-30_000)).toBe("now");
  });
});
