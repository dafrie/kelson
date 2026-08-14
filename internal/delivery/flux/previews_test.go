package flux

import (
	"context"
	"reflect"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

var (
	resourceSetGVK   = schema.GroupVersionKind{Group: "fluxcd.controlplane.io", Version: "v1", Kind: "ResourceSet"}
	inputProviderGVK = schema.GroupVersionKind{Group: "fluxcd.controlplane.io", Version: "v1", Kind: "ResourceSetInputProvider"}
	ociRepositoryGVK = schema.GroupVersionKind{Group: "source.toolkit.fluxcd.io", Version: "v1beta2", Kind: "OCIRepository"}
	httpRouteGVK     = schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "HTTPRoute"}
)

func previewKinds() []schema.GroupVersionKind {
	return []schema.GroupVersionKind{
		kustomizationGVK, resourceSetGVK, inputProviderGVK, ociRepositoryGVK, httpRouteGVK,
	}
}

var previewScope = PreviewScope{Project: "checkout", Environment: "staging", Namespace: "checkout-staging"}

// provenance is what the ResourceSet's commonMetadata stamps onto everything it
// generates, and what the publisher's render stamps onto the artifact's own
// objects (ADR-0017 decision 4).
func provenance() map[string]any {
	return map[string]any{
		"app.kubernetes.io/managed-by": "kelson",
		"kelson.dev/project":           "checkout",
		"kelson.dev/environment":       "staging",
	}
}

func seedPair(t *testing.T, dyn *dynamicfake.FakeDynamicClient, id, sha, artifact, applied string) {
	t.Helper()
	name := "checkout-staging-pr" + id
	seed(t, dyn, ociRepositoryGVR, object(ociRepositoryGVK, previewScope.Namespace, name, map[string]any{
		"spec":   map[string]any{"ref": map[string]any{"tag": sha}},
		"status": map[string]any{"conditions": []any{readyCondition(artifact, "Succeeded", "stored artifact for digest "+sha)}},
	}))
	seed(t, dyn, kustomizationGVR, object(kustomizationGVK, previewScope.Namespace, name, map[string]any{
		"spec": map[string]any{"targetNamespace": name},
		"status": map[string]any{
			"conditions":          []any{readyCondition(applied, "ReconciliationSucceeded", "applied revision "+sha)},
			"lastAppliedRevision": sha,
		},
	}))
}

func seedLifecycle(t *testing.T, dyn *dynamicfake.FakeDynamicClient, provider, set string) {
	t.Helper()
	name := "checkout-staging-previews"
	seed(t, dyn, inputProviderGVR, object(inputProviderGVK, previewScope.Namespace, name, map[string]any{
		"status": map[string]any{"conditions": []any{readyCondition(provider, "ReconciliationSucceeded", "2 change requests")}},
	}))
	seed(t, dyn, resourceSetGVR, object(resourceSetGVK, previewScope.Namespace, name, map[string]any{
		"status": map[string]any{"conditions": []any{readyCondition(set, "ReconciliationSucceeded", "applied 4 resources")}},
	}))
}

func TestPreviewsReportsThePairPerChangeRequest(t *testing.T) {
	dyn := newFakeCluster(t, previewKinds()...)
	seedLifecycle(t, dyn, "True", "True")
	seedPair(t, dyn, "9", "aaaa", "True", "True")
	seedPair(t, dyn, "412", "bbbb", "True", "Unknown")
	seed(t, dyn, httpRouteGVR, object(httpRouteGVK, "checkout-staging-pr412", "web", map[string]any{
		"metadata": map[string]any{"labels": provenance()},
		"spec":     map[string]any{"hostnames": []any{"web-pr412.staging.acme.run"}},
	}))
	// The parent environment's own route carries the identical labels and must
	// not become a preview's hostname.
	seed(t, dyn, httpRouteGVR, object(httpRouteGVK, previewScope.Namespace, "web", map[string]any{
		"metadata": map[string]any{"labels": provenance()},
		"spec":     map[string]any{"hostnames": []any{"web.staging.acme.run"}},
	}))

	got, err := DynamicStatusReader{Client: dyn}.Previews(context.Background(), previewScope)
	if err != nil {
		t.Fatalf("Previews: %v", err)
	}

	if !got.Lifecycle.Served || !got.Lifecycle.Present {
		t.Fatalf("lifecycle = %+v, want served and present", got.Lifecycle)
	}
	if got.Lifecycle.Name != "checkout-staging-previews" {
		t.Errorf("lifecycle name = %q", got.Lifecycle.Name)
	}
	if got.Lifecycle.Provider != ConditionTrue || got.Lifecycle.Set != ConditionTrue {
		t.Errorf("lifecycle conditions = %v/%v, want True/True", got.Lifecycle.Provider, got.Lifecycle.Set)
	}

	if len(got.Previews) != 2 {
		t.Fatalf("previews = %d, want 2", len(got.Previews))
	}
	// Highest change request first, and numerically: pr412 above pr9 is the
	// ordering a string sort would get wrong.
	if got.Previews[0].ID != "412" || got.Previews[1].ID != "9" {
		t.Fatalf("order = %q, %q; want 412, 9", got.Previews[0].ID, got.Previews[1].ID)
	}

	newest := got.Previews[0]
	if newest.Namespace != "checkout-staging-pr412" {
		t.Errorf("namespace = %q", newest.Namespace)
	}
	if newest.SHA != "bbbb" {
		t.Errorf("sha = %q, want the OCIRepository's pinned tag", newest.SHA)
	}
	if newest.Phase != PreviewApplying {
		t.Errorf("phase = %q, want applying", newest.Phase)
	}
	if !reflect.DeepEqual(newest.Hosts, []string{"web-pr412.staging.acme.run"}) {
		t.Errorf("hosts = %v", newest.Hosts)
	}
	if got.Previews[1].Phase != PreviewReady {
		t.Errorf("pr9 phase = %q, want ready", got.Previews[1].Phase)
	}
	if len(got.Previews[1].Hosts) != 0 {
		t.Errorf("pr9 hosts = %v, want none: it has no HTTPRoute", got.Previews[1].Hosts)
	}
}

// The failure ADR-0017 decision 8 names as the visible one: CI never ran
// `kelson preview publish`, so the OCIRepository has nothing to fetch and the
// preview's namespace was never created. Listing namespaces would report no
// previews at all; the pair reports the change request and the reason.
func TestPreviewsReportsAnUnpublishedArtifactAgainstItsChangeRequest(t *testing.T) {
	dyn := newFakeCluster(t, previewKinds()...)
	seedLifecycle(t, dyn, "True", "True")
	seed(t, dyn, ociRepositoryGVR, object(ociRepositoryGVK, previewScope.Namespace, "checkout-staging-pr7", map[string]any{
		"spec": map[string]any{"ref": map[string]any{"tag": "cafe"}},
		"status": map[string]any{"conditions": []any{
			readyCondition("False", "OCIArtifactPullFailed", "failed to pull artifact: MANIFEST_UNKNOWN"),
		}},
	}))
	seed(t, dyn, kustomizationGVR, object(kustomizationGVK, previewScope.Namespace, "checkout-staging-pr7", map[string]any{
		"status": map[string]any{"conditions": []any{
			readyCondition("False", "ArtifactFailed", "source is not ready"),
		}},
	}))

	got, err := DynamicStatusReader{Client: dyn}.Previews(context.Background(), previewScope)
	if err != nil {
		t.Fatalf("Previews: %v", err)
	}
	if len(got.Previews) != 1 {
		t.Fatalf("previews = %d, want 1", len(got.Previews))
	}
	p := got.Previews[0]
	if p.Phase != PreviewAwaitingArtifact {
		t.Fatalf("phase = %q, want awaiting-artifact", p.Phase)
	}
	// The artifact's own words, not the Kustomization's consequence of them.
	if p.Reason != "OCIArtifactPullFailed" || p.Message != "failed to pull artifact: MANIFEST_UNKNOWN" {
		t.Errorf("reason/message = %q/%q, want the OCIRepository's", p.Reason, p.Message)
	}
	if p.Artifact != ConditionFalse || p.Applied != ConditionFalse {
		t.Errorf("conditions = %v/%v", p.Artifact, p.Applied)
	}
}

func TestPreviewsReportsFluxOperatorAbsentRatherThanFailing(t *testing.T) {
	dyn := newFakeCluster(t, previewKinds()...)
	withoutCRD(dyn, "resourcesetinputproviders")

	got, err := DynamicStatusReader{Client: dyn}.Previews(context.Background(), previewScope)
	if err != nil {
		t.Fatalf("Previews: %v", err)
	}
	if got.Lifecycle.Served {
		t.Errorf("served = true, want false when the CRD is not installed")
	}
	if got.Lifecycle.Name != "checkout-staging-previews" {
		t.Errorf("name = %q: the scheme is known without the cluster", got.Lifecycle.Name)
	}
	if len(got.Previews) != 0 {
		t.Errorf("previews = %d, want none", len(got.Previews))
	}
}

func TestPreviewsSeparatesAMissingPairFromAMissingOperator(t *testing.T) {
	dyn := newFakeCluster(t, previewKinds()...)

	got, err := DynamicStatusReader{Client: dyn}.Previews(context.Background(), previewScope)
	if err != nil {
		t.Fatalf("Previews: %v", err)
	}
	if !got.Lifecycle.Served {
		t.Errorf("served = false: the CRDs are installed, the objects are not")
	}
	if got.Lifecycle.Present {
		t.Errorf("present = true, want false: nothing was delivered yet")
	}
}

func TestPreviewsSurvivesAClusterWithoutGatewayAPI(t *testing.T) {
	dyn := newFakeCluster(t, previewKinds()...)
	withoutCRD(dyn, "httproutes")
	seedLifecycle(t, dyn, "True", "True")
	seedPair(t, dyn, "1", "dddd", "True", "True")

	got, err := DynamicStatusReader{Client: dyn}.Previews(context.Background(), previewScope)
	if err != nil {
		t.Fatalf("Previews: %v", err)
	}
	if len(got.Previews) != 1 || got.Previews[0].Phase != PreviewReady {
		t.Fatalf("previews = %+v", got.Previews)
	}
	if len(got.Previews[0].Hosts) != 0 {
		t.Errorf("hosts = %v, want none", got.Previews[0].Hosts)
	}
}

func TestPreviewsIgnoresTheEnvironmentsOwnObjects(t *testing.T) {
	dyn := newFakeCluster(t, previewKinds()...)
	seedLifecycle(t, dyn, "True", "True")
	// The environment's own Kustomization, in the same namespace, whose name is
	// the lifecycle stem without a change request number.
	seed(t, dyn, kustomizationGVR, object(kustomizationGVK, previewScope.Namespace, "checkout-staging", map[string]any{
		"status": map[string]any{"conditions": []any{readyCondition("True", "ReconciliationSucceeded", "applied")}},
	}))
	seedPair(t, dyn, "3", "eeee", "True", "True")

	got, err := DynamicStatusReader{Client: dyn}.Previews(context.Background(), previewScope)
	if err != nil {
		t.Fatalf("Previews: %v", err)
	}
	if len(got.Previews) != 1 || got.Previews[0].ID != "3" {
		t.Fatalf("previews = %+v, want only pr3", got.Previews)
	}
}

func TestPreviewsRecordsSuspensionAndCreationTime(t *testing.T) {
	dyn := newFakeCluster(t, previewKinds()...)
	seedLifecycle(t, dyn, "True", "True")
	seed(t, dyn, ociRepositoryGVR, object(ociRepositoryGVK, previewScope.Namespace, "checkout-staging-pr5", map[string]any{
		"spec":   map[string]any{"ref": map[string]any{"tag": "ffff"}},
		"status": map[string]any{"conditions": []any{readyCondition("True", "Succeeded", "stored")}},
	}))
	seed(t, dyn, kustomizationGVR, object(kustomizationGVK, previewScope.Namespace, "checkout-staging-pr5", map[string]any{
		"metadata": map[string]any{"creationTimestamp": "2026-08-14T09:00:00Z"},
		"spec":     map[string]any{"suspend": true},
		"status": map[string]any{
			"conditions":          []any{readyCondition("True", "ReconciliationSucceeded", "applied")},
			"lastAppliedRevision": "ffff@sha256:1234",
		},
	}))

	got, err := DynamicStatusReader{Client: dyn}.Previews(context.Background(), previewScope)
	if err != nil {
		t.Fatalf("Previews: %v", err)
	}
	if len(got.Previews) != 1 {
		t.Fatalf("previews = %d", len(got.Previews))
	}
	p := got.Previews[0]
	if !p.Suspended {
		t.Errorf("suspended = false, want true")
	}
	if p.Revision != "ffff@sha256:1234" {
		t.Errorf("revision = %q", p.Revision)
	}
	want := time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC)
	if !p.CreatedAt.Equal(want) {
		t.Errorf("createdAt = %v, want %v", p.CreatedAt, want)
	}
}

func TestPreviewPhaseMapping(t *testing.T) {
	cases := []struct {
		artifact, applied ConditionState
		want              PreviewPhase
	}{
		{ConditionTrue, ConditionTrue, PreviewReady},
		{ConditionTrue, ConditionFalse, PreviewFailed},
		{ConditionTrue, ConditionUnknown, PreviewApplying},
		{ConditionFalse, ConditionTrue, PreviewAwaitingArtifact},
		{ConditionFalse, ConditionFalse, PreviewAwaitingArtifact},
		{ConditionUnknown, ConditionTrue, PreviewUnknown},
	}
	for _, c := range cases {
		if got := previewPhase(c.artifact, c.applied); got != c.want {
			t.Errorf("previewPhase(%v, %v) = %q, want %q", c.artifact, c.applied, got, c.want)
		}
	}
}

func TestPreviewsWithoutAClientIsAnError(t *testing.T) {
	if _, err := (DynamicStatusReader{}).Previews(context.Background(), previewScope); err == nil {
		t.Fatal("want an error when no dynamic client is configured")
	}
}
