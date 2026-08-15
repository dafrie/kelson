package flux

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// DefaultNamespace is where Flux and flux-operator conventionally run. It is
// only a default for the controller-health fallback; the CRs kelson reads live
// wherever the user put them and are listed across all namespaces.
const DefaultNamespace = "flux-system"

// The Flux API coordinates kelson reads. Pinned versions rather than discovery
// lookups, for the same reason internal/clusterprofile/detect pins its own:
// these groups are stable, and the renderer's own preview Kustomization
// already writes kustomize/source v1 (internal/renderer/previews.go), so a
// version this adapter could read but not write would be the inconsistency.
//
// KustomizationGVR and OCIRepositoryGVR are exported because the controller
// (internal/controller) *writes* those two objects, and the version it writes
// must be the version this package reads back (ADR-0028 decision 3). Two
// spellings of "source.toolkit.fluxcd.io/v1" would be one bump away from a
// controller that applies to a version its own observer does not watch.
var (
	// KustomizationGVR is kustomize-controller's Kustomization: the object
	// kelson owns per environment, and the one whose Ready condition means the
	// deployment is healthy.
	KustomizationGVR = schema.GroupVersionResource{Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Resource: "kustomizations"}
	// OCIRepositoryGVR is source-controller's OCIRepository: the other half of
	// the pair, pinned to the artifact tag kelson just published.
	OCIRepositoryGVR = schema.GroupVersionResource{Group: "source.toolkit.fluxcd.io", Version: "v1", Resource: "ocirepositories"}

	kustomizationGVR = KustomizationGVR
	gitRepositoryGVR = schema.GroupVersionResource{Group: "source.toolkit.fluxcd.io", Version: "v1", Resource: "gitrepositories"}
	helmReleaseGVR   = schema.GroupVersionResource{Group: "helm.toolkit.fluxcd.io", Version: "v2", Resource: "helmreleases"}
	deploymentGVR    = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}

	// fluxReportGVR is flux-operator's cluster-wide report on the Flux
	// installation. LICENSE: flux-operator is AGPL-3.0 and kelson is MIT, so
	// integration is CR-only — the GVR and the field paths below are written by
	// hand and kelson never imports a flux-operator Go module
	// (docs/architecture.md "Living with flux-operator", issue #137).
	fluxReportGVR = schema.GroupVersionResource{Group: "fluxcd.controlplane.io", Version: "v1", Resource: "fluxreports"}
)

// The kinds behind the two GVRs kelson writes. A GVR names a resource and an
// apply names a kind, so both spellings are needed and both live here.
const (
	KindKustomization = "Kustomization"
	KindOCIRepository = "OCIRepository"
)

// DynamicStatusReader reads Flux objects with the Kubernetes dynamic client,
// the same client the direct adapter applies with and the same one
// internal/clusterprofile/detect probes with (issue #137). Unstructured reads
// rather than typed clients: the Flux (and flux-operator) API types would each
// drag their own module and API-version pin into a control plane that only
// needs six fields out of each object.
type DynamicStatusReader struct {
	// Client is the dynamic client, built once by internal/delivery/kube.Connect
	// and shared with the other adapters in the plane.
	Client dynamic.Interface
	// Namespace limits the query; empty means all namespaces, which is the
	// right default because a Kustomization may live anywhere.
	Namespace string
	// FluxOperator is the ClusterProfile's finding about flux-operator, when a
	// profile was captured at all (clusterprofile.ClusterProfile.FluxOperator,
	// issue #157). true reads the FluxReport, false goes straight to the
	// Deployment aggregate and spends no round trip on a CRD the cluster does
	// not serve.
	//
	// Nil means nobody looked — no profile, or a probe that could not see the
	// group — and the reader then probes and falls back, because absence in a
	// profile that was never captured is not a finding. Health is advisory in
	// every case: this field only changes which source is asked first, never
	// the phase a caller ends up with.
	FluxOperator *bool
}

var (
	_ StatusReader = DynamicStatusReader{}
	_ HealthReader = DynamicStatusReader{}
)

func (r DynamicStatusReader) list(ctx context.Context, gvr schema.GroupVersionResource) (*unstructured.UnstructuredList, error) {
	if r.Client == nil {
		return nil, errors.New("no dynamic client is configured")
	}
	if r.Namespace != "" {
		return r.Client.Resource(gvr).Namespace(r.Namespace).List(ctx, metav1.ListOptions{})
	}
	return r.Client.Resource(gvr).List(ctx, metav1.ListOptions{})
}

// Kustomizations implements StatusReader.
func (r DynamicStatusReader) Kustomizations(ctx context.Context) ([]Kustomization, error) {
	list, err := r.list(ctx, kustomizationGVR)
	if err != nil {
		return nil, readErr("Kustomizations", err)
	}

	sources, err := r.gitRepositories(ctx)
	if err != nil {
		// Source resolution is best effort: without it kelson matches on path
		// alone, which is still correct for single-repo installs.
		sources = nil
	}

	ks := make([]Kustomization, 0, len(list.Items))
	for i := range list.Items {
		obj := list.Items[i].Object
		k := KustomizationFrom(obj, list.Items[i].GetName(), list.Items[i].GetNamespace())

		// A sourceRef without a namespace names an object in the
		// Kustomization's own namespace, which is Flux's rule and not something
		// the reader may guess differently.
		ns := k.Namespace
		if v, _, _ := unstructured.NestedString(obj, "spec", "sourceRef", "namespace"); v != "" {
			ns = v
		}
		if src, ok := sources[ns+"/"+k.SourceName]; ok {
			k.SourceURL, k.SourceBranch = src.url, src.branch
		}
		ks = append(ks, k)
	}
	return ks, nil
}

// KustomizationFrom reads the slice of a live Kustomization kelson cares about
// out of its unstructured content.
//
// It is exported and separate from the list loop above because there are now
// two readers of the same object and they arrive at it differently: this
// package lists Kustomizations across a cluster with the dynamic client, and
// internal/controller reads back the single one it owns through
// controller-runtime's cache (ADR-0028 decision 1, step 6). Extracting the
// field paths is what stops the second reader from growing its own opinion
// about where `lastAppliedRevision` lives.
//
// The name and namespace are arguments rather than read from the content
// because both callers already hold them, and an object fetched through a typed
// path may carry them only in its metadata wrapper.
//
// Source resolution is not here: it needs a second list, which is a property of
// how a caller reads rather than of what a Kustomization says.
func KustomizationFrom(obj map[string]any, name, namespace string) Kustomization {
	k := Kustomization{Name: name, Namespace: namespace, Ready: ConditionUnknown}
	k.Path, _, _ = unstructured.NestedString(obj, "spec", "path")
	k.Suspended, _, _ = unstructured.NestedBool(obj, "spec", "suspend")
	k.SourceKind, _, _ = unstructured.NestedString(obj, "spec", "sourceRef", "kind")
	k.SourceName, _, _ = unstructured.NestedString(obj, "spec", "sourceRef", "name")
	k.LastAppliedRevision, _, _ = unstructured.NestedString(obj, "status", "lastAppliedRevision")
	k.LastAttemptedRevision, _, _ = unstructured.NestedString(obj, "status", "lastAttemptedRevision")

	for _, c := range conditionsOf(obj) {
		switch c.kind {
		case "Ready":
			k.Ready, k.Reason, k.Message = ConditionState(c.status), c.reason, c.message
		case "Reconciling":
			k.Reconciling = c.status == string(ConditionTrue)
		}
	}
	return k
}

type gitSource struct{ url, branch string }

func (r DynamicStatusReader) gitRepositories(ctx context.Context) (map[string]gitSource, error) {
	list, err := r.list(ctx, gitRepositoryGVR)
	if err != nil {
		return nil, err
	}
	m := make(map[string]gitSource, len(list.Items))
	for i := range list.Items {
		obj := list.Items[i].Object
		var src gitSource
		src.url, _, _ = unstructured.NestedString(obj, "spec", "url")
		src.branch, _, _ = unstructured.NestedString(obj, "spec", "ref", "branch")
		m[list.Items[i].GetNamespace()+"/"+list.Items[i].GetName()] = src
	}
	return m, nil
}

// HelmReleases implements StatusReader.
func (r DynamicStatusReader) HelmReleases(ctx context.Context) ([]HelmRelease, error) {
	list, err := r.list(ctx, helmReleaseGVR)
	if err != nil {
		return nil, readErr("HelmReleases", err)
	}
	hrs := make([]HelmRelease, 0, len(list.Items))
	for i := range list.Items {
		hr := HelmRelease{
			Name:      list.Items[i].GetName(),
			Namespace: list.Items[i].GetNamespace(),
			Ready:     ConditionUnknown,
		}
		for _, c := range conditionsOf(list.Items[i].Object) {
			if c.kind == "Ready" {
				hr.Ready, hr.Reason, hr.Message = ConditionState(c.status), c.reason, c.message
			}
		}
		hrs = append(hrs, hr)
	}
	return hrs, nil
}

// Health implements HealthReader, preferring flux-operator's FluxReport where
// the cluster has one: the operator already aggregates distribution version and
// per-controller readiness, and re-deriving that from Deployments would be
// kelson guessing at an answer another component publishes (docs/roadmap.md,
// "Delegate to flux-operator"). The Deployment aggregate stays as the fallback
// for the majority of clusters that run Flux without the operator.
//
// Which source is tried first is a detection answer where one exists
// (FluxOperator, issue #157) rather than something this reader establishes by
// attempting the read: probing to find out what a ClusterProfile already
// records inverts the detection model (ADR-0003). Where no profile was
// captured the probe-and-fallback remains, so a caller that never asked for
// detection still gets an answer.
func (r DynamicStatusReader) Health(ctx context.Context) (Health, error) {
	var reportErr error
	if r.FluxOperator == nil || *r.FluxOperator {
		h, err := r.fluxReport(ctx)
		if err == nil {
			return h, nil
		}
		// A profile that says the operator is there and a read that fails
		// anyway is still not a health failure: the fallback answers.
		reportErr = err
	}
	h, err := r.controllerHealth(ctx)
	if err != nil {
		return Health{}, readErr("health", errors.Join(reportErr, err))
	}
	return h, nil
}

// fluxReport reads flux-operator's FluxReport. LICENSE (issue #137):
// flux-operator is AGPL-3.0 and kelson is MIT, so this reads the CR as
// unstructured data — GVR above, field paths below, no module import.
func (r DynamicStatusReader) fluxReport(ctx context.Context) (Health, error) {
	list, err := r.list(ctx, fluxReportGVR)
	if err != nil {
		return Health{}, err
	}
	if len(list.Items) == 0 {
		return Health{}, errors.New("no FluxReport found: flux-operator is not installed")
	}
	// The operator publishes one report per cluster; if an install ever had
	// several, the first in list order is as good an answer as any and the
	// alternative (merging them) would invent a state no component reports.
	obj := list.Items[0].Object

	h := Health{Source: HealthFromReport}
	h.Version, _, _ = unstructured.NestedString(obj, "spec", "distribution", "version")
	distribution, _, _ := unstructured.NestedString(obj, "spec", "distribution", "status")

	components, _, _ := unstructured.NestedSlice(obj, "spec", "components")
	for _, c := range components {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if ready, _, _ := unstructured.NestedBool(m, "ready"); ready {
			continue
		}
		name, _, _ := unstructured.NestedString(m, "name")
		h.Unready = append(h.Unready, name)
	}
	sort.Strings(h.Unready)

	h.Ready = len(h.Unready) == 0
	h.Message = healthMessage(h, distribution)
	return h, nil
}

// controllerHealth aggregates the Flux controller Deployments by hand. It is
// the fallback for a Flux installed without flux-operator, and reports the
// same shape so callers never branch on where the answer came from.
func (r DynamicStatusReader) controllerHealth(ctx context.Context) (Health, error) {
	if r.Client == nil {
		return Health{}, errors.New("no dynamic client is configured")
	}
	ns := r.Namespace
	if ns == "" {
		ns = DefaultNamespace
	}
	// Every Flux controller Deployment carries this label; without it the
	// aggregate would report on whatever else shares the namespace.
	list, err := r.Client.Resource(deploymentGVR).Namespace(ns).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/part-of=flux",
	})
	if err != nil {
		return Health{}, err
	}
	if len(list.Items) == 0 {
		return Health{}, fmt.Errorf("no Flux controllers found in namespace %q", ns)
	}

	h := Health{Source: HealthFromControllers}
	for i := range list.Items {
		if deploymentAvailable(list.Items[i].Object) {
			continue
		}
		h.Unready = append(h.Unready, list.Items[i].GetName())
	}
	sort.Strings(h.Unready)
	h.Ready = len(h.Unready) == 0
	h.Message = healthMessage(h, "")
	return h, nil
}

func healthMessage(h Health, distribution string) string {
	if !h.Ready {
		return "Flux is not ready: " + strings.Join(h.Unready, ", ") + " unavailable"
	}
	if distribution != "" {
		return "Flux is ready (" + distribution + ")"
	}
	return "Flux is ready"
}

func deploymentAvailable(obj map[string]any) bool {
	for _, c := range conditionsOf(obj) {
		if c.kind == "Available" {
			return c.status == string(ConditionTrue)
		}
	}
	return false
}

// condition is one entry of the standard status.conditions array, which Flux,
// flux-operator and apps/v1 all spell the same way.
type condition struct{ kind, status, reason, message string }

func conditionsOf(obj map[string]any) []condition {
	raw, found, err := unstructured.NestedSlice(obj, "status", "conditions")
	if !found || err != nil {
		return nil
	}
	out := make([]condition, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		var c condition
		c.kind, _, _ = unstructured.NestedString(m, "type")
		c.status, _, _ = unstructured.NestedString(m, "status")
		c.reason, _, _ = unstructured.NestedString(m, "reason")
		c.message, _, _ = unstructured.NestedString(m, "message")
		out = append(out, c)
	}
	return out
}
