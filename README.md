# kelson

A self-hosted PaaS that runs on your Kubernetes cluster and writes standard manifests instead of hiding them.

**Pre-alpha, and not usable yet.** What works end to end today: `kelson render`, `kelson diff`
(including server-side dry-run), `kelson deploy`, `kelson status`, `kelson rollback`,
`kelson promote` and `kelson profile`, plus `kelson-server`, which serves the same capabilities over
ConnectRPC — loopback-only and unauthenticated in v0
([ADR-0013](docs/adr/0013-server-state-and-api-v0.md)) — and `kelson-mcp`, the agent surface over that
API ([docs/mcp.md](docs/mcp.md)). There is no UI and no install path yet.
The [roadmap](docs/roadmap.md) and
[issues](https://github.com/dafrie/kelson/issues) track the assembly work.

## Why Kubernetes

Every PaaS that isn't Kubernetes is a bet that you won't need something its authors didn't plan for. The bet fails the first time you need a sidecar, an operator, a network policy, or a second region. At that point you aren't extending the platform, you're leaving it.

Kubernetes works the same way on one node and on two hundred. What it lacks is a good front door, not capability.

There's a newer reason too. LLMs are good at Kubernetes, because the manifests, operators and documentation are all in the training data. An agent working with a Deployment or an HTTPRoute is on familiar ground. An agent working with a PaaS's private abstraction is guessing at someone's internal model.

So kelson doesn't wrap Kubernetes in new concepts. It generates Kubernetes.

## How it works

```
     kelson.yaml
          │
    ┌─────▼─────┐
    │  Renderer │   pure function: no cluster, no network, no clock
    └─────┬─────┘
          │  plain Kubernetes YAML
      ┌───┴───┐
      ▼       ▼
   direct    Flux
      └───┬───┘
          ▼
     your cluster
```

The renderer is a pure function, so the same input always produces the same bytes. That's what makes the delivery modes one code path rather than two, and what makes previews worth trusting. Delivery is a pluggable adapter seam; Flux is the supported GitOps mode, and an Argo CD adapter may return later ([ADR-0012](docs/adr/0012-flux-only-gitops.md)).

Direct mode is Git mode with an implicit repository. It still versions rendered output, so you keep diffs, history and rollback — and since every deployment is already standard manifests, `kelson render` reproduces the exact YAML whenever you want it outside kelson.

## What's different

**Uninstalling doesn't break anything.** `kelson uninstall` removes what kelson deployed for one environment — previewed object by object, data called out separately, nothing deleted without an answer — and leaves everything else in the namespace untouched, including a namespace it adopted rather than created. `helm uninstall` removes the server and leaves your applications running. Both halves are covered by the end-to-end suite ([#59](https://github.com/dafrie/kelson/issues/59), [test/e2e](test/e2e/)); that suite is not yet a required CI check, so treat it as verified-on-demand rather than gated.

**You see what will happen first.** Three levels: a rendered diff, a server-side dry-run against the real API server, and an ephemeral live environment. The middle one is the API server's own answer, including admission webhooks, policy rejections and quota checks. Nothing else in this category surfaces it.

**It adopts what you already run.** kelson detects Gateway API, cert-manager, external-secrets, Prometheus, CloudNativePG and Flux, and renders to fit. Routing is Gateway API only — clusters without it get a clear capability gap and an offer to install a Gateway implementation, never a parallel ingress stack next to yours.

**Agents get guardrails, not just tools.** The MCP server ships (`kelson-mcp`, [docs/mcp.md](docs/mcp.md)): seven task-shaped tools over the same API, every mutation defaulting to a dry run, every error structured with a code and a remediation. *Still designed rather than built ([M7](https://github.com/dafrie/kelson/issues/8))*: agents authenticating as themselves with scoped, expiring credentials, and per-environment policy deciding what they may do unsupervised. In production the intended default is propose-only, which is the same pull-request path a human uses.

**It doesn't reimplement operators.** CloudNativePG for Postgres, Strimzi for Kafka, cert-manager for TLS, external-secrets for secrets. Kubero vendored Bitnami charts and broke working installs when the catalog was withdrawn.

## Decisions

- [ADR-0001](docs/adr/0001-hybrid-state-model.md) — One pure renderer, pluggable delivery
- [ADR-0002](docs/adr/0002-tech-stack.md) — Go control plane, TypeScript/React UI
- [ADR-0003](docs/adr/0003-install-model.md) — Adopt existing clusters, bootstrap empty ones
- [ADR-0004](docs/adr/0004-licensing.md) — MIT, no feature gating
- [ADR-0005](docs/adr/0005-delegate-to-operators.md) — Delegate stateful workloads
- [ADR-0006](docs/adr/0006-project-application-environment.md) — Project, Application, Environment
- [ADR-0007](docs/adr/0007-data-services.md) — Data services: presets, delegation, branching
- [ADR-0012](docs/adr/0012-flux-only-gitops.md) — Flux-only GitOps delivery; Argo CD deferred
- [ADR-0014](docs/adr/0014-components.md) — One `components` list, per-component identity

## Docs

[Vision](docs/vision.md) · [Architecture](docs/architecture.md) · [Competitive analysis](docs/competitive-analysis.md) · [Roadmap](docs/roadmap.md)

## Contributing

The useful contribution right now is argument. If one of the ADRs is wrong, say so in an issue while it's still cheap to change. Experience reports from people running Coolify, Dokploy, Kubero or Canine are worth more than feature requests. See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

MIT. Every feature. SSO, RBAC and audit logs won't be paywalled.

---

*A kelson is the beam fastened along the inside of a ship's keel. It reinforces the keel rather than replacing it.*
