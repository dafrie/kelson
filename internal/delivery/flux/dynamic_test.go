package flux

import (
	"context"
	"reflect"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/dafrie/kelson/internal/delivery"
)

// The kinds behind the GVRs in dynamic.go. The fake dynamic client derives the
// resource name from the kind, so registering the kind here is what makes
// dyn.Resource(<gvr>) resolvable at all; an uninstalled CRD is modelled with
// withoutCRD below, because the fake panics on a listed kind it never learned
// instead of answering the 404 a real API server sends.
var (
	kustomizationGVK = schema.GroupVersionKind{Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Kind: "Kustomization"}
	gitRepositoryGVK = schema.GroupVersionKind{Group: "source.toolkit.fluxcd.io", Version: "v1", Kind: "GitRepository"}
	helmReleaseGVK   = schema.GroupVersionKind{Group: "helm.toolkit.fluxcd.io", Version: "v2", Kind: "HelmRelease"}
	fluxReportGVK    = schema.GroupVersionKind{Group: "fluxcd.controlplane.io", Version: "v1", Kind: "FluxReport"}
	deploymentGVK    = schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}
)

func allFluxKinds() []schema.GroupVersionKind {
	return []schema.GroupVersionKind{kustomizationGVK, gitRepositoryGVK, helmReleaseGVK, fluxReportGVK, deploymentGVK}
}

// newFakeCluster stands up a fake dynamic client that serves exactly the given
// kinds, mirroring internal/clusterprofile/detect's test setup.
func newFakeCluster(t *testing.T, kinds ...schema.GroupVersionKind) *dynamicfake.FakeDynamicClient {
	t.Helper()
	s := runtime.NewScheme()
	for _, k := range kinds {
		s.AddKnownTypeWithName(k, &unstructured.Unstructured{})
		lk := k
		lk.Kind += "List"
		s.AddKnownTypeWithName(lk, &unstructured.UnstructuredList{})
	}
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(s, nil)
}

// withoutCRD makes a resource answer the way a real API server answers for a
// CRD that is not installed — 404, not an empty list. It is how these tests say
// "flux-operator is not on this cluster".
func withoutCRD(dyn *dynamicfake.FakeDynamicClient, resource string) {
	dyn.PrependReactor("list", resource, func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: resource}, "")
	})
}

func seed(t *testing.T, dyn *dynamicfake.FakeDynamicClient, gvr schema.GroupVersionResource, obj *unstructured.Unstructured) {
	t.Helper()
	if err := dyn.Tracker().Create(gvr, obj, obj.GetNamespace()); err != nil {
		t.Fatalf("seeding %s/%s: %v", gvr, obj.GetName(), err)
	}
}

func object(gvk schema.GroupVersionKind, namespace, name string, body map[string]any) *unstructured.Unstructured {
	obj := map[string]any{"apiVersion": gvk.GroupVersion().String(), "kind": gvk.Kind}
	for k, v := range body {
		obj[k] = v
	}
	meta, _ := obj["metadata"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
	}
	meta["name"], meta["namespace"] = name, namespace
	obj["metadata"] = meta
	return &unstructured.Unstructured{Object: obj}
}

func readyCondition(status, reason, message string) map[string]any {
	return map[string]any{"type": "Ready", "status": status, "reason": reason, "message": message}
}

// TestDynamicStatusReaderReadsKustomizations is the shape of the read the
// adapter depends on: spec, conditions and the source URL resolved from the
// GitRepository the Kustomization references.
func TestDynamicStatusReaderReadsKustomizations(t *testing.T) {
	dyn := newFakeCluster(t, allFluxKinds()...)
	seed(t, dyn, kustomizationGVR, object(kustomizationGVK, "apps", "web", map[string]any{
		"spec": map[string]any{
			"path":      "./apps/web",
			"sourceRef": map[string]any{"kind": "GitRepository", "name": "deploy"},
		},
		"status": map[string]any{
			"lastAppliedRevision": "main@sha1:abc123def",
			"conditions":          []any{readyCondition("True", "ReconciliationSucceeded", "ok")},
		},
	}))
	seed(t, dyn, gitRepositoryGVR, object(gitRepositoryGVK, "apps", "deploy", map[string]any{
		"spec": map[string]any{
			"url": "https://github.com/acme/deploy.git",
			"ref": map[string]any{"branch": "main"},
		},
	}))

	ks, err := DynamicStatusReader{Client: dyn}.Kustomizations(context.Background())
	if err != nil {
		t.Fatalf("kustomizations: %v", err)
	}
	if len(ks) != 1 {
		t.Fatalf("got %d kustomizations", len(ks))
	}
	k := ks[0]
	if k.Name != "web" || k.Namespace != "apps" || k.Path != "./apps/web" || k.Ready != ConditionTrue {
		t.Fatalf("k = %+v", k)
	}
	if k.SourceURL != "https://github.com/acme/deploy.git" || k.SourceBranch != "main" {
		t.Fatalf("source = %q on %q", k.SourceURL, k.SourceBranch)
	}
	if k.LastAppliedRevision != "main@sha1:abc123def" {
		t.Fatalf("lastAppliedRevision = %q", k.LastAppliedRevision)
	}
}

// TestDynamicStatusReaderReadsSuspendAndReconciling verifies the two fields
// that decide "will this ever apply" and "is it working on it right now".
func TestDynamicStatusReaderReadsSuspendAndReconciling(t *testing.T) {
	dyn := newFakeCluster(t, allFluxKinds()...)
	seed(t, dyn, kustomizationGVR, object(kustomizationGVK, "apps", "web", map[string]any{
		"spec": map[string]any{"path": "./apps/web", "suspend": true},
		"status": map[string]any{
			"lastAttemptedRevision": "main@sha1:abc123def",
			"conditions": []any{
				readyCondition("False", reasonProgressing, "reconciliation in progress"),
				map[string]any{"type": "Reconciling", "status": "True"},
			},
		},
	}))

	ks, err := DynamicStatusReader{Client: dyn}.Kustomizations(context.Background())
	if err != nil {
		t.Fatalf("kustomizations: %v", err)
	}
	k := ks[0]
	if !k.Suspended || !k.Reconciling {
		t.Fatalf("k = %+v, want suspended and reconciling", k)
	}
	if k.Ready != ConditionFalse || k.Reason != reasonProgressing || k.Message == "" {
		t.Fatalf("ready condition = %+v", k)
	}
	if k.LastAttemptedRevision != "main@sha1:abc123def" {
		t.Fatalf("lastAttemptedRevision = %q", k.LastAttemptedRevision)
	}
}

// TestDynamicStatusReaderScopesToNamespace verifies the Namespace field limits
// the query the way the --all-namespaces/-n switch used to.
func TestDynamicStatusReaderScopesToNamespace(t *testing.T) {
	dyn := newFakeCluster(t, allFluxKinds()...)
	for _, ns := range []string{"apps", "other"} {
		seed(t, dyn, kustomizationGVR, object(kustomizationGVK, ns, "web", map[string]any{
			"spec": map[string]any{"path": "./apps/web"},
		}))
	}

	all, err := DynamicStatusReader{Client: dyn}.Kustomizations(context.Background())
	if err != nil {
		t.Fatalf("all namespaces: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("all namespaces returned %d kustomizations, want 2", len(all))
	}
	scoped, err := DynamicStatusReader{Client: dyn, Namespace: "apps"}.Kustomizations(context.Background())
	if err != nil {
		t.Fatalf("scoped: %v", err)
	}
	if len(scoped) != 1 || scoped[0].Namespace != "apps" {
		t.Fatalf("scoped = %+v", scoped)
	}
}

// TestDynamicStatusReaderSourceIsBestEffort verifies an unreadable GitRepository
// does not fail the read: without a source URL kelson matches on path alone,
// which is still correct for single-repo installs.
func TestDynamicStatusReaderSourceIsBestEffort(t *testing.T) {
	dyn := newFakeCluster(t, allFluxKinds()...)
	seed(t, dyn, kustomizationGVR, object(kustomizationGVK, "apps", "web", map[string]any{
		"spec": map[string]any{"path": "./apps/web", "sourceRef": map[string]any{"kind": "GitRepository", "name": "deploy"}},
	}))
	dyn.PrependReactor("list", "gitrepositories", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "source.toolkit.fluxcd.io", Resource: "gitrepositories"}, "", nil)
	})

	ks, err := DynamicStatusReader{Client: dyn}.Kustomizations(context.Background())
	if err != nil {
		t.Fatalf("kustomizations must survive an unreadable source: %v", err)
	}
	if len(ks) != 1 || ks[0].SourceURL != "" {
		t.Fatalf("ks = %+v", ks)
	}
}

// TestDynamicStatusReaderReadErrorIsStructured verifies a failed Kustomization
// read is a delivery error naming what could not be read, not a bare client
// error.
func TestDynamicStatusReaderReadErrorIsStructured(t *testing.T) {
	dyn := newFakeCluster(t, allFluxKinds()...)
	dyn.PrependReactor("list", "kustomizations", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "kustomize.toolkit.fluxcd.io", Resource: "kustomizations"}, "", nil)
	})
	_, err := DynamicStatusReader{Client: dyn}.Kustomizations(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if !delivery.AsApplyFailed(err) {
		t.Fatalf("error = %v, want delivery/apply-failed", err)
	}
}

func TestDynamicStatusReaderReadsHelmReleases(t *testing.T) {
	dyn := newFakeCluster(t, allFluxKinds()...)
	seed(t, dyn, helmReleaseGVR, object(helmReleaseGVK, "apps", "redis", map[string]any{
		"status": map[string]any{"conditions": []any{readyCondition("False", "UpgradeFailed", "timed out")}},
	}))
	hrs, err := DynamicStatusReader{Client: dyn}.HelmReleases(context.Background())
	if err != nil {
		t.Fatalf("helmreleases: %v", err)
	}
	if len(hrs) != 1 {
		t.Fatalf("got %d helm releases", len(hrs))
	}
	hr := hrs[0]
	if hr.Name != "redis" || hr.Namespace != "apps" || hr.Ready != ConditionFalse || hr.Reason != "UpgradeFailed" {
		t.Fatalf("hr = %+v", hr)
	}
}

// TestHealthPrefersFluxReport is the #137 delegation: where flux-operator
// publishes a FluxReport, kelson reads its aggregate instead of deriving one.
// The CR is read unstructured — flux-operator is AGPL-3.0 and kelson is MIT, so
// no Go type of theirs may appear here.
func TestHealthPrefersFluxReport(t *testing.T) {
	dyn := newFakeCluster(t, allFluxKinds()...)
	seed(t, dyn, fluxReportGVR, object(fluxReportGVK, "flux-system", "flux", map[string]any{
		"spec": map[string]any{
			"distribution": map[string]any{"version": "v2.4.0", "status": "Installed"},
			"components": []any{
				map[string]any{"name": "source-controller", "ready": false, "status": "not available"},
				map[string]any{"name": "kustomize-controller", "ready": true},
			},
		},
	}))
	// A healthy Deployment aggregate would answer the opposite, so this also
	// proves which source won.
	seed(t, dyn, deploymentGVR, availableController(t, "source-controller"))

	h, err := DynamicStatusReader{Client: dyn}.Health(context.Background())
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if h.Source != HealthFromReport {
		t.Fatalf("source = %q, want %q", h.Source, HealthFromReport)
	}
	if h.Ready {
		t.Fatalf("health = %+v, want not ready", h)
	}
	if !reflect.DeepEqual(h.Unready, []string{"source-controller"}) {
		t.Fatalf("unready = %v", h.Unready)
	}
	if h.Version != "v2.4.0" || h.Message == "" {
		t.Fatalf("health = %+v", h)
	}
}

// TestHealthFallsBackToControllers verifies a Flux installed without
// flux-operator (no FluxReport CRD at all) still gets an answer, aggregated
// from the controller Deployments.
func TestHealthFallsBackToControllers(t *testing.T) {
	dyn := newFakeCluster(t, allFluxKinds()...)
	withoutCRD(dyn, "fluxreports")
	seed(t, dyn, deploymentGVR, availableController(t, "source-controller"))
	seed(t, dyn, deploymentGVR, object(deploymentGVK, DefaultNamespace, "kustomize-controller", map[string]any{
		"metadata": map[string]any{"labels": map[string]any{"app.kubernetes.io/part-of": "flux"}},
		"status":   map[string]any{"conditions": []any{map[string]any{"type": "Available", "status": "False"}}},
	}))

	h, err := DynamicStatusReader{Client: dyn}.Health(context.Background())
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if h.Source != HealthFromControllers {
		t.Fatalf("source = %q, want %q", h.Source, HealthFromControllers)
	}
	if h.Ready || !reflect.DeepEqual(h.Unready, []string{"kustomize-controller"}) {
		t.Fatalf("health = %+v", h)
	}
}

// TestHealthReadyWhenControllersAvailable verifies the happy path reports ready
// rather than merely "no complaints".
func TestHealthReadyWhenControllersAvailable(t *testing.T) {
	dyn := newFakeCluster(t, allFluxKinds()...)
	withoutCRD(dyn, "fluxreports")
	seed(t, dyn, deploymentGVR, availableController(t, "source-controller"))
	h, err := DynamicStatusReader{Client: dyn}.Health(context.Background())
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if !h.Ready || len(h.Unready) != 0 {
		t.Fatalf("health = %+v", h)
	}
}

// TestHealthUnknownWhenNothingAnswers verifies neither source silently reports
// "healthy" when it saw nothing at all.
func TestHealthUnknownWhenNothingAnswers(t *testing.T) {
	dyn := newFakeCluster(t, allFluxKinds()...)
	withoutCRD(dyn, "fluxreports")
	if _, err := (DynamicStatusReader{Client: dyn}).Health(context.Background()); err == nil {
		t.Fatal("a cluster with no FluxReport and no controllers must not report health")
	}
}

// TestHealthSkipsFluxReportWhenProfileSaysAbsent is the #157 inversion fixed:
// where detection reports no flux-operator, the reader must not establish that
// by attempting the read. Both sources are seeded and would answer, so the
// source name is proof of which one was asked.
func TestHealthSkipsFluxReportWhenProfileSaysAbsent(t *testing.T) {
	dyn := newFakeCluster(t, allFluxKinds()...)
	seed(t, dyn, fluxReportGVR, object(fluxReportGVK, DefaultNamespace, "flux", map[string]any{
		"spec": map[string]any{"distribution": map[string]any{"version": "v2.4.0", "status": "Installed"}},
	}))
	seed(t, dyn, deploymentGVR, availableController(t, "source-controller"))

	h, err := DynamicStatusReader{Client: dyn, FluxOperator: boolPtr(false)}.Health(context.Background())
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if h.Source != HealthFromControllers {
		t.Fatalf("source = %q, want %q — a profile that says absent must not be re-probed", h.Source, HealthFromControllers)
	}
	if !h.Ready {
		t.Fatalf("health = %+v, want ready", h)
	}
}

// TestHealthUsesFluxReportWhenProfileSaysPresent covers the other finding, and
// that a profile is never allowed to turn a health read into a failure: an
// operator the profile promised but whose CRD does not answer still falls back.
func TestHealthUsesFluxReportWhenProfileSaysPresent(t *testing.T) {
	dyn := newFakeCluster(t, allFluxKinds()...)
	seed(t, dyn, fluxReportGVR, object(fluxReportGVK, DefaultNamespace, "flux", map[string]any{
		"spec": map[string]any{"distribution": map[string]any{"version": "v2.4.0", "status": "Installed"}},
	}))
	h, err := DynamicStatusReader{Client: dyn, FluxOperator: boolPtr(true)}.Health(context.Background())
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if h.Source != HealthFromReport || h.Version != "v2.4.0" {
		t.Fatalf("health = %+v, want the FluxReport answer", h)
	}

	gone := newFakeCluster(t, allFluxKinds()...)
	withoutCRD(gone, "fluxreports")
	seed(t, gone, deploymentGVR, availableController(t, "source-controller"))
	h, err = DynamicStatusReader{Client: gone, FluxOperator: boolPtr(true)}.Health(context.Background())
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if h.Source != HealthFromControllers {
		t.Fatalf("source = %q, want the fallback when the promised report cannot be read", h.Source)
	}
}

func boolPtr(b bool) *bool { return &b }

func availableController(t *testing.T, name string) *unstructured.Unstructured {
	t.Helper()
	return object(deploymentGVK, DefaultNamespace, name, map[string]any{
		"metadata": map[string]any{"labels": map[string]any{"app.kubernetes.io/part-of": "flux"}},
		"status":   map[string]any{"conditions": []any{map[string]any{"type": "Available", "status": "True"}}},
	})
}
