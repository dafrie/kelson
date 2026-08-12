<!-- GENERATED FILE — do not edit by hand. Regenerate with `go generate ./internal/specrefdoc`, which reads `schema/project.schema.json` and rewrites this page. -->

# Project spec

This reference is **generated** from the committed JSON Schema [`schema/project.schema.json`](https://github.com/dafrie/kelson/blob/main/schema/project.schema.json), so it cannot drift from the model. Edit the Go types in `internal/model` and regenerate; never edit this page by hand. See [the model](../model.md) for the concepts and precedence rules (P1–P6) behind these fields.

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
| `applications` | array of object · min 1 item(s) | yes |  |  |
| `build` | object | no |  |  |
| `defaults` | object | no |  |  |
| `env` | map of one of: string, object | no |  |  |
| `image` | string | no |  | pre-built image reference |
| `overlays` | array of object | no |  |  |
| `services` | array of object | no |  | data services this project's applications bind to |
| `source` | object | no |  |  |

#### `spec.applications[]`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `command` | array of string | no |  | container command; wins over the image default |
| `domains` | array of string | no |  |  |
| `env` | map of one of: string, object | no |  |  |
| `health` | string | no |  | HTTP liveness/readiness path |
| `image` | string | no |  | overrides the Project image (rule P3) |
| `name` | string pattern `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` | yes |  |  |
| `port` | integer min 1, max 65535 | no |  |  |
| `replicas` | object | no |  |  |
| `resources` | object | no |  |  |
| `schedule` | string | no |  | five-field cron expression |

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

#### `spec.build`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `dockerfile` | string | no |  |  |
| `strategy` | string enum `"auto"`, `"dockerfile"`, `"buildpacks"`, `"none"` | no | `"auto"` |  |

#### `spec.defaults`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `deliveryMode` | string | no |  |  |
| `policy` | object | no |  |  |
| `secrets` | object | no |  |  |

##### `spec.defaults.policy`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `agents` | string enum `"allow"`, `"propose-only"` | no | `"propose-only"` |  |
| `deployers` | array of string | no |  |  |
| `require` | array of string | no |  | guards that must hold before deploy; only dry-run is defined |

##### `spec.defaults.secrets`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `backend` | string enum `"cluster"`, `"externalSecrets"`, `"sops"` | yes |  |  |
| `store` | string | no |  |  |

#### `spec.overlays[]`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `manifest` | string | no |  | path to an extra Kubernetes manifest |
| `patch` | string | no |  | YAML document merged into the resource it targets |

#### `spec.services[]`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `name` | string | yes |  |  |
| `plan` | string enum `"shared"`, `"small"`, `"ha-small"`, `"ha-medium"`, `"branch"` | no | `"shared"` |  |
| `type` | string enum `"postgres"`, `"valkey"` | yes |  |  |

#### `spec.source`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `git` | string format uri | yes |  | git URL of the application source |
| `ref` | string | no | `"main"` |  |
