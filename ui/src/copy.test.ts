import { describe, expect, it } from "vitest";

/**
 * The interface cites no tracker.
 *
 * The owner's rule for this UI: no ADR references and no issue numbers in
 * anything a user sees — those are working records for the people building
 * kelson, and a screen that cites them is documentation leaking into an
 * interface. The rule was applied once by hand and regressed twice, because
 * nothing enforced it; this does.
 *
 * The scan reads every source file the bundler would ship (tests and
 * generated code excluded), drops block comments and comment-only lines —
 * where citations are welcome and encouraged — and fails on what remains:
 * an ADR number, a parenthesized issue number, or a tracker URL in code is
 * either rendered or one refactor away from it.
 */
const sources = import.meta.glob("./**/*.{ts,tsx}", {
  eager: true,
  query: "?raw",
  import: "default",
}) as Record<string, string>;

const FORBIDDEN = [/ADR-\d{4}/, /\(#\d{1,4}\)/, /issues\/\d+/];

describe("copy", () => {
  it("no ADR or issue citation survives outside comments", () => {
    const offences: string[] = [];
    for (const [path, raw] of Object.entries(sources)) {
      if (path.includes(".test.") || path.startsWith("./gen/")) continue;
      const noBlocks = raw.replace(/\/\*[\s\S]*?\*\//g, "");
      noBlocks.split("\n").forEach((line, i) => {
        const trimmed = line.trim();
        if (trimmed.startsWith("//") || trimmed.startsWith("*")) return;
        for (const pattern of FORBIDDEN) {
          if (pattern.test(trimmed)) {
            offences.push(`${path}:${i + 1}: ${trimmed.slice(0, 100)}`);
          }
        }
      });
    }
    expect(offences, "tracker citations in shippable source").toEqual([]);
  });
});
