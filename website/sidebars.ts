import type {SidebarsConfig} from '@docusaurus/plugin-content-docs';

// Ported one-for-one from the `nav:` block of the mkdocs.yml this site replaces,
// so no page that was reachable before is orphaned now. Every markdown file under
// ../docs (excluding design/) appears exactly once below.
//
// Labels are set explicitly for the same reason MkDocs set them: several pages
// have long descriptive H1s that make a poor navigation entry. Without a label
// here the sidebar would inherit the H1.

const sidebars: SidebarsConfig = {
  docs: [
    {type: 'doc', id: 'index', label: 'Home'},
    {type: 'doc', id: 'vision', label: 'Vision'},
    {type: 'doc', id: 'install', label: 'Install'},
    {type: 'doc', id: 'architecture', label: 'Architecture'},
    {type: 'doc', id: 'roadmap', label: 'Roadmap'},
    {type: 'doc', id: 'competitive-analysis', label: 'Competitive analysis'},
    {
      type: 'category',
      label: 'Reference',
      collapsed: false,
      items: [
        {type: 'doc', id: 'reference/project', label: 'Project spec'},
        {type: 'doc', id: 'reference/environment', label: 'Environment spec'},
        {type: 'doc', id: 'detection', label: 'Cluster detection'},
        {type: 'doc', id: 'server', label: 'Server'},
        {type: 'doc', id: 'mcp', label: 'MCP server'},
        {type: 'doc', id: 'reference/support-matrix', label: 'Support matrix'},
        {type: 'doc', id: 'e2e', label: 'E2E harness'},
      ],
    },
    {
      type: 'category',
      label: 'Design',
      collapsed: false,
      items: [
        {type: 'doc', id: 'model', label: 'Model'},
        {type: 'doc', id: 'build', label: 'Build'},
        {type: 'doc', id: 'delivery', label: 'Delivery'},
        {type: 'doc', id: 'secrets', label: 'Secrets'},
        {type: 'doc', id: 'data-services', label: 'Data services'},
        {type: 'doc', id: 'statemachine', label: 'State machine'},
        {type: 'doc', id: 'release-policy', label: 'Release policy'},
      ],
    },
    {
      type: 'category',
      label: 'Research',
      collapsed: false,
      items: [
        {
          type: 'doc',
          id: 'research/build-strategy-selection',
          label: 'Build strategy selection',
        },
        {
          type: 'doc',
          id: 'research/score-as-input-format',
          label: 'Score as an input format',
        },
        {
          type: 'doc',
          id: 'research/hierarchy-prior-art',
          label: 'Hierarchy prior art',
        },
      ],
    },
    {
      type: 'category',
      label: 'ADRs',
      collapsed: false,
      items: [
        {type: 'doc', id: 'adr/README', label: 'Index'},
        {
          type: 'doc',
          id: 'adr/0001-hybrid-state-model',
          label: '0001 — Hybrid state model',
        },
        {type: 'doc', id: 'adr/0002-tech-stack', label: '0002 — Tech stack'},
        {
          type: 'doc',
          id: 'adr/0003-install-model',
          label: '0003 — Install model',
        },
        {type: 'doc', id: 'adr/0004-licensing', label: '0004 — Licensing'},
        {
          type: 'doc',
          id: 'adr/0005-delegate-to-operators',
          label: '0005 — Delegate to operators',
        },
        {
          type: 'doc',
          id: 'adr/0006-project-application-environment',
          label: '0006 — Project, Application, Environment',
        },
        {
          type: 'doc',
          id: 'adr/0007-data-services',
          label: '0007 — Data services',
        },
        {type: 'doc', id: 'adr/0008-mcp-surface', label: '0008 — MCP surface'},
        {type: 'doc', id: 'adr/0009-secrets', label: '0009 — Secrets'},
        {
          type: 'doc',
          id: 'adr/0010-build-strategy',
          label: '0010 — Build strategy',
        },
        {type: 'doc', id: 'adr/0011-build-cache', label: '0011 — Build cache'},
        {
          type: 'doc',
          id: 'adr/0012-flux-only-gitops',
          label: '0012 — Flux-only GitOps',
        },
        {
          type: 'doc',
          id: 'adr/0013-server-state-and-api-v0',
          label: '0013 — Server state and API v0',
        },
        {
          type: 'doc',
          id: 'adr/0014-components',
          label: '0014 — Components',
        },
        {
          type: 'doc',
          id: 'adr/0015-valkey-operator',
          label: '0015 — Valkey operator',
        },
        {
          type: 'doc',
          id: 'adr/0016-delivery-flows-v0',
          label: '0016 — Delivery flows v0',
        },
        {
          type: 'doc',
          id: 'adr/0017-pr-previews',
          label: '0017 — PR previews',
        },
        {
          type: 'doc',
          id: 'adr/0018-secret-references',
          label: '0018 — Secret references',
        },
        {
          type: 'doc',
          id: 'adr/0019-release-command-hook',
          label: '0019 — Release command hook',
        },
        {
          type: 'doc',
          id: 'adr/0020-external-secrets',
          label: '0020 — External secrets',
        },
        {
          type: 'doc',
          id: 'adr/0021-installing-missing-components',
          label: '0021 — Installing missing components',
        },
        {
          type: 'doc',
          id: 'adr/0022-sops-age',
          label: '0022 — SOPS + age',
        },
        {
          type: 'doc',
          id: 'adr/0023-explain-structured-causes',
          label: '0023 — Explain: structured causes',
        },
        {
          type: 'doc',
          id: 'adr/0024-agent-identities',
          label: '0024 — Agent identities',
        },
        {
          type: 'doc',
          id: 'adr/0025-agent-policy',
          label: '0025 — Agent policy',
        },
        {
          type: 'doc',
          id: 'adr/0026-agent-audit-trail',
          label: '0026 — The audit trail',
        },
      ],
    },
  ],
};

export default sidebars;
