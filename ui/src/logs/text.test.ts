import { describe, expect, it } from "vitest";
import { create } from "@bufbuild/protobuf";

import { LogLineSchema } from "../gen/kelson/v1alpha1/logs_pb";
import { isoStamp, logFileName, stamp, toText } from "./text";

function line(ts: number, message: string, pod = "web-6c9") {
  return create(LogLineSchema, {
    timestampUnixMs: BigInt(ts),
    pod,
    container: "web",
    message,
  });
}

const AT = 1_700_000_000_000; // 2023-11-14T22:13:20Z

describe("stamp", () => {
  it("is the wall clock, because a tail is about now", () => {
    expect(stamp(BigInt(AT))).toBe("22:13:20");
  });

  it("says a line had no timestamp rather than showing 1970", () => {
    expect(stamp(0n)).toBe("--:--:--");
  });
});

describe("isoStamp", () => {
  it("is the whole instant, because a saved file outlives the day", () => {
    expect(isoStamp(BigInt(AT))).toBe("2023-11-14T22:13:20.000Z");
  });

  it("writes a dash for a line the engine could not timestamp", () => {
    expect(isoStamp(0n)).toBe("-");
  });
});

describe("toText", () => {
  it("writes one line per line, ending with a newline", () => {
    expect(toText([line(AT, "listening on :8080"), line(0, "unstamped")])).toBe(
      "2023-11-14T22:13:20.000Z web-6c9 web listening on :8080\n" +
        "- web-6c9 web unstamped\n",
    );
  });

  it("writes whatever it is given — the filtered view or the whole buffer", () => {
    const buffer = [line(AT, "a"), line(AT, "b"), line(AT, "c")];
    const filtered = [buffer[1] as (typeof buffer)[number]];
    expect(toText(filtered).split("\n")).toHaveLength(2);
    expect(toText(buffer).split("\n")).toHaveLength(4);
  });

  it("is empty for no lines rather than a stray newline", () => {
    expect(toText([])).toBe("");
  });
});

describe("logFileName", () => {
  it("names the workload and the instant it was saved", () => {
    expect(logFileName("checkout-production", "web", new Date(AT))).toBe(
      "kelson-logs-checkout-production-web-20231114T221320Z.txt",
    );
  });

  // The separator is what makes a name a path, so the separator is what goes.
  it("keeps a name a filesystem will accept", () => {
    expect(logFileName("../etc", "we b/1", new Date(AT))).toBe(
      "kelson-logs-..-etc-we-b-1-20231114T221320Z.txt",
    );
  });

  it("leaves out a part nobody filled in", () => {
    expect(logFileName("checkout-production", "", new Date(AT))).toBe(
      "kelson-logs-checkout-production-20231114T221320Z.txt",
    );
  });
});
