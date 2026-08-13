<!-- GENERATED FILE — do not edit by hand. Regenerate with `go generate ./internal/clusterprofile/support/gendoc`, which reads `internal/clusterprofile/support/matrix.go` and rewrites this page. -->

# Support matrix

This page is **generated** from the declared support matrix in [`internal/clusterprofile/support/matrix.go`](https://github.com/dafrie/kelson/blob/main/internal/clusterprofile/support/matrix.go) and the storage clone-capability table in [`internal/clusterprofile/storage/drivers.go`](https://github.com/dafrie/kelson/blob/main/internal/clusterprofile/storage/drivers.go), the same data the checks enforce. Edit those and regenerate; never edit this page by hand (issues #57, #91).

kelson adopts components a cluster already has rather than installing its own (ADR-0003). Each component below has a **minimum supported version**: a cluster reporting a version at or above it is Supported, one below it is Unsupported, and a version a probe could not read is Unknown — reported for the caller to decide, never silently treated as fine.

## Supported components

| Component | Minimum supported | Too-old behaviour | Notes |
|-----------|-------------------|-------------------|-------|
| `kubernetes` | `1.27` | refuse with a reason | kelson renders stock Kubernetes objects that have been stable for years; below this floor the reasonable move is to refuse, because there is no older API to render. |
| `gateway-api` | `1.0.0` | refuse with a reason | the Gateway API CRDs kelson targets changed shape across experimental v0.x; rendering against a v0 cluster would need a second manifest family, so refuse for now. |
| `cert-manager` | `1.14.0` | refuse with a reason | cert-manager's ClusterIssuer and Certificate CRDs used by the TLS path stabilised with v1.14; older installs are refused with a message naming the required version. |
| `cnpg` | `1.23.0` | refuse with a reason | the CNPG Cluster API version kelson writes for database services is not emitted for older operators; the degradation is a refusal pending an older-API render path. kelson targets the latest CloudNativePG release: the declarative capabilities above this floor (managed roles from 1.20, the Database CRD from 1.25, schemas and extensions from 1.26) are reported per capability by internal/clusterprofile/postgres rather than refused here, so an older operator loses the features it cannot serve and keeps the ones it can (issue #90). |
| `valkey-operator` | `0.5.0` | refuse with a reason | the ValkeyCluster API kelson writes for cache components (ADR-0015). The floor is the release whose CRD shape kelson renders — spec.config for maxmemory and the eviction policy, spec.persistence, spec.podDisruptionBudget — and an older operator is refused rather than served a manifest it would silently drop fields from. The operator's API is still v1alpha1 and the floor is expected to move with it (issue #98). |
| `flux` | `2.0.0` | refuse with a reason | flux v2's GitRepository/Kustomization API is what kelson commits against in GitOps mode. |
| `argo-cd` | `2.9.0` | refuse with a reason | the Application CRD kelson targets for the Argo CD delivery path. |
| `metrics-server` | `0.6.0` | refuse with a reason | metrics-server's aggregated API is stable well above this floor; the floor guards against installs old enough to lack the HPA autoscaling API kelson relies on. |
| `prometheus` | `0.66.0` | refuse with a reason | the prometheus-operator ServiceMonitor/PodMonitor CRD versions kelson emits. |
| `kyverno` | `1.10.0` | refuse with a reason | kyverno's policy API version that preview consults for admission reasons (issue #45). |
| `gatekeeper` | `3.13.0` | refuse with a reason | Gatekeeper's ConstraintTemplate/Constraint APIs preview reads for admission reasons. |

## Too-old behaviour

- **`refuse`** — a version below the minimum makes kelson refuse to render and say why, naming the component, the version found and the version required. Enforcement is the renderer's job and is not yet wired; the checks report the finding so a caller can act (issue #57).
- **`render-older`** — a version below the minimum is still usable but kelson must render the older API. No component is on this path yet; the decision is recorded here so a future one is explicit.

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
