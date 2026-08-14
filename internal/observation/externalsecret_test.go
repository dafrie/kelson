package observation

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// externalSecret builds an ExternalSecret with the given Ready condition. A nil
// condition means the controller has not reported yet.
func externalSecret(ns, name string, ready map[string]any) *unstructured.Unstructured {
	m := map[string]any{
		"apiVersion": "external-secrets.io/v1",
		"kind":       "ExternalSecret",
		"metadata": map[string]any{
			"name":      name,
			"namespace": ns,
		},
		"spec": map[string]any{
			"refreshInterval": "1h",
			"secretStoreRef":  map[string]any{"name": "vault-backend", "kind": "ClusterSecretStore"},
			"target":          map[string]any{"name": name, "creationPolicy": "Owner"},
		},
	}
	if ready != nil {
		m["status"] = map[string]any{"conditions": []any{ready}}
	}
	return &unstructured.Unstructured{Object: m}
}

func syncClient(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	s := probeScheme()
	gvk := schema.GroupVersionKind{Group: "external-secrets.io", Version: "v1", Kind: "ExternalSecret"}
	s.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
	listGVK := gvk
	listGVK.Kind += "List"
	s.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		s,
		map[schema.GroupVersionResource]string{
			deploymentGVR:     "DeploymentList",
			podGVR:            "PodList",
			externalSecretGVR: "ExternalSecretList",
		},
		objs...,
	)
}

func syncProbe(t *testing.T, objs ...runtime.Object) *Probe {
	t.Helper()
	p, err := NewProbe(ProbeConfig{Client: syncClient(objs...)})
	if err != nil {
		t.Fatalf("NewProbe: %v", err)
	}
	return p
}

// TestSecretSyncFailureNamesTheCause is issue #80's acceptance criterion: a
// failed sync surfaces as a component-level problem with the cause named,
// not as silence.
func TestSecretSyncFailureNamesTheCause(t *testing.T) {
	p := syncProbe(t, externalSecret(testNS, "payments", map[string]any{
		"type":    "Ready",
		"status":  "False",
		"reason":  "SecretSyncedError",
		"message": `could not get secret data from provider: cannot get secret "payments": permission denied`,
	}))

	v, err := p.EvaluateSecretSync(context.Background(), testNS, "payments")
	if err != nil {
		t.Fatalf("EvaluateSecretSync: %v", err)
	}
	if v.Healthy || v.Code != CodeSecretSyncFailed {
		t.Fatalf("verdict = %+v, want a secret-sync-failed failure", v)
	}
	if !IsFailure(v.Code) {
		t.Errorf("a controller that looked and said no is a failure, not a wait state")
	}
	for _, want := range []string{"SecretSyncedError", "permission denied"} {
		if !strings.Contains(v.Reason, want) {
			t.Errorf("reason %q must carry the controller's own cause %q", v.Reason, want)
		}
	}
	if v.Remediation == "" {
		t.Errorf("a failure must state the fix")
	}
	if v.Resource != "external-secrets.io/ExternalSecret/prod/payments" {
		t.Errorf("resource = %q, want the ExternalSecret's own coordinates", v.Resource)
	}
	// The one-line summary is what `kelson status` and diagnose print.
	if s := v.String(); !strings.Contains(s, "degraded") || !strings.Contains(s, "permission denied") {
		t.Errorf("summary = %q, want a degraded line carrying the cause", s)
	}
}

// TestSecretSyncStates covers the rest of the table, including the two that
// must never read as failures.
func TestSecretSyncStates(t *testing.T) {
	for _, tc := range []struct {
		name        string
		ready       map[string]any
		wantCode    Code
		wantHealthy bool
	}{
		{
			name:        "synced",
			ready:       map[string]any{"type": "Ready", "status": "True", "reason": "SecretSynced"},
			wantCode:    CodeHealthy,
			wantHealthy: true,
		},
		{
			// Applied a moment ago and not yet reconciled. A deploy must not
			// flash red on its way to green.
			name:     "not reported on yet",
			ready:    nil,
			wantCode: CodeProgressing,
		},
		{
			name:     "condition Unknown",
			ready:    map[string]any{"type": "Ready", "status": "Unknown", "reason": "SecretSynced"},
			wantCode: CodeProgressing,
		},
		{
			// A different condition type is not the one that answers the
			// question, so the object still counts as unreported.
			name:     "only a Deleted condition",
			ready:    map[string]any{"type": "Deleted", "status": "True", "reason": "SecretDeleted"},
			wantCode: CodeProgressing,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := syncProbe(t, externalSecret(testNS, "payments", tc.ready))
			v, err := p.EvaluateSecretSync(context.Background(), testNS, "payments")
			if err != nil {
				t.Fatalf("EvaluateSecretSync: %v", err)
			}
			if v.Code != tc.wantCode || v.Healthy != tc.wantHealthy {
				t.Fatalf("verdict = %+v, want code %q healthy=%v", v, tc.wantCode, tc.wantHealthy)
			}
			if tc.wantCode == CodeProgressing && IsFailure(v.Code) {
				t.Errorf("a wait state must never be a failure")
			}
		})
	}
}

// TestSecretSyncMissingExternalSecret: the resource kelson rendered is not in
// the cluster. That is missing, not failed — the apply is what did not happen.
func TestSecretSyncMissingExternalSecret(t *testing.T) {
	p := syncProbe(t)
	v, err := p.EvaluateSecretSync(context.Background(), testNS, "payments")
	if err != nil {
		t.Fatalf("EvaluateSecretSync: %v", err)
	}
	if v.Code != CodeMissing || v.Healthy {
		t.Fatalf("verdict = %+v, want missing", v)
	}
}

// TestSecretSyncReadsNoSecret is the acceptance criterion stated as a test: no
// secret value passes through kelson. The probe reads the ExternalSecret and
// never the Secret it produces, so a Secret sitting in the same namespace with
// a real value in it contributes nothing to any verdict — and could not, since
// nothing here addresses the Secret resource at all.
func TestSecretSyncReadsNoSecret(t *testing.T) {
	const value = "hunter2-the-actual-credential"
	secret := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   map[string]any{"name": "payments", "namespace": testNS},
		"stringData": map[string]any{"stripe-api-key": value},
	}}
	p := syncProbe(t,
		secret,
		externalSecret(testNS, "payments", map[string]any{
			"type": "Ready", "status": "True", "reason": "SecretSynced",
		}),
	)
	v, err := p.EvaluateSecretSync(context.Background(), testNS, "payments")
	if err != nil {
		t.Fatalf("EvaluateSecretSync: %v", err)
	}
	if strings.Contains(v.String(), value) || strings.Contains(v.Reason, value) {
		t.Fatalf("a verdict must never carry a secret value: %+v", v)
	}
	if !v.Healthy {
		t.Fatalf("verdict = %+v, want healthy", v)
	}
}

// TestProbeImplementsSecretSyncEvaluator pins the optional-capability seam the
// status wiring type-asserts on: a Probe must satisfy it, or `kelson status`
// silently stops reporting sync failures.
func TestProbeImplementsSecretSyncEvaluator(t *testing.T) {
	var _ SecretSyncEvaluator = (*Probe)(nil)
}
