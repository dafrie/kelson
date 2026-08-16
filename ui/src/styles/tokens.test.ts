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
 *
 * # The contrast pins (#260)
 *
 * The block at the bottom computes WCAG relative luminance and pins the ratios
 * this design depends on. They are not a lint: each one is a decision about how
 * loud a thing is allowed to be, and a failure here means the decision moved.
 * The Console's identity is carried almost entirely by contrast — an ink, a
 * quiet step, five tones and one hairline — so the numbers are the design.
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

const dark = declarations(parsed.get(DARK) ?? "");
const light = declarations(parsed.get(LIGHT) ?? "");
const palettes = { dark, light };

/** WCAG 2.1 relative luminance of a #rrggbb token value. */
function luminance(hex: string): number {
  const channel = (v: number) =>
    v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4;
  const [r, g, b] = [1, 3, 5].map((i) =>
    channel(parseInt(hex.slice(i, i + 2), 16) / 255),
  ) as [number, number, number];
  return 0.2126 * r + 0.7152 * g + 0.0722 * b;
}

/** The WCAG contrast ratio between two tokens, in one palette. */
function contrast(
  theme: keyof typeof palettes,
  foreground: string,
  background: string,
): number {
  const fg = palettes[theme].get(foreground);
  const bg = palettes[theme].get(background);
  if (fg === undefined || bg === undefined) {
    throw new Error(`${theme}: no such token (${foreground} on ${background})`);
  }
  const [lighter, darker] = [luminance(fg), luminance(bg)].sort((a, b) => b - a);
  return ((lighter ?? 0) + 0.05) / ((darker ?? 0) + 0.05);
}

/** Asserts a floor in both themes, naming the pair so a failure reads. */
function atLeast(foreground: string, background: string, floor: number) {
  for (const theme of ["dark", "light"] as const) {
    const ratio = contrast(theme, foreground, background);
    expect(
      `${theme} ${foreground} on ${background} = ${ratio.toFixed(2)}`,
      `${theme}: ${foreground} on ${background} must clear ${floor}:1`,
    ).toBe(
      ratio >= floor
        ? `${theme} ${foreground} on ${background} = ${ratio.toFixed(2)}`
        : `at least ${floor}`,
    );
  }
}

/** Every surface a given foreground can legitimately land on. */
const SURFACES = ["--kelson-page", "--kelson-panel", "--kelson-panel-dim"];

describe("tokens.css", () => {
  it("is the three blocks the theming contract expects", () => {
    expect([...parsed.keys()]).toEqual([BASE, DARK, LIGHT]);
  });

  it("defines the same token set in both palettes", () => {
    const inDark = [...dark.keys()].sort();
    const inLight = [...light.keys()].sort();
    expect(inDark.length).toBeGreaterThan(30);
    // Named rather than `toEqual(light)` so a failure says which token moved.
    expect(inLight).toEqual(inDark);
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
    // because the page went white. The accent is per-theme ink, so it moves.
    expect(light.get("--kelson-green")).toBe(dark.get("--kelson-green"));
    expect(light.get("--kelson-accent")).not.toBe(dark.get("--kelson-accent"));
  });

  it("keeps the accent off the status ramps entirely (#260)", () => {
    // The Console's founding rule: colour means status. The accent used to be
    // --kelson-green, which is also --kelson-synced, so a link and a healthy
    // environment were the same hue and neither could teach the reader
    // anything. The accent is neutral in both themes now, and this is the
    // assertion that stops it drifting back.
    for (const [theme, palette] of Object.entries(palettes)) {
      const accent = palette.get("--kelson-accent");
      for (const tone of [
        "--kelson-green",
        "--kelson-synced",
        "--kelson-reconciling",
        "--kelson-degraded",
        "--kelson-failed",
        "--kelson-suspended",
      ]) {
        expect(`${theme} accent=${accent} ${tone}=${palette.get(tone)}`).not.toBe(
          `${theme} accent=${accent} ${tone}=${accent}`,
        );
      }
      // Neutral in the literal sense: the three channels stay within a hair of
      // each other, so nothing in the chrome can read as a hue.
      const hex = (accent ?? "#000000").slice(1);
      const [r, g, b] = [0, 2, 4].map((i) => parseInt(hex.slice(i, i + 2), 16));
      const spread = Math.max(r ?? 0, g ?? 0, b ?? 0) - Math.min(r ?? 0, g ?? 0, b ?? 0);
      expect(`${theme} accent channel spread ${spread} <= 16`).toBe(
        `${theme} accent channel spread ${spread <= 16 ? spread : "> 16"} <= 16`,
      );
    }
  });
});

/**
 * The contrast pins.
 *
 * Each `it` is one decision. The floors are deliberately different from each
 * other: "you read this" and "you notice this" are different jobs and pinning
 * them at one number would make the pin meaningless.
 */
describe("tokens.css contrast", () => {
  it("puts the ink at AAA on every surface it lands on", () => {
    // The thing a reader actually reads — headings, row names, values that
    // matter — clears 7:1 in both themes. --kelson-accent is the same ink,
    // because a link is text first.
    for (const surface of SURFACES) {
      atLeast("--kelson-text", surface, 7);
      atLeast("--kelson-accent", surface, 7);
    }
    // The primary button inverts them: page-coloured text on an ink fill.
    atLeast("--kelson-page", "--kelson-accent", 7);
  });

  it("keeps the secondary and fact steps at AA", () => {
    // Labels, metas, kv values, the fact under a matrix cell. These are
    // 11.5–12.5px and therefore "normal text" by WCAG's definition, so 4.5:1
    // is the floor, not 3:1.
    for (const surface of SURFACES) {
      atLeast("--kelson-text-2", surface, 4.5);
      atLeast("--kelson-text-3", surface, 4.5);
    }
  });

  it("holds the quiet step at 4:1, which is where the recorded palette puts it", () => {
    // DISCLOSED, and deliberately not fixed here: --kelson-muted is #6f7b89 in
    // dark, which is 4.18:1 on --kelson-panel — under AA's 4.5. It is one of
    // the values pinned to docs/design/assets/README.md above, so raising it is
    // an owner's decision about the recorded design rather than a side effect
    // of a visual-identity pass. What this pass did do is stop using the step
    // *below* it (--kelson-muted-deep, 2.46:1) for text at all, which moved
    // every meta line, gauge label and rail actor up to this number.
    for (const surface of SURFACES) {
      atLeast("--kelson-muted", surface, 4);
    }
  });

  it("holds every tone that is text at AA on a bare surface", () => {
    // THE PIN THIS REDESIGN NEEDED. Status pills used to sit on a tinted fill
    // of their own tone, so the only ratio that mattered was tone-on-own-fill.
    // Live, waiting and deploying badges have no fill now — they sit on the
    // page, on a panel, or on a dim panel — so each tone that appears as *text*
    // has to clear AA against all three.
    for (const surface of SURFACES) {
      atLeast("--kelson-synced", surface, 4.5);
      atLeast("--kelson-reconciling", surface, 4.5);
      atLeast("--kelson-degraded", surface, 4.5);
      atLeast("--kelson-failed", surface, 4.5);
    }
  });

  it("holds suspended to the graphical floor, because it is only ever a dot", () => {
    // --kelson-suspended is 4.18:1 on a dark panel and would fail the rule
    // above. It does not have to pass it: a suspended badge says its word in
    // neutral ink and keeps the tone in its 5px dot, which is a graphical
    // object and clears WCAG 1.4.11's 3:1. The day someone paints text in it,
    // this test is the one that says so.
    for (const surface of SURFACES) {
      atLeast("--kelson-suspended", surface, 3);
    }
  });

  it("keeps the two surfaces that are still allowed to shout legible", () => {
    // The needs-attention band and the error panel are the only filled objects
    // left in the UI, so everything printed on them is pinned: the sentence at
    // AAA, the supporting line and the tone's own eyebrow at AA.
    atLeast("--kelson-text", "--kelson-degraded-fill", 7);
    atLeast("--kelson-text-2", "--kelson-degraded-fill", 4.5);
    atLeast("--kelson-degraded", "--kelson-degraded-fill", 4.5);
    atLeast("--kelson-text", "--kelson-failed-fill", 7);
    atLeast("--kelson-text-2", "--kelson-failed-fill", 4.5);
    atLeast("--kelson-failed", "--kelson-failed-fill", 4.5);
  });

  it("keeps a link's underline visible and quieter than its ink", () => {
    // --kelson-line is what replaced the accent hue as the mark of a link, so
    // it has a floor (you can see it) and a ceiling (a paragraph of links does
    // not become a striped block). Both themes land near 1.7–1.9:1.
    for (const theme of ["dark", "light"] as const) {
      const ratio = contrast(theme, "--kelson-line", "--kelson-panel");
      expect(`${theme} line ${ratio.toFixed(2)} in 1.4..3.0`).toBe(
        `${theme} line ${ratio >= 1.4 && ratio <= 3 ? ratio.toFixed(2) : "out of range"} in 1.4..3.0`,
      );
    }
  });

  it("keeps hairlines below the line, so a rule never reads as a border", () => {
    // Rows are separated by --kelson-hairline and it must stay the quietest
    // mark in the system: quieter than a link's underline, in both themes.
    for (const theme of ["dark", "light"] as const) {
      const hairline = contrast(theme, "--kelson-hairline", "--kelson-panel");
      const line = contrast(theme, "--kelson-line", "--kelson-panel");
      expect(`${theme} hairline ${hairline.toFixed(2)} < line ${line.toFixed(2)}`).toBe(
        `${theme} hairline ${hairline < line ? hairline.toFixed(2) : "too loud"} < line ${line.toFixed(2)}`,
      );
    }
  });
});
