// Package v1alpha1 is the Kubernetes API surface of kelson: the Project and
// Environment custom resources of group kelson.dev (ADR-0027).
//
// # What is here, and what is deliberately not
//
// This package holds the CRD wrappers and nothing else: TypeMeta + ObjectMeta +
// Spec + Status for each kind, the list types, the status types, the scheme
// registration, and the generated deep copies. There is no validation here, no
// defaulting and no behaviour of any kind.
//
// The spec structs are *not* redefined here. `Spec` is typed as
// [github.com/dafrie/kelson/internal/model.ProjectSpec] and
// [github.com/dafrie/kelson/internal/model.EnvironmentSpec] directly, because
// internal/model is the single home of what a kelson spec is — its fields, its
// yaml/json tags and its validate.go (ADR-0027 decision 3). A second copy of
// those structs would be a second answer to "what may an author write", and the
// first time the two disagreed the disagreement would be silent.
//
// The split exists for a mechanical reason as well as a doctrinal one:
// internal/model is imported by the pure renderer and by the CLI, and the `main`
// depguard rule in .golangci.yml allows neither apimachinery nor
// controller-runtime there. Keeping the wrappers in their own package buys one
// new fence rule (`controller`) instead of a loosened `main`.
//
// # Why it is public
//
// A CRD's Go types are part of the contract other people's controllers consume:
// anyone writing a controller that reads a kelson Environment needs these
// structs, and they cannot import `internal/`. That makes this package a
// compatibility surface in a way nothing else outside cmd/ is —
// `v1alpha1` says what it says, but the boundary is maintained from the moment
// someone imports it.
//
// # The two kubebuilder markers
//
// `+kubebuilder:object:generate=true` below, and `+kubebuilder:object:root=true`
// on Project/ProjectList/Environment/EnvironmentList, drive exactly one thing:
// controller-gen's deepcopy generator, which produces zz_generated.deepcopy.go.
// They are not a schema pipeline. ADR-0027 decision 4 refuses marker-driven CRD
// generation — the openAPIV3Schema in deploy/crds/*.yaml comes from
// internal/schemagen reflecting over the same structs that produce
// schema/*.json, so there is one answer to "what is a valid kelson spec" and not
// two.
//
// +kubebuilder:object:generate=true
// +groupName=kelson.dev
package v1alpha1
