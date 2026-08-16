import { beforeEach, describe, expect, it, vi } from "vitest";
import { act, render, screen } from "@testing-library/react";

import {
  DETAIL_STORAGE_KEY,
  readStoredDetail,
  setDetail,
  useDetail,
} from "./preference";

/**
 * The Kubernetes-detail preference (#260): persisted, default off, and shared
 * by every surface without a provider.
 */

/** A component that says what the preference is, for the subscription tests. */
function Probe() {
  return <span data-testid="probe">{useDetail() ? "on" : "off"}</span>;
}

beforeEach(() => {
  // Order matters: the setter is what syncs the module's cached value, and
  // clearing storage underneath it would leave the two disagreeing.
  setDetail(false);
  window.localStorage.clear();
});

describe("the Kubernetes-detail preference", () => {
  it("is off in a browser that has never been told otherwise", () => {
    expect(readStoredDetail()).toBe(false);
    render(<Probe />);
    expect(screen.getByTestId("probe").textContent).toBe("off");
  });

  it("persists on, and spells off as the absence of the key", () => {
    setDetail(true);
    expect(window.localStorage.getItem(DETAIL_STORAGE_KEY)).toBe("on");
    expect(readStoredDetail()).toBe(true);

    setDetail(false);
    // Not "off", not "false": the same shape the theme gives its system state,
    // so a fresh browser and a reset one behave identically and there is no
    // second spelling to keep in step.
    expect(window.localStorage.getItem(DETAIL_STORAGE_KEY)).toBeNull();
    expect(readStoredDetail()).toBe(false);
  });

  it("reads anything that is not the one on value as off", () => {
    window.localStorage.setItem(DETAIL_STORAGE_KEY, "true");
    expect(readStoredDetail()).toBe(false);
    window.localStorage.setItem(DETAIL_STORAGE_KEY, "");
    expect(readStoredDetail()).toBe(false);
  });

  it("is off when storage throws outright", () => {
    const getItem = vi
      .spyOn(Storage.prototype, "getItem")
      .mockImplementation(() => {
        throw new Error("the profile is locked down");
      });
    try {
      expect(readStoredDetail()).toBe(false);
    } finally {
      getItem.mockRestore();
    }
  });

  it("re-renders every reader when it flips", () => {
    render(<Probe />);
    expect(screen.getByTestId("probe").textContent).toBe("off");

    act(() => setDetail(true));
    expect(screen.getByTestId("probe").textContent).toBe("on");

    act(() => setDetail(false));
    expect(screen.getByTestId("probe").textContent).toBe("off");
  });

  it("follows another tab of the same instance", () => {
    render(<Probe />);

    window.localStorage.setItem(DETAIL_STORAGE_KEY, "on");
    act(() => {
      window.dispatchEvent(
        new StorageEvent("storage", { key: DETAIL_STORAGE_KEY }),
      );
    });
    expect(screen.getByTestId("probe").textContent).toBe("on");

    // A cleared storage arrives with a null key and must not be ignored.
    window.localStorage.clear();
    act(() => {
      window.dispatchEvent(new StorageEvent("storage", { key: null }));
    });
    expect(screen.getByTestId("probe").textContent).toBe("off");
  });

  it("ignores another key moving", () => {
    render(<Probe />);
    act(() => setDetail(true));

    window.localStorage.setItem("kelson-theme", "light");
    act(() => {
      window.dispatchEvent(new StorageEvent("storage", { key: "kelson-theme" }));
    });
    expect(screen.getByTestId("probe").textContent).toBe("on");
  });
});
