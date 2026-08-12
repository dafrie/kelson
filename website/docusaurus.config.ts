import type * as Preset from '@docusaurus/preset-classic';
import type {Config} from '@docusaurus/types';
import {themes as prismThemes} from 'prism-react-renderer';

// The docs site renders the markdown in ../docs in place. That directory is the
// canonical content location — AGENTS.md and the CI docs-only fast path both key
// on it — so nothing is copied or synced into this directory. `docs/design/` is
// excluded: it holds design artifacts (.dc.html, brand SVGs), not documentation.
//
// Replaces the previous Material for MkDocs setup. Maintainer chose Docusaurus
// with versioned docs on 2026-08-13; an ADR was explicitly skipped.

const config: Config = {
  title: 'kelson',
  tagline:
    'A self-hosted PaaS that runs on your Kubernetes cluster and writes standard manifests instead of hiding them.',
  favicon: 'kelson-favicon-32.svg',

  url: 'https://dafrie.github.io',
  baseUrl: '/kelson/',

  organizationName: 'dafrie',
  projectName: 'kelson',

  onBrokenLinks: 'throw',
  onBrokenAnchors: 'warn',

  future: {
    v4: true,
    faster: true,
  },

  // `detect` parses .md as CommonMark and only .mdx as MDX. The docs in ../docs
  // were written for MkDocs and must keep rendering unchanged — this is what
  // stops MDX from reinterpreting angle brackets and braces in them.
  markdown: {
    format: 'detect',
    hooks: {
      onBrokenMarkdownLinks: 'throw',
    },
  },

  // The brand SVGs are served straight out of the design import rather than
  // copied here, so there is one source of truth for them.
  staticDirectories: ['static', '../docs/design/assets'],

  i18n: {
    defaultLocale: 'en',
    locales: ['en'],
  },

  presets: [
    [
      'classic',
      {
        docs: {
          path: '../docs',
          exclude: ['design/**'],
          routeBasePath: '/',
          sidebarPath: './sidebars.ts',
          editUrl: 'https://github.com/dafrie/kelson/edit/main/docs/',
          showLastUpdateTime: true,

          // Docusaurus otherwise reads the `0001-` in `adr/0001-hybrid-state-model.md`
          // as an ordering prefix and strips it from the URL. The ADR number is part
          // of the name, and the MkDocs site published these paths — keep them.
          numberPrefixParser: false,

          // Versioning is wired but no version is cut. kelson is pre-alpha, so
          // the live docs are the only docs. At first release, run
          //   npm run docusaurus docs:version 0.1
          // which snapshots ../docs into versioned_docs/. See README.md.
          lastVersion: 'current',
          versions: {
            current: {
              label: 'unreleased',
              path: '',
              // No banner: with no released version yet, "current" is also the
              // latest, so an "unreleased, see the latest version" notice would
              // link readers back to the page they are already on. Cutting the
              // first version turns this on for the older snapshots for free.
              banner: 'none',
            },
          },
        },
        blog: false,
        pages: false,
        theme: {
          customCss: './src/css/custom.css',
        },
      } satisfies Preset.Options,
    ],
  ],

  themeConfig: {
    image: 'kelson-lockup.svg',
    colorMode: {
      // The docs mockups are dark-only and no light palette was explored.
      // See docs/design/README.md.
      defaultMode: 'dark',
      disableSwitch: true,
      respectPrefersColorScheme: false,
    },
    navbar: {
      title: 'kelson',
      logo: {
        alt: 'kelson',
        src: 'kelson-mark.svg',
      },
      items: [
        {
          type: 'docsVersionDropdown',
          position: 'right',
        },
        {
          href: 'https://github.com/dafrie/kelson',
          label: 'GitHub',
          position: 'right',
        },
      ],
    },
    footer: {
      style: 'dark',
      links: [
        {
          title: 'Docs',
          items: [
            {label: 'Overview', to: '/'},
            {label: 'Architecture', to: '/architecture'},
            {label: 'Roadmap', to: '/roadmap'},
            {label: 'ADRs', to: '/adr/'},
          ],
        },
        {
          title: 'Project',
          items: [
            {label: 'GitHub', href: 'https://github.com/dafrie/kelson'},
            {label: 'Issues', href: 'https://github.com/dafrie/kelson/issues'},
            {
              label: 'Milestones',
              href: 'https://github.com/dafrie/kelson/milestones',
            },
          ],
        },
      ],
      copyright:
        'MIT. Every feature — SSO, RBAC and audit logs are not paywalled.',
    },
    prism: {
      theme: prismThemes.vsDark,
      darkTheme: prismThemes.vsDark,
      additionalLanguages: ['bash', 'yaml', 'json', 'go', 'diff'],
    },
  } satisfies Preset.ThemeConfig,
};

export default config;
