<!-- GENERATED FILE — do not edit by hand. Regenerate with `go generate ./internal/specrefdoc`, which reads `schema/environment.schema.json` and rewrites this page. -->

# Environment spec

This reference is **generated** from the committed JSON Schema [`schema/environment.schema.json`](https://github.com/dafrie/kelson/blob/main/schema/environment.schema.json), so it cannot drift from the model. Edit the Go types in `internal/model` and regenerate; never edit this page by hand. See [the model](../model.md) for the concepts and precedence rules (P1–P6) behind these fields.

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
| `autoDeploy` | boolean | no | `false` | follow the components' sources — a push to a bound repository re-renders and republishes this environment (ADR-0036) |
| `cluster` | string | no |  |  |
| `components` | array of object | no |  |  |
| `delivery` | object | no |  |  |
| `namespace` | string | no |  |  |
| `overlays` | array of object | no |  |  |
| `policy` | object | no |  |  |
| `previews` | object | no |  |  |
| `project` | string | yes |  |  |
| `routing` | object | no |  |  |
| `secrets` | object | no |  |  |

#### `spec.components[]`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `autoDeploy` | boolean | no |  | workloads only; follow this component's source here — it overrides the environment's own setting (ADR-0036) |
| `env` | map of one of: string, object {from}, object {secret, key} | no |  |  |
| `image` | string | no |  | pins this component's image in this environment only; the promotion primitive (rule P3) |
| `imageTracked` | boolean | no |  | workloads only; the image named here is a starting point rather than a hold — tracking may still advance it and the trigger overwrites it on the next push (ADR-0036 decision 5) |
| `name` | string | yes |  |  |
| `preset` | string enum `"shared"`, `"small"`, `"ha-small"`, `"ha-medium"`, `"branch"` | no |  | data components only |
| `replicas` | object | no |  |  |
| `resources` | object | no |  |  |

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

#### `spec.delivery`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `git` | object | no |  |  |
| `mode` | string enum `"direct"`, `"flux"` | yes |  |  |

##### `spec.delivery.git`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `branch` | string | no | `"main"` |  |
| `path` | string | no |  | directory within the repo for rendered manifests |
| `repo` | string | yes |  | git URL of the deployment repository |

#### `spec.overlays[]`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `manifest` | string | no |  | path to an extra Kubernetes manifest |
| `patch` | string | no |  | YAML document merged into the resource it targets |

#### `spec.policy`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `agents` | string enum `"allow"`, `"propose-only"` | no | `"allow"` | what an agent may do here unsupervised; propose-only refuses every live mutation |
| `deployers` | array of string | no |  |  |
| `forbid` | array of string | no |  | agents only; operations refused to agents here — one of deploy / rollback / promote / build / secret-set / secret-delete / spec-write / spec-delete |
| `maxReplicas` | integer min 1 | no |  | agents only; the highest replica count an agent may deploy in this environment |
| `protect` | array of string | no |  | agents only; components an agent may not delete or scale to zero |
| `require` | array of string | no |  | guards that must hold before an agent deploy; only dry-run is defined |

#### `spec.previews`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `artifacts` | object | yes |  |  |
| `filter` | object | no |  |  |
| `interval` | string | no | `"10m"` | how often the forge is polled for change requests |
| `provider` | string enum `"github"`, `"gitlab"` | yes |  | the forge whose change requests become previews |
| `repo` | string | yes |  | HTTP(S) URL of the source repository whose change requests become previews; not delivery.git.repo |
| `secretRef` | string | no |  | name of the Secret holding forge credentials; never a token. Omit it to have kelson materialize one from the git connection covering previews.repo (ADR-0033) |
| `skip` | object | no |  |  |

##### `spec.previews.artifacts`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `repository` | string | yes |  | oci:// URL of the repository holding per-pull-request manifests; no tag |
| `secretRef` | string | no |  | name of a docker-registry Secret for a private artifact repository |

##### `spec.previews.filter`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `excludeBranch` | string | no |  | regular expression; matching branches are excluded |
| `includeBranch` | string | no |  | regular expression; only matching branches become previews |
| `labels` | array of string | no |  | only change requests carrying one of these labels become previews |
| `limit` | integer min 1, max 10000 | no | `10` | maximum number of simultaneous previews |

##### `spec.previews.skip`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `labels` | array of string | no |  | pause preview updates while one of these labels is present; a ! prefix inverts the test |

#### `spec.routing`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `domainSuffix` | string | no |  |  |
| `gatewayClass` | string | no |  |  |
| `tls` | boolean | no |  |  |

#### `spec.secrets`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `ageKeySecret` | string | no | `"sops-age"` | sops only; name of the Secret holding the age identity — created by the operator in the Kustomization's namespace |
| `ageRecipients` | array of string | no |  | sops only; age public keys (age1…) that encrypted secrets are readable by — required for backend sops |
| `backend` | string enum `"cluster"`, `"externalSecrets"`, `"sops"` | yes |  |  |
| `refreshInterval` | string | no | `"1h"` | externalSecrets only; how often the value is re-read from the backing store; a positive Go duration such as 30s or 15m or 1h |
| `store` | string | no |  | externalSecrets only; name of a SecretStore in this namespace or a ClusterSecretStore — optional when the cluster offers exactly one |
