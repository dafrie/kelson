# kelson

A self-hosted PaaS that runs on your Kubernetes cluster and writes standard manifests instead of hiding them.

**Pre-alpha, and not usable yet.** `kelson render`, `kelson diff` (including server-side dry-run), `kelson eject` and `kelson profile` work today. The delivery adapters (direct and Flux), build drivers and observation layer are implemented and tested but not yet wired to a `deploy` command, and there is no server or UI. The [roadmap](roadmap.md) and the [issue tracker](https://github.com/dafrie/kelson/issues) are the current state of the project.

## Why Kubernetes

Every PaaS that isn't Kubernetes is a bet that you won't need something its authors didn't plan for. The bet fails the first time you need a sidecar, an operator, a network policy, or a second region. At that point you aren't extending the platform, you're leaving it.

Kubernetes works the same way on one node and on two hundred. What it lacks is a good front door, not capability.

kelson doesn't wrap Kubernetes in new concepts. It generates Kubernetes.

## What's different

- **Uninstalling doesn't break anything.** Your apps keep running and you're left with a plain Kustomize repo.
- **You see what will happen first.** Three preview levels: a rendered diff, a server-side dry-run, an ephemeral live environment.
- **It adopts what you already run.** Detects Gateway API, cert-manager, external-secrets, Prometheus, CloudNativePG and Flux, and renders to fit. Routing is Gateway API only.
- **Agents get guardrails, not just tools** *(designed; lands with M7)*. Every mutation supports dry-run; production defaults to propose-only.
- **It doesn't reimplement operators.** CloudNativePG, Strimzi, cert-manager, external-secrets.

## Docs

- [Vision](vision.md) — why this project exists
- [Architecture](architecture.md) — four planes, the renderer, delivery
- [Roadmap](roadmap.md) — four phases, seventeen milestones
- [Competitive analysis](competitive-analysis.md) — the field, and where kelson lands
- [ADR index](adr/README.md) — the load-bearing decisions

## License

MIT. Every feature. SSO, RBAC and audit logs won't be paywalled.

---

*A kelson is the beam fastened along the inside of a ship's keel. It reinforces the keel rather than replacing it.*
