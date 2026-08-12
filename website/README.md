# kelson docs site

[Docusaurus](https://docusaurus.io/) 3.10.2, static output, replacing the previous Material for
MkDocs setup. The maintainer chose Docusaurus with versioned docs on 2026-08-13; no ADR was written
for it.

```sh
npm install
npm start      # dev server with hot reload, http://localhost:3000/kelson/
npm run build  # static site into build/
npm run serve  # serve build/ locally
npm run typecheck
```

Node >= 20 (`engines` in `package.json`). Dependencies are pinned to exact versions and
`package-lock.json` is committed.

## The content lives in `../docs`, not here

`docusaurus.config.ts` points the docs plugin at `path: '../docs'`. Nothing is copied or synced —
the markdown is read where it sits. That directory is canonical for three reasons that all still
hold: `AGENTS.md` describes it as such, the CI docs-only fast path keys on `docs/`, and the
repository's own cross-links (`docs/adr/0001-....md` and friends) resolve against it.

Consequences worth knowing before you change things:

- **Adding a page means adding it to `sidebars.ts`.** The sidebar is explicit, ported one-for-one
  from the old `mkdocs.yml` `nav:` so nothing was orphaned in the migration. A new file under
  `docs/` will build but will not appear in navigation until it is listed.
- **`docs/design/` is excluded.** It holds design mockups and brand assets, not documentation.
- **`numberPrefixParser` is off**, so `adr/0001-hybrid-state-model.md` keeps its number in the URL
  instead of Docusaurus stripping `0001-` as an ordering prefix. This preserves the URLs the MkDocs
  site published.
- **`markdown.format` is `detect`**, so `.md` files parse as CommonMark and only `.mdx` parses as
  MDX. The existing docs were written for MkDocs; this is what keeps angle brackets and braces in
  them from being reinterpreted as JSX. If you want components in a page, name it `.mdx`.
- **Broken markdown links fail the build** (`onBrokenLinks` and `onBrokenMarkdownLinks` are
  `throw`). This is a feature — it caught a link in `docs/release-policy.md` that had been pointing
  outside the docs tree and would have 404'd on the published MkDocs site.

## Versioning

Versioning is wired but **no version has been cut**, because kelson has not released. The live
content in `../docs` is the single "current" version, labelled `unreleased`, served at the site
root, and shown in the navbar version dropdown.

At the first release:

```sh
npm run docusaurus docs:version 0.1
```

That **snapshots `../docs` into `website/versioned_docs/version-0.1/`** and writes
`versioned_sidebars/`. From then on `../docs` is the in-progress "next" docs and the snapshot is
what users see by default. Two things to decide at that moment, not before:

- The snapshot is a real copy in git. That is inherent to how Docusaurus versions docs, and it is
  the reason no placeholder version was cut now — a fake `0.1` would have duplicated every page for
  no benefit and quietly created a second, staler copy of the canonical content.
- `versions.current.banner` is `none` today because with one version "current" is also the latest,
  so the standard "this is unreleased, see the latest version" notice would link readers to the page
  they are already on. Once a version exists, drop the override and let Docusaurus manage banners.

## Brand assets

`staticDirectories` includes `../docs/design/assets`, so the navbar mark, the favicon and the social
image are served straight from the design import rather than duplicated here. The side effect is
that `docs/design/assets/README.md` is also copied to the site root as `/README.md`. That was judged
better than keeping a second copy of the brand SVGs that could silently drift.

## Design

The look follows the mockups in `docs/design/Kelson Docs.dc.html` and
`docs/design/Kelson Install.dc.html`, implemented entirely in `src/css/custom.css` against the
stock classic theme — no swizzled components. Those mockups are reference material, not a
specification; see `docs/design/README.md`.

The site is dark-only and the colour-mode switch is disabled, because no light palette was explored.
Fonts (Space Grotesk, IBM Plex Mono) are loaded from Google Fonts, as in the mockups. If the project
would rather the docs site made no third-party requests, self-host them — that is a one-file change
here.
