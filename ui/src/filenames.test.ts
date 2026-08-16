import { readdirSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";

/**
 * No two files may differ only by letter case.
 *
 * CI builds on a case-sensitive filesystem, where `ticker.ts` and `Ticker.tsx`
 * are two modules and every import resolves to the one it names. macOS is
 * case-insensitive by default, so there they are one name — and the bundler,
 * asked for `./Ticker`, is free to pick `ticker.ts` and then fail because the
 * component it wanted is not exported by the logic module it got. That is a
 * build that is green in CI and broken on a contributor's laptop, which is the
 * worst distribution of the two outcomes.
 *
 * The fix is never clever resolution — it is not having the collision. This
 * walks all of src/ (every extension: a Ticker.css beside a ticker.css breaks
 * the same way) and names the offending pair, so the failure explains itself.
 */
describe("filenames", () => {
  it("no two entries in one directory differ only by case", () => {
    const collisions: string[] = [];
    const walk = (dir: string) => {
      const entries = readdirSync(dir, { withFileTypes: true });
      const byFold = new Map<string, string[]>();
      for (const entry of entries) {
        const fold = entry.name.toLowerCase();
        byFold.set(fold, [...(byFold.get(fold) ?? []), entry.name]);
      }
      for (const names of byFold.values()) {
        if (names.length > 1) {
          collisions.push(`${dir}: ${names.join(" vs ")}`);
        }
      }
      for (const entry of entries) {
        if (entry.isDirectory()) walk(join(dir, entry.name));
      }
    };
    walk(join(__dirname));
    expect(collisions, "case-insensitive filename collisions").toEqual([]);
  });
});
