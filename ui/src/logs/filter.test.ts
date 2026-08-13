import { describe, expect, it } from "vitest";
import { create } from "@bufbuild/protobuf";

import { LogLineSchema } from "../gen/kelson/v1alpha1/logs_pb";
import {
  compileFilter,
  POD_TONES,
  podTone,
  podsOf,
  segments,
  visibleLines,
  type Filtering,
} from "./filter";

function line(message: string, pod = "web-1") {
  return create(LogLineSchema, {
    timestampUnixMs: 1n,
    pod,
    container: "web",
    message,
  });
}

function ranges(query: string, isRegex: boolean, text: string) {
  const compiled = compileFilter(query, isRegex);
  if (compiled.kind !== "filter") throw new Error(`not a filter: ${compiled.kind}`);
  return compiled.filter.ranges(text);
}

describe("compileFilter", () => {
  it("treats an empty query as no filter at all", () => {
    expect(compileFilter("", false)).toEqual({ kind: "all" });
    expect(compileFilter("", true)).toEqual({ kind: "all" });
  });

  it("finds every occurrence of a substring, case-insensitively", () => {
    expect(ranges("timeout", false, "TIMEOUT after timeout")).toEqual([
      [0, 7],
      [14, 21],
    ]);
  });

  it("does not overlap a substring with itself", () => {
    expect(ranges("aa", false, "aaaa")).toEqual([
      [0, 2],
      [2, 4],
    ]);
  });

  it("finds nothing when the substring is absent", () => {
    expect(ranges("nope", false, "all is well")).toEqual([]);
  });

  it("matches a regular expression and reports where", () => {
    expect(ranges("c[au]t", true, "cat and cut")).toEqual([
      [0, 3],
      [8, 11],
    ]);
  });

  it("does not treat a substring query as a pattern", () => {
    // `.` is a literal here; a substring filter that quietly compiled as a
    // regex would match everything.
    expect(ranges("a.c", false, "abc")).toEqual([]);
    expect(ranges("a.c", false, "xa.cx")).toEqual([[1, 4]]);
  });

  // A pattern that can match the empty string would spin forever if the walker
  // did not step past a zero-width hit.
  it("terminates on a pattern that can match nothing", () => {
    expect(ranges("x*", true, "axb")).toEqual([[1, 2]]);
  });

  it("reports an unparseable pattern rather than guessing", () => {
    const compiled = compileFilter("[unclosed", true);
    expect(compiled.kind).toBe("invalid");
    if (compiled.kind === "invalid") expect(compiled.message).not.toBe("");
  });
});

describe("segments", () => {
  it("returns the whole line as one plain run when nothing matched", () => {
    expect(segments("hello", [])).toEqual([{ text: "hello", hit: false }]);
  });

  it("splits into plain and matched runs, losing nothing", () => {
    const parts = segments("a timeout here", [[2, 9]]);
    expect(parts).toEqual([
      { text: "a ", hit: false },
      { text: "timeout", hit: true },
      { text: " here", hit: false },
    ]);
    expect(parts.map((p) => p.text).join("")).toBe("a timeout here");
  });

  it("handles a match at each end", () => {
    expect(segments("abc", [[0, 3]])).toEqual([{ text: "abc", hit: true }]);
    expect(segments("abc", [[0, 1]])).toEqual([
      { text: "a", hit: true },
      { text: "bc", hit: false },
    ]);
  });
});

describe("podTone", () => {
  it("is stable for a name and inside the palette", () => {
    for (const pod of ["web-6c9", "web-77f", "worker-2", ""]) {
      const tone = podTone(pod);
      expect(tone).toBe(podTone(pod));
      expect(tone).toBeGreaterThanOrEqual(1);
      expect(tone).toBeLessThanOrEqual(POD_TONES);
    }
  });

  it("spreads a replica set across the palette rather than piling up", () => {
    const pods = Array.from({ length: 24 }, (_, i) => `web-6c9df4b8d5-${i}xkq`);
    expect(new Set(pods.map(podTone)).size).toBeGreaterThan(POD_TONES / 2);
  });
});

describe("podsOf", () => {
  it("lists each pod once, in first-seen order, ignoring unattributed lines", () => {
    expect(
      podsOf([line("a", "web-2"), line("b", "web-1"), line("c", "web-2"), line("d", "")]),
    ).toEqual(["web-2", "web-1"]);
  });
});

describe("visibleLines", () => {
  const all: Filtering = { kind: "all" };
  const lines = [line("first", "web-1"), line("second", "web-2"), line("third", "web-1")];

  it("shows everything when nothing is chosen", () => {
    const visible = visibleLines(lines, all, new Set());
    expect(visible.map((v) => v.line.message)).toEqual(["first", "second", "third"]);
    expect(visible.map((v) => v.index)).toEqual([0, 1, 2]);
  });

  it("narrows to the chosen pods", () => {
    const visible = visibleLines(lines, all, new Set(["web-1"]));
    expect(visible.map((v) => v.line.message)).toEqual(["first", "third"]);
    // The index is the line's place in the buffer, not in the filtered view.
    expect(visible.map((v) => v.index)).toEqual([0, 2]);
  });

  it("drops the lines a query misses and marks where it hit", () => {
    const visible = visibleLines(lines, compileFilter("ir", false), new Set());
    expect(visible.map((v) => v.line.message)).toEqual(["first", "third"]);
    expect(visible[0]?.ranges).toEqual([[1, 3]]);
    expect(visible[1]?.ranges).toEqual([[2, 4]]);
  });

  it("combines the two filters", () => {
    const visible = visibleLines(lines, compileFilter("ir", false), new Set(["web-1"]));
    expect(visible.map((v) => v.line.message)).toEqual(["first", "third"]);
  });

  // Blanking the view while someone types `[` mid-pattern helps nobody: the
  // message says what is wrong and the lines stay on screen.
  it("hides nothing while a pattern is unparseable", () => {
    const visible = visibleLines(lines, compileFilter("[", true), new Set());
    expect(visible).toHaveLength(3);
    expect(visible[0]?.ranges).toEqual([]);
  });
});
