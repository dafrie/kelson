// Package model defines the authoring-plane data model: Project, Component
// and Environment (ADR-0006 as amended by ADR-0014, issues #24, #25). The
// types here are what users write and what agents generate; they are the
// shared contract consumed by the renderer.
//
// # Documents
//
// Two document kinds exist — Project and Environment — both carrying
// apiVersion kelson.dev/v1alpha1. A Project names its Components inline
// (spec.components); an Environment binds to a Project by name (spec.project)
// and carries everything that differs per deployment target: cluster,
// namespace, routing, delivery mode, policy, secret backend and per-Component
// overrides.
//
// # Design rules
//
//   - Thin on purpose: model what genuinely recurs, hand the rest to
//     overlays (always available, never required).
//   - One list of Components, one closed set of kinds: service, worker, cron,
//     agent, postgres, valkey (ADR-0014). Workload kinds are derived from the
//     shape — port set → web service, schedule set → CronJob, neither →
//     worker — and an explicit kind: is checked against the enum and against
//     the shape rather than overriding it silently. Data kinds are always
//     explicit.
//   - No field is silently inert. A field that belongs to the other half of
//     the kind set — preset on a worker, port on a database, tools on
//     anything but an agent — is a validation error (issue #141).
//   - A Component belongs to exactly one Project, identified by the pair
//     (project, component).
//   - Secret values are never literals: environment values are plain strings
//     or {from: {service, key}} bindings (ADR-0009). Literal-looking secrets
//     are validation errors (secret/literal).
//   - The Project document is the versioned unit; Components deploy
//     independently from any version.
//
// # Precedence (docs/model.md rules P1–P6)
//
// Environment variables merge key-by-key, innermost scope first:
// Project env < Component env < Environment per-Component override env. The
// image follows the same chain and is taken whole: an Environment's per-
// Component pin — the promotion primitive of ADR-0016 — beats a Component
// image, which beats the Project's.
// Per-Component replicas/resources, the Environment preset override on a data
// component and the Environment-scoped concerns (delivery, policy, secrets)
// are taken whole from the innermost scope that sets them; delivery/policy/
// secrets fall back to Project defaults, then to built-in defaults (direct,
// propose-only, cluster). Overlays concatenate Project-first.
//
// # Constraints
//
// This package must not import Kubernetes clients, network clients, or take
// ambient inputs: it stays importable by the pure renderer (issue #20).
package model

//go:generate go run ../schemagen -out ../../schema

const (
	// APIVersion is the only supported spec apiVersion.
	APIVersion = "kelson.dev/v1alpha1"

	KindProject     = "Project"
	KindEnvironment = "Environment"
)

// TypeMeta carries apiVersion and kind for both document kinds.
type TypeMeta struct {
	APIVersion string `yaml:"apiVersion" json:"apiVersion" jsonschema:"required"`
	Kind       string `yaml:"kind" json:"kind" jsonschema:"required,enum=Project,enum=Environment"`
}

// ObjectMeta is the (deliberately minimal) shared metadata.
type ObjectMeta struct {
	Name string `yaml:"name" json:"name" jsonschema:"required,pattern=^[a-z0-9]([-a-z0-9]*[a-z0-9])?$,maxLength=63,description=DNS-1123 label"`
}
