# kelson

A self-hosted PaaS that runs on your Kubernetes cluster and writes standard manifests instead of hiding them.

**Pre-alpha.** Nothing is implemented yet. The [roadmap](docs/roadmap.md) and [issues](https://github.com/dafrie/kelson/issues) are the current state of the project.

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
   ┌──────┼──────┐
   ▼      ▼      ▼
 direct  Flux  Argo CD
   └──────┼──────┘
          ▼
     your cluster
```

The renderer is a pure function, so the same input always produces the same bytes. That's what makes the three delivery modes one code path rather than three, and what makes previews worth trusting.

Direct mode is Git mode with an implicit repository. It still versions rendered output, so you keep diffs, history and rollback, and `kelson eject --to-git` replays that history into a real repo when you want it.

## What's different

**Uninstalling doesn't break anything.** Your apps keep running and you're left with a plain Kustomize repo. There's a CI test that proves it.

**You see what will happen first.** Three levels: a rendered diff, a server-side dry-run against the real API server, and an ephemeral live environment. The middle one is the API server's own answer, including admission webhooks, policy rejections and quota checks. Nothing else in this category surfaces it.

**It adopts what you already run.** kelson detects Gateway API, cert-manager, external-secrets, Prometheus, CloudNativePG, Flux and Argo CD, and renders to fit. It won't install a second ingress controller next to yours.

**Agents get guardrails, not just tools.** Every mutation supports dry-run. Errors are structured with remediation hints. Agents authenticate as themselves with scoped, expiring credentials, and per-environment policy decides what they can do unsupervised. In production the default is propose-only, which is the same pull-request path a human uses.

**It doesn't reimplement operators.** CloudNativePG for Postgres, Strimzi for Kafka, cert-manager for TLS, external-secrets for secrets. Kubero vendored Bitnami charts and broke working installs when the catalog was withdrawn.

## Decisions

- [ADR-0001](docs/adr/0001-hybrid-state-model.md) — One pure renderer, pluggable delivery
- [ADR-0002](docs/adr/0002-tech-stack.md) — Go control plane, TypeScript/React UI
- [ADR-0003](docs/adr/0003-install-model.md) — Adopt existing clusters, bootstrap empty ones
- [ADR-0004](docs/adr/0004-licensing.md) — MIT, no feature gating
- [ADR-0005](docs/adr/0005-delegate-to-operators.md) — Delegate stateful workloads
- [ADR-0006](docs/adr/0006-project-application-environment.md) — Project, Application, Environment

## Docs

[Vision](docs/vision.md) · [Architecture](docs/architecture.md) · [Competitive analysis](docs/competitive-analysis.md) · [Roadmap](docs/roadmap.md)

## Contributing

The useful contribution right now is argument. If one of the ADRs is wrong, say so in an issue while it's still cheap to change. Experience reports from people running Coolify, Dokploy, Kubero or Canine are worth more than feature requests. See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

MIT. Every feature. SSO, RBAC and audit logs won't be paywalled.

---

*A kelson is the beam fastened along the inside of a ship's keel. It reinforces the keel rather than replacing it.*
