<!-- GENERATED FILE — do not edit by hand. Regenerate with `go generate ./internal/specrefdoc`, which reads `schema/project.schema.json` and rewrites this page. -->

# Project spec

This reference is **generated** from the committed JSON Schema [`schema/project.schema.json`](https://github.com/dafrie/kelson/blob/main/schema/project.schema.json), so it cannot drift from the model. Edit the Go types in `internal/model` and regenerate; never edit this page by hand. See [the model](../model.md) for the concepts and precedence rules (P1–P6) behind these fields.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `apiVersion` | string | yes |  |  |
| `kind` | string enum `"Project"`, `"Environment"`, `"GitConnection"`, `"GitSource"` | yes |  |  |
| `metadata` | object | yes |  |  |
| `spec` | object | yes |  |  |

### `metadata`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `name` | string max length 63, pattern `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` | yes |  | DNS-1123 label |

### `spec`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `build` | object | no |  |  |
| `components` | array of object · min 1 item(s) | yes |  |  |
| `defaults` | object | no |  |  |
| `env` | map of one of: string, object {from}, object {secret, key} | no |  |  |
| `image` | string | no |  | pre-built image reference |
| `overlays` | array of object | no |  |  |
| `source` | object | no |  |  |
| `sources` | array of object | no |  | repositories this Project declares for its components to build from; the singular source: is shorthand for one entry named default |

#### `spec.build`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `by` | string enum `"kelson"`, `"ci"` | no | `"kelson"` | who produces this project's images; kelson builds them in its own build plane and ci reports images its pipeline already built |
| `dockerfile` | string | no |  |  |
| `strategy` | string enum `"auto"`, `"dockerfile"`, `"buildpacks"`, `"none"` | no | `"auto"` |  |

#### `spec.components[]`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `auth` | object | no |  | valkey components only; names the Secret holding the cache password — kelson references it and never creates or reads it |
| `chart` | string | no |  | helm components only; the chart name within its source |
| `chartVersion` | string | no |  | helm components only; the exact chart version — required because an unpinned chart is not reproducible |
| `command` | array of string | no |  | container command; wins over the image default |
| `domains` | array of string | no |  |  |
| `env` | map of one of: string, object {from}, object {secret, key} | no |  |  |
| `health` | string | no |  | HTTP liveness/readiness path |
| `image` | string | no |  | overrides the Project image (rule P3) |
| `kind` | string enum `"service"`, `"worker"`, `"cron"`, `"agent"`, `"postgres"`, `"valkey"`, `"helm"` | no |  | derived from port/schedule when omitted; required for postgres and valkey and helm |
| `name` | string pattern `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` | yes |  |  |
| `port` | integer min 1, max 65535 | no |  |  |
| `preset` | string enum `"shared"`, `"small"`, `"ha-small"`, `"ha-medium"`, `"branch"` | no | `"shared"` | data components only |
| `release` | object | no |  | command run to completion before this revision's workloads roll; refused until issue #227 |
| `replicas` | object | no |  |  |
| `resources` | object | no |  |  |
| `schedule` | string | no |  | five-field cron expression |
| `source` | one of: string max length 63, pattern `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`, object {oci, repository} | no |  | a source name for a buildable component (ADR-0035) or the chart source of a helm component — exactly one of repository or oci (ADR-0016) |
| `tools` | array of string | no |  | agent components only; refused until issue #75 |
| `values` | object | no |  | helm components only; chart values rendered verbatim into the HelmRelease — plain configuration only and never secret material (put that in valuesFrom) |
| `valuesFrom` | array of object | no |  | helm components only; Secrets and ConfigMaps merged into the chart values by helm-controller |

##### `spec.components[].auth`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `key` | string | yes |  | key within that Secret |
| `secret` | string | yes |  | name of a Secret in the environment's namespace; kelson references it and never creates or reads it |

##### `spec.components[].release`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `command` | array of string · min 1 item(s) | yes |  | argv of the command; it runs with the component's image and environment |
| `timeout` | string | no | `"10m"` | Go duration such as 30m; the Job's activeDeadlineSeconds |

##### `spec.components[].replicas`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `max` | integer min 0 | no |  |  |
| `min` | integer min 0 | yes |  |  |

##### `spec.components[].resources`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `limits` | object | no |  |  |
| `requests` | object | no |  |  |

###### `spec.components[].resources.limits`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `cpu` | string | no |  | Kubernetes quantity |
| `memory` | string | no |  | Kubernetes quantity |

###### `spec.components[].resources.requests`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `cpu` | string | no |  | Kubernetes quantity |
| `memory` | string | no |  | Kubernetes quantity |

##### `spec.components[].source`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `oci` | string | no |  | OCI registry URL holding the chart — the registry path without the chart name (oci://ghcr.io/acme/charts) |
| `repository` | string format uri | no |  | classic Helm repository URL — the one serving index.yaml |

##### `spec.components[].valuesFrom[]`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `configMapRef` | string | no |  | name of a ConfigMap in the environment namespace |
| `secretRef` | string | no |  | name of a Secret in the environment namespace |

#### `spec.defaults`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `policy` | object | no |  |  |
| `secrets` | object | no |  |  |

##### `spec.defaults.policy`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `agents` | string enum `"allow"`, `"propose-only"` | no | `"allow"` | what an agent may do here unsupervised; propose-only refuses every live mutation |
| `deployers` | array of string | no |  |  |
| `forbid` | array of string | no |  | agents only; operations refused to agents here — one of deploy / rollback / promote / build / secret-set / secret-delete / spec-write / spec-delete |
| `maxReplicas` | integer min 1 | no |  | agents only; the highest replica count an agent may deploy in this environment |
| `protect` | array of string | no |  | agents only; components an agent may not delete or scale to zero |
| `require` | array of string | no |  | guards that must hold before an agent deploy; only dry-run is defined |

##### `spec.defaults.secrets`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `ageKeySecret` | string | no | `"sops-age"` | sops only; name of the Secret holding the age identity — created by the operator in the Kustomization's namespace |
| `ageRecipients` | array of string | no |  | sops only; age public keys (age1…) that encrypted secrets are readable by — required for backend sops |
| `backend` | string enum `"cluster"`, `"externalSecrets"`, `"sops"` | yes |  |  |
| `refreshInterval` | string | no | `"1h"` | externalSecrets only; how often the value is re-read from the backing store; a positive Go duration such as 30s or 15m or 1h |
| `store` | string | no |  | externalSecrets only; name of a SecretStore in this namespace or a ClusterSecretStore — optional when the cluster offers exactly one |

#### `spec.overlays[]`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `manifest` | string | no |  | path to an extra Kubernetes manifest |
| `patch` | string | no |  | YAML document merged into the resource it targets |

#### `spec.source`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `connection` | string | no |  | name of the GitConnection to authenticate with; resolved by host match against the instance's connections when omitted |
| `git` | string format uri | yes |  | git URL of the application source |
| `name` | string max length 63, pattern `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` | no |  | DNS-1123 label a component binds to; required in spec.sources and refused on the singular spec.source |
| `ref` | string | no | `"main"` |  |

#### `spec.sources[]`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `connection` | string | no |  | name of the GitConnection to authenticate with; resolved by host match against the instance's connections when omitted |
| `git` | string format uri | yes |  | git URL of the application source |
| `name` | string max length 63, pattern `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` | no |  | DNS-1123 label a component binds to; required in spec.sources and refused on the singular spec.source |
| `ref` | string | no | `"main"` |  |
