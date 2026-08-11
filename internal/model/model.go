// Package model defines the authoring-plane data model: Project, Application
// and Environment (ADR-0006, issues #24, #25). The types here are what users
// write and what agents generate; they are the shared contract consumed by
// the renderer.
//
// # Documents
//
// Two document kinds exist — Project and Environment — both carrying
// apiVersion kelson.dev/v1alpha1. A Project names its Applications inline
// (spec.applications); an Environment binds to a Project by name
// (spec.project) and carries everything that differs per deployment target:
// cluster, namespace, routing, delivery mode, policy, secret backend and
// per-Application overrides.
//
// # Design rules
//
//   - Thin on purpose: model what genuinely recurs, hand the rest to
//     overlays (always available, never required).
//   - One Application renders to one workload. The workload kind is derived:
//     port set → web service, schedule set → CronJob, neither → worker.
//     schedule and port are mutually exclusive.
//   - An Application belongs to exactly one Project, identified by the pair
//     (project, application).
//   - Secret values are never literals: environment values are plain strings
//     or {from: {service, key}} bindings (ADR-0009). Literal-looking secrets
//     are validation errors (secret/literal).
//   - The Project document is the versioned unit; Applications deploy
//     independently from any version.
//
// # Precedence (docs/model.md rules P1–P6)
//
// Environment variables merge key-by-key, innermost scope first:
// Project env < Application env < Environment per-Application override env.
// Per-Application replicas/resources, Environment service plan overrides and
// the Environment-scoped concerns (delivery, policy, secrets) are taken whole
// from the innermost scope that sets them; delivery/policy/secrets fall back
// to Project defaults, then to built-in defaults (direct, propose-only,
// cluster). Overlays concatenate Project-first.
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
