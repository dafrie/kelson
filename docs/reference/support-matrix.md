<!-- GENERATED FILE — do not edit by hand. Regenerate with `go generate ./internal/clusterprofile/support/gendoc`, which reads `internal/clusterprofile/support/matrix.go` and rewrites this page. -->

# Support matrix

This page is **generated** from the declared support matrix in [`internal/clusterprofile/support/matrix.go`](https://github.com/dafrie/kelson/blob/main/internal/clusterprofile/support/matrix.go), the same data the version-skew checks enforce. Edit the matrix and regenerate; never edit this page by hand (issue #57).

kelson adopts components a cluster already has rather than installing its own (ADR-0003). Each component below has a **minimum supported version**: a cluster reporting a version at or above it is Supported, one below it is Unsupported, and a version a probe could not read is Unknown — reported for the caller to decide, never silently treated as fine.

## Supported components

| Component | Minimum supported | Too-old behaviour | Notes |
|-----------|-------------------|-------------------|-------|
| `kubernetes` | `1.27` | refuse with a reason | kelson renders stock Kubernetes objects that have been stable for years; below this floor the reasonable move is to refuse, because there is no older API to render. |
| `gateway-api` | `1.0.0` | refuse with a reason | the Gateway API CRDs kelson targets changed shape across experimental v0.x; rendering against a v0 cluster would need a second manifest family, so refuse for now. |
| `cert-manager` | `1.14.0` | refuse with a reason | cert-manager's ClusterIssuer and Certificate CRDs used by the Ingress TLS path stabilised with v1.14; older installs are refused with a message naming the required version. |
| `cnpg` | `1.23.0` | refuse with a reason | the CNPG Cluster API version kelson writes for database services is not emitted for older operators; the degradation is a refusal pending an older-API render path. |
| `flux` | `2.0.0` | refuse with a reason | flux v2's GitRepository/Kustomization API is what kelson commits against in GitOps mode. |
| `argo-cd` | `2.9.0` | refuse with a reason | the Application CRD kelson targets for the Argo CD delivery path. |
| `metrics-server` | `0.6.0` | refuse with a reason | metrics-server's aggregated API is stable well above this floor; the floor guards against installs old enough to lack the HPA autoscaling API kelson relies on. |
| `prometheus` | `0.66.0` | refuse with a reason | the prometheus-operator ServiceMonitor/PodMonitor CRD versions kelson emits. |
| `kyverno` | `1.10.0` | refuse with a reason | kyverno's policy API version that preview consults for admission reasons (issue #45). |
| `gatekeeper` | `3.13.0` | refuse with a reason | Gatekeeper's ConstraintTemplate/Constraint APIs preview reads for admission reasons. |

## Too-old behaviour

- **`refuse`** — a version below the minimum makes kelson refuse to render and say why, naming the component, the version found and the version required. Enforcement is the renderer's job and is not yet wired; the checks report the finding so a caller can act (issue #57).
- **`render-older`** — a version below the minimum is still usable but kelson must render the older API. No component is on this path yet; the decision is recorded here so a future one is explicit.
