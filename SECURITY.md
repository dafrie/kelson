# Security

kelson is self-hosted software that can run with broad access to your cluster and your secrets. Treat
security reports seriously.

## Reporting a vulnerability

Please do **not** open a public issue for a security vulnerability. Report it privately to
[kelson-security@inbox.dafrie.de](mailto:kelson-security@inbox.dafrie.de).

Include, where possible:

- The affected component and version
- A description of the vulnerability and its impact
- Steps to reproduce, or a minimal proof of concept
- Any suggested remediation

You will receive an acknowledgment within 72 hours, and a status update on the next steps and timeline.

## Supported versions

kelson is pre-alpha with no stable releases. Security fixes are applied to the current `main` and
released with the next cut. There are no long-term support branches yet.

## Design posture

- **Secrets are references, never values** ([ADR-0009](docs/adr/0009-secrets.md)) — values don't enter Git.
- **Renderer purity** ([ADR-0001](docs/adr/0001-hybrid-state-model.md)) bounds what the rendering path can
  reach.
- Production defaults to propose-only mutation (see [README](README.md)).
