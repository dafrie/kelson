package controller

import (
	"context"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dafrie/kelson/api/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/build/registry"
	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/delivery/flux"
	"github.com/dafrie/kelson/internal/delivery/statemachine"
)

// DefaultFluxNamespace is where kelson's own Flux objects live. It is the
// namespace the chart installs kelson into, not `flux-system`: they are
// kelson's objects and they belong beside the controller that writes them
// (ADR-0028 decision 3).
const DefaultFluxNamespace = "kelson-system"

// DefaultInterval is how often Flux re-checks an object kelson owns.
//
// It is not how fast a deploy is. The OCIRepository is pinned to an explicit
// tag and re-applied the moment a spec changes, so a new revision reaches Flux
// through a watch, not a poll. What the interval buys is drift correction — the
// thing kelson stopped having opinions about when kustomize-controller took it
// over — so it is set for that job: often enough that a hand-edited Deployment
// is put back within minutes, rarely enough that a hundred environments do not
// make the registry a hot path.
const DefaultInterval = 5 * time.Minute

// DefaultPublishTimeout bounds one publish: packaging is in-process and
// instant, so this is the registry's budget for a handful of small uploads.
//
// It exists because a reconcile has no deadline of its own and the worker pool
// is small. A registry that completes its TCP handshake and then answers
// nothing — a black hole in front of a proxy, a hung load balancer — would
// otherwise wedge one of those workers indefinitely, and a controller that has
// silently stopped reconciling every other environment is a much worse failure
// than a publish that gave up and requeued. Two minutes is far more than a
// manifest set needs and far less than "forever".
const DefaultPublishTimeout = 2 * time.Minute

// FluxDeliverer is ADR-0028 steps 4 to 6: publish the artifact, apply the two
// Flux objects that consume it, and read the result back.
//
// # What it does not do
//
// It does not wait. There is a state machine in this repository that blocks
// until a deployment settles (internal/delivery/statemachine's Engine), and
// calling it from inside Reconcile would hold a worker for the length of a
// rollout — which is how a controller with a bounded worker pool stops
// reconciling everything else. What is used here is the transition *table*
// (statemachine.Validate): each reconcile observes once, checks that the phase
// it saw is a legal successor of the phase in the status, and returns. The
// waiting is controller-runtime's, through the watch on the Flux objects and a
// requeue on non-terminal phases.
//
// # Why it is behind an interface at all
//
// [Deliverer] has exactly one production implementation, and ADR-0028 decision
// 9 is explicit that one implementation behind an interface is an interface
// describing nothing. This one earns its keep for a different reason than the
// deleted adapter seam did: it is the boundary between "decides what to
// publish" and "talks to a registry and an API server", and the reconciler's
// own tests — every branch of validation, rollback, history and the finalizer —
// need the second half to be a fake.
type FluxDeliverer struct {
	// Client reads and writes the Flux objects. It is the manager's, so the
	// reads are served from a cache scoped to FluxNamespace.
	Client client.Client

	// Registry is the operator's push prefix, e.g. "ghcr.io/acme". Artifacts go
	// to <Registry>/kelson/<project>-<environment>. Empty is a configuration
	// error reported as RegistryNotConfigured rather than a crash at start-up,
	// because a controller that will not start cannot tell anybody why.
	Registry string

	// RegistryConfig is the path to a docker config.json holding the push
	// credential. Absent is an anonymous push.
	RegistryConfig string

	// InsecureRegistries are the hosts that speak plain HTTP, named by the
	// operator and never guessed (internal/build/registry/insecure.go).
	InsecureRegistries []string

	// FluxNamespace holds the OCIRepository and Kustomization pair. Empty means
	// DefaultFluxNamespace.
	FluxNamespace string

	// PullSecret names a dockerconfigjson Secret in FluxNamespace that
	// source-controller authenticates with. It is a *different* credential from
	// RegistryConfig: the controller pushes and source-controller pulls, and
	// they are different processes.
	PullSecret string

	// Interval is what both Flux objects reconcile on. Zero means
	// DefaultInterval.
	Interval time.Duration

	// PublishTimeout bounds step 4. Zero means DefaultPublishTimeout.
	PublishTimeout time.Duration

	// Pusher is the seam tests replace. Nil is the real registry client.
	Pusher PusherFor
}

var _ Deliverer = (*FluxDeliverer)(nil)

func (d *FluxDeliverer) namespace() string {
	if d.FluxNamespace != "" {
		return d.FluxNamespace
	}
	return DefaultFluxNamespace
}

func (d *FluxDeliverer) interval() string {
	i := d.Interval
	if i <= 0 {
		i = DefaultInterval
	}
	return i.String()
}

func (d *FluxDeliverer) publishTimeout() time.Duration {
	if d.PublishTimeout > 0 {
		return d.PublishTimeout
	}
	return DefaultPublishTimeout
}

func (d *FluxDeliverer) insecure(repository string) bool {
	return registry.IsInsecure(d.InsecureRegistries, repository)
}

// Deliver runs steps 4, 5 and 6 for one revision.
func (d *FluxDeliverer) Deliver(ctx context.Context, rev Revision) (Outcome, error) {
	// Step 0, and it is not in the ADR's list because it is not work: a cluster
	// with no Flux has nothing that would reconcile what kelson publishes, so
	// publishing would be a push into an empty room. This is the *expected*
	// state of a fresh cluster, so it is a status and a timer, never a crash
	// loop (see errors.go).
	if !rev.FluxPresent {
		return Outcome{}, newDeliveryError(v1alpha1.ReasonFluxNotInstalled,
			"the ClusterProfile reports no Flux in this cluster, and Flux is what reconciles what kelson "+
				"publishes. Install it with `kelson install`, then this environment converges on its own.", nil)
	}

	// The tag is derived even for a rollback, because the rollback's own tag is
	// what the OCIRepository is pinned to and the repository is the same either
	// way.
	repository, tag, err := ArtifactRef(d.Registry, rev.Project, rev.Environment, rev.Generation, rev.SpecHash)
	if err != nil {
		return Outcome{}, err
	}

	out := Outcome{Revision: tag}
	switch {
	case rev.PinnedTo != "":
		// Rollback (ADR-0028 decision 5): steps 3 and 4 do not run. The tag is
		// immutable and already in the registry — that is what the caller
		// verified against status.history — so there is nothing to render and
		// nothing to push, only a pointer to move.
		out.Revision = rev.PinnedTo
		out.Digest = rev.PinnedDigest
		out.RolledBack = true

	case d.settled(ctx, rev, tag):
		// Nothing to do: the status already claims this tag and the live
		// OCIRepository agrees. Skipping is an optimisation rather than a
		// correctness requirement — packaging is deterministic, so a republish
		// would upload nothing — which is exactly why it is safe to have.
		out.Digest = rev.ObservedDigest

	default:
		// The push is the one step of a reconcile that talks to something
		// outside the cluster, and a registry that accepts a connection and then
		// answers nothing would hold this worker — one of a small pool — for as
		// long as it cared to. The context deadline is the outer bound; the HTTP
		// client's own timeout (internal/artifact) bounds each request inside it.
		pushCtx, cancel := context.WithTimeout(ctx, d.publishTimeout())
		a, _, err := d.publish(pushCtx, rev, repository, tag)
		cancel()
		if err != nil {
			return Outcome{}, err
		}
		out.Digest = a.Digest
		out.Published = true
		out.Images = images(rev)
	}

	// The tag the pair is pinned to is the outcome's revision, which is the
	// rollback target when one is in force and the freshly published tag
	// otherwise. That single substitution is the whole of what a rollback does
	// to the cluster (ADR-0028 decision 5: "a pointer moves to bytes that
	// already exist and cannot have changed").
	if err := d.ensure(ctx, rev, repository, out.Revision, out.Digest); err != nil {
		return Outcome{}, err
	}

	// Step 6: observe. One read, no waiting — see the type doc.
	phase, cause := d.observe(ctx, rev, out.Revision)
	out.Phase, out.Cause = phase, cause
	return out, nil
}

// Teardown implements [Deliverer].
func (d *FluxDeliverer) Teardown(ctx context.Context, project, environment string) error {
	return d.teardown(ctx, project, environment)
}

// settled reports whether this exact tag is already published *and* live: the
// status says so, and the OCIRepository in the cluster is actually pinned to it.
//
// Both halves are required, and the second is the one that matters. A status
// claiming a revision proves only that a previous reconcile got as far as
// writing the status; if the OCIRepository was then deleted, or edited, or
// never applied because the process died between the push and the apply, the
// environment is not serving what the status says. Checking the live object is
// what makes the skip an optimisation instead of a way to get permanently stuck.
func (d *FluxDeliverer) settled(ctx context.Context, rev Revision, tag string) bool {
	if rev.Observed != tag {
		return false
	}
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(ociRepositoryGVK)
	key := types.NamespacedName{Namespace: d.namespace(), Name: ObjectName(rev.Project, rev.Environment)}
	if err := d.Client.Get(ctx, key, live); err != nil {
		return false
	}
	got, _, _ := unstructured.NestedString(live.Object, "spec", "ref", "tag")
	return got == tag
}

// observe reads the Kustomization back and maps it onto a delivery phase.
//
// The mapping is internal/delivery/flux's, not a second one: `kelson status`
// answers this question about any Kustomization in the cluster, and the
// controller answering it differently about its own would be two opinions about
// what Degraded means. `wait: true` on the Kustomization (fluxobjects.go) is
// what makes Ready mean healthy, so the phase this returns covers the workloads
// without the controller holding any RBAC over them.
//
// A Kustomization that is not there yet is Committed and not an error: it was
// applied a few milliseconds ago through a cache that has not caught up, and
// reporting a failure for that would make every first deploy look broken.
func (d *FluxDeliverer) observe(ctx context.Context, rev Revision, revision string) (string, string) {
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(kustomizationGVK)
	name := ObjectName(rev.Project, rev.Environment)
	key := types.NamespacedName{Namespace: d.namespace(), Name: name}
	if err := d.Client.Get(ctx, key, live); err != nil {
		if apierrors.IsNotFound(err) || kindNotServed(err) {
			return v1alpha1.PhaseCommitted, "the Kustomization has not been observed yet"
		}
		return v1alpha1.PhaseCommitted, "could not read the Kustomization back: " + err.Error()
	}
	status := flux.PhaseFor(flux.KustomizationFrom(live.Object, name, d.namespace()), revision)
	return string(status.Phase), status.Cause
}

// images is what this revision resolved to, in component order: the answer to
// "which build is in production", and what a promotion reads (ADR-0016
// decision 2).
func images(rev Revision) []string {
	if rev.Resolved == nil {
		return nil
	}
	out := make([]string, 0, len(rev.Resolved.Components))
	for _, c := range rev.Resolved.Components {
		if c.Image != "" {
			out = append(out, c.Image)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// phaseFor decides the phase one reconcile writes, and the decision it makes
// before reaching the guard is the load-bearing half.
//
// [statemachine.Validate] is revision-blind: it answers "can a deployment go
// from Healthy to Committed?", and the answer is always no, because a revision
// cannot become uncommitted. But a *new* revision starts its own lifecycle, and
// running its first observation through the guard against the last revision's
// terminal phase freezes the status forever: an environment that reached Healthy
// would report Healthy for every subsequent spec edit, including one Flux
// rejected, and [EnvironmentReconciler.requeueFor] would stop polling because
// the phase it read is terminal.
//
// So a new revision — one this reconcile published, or one whose tag differs
// from the tag the status held — takes the observed phase directly, and the
// guard governs only what it was written for: repeated observations of the
// revision already in the status.
func phaseFor(current, observed string, outcome Outcome) string {
	if outcome.Phase == "" {
		return current
	}
	if outcome.Published || (outcome.Revision != "" && outcome.Revision != observed) {
		if statemachine.Known(delivery.Phase(outcome.Phase)) {
			return outcome.Phase
		}
		return current
	}
	return nextPhase(current, outcome.Phase)
}

// nextPhase is the transition guard. It is [statemachine.Validate] and nothing
// else: the table that says a revision cannot become uncommitted, that nothing
// precedes the commit, and that rejection is pre-apply.
//
// An illegal transition is not propagated as a failure, because the observation
// is not wrong — it is an observation of a *different* revision's lifecycle
// arriving after a new one started, which is normal the moment a spec is edited
// mid-rollout. So the guard drops the impossible step and keeps the phase the
// status already holds, and the next observation (which will be about the new
// revision) moves it. Reporting an error here would turn a routine race into a
// red status.
//
// It is reached only for observations of the revision the status already names:
// [phaseFor] decides that, and says why the distinction matters.
func nextPhase(from, to string) string {
	if to == "" {
		return from
	}
	if from == "" {
		// Nothing observed before: the first phase is whatever was seen, as
		// long as the table knows it at all.
		if statemachine.Known(delivery.Phase(to)) {
			return to
		}
		return from
	}
	if err := statemachine.Validate(delivery.Phase(from), delivery.Phase(to)); err != nil {
		return from
	}
	return to
}
