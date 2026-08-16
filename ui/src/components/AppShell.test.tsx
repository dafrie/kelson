import { afterEach, describe, expect, it } from "vitest";
import { act, render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { AppShell } from "./AppShell";
import { StatusPill } from "./StatusPill";
import { THEME_STORAGE_KEY } from "../theme";

function renderShell(at: string) {
  return render(
    <MemoryRouter initialEntries={[at]}>
      <Routes>
        <Route element={<AppShell />}>
          <Route path="projects" element={<span>projects screen</span>} />
          <Route path="cluster" element={<span>cluster screen</span>} />
          <Route path="connections" element={<span>connections screen</span>} />
        </Route>
      </Routes>
    </MemoryRouter>,
  );
}

describe("AppShell", () => {
  it("ships only nav items that lead somewhere", () => {
    renderShell("/projects");
    const nav = screen.getByRole("navigation", { name: "Primary" });
    expect(
      Array.from(nav.querySelectorAll("a")).map((a) => a.textContent),
    ).toEqual(["Projects", "Cluster", "Connections", "Setup"]);
  });

  it("marks the current destination active", () => {
    renderShell("/cluster");
    expect(screen.getByRole("link", { name: "Cluster" }).className).toContain(
      "k-nav__item--active",
    );
    expect(screen.getByRole("link", { name: "Projects" }).className).not.toContain(
      "k-nav__item--active",
    );
  });

  it("points Docs at a destination that exists", () => {
    renderShell("/projects");
    expect(screen.getByRole("link", { name: "Docs" })).toHaveProperty(
      "href",
      "https://github.com/dafrie/kelson/tree/main/docs",
    );
  });
});

describe("ThemeToggle in the shell", () => {
  afterEach(() => {
    window.localStorage.clear();
    document.documentElement.removeAttribute("data-theme");
  });

  it("sits in the header and cycles the theme on <html>", () => {
    renderShell("/projects");
    const toggle = screen.getByRole("button", { name: /^Theme:/ });

    // jsdom expresses no colour-scheme preference, so the shell opens on the
    // shipped design and the button reports that it is the system's call.
    expect(toggle.dataset["choice"]).toBe("system");
    expect(document.documentElement.getAttribute("data-theme")).toBe("dark");

    act(() => toggle.click());
    expect(toggle.dataset["choice"]).toBe("light");
    expect(document.documentElement.getAttribute("data-theme")).toBe("light");
    expect(window.localStorage.getItem(THEME_STORAGE_KEY)).toBe("light");

    act(() => toggle.click());
    expect(toggle.dataset["choice"]).toBe("dark");
    expect(document.documentElement.getAttribute("data-theme")).toBe("dark");

    act(() => toggle.click());
    expect(toggle.dataset["choice"]).toBe("system");
    expect(window.localStorage.getItem(THEME_STORAGE_KEY)).toBeNull();
  });
});

describe("StatusPill", () => {
  it("carries its tone as a data attribute and a modifier class", () => {
    render(<StatusPill status="reconciling" label="deploying" />);
    const pill = screen.getByText("deploying");
    expect(pill.dataset["status"]).toBe("reconciling");
    expect(pill.className).toContain("k-pill--reconciling");
  });

  it("prints the label and never the tone", () => {
    // The tone names are Flux's and Kubernetes' — the words a reader sees come
    // from components/status.ts, so `label` is required and there is no path by
    // which "reconciling" reaches a screen as text.
    render(<StatusPill status="unknown" label="no status yet" />);
    expect(screen.getByText("no status yet")).toBeTruthy();
    expect(screen.queryByText("unknown")).toBeNull();
  });
});
