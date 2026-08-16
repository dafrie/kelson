import { describe, expect, it } from "vitest";

/**
 * No two paths under src/ may differ only by letter case.
 *
 * CI builds on a case-sensitive filesystem, where `ticker.ts` and `Ticker.tsx`
 * are two modules and every import resolves to the one it names. macOS is
 * case-insensitive by default, so there they are one name — and the bundler,
 * asked for `./Ticker`, is free to pick `ticker.ts` and then fail because the
 * component it wanted is not exported by the logic module it got. That is a
 * build that is green in CI and broken on a contributor's laptop, which is the
 * worst distribution of the two outcomes.
 *
 * The fix is never clever resolution — it is not having the collision. The
 * listing comes from the bundler itself (`import.meta.glob`, never loaded, so
 * no Node API and no extra types), covers every extension — a `Ticker.css`
 * beside a `ticker.css` breaks the same way — and folds whole paths, so two
 * *directories* differing only by case are caught too. On a case-insensitive
 * checkout a collision cannot even exist on disk, so the rule necessarily
 * bites where the colliding file gets committed: in CI.
 */
const everything = import.meta.glob("./**");

describe("filenames", () => {
  it("no two paths under src differ only by case", () => {
    const byFold = new Map<string, string[]>();
    for (const path of Object.keys(everything)) {
      const fold = path.toLowerCase();
      byFold.set(fold, [...(byFold.get(fold) ?? []), path]);
    }
    const collisions = [...byFold.values()].filter((group) => group.length > 1);
    expect(collisions, "case-insensitive path collisions").toEqual([]);
  });
});
