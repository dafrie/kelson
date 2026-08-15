// Package controller is the reconciliation plane: the controllers behind
// kelson's two custom resources (ADR-0027, ADR-0028).
//
// # What a reconcile does
//
// ADR-0028 decision 1's six steps, in [EnvironmentReconciler.Reconcile]:
//
//  1. validate — internal/model/validate.go, the same function the CLI and the
//     server call. An invalid document is a *status*, never an error return.
//  2. detect   — behind [ProfileSource]. One start-up detection, served
//     forever by [StaticProfileSource]; live refresh is later work.
//  3. resolve and render — internal/model's resolver and the pure renderer,
//     unchanged and still pure: this package is the caller that has cluster
//     access, the renderer still has none.
//  4. publish — the rendered set as an immutable OCI artifact
//     (internal/artifact, the publisher previews already use).
//  5. ensure — server-side apply the OCIRepository and Kustomization pair.
//  6. observe — read the Kustomization's condition back and map it onto a
//     phase with internal/delivery/flux's own mapping.
//
// Steps 4 to 6 are behind [Deliverer], implemented by [FluxDeliverer]. The seam
// is the boundary between "decides what to publish", which is a pure function
// of the spec and the status, and "talks to a registry and an API server".
//
// # Three things this package is careful about
//
//   - **The state machine is a table here, not an engine.**
//     internal/delivery/statemachine has an Engine that blocks until a
//     deployment settles, and calling it inside Reconcile would hold a worker
//     for the length of a rollout. What is used is Validate — the transition
//     table — once per observation.
//   - **Every refusal has exactly one requeue behaviour**, decided by its
//     reason and not by its call site (errors.go). FluxNotInstalled in
//     particular never returns an error, because a cluster with no Flux is the
//     expected state of a fresh install and a crash-looping controller cannot
//     tell anybody so.
//   - **Deleting an Environment deletes its workloads.** The finalizer removes
//     the Kustomization, which prunes what it applied. See
//     [EnvironmentReconciler.finalize] for what is deliberately *not* removed.
//
// # An invalid spec is never an error return
//
// This is the load-bearing rule of the whole package. Returning an error from
// Reconcile makes controller-runtime requeue with backoff, and a document that
// will never become valid on its own would be re-validated forever, at an
// increasing but never-zero rate, producing an identical failure each time. So
// a validation failure sets `Ready=False`, `reason: SpecInvalid` and
// `status.validationErrors[]`, and returns nil: nothing more will happen until
// somebody edits the document, and editing it bumps `.metadata.generation`,
// which is a watch event.
//
// Errors *are* returned for the things a retry can fix — an API server that
// refused a status write, a profile source that could not read the cluster —
// because those are exactly the cases where waiting and trying again is the
// right behaviour.
package controller

import (
	"context"
	"errors"
	"strconv"
	"sync"

	"github.com/dafrie/kelson/api/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/clusterprofile"
	"github.com/dafrie/kelson/internal/model"
	"github.com/dafrie/kelson/internal/renderer"
)

// ProfileSource supplies the ClusterProfile the renderer is told about
// (ADR-0028 decision 1, step 2).
//
// It is an interface and not a call to internal/clusterprofile/detect because
// detection is a cluster round trip with its own caching, RBAC and failure
// modes, and a reconciler that called it inline would probe the cluster once
// per reconcile of every environment. The wiring — one detection, cached,
// refreshed on a timer or on a CRD registration — is deliberately later work;
// what this package needs now is the seam, so the renderer's input has one
// named source rather than several call sites.
type ProfileSource interface {
	Profile(ctx context.Context) (clusterprofile.ClusterProfile, error)
}

// GitSourceLister reads the repositories the instance offers to every project —
// the GitSources of ADR-0035 decision 2, the global tier a component may bind
// to by name.
//
// It is a seam rather than a client call for the same reason [ProfileSource]
// is: a reconciler that decided *which namespace* to list from at the call site
// would make that decision three times. The binary makes it once
// (cmd/kelson-controller wires kelson's own namespace, where the chart installs
// the GitSources beside the connections) and hands the result here.
//
// It is spelled exactly as internal/api's lister of the same name, over the
// same [model.Source] shape, because the two planes must resolve the same
// binding to the same repository: a component that builds from a GitSource in
// `kelson build` and fails to deploy with `ref/unknown-source` would be one
// join written twice.
//
// A nil one is a controller with no global tier, which resolves every project
// against its own declared sources alone. That is the pre-ADR-0035 posture and
// stays correct for a project that declares what it builds from — but it is not
// the same thing as a listing that *failed*, which is a refusal
// ([EnvironmentReconciler.globalSources]).
type GitSourceLister interface {
	// ListSources returns every GitSource the instance offers, already in the
	// shape [model.Resolve] takes (model.GitSource.AsSource).
	ListSources(ctx context.Context) ([]model.Source, error)
}

// StaticProfileSource serves one profile, forever: the one
// cmd/kelson-controller detected at start-up.
//
// It is static rather than refreshed because the same finding decides whether
// the Flux watches are registered at all, and a watch cannot be added to a
// running manager — so a profile that changed under the process would be a
// profile half the code had acted on. Live refresh is tracked separately; the
// stated cost is that installing Flux under a running controller needs a
// restart, which is said out loud on stdout.
//
// An empty profile is a real answer, not a missing one: the renderer treats
// "absent" as "do not emit the optional resource" rather than as an error
// (internal/clusterprofile's tri-state discipline).
type StaticProfileSource struct {
	ClusterProfile clusterprofile.ClusterProfile
}

// Profile returns the configured profile.
func (s StaticProfileSource) Profile(context.Context) (clusterprofile.ClusterProfile, error) {
	return s.ClusterProfile, nil
}

// ProbedProfileSource is [StaticProfileSource] for the case the static one
// cannot represent: a start-up probe that *failed*.
//
// Detection failing and detection finding nothing produce the same empty
// profile and mean opposite things. Serving the empty one would make every
// environment in the cluster report FluxNotInstalled — a fix, `kelson install`,
// for a cluster that may already have Flux and merely refused the probe — and
// nothing would ever revisit it, because [StaticProfileSource] serves one answer
// forever. That is a controller wedged in a wrong answer by a transient failure.
//
// So a failed probe is an *error state*: [ProbedProfileSource.Profile] returns
// the error, which the reconciler reports under ReasonClusterProfileUnavailable
// and requeues with backoff, and the next call probes again. The first probe to
// succeed is latched and served from then on, which keeps the property
// StaticProfileSource exists for — one detection per process, not one per
// reconcile — and keeps this a fix for the failure case rather than the live
// refresh in issue #133 (which is what would let the *watches* appear without a
// restart).
type ProbedProfileSource struct {
	// Probe reads the cluster. Required; nil probes nothing and says so.
	Probe func(context.Context) (clusterprofile.ClusterProfile, error)

	mu      sync.Mutex
	profile clusterprofile.ClusterProfile
	latched bool
}

// Profile serves the latched profile, or probes again.
func (s *ProbedProfileSource) Profile(ctx context.Context) (clusterprofile.ClusterProfile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.latched {
		return s.profile, nil
	}
	if s.Probe == nil {
		return clusterprofile.ClusterProfile{}, errors.New("no cluster probe is configured")
	}
	profile, err := s.Probe(ctx)
	if err != nil {
		return clusterprofile.ClusterProfile{}, err
	}
	s.profile, s.latched = profile, true
	return profile, nil
}

// Latched reports whether a probe has succeeded. It is what a caller that
// started in the error state — cmd/kelson-controller's banner — uses to say
// which posture the process is in without probing a second time.
func (s *ProbedProfileSource) Latched() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.latched
}

// Revision is one candidate: everything steps 4 to 6 need about an Environment,
// and nothing about the custom resource it came from.
//
// The reconciler assembles it and a [Deliverer] consumes it, which is what
// keeps the two halves separable: deciding *what* to publish is a pure function
// of the spec and the status, and publishing it is I/O against a registry and
// an API server.
type Revision struct {
	// Project and Environment name the pair, and are the artifact's repository
	// path: <registry>/kelson/<project>-<environment>.
	Project     string
	Environment string

	// EnvironmentNamespace is where the *custom resource* lives. It is the
	// value of the kelson.dev/environment-namespace label on both Flux objects,
	// which is what a watch event maps back through and what the name-conflict
	// check compares — so it is the CR's namespace and never the workload's.
	EnvironmentNamespace string

	// TargetNamespace is where the workloads land: the resolved namespace,
	// which defaults to <project>-<environment> and which the Kustomization
	// applies into. It is carried separately from Resolved because the Flux
	// objects need it on a rollback too, where nothing re-renders.
	TargetNamespace string

	// Generation is the Environment's .metadata.generation — the revision
	// number, allocated by the API server rather than by kelson (ADR-0028
	// decision 2).
	Generation int64

	// SpecHash is the hash of the resolved spec (model.SpecHash). Its leading
	// eight characters are the second half of the artifact tag.
	SpecHash string

	// Manifests are the rendered set, in render order. Empty under a rollback,
	// where step 3 does not run.
	Manifests []renderer.Manifest

	// Resolved is the spec the manifests came from: the namespace, the secret
	// backend and the resolved images.
	Resolved *model.Resolved

	// FluxPresent is the ClusterProfile's finding. False means there is nothing
	// in the cluster that would reconcile what kelson published, which is a
	// status and a timer rather than a failure (ADR-0030).
	FluxPresent bool

	// Observed is status.revision: the tag a previous reconcile published.
	// ObservedDigest is that artifact's digest, carried so a skipped publish
	// does not lose it from the history.
	Observed       string
	ObservedDigest string

	// PinnedTo is the immutable tag a rollback pins the pair to. Non-empty
	// means steps 3 and 4 did not run and must not (ADR-0028 decision 5); the
	// caller has already verified the target against status.history.
	PinnedTo string

	// PinnedDigest is that target's digest, out of the same history entry the
	// verification found it in. It is what lets a rollback pin the pair to the
	// bytes rather than only to the tag pointing at them; empty when the entry
	// predates digests being recorded, which is a tag-only pin.
	PinnedDigest string
}

// Outcome is what a Deliverer reports back into the Environment's status.
type Outcome struct {
	// Revision is the artifact tag that is now serving, e.g. "7-1a2b3c4d".
	// Empty means nothing was published.
	Revision string

	// Digest is that artifact's OCI digest, when this reconcile knows it.
	Digest string

	// Phase is the delivery state-machine phase (the v1alpha1.Phase*
	// constants). Empty means the phase is unchanged.
	Phase string

	// Cause is the phase's explanation, when it has one: the sentence
	// internal/delivery/flux produced from the Kustomization's own condition.
	Cause string

	// Published is true when this reconcile pushed a tag that was not already
	// the settled one. It is what makes a history entry — a re-observation of
	// an unchanged revision must not create one.
	Published bool

	// RolledBack is true when the pair was pinned to a rollback target rather
	// than to a freshly published revision.
	RolledBack bool

	// Images are what this revision resolved to, one entry per component that
	// resolved one, in component order. The component name is carried rather
	// than only the image because it is exact here — the resolved spec is in
	// hand — and can only ever be guessed at downstream.
	Images []v1alpha1.ComponentImage

	// Workloads is the observation-plane readback: which workloads this
	// revision's Kustomization applied, and what they are doing (issue #240).
	//
	// Nil means this reconcile did not look — no [WorkloadObserver] is wired,
	// or the Kustomization itself was not readable, so there was nothing whose
	// workloads could be read back. It is written into the status wholesale,
	// nil included, because a readback kept from a previous reconcile is a
	// status that mixes two moments in time.
	Workloads *v1alpha1.WorkloadsStatus
}

// Deliverer performs ADR-0028's steps 4, 5 and 6: push the rendered set as an
// immutable OCI artifact, server-side apply the OCIRepository and Kustomization
// pair that consume it, and read the result back.
//
// The production implementation is [FluxDeliverer]. The interface exists
// because the reconciler above it — validation, rollback, history, the
// finalizer — is worth testing without a registry, and because "decides what to
// publish" and "talks to the network" is a boundary worth being able to see.
type Deliverer interface {
	Deliver(ctx context.Context, rev Revision) (Outcome, error)

	// Teardown removes the Flux objects for one pair. It runs while an
	// Environment is being deleted, on every reconcile until the finalizer
	// clears, so it must be idempotent and must treat "already gone" as
	// success.
	Teardown(ctx context.Context, project, environment string) error
}

// NoopDeliverer publishes nothing and reports nothing.
//
// It is what a zero-value reconciler falls back to, so a test that only cares
// about validation does not have to build a registry. It returns an empty
// Outcome rather than a fabricated one on purpose: a phase of "Healthy" for a
// deployment that never happened would be a status that lies, which is the
// specific failure ADR-0027 says a status subresource exists to prevent.
type NoopDeliverer struct{}

// Deliver does nothing.
func (NoopDeliverer) Deliver(context.Context, Revision) (Outcome, error) {
	return Outcome{}, nil
}

// Teardown does nothing: there is nothing a no-op deliverer could have created.
func (NoopDeliverer) Teardown(context.Context, string, string) error { return nil }

// modelProject lifts a custom resource into the authoring-model document
// validate.go and the resolver expect.
//
// The Spec is shared, not copied: it is the same struct
// (ADR-0027 decision 3), so the only thing to build is the TypeMeta and
// ObjectMeta the model spells its own way. A custom resource always carries the
// group's apiVersion and kind, so they are restated as constants rather than
// read off the object — an object fetched through a typed client has an empty
// TypeMeta, and validate.go would then refuse a document the API server had
// already accepted.
func modelProject(p *v1alpha1.Project) *model.Project {
	return &model.Project{
		TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindProject},
		Metadata: model.ObjectMeta{Name: p.Name},
		Spec:     p.Spec,
	}
}

func modelEnvironment(e *v1alpha1.Environment) *model.Environment {
	return &model.Environment{
		TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindEnvironment},
		Metadata: model.ObjectMeta{Name: e.Name},
		Spec:     e.Spec,
	}
}

// validationErrors converts model.Errors into the status shape.
//
// Every field is carried across, including the ones a custom resource cannot
// fill: Line and Column are zero here because the document arrived as an object
// and not as text, and the field exists so that the same taxonomy can carry
// them when the CLI produced the error. One taxonomy, spelled once (ADR-0027
// decision 5).
func validationErrors(errs model.Errors) []v1alpha1.ValidationError {
	if len(errs) == 0 {
		return nil
	}
	out := make([]v1alpha1.ValidationError, 0, len(errs))
	for _, e := range errs {
		out = append(out, v1alpha1.ValidationError{
			Code:        string(e.Code),
			Resource:    e.Resource,
			Field:       e.Field,
			Message:     e.Message,
			Remediation: e.Remediation,
			DocsURL:     e.DocsURL,
			Line:        e.Line,
			Column:      e.Column,
		})
	}
	return out
}

// summarize turns a set of validation errors into the one line a condition
// message may hold. The detail is in status.validationErrors; the message says
// how much detail there is and where to look, because a condition message that
// tried to hold every error would be truncated by the API server at a point
// nobody chose.
func summarize(errs model.Errors) string {
	switch len(errs) {
	case 0:
		return ""
	case 1:
		return errs[0].Error()
	default:
		return errs[0].Error() + " (and " + plural(len(errs)-1) + " in status.validationErrors)"
	}
}

func plural(n int) string {
	if n == 1 {
		return "1 more error"
	}
	return strconv.Itoa(n) + " more errors"
}
