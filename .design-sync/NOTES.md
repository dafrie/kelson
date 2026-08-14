# design-sync notes for kelson

Repo-specific facts a future sync needs. The config lives in
`.design-sync/config.json`; scripts are staged into `.ds-sync/` (gitignored)
per the design-sync skill.

## Setup

- `kelson-ui` is a **private Vite app**, not a built library: no `dist/`, no
  `.d.ts` tree. The bundle entry is the committed barrel
  `ui/design-sync.entry.ts` (wired as `cfg.entry`), which exports exactly the
  presentational component set and imports the three stylesheets
  (`tokens.css`, `base.css`, `pages.css`) the way `main.tsx` does.
- **Never let the converter synthesize an entry from `src/`**: the synth barrel
  star-exports `main.tsx`, whose top-level `createRoot` throws
  ("index.html is missing #root") in any page without a `#root` div and kills
  the whole bundle IIFE. The barrel entry exists precisely to avoid this.
- Install: `cd ui && npm ci` (package-lock; node >= 22.12).
- Converter deps in `.ds-sync/` also need `playwright` (npm i playwright);
  launch chromium with `DS_CHROMIUM_PATH=/opt/pw-browsers/chromium` in the
  remote container (the pw cache pins chromium-1194; a freshly installed
  playwright wants a newer build it doesn't have).
- Shared control styles (`k-input`, `k-button`, `k-select`, `k-disclosure`,
  `k-copy`, `k-pre`, `k-user`, `k-nav`…) live in `ui/src/pages/pages.css`,
  not next to the components — the entry's CSS imports cover them.
- Fonts ship from `@fontsource` via `cfg.extraFonts` (Space Grotesk 400/500/600,
  IBM Plex Mono 400/500) — 19 @font-face rules, self-hosted like the app.
- `cfg.provider` is `PreviewProvider` from `.design-sync/preview-support.tsx`
  (merged via `extraEntries`): a MemoryRouter (AppShell/PhaseRail render Link/
  NavLink/Outlet) plus a frame painting `var(--kelson-page)` behind every cell
  — the card harness paints white, kelson is dark-first.
- `useAuth` needs no provider: its context default is `{status: "disabled"}`,
  which renders AppShell with no user chip, same as an unauthenticated server.

## Scope decisions (owner-confirmed 2026-08-14)

- Included: the presentational set — AppShell, StatusPill, ErrorPanel,
  Disclosure, YamlBlock, LiveIndicator, ThemeToggle, Copyable, KelsonMark,
  EnvValueFields, LoadingState/EmptyState/ErrorState/ServerUnreachableState,
  PhaseRail, DiffView (16 cards from 12 source files).
- Excluded on purpose: the backend-wired panels (ComponentsChecklist,
  NodesSection, CapabilityPanel, DataServices, Previews, SecretsPanel) — they
  fetch from kelson-server via useClients/useAsync and cannot render as
  standalone design parts — and all of `pages/`.

## Known render warns

- AppShell and ThemeToggle cards render in the **light** palette: ThemeToggle's
  `useTheme` applies the resolved theme (`prefers-color-scheme`) to `<html>`,
  and headless chromium reports light. Both palettes are real shipped themes;
  not a bug. Every other card renders dark (bare `:root` is the dark palette).
- LiveIndicator "Reconnecting" screenshots can catch the dot mid-pulse
  (opacity dip) — the animation is real, not a broken render.
- `cardMode: column` is set for DiffView and EnvValueFields (grid overflow);
  applied 2026-08-14, don't remove without re-checking [GRID_OVERFLOW].

## Re-sync risks

- `ui/design-sync.entry.ts` is hand-maintained: a new presentational component
  must be added there AND to `cfg.componentSrcMap`, or it won't ship.
- Preview data (RailInput/Diff/WireError literals in
  `.design-sync/previews/*.tsx`) mirrors types in `ui/src/deploy/rail.ts`,
  `ui/src/diff/parse.ts` and the generated proto — a breaking rename there
  breaks preview compiles (shows as floor cards; check the build log for
  `! preview build failed`).
- The light-theme rendering of AppShell/ThemeToggle depends on the harness's
  `prefers-color-scheme`; a headless default change would flip those two cards
  (harmless, but screenshots will differ).
- Toolchain assumed: node 22, npm; chromium from the container's
  /opt/pw-browsers cache. Nothing is fetched from the network at render time
  (fonts self-hosted — mirrors the app's air-gap constraint).
