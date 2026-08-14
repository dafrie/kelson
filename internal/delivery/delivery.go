// Package delivery is the DELIVERY plane (docs/architecture.md): the vocabulary
// the last mile is described in, and the packages that perform parts of it.
//
// # There is no adapter interface any more
//
// There was one, with a Registry and a per-environment Selector, because
// [ADR-0001](docs/adr/0001-hybrid-state-model.md) chose hybrid delivery and the
// seam was what kept two delivery modes from becoming two products (issue #32).
// [ADR-0028](docs/adr/0028-delivery-spine.md) decided there is one path —
// render, publish an OCI artifact, let Flux reconcile — and collapsed the seam
// with it: one implementation behind an interface is an interface describing
// nothing. `Adapter`, `Registry`, `Selector` and `Capabilities` are deleted
// (decision 9), and the reconciliation they used to select between is a
// controller loop (internal/controller).
//
// # The one rule that survives, because it is the one that carried the weight
//
// The renderer is a pure function of (spec, ClusterProfile) → manifests, and
// nothing downstream of it may influence a render. What this package still
// defines is the vocabulary of everything *after* that function returns: a
// [ManifestSet] is a rendered set with the provenance that makes status
// readback possible, [Phase] and [Status] are what the state machine
// (internal/delivery/statemachine) transitions between, and [Entry] is one
// element of an environment's history.
//
// Publishing a set is internal/preview's packaging (ADR-0028 decision 2, one
// publisher); reading Flux's state back is internal/delivery/flux; classifying
// live workloads is internal/observation.
package delivery

// Manifest is a single rendered Kubernetes resource. It mirrors the renderer's
// Manifest identity so the delivery plane never depends on renderer internals
// beyond its stable public surface.
type Manifest struct {
	APIVersion string
	Kind       string
	Name       string
	Namespace  string
	YAML       []byte
}

// ManifestSet is a rendered set with its identity: the output for one
// deployment, plus the provenance that makes status readback and history
// possible for a plane that does not own the apply step (issues #36, #37) —
// which, now that kustomize-controller owns it, is every plane kelson has.
type ManifestSet struct {
	// Project and Environment identify the application model this set renders.
	Project     string
	Environment string

	// SpecHash is the deterministic provenance hash stamped on every rendered
	// resource (kelson.dev/spec-hash).
	SpecHash string

	// Revision is the reconciler-visible revision id — the artifact tag of
	// ADR-0028 decision 2. It is carried by provenance annotations so the
	// observation plane can correlate without owning apply.
	Revision string

	// Manifests in apply order (namespaces first, workloads, CRDs before CRs).
	// The ordering is produced by the renderer and preserved verbatim.
	Manifests []Manifest
}

// Phase is the deployment state-machine phase (issue #37).
//
//	Proposed --[commit]--> Committed --[reconciling]--> Reconciling --[progressing]--> Applied --[health]--> Healthy
//	   |                         |                            |
//	   +--[rejected]------------> Rejected                    +--> Degraded
//
// The observers report this; the state machine owns the transitions. Failed
// transitions carry a Cause naming the responsible component and reason.
type Phase string

const (
	PhaseProposed    Phase = "Proposed"
	PhaseCommitted   Phase = "Committed"
	PhaseReconciling Phase = "Reconciling"
	PhaseApplied     Phase = "Applied"
	PhaseHealthy     Phase = "Healthy"
	PhaseRejected    Phase = "Rejected"
	PhaseDegraded    Phase = "Degraded"
)

// Status answers "is my change live?" (issue #37). Whatever produces one must
// distinguish "reconciler has not picked it up yet" from "reconciler rejected
// it" from "applied but unhealthy" — three very different user actions.
type Status struct {
	Phase    Phase  `json:"phase"`
	Revision string `json:"revision,omitempty"`
	// Cause names the responsible component and reason on every failure
	// transition (e.g. "flux: Kustomization ./path is NotReady:
	// health check failed"). Empty on success transitions.
	Cause string `json:"cause,omitempty"`
	// ObservedGeneration/counts are source-specific detail, kept opaque here.
	Detail map[string]string `json:"detail,omitempty"`
}

// Entry is one element of an environment's history (issue #38). Under
// ADR-0028 decision 4 the record is the registry's tag list and
// Environment.status mirrors a bounded window of it; this is the shape a caller
// reads either through.
type Entry struct {
	Revision string `json:"revision"`
	SpecHash string `json:"specHash"`
	// CommittedAt is the wall-clock time the entry was recorded. It is not
	// part of any rendered artifact — durability of the rendered output does
	// not depend on it.
	CommittedAt string `json:"committedAt,omitempty"`
	Message     string `json:"message,omitempty"`
	Author      string `json:"author,omitempty"`
}
