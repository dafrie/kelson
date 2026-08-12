# ADR-0004: MIT, fully open, no feature gating

- **Status:** Accepted (amended 2026-08-12: no monetization intended — the earlier "hosting and support" hedge is removed)
- **Date:** 2026-08-11

## Context

The category offers three worked examples. Coolify is Apache-2.0 with no feature gating, is profitable,
and has 60k stars. Dokploy is Apache-2.0 with a carved-out `/proprietary` directory, with OIDC and audit
logs behind a paid enterprise tier — and users notice and resent it. Kubero is GPL-3.0.

Options considered: fully open; open core; fair-source/BSL.

## Decision

**MIT. Every feature. Permanently.**

SSO, RBAC, audit logging and multi-tenancy are security fundamentals and will never be paywalled.
kelson is not a commercial project and there is no monetization plan — no paid tiers, no paid presets,
no enterprise edition. It is open source because that is the point.

## Rationale

Paywalling authentication and audit means shipping a product that is insecure by default for anyone who
won't pay, which is most self-hosters. That is the anti-pattern in this category and it costs real
goodwill; Dokploy demonstrates it plainly.

Open core also imposes an ongoing tax that is easy to underestimate: every feature discussion becomes a
negotiation about which side of the line it falls on. For a small project that needs outside contributors,
that friction is more expensive than the revenue it protects.

BSL and fair-source were rejected because kelson's positioning is explicitly CNCF-adjacent — composing
with Flux, Argo CD, Gateway API, cert-manager, external-secrets and CloudNativePG. A non-OSI license
would cost exactly the adoption and contribution that positioning depends on.

MIT over Apache-2.0 keeps the existing license unchanged and stays maximally permissive. Apache-2.0's
explicit patent grant is a reasonable argument for a project targeting enterprise clusters, and this is
worth revisiting once there are outside contributors — but it is not worth a relicensing exercise today.

## Consequences

**Positive.**
- No feature-gating negotiation, ever. Every contribution lands in one codebase.
- Maximum compatibility with the CNCF ecosystem kelson depends on.
- The security posture matches the values the project claims.

**Negative.**
- No license-based protection against a hyperscaler repackaging kelson as a managed service. Accepted:
  at this stage obscurity is the larger risk, and Coolify demonstrates that a strong community and brand
  are adequate defence in this category.
- There is no revenue path, by intent. If that ever changes it requires a superseding ADR, and feature
  gating stays off the table regardless.
- MIT permits closed-source forks with no reciprocity. Accepted as the cost of maximum adoption.

## Revisit when

There are external contributors of substance (reconsider Apache-2.0 for the patent grant, which needs
their consent and so gets harder over time), or a commercial entity is created around the project.
