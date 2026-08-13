import { useCallback, useEffect, useState } from "react";

/**
 * Theme selection.
 *
 * Three states, not two. "light" and "dark" are choices the user made and they
 * persist; the third is *no choice*, which follows the operating system and
 * keeps following it — a machine that flips to dark at sunset flips this UI
 * with it, without a reload. Absence of the key in localStorage is what "no
 * choice" means, so a fresh browser and a browser that was reset to system
 * behave identically and there is no third value to keep in sync.
 *
 * Dark is where an unknown system falls: it is the shipped design
 * (docs/design/README.md), and `prefers-color-scheme` matches neither branch
 * when the user has expressed nothing at all.
 *
 * The token file (src/styles/tokens.css) reads `data-theme` on <html>, which
 * must already be right at first paint — index.html sets it inline before the
 * bundle loads, and this module owns it from then on.
 */

export type ThemeChoice = "light" | "dark" | "system";
export type Theme = "light" | "dark";

export const THEME_STORAGE_KEY = "kelson-theme";
export const LIGHT_QUERY = "(prefers-color-scheme: light)";

/** light -> dark -> system -> light. Same order as the inline script's states. */
const NEXT: Record<ThemeChoice, ThemeChoice> = {
  light: "dark",
  dark: "system",
  system: "light",
};

/** Kept in step with the values the inline script in index.html writes. */
const THEME_COLOR: Record<Theme, string> = {
  dark: "#0d1218",
  light: "#fbfbfa",
};

export function nextChoice(choice: ThemeChoice): ThemeChoice {
  return NEXT[choice];
}

/**
 * Read the persisted choice. Anything that is not one of the two explicit
 * values — absent, corrupted, written by an older build — is "system", so a bad
 * value degrades to the default rather than to a broken page. Storage can throw
 * outright (Safari private browsing, a locked-down profile); that is the same
 * answer.
 */
export function readStoredChoice(): ThemeChoice {
  try {
    const raw = window.localStorage.getItem(THEME_STORAGE_KEY);
    return raw === "light" || raw === "dark" ? raw : "system";
  } catch {
    return "system";
  }
}

function storeChoice(choice: ThemeChoice): void {
  try {
    if (choice === "system") {
      window.localStorage.removeItem(THEME_STORAGE_KEY);
    } else {
      window.localStorage.setItem(THEME_STORAGE_KEY, choice);
    }
  } catch {
    // The choice still applies to this page; it just will not outlive it.
  }
}

function lightQuery(): MediaQueryList | undefined {
  return typeof window.matchMedia === "function"
    ? window.matchMedia(LIGHT_QUERY)
    : undefined;
}

export function systemTheme(): Theme {
  return lightQuery()?.matches ? "light" : "dark";
}

export function resolveTheme(choice: ThemeChoice, system: Theme): Theme {
  return choice === "system" ? system : choice;
}

export function applyTheme(theme: Theme): void {
  document.documentElement.setAttribute("data-theme", theme);
  const meta = document.querySelector('meta[name="theme-color"]');
  meta?.setAttribute("content", THEME_COLOR[theme]);
}

export function useTheme(): {
  choice: ThemeChoice;
  theme: Theme;
  cycle: () => void;
} {
  const [choice, setChoice] = useState<ThemeChoice>(readStoredChoice);
  const [system, setSystem] = useState<Theme>(systemTheme);

  // Subscribed unconditionally rather than only while `choice` is "system":
  // the listener is cheap, and keeping `system` current means switching back to
  // system mode lands on the right theme with no gap.
  useEffect(() => {
    const mql = lightQuery();
    if (!mql) return;
    const onChange = (event: MediaQueryListEvent) => {
      setSystem(event.matches ? "light" : "dark");
    };
    mql.addEventListener("change", onChange);
    return () => mql.removeEventListener("change", onChange);
  }, []);

  const theme = resolveTheme(choice, system);

  useEffect(() => {
    applyTheme(theme);
  }, [theme]);

  const cycle = useCallback(() => {
    setChoice((current) => {
      const next = NEXT[current];
      storeChoice(next);
      return next;
    });
  }, []);

  return { choice, theme, cycle };
}
