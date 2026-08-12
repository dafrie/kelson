# Design artifacts

Imported from the maintainer's Claude Design project on 2026-08-13. These files are the **design
source of truth for the M6 web UI ([#7](https://github.com/dafrie/kelson/issues/7)) and for the
documentation site** until each is implemented in code. When an implementation lands, the code
becomes authoritative and the artifact here becomes the historical record of the intent.

Canonical (editable) source: <https://claude.ai/design/p/f966a76d-3e0f-461c-a72d-319daf7a63dd>

## What each artifact is

| File | What it shows | Implements against |
|---|---|---|
| `Kelson Dashboard.dc.html` | The application-shaped console: app list with per-app status, environment/namespace scope, and the left rail (Apps, Agents, Sources, Events, Settings, Docs) | [#61](https://github.com/dafrie/kelson/issues/61), [#62](https://github.com/dafrie/kelson/issues/62), [#68](https://github.com/dafrie/kelson/issues/68) |
| `Kelson Docs.dc.html` | Documentation site shell: 236px left nav, quickstart terminal block, section card grid | Docs site |
| `Kelson Install.dc.html` | A documentation *content* page — numbered install steps, callout, right-hand "On this page" rail | Docs site |
| `Kelson Logo.dc.html` | Identity exploration board: mark construction on the 120-unit grid, clear space, lockups, variants, favicon, README banner | Brand |
| `assets/` | The shipping brand SVGs, plus [`assets/README.md`](assets/README.md) with the palette, type and clear-space rules | Brand |
| `support.js` | Third-party viewer runtime the `.dc.html` files load. Not kelson code — a generated bundle from the Design Claude toolchain | — |

## Viewing them

The `.dc.html` files are Design Claude documents: a `<x-dc>` template rendered client-side by
`support.js`, which pulls React and web fonts from CDNs. Open one in a browser from this directory
so the relative `./support.js` resolves. They are **artifacts, not part of any build** — nothing in
CI or the site build reads them.

## Design language

Both the docs and dashboard designs share one system, recorded in full in
[`assets/README.md`](assets/README.md):

- **Surfaces** `#0b0f13` page, `#10171f` panel, `#1d2732` border, `#18202a` hairline
- **Text** `#e6ebf0` primary, `#9aa7b4` secondary, `#8b98a6` tertiary, `#5d6875` muted
- **Brand / accent** `#0FA36B`, hover `#3ed49b`
- **Status** synced `#0FA36B`, reconciling `#4EA3FF`, degraded `#E0A944`, failed `#E2543A`, suspended `#6F7B89`
- **Type** Space Grotesk 400/500/600 for UI and headings (negative tracking on display sizes); IBM Plex Mono 400/500 for code, shas, labels and metadata
- The designs are **dark-only**. A light palette is not specified and would need a design decision.

## Read the copy as layout, not as fact

The screens are populated with **illustrative placeholder content that does not describe kelson as
it exists**. `Kelson Install.dc.html` shows a `helm repo add kelson https://charts.kelson.dev`
flow, a `kelson status` command and installed CRDs; the sidebars show a version `0.4.2`. None of
that ships today — per [CONTRIBUTING.md](../../CONTRIBUTING.md) kelson is pre-alpha and the install
path does not exist, and the install model is
[ADR-0003](../adr/0003-install-model.md)'s adoption-over-installation, which is not a Helm chart.

Take the **structure, hierarchy, spacing and visual language** from these files. Take the words from
[`docs/`](../).
