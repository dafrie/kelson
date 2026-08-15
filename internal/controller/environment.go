package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/dafrie/kelson/api/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/model"
	"github.com/dafrie/kelson/internal/renderer"
)

// Finalizer is what keeps an Environment alive until its Flux objects are gone.
//
// # Why there is one at all
//
// Deleting an Environment has to delete the Kustomization, and deleting the
// Kustomization is what removes the workloads — it prunes its inventory on the
// way out. Without a finalizer the custom resource disappears the instant
// `kubectl delete` returns, taking with it the only record of which objects
// were kelson's, and the Kustomization keeps reconciling an environment nobody
// declared any more.
//
// # What it deliberately does not clean up
//
// The workload namespace and the published artifacts. See the deletion path in
// [EnvironmentReconciler.finalize] for why.
//
// # The one way to make it clean up nothing
//
// [v1alpha1.AnnotationOrphanOnDelete] on the Environment. The finalizer still
// runs and still releases itself; it skips the teardown, and the pair — with the
// application it reconciles — stays (issue #242).
const Finalizer = "kelson.dev/environment"

// nonTerminalRequeue is the belt-and-braces poll while a deployment is in
// flight.
//
// The watch on the Flux objects is what actually reports progress, and in a
// healthy cluster this timer never fires before the watch does. It exists for
// the cases where the watch cannot: an informer that has not synced, a cache
// scoped to a namespace the objects moved out of, an event dropped during a
// leader-election handover. Ten seconds is short enough to be invisible in a
// rollout and long enough that it is not a poll loop.
const nonTerminalRequeue = 10 * time.Second

// EnvironmentReconciler converges one Environment: all six steps of ADR-0028
// decision 1.
//
//	1 validate  — internal/model/validate.go. A status, never an error return.
//	2 detect    — the ClusterProfile, behind [ProfileSource].
//	3 render    — the pure renderer. Suspended while a rollback is pinned.
//	4 publish   — the OCI artifact. Suspended with it.
//	5 ensure    — the OCIRepository and Kustomization pair.
//	6 observe   — the Kustomization's condition, mapped onto a phase.
//
// Steps 4 to 6 are behind [Deliverer]; everything else is here.
type EnvironmentReconciler struct {
	Client client.Client

	// Profiles supplies the ClusterProfile the render is judged against, and
	// the Flux finding that decides whether publishing happens at all.
	Profiles ProfileSource

	// Sources supplies the instance's declared repositories, which resolution
	// needs to turn a component's `source: <name>` into a repository when the
	// name is a GitSource's rather than one of the Project's (ADR-0035
	// decision 3). Nil is a controller with no global tier; see
	// [GitSourceLister].
	Sources GitSourceLister

	// Delivery publishes the rendered set and applies the Flux objects. Never
	// nil in production; [NoopDeliverer] is what a zero-value reconciler falls
	// back to, so a test about validation needs no registry.
	Delivery Deliverer

	// Revisions reads the registry's tag list: the record ADR-0028 decision 4
	// makes the history, of which `status.history` is a bounded mirror. It is
	// what lets a rollback reach a revision older than the window (issue #241).
	//
	// Nil is a reconciler that can see only the mirror, and it refuses an
	// aged-out target by saying so — which is honest, and is the behaviour
	// every test that does not care about the registry gets for free.
	Revisions RevisionLister

	// Recorder is where a deletion that left its workloads running says so
	// (issue #242). SetupWithManager fills it in from the manager; nil records
	// nothing, which is what every test that does not assert on events gets.
	//
	// It is an event and not a condition because the object it is about is
	// being deleted: a status written a moment before the finalizer clears is a
	// status nobody can read afterwards, and an event outlives the object it
	// references.
	Recorder record.EventRecorder

	// FluxWatches records whether SetupWithManager registered the watches on
	// the Flux objects. It is false on a cluster with no Flux, where an
	// informer on an unserved CRD would hang the cache sync forever — see
	// SetupWithManager.
	FluxWatches bool
}

// Reconcile converges one Environment and records what it observed.
func (r *EnvironmentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var env v1alpha1.Environment
	if err := r.Client.Get(ctx, req.NamespacedName, &env); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Deletion comes first, and before validation: an Environment whose spec
	// stopped validating must still be deletable, and an object being deleted
	// has nothing to converge towards.
	if !env.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &env)
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
		return r.halt(ctx, &env, base, v1alpha1.ReasonProjectNotFound,
			fmt.Sprintf("spec.project names %q, and no Project of that name exists in namespace %s. "+
				"Apply the Project, or point spec.project at one that exists.", key.Name, env.Namespace))
	}

	// Step 1: validate. The pair is validated together, by the same function
	// the CLI and the server call, so the three produce identical codes, field
	// paths and remediation text (ADR-0027 decision 5).
	mp, me := modelProject(&project), modelEnvironment(&env)
	if errs := model.ValidateSet(mp, me); len(errs) > 0 {
		logger.Info("environment spec is invalid", "errors", len(errs))
		env.Status.ValidationErrors = validationErrors(errs)
		return r.halt(ctx, &env, base, v1alpha1.ReasonSpecInvalid, summarize(errs))
	}

	// The instance's declared sources, read before resolution because a
	// component may bind to one by name and resolution is what turns that name
	// into a repository (ADR-0035 decisions 2 and 3). It is the same join
	// internal/api does before a build, against the same GitSources: a
	// component that builds from `tools` and then fails to deploy with
	// `ref/unknown-source` is the two planes disagreeing about one binding.
	globals, err := r.globalSources(ctx)
	if err != nil {
		logger.Info("the instance's declared sources could not be listed", "error", err)
		if _, patchErr := r.halt(ctx, &env, base, v1alpha1.ReasonSourcesUnavailable,
			fmt.Sprintf("kelson could not read the GitSources this instance offers, so it cannot tell whether a "+
				"component binds to one: %v. Nothing was rendered — resolving against an empty global tier would "+
				"refuse a component bound to a GitSource as if the instance declared none. The controller retries "+
				"with backoff.", err)); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
		return ctrl.Result{}, fmt.Errorf("listing the instance's GitSources: %w", err)
	}

	// Step 3a: resolve. Resolution can still produce taxonomy errors — an image
	// that resolves to nothing, for instance — and they are the same kind of
	// answer as a validation failure, so they land in the same place.
	//
	// It runs even under a rollback, where the render below does not. Resolution
	// is pure and cheap, and the Kustomization needs the namespace and the
	// secret backend on every reconcile, including the ones where nothing is
	// rendered. What a rollback suspends is producing *new bytes to publish*,
	// which is the render and the push.
	resolved, errs := model.Resolve(mp, me, globals...)
	if len(errs) > 0 {
		logger.Info("environment spec does not resolve", "errors", len(errs))
		env.Status.ValidationErrors = validationErrors(errs)
		return r.halt(ctx, &env, base, v1alpha1.ReasonSpecInvalid, summarize(errs))
	}

	// Overlays are paths relative to the authoring documents, and a document
	// that arrived as a custom resource has no authoring directory — the same
	// wall internal/api hits and refuses by name (internal/api/pipeline.go's
	// checkOverlays). Rendering with a nil resolver would fail deep inside the
	// renderer; saying so here means the author is told "kelson cannot do this
	// yet" instead of being handed the renderer's internals.
	if len(resolved.Overlays) > 0 {
		return r.halt(ctx, &env, base, v1alpha1.ReasonRenderFailed,
			"spec.overlays are not supported for an Environment reconciled from the cluster: "+
				"overlay paths resolve against the spec files, and a custom resource has none. "+
				"Render this spec with the CLI, or remove the overlay.")
	}

	// Step 2: detect. A profile that cannot be read is a transient cluster
	// problem and therefore one of the few things in this function that IS an
	// error return: waiting and trying again is the right behaviour.
	//
	// It is also written into the status first, under its own reason. A probe
	// that failed is not a finding about the cluster, and the one thing it must
	// never be reported as is FluxNotInstalled — that reason names a fix
	// (`kelson install`) which is wrong here and would send an operator whose
	// Flux is fine to reinstall it.
	profile, err := r.Profiles.Profile(ctx)
	if err != nil {
		logger.Info("the cluster profile could not be read", "error", err)
		if _, patchErr := r.halt(ctx, &env, base, v1alpha1.ReasonClusterProfileUnavailable,
			fmt.Sprintf("kelson could not read what this cluster provides, so it cannot tell whether Flux "+
				"is installed or render against a profile it has not got: %v. The controller retries with "+
				"backoff and probes again each time.", err)); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
		return ctrl.Result{}, fmt.Errorf("reading the cluster profile: %w", err)
	}

	specHash, err := model.SpecHash(resolved)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("hashing the resolved spec: %w", err)
	}

	// Rollback (ADR-0028 decision 5), decided before the render because it
	// decides whether there is one.
	rb := rollbackFor(env.Annotations, env.Generation, env.Status)
	// The target's digest travels with the pin: it is what lets the
	// OCIRepository name the immutable bytes rather than only the tag that
	// points at them (fluxobjects.go).
	var target rollbackTarget
	if rb.Active {
		target, err = r.verifyRollbackTarget(ctx, project.Name, env.Name, rb.Requested, env.Status.History)
		if err != nil {
			return r.refuse(ctx, &env, base, err)
		}
	}

	var manifests []renderer.Manifest
	if !rb.Active {
		// Step 3b: render. Still pure — this reconciler is the caller that has
		// cluster access, the renderer has none (ADR-0028 decision 1, step 3).
		manifests, err = renderer.Render(resolved, profile, nil)
		if err != nil {
			// A render refusal is kelson's gap and not the author's mistake — an
			// unimplemented field, a capability the profile does not offer — so it
			// gets its own reason and, like an invalid spec, no requeue.
			logger.Info("render failed", "error", err)
			return r.halt(ctx, &env, base, v1alpha1.ReasonRenderFailed, err.Error())
		}
	}

	// Steps 4, 5 and 6.
	outcome, err := r.deliverer().Deliver(ctx, Revision{
		Project:              project.Name,
		Environment:          env.Name,
		EnvironmentNamespace: env.Namespace,
		TargetNamespace:      resolved.Environment.Namespace,
		Generation:           env.Generation,
		SpecHash:             specHash,
		Manifests:            manifests,
		Resolved:             resolved,
		FluxPresent:          profile.Flux != nil,
		Observed:             env.Status.Revision,
		ObservedDigest:       headDigest(env.Status.History, env.Status.Revision),
		PinnedTo:             rb.Pin(),
		PinnedDigest:         target.Digest,
	})
	if err != nil {
		return r.refuse(ctx, &env, base, err)
	}

	// A finalizer is added only now, on the first reconcile that got this far.
	// Adding it earlier would put a deletion blocker on an object that has
	// nothing to clean up, and an Environment whose spec never validated would
	// then need the finalizer stripped by hand before `kubectl delete` returned.
	//
	// The window between the apply and the finalizer is real, and losing the
	// race means the Flux pair outlives the object that declared it — see
	// [EnvironmentReconciler.abandon].
	if err := r.addFinalizer(ctx, &env); err != nil {
		if errors.Is(err, errEnvironmentGone) {
			return r.abandon(ctx, &env)
		}
		return ctrl.Result{}, err
	}
	// The finalizer patch moved the object on, and the status patch below locks
	// against the version it was computed from (patchStatus). This reconcile is
	// the writer that moved it, so what it observed is the new version.
	base.ResourceVersion = env.ResourceVersion

	previous := env.Status.Revision
	if outcome.Revision != "" {
		env.Status.Revision = outcome.Revision
	}
	env.Status.Phase = phaseFor(env.Status.Phase, previous, outcome)
	env.Status.History = recordHistory(env.Status.History, outcome, specHash, metav1.Now())
	// Wholesale, nil included (issue #240). A readback is a snapshot of one
	// moment, and keeping the last good one when this reconcile did not look
	// would produce a status whose phase and workload counts describe different
	// minutes — which is worse than a section that is simply absent.
	env.Status.Workloads = outcome.Workloads
	// Both the active and the inert case keep their bookkeeping: an inert
	// rollback that lost its status.rollbackGeneration would be re-read as a
	// *new* rollback on the next reconcile (case 1 of rollbackFor) and pin
	// again — the spec edit would publish once and flap back to the pinned
	// tag. Only a removed annotation clears the fields.
	env.Status.RollbackRevision, env.Status.RollbackGeneration = "", 0
	if rb.Active || rb.Inert {
		env.Status.RollbackRevision, env.Status.RollbackGeneration = rb.Requested, rb.Generation
	}

	r.setConditions(&env, rb, target, outcome, len(manifests))
	if err := patchStatus(ctx, r.Client, &env, base); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: r.requeueFor(env.Status.Phase)}, nil
}

// setConditions writes both conditions from one outcome, so Ready and
// Progressing can never disagree about what happened.
func (r *EnvironmentReconciler) setConditions(env *v1alpha1.Environment, rb rollback, target rollbackTarget,
	outcome Outcome, rendered int) {
	switch {
	case outcome.RolledBack:
		setReady(&env.Status.Conditions, env.Generation, metav1.ConditionTrue,
			v1alpha1.ReasonRolledBack, rolledBackMessage(outcome.Revision, target.BeyondWindow))
		setProgressing(&env.Status.Conditions, env.Generation, metav1.ConditionFalse,
			v1alpha1.ReasonRollbackPinned, rollbackPinnedMessage(outcome.Revision))
		return

	case outcome.Phase == v1alpha1.PhaseRejected, outcome.Phase == v1alpha1.PhaseDegraded:
		// Flux processed the change: it refused it, or it applied it and the
		// result is unhealthy. Either way the cause is Flux's own words, and
		// relaying them verbatim is what makes the status actionable.
		setReady(&env.Status.Conditions, env.Generation, metav1.ConditionFalse,
			v1alpha1.ReasonRenderFailed, outcome.Cause)

	case outcome.Phase == v1alpha1.PhaseHealthy:
		setReady(&env.Status.Conditions, env.Generation, metav1.ConditionTrue, v1alpha1.ReasonReady,
			fmt.Sprintf("revision %s is live and healthy (%s)", outcome.Revision, resourceCount(rendered)))

	case outcome.Revision != "":
		setReady(&env.Status.Conditions, env.Generation, metav1.ConditionTrue, v1alpha1.ReasonReady,
			fmt.Sprintf("revision %s is published and %s is reconciling it",
				outcome.Revision, defaultString(outcome.Phase, "Flux")))

	default:
		// Nothing was published: the no-op deliverer, or a build with delivery
		// unwired. The render still happened and is still worth reporting.
		setReady(&env.Status.Conditions, env.Generation, metav1.ConditionTrue, v1alpha1.ReasonReady,
			fmt.Sprintf("the environment is valid and renders %s", resourceCount(rendered)))
	}

	message := ""
	if rb.Inert {
		message = rollbackInertMessage(rb.Requested, rb.Generation)
	}
	if terminalPhase(outcome.Phase) {
		setProgressing(&env.Status.Conditions, env.Generation, metav1.ConditionFalse,
			v1alpha1.ReasonSettled, joinMessages("nothing is in flight for generation "+
				fmt.Sprint(env.Generation), message))
		return
	}
	setProgressing(&env.Status.Conditions, env.Generation, metav1.ConditionTrue,
		v1alpha1.ReasonReconciling, joinMessages(defaultString(outcome.Cause,
			"waiting for Flux to reconcile revision "+outcome.Revision), message))
}

// halt is the refusal that happens *before* delivery: an unresolvable Project,
// a spec that does not validate or resolve, an overlay the cluster path cannot
// serve, a render kelson cannot do.
//
// It writes both conditions, and that is the whole reason it exists. Ready
// alone is not enough: "settled" is read elsewhere as `Progressing exists and
// is not True` (internal/api's deploy stream), so a refusal that left a stale
// `Progressing=True` behind would hang a caller waiting on this generation for
// its whole budget and then report the timeout instead of the refusal — and on
// a fresh Environment, which has no Progressing condition at all, it would hang
// the same way for the opposite reason. Nothing is in flight after any of
// these, so the pair is written together (the same rule [refuse] follows for
// the delivery half of the taxonomy).
func (r *EnvironmentReconciler) halt(ctx context.Context, env *v1alpha1.Environment, base client.Object, reason, message string) (ctrl.Result, error) {
	setReady(&env.Status.Conditions, env.Generation, metav1.ConditionFalse, reason, message)
	setProgressing(&env.Status.Conditions, env.Generation, metav1.ConditionFalse,
		v1alpha1.ReasonSettled, message)
	return ctrl.Result{}, patchStatus(ctx, r.Client, env, base)
}

// refuse writes a delivery failure into the status and decides what happens
// next. It is the single place the taxonomy in errors.go is turned into a
// controller-runtime result, which is what makes "which reason requeues how"
// a table rather than a habit.
func (r *EnvironmentReconciler) refuse(ctx context.Context, env *v1alpha1.Environment, base client.Object, err error) (ctrl.Result, error) {
	de, ok := asDeliveryError(err)
	if !ok {
		// Not part of the taxonomy: treat it as transient and let
		// controller-runtime back off. An unclassified failure that stopped
		// retrying would be a deployment silently abandoned.
		return ctrl.Result{}, err
	}

	log.FromContext(ctx).Info("delivery refused", "reason", de.Reason, "message", de.Message)
	setReady(&env.Status.Conditions, env.Generation, metav1.ConditionFalse, de.Reason, de.Error())
	// Progressing is False for every refusal: nothing is in flight, whether or
	// not something will be retried.
	setProgressing(&env.Status.Conditions, env.Generation, metav1.ConditionFalse,
		v1alpha1.ReasonSettled, de.Message)
	if patchErr := patchStatus(ctx, r.Client, env, base); patchErr != nil {
		return ctrl.Result{}, patchErr
	}
	if de.Backoff {
		return ctrl.Result{}, de
	}
	return ctrl.Result{RequeueAfter: de.Retry}, nil
}

// finalize is the deletion path (ADR-0028, the 2026-08-14 amendment).
//
// Order is the decision: the Kustomization goes first, because deleting it is
// what removes the workloads — it prunes its own inventory on the way out.
// Deleting the OCIRepository first would leave the Kustomization pointing at a
// source that no longer exists, so it would stop reconciling with an error,
// prune nothing, and the workloads would outlive the Environment that declared
// them.
//
// Two things are deliberately not deleted:
//
//   - **The workload namespace.** Deleting a namespace cascades to everything
//     inside it, including resources kelson never created, and the labels alone
//     can never prove kelson created the namespace rather than adopting one
//     that already existed (internal/delivery/provenance.go's ownership
//     annotation exists precisely for this distinction). `kelson uninstall`
//     is the verb that reasons about namespaces; deleting a custom resource is
//     not.
//   - **The published artifacts.** They are the history (ADR-0028 decision 4)
//     and they are immutable. Deleting an Environment must not make its own
//     record unrecoverable, and re-applying the same spec then finds every
//     revision it ever published still there.
//
// # The escape hatch
//
// [v1alpha1.AnnotationOrphanOnDelete] skips the teardown entirely (issue #242).
// The finalizer still runs and still releases — the custom resource must never
// become undeletable — and the pair is left exactly as it stands, still labelled
// and still reconciling the last artifact kelson published. It is the
// environment-level form of the guarantee `kelson uninstall` already makes for
// the instance (issue #59): you can delete kelson's record of an application
// without deleting the application.
func (r *EnvironmentReconciler) finalize(ctx context.Context, env *v1alpha1.Environment) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(env, Finalizer) {
		return ctrl.Result{}, nil
	}
	if o := orphanFor(env.Annotations); o.Requested {
		r.announceOrphan(ctx, env, o)
		return ctrl.Result{}, r.release(ctx, env)
	}
	if err := r.deliverer().Teardown(ctx, env.Spec.Project, env.Name); err != nil {
		de, ok := asDeliveryError(err)
		if !ok || de.Backoff {
			return ctrl.Result{}, err
		}
		// A teardown that cannot proceed — RBAC, most likely — keeps the
		// finalizer and retries. The object stays in Terminating, which is the
		// honest state: its workloads are still running.
		log.FromContext(ctx).Info("teardown refused", "reason", de.Reason, "message", de.Message)
		return ctrl.Result{RequeueAfter: de.Retry}, nil
	}
	return ctrl.Result{}, r.release(ctx, env)
}

// release drops the deletion blocker, which is what lets the API server remove
// the object. A NotFound is success: something else got there first, and the
// state this is trying to reach is "the object is gone".
func (r *EnvironmentReconciler) release(ctx context.Context, env *v1alpha1.Environment) error {
	patch := client.MergeFrom(env.DeepCopy())
	controllerutil.RemoveFinalizer(env, Finalizer)
	if err := r.Client.Patch(ctx, env, patch); err != nil {
		return client.IgnoreNotFound(err)
	}
	return nil
}

// announceOrphan is the record a skipped teardown leaves.
//
// Both a log line and an event, because they are read by different people at
// different times: the log is what an operator watching the controller sees now,
// and the event is what is still in the cluster afterwards, when the Environment
// itself is gone and the only remaining question is why there is a Kustomization
// in kelson-system that nothing owns.
func (r *EnvironmentReconciler) announceOrphan(ctx context.Context, env *v1alpha1.Environment, o orphan) {
	logger := log.FromContext(ctx)
	message := orphanedMessage(env.Spec.Project, env.Name)
	logger.Info(message, "environment", env.Name, "namespace", env.Namespace,
		"annotation", v1alpha1.AnnotationOrphanOnDelete, "value", o.Value)
	r.event(env, corev1.EventTypeNormal, EventReasonOrphaned, message)

	if o.Malformed {
		malformed := orphanMalformedMessage(o.Value)
		logger.Info(malformed, "environment", env.Name, "namespace", env.Namespace)
		r.event(env, corev1.EventTypeWarning, EventReasonOrphanAnnotationMalformed, malformed)
	}
}

// event records one, or does nothing when no recorder is wired.
func (r *EnvironmentReconciler) event(env *v1alpha1.Environment, eventType, reason, message string) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Event(env, eventType, reason, message)
}

// errEnvironmentGone reports that the Environment was deleted between the apply
// and the finalizer, so there is nothing left to protect and something left to
// clean up. See [EnvironmentReconciler.abandon].
var errEnvironmentGone = errors.New("the environment was deleted before the finalizer was added")

// addFinalizer puts the deletion blocker on, once, after the first successful
// Ensure. The patch is against the object and not the status subresource,
// because a finalizer is metadata.
//
// It patches a *copy*. A client patch writes the API server's response back
// over the object it was handed, which would discard the status this reconcile
// has spent the whole function assembling and has not written yet — a bug that
// shows up as an environment whose observedGeneration never moves. So only the
// two fields the patch actually changed are carried back.
//
// A NotFound, or an object that came back mid-deletion, is [errEnvironmentGone]
// rather than a swallowed error: it is the one window in which the pair this
// reconcile just applied has no owner and no sweeper.
func (r *EnvironmentReconciler) addFinalizer(ctx context.Context, env *v1alpha1.Environment) error {
	if controllerutil.ContainsFinalizer(env, Finalizer) {
		return nil
	}
	patched := env.DeepCopy()
	patch := client.MergeFrom(env.DeepCopy())
	controllerutil.AddFinalizer(patched, Finalizer)
	if err := r.Client.Patch(ctx, patched, patch); err != nil {
		if apierrors.IsNotFound(err) {
			return errEnvironmentGone
		}
		// The API server forbids adding a finalizer to an object that is
		// already terminating, so the same race can arrive as a refused write
		// rather than as a 404. One re-read tells the two apart.
		if terminating, checkErr := r.isTerminating(ctx, env); checkErr == nil && terminating {
			return errEnvironmentGone
		}
		return err
	}
	if !patched.DeletionTimestamp.IsZero() {
		return errEnvironmentGone
	}
	env.Finalizers, env.ResourceVersion = patched.Finalizers, patched.ResourceVersion
	return nil
}

// isTerminating re-reads the object to tell "gone or going" from "the write was
// refused for some other reason". A read failure is reported as such: guessing
// would either leak the pair or tear down a live environment, and both are
// worse than one more retry.
func (r *EnvironmentReconciler) isTerminating(ctx context.Context, env *v1alpha1.Environment) (bool, error) {
	var live v1alpha1.Environment
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Name}
	if err := r.Client.Get(ctx, key, &live); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}
	return !live.DeletionTimestamp.IsZero(), nil
}

// abandon tears down the pair this reconcile just applied for an Environment
// that no longer exists.
//
// The finalizer is what normally guarantees the teardown, and it is deliberately
// added only after the first successful apply (ADR-0028's amendment). That
// leaves one window: a delete that lands between the apply and the finalizer
// removes the custom resource immediately — there is nothing to block it — and
// [EnvironmentReconciler.finalize] will never run, because the deletion produces
// one reconcile of an object the client can no longer read. The `prune: true`
// Kustomization and everything it applied would keep running with nothing in the
// cluster saying whose they were.
//
// So the reconcile that lost the race does the teardown itself, in the same
// order finalize uses. A failure is returned rather than swallowed: it is the
// one thing here a log has to show, since no later reconcile of a deleted object
// will retry it.
//
// The escape hatch is honoured here too, and it has to be: this path exists
// precisely because [EnvironmentReconciler.finalize] never ran, so an operator
// who annotated the Environment and then deleted it would otherwise have their
// application torn down by whichever of the two paths won a race they cannot
// see (issue #242).
func (r *EnvironmentReconciler) abandon(ctx context.Context, env *v1alpha1.Environment) (ctrl.Result, error) {
	if o := orphanFor(env.Annotations); o.Requested {
		r.announceOrphan(ctx, env, o)
		return ctrl.Result{}, nil
	}
	log.FromContext(ctx).Info("the environment was deleted before the finalizer was added; "+
		"tearing the pair down from this reconcile", "environment", env.Name, "namespace", env.Namespace)
	if err := r.deliverer().Teardown(ctx, env.Spec.Project, env.Name); err != nil {
		return ctrl.Result{}, fmt.Errorf("tearing down the Flux pair of a deleted environment: %w", err)
	}
	return ctrl.Result{}, nil
}

// requeueFor is the belt-and-braces poll. A settled phase needs none: the watch
// on the Flux objects reports the next change, and a timer on a healthy
// environment is load with no question behind it.
func (r *EnvironmentReconciler) requeueFor(phase string) time.Duration {
	if phase == "" || terminalPhase(phase) {
		return 0
	}
	return nonTerminalRequeue
}

// terminalPhase reports whether Flux is done with this revision, well or badly.
// Degraded counts: it is live, it is wrong, and nothing kelson does next
// changes that — only a new spec or a recovering workload will, and both are
// watch events.
func terminalPhase(phase string) bool {
	switch phase {
	case v1alpha1.PhaseHealthy, v1alpha1.PhaseRejected, v1alpha1.PhaseDegraded:
		return true
	default:
		return false
	}
}

// headDigest finds the digest already recorded for a revision, so a reconcile
// that skips the publish does not drop it out of the history.
func headDigest(history []v1alpha1.HistoryEntry, revision string) string {
	for _, e := range history {
		if e.Revision == revision {
			return e.Digest
		}
	}
	return ""
}

func defaultString(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func joinMessages(primary, extra string) string {
	if extra == "" {
		return primary
	}
	return primary + ". " + extra
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

// globalSources reads the instance's tier for one reconcile.
//
// A failure is reported rather than swallowed, which is the rule internal/api
// states at its own copy of this function and the reason this one exists at
// all: resolving against an empty global tier after a failed listing would
// refuse a component bound to a GitSource with `ref/unknown-source` — "this
// instance offers: nothing" — which is a truthful sentence about a lookup that
// failed and a false one about the instance. An environment told its spec is
// invalid would then be told to edit a spec that is fine, and the edit would
// not help.
//
// A nil lister is not that case: it is a controller wired without a global
// tier, which resolves projects against their own declared sources and is the
// pre-ADR-0035 posture (see [GitSourceLister]).
func (r *EnvironmentReconciler) globalSources(ctx context.Context) ([]model.Source, error) {
	if r.Sources == nil {
		return nil, nil
	}
	return r.Sources.ListSources(ctx)
}

// SetupWithManager registers the reconciler and the watches it needs beyond its
// own kind.
//
// # The Project watch
//
// A change to a Project changes what its Environments validate and render
// against, so each one has to be re-reconciled — otherwise an Environment
// refused for a missing Project would stay refused after the Project was
// applied, until something else happened to touch it. The map function lists
// Environments in the Project's namespace and filters by spec.project; a field
// index would be faster and is not worth its setup at this size, since the list
// is served from the same cache the reconciler already has.
//
// # The Flux watches, and why they are conditional
//
// Step 6 is watch-driven: a Kustomization going Ready is what turns an
// Environment's phase to Healthy, and without the watch that transition would
// wait for a timer. But an informer on a CRD the API server does not serve does
// not fail — it blocks the manager's cache sync, forever, and the whole
// controller never becomes ready. A cluster with no Flux is exactly the cluster
// ADR-0030 exists for, so registering those watches unconditionally would make
// the controller unable to start on the clusters it most needs to start on and
// report `FluxNotInstalled` from.
//
// So `fluxPresent` is a start-up finding (cmd/kelson-controller does one
// detection before building the manager) and the watches are gated on it. The
// cost is real and is stated where it is paid: a cluster where Flux is
// installed *after* the controller starts keeps reconciling on the requeue
// timer until the controller is restarted. That is a restart after an install,
// which is a thing an operator is already doing.
func (r *EnvironmentReconciler) SetupWithManager(mgr ctrl.Manager, fluxPresent bool) error {
	if r.Profiles == nil {
		r.Profiles = StaticProfileSource{}
	}
	// The manager already holds a recorder and the chart already grants what it
	// writes, so this is defaulted here rather than passed in: a binary that
	// forgot to wire it would silently lose the only trace an orphaned pair
	// leaves in the cluster.
	//
	// The deprecated accessor is the deliberate one. It records core v1 Events,
	// which is exactly the grant the chart's controller ClusterRole carries and
	// the API the manager's leader election already writes to. Its replacement,
	// GetEventRecorder, writes events.k8s.io/v1 — a different API group, so
	// switching would need a second RBAC rule before a single event landed, and
	// that is a chart change to make on purpose rather than a side effect of
	// this one.
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("kelson-controller") //nolint:staticcheck // see above
	}
	r.FluxWatches = fluxPresent

	builder := ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Environment{}).
		Watches(&v1alpha1.Project{}, handler.EnqueueRequestsFromMapFunc(r.environmentsOfProject)).
		Named("environment")

	// The GitSource watch, for the reason the Project watch exists: a component
	// bound to a source the instance had not declared yet is refused with
	// `ref/unknown-source`, and that refusal does not requeue — so without this,
	// applying the GitSource would fix nothing until somebody edited the spec or
	// the controller restarted. It is gated on the lister because a controller
	// with no global tier does not read GitSources at all, and a watch whose
	// events cannot change an answer is load with no question behind it.
	if r.Sources != nil {
		builder = builder.Watches(&v1alpha1.GitSource{},
			handler.EnqueueRequestsFromMapFunc(r.environmentsOfGitSource))
	}

	if fluxPresent {
		for _, gvk := range []struct{ group, version, kind string }{
			{ociRepositoryGVK.Group, ociRepositoryGVK.Version, ociRepositoryGVK.Kind},
			{kustomizationGVK.Group, kustomizationGVK.Version, kustomizationGVK.Kind},
		} {
			obj := &unstructured.Unstructured{}
			obj.SetAPIVersion(gvk.group + "/" + gvk.version)
			obj.SetKind(gvk.kind)
			builder = builder.Watches(obj, handler.EnqueueRequestsFromMapFunc(environmentOfFluxObject))
		}
	}
	return builder.Complete(r)
}

// environmentOfFluxObject maps a Flux object back to the Environment that owns
// it.
//
// The mapping is the labels and nothing else. It cannot be an owner reference:
// the Flux objects live in the Flux namespace and the Environment lives in the
// application's, and Kubernetes garbage collection does not cross namespaces —
// a cross-namespace owner reference is not merely unsupported, it makes the
// dependent eligible for deletion as an orphan. Hence the finalizer, and hence
// this: [delivery.LabelEnvironmentNamespace] and [delivery.LabelEnvironment]
// are exactly the namespace and name of the object to enqueue.
func environmentOfFluxObject(_ context.Context, obj client.Object) []reconcile.Request {
	labels := obj.GetLabels()
	if labels[delivery.LabelManagedBy] != delivery.ManagedByKelson {
		return nil
	}
	namespace, name := labels[delivery.LabelEnvironmentNamespace], labels[delivery.LabelEnvironment]
	if namespace == "" || name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: namespace, Name: name}}}
}

// environmentsOfGitSource enqueues the Environments whose render could change
// because one of the instance's declared sources did.
//
// A GitSource is instance-wide, so the naive mapping is "every Environment in
// the cluster". This narrows it to the Projects that actually have the source in
// scope — a component bound to that name, and no project-local source shadowing
// it (ADR-0035 decision 3) — because the alternative is re-reconciling every
// environment on the cluster each time an operator edits one global source, and
// almost none of them are about it.
//
// A deleted GitSource maps the same way, deliberately: the environments bound to
// it are exactly the ones that must now report that their binding resolves
// nowhere.
//
// Both lists are served from the manager's cache, which already holds kelson's
// own kinds cluster-wide, so this costs no API call.
func (r *EnvironmentReconciler) environmentsOfGitSource(ctx context.Context, source client.Object) []reconcile.Request {
	logger := log.FromContext(ctx)
	var projects v1alpha1.ProjectList
	if err := r.Client.List(ctx, &projects); err != nil {
		logger.Error(err, "listing projects for a git source change", "gitsource", source.GetName())
		return nil
	}
	bound := map[types.NamespacedName]bool{}
	for i := range projects.Items {
		p := &projects.Items[i]
		if bindsGlobalSource(p.Spec, source.GetName()) {
			bound[types.NamespacedName{Namespace: p.Namespace, Name: p.Name}] = true
		}
	}
	if len(bound) == 0 {
		return nil
	}

	var envs v1alpha1.EnvironmentList
	if err := r.Client.List(ctx, &envs); err != nil {
		logger.Error(err, "listing environments for a git source change", "gitsource", source.GetName())
		return nil
	}
	var out []reconcile.Request
	for i := range envs.Items {
		env := &envs.Items[i]
		if !bound[types.NamespacedName{Namespace: env.Namespace, Name: env.Spec.Project}] {
			continue
		}
		out = append(out, reconcile.Request{
			NamespacedName: types.NamespacedName{Namespace: env.Namespace, Name: env.Name},
		})
	}
	return out
}

// bindsGlobalSource reports whether a GitSource of this name is in the Project's
// scope: some component binds to the name, and the Project declares no source of
// its own that shadows it. It is the resolver's rule
// (internal/model's resolveSources) asked the other way round — which projects
// does *this* source answer for — and it stays a pure function of the spec so
// the two cannot drift into different answers.
func bindsGlobalSource(spec model.ProjectSpec, name string) bool {
	if name == "" {
		return false
	}
	for _, src := range spec.EffectiveSources() {
		if src.Name == name {
			return false
		}
	}
	for _, c := range spec.Components {
		if c.SourceName() == name {
			return true
		}
	}
	return false
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
