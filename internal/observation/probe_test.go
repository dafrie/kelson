package observation

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// probeScheme registers Deployment and Pod against the dynamic fake, with list
// kinds so List works.
func probeScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	add := func(gvk schema.GroupVersionKind) {
		s.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		l := gvk
		l.Kind += "List"
		s.AddKnownTypeWithName(l, &unstructured.UnstructuredList{})
	}
	add(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"})
	add(schema.GroupVersionKind{Version: "v1", Kind: "Pod"})
	return s
}

func probeClient(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		probeScheme(),
		map[schema.GroupVersionResource]string{
			deploymentGVR: "DeploymentList",
			podGVR:        "PodList",
		},
		objs...,
	)
}

// TestProbeEvaluatesLiveDeployment exercises the whole probe: reading the
// Deployment, listing its pods by selector, and surfacing the crash. It proves
// no test needs a cluster.
func TestProbeEvaluatesLiveDeployment(t *testing.T) {
	dep := deployment(testNS, testApp, nil)
	crash := pod(testNS, "web-0", func(m map[string]any) { podCrashLoop(m) })
	healthy := pod(testNS, "web-1", nil) // deliberately not in the selector set? both carry it

	probe, err := NewProbe(ProbeConfig{Client: probeClient(dep, crash, healthy)})
	if err != nil {
		t.Fatal(err)
	}

	v, err := probe.Evaluate(context.Background(), testNS, testApp)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if v.Healthy {
		t.Fatal("a deployment with a crashing pod must not be healthy")
	}
	if v.Code != CodeCrashLoopBackOff {
		t.Fatalf("Code = %q, want crash-loop-back-off", v.Code)
	}
	if len(v.Containers) != 1 || v.Containers[0].Pod != "web-0" {
		t.Fatalf("expected the failing container from web-0, got %+v", v.Containers)
	}
}

// TestProbeFiltersPodsBySelector proves the probe lists only the Deployment's
// own pods: a failing pod that is not in the selector set must not count.
func TestProbeFiltersPodsBySelector(t *testing.T) {
	dep := deployment(testNS, testApp, nil)
	unrelated := pod(testNS, "other-0", func(m map[string]any) {
		m["metadata"].(map[string]any)["labels"].(map[string]any)["kelson.dev/application"] = "other"
		podCrashLoop(m)
	})

	probe, err := NewProbe(ProbeConfig{Client: probeClient(dep, unrelated)})
	if err != nil {
		t.Fatal(err)
	}

	v, err := probe.Evaluate(context.Background(), testNS, testApp)
	if err != nil {
		t.Fatal(err)
	}
	// The unrelated failing pod is excluded; with no selector pods the verdict
	// is a wait state, not the unrelated pod's failure — and never healthy.
	if v.Code != CodeProgressing {
		t.Fatalf("Code = %q, want progressing (unrelated pod ignored)", v.Code)
	}
}

func TestProbeMissingDeployment(t *testing.T) {
	probe, err := NewProbe(ProbeConfig{Client: probeClient()})
	if err != nil {
		t.Fatal(err)
	}
	v, err := probe.Evaluate(context.Background(), testNS, testApp)
	if err != nil {
		t.Fatalf("a missing workload is a verdict, not an error: %v", err)
	}
	if v.Healthy {
		t.Fatal("a missing deployment must not be healthy")
	}
	if v.Code != CodeMissing {
		t.Fatalf("Code = %q, want missing", v.Code)
	}
}

func TestNewProbeRequiresClient(t *testing.T) {
	if _, err := NewProbe(ProbeConfig{}); err == nil {
		t.Fatal("expected an error for a nil client")
	}
}
