package controller

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/dafrie/kelson/api/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/model"
)

// ProjectReconciler validates a Project and records the verdict. That is all it
// does, and all it will do.
//
// A Project is the shared half of a spec; nothing deploys from it on its own,
// because what runs is always an Environment. So there is no render here, no
// publish and no delivery — an Environment's reconcile validates the pair
// anyway (validate.go's ValidateSet takes the Project and its Environments
// together), and this loop exists so an author who applies a Project alone finds
// out immediately whether it parsed, instead of finding out when the first
// Environment refuses.
type ProjectReconciler struct {
	Client client.Client
}

// Reconcile validates one Project and writes the result to its status.
func (r *ProjectReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var project v1alpha1.Project
	if err := r.Client.Get(ctx, req.NamespacedName, &project); err != nil {
		// A deleted object is not an error and there is nothing to clean up:
		// the CRs kelson owns hold no finalizer, so deletion is the API
		// server's business and not this loop's.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	base := project.DeepCopy()
	project.Status.ObservedGeneration = project.Generation

	errs := model.ValidateSet(modelProject(&project))
	project.Status.ValidationErrors = validationErrors(errs)
	if len(errs) > 0 {
		log.FromContext(ctx).Info("project spec is invalid", "errors", len(errs))
		setReady(&project.Status.Conditions, project.Generation,
			metav1.ConditionFalse, v1alpha1.ReasonSpecInvalid, summarize(errs))
	} else {
		setReady(&project.Status.Conditions, project.Generation,
			metav1.ConditionTrue, v1alpha1.ReasonReady, "the project document is valid")
	}

	// The verdict is a status and never an error return: an invalid document
	// does not become valid by being reconciled again (see the package doc).
	return ctrl.Result{}, patchStatus(ctx, r.Client, &project, base)
}

// SetupWithManager registers the reconciler.
//
// It watches Projects and nothing else. It does not watch the Environments that
// reference it, because a Project's own status says nothing about them — that
// direction of the edge belongs to EnvironmentReconciler, which does watch
// Projects.
func (r *ProjectReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Project{}).
		Named("project").
		Complete(r)
}

// setReady sets the Ready condition, stamping the generation it describes.
//
// meta.SetStatusCondition leaves LastTransitionTime alone when only the message
// changed, so a status that keeps saying the same thing does not keep producing
// watch events for every other controller in the cluster.
func setReady(conditions *[]metav1.Condition, generation int64, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(conditions, metav1.Condition{
		Type:               v1alpha1.ConditionReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: generation,
	})
}

// setProgressing sets the Progressing condition, which only an Environment
// carries: a Project has no delivery of its own and so has nothing to be
// progressing towards.
//
// It is a second condition rather than another reason on Ready because the two
// answer different questions and a rolled-back environment answers them
// differently: Ready=True (the pinned revision is live and healthy) with
// Progressing=False/RollbackPinned (and it is deliberately not tracking your
// spec). Folding that into one condition would force a choice between claiming
// an environment is broken and hiding that it has stopped deploying.
func setProgressing(conditions *[]metav1.Condition, generation int64, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(conditions, metav1.Condition{
		Type:               v1alpha1.ConditionProgressing,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: generation,
	})
}

// patchStatus writes the status subresource with a merge patch against the
// object as it was read.
//
// A patch and not an update: the reconciler has an object from a cache that may
// be a few milliseconds old, and an Update would send the whole status and
// clobber a concurrent write with a stale copy. MergeFrom sends only what this
// reconcile changed. A conflict is returned as an error so controller-runtime
// requeues — the one case where a retry is exactly right.
func patchStatus(ctx context.Context, c client.Client, obj, base client.Object) error {
	if err := c.Status().Patch(ctx, obj, client.MergeFrom(base)); err != nil {
		// A status write against an object that has since been deleted is not a
		// failure of this reconcile.
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	return nil
}
