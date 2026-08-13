import { describe, expect, it } from "vitest";

// `?raw` rather than node:fs: Vite already knows how to hand a file to a test
// as a string, and reaching for the filesystem would mean adding @types/node to
// a package whose README says a dependency here is a decision, not a
// convenience.
import css from "./tokens.css?raw";

/**
 * The regression guard for a two-theme token file.
 *
 * A token added to the dark palette and forgotten in the light one does not
 * throw, does not fail a type check and does not break a render test — it
 * inherits the dark value and produces one illegible element on a white page,
 * which is exactly the failure a screenshot review misses. So the file is
 * parsed and the two palettes are compared by name.
 *
 * The dark values are also pinned against docs/design/assets/README.md. The
 * light theme was added on the condition that the shipped design does not move;
 * this is what makes that condition checkable.
 */

/** tokens.css has no nested at-rules, so a flat selector/body split is exact. */
function blocks(source: string): Map<string, string> {
  const withoutComments = source.replace(/\/\*[\s\S]*?\*\//g, "");
  const found = new Map<string, string>();
  for (const match of withoutComments.matchAll(/([^{}]+)\{([^{}]*)\}/g)) {
    const selector = match[1] ?? "";
    found.set(selector.trim().replace(/\s+/g, " "), match[2] ?? "");
  }
  return found;
}

function declarations(body: string): Map<string, string> {
  const out = new Map<string, string>();
  for (const match of body.matchAll(/(--kelson-[a-z0-9-]+)\s*:\s*([^;]+);/g)) {
    out.set(match[1] ?? "", (match[2] ?? "").trim());
  }
  return out;
}

const parsed = blocks(css);
const BASE = ":root";
const DARK = ':root, :root[data-theme="dark"]';
const LIGHT = ':root[data-theme="light"]';

describe("tokens.css", () => {
  it("is the three blocks the theming contract expects", () => {
    expect([...parsed.keys()]).toEqual([BASE, DARK, LIGHT]);
  });

  it("defines the same token set in both palettes", () => {
    const dark = [...declarations(parsed.get(DARK) ?? "").keys()].sort();
    const light = [...declarations(parsed.get(LIGHT) ?? "").keys()].sort();
    expect(dark.length).toBeGreaterThan(30);
    // Named rather than `toEqual(light)` so a failure says which token moved.
    expect(light).toEqual(dark);
  });

  it("keeps colours out of the theme-independent block", () => {
    // Type and layout only. A colour here would be one no theme can redefine.
    const base = parsed.get(BASE) ?? "";
    expect(base).not.toMatch(/#[0-9a-f]{3,8}\b/i);
    expect(base).not.toMatch(/\b(rgba?|hsla?|oklch|color-mix)\(/);
  });

  it("declares color-scheme with each palette", () => {
    expect(parsed.get(DARK)).toMatch(/color-scheme:\s*dark;/);
    expect(parsed.get(LIGHT)).toMatch(/color-scheme:\s*light;/);
  });

  it("still ships the recorded dark palette, value for value", () => {
    const dark = declarations(parsed.get(DARK) ?? "");
    // docs/design/assets/README.md and docs/design/README.md.
    const recorded: Record<string, string> = {
      "--kelson-page": "#0b0f13",
      "--kelson-panel": "#10171f",
      "--kelson-border": "#1d2732",
      "--kelson-hairline": "#18202a",
      "--kelson-text": "#e6ebf0",
      "--kelson-text-2": "#9aa7b4",
      "--kelson-text-3": "#8b98a6",
      "--kelson-muted": "#6f7b89",
      "--kelson-muted-2": "#5d6875",
      "--kelson-green": "#0fa36b",
      "--kelson-synced": "#0fa36b",
      "--kelson-reconciling": "#4ea3ff",
      "--kelson-degraded": "#e0a944",
      "--kelson-failed": "#e2543a",
      "--kelson-suspended": "#6f7b89",
    };
    for (const [name, value] of Object.entries(recorded)) {
      expect(`${name}=${dark.get(name)}`).toBe(`${name}=${value}`);
    }
  });

  it("keeps the brand mark's green out of the theme split", () => {
    // #0FA36B is what docs/design/assets ships; a logo does not change hue
    // because the page went white. Brand-as-*text* is --kelson-accent, and that
    // one does move, because #0FA36B on white fails AA.
    const dark = declarations(parsed.get(DARK) ?? "");
    const light = declarations(parsed.get(LIGHT) ?? "");
    expect(light.get("--kelson-green")).toBe(dark.get("--kelson-green"));
    expect(light.get("--kelson-accent")).not.toBe(dark.get("--kelson-accent"));
  });
});
