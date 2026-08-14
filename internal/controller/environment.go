package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/dafrie/kelson/api/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/model"
	"github.com/dafrie/kelson/internal/renderer"
)

// EnvironmentReconciler converges one Environment (ADR-0028 decision 1).
//
// It runs steps 1 to 3 — validate, detect, resolve and render — and hands the
// result to a [Deliverer], which is a no-op until issue #224. See the package
// doc for what that means and why the seam is here already.
type EnvironmentReconciler struct {
	Client client.Client

	// Profiles supplies the ClusterProfile the render is judged against.
	Profiles ProfileSource

	// Delivery publishes the rendered set. Never nil in production;
	// [NoopDeliverer] is the default a manager wires when nothing else is
	// configured.
	Delivery Deliverer
}

// Reconcile converges one Environment and records what it observed.
func (r *EnvironmentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var env v1alpha1.Environment
	if err := r.Client.Get(ctx, req.NamespacedName, &env); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	base := env.DeepCopy()
	env.Status.ObservedGeneration = env.Generation
	// Every path below decides the condition and the errors afresh, so a
	// problem that has been fixed does not leave its evidence behind.
	env.Status.ValidationErrors = nil

	// The Project this Environment binds to, in the same namespace. The
	// reference is namespace-local by design (api/kelson/v1alpha1): a Project
	// and its Environments are one unit of ownership.
	var project v1alpha1.Project
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Spec.Project}
	if err := r.Client.Get(ctx, key, &project); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("reading project %s: %w", key, err)
		}
		// A missing Project gets its own reason rather than a validation
		// error, because it is not a property of this document: the same
		// Environment becomes valid the moment the Project is applied, and the
		// watch below is what notices. Returning nil rather than an error is
		// the point — an ordering mistake must not spin.
		logger.Info("project not found", "project", key.Name)
		setReady(&env.Status.Conditions, env.Generation, metav1.ConditionFalse,
			v1alpha1.ReasonProjectNotFound,
			fmt.Sprintf("spec.project names %q, and no Project of that name exists in namespace %s. "+
				"Apply the Project, or point spec.project at one that exists.", key.Name, env.Namespace))
		return ctrl.Result{}, patchStatus(ctx, r.Client, &env, base)
	}

	// Step 1: validate. The pair is validated together, by the same function
	// the CLI and the server call, so the three produce identical codes, field
	// paths and remediation text (ADR-0027 decision 5).
	mp, me := modelProject(&project), modelEnvironment(&env)
	if errs := model.ValidateSet(mp, me); len(errs) > 0 {
		logger.Info("environment spec is invalid", "errors", len(errs))
		env.Status.ValidationErrors = validationErrors(errs)
		setReady(&env.Status.Conditions, env.Generation, metav1.ConditionFalse,
			v1alpha1.ReasonSpecInvalid, summarize(errs))
		return ctrl.Result{}, patchStatus(ctx, r.Client, &env, base)
	}

	// Step 3a: resolve. Resolution can still produce taxonomy errors — an image
	// that resolves to nothing, for instance — and they are the same kind of
	// answer as a validation failure, so they land in the same place.
	resolved, errs := model.Resolve(mp, me)
	if len(errs) > 0 {
		logger.Info("environment spec does not resolve", "errors", len(errs))
		env.Status.ValidationErrors = validationErrors(errs)
		setReady(&env.Status.Conditions, env.Generation, metav1.ConditionFalse,
			v1alpha1.ReasonSpecInvalid, summarize(errs))
		return ctrl.Result{}, patchStatus(ctx, r.Client, &env, base)
	}

	// Overlays are paths relative to the authoring documents, and a document
	// that arrived as a custom resource has no authoring directory — the same
	// wall internal/api hits and refuses by name (internal/api/pipeline.go's
	// checkOverlays). Rendering with a nil resolver would fail deep inside the
	// renderer; saying so here means the author is told "kelson cannot do this
	// yet" instead of being handed the renderer's internals.
	if len(resolved.Overlays) > 0 {
		setReady(&env.Status.Conditions, env.Generation, metav1.ConditionFalse,
			v1alpha1.ReasonRenderFailed,
			"spec.overlays are not supported for an Environment reconciled from the cluster: "+
				"overlay paths resolve against the spec files, and a custom resource has none. "+
				"Render this spec with the CLI, or remove the overlay.")
		return ctrl.Result{}, patchStatus(ctx, r.Client, &env, base)
	}

	// Step 2: detect. A profile that cannot be read is a transient cluster
	// problem and therefore the one thing in this function that IS an error
	// return: waiting and trying again is the right behaviour.
	profile, err := r.Profiles.Profile(ctx)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("reading the cluster profile: %w", err)
	}

	// Step 3b: render. Still pure — this reconciler is the caller that has
	// cluster access, the renderer has none (ADR-0028 decision 1, step 3).
	manifests, err := renderer.Render(resolved, profile, nil)
	if err != nil {
		// A render refusal is kelson's gap and not the author's mistake — an
		// unimplemented field, a capability the profile does not offer — so it
		// gets its own reason and, like an invalid spec, no requeue.
		logger.Info("render failed", "error", err)
		setReady(&env.Status.Conditions, env.Generation, metav1.ConditionFalse,
			v1alpha1.ReasonRenderFailed, err.Error())
		return ctrl.Result{}, patchStatus(ctx, r.Client, &env, base)
	}

	// Steps 4 and 5: publish and ensure. Stubbed (issue #224).
	outcome, err := r.deliverer().Deliver(ctx, Revision{
		Project:     project.Name,
		Environment: env.Name,
		Generation:  env.Generation,
		Manifests:   manifests,
		Resolved:    resolved,
	})
	if err != nil {
		// Publishing is I/O against a registry and a cluster, so a failure is
		// worth retrying and is returned as an error.
		return ctrl.Result{}, fmt.Errorf("delivering %s/%s: %w", project.Name, env.Name, err)
	}
	if outcome.Revision != "" {
		env.Status.Revision = outcome.Revision
	}
	if outcome.Phase != "" {
		env.Status.Phase = outcome.Phase
	}

	setReady(&env.Status.Conditions, env.Generation, metav1.ConditionTrue, v1alpha1.ReasonReady,
		fmt.Sprintf("the environment is valid and renders %s", resourceCount(len(manifests))))
	return ctrl.Result{}, patchStatus(ctx, r.Client, &env, base)
}

func resourceCount(n int) string {
	if n == 1 {
		return "1 resource"
	}
	return fmt.Sprintf("%d resources", n)
}

// deliverer is the nil-safe accessor. A zero-value reconciler renders and stops
// rather than panicking, which is what makes the struct usable in a test that
// only cares about validation.
func (r *EnvironmentReconciler) deliverer() Deliverer {
	if r.Delivery == nil {
		return NoopDeliverer{}
	}
	return r.Delivery
}

// SetupWithManager registers the reconciler and the one watch it needs beyond
// its own kind.
//
// A change to a Project changes what its Environments validate and render
// against, so each one has to be re-reconciled — otherwise an Environment
// refused for a missing Project would stay refused after the Project was
// applied, until something else happened to touch it. The map function lists
// Environments in the Project's namespace and filters by spec.project; a field
// index would be faster and is not worth its setup at this size, since the list
// is served from the same cache the reconciler already has.
func (r *EnvironmentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Profiles == nil {
		r.Profiles = StaticProfileSource{}
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Environment{}).
		Watches(&v1alpha1.Project{}, handler.EnqueueRequestsFromMapFunc(r.environmentsOfProject)).
		Named("environment").
		Complete(r)
}

func (r *EnvironmentReconciler) environmentsOfProject(ctx context.Context, project client.Object) []reconcile.Request {
	var list v1alpha1.EnvironmentList
	if err := r.Client.List(ctx, &list, client.InNamespace(project.GetNamespace())); err != nil {
		log.FromContext(ctx).Error(err, "listing environments for a project change",
			"project", project.GetName(), "namespace", project.GetNamespace())
		return nil
	}
	var out []reconcile.Request
	for i := range list.Items {
		env := &list.Items[i]
		if env.Spec.Project != project.GetName() {
			continue
		}
		out = append(out, reconcile.Request{
			NamespacedName: types.NamespacedName{Namespace: env.Namespace, Name: env.Name},
		})
	}
	return out
}
