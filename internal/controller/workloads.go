package controller

import (
	"context"
	"fmt"
	"sort"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dafrie/kelson/api/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/observation"
)

// The second half of ADR-0028 step 6 (issue #240): after the Kustomization's
// own condition, the workloads it applied.
//
// # What Flux already answers, and what it does not
//
// The Kustomization kelson writes carries `wait: true`, so `Ready=True` means
// kustomize-controller waited for the applied set to converge and it did. That
// is a *correct* health signal and it is why the controller shipped without
// this. What it is not is a *diagnosable* one: `Ready=False` with "health check
// failed after 5m0s" tells an operator that something did not come up and
// nothing about which thing, which pod, or why — the three commands they then
// have to run, in a namespace they have to work out kelson chose.
//
// # Why the classifier is imported and the reading is not
//
// internal/observation already knows how to turn a Deployment and its pods into
// CrashLoopBackOff / ImagePullBackOff / failing-probe (issue #53). Its own
// reader, observation.Probe, takes a dynamic client, and this plane's client is
// controller-runtime's — so what crosses the boundary is
// [observation.Classify], which is pure and takes objects. Neither fence moved
// for this: internal/observation gains no controller-runtime, internal/controller
// gains no dynamic client, and there is exactly one definition of what
// "crash-loop-back-off" means in this repository.
//
// # Read-only, and it may not fail a deploy
//
// The readback never writes and never returns an error that reaches
// [EnvironmentReconciler.Reconcile] as one. A revision that published and
// applied has been delivered; discovering that the controller may not list pods
// is a gap in the *status*, reported as such
// ([v1alpha1.WorkloadsStatus.Unavailable]), and refusing the deploy over it
// would put an RBAC mistake between a user and production.

// WorkloadObserver reads back the workloads one revision applied and reports
// what they are doing.
//
// It is a seam for the reason [PreviewSecrets] is one beside it: [FluxDeliverer]
// must be testable without a cluster full of pods, and the reconciler's own
// tests fake the whole [Deliverer] above it. A nil one is a controller that does
// not read workloads back, which is the behaviour before this existed and is
// still the right posture for an operator who has not granted the RBAC.
type WorkloadObserver interface {
	// Observe classifies the workloads of one environment. It returns the
	// readback or an error; an error is turned into
	// [v1alpha1.WorkloadsStatus.Unavailable] by the caller, never propagated
	// into the delivery result.
	Observe(ctx context.Context, rev Revision) (*v1alpha1.WorkloadsStatus, error)
}

// The kinds the readback covers. Deployments and their pods, and that is the
// whole list, because [observation.Classify] classifies a Deployment against
// the pods in its selector set and nothing else.
//
// The renderer emits more than that — a CronJob, a release Job, a CNPG Cluster,
// a ValkeyCluster — and each of those has an operator of its own with its own
// notion of health. Reading them here would mean writing a second classifier
// per kind, in the controller, for kinds `kelson status` does not classify
// either. They stay covered by the Kustomization's `wait: true`, which is a
// coarser answer and a correct one, and the RBAC stays at the two resources
// something actually reads (deploy/chart/kelson/templates/controller-clusterrole.yaml).
var (
	deploymentGVK = schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "DeploymentList"}
	podGVK        = schema.GroupVersionKind{Version: "v1", Kind: "PodList"}
)

// WorkloadReader is the slice of a Kubernetes client the readback needs: one
// namespaced, label-selected list, over kinds it does not have typed structs
// for.
//
// It is declared rather than taking a client.Client so a test drives one method
// with fixtures instead of faking a reader, a writer and a status writer — the
// same reason [SecretClient] exists.
type WorkloadReader interface {
	// List returns the objects of one kind in a namespace carrying every label
	// in match, in name order. An empty match must list nothing rather than
	// everything (see [ClientWorkloads.List]).
	List(ctx context.Context, listGVK schema.GroupVersionKind, namespace string, match map[string]string) ([]*unstructured.Unstructured, error)
}

// ClientWorkloads adapts a controller-runtime client to [WorkloadReader].
//
// The client handed here should be a *direct* one and not the manager's, and
// this is the sharpest case of that rule in the package. The manager's cache
// starts an informer for every kind read through it, so a cached read here
// would put every Pod in the cluster in this process's memory and re-reconcile
// on every pod event in it — the same argument [ClientSecrets] makes about
// Secrets, at a much larger scale. cmd/kelson-controller builds one from the
// same rest.Config.
func ClientWorkloads(c client.Client) WorkloadReader { return clientWorkloads{c: c} }

type clientWorkloads struct{ c client.Client }

func (w clientWorkloads) List(ctx context.Context, listGVK schema.GroupVersionKind, namespace string, match map[string]string) ([]*unstructured.Unstructured, error) {
	// An empty selector is not "match nothing" to the API server, it is "match
	// everything". Refusing it here is what keeps a Deployment that declares no
	// selector from being classified against every pod in its namespace —
	// somebody else's failures reported as this environment's.
	if len(match) == 0 {
		return nil, nil
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(listGVK)
	if err := w.c.List(ctx, list, client.InNamespace(namespace), client.MatchingLabels(match)); err != nil {
		return nil, err
	}
	out := make([]*unstructured.Unstructured, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, &list.Items[i])
	}
	// Name order, because [observation.Classify] considers pods in the order it
	// is given them and the API server's order is not a promise.
	sort.Slice(out, func(i, j int) bool { return out[i].GetName() < out[j].GetName() })
	return out, nil
}

// ClusterWorkloads is the production [WorkloadObserver].
type ClusterWorkloads struct {
	// Reader lists the Deployments and the Pods. Required.
	Reader WorkloadReader
}

var _ WorkloadObserver = ClusterWorkloads{}

// Observe lists this environment's Deployments by kelson's own provenance
// labels, lists each one's pods by its own selector, and classifies the pair.
//
// The provenance labels are the correlation and not the namespace: an
// environment's target namespace may hold workloads kelson did not apply — an
// adopted namespace is the case ADR-0003 is built around — and classifying
// those would report somebody else's broken Deployment as this environment's.
func (w ClusterWorkloads) Observe(ctx context.Context, rev Revision) (*v1alpha1.WorkloadsStatus, error) {
	if w.Reader == nil {
		return nil, fmt.Errorf("no workload reader is configured")
	}
	deployments, err := w.Reader.List(ctx, deploymentGVK, rev.TargetNamespace, map[string]string{
		delivery.LabelManagedBy:   delivery.ManagedByKelson,
		delivery.LabelProject:     rev.Project,
		delivery.LabelEnvironment: rev.Environment,
	})
	if err != nil {
		return nil, fmt.Errorf("listing the workloads in %s: %w", rev.TargetNamespace, err)
	}

	var out v1alpha1.WorkloadsStatus
	var unhealthy []v1alpha1.UnhealthyWorkload
	for _, dep := range deployments {
		pods, err := w.Reader.List(ctx, podGVK, dep.GetNamespace(), observation.PodSelector(dep))
		if err != nil {
			return nil, fmt.Errorf("listing the pods of %s/%s: %w", dep.GetNamespace(), dep.GetName(), err)
		}
		verdict := observation.Classify(dep, pods)
		out.Checked++
		switch {
		case verdict.Healthy:
			out.Healthy++
		case observation.IsFailure(verdict.Code):
			out.Degraded++
			unhealthy = append(unhealthy, unhealthyWorkload(verdict))
		default:
			out.Progressing++
		}
	}

	sort.Slice(unhealthy, func(i, j int) bool { return unhealthy[i].Resource < unhealthy[j].Resource })
	if len(unhealthy) > v1alpha1.MaxUnhealthyWorkloads {
		unhealthy = unhealthy[:v1alpha1.MaxUnhealthyWorkloads]
	}
	out.Unhealthy = unhealthy
	return &out, nil
}

// unhealthyWorkload copies one verdict into the status shape, dropping what
// must not travel: [observation.Container] carries Logs and LogError, and
// neither has a field here (see [v1alpha1.WorkloadsStatus]).
//
// The containers arrive from the classifier already ordered by pod then
// container name, so the bound takes a prefix of a stable list rather than a
// sample that moves between reconciles.
func unhealthyWorkload(v observation.Verdict) v1alpha1.UnhealthyWorkload {
	out := v1alpha1.UnhealthyWorkload{
		Resource:    v.Resource,
		Code:        string(v.Code),
		Reason:      v.Reason,
		Remediation: v.Remediation,
	}
	containers := v.Containers
	if len(containers) > v1alpha1.MaxUnhealthyContainers {
		containers = containers[:v1alpha1.MaxUnhealthyContainers]
	}
	for _, c := range containers {
		out.Containers = append(out.Containers, v1alpha1.UnhealthyContainer{
			Pod:    c.Pod,
			Name:   c.Name,
			Code:   string(c.Code),
			Reason: c.Reason,
		})
	}
	return out
}

// workloadCause is the sentence a degraded readback puts on the Ready
// condition, in place of the Kustomization's own "health check failed".
//
// It names one workload and not all of them for the reason
// [EnvironmentReconciler] summarizes validation errors: a condition message the
// API server truncates is truncated at a point nobody chose. The first entry is
// the alphabetically-first failing workload, which is a stable choice rather
// than a ranked one, and `status.workloads` is where the rest is.
func workloadCause(w *v1alpha1.WorkloadsStatus) string {
	if w == nil || len(w.Unhealthy) == 0 {
		return ""
	}
	first := w.Unhealthy[0]
	cause := fmt.Sprintf("%s is %s", first.Resource, first.Code)
	if first.Reason != "" {
		cause += " (" + first.Reason + ")"
	}
	if len(first.Containers) > 0 {
		c := first.Containers[0]
		cause += fmt.Sprintf(", container %s in pod %s", c.Name, c.Pod)
	}
	if w.Degraded > 1 {
		cause += fmt.Sprintf(" — and %d more in status.workloads", w.Degraded-1)
	}
	return cause
}
