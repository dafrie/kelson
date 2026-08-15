// Package model defines the authoring-plane data model: Project, Component
// and Environment (ADR-0006 as amended by ADR-0014, issues #24, #25). The
// types here are what users write and what agents generate; they are the
// shared contract consumed by the renderer.
//
// # Documents
//
// Three document kinds exist — Project, Environment and GitConnection — all
// carrying apiVersion kelson.dev/v1alpha1. A Project names its Components
// inline (spec.components); an Environment binds to a Project by name
// (spec.project) and carries everything that differs per deployment target:
// cluster, namespace, routing, delivery mode, policy, secret backend and
// per-Component overrides.
//
// GitConnection is the odd one out and deliberately so (ADR-0033): it is a
// control-plane document rather than an authoring one. Nothing about it is
// rendered and the renderer never sees one — it says which forge kelson can
// talk to and which Secret it talks with, and it is read by the planes that
// have cluster access. It lives here because what a kelson document *is* has
// exactly one home, and because validate.go is the one taxonomy every surface
// reports (ADR-0027 decisions 3 and 5).
//
// # Design rules
//
//   - Thin on purpose: model what genuinely recurs, hand the rest to
//     overlays (always available, never required).
//   - One list of Components, one closed set of kinds: service, worker, cron,
//     agent, postgres, valkey (ADR-0014) and helm (ADR-0016). Workload kinds
//     are derived from the shape — port set → web service, schedule set →
//     CronJob, neither → worker — and an explicit kind: is checked against the
//     enum and against the shape rather than overriding it silently. Data kinds
//     and helm are always explicit.
//   - No field is silently inert. A field that belongs to the other half of
//     the kind set — preset on a worker, port on a database, tools on
//     anything but an agent — is a validation error (issue #141).
//   - A Component belongs to exactly one Project, identified by the pair
//     (project, component).
//   - Secret values are never literals: an environment value is a plain string,
//     a {secret: <name>, key: <key>} reference (ADR-0018) or a
//     {from: {service, key}} binding (ADR-0009), and both mapping forms render
//     into the same secretKeyRef. Literal-looking secrets are validation errors
//     (secret/literal).
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
// secrets fall back to Project defaults, then to built-in defaults (direct
// delivery, no agent restrictions, cluster secrets). Overlays concatenate
// Project-first.
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

	KindProject       = "Project"
	KindEnvironment   = "Environment"
	KindGitConnection = "GitConnection"
)

// Kinds is every document kind, in the order error remediations list them.
var Kinds = []string{KindProject, KindEnvironment, KindGitConnection}

// TypeMeta carries apiVersion and kind for every document kind.
type TypeMeta struct {
	APIVersion string `yaml:"apiVersion" json:"apiVersion" jsonschema:"required"`
	Kind       string `yaml:"kind" json:"kind" jsonschema:"required,enum=Project,enum=Environment,enum=GitConnection"`
}

// ObjectMeta is the (deliberately minimal) shared metadata.
type ObjectMeta struct {
	Name string `yaml:"name" json:"name" jsonschema:"required,pattern=^[a-z0-9]([-a-z0-9]*[a-z0-9])?$,maxLength=63,description=DNS-1123 label"`
}
