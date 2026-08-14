/**
 * Preview-only support for design-sync (claude.ai/design) cards.
 *
 * Two jobs:
 *
 *  - AppShell and PhaseRail render react-router elements (Link, NavLink,
 *    Outlet), so every preview is wrapped in a MemoryRouter via cfg.provider.
 *    Routes/Route are re-exported so the AppShell preview can mount it as a
 *    layout route and fill its Outlet — importing them from a second
 *    react-router copy would split the router context across bundles.
 *
 *  - The card harness paints a white page, but kelson is dark-first (the bare
 *    `:root` in tokens.css IS the dark palette). Cells are framed on the DS's
 *    own page surface so cards show the shipped design, not dark panels
 *    stranded on white.
 *
 * This module ships only in the design-sync bundle; the app never imports it.
 */
import type { ReactNode } from "react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

export function PreviewProvider({ children }: { children: ReactNode }) {
  return (
    <MemoryRouter initialEntries={["/projects"]}>
      <div
        style={{
          background: "var(--kelson-page)",
          color: "var(--kelson-text)",
          fontFamily: "var(--kelson-font)",
          fontSize: 14,
          lineHeight: 1.55,
          padding: 20,
          borderRadius: 8,
        }}
      >
        {children}
      </div>
    </MemoryRouter>
  );
}

export { Route, Routes };
