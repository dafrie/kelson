<!-- GENERATED FILE — do not edit by hand. Regenerate with `go generate ./internal/specrefdoc`, which reads `schema/environment.schema.json` and rewrites this page. -->

# Environment spec

This reference is **generated** from the committed JSON Schema [`schema/environment.schema.json`](https://github.com/dafrie/kelson/blob/main/schema/environment.schema.json), so it cannot drift from the model. Edit the Go types in `internal/model` and regenerate; never edit this page by hand. See [the model](../model.md) for the concepts and precedence rules (P1–P6) behind these fields.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `apiVersion` | string | yes |  |  |
| `kind` | string enum `"Project"`, `"Environment"` | yes |  |  |
| `metadata` | object | yes |  |  |
| `spec` | object | yes |  |  |

### `metadata`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `name` | string max length 63, pattern `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` | yes |  | DNS-1123 label |

### `spec`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `applications` | array of object | no |  |  |
| `cluster` | string | no |  |  |
| `delivery` | object | no |  |  |
| `namespace` | string | no |  |  |
| `overlays` | array of object | no |  |  |
| `policy` | object | no |  |  |
| `project` | string | yes |  |  |
| `routing` | object | no |  |  |
| `secrets` | object | no |  |  |
| `services` | array of object | no |  |  |

#### `spec.applications[]`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `env` | map of one of: string, object | no |  |  |
| `name` | string | yes |  |  |
| `replicas` | object | no |  |  |
| `resources` | object | no |  |  |

##### `spec.applications[].replicas`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `max` | integer min 0 | no |  |  |
| `min` | integer min 0 | yes |  |  |

##### `spec.applications[].resources`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `limits` | object | no |  |  |
| `requests` | object | no |  |  |

###### `spec.applications[].resources.limits`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `cpu` | string | no |  | Kubernetes quantity |
| `memory` | string | no |  | Kubernetes quantity |

###### `spec.applications[].resources.requests`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `cpu` | string | no |  | Kubernetes quantity |
| `memory` | string | no |  | Kubernetes quantity |

#### `spec.delivery`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `git` | object | no |  |  |
| `mode` | string enum `"direct"`, `"flux"`, `"argocd"` | yes |  |  |

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
| `agents` | string enum `"allow"`, `"propose-only"` | no | `"propose-only"` |  |
| `deployers` | array of string | no |  |  |
| `require` | array of string | no |  | guards that must hold before deploy; only dry-run is defined |

#### `spec.routing`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `domainSuffix` | string | no |  |  |
| `gatewayClass` | string | no |  |  |
| `tls` | boolean | no |  |  |

#### `spec.secrets`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `backend` | string enum `"cluster"`, `"externalSecrets"`, `"sops"` | yes |  |  |
| `store` | string | no |  |  |

#### `spec.services[]`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `name` | string | yes |  |  |
| `preset` | string enum `"shared"`, `"small"`, `"ha-small"`, `"ha-medium"`, `"branch"` | yes |  |  |
