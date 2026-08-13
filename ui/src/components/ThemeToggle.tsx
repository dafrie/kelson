import { nextChoice, useTheme } from "../theme";
import type { ThemeChoice } from "../theme";

/**
 * The theme control: one button that cycles light -> dark -> system.
 *
 * The icon shows what is on screen right now, which in system mode is whatever
 * the OS says; the small "auto" mark next to it is what distinguishes "dark
 * because you asked" from "dark because your machine is". A three-state control
 * needs that, and a segmented control of three would outweigh everything else
 * in a 56px bar.
 *
 * The SVGs are inline and hand-drawn against the mark's own vocabulary — 2px
 * round-capped strokes on currentColor — rather than pulled from an icon
 * package. `ui/README.md`: adding a dependency here is a decision, not a
 * convenience, and two glyphs are not a decision.
 */

const DESCRIPTION: Record<ThemeChoice, string> = {
  light: "light",
  dark: "dark",
  system: "system",
};

export function ThemeToggle() {
  const { choice, theme, cycle } = useTheme();
  const label = `Theme: ${DESCRIPTION[choice]}. Switch to ${DESCRIPTION[nextChoice(choice)]}.`;

  return (
    <button
      type="button"
      className="k-theme"
      onClick={cycle}
      aria-label={label}
      title={label}
      data-choice={choice}
    >
      {theme === "light" ? <SunIcon /> : <MoonIcon />}
      {choice === "system" ? (
        <span className="k-theme__auto" aria-hidden="true">
          auto
        </span>
      ) : null}
    </button>
  );
}

function SunIcon() {
  return (
    <svg
      width="15"
      height="15"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="2"
      strokeLinecap="round"
      aria-hidden="true"
      focusable="false"
    >
      <circle cx="12" cy="12" r="4.4" />
      <path d="M12 2.4v2.2M12 19.4v2.2M21.6 12h-2.2M4.6 12H2.4M18.8 5.2l-1.6 1.6M6.8 17.2l-1.6 1.6M18.8 18.8l-1.6-1.6M6.8 6.8L5.2 5.2" />
    </svg>
  );
}

function MoonIcon() {
  return (
    <svg
      width="15"
      height="15"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="2"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
      focusable="false"
    >
      <path d="M20.5 14.3A8.6 8.6 0 0 1 9.7 3.5a8.6 8.6 0 1 0 10.8 10.8Z" />
    </svg>
  );
}
