# Building with kelson-ui

kelson is a calm, mono-accented operations console: Space Grotesk for UI text,
IBM Plex Mono for anything an operator might retype (shas, namespaces, codes),
one brand green, and dark as the shipped default theme.

## Theme and page setup

- Set `data-theme="dark"` or `data-theme="light"` on `<html>`. The bare
  `:root` already carries the dark palette, so an app that sets nothing renders
  dark; `data-theme="light"` switches every token. Never hard-code colours —
  both themes redefine the same `--kelson-*` token set.
- Paint pages with the tokens: `background: var(--kelson-page)`,
  `color: var(--kelson-text)`, `font-family: var(--kelson-font)`, 14px body.
- `ThemeToggle` is the one theme control (cycles light → dark → system) and
  applies the choice to `<html>` itself.
- `AppShell` and `PhaseRail` render react-router elements (`NavLink`,
  `Outlet`, `Link`), so mount them under a router; `AppShell` is a layout
  route — put the page in its child route, it renders into the `Outlet`.
  No other provider is required by any component.

## Styling idiom: tokens + the small `k-` vocabulary

Style with CSS custom properties, never raw hex. The families:

- Surfaces: `--kelson-page`, `--kelson-panel`, `--kelson-panel-dim`,
  `--kelson-header`, `--kelson-inset`; borders `--kelson-border`,
  `--kelson-hairline`, `--kelson-border-hover`; `--kelson-shadow`.
- Text ramp: `--kelson-text`, `--kelson-text-2`, `--kelson-text-3`,
  `--kelson-muted`, `--kelson-muted-2`, `--kelson-muted-deep`, `--kelson-code`.
- Brand: `--kelson-green` (the mark, theme-independent), `--kelson-accent`
  (brand as text/links), `--kelson-accent-hover`.
- Status (with matching `-fill`/`-border` pairs for chips): `--kelson-synced`,
  `--kelson-reconciling`, `--kelson-degraded`, `--kelson-failed`,
  `--kelson-suspended` — but prefer `StatusPill` over hand-built chips.
- Type: `--kelson-font`, `--kelson-font-mono`; layout:
  `--kelson-header-height` (56px), `--kelson-content-max` (1240px).

Shared classes the stylesheet ships for your own glue (use them instead of
reinventing): `k-panel` (card surface; `k-panel--dim`, `k-panel--interactive`),
`k-state` (loading/empty/error blocks — or just use the State components),
`k-eyebrow` (uppercase mono section label), `k-mono` (12.5px mono),
`k-input`, `k-select`, `k-button` (form controls, mono).

Layout is plain flex/grid with numeric gaps (8–28px); headings carry
-0.02em tracking (h1 26px / h2 18px / h3 15px, already styled on bare tags).

## Where the truth lives

Read `styles.css` → `_ds_bundle.css` (tokens are defined at the top-level
`:root` blocks; component classes follow) before inventing any style, and each
component's `.d.ts` + `.prompt.md` for its API and a working composition.

## Idiomatic page skeleton

```tsx
import { AppShell, StatusPill, EmptyState } from "kelson-ui";
// under a router:
<Route element={<AppShell />}>
  <Route path="/projects" element={
    <section style={{ display: "grid", gap: 12 }}>
      <span className="k-eyebrow">Projects</span>
      <a className="k-panel k-panel--interactive" href="#">
        <div style={{ display: "flex", gap: 10, alignItems: "center" }}>
          <h3>checkout</h3>
          <StatusPill status="synced" />
        </div>
        <span className="k-mono">production · rev 9f31c0d</span>
      </a>
      <EmptyState title="No other projects yet" />
    </section>
  } />
</Route>
```
