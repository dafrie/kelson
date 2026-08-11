# Contributing to kelson

kelson is pre-alpha and in the design phase. Nothing is implemented yet.

## The most useful contribution right now is argument

The [ADRs](docs/adr/) record decisions that everything else will be built on. If you think one is wrong,
say so in an issue — reversing a decision today costs a conversation, and in a year it costs a rewrite.

Particularly interested in pushback on:

- **[ADR-0001](docs/adr/0001-hybrid-state-model.md)** — is renderer purity actually defensible in practice,
  or will real-world cases force cluster lookups into it?
- **[ADR-0003](docs/adr/0003-install-model.md)** — is adoption-over-installation testable at reasonable
  cost across the combinatorial space of cluster shapes?
- **[ADR-0005](docs/adr/0005-delegate-to-operators.md)** — does delegating to operators make the
  prerequisite burden too heavy for the low end of the market?

Also valuable: experience reports. If you run Coolify, Dokploy, Kubero or Canine in anger, what breaks?
What did you have to work around? That is worth more than a feature request.

## Ways to help

- **Experience reports** — open an issue describing what you run and what hurts
- **ADR review** — argue with the decisions
- **Design review** — [architecture](docs/architecture.md) and [roadmap](docs/roadmap.md)
- **Prior art** — if a project already solves one of these problems well, point us at it

## Once implementation starts

Guidelines for code, tests, commits and reviews will land with the M0 foundations work. Until then,
please don't send implementation PRs — the interfaces they'd target don't exist yet, and reviewing
speculative code against an unbuilt design wastes your time more than ours.

## Conduct

Be decent. Assume good faith. Argue about the technical substance, not the person making the argument.
