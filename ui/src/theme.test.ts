import { afterEach, describe, expect, it } from "vitest";
import { act, renderHook } from "@testing-library/react";

import {
  LIGHT_QUERY,
  THEME_STORAGE_KEY,
  nextChoice,
  readStoredChoice,
  useTheme,
} from "./theme";

type Listener = (event: MediaQueryListEvent) => void;

const realMatchMedia = window.matchMedia;

/**
 * Stand in for the OS preference.
 *
 * jsdom's own `matchMedia` never matches anything and never fires, which would
 * make "follows the system" untestable in the direction that matters. This
 * substitute answers `(prefers-color-scheme: light)` and can flip, so the live
 * system-change path is exercised rather than assumed.
 */
function stubSystem(prefersLight: boolean) {
  const listeners = new Set<Listener>();
  const mql = {
    matches: prefersLight,
    media: LIGHT_QUERY,
    addEventListener: (_type: string, fn: Listener) => void listeners.add(fn),
    removeEventListener: (_type: string, fn: Listener) => void listeners.delete(fn),
  };
  window.matchMedia = ((query: string) => {
    if (query !== LIGHT_QUERY) {
      throw new Error(`unexpected media query: ${query}`);
    }
    return mql as unknown as MediaQueryList;
  }) as typeof window.matchMedia;

  return {
    flipTo(light: boolean) {
      mql.matches = light;
      act(() => {
        for (const fn of listeners) fn({ matches: light } as MediaQueryListEvent);
      });
    },
    get listenerCount() {
      return listeners.size;
    },
  };
}

function themeAttribute() {
  return document.documentElement.getAttribute("data-theme");
}

afterEach(() => {
  window.matchMedia = realMatchMedia;
  window.localStorage.clear();
  document.documentElement.removeAttribute("data-theme");
});

describe("useTheme", () => {
  it("follows the system when nothing is stored", () => {
    stubSystem(true);
    const { result } = renderHook(() => useTheme());
    expect(result.current.choice).toBe("system");
    expect(result.current.theme).toBe("light");
    expect(themeAttribute()).toBe("light");
  });

  it("falls to dark when the system prefers nothing", () => {
    // No expressed preference: `(prefers-color-scheme: light)` does not match,
    // and dark is the shipped design, so that is where an unknown lands.
    stubSystem(false);
    const { result } = renderHook(() => useTheme());
    expect(result.current.theme).toBe("dark");
    expect(themeAttribute()).toBe("dark");
  });

  it("takes a stored choice over the system", () => {
    window.localStorage.setItem(THEME_STORAGE_KEY, "light");
    stubSystem(false);
    const { result } = renderHook(() => useTheme());
    expect(result.current.choice).toBe("light");
    expect(themeAttribute()).toBe("light");
  });

  it("treats a value it did not write as no choice at all", () => {
    window.localStorage.setItem(THEME_STORAGE_KEY, "sepia");
    stubSystem(true);
    expect(readStoredChoice()).toBe("system");
    const { result } = renderHook(() => useTheme());
    expect(result.current.choice).toBe("system");
    expect(result.current.theme).toBe("light");
  });

  it("cycles light -> dark -> system and persists each step", () => {
    stubSystem(true);
    window.localStorage.setItem(THEME_STORAGE_KEY, "light");
    const { result } = renderHook(() => useTheme());

    expect(result.current.choice).toBe("light");

    act(() => result.current.cycle());
    expect(result.current.choice).toBe("dark");
    expect(result.current.theme).toBe("dark");
    expect(themeAttribute()).toBe("dark");
    expect(window.localStorage.getItem(THEME_STORAGE_KEY)).toBe("dark");

    act(() => result.current.cycle());
    expect(result.current.choice).toBe("system");
    // System mode is the *absence* of the key, so there is one representation
    // of "no choice" and a reset browser behaves like a fresh one.
    expect(window.localStorage.getItem(THEME_STORAGE_KEY)).toBeNull();
    expect(result.current.theme).toBe("light");

    act(() => result.current.cycle());
    expect(result.current.choice).toBe("light");
    expect(window.localStorage.getItem(THEME_STORAGE_KEY)).toBe("light");
  });

  it("applies a system change live while in system mode", () => {
    const system = stubSystem(false);
    const { result } = renderHook(() => useTheme());
    expect(result.current.theme).toBe("dark");

    system.flipTo(true);
    expect(result.current.theme).toBe("light");
    expect(themeAttribute()).toBe("light");

    system.flipTo(false);
    expect(result.current.theme).toBe("dark");
    expect(themeAttribute()).toBe("dark");
  });

  it("ignores a system change once a choice was made", () => {
    const system = stubSystem(false);
    const { result } = renderHook(() => useTheme());
    act(() => result.current.cycle()); // system -> light

    system.flipTo(true);
    expect(result.current.choice).toBe("light");
    expect(themeAttribute()).toBe("light");

    system.flipTo(false);
    expect(result.current.theme).toBe("light");
  });

  it("drops the media listener on unmount", () => {
    const system = stubSystem(false);
    const { unmount } = renderHook(() => useTheme());
    expect(system.listenerCount).toBe(1);
    unmount();
    expect(system.listenerCount).toBe(0);
  });
});

describe("nextChoice", () => {
  it("is a three-state cycle", () => {
    expect(nextChoice("light")).toBe("dark");
    expect(nextChoice("dark")).toBe("system");
    expect(nextChoice("system")).toBe("light");
  });
});
