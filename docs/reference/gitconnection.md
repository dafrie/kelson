<!-- GENERATED FILE — do not edit by hand. Regenerate with `go generate ./internal/specrefdoc`, which reads `schema/gitconnection.schema.json` and rewrites this page. -->

# GitConnection spec

This reference is **generated** from the committed JSON Schema [`schema/gitconnection.schema.json`](https://github.com/dafrie/kelson/blob/main/schema/gitconnection.schema.json), so it cannot drift from the model. Edit the Go types in `internal/model` and regenerate; never edit this page by hand. See [the model](../model.md) for the concepts and precedence rules (P1–P6) behind these fields.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `apiVersion` | string | yes |  |  |
| `kind` | string enum `"Project"`, `"Environment"`, `"GitConnection"` | yes |  |  |
| `metadata` | object | yes |  |  |
| `spec` | object | yes |  |  |

### `metadata`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `name` | string max length 63, pattern `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` | yes |  | DNS-1123 label |

### `spec`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `auth` | object | yes |  |  |
| `host` | string format uri | no |  | forge base URL such as https://github.com or https://git.acme.internal; defaults to https://github.com for provider github |
| `owner` | object | no |  |  |
| `provider` | string enum `"github"`, `"generic"` | yes |  | the forge this connection speaks to; the enum grows one value per adapter |

#### `spec.auth`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `githubApp` | object | no |  | provider github only; the per-instance GitHub App this connection acts as |
| `token` | object | no |  | a personal or project access token held in a Secret; the fallback every forge answers |

##### `spec.auth.githubApp`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `appID` | integer min 1 | yes |  | the GitHub App's numeric ID as the manifest flow reports it |
| `installationID` | integer min 0 | no |  | the installation this app acts as; 0 is the valid pre-install state before the installation webhook arrives |
| `secretRef` | string min length 1 | yes |  | name of the Secret holding the app private key and webhook secret; never a value |

##### `spec.auth.token`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `secretRef` | string min length 1 | yes |  | name of the Secret holding the token; never a value |

#### `spec.owner`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `kind` | string enum `"instance"`, `"user"`, `"team"` | yes |  | who may edit this connection; only instance is enforced today |
| `name` | string | no |  | the owning principal; required when kind is user or team and refused when it is instance |
