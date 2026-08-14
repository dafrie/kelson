// Package controller is the reconciliation plane: the controllers behind
// kelson's two custom resources (ADR-0027, ADR-0028).
//
// # What a reconcile does today, and what it will do
//
// ADR-0028 decision 1 lists six steps for an Environment: validate, detect,
// resolve-and-render, publish, ensure the Flux objects, observe. This package
// implements the first three and stops:
//
//  1. validate — internal/model/validate.go, the same function the CLI and the
//     server call. An invalid document is a *status*, never an error return.
//  2. detect   — behind [ProfileSource]. The real implementation reads a
//     ClusterProfile from the cluster; [StaticProfileSource] stands
//     in until that wiring lands.
//  3. resolve and render — internal/model's resolver and the pure renderer,
//     unchanged and still pure: this package is the caller that has cluster
//     access, the renderer still has none.
//
// Steps 4 to 6 — push the OCI artifact, server-side apply the OCIRepository and
// Kustomization pair, watch them back into the state machine — are behind
// [Deliverer], whose only implementation here is [NoopDeliverer]. That is
// issue #224. The interface exists now so the reconcile loop above it is the
// shape it will keep, and so the seam is a named thing in the tree rather than
// a TODO in the middle of a function.
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
	"strconv"

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

// StaticProfileSource serves one profile, forever. It is what
// cmd/kelson-controller runs with until detection is wired: an empty profile is
// the honest answer for a controller that has not looked, and the renderer
// already treats "absent" as "do not emit the optional resource" rather than as
// an error (internal/clusterprofile's tri-state discipline).
type StaticProfileSource struct {
	ClusterProfile clusterprofile.ClusterProfile
}

// Profile returns the configured profile.
func (s StaticProfileSource) Profile(context.Context) (clusterprofile.ClusterProfile, error) {
	return s.ClusterProfile, nil
}

// Revision is one rendered candidate: what steps 4 and 5 would publish.
type Revision struct {
	// Project and Environment name the pair, and are the artifact's repository
	// path: <registry>/kelson/<project>-<environment>.
	Project     string
	Environment string

	// Generation is the Environment's .metadata.generation — the revision
	// number, allocated by the API server rather than by kelson (ADR-0028
	// decision 2).
	Generation int64

	// Manifests are the rendered set, in render order.
	Manifests []renderer.Manifest

	// Resolved is the spec the manifests came from, for anything the publisher
	// needs to label or annotate.
	Resolved *model.Resolved
}

// Outcome is what a Deliverer reports back into the Environment's status.
type Outcome struct {
	// Revision is the artifact tag that is now serving, e.g. "7-1a2b3c4d".
	// Empty means nothing was published.
	Revision string

	// Phase is the delivery state-machine phase (the v1alpha1.Phase*
	// constants). Empty means the phase is unchanged.
	Phase string
}

// Deliverer performs ADR-0028's steps 4 and 5: push the rendered set as an
// immutable OCI artifact, then server-side apply the OCIRepository and
// Kustomization pair that consume it.
//
// TODO(#224): the real implementation. It needs the artifact publisher the
// preview pipeline already has (ADR-0017 decision 10, the same package by
// decision 2 of ADR-0028), a registry and push credential from the controller's
// flags, and the two Flux objects in kelson-system. Until then
// [NoopDeliverer] renders and stops, which is a deliberate half: the render is
// the part that can tell an author their document is wrong, and it is worth
// doing before the publishing exists.
type Deliverer interface {
	Deliver(ctx context.Context, rev Revision) (Outcome, error)
}

// NoopDeliverer publishes nothing and reports nothing.
//
// It returns an empty Outcome rather than a fabricated one on purpose: a phase
// of "Healthy" for a deployment that never happened would be a status that
// lies, which is the specific failure ADR-0027 says a status subresource exists
// to prevent.
type NoopDeliverer struct{}

// Deliver does nothing. See [Deliverer] and issue #224.
func (NoopDeliverer) Deliver(context.Context, Revision) (Outcome, error) {
	return Outcome{}, nil
}

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
