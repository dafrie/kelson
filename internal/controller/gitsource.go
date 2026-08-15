package controller

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/dafrie/kelson/api/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/model"
)

// GitSourceReconciler validates a GitSource and records the verdict. That is
// all it does, and — unlike the Project reconciler it is modelled on — all it
// *can* do.
//
// A GitSource is the instance's tier of declared sources (ADR-0035 decision 2):
// a repository URL, a ref, and the name of the connection to read it with.
// Nothing is rendered from it and nothing is deployed by it; what it changes is
// which names a component may bind to. So the loop exists for the reason
// ProjectReconciler's does — an operator who applies one finds out immediately
// whether it parsed, rather than finding out when a project that binds to it
// refuses to build — and it stops there.
//
// # There is no Reachable condition, deliberately
//
// A GitConnection carries one, because it holds the credential and can ask the
// forge. A GitSource holds no credential at all; whether its repository answers
// is a question about the connection that serves it, and that connection
// already records the answer (ADR-0033 decision 1, ADR-0035 decision 2). Asking
// it here as well would give an operator two places to look and two answers
// that can disagree — and this loop could not even ask honestly: the connection
// it would have to borrow is chosen by host match at *use* time, against the
// connections the instance holds when a build runs, not when a source is
// applied.
//
// It does not check that `spec.connection` names a connection that exists
// either, for the same reason: a name that resolves to nothing is refused where
// it is used, with the repository in hand and both halves of the scope to list
// (internal/forgeconn), and a status here would be a second, staler opinion.
type GitSourceReconciler struct {
	Client client.Client
}

// Reconcile validates one GitSource and writes the result to its status.
func (r *GitSourceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var source v1alpha1.GitSource
	if err := r.Client.Get(ctx, req.NamespacedName, &source); err != nil {
		// A deleted object is not an error and there is nothing to clean up:
		// kelson holds no finalizer on a GitSource, and a project bound to one
		// that has gone learns so from its own resolution.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	base := source.DeepCopy()
	source.Status.ObservedGeneration = source.Generation

	errs := model.ValidateGitSource(modelGitSource(&source))
	source.Status.ValidationErrors = validationErrors(errs)
	if len(errs) > 0 {
		log.FromContext(ctx).Info("git source spec is invalid", "errors", len(errs))
		setReady(&source.Status.Conditions, source.Generation,
			metav1.ConditionFalse, v1alpha1.ReasonSpecInvalid, summarize(errs))
	} else {
		setReady(&source.Status.Conditions, source.Generation,
			metav1.ConditionTrue, v1alpha1.ReasonReady,
			"the source document is valid; whether the repository can be reached is the connection's to report")
	}

	// A status and never an error return, for the reason the package doc
	// states: an invalid document does not become valid by being reconciled
	// again, and requeueing it forever would produce one identical failure per
	// backoff interval until somebody edits it.
	return ctrl.Result{}, patchStatus(ctx, r.Client, &source, base)
}

// SetupWithManager registers the reconciler.
//
// It watches GitSources and nothing else. It does not watch the Projects that
// bind to it: a source's own status says nothing about them, and a project
// whose binding stopped resolving is the *project's* status to carry — which is
// the same direction of the edge ProjectReconciler declines to watch.
func (r *GitSourceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.GitSource{}).
		Named("gitsource").
		Complete(r)
}

// modelGitSource lifts the custom resource into the authoring-model document
// validate.go expects, exactly as [modelProject] does and for the same reason:
// the Spec is the shared struct (ADR-0027 decision 3), so only the TypeMeta and
// the ObjectMeta have to be built, and they are restated as constants because
// an object read through a typed client carries an empty TypeMeta.
func modelGitSource(g *v1alpha1.GitSource) *model.GitSource {
	return &model.GitSource{
		TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindGitSource},
		Metadata: model.ObjectMeta{Name: g.Name},
		Spec:     g.Spec,
	}
}
