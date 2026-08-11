# ADR-0002: Go control plane, TypeScript/React UI

- **Status:** Accepted
- **Date:** 2026-08-11

## Context

kelson is a Kubernetes-native project that must interoperate deeply with CRDs, controllers, admission
webhooks, and the Flux and Argo CD ecosystems. It also needs a genuinely good web UI — live logs, diff
views, an application graph — because DX is the product.

Options considered: Go core with a React UI; Go end-to-end with templ/HTMX; TypeScript end-to-end
(Kubero's choice); Rust core with a TypeScript UI.

## Decision

**Go for the control plane, CLI and MCP server. TypeScript and React for the web UI.**

- Control plane and controller: Go with `controller-runtime` / kubebuilder
- CLI: Go, distributed as a single static binary
- MCP server: Go, a thin adapter over the API
- Web UI: TypeScript and React
- API transport: ConnectRPC — gRPC and HTTP/JSON from one schema, generating clients for CLI, UI and MCP

## Rationale

The entire Kubernetes ecosystem is Go. `client-go`, `controller-runtime`, `apimachinery`, server-side
apply, the Flux and Argo CD client libraries, Gateway API types and CloudNativePG types are all first-class
Go and second-class or absent everywhere else. Devtron, KubeVela, Radius, Kargo and kagent are all Go.
Choosing anything else means reimplementing or wrapping this surface indefinitely.

TypeScript end-to-end was rejected for exactly that reason — Kubero fights it today. Rust has good
libraries in `kube-rs` but a far smaller operator ecosystem and a steeper contributor barrier for a
project that needs outside help. Go-with-HTMX was tempting for the single-binary story, but the UI
ambitions here (live log streaming, rich diff rendering, an app graph) put a real ceiling on it.

ConnectRPC is chosen over plain REST or plain gRPC because it produces one schema with both gRPC and
HTTP/JSON, giving generated clients everywhere and browser support without a proxy. Since
[ADR-0001](0001-hybrid-state-model.md) requires the UI, CLI and MCP server to be true peers over one API,
a single generated schema is what makes that enforceable rather than aspirational.

## Consequences

**Positive.**
- Native access to every Kubernetes library and operator type kelson needs.
- Single static binary for CLI and server; small footprint, easy distribution.
- One schema generating clients for all four API consumers.
- A large contributor pool familiar with Go operator patterns.

**Negative.**
- Two languages means two toolchains, two test setups, two dependency-audit surfaces.
- Go's ergonomics for rendering YAML are mediocre; expect deliberate work on the renderer's internal
  representation rather than string templating.
- ConnectRPC is less universally familiar than REST. Mitigated by its HTTP/JSON transport, which means
  `curl` still works.
