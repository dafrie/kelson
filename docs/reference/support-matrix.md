<!-- GENERATED FILE — do not edit by hand. Regenerate with `go generate ./internal/clusterprofile/support/gendoc`, which reads `internal/clusterprofile/support/matrix.go` and rewrites this page. -->

# Support matrix

This page is **generated** from the declared support matrix in [`internal/clusterprofile/support/matrix.go`](https://github.com/dafrie/kelson/blob/main/internal/clusterprofile/support/matrix.go) and the storage clone-capability table in [`internal/clusterprofile/storage/drivers.go`](https://github.com/dafrie/kelson/blob/main/internal/clusterprofile/storage/drivers.go), the same data the checks enforce. Edit those and regenerate; never edit this page by hand (issues #57, #91).

kelson adopts components a cluster already has rather than installing its own (ADR-0003). Each component below has a **minimum supported version**: a cluster reporting a version at or above it is Supported, one below it is Unsupported, and a version a probe could not read is Unknown — reported for the caller to decide, never silently treated as fine.

The **What degrades** column is the part to read first. A version number on its own sends you to the source; knowing that a too-old operator means every `kind: postgres` component refuses to render is a decision you can make. `kelson profile` prints these same statements for the cluster in front of you, and so does every command that resolves a live profile — see [detection](../detection.md#version-skew-and-explicit-degradation).

## Supported components

| Component | Minimum supported | Tested up to | Below the floor | What degrades |
|-----------|-------------------|--------------|-----------------|---------------|
| `kubernetes` | `1.26` | `1.34` | refuse with a reason | everything kelson renders and applies — the workload, routing, autoscaling and delivery manifests all target APIs this floor is derived from |
| `gateway-api` | `1.0.0` | — | refuse with a reason | all HTTP routing: the HTTPRoute kelson renders for every service that declares domains, and the Gateway it attaches to |
| `cert-manager` | `1.14.0` | — | refuse with a reason | TLS on routed services: the cert-manager.io/v1 Certificate kelson renders when routing.tls is set and the cluster reports a ClusterIssuer |
| `cnpg` | `1.23.0` | — | refuse with a reason | every `kind: postgres` component: the preset judgement in internal/clusterprofile/postgres refuses to render a database this operator cannot serve |
| `valkey-operator` | `0.5.0` | — | refuse with a reason | every `kind: valkey` component: the preset judgement in internal/clusterprofile/valkey refuses to render a cache this operator cannot serve |
| `flux` | `2.0.0` | — | refuse with a reason | the delivery spine: the OCIRepository and Kustomization the kelson controller writes for every environment, and the chart source a HelmRelease fetches from |
| `helm-controller` | `1.0.0` | — | refuse with a reason | every `kind: helm` component: the helm.toolkit.fluxcd.io/v2 HelmRelease kelson renders is rejected by a controller that serves only v2beta1/v2beta2 |
| `argo-cd` | `2.9.0` | — | refuse with a reason | the Argo CD delivery path. None ships (issue #138) and the spine is Flux-only (ADR-0028), so nothing degrades yet — the floor is recorded so detection can say what it saw |
| `metrics-server` | `0.6.0` | — | refuse with a reason | autoscaling: the autoscaling/v2 HorizontalPodAutoscaler kelson renders for a component with a replica range has no metrics source to scale on |
| `prometheus` | `0.66.0` | — | refuse with a reason | scrape configuration: the monitoring.coreos.com/v1 ServiceMonitor kelson renders per service |
| `kyverno` | `1.10.0` | — | refuse with a reason | preview's admission explanations: a rejected resource is still reported, but without naming the policy that rejected it (issue #45) |
| `gatekeeper` | `3.13.0` | — | refuse with a reason | preview's admission explanations: a rejected resource is still reported, but without naming the constraint that rejected it (issue #45) |

## Below the floor

- **`refuse`** — a version below the minimum makes kelson refuse to render and say why, naming the component, the version found, the version required and what degrades. The per-capability refusals for data and chart components are enforced in the renderer; the rest is reported so a caller can act before apply time (issue #57).
- **`render-older`** — a version below the minimum is still usable but kelson must render the older API. No component is on this path yet; the decision is recorded here so a future one is explicit.

## Newer than tested

A version **above** the tested column is never a refusal. kelson knows only that it has not exercised that release — not that anything is wrong with it — and refusing on that basis would break working clusters to prevent a hypothetical. It is reported as a `[note]`, so that if something does behave oddly there, the fact that you are outside the tested range is already on screen instead of being a mystery. A blank tested column claims no upper bound at all.

## Storage clone capability

Database branching snapshots a Postgres cluster's volume, and what that costs depends entirely on the CSI driver behind the storage class (ADR-0007). `kelson profile` reports it per storage class as `cloneCapability`, with a `cloneConfidence` saying how the answer was reached.

| Capability | What branching does | Cost |
|------------|---------------------|------|
| `thin` | copy-on-write clone of the volume | seconds, almost no extra space |
| `full-copy` | snapshot restored into a full-size volume | time and space proportional to the database |
| `none` | no CSI snapshot exists; restore from a backup instead | not available yet — the destination is issue #94 and the restore is #100 |
| `unknown` | snapshots exist but the driver is unrecognised, or storage could not be read | plan for a full copy until confirmed |

### Drivers kelson recognises

This table is maintained by driver name rather than probed: measuring a driver would mean provisioning and snapshotting a volume during what is a read-only capability probe. A driver that is not listed is reported as `unknown` with `cloneConfidence: unknown-driver` — never guessed in either direction.

| Driver | Clone capability |
|--------|------------------|
| `disk.csi.azure.com` | `full-copy` |
| `dobs.csi.digitalocean.com` | `full-copy` |
| `ebs.csi.aws.com` | `full-copy` |
| `kubernetes.io/no-provisioner` | `none` |
| `linodebs.csi.linode.com` | `full-copy` |
| `local.csi.openebs.io` | `thin` |
| `openebs.io/local` | `none` |
| `pd.csi.storage.gke.io` | `full-copy` |
| `rancher.io/local-path` | `none` |
| `rbd.csi.ceph.com` | `thin` |
| `topolvm.cybozu.com` | `thin` |
| `topolvm.io` | `thin` |
| `zfs.csi.openebs.io` | `thin` |

### Confidence

- **`observed`** — no snapshot class serves the provisioner, so nothing can clone it. Read from cluster state, not from the table.
- **`known-driver`** — the driver is in the table above.
- **`unknown-driver`** — snapshots exist, the driver is not in the table, so the cost is unknown.
- **`unreadable`** — the probe could not list snapshot classes; the profile's `incomplete` entry names the permission that would settle it.

## Why these floors

Each floor is a decision, not a default. Move one by editing its row in `internal/clusterprofile/support/matrix.go` and regenerating this page.

- **`kubernetes` 1.26** — the floor is derived from the APIs kelson actually emits and calls, not from upstream's own support window: batch/v1 CronJob is GA from 1.21, server-side apply — how every delivery adapter writes — from 1.22, and autoscaling/v2 HorizontalPodAutoscaler from 1.23. The binding constraint is Gateway API v1: kelson renders gateway.networking.k8s.io/v1 exclusively (issue #140) and the Gateway API releases that serve it require Kubernetes 1.26 or newer. Below the floor there is no older API to render, so the degradation is a refusal. Kelson does not refuse a cluster merely because upstream stopped patching it: that is the operator's risk to take, and a floor that tracked the upstream window would refuse most of the clusters kelson exists to be adopted by. The ceiling is the other half of the same honesty — Tested is the version the e2e harness runs (hack/e2e/lib.sh pins a kindest/node image) and the Kubernetes client libraries are one minor newer again; a cluster above it is reported as a note, because kelson has not seen that release, not because it is known to be broken.
- **`gateway-api` 1.0.0** — the Gateway API CRDs kelson targets changed shape across experimental v0.x; rendering against a v0 cluster would need a second manifest family, so refuse for now.
- **`cert-manager` 1.14.0** — cert-manager's ClusterIssuer and Certificate CRDs used by the TLS path stabilised with v1.14; older installs are refused with a message naming the required version.
- **`cnpg` 1.23.0** — the CNPG Cluster API version kelson writes for database services is not emitted for older operators; the degradation is a refusal pending an older-API render path. kelson targets the latest CloudNativePG release: the declarative capabilities above this floor (managed roles from 1.20, the Database CRD from 1.25, schemas and extensions from 1.26) are reported per capability by internal/clusterprofile/postgres rather than refused here, so an older operator loses the features it cannot serve and keeps the ones it can (issue #90).
- **`valkey-operator` 0.5.0** — the ValkeyCluster API kelson writes for cache components (ADR-0015). The floor is the release whose CRD shape kelson renders — spec.config for maxmemory and the eviction policy, spec.persistence, spec.podDisruptionBudget — and an older operator is refused rather than served a manifest it would silently drop fields from. The operator's API is still v1alpha1 and the floor is expected to move with it (issue #98).
- **`flux` 2.0.0** — flux v2's Kustomization/OCIRepository API is what the kelson controller writes (ADR-0028).
- **`helm-controller` 1.0.0** — the HelmRelease API kelson writes for `kind: helm` components is helm.toolkit.fluxcd.io/v2, which helm-controller serves as stable from 1.0.0 (Flux 2.3). A controller below that floor serves v2beta1/v2beta2 instead, and the difference is not cosmetic — v2 moved the chart reference and the drift-detection settings — so kelson refuses rather than emitting a manifest the controller would reject. It is a separate row from `flux` because a FluxInstance may install a components subset and leave helm-controller out entirely (ADR-0016, issue #60).
- **`argo-cd` 2.9.0** — the Application CRD kelson targets for the Argo CD delivery path.
- **`metrics-server` 0.6.0** — metrics-server's aggregated API is stable well above this floor; the floor guards against installs old enough to lack the HPA autoscaling API kelson relies on.
- **`prometheus` 0.66.0** — the prometheus-operator ServiceMonitor/PodMonitor CRD versions kelson emits.
- **`kyverno` 1.10.0** — kyverno's policy API version that preview consults for admission reasons (issue #45).
- **`gatekeeper` 3.13.0** — Gatekeeper's ConstraintTemplate/Constraint APIs preview reads for admission reasons.
