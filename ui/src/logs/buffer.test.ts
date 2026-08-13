import { describe, expect, it } from "vitest";
import { create } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";

import { LogLineSchema } from "../gen/kelson/v1alpha1/logs_pb";
import {
  backoffMs,
  dedupKey,
  GapGate,
  isTerminal,
  lastSeenUnixMs,
  LogBuffer,
} from "./buffer";

function line(ts: number, message: string, pod = "web-1", container = "web") {
  return create(LogLineSchema, {
    timestampUnixMs: BigInt(ts),
    pod,
    container,
    message,
  });
}

const messages = (lines: readonly { message: string }[]) =>
  lines.map((l) => l.message);

describe("LogBuffer", () => {
  it("keeps the newest cap lines and says how many it lost", () => {
    const buf = new LogBuffer(3);
    buf.append([line(1, "a"), line(2, "b")]);
    expect(messages(buf.lines)).toEqual(["a", "b"]);
    expect(buf.evicted).toBe(0);

    expect(buf.append([line(3, "c"), line(4, "d")])).toBe(1);
    expect(messages(buf.lines)).toEqual(["b", "c", "d"]);
    expect(buf.length).toBe(3);
    expect(buf.evicted).toBe(1);
  });

  it("evicts one at a time on the per-line path too", () => {
    const buf = new LogBuffer(2);
    for (const [ts, message] of [
      [1, "a"],
      [2, "b"],
      [3, "c"],
      [4, "d"],
    ] as const) {
      buf.push(line(ts, message));
    }
    expect(messages(buf.lines)).toEqual(["c", "d"]);
    expect(buf.evicted).toBe(2);
  });

  // The point of the cap is constant memory. A batch bigger than the whole
  // window must not be materialised first and trimmed afterwards.
  it("takes only the tail of a batch larger than the cap", () => {
    const buf = new LogBuffer(2);
    const batch = Array.from({ length: 1_000 }, (_, i) => line(i, `l${i}`));
    expect(buf.append(batch)).toBe(998);
    expect(messages(buf.lines)).toEqual(["l998", "l999"]);
    expect(buf.length).toBe(2);
    expect(buf.evicted).toBe(998);
  });

  it("stays exactly at the cap however many lines pass through it", () => {
    const buf = new LogBuffer(100);
    for (let i = 0; i < 10_000; i += 1) buf.push(line(i, `l${i}`));
    expect(buf.length).toBe(100);
    expect(buf.lines).toHaveLength(100);
    expect(buf.evicted).toBe(9_900);
    expect(buf.lines[0]?.message).toBe("l9900");
    expect(buf.lines[99]?.message).toBe("l9999");
  });

  it("drains to empty and forgets what it evicted", () => {
    const buf = new LogBuffer(2);
    buf.append([line(1, "a"), line(2, "b"), line(3, "c")]);
    expect(messages(buf.drain())).toEqual(["b", "c"]);
    expect(buf.length).toBe(0);
    expect(buf.evicted).toBe(0);
    expect(buf.lines).toEqual([]);
  });

  it("refuses a cap that cannot hold a line", () => {
    expect(() => new LogBuffer(0)).toThrow();
  });
});

describe("lastSeenUnixMs", () => {
  it("is the newest instant, not the last line's", () => {
    expect(lastSeenUnixMs([line(30, "a"), line(10, "b")])).toBe(30n);
  });

  // 0 is "the engine could not parse a timestamp", not 1970 — such a line
  // cannot be a resume point.
  it("ignores lines with no timestamp", () => {
    expect(lastSeenUnixMs([line(5, "a"), line(0, "b")])).toBe(5n);
    expect(lastSeenUnixMs([line(0, "b")])).toBe(0n);
    expect(lastSeenUnixMs([])).toBe(0n);
  });
});

describe("dedupKey", () => {
  it("separates the fields with a byte a log line cannot contain", () => {
    expect(dedupKey(line(1, "a b", "pod", "c"))).toBe(
      ["1", "pod", "c", "a b"].join("\u0000"),
    );
  });

  it("tells apart lines that differ only in pod or in instant", () => {
    expect(dedupKey(line(1, "same", "a"))).not.toBe(dedupKey(line(1, "same", "b")));
    expect(dedupKey(line(1, "same"))).not.toBe(dedupKey(line(2, "same")));
  });
});

describe("GapGate", () => {
  // The reconnect story: three lines on screen, the connection drops after the
  // second, and both the backfill query and the new follow replay from the last
  // instant seen. What comes back overlaps; what reaches the screen must not.
  it("suppresses the replayed overlap and admits what was missed", () => {
    const retained = [line(10, "a"), line(20, "b")];
    const since = lastSeenUnixMs(retained);
    const gate = new GapGate(retained, since);

    const backfill = [line(20, "b"), line(21, "c"), line(22, "d")];
    expect(messages(gate.admitAll(backfill))).toEqual(["c", "d"]);
  });

  it("closes once it has nothing left to suppress", () => {
    const gate = new GapGate([line(10, "a"), line(20, "b")], 20n);
    expect(gate.open).toBe(true);
    expect(gate.admit(line(20, "b"))).toBe(false);
    expect(gate.open).toBe(false);
    // A later line identical to one it already matched is a genuine repeat.
    expect(gate.admit(line(20, "b"))).toBe(true);
  });

  it("only holds the window at or after the resume point", () => {
    // The older identical line is outside the window, so a replay of it is not
    // something this gate claims to have seen.
    const gate = new GapGate([line(5, "old"), line(20, "new")], 20n);
    expect(gate.admit(line(5, "old"))).toBe(true);
    expect(gate.admit(line(20, "new"))).toBe(false);
  });

  it("counts repeats rather than collapsing them", () => {
    const retained = [line(20, "same"), line(20, "same")];
    const gate = new GapGate(retained, 20n);
    expect(gate.admit(line(20, "same"))).toBe(false);
    expect(gate.admit(line(20, "same"))).toBe(false);
    // A third copy is one the reader has not seen.
    expect(gate.admit(line(20, "same"))).toBe(true);
  });

  // A line the engine could not timestamp cannot be placed in time, but it is
  // still in the window by arrival, so a replay of it is caught.
  it("keys untimestamped lines that arrived inside the window", () => {
    const retained = [line(20, "b"), line(0, "no stamp")];
    const gate = new GapGate(retained, 20n);
    expect(gate.admit(line(0, "no stamp"))).toBe(false);
    expect(gate.admit(line(0, "another"))).toBe(true);
  });

  it("admits everything when nothing was retained", () => {
    const gate = new GapGate([], 0n);
    expect(gate.open).toBe(false);
    expect(messages(gate.admitAll([line(1, "a")]))).toEqual(["a"]);
  });
});

describe("isTerminal", () => {
  it("keeps retrying what a second identical request could answer", () => {
    expect(isTerminal(new ConnectError("gone", Code.Unavailable))).toBe(false);
    expect(isTerminal(new ConnectError("boom", Code.Internal))).toBe(false);
    // A stream that simply ended is not an error at all.
    expect(isTerminal(new Error("connection reset"))).toBe(false);
  });

  it("stops on the answers that will not change", () => {
    expect(isTerminal(new ConnectError("no plane", Code.Unimplemented))).toBe(true);
    expect(isTerminal(new ConnectError("bad selector", Code.InvalidArgument))).toBe(
      true,
    );
    expect(isTerminal(new ConnectError("no session", Code.Unauthenticated))).toBe(
      true,
    );
  });
});

describe("backoffMs", () => {
  it("doubles and then holds at the cap", () => {
    expect(backoffMs(0)).toBe(1_000);
    expect(backoffMs(1)).toBe(2_000);
    expect(backoffMs(4)).toBe(16_000);
    expect(backoffMs(20)).toBe(30_000);
  });
});
