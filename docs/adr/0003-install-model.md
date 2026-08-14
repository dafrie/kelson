# ADR-0003: Adopt existing clusters, bootstrap empty ones

- **Status:** Accepted (revised 2026-08-11, supersedes the original "bootstrap is a later addition" framing; amended 2026-08-12 by the architecture review: routing is Gateway API only, so the "`Ingress` where it doesn't" clause in the Decision below is withdrawn — a cluster without Gateway API is a reported capability gap with an offer to install an implementation, never a silent fallback. Tracked in [#140](https://github.com/dafrie/kelson/issues/140); ingress-class *detection* stays as advisory data for the migration nudge, [#112](https://github.com/dafrie/kelson/issues/112).)
  (Amended 2026-08-14 by [ADR-0027](0027-crd-native-control-plane.md): registering kelson's CRDs is an install step, and it is shown there to be additive — an API group nobody else claims, no existing object touched. The "never install what is already there" rule is unchanged, and [ADR-0030](0030-flux-aio-install.md) applies it to Flux itself.)
- **Date:** 2026-08-11

## Context

kelson's premise is that Kubernetes is the substrate you never have to migrate off. Someone should be able
to start on it with nothing and stay on it at scale.

That premise cuts both ways on installation.

Someone already running Kubernetes has an ingress controller, cert-manager, external-secrets, a Prometheus
stack and probably Flux or Argo CD. Installing a parallel platform stack next to all of that is
unacceptable, and it is what every incumbent does — which is why none of them serve these users at all.

Someone starting from nothing has no cluster. If the answer is "learn Kubernetes first", the "start on it
from day zero" claim is empty and only the second half of the pitch is real.

The original version of this ADR treated the second case as a later concession to a different audience.
That was wrong. They are the same claim at two points in time.

## Decision

**Detection and adoption is the primary mechanism. Bootstrap is a thin front door onto it.**

kelson probes the cluster and records a `ClusterProfile`: Gateway API, ingress classes, cert-manager,
external-secrets, Prometheus, CloudNativePG, Flux, Argo CD, policy engines, metrics-server. The renderer
takes this profile as an input and emits resources that fit what is present. `HTTPRoute` where Gateway API
exists and `Ingress` where it doesn't. A `Certificate` where cert-manager is present rather than an ACME
client of our own. A `ServiceMonitor` only when something will read it.

**Rule: never install what is already there.**

Bootstrap provisions a cluster via k3s or Talos, installs the platform components that are missing, and
then runs the *same* additive installer. It is a front door, not a second path. If it ever forks into
separate code, the design has failed.

## Rationale

Adoption is what makes kelson usable by teams already running Kubernetes, which no competitor manages. It
also keeps the architecture honest: a renderer that must handle both "cert-manager present" and
"cert-manager absent" cannot quietly assume a kelson-owned world.

Treating bootstrap as a thin layer over that same mechanism is what keeps the cost proportionate. The
expensive version of bootstrap is owning cluster lifecycle — node upgrades, CNI problems, every broken-node
support request. The cheap version delegates provisioning to k3s or Talos and then reuses the installer
that already exists. Only the cheap version is in scope.

An empty cluster is just the degenerate `ClusterProfile` where everything is absent. Building detection
first means bootstrap is mostly a matter of filling in the gaps it reports, which is why the ordering is
detection first and bootstrap immediately after, rather than bootstrap much later.

## Consequences

**Positive.**
- Serves teams already running Kubernetes, which nothing else does.
- Serves users with nothing, without a separate product.
- Detection forces genuine flexibility into the renderer instead of one assumed environment.
- Bootstrap reuses the installer, so there is one code path regardless of starting point.

**Negative.**
- Harder to test: the renderer must be correct across a combinatorial space of cluster shapes. Mitigated by
  `ClusterProfile` being an explicit input, which makes those shapes cheap golden-test fixtures rather than
  real clusters.
- Adopting a component means inheriting its failure modes and version skew. Detection must record versions
  and degrade explicitly when something is too old, rather than rendering what silently won't work.
- Even the thin bootstrap adds real support burden. Users will hit node, storage and networking problems
  and will reasonably ask kelson about them. The boundary of what kelson supports needs stating plainly in
  the docs before this ships, not after.
- Delegating provisioning means inheriting k3s and Talos release cadences and their failure modes.

## Revisit when

Bootstrap support load becomes disproportionate to its adoption benefit, or a third provisioning target is
requested — at which point the delegation boundary should be re-examined rather than extended by default.
