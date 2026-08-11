// Package renderer is the RENDERING plane (see docs/architecture.md).
//
// It is the pure function at the centre of kelson:
//
//	(spec, ClusterProfile) → manifests
//
// # Dependency rule (enforced by CI, issue #20)
//
// This package MUST remain deterministically pure. It must not import:
//
//   - any Kubernetes client (client-go, controller-runtime, dynamic clients)
//   - any HTTP or network client
//   - the standard library's `time` (no clock) or `os` (no ambient inputs)
//   - `math/rand` without an injected, deterministic source
//
// `ClusterProfile` is passed in as an input, never looked up. See
// ADR-0001 and issue #20. Adding a forbidden import fails CI with an
// explanation; an exception requires an ADR, not a //nolint.
package renderer

// Spec is the resolved authoring input to the renderer: one Project plus the
// selected Environment and ClusterProfile. Concrete types land with M1
// (issues #24-26).
type Spec struct{}

// Manifest is a single rendered Kubernetes resource.
type Manifest struct {
	APIVersion string            `json:"apiVersion" yaml:"apiVersion"`
	Kind       string            `json:"kind" yaml:"kind"`
	Name       string            `json:"name" yaml:"name"`
	Data       map[string]any    `json:"data" yaml:"data"`
	Annotations map[string]string `json:"annotations,omitempty" yaml:"annotations,omitempty"`
}

// Render returns the Kubernetes manifests for the given inputs. It is a pure
// function: no cluster, no network, no clock, no filesystem access.
func Render(spec Spec) []Manifest {
	return []Manifest{}
}
