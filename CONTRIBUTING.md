# Contributing to kelson

kelson is pre-alpha. The renderer, the spec model, the delivery adapters (direct and Flux), the
`kelson` CLI, `kelson-server` — which serves the v1alpha1 schema over ConnectRPC — and `kelson-mcp`,
the agent surface over that API ([docs/mcp.md](docs/mcp.md)), exist and are tested. The server
installs with the Helm chart in `deploy/chart/kelson` ([docs/install.md](docs/install.md)); the UI
does not exist yet. The server's authentication is an interim single shared password
([ADR-0013](docs/adr/0013-server-state-and-api-v0.md) §3, amended 2026-08-13,
[docs/server.md](docs/server.md)): without one it binds loopback only, with one it may bind wider
behind a TLS-terminating proxy. The real design is
[#84](https://github.com/dafrie/kelson/issues/84).
The [ADRs](docs/adr/) record the load-bearing decisions, and the
[milestones](https://github.com/dafrie/kelson/milestones) are the current state of the project.

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

## Issues and templates

Use the issue forms in `.github/ISSUE_TEMPLATE/`: **bug report** for broken behavior, **feature request**
for new capability, and **design / ADR request** for anything that changes a load-bearing decision.
Design questions belong in the design template, not buried in a feature request — they need the
alternatives-and-consequences reasoning an ADR records.

## Implementation

Work is scoped in the [issue tracker](https://github.com/dafrie/kelson/issues) and grouped into
[milestones](https://github.com/dafrie/kelson/milestones); an issue's milestone tells you whether the
ground it stands on exists yet. Before you start, check the roadmap: if an interface it would target
doesn't exist yet, the PR will bounce. The four-plane layout is the contract — changes that cross a
plane boundary or touch a load-bearing property should reference or argue with the relevant
[ADR](docs/adr/).

### Setup

```sh
go test ./...   # or: make test (adds -race)
make lint       # golangci-lint
make build
make fmt
```

### Code style

- Go. Format with `gofmt` (see `make fmt`). `golangci-lint` must pass — the rule set is in `.golangci.yml`.
- Two languages (Go core, TypeScript/React UI per [ADR-0002](docs/adr/0002-tech-stack.md)); keep each side
  idiomatic for its toolchain and follow the lint rules that ship with it.
- No comments that restate the code. Comments explain *why*, not *what*.

### The renderer-purity rule

`internal/renderer` must stay a deterministically **pure function** ([ADR-0001](docs/adr/0001-hybrid-state-model.md),
[#20](https://github.com/dafrie/kelson/issues/20)): no Kubernetes client, no network, no clock, no ambient
filesystem. This is enforced by a `depguard` rule in `.golangci.yml` and is the load-bearing constraint of
the project. Golden-file tests depend on it — the same input must produce the same bytes.

### Tests

- Renderer logic is **golden-file tested**: fixed input, byte-identical output. Any change that alters
  rendered output for unchanged input updates the golden files and must be reviewed as such.
- Keep the renderer pure so it can be tested without a cluster. Anything that needs a real API server
  lives outside `internal/renderer`.
- Run the full suite with `make test` before opening a PR.

### Commit conventions

[Conventional commits](https://www.conventionalcommits.org/), one logical change per commit, and reference
the issue you're working on:

```
feat(renderer): add ClusterProfile detection
fix(cli): honor --dry-run flag
docs(adr): add ADR-0010 identity model
```

Commit messages describe the change, not the process. Always reference the issue number.

### Review process

- Open a PR against `main` using the [pull request template](.github/pull_request_template.md). Reference
  the issue(s) it closes and any ADRs it touches.
- Any behavior change must point at the ADR that licenses it, or add one. If a change contradicts an
  accepted ADR, that is a design discussion, not a code review.
- CI runs lint and tests; keep them green. Renderer-purity violations are a review-blocking defect, not
  style feedback.

## Conduct

Be decent. Assume good faith. Argue about the technical substance, not the person making the argument.
See [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md).
