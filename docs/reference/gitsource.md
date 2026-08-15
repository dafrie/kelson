<!-- GENERATED FILE — do not edit by hand. Regenerate with `go generate ./internal/specrefdoc`, which reads `schema/gitsource.schema.json` and rewrites this page. -->

# GitSource spec

This reference is **generated** from the committed JSON Schema [`schema/gitsource.schema.json`](https://github.com/dafrie/kelson/blob/main/schema/gitsource.schema.json), so it cannot drift from the model. Edit the Go types in `internal/model` and regenerate; never edit this page by hand. See [the model](../model.md) for the concepts and precedence rules (P1–P6) behind these fields.

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
| `connection` | string | no |  | name of the GitConnection to authenticate with; resolved by host match against the instance's connections when omitted |
| `git` | string format uri | yes |  | git URL of the repository this source offers |
| `owner` | object | no |  |  |
| `ref` | string | no | `"main"` |  |

#### `spec.owner`

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `kind` | string enum `"instance"`, `"user"`, `"team"` | yes |  | who may edit this document; only instance is enforced today |
| `name` | string | no |  | the owning principal; required when kind is user or team and refused when it is instance |
