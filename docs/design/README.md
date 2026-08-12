# Design mockups

Imported from the maintainer's Claude Design project on 2026-08-13.

These are **mockups and reference material, not a specification.** They are one explored direction
for the look and feel of the web UI and the documentation site. Nothing here is binding: no issue is
committed to matching them, and an implementation that diverges is not thereby wrong. Treat them as
a starting point and a shared visual vocabulary — the kind of thing you look at before designing a
screen, not a document you implement against.

Canonical (editable) source: <https://claude.ai/design/p/f966a76d-3e0f-461c-a72d-319daf7a63dd>

## What each artifact is

| File | What it shows |
|---|---|
| `Kelson Dashboard.dc.html` | A sketch of an application-shaped console: app list with per-app status, environment scope, left rail |
| `Kelson Docs.dc.html` | A docs site shell: left nav, quickstart terminal block, section card grid |
| `Kelson Install.dc.html` | A docs *content* page: numbered steps, callout, right-hand "On this page" rail |
| `Kelson Logo.dc.html` | Identity exploration board: mark construction on the 120-unit grid, clear space, lockups, variants, favicon |
| `assets/` | The brand SVGs, plus [`assets/README.md`](assets/README.md) with the palette, type and clear-space notes |
| `support.js` | Third-party viewer runtime the `.dc.html` files load. Not kelson code — a generated bundle from the Design Claude toolchain |

## Viewing them

The `.dc.html` files are Design Claude documents: a `<x-dc>` template rendered client-side by
`support.js`, which pulls React and web fonts from CDNs. Open one in a browser from this directory
so the relative `./support.js` resolves. They are **artifacts, not part of any build** — no Go code
reads them, and nothing in CI depends on them.

## The visual language they explore

Recorded in full in [`assets/README.md`](assets/README.md); summarised here because the docs site
under `website/` currently follows it:

- **Surfaces** `#0b0f13` page, `#10171f` panel, `#1d2732` border, `#18202a` hairline
- **Text** `#e6ebf0` primary, `#9aa7b4` secondary, `#8b98a6` tertiary, `#5d6875` muted
- **Brand / accent** `#0FA36B`, hover `#3ed49b`
- **Status** synced `#0FA36B`, reconciling `#4EA3FF`, degraded `#E0A944`, failed `#E2543A`, suspended `#6F7B89`
- **Type** Space Grotesk 400/500/600 for UI and headings; IBM Plex Mono 400/500 for code, shas, labels and metadata
- The mockups are **dark-only**. No light palette is explored.

## Read the copy as placeholder

The screens are populated with **illustrative content that does not describe kelson as it exists**.
`Kelson Install.dc.html` shows a `helm repo add kelson https://charts.kelson.dev` flow, a
`kelson status` command and installed CRDs; the sidebars show a version `0.4.2`. None of that
ships today — kelson is pre-alpha, and a Helm-chart install is not the model
[ADR-0003](../adr/0003-install-model.md) describes.

Take the words from [`docs/`](../). The mockups are there for structure and feel.
