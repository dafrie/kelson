// Package delivery is the DELIVERY plane (docs/architecture.md). It defines
// the pluggable adapter interface that keeps hybrid delivery from becoming two
// products (issue #32).
//
// # The one rule that matters
//
// Every adapter consumes IDENTICAL rendered manifests. The only difference
// between adapters is the last mile — who calls apply. Adapters MUST NOT
// influence rendering in any way: the renderer stays a pure function of
// (spec, ClusterProfile) → manifests, and a delivery Adapter only ever
// receives an already-rendered ManifestSet. Adding a fourth adapter requires
// no renderer changes (ADR-0001).
//
// New adapters implement Adapter and register themselves in the Registry.
package delivery

import (
	"context"
	"fmt"
)

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

// ManifestSet is what adapters consume: the rendered output for one
// deployment, plus the provenance that makes status readback and history
// possible across modes that do not own the apply step (issues #36, #37).
type ManifestSet struct {
	// Project and Environment identify the application model this set renders.
	Project     string
	Environment string

	// SpecHash is the deterministic provenance hash stamped on every rendered
	// resource (kelson.dev/spec-hash).
	SpecHash string

	// Revision is the reconciler-visible revision id (a git sha in Git modes,
	// a sequence id in direct mode). It is carried by provenance annotations
	// so the observation plane can correlate without owning apply.
	Revision string

	// Manifests in apply order (namespaces first, workloads, CRDs before CRs).
	// The ordering is produced by the renderer and preserved verbatim.
	Manifests []Manifest
}

// Result is the outcome of an apply or rollback.
type Result struct {
	// Revision now live in the target (git sha, artifact digest, or the
	// recorded store entry id for direct mode).
	Revision string
	// Applied is true when the adapter completed the last mile.
	Applied bool
}

// Capabilities declares what an adapter supports so callers can negotiate
// (issue #32): Git modes support pull requests, direct mode does not.
type Capabilities struct {
	// SupportsPR is true when the adapter can deliver through a pull request
	// (the git-writer's PR mode, issue #39). Direct mode cannot.
	SupportsPR bool
	// RequiresGit is true when the adapter delivers through a git repository.
	RequiresGit bool
	// SupportsRollback is true when the adapter can roll back a Deployment.
	SupportsRollback bool
}

// Phase is the deployment state-machine phase (issue #37).
//
//	Proposed --[commit]--> Committed --[reconciling]--> Reconciling --[progressing]--> Applied --[health]--> Healthy
//	   |                         |                            |
//	   +--[rejected]------------> Rejected                    +--> Degraded
//
// Adapters report this; the state machine owns the transitions. Failed
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

// Status answers "is my change live?" (issue #37). Adapters must distinguish
// "reconciler has not picked it up yet" from "reconciler rejected it" from
// "applied but unhealthy" — three very different user actions.
type Status struct {
	Phase    Phase  `json:"phase"`
	Revision string `json:"revision,omitempty"`
	// Cause names the responsible component and reason on every failure
	// transition (e.g. "flux: Kustomization ./path is NotReady:
	// health check failed"). Empty on success transitions.
	Cause string `json:"cause,omitempty"`
	// ObservedGeneration/counts are adapter-specific detail, kept opaque here.
	Detail map[string]string `json:"detail,omitempty"`
}

// Entry is one element of the rendered-history (issue #38), uniform across
// direct and Git modes from the caller's perspective.
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

// Adapter is the last mile of one delivery mode. It consumes a ManifestSet and
// reports status against the live system. Purity rule: an Adapter must never
// mutate the ManifestSet or re-render — rendering is the renderer's job.
type Adapter interface {
	// Name returns the adapter id, e.g. "direct", "flux", "argocd".
	Name() string

	// Capabilities declares what this adapter can and cannot do.
	Capabilities() Capabilities

	// Apply makes the rendered output live. In direct mode this is a
	// server-side apply; in Git modes it is a commit (+ optional PR) to the
	// deployment repository, followed by a reconciliation trigger.
	Apply(ctx context.Context, set ManifestSet) (Result, error)

	// Status reports the deployment state machine phase for the given set,
	// correlated via its Revision/SpecHash provenance (issue #37).
	Status(ctx context.Context, set ManifestSet) (Status, error)

	// History returns the recorded rendered-history for the project/environment
	// (issue #38), newest first.
	History(ctx context.Context, set ManifestSet) ([]Entry, error)

	// Rollback returns a Deployment to the given history entry (issue #38).
	Rollback(ctx context.Context, set ManifestSet, to Entry) (Result, error)
}

// Selector chooses an adapter for an environment by delivery mode. Keeping
// per-environment selection here means delivery mode stays a property of the
// Environment spec, never of the renderer.
type Selector func(mode string) (Adapter, error)

// Registry maps adapter names to adapters and provides per-mode selection.
// Adding a fourth adapter is: implement Adapter, register here.
type Registry struct {
	byName map[string]Adapter
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{byName: map[string]Adapter{}}
}

// Register records an adapter under its Name. A duplicate name is an error.
func (r *Registry) Register(a Adapter) error {
	if _, dup := r.byName[a.Name()]; dup {
		return fmt.Errorf("delivery: adapter %q already registered", a.Name())
	}
	r.byName[a.Name()] = a
	return nil
}

// Get returns an adapter by name.
func (r *Registry) Get(name string) (Adapter, error) {
	a, ok := r.byName[name]
	if !ok {
		return nil, fmt.Errorf("delivery: no adapter %q registered", name)
	}
	return a, nil
}

// Select resolves a delivery mode to an adapter. Direct mode selects the
// "direct" adapter; Git modes select the shared git adapter (flux/argocd) —
// the mode string is matched against adapter names exactly as resolved "direct",
// "flux", "argocd" (model.DeliveryMode).
func (r *Registry) Select(mode string) (Adapter, error) {
	return r.Get(mode)
}
