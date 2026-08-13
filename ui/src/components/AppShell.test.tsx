import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { AppShell } from "./AppShell";
import { StatusPill } from "./StatusPill";

function renderShell(at: string) {
  return render(
    <MemoryRouter initialEntries={[at]}>
      <Routes>
        <Route element={<AppShell />}>
          <Route path="apps" element={<span>apps screen</span>} />
          <Route path="cluster" element={<span>cluster screen</span>} />
        </Route>
      </Routes>
    </MemoryRouter>,
  );
}

describe("AppShell", () => {
  it("ships only nav items that lead somewhere", () => {
    renderShell("/apps");
    const nav = screen.getByRole("navigation", { name: "Primary" });
    expect(
      Array.from(nav.querySelectorAll("a")).map((a) => a.textContent),
    ).toEqual(["Apps", "Cluster"]);
  });

  it("marks the current destination active", () => {
    renderShell("/cluster");
    expect(screen.getByRole("link", { name: "Cluster" }).className).toContain(
      "k-nav__item--active",
    );
    expect(screen.getByRole("link", { name: "Apps" }).className).not.toContain(
      "k-nav__item--active",
    );
  });

  it("points Docs at a destination that exists", () => {
    renderShell("/apps");
    expect(screen.getByRole("link", { name: "Docs" })).toHaveProperty(
      "href",
      "https://github.com/dafrie/kelson/tree/main/docs",
    );
  });
});

describe("StatusPill", () => {
  it("carries its status as a data attribute and a modifier class", () => {
    render(<StatusPill status="reconciling" />);
    const pill = screen.getByText("reconciling");
    expect(pill.dataset["status"]).toBe("reconciling");
    expect(pill.className).toContain("k-pill--reconciling");
  });

  it("takes an override label", () => {
    render(<StatusPill status="unknown" label="no status yet" />);
    expect(screen.getByText("no status yet")).toBeTruthy();
  });
});
