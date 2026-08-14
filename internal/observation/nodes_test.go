package observation

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	ksfake "k8s.io/client-go/kubernetes/fake"
)

func testNode(name string, ready bool, labels map[string]string) *corev1.Node {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Status: corev1.NodeStatus{
			Capacity: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("4"),
				corev1.ResourceMemory: resource.MustParse("8Gi"),
			},
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("3800m"),
				corev1.ResourceMemory: resource.MustParse("7Gi"),
			},
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status}},
			NodeInfo: corev1.NodeSystemInfo{
				KubeletVersion: "v1.31.2",
				Architecture:   "arm64",
				OSImage:        "Talos (v1.8.0)",
			},
		},
	}
}

func nodeMetrics(name, cpu, memory string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "metrics.k8s.io/v1beta1",
		"kind":       "NodeMetrics",
		"metadata":   map[string]any{"name": name},
		"usage":      map[string]any{"cpu": cpu, "memory": memory},
	}}
}

// metricsClient seeds NodeMetrics through the tracker with the explicit GVR:
// metrics.k8s.io names the resource "nodes" while the kind is NodeMetrics, an
// irregular pairing the fake's pluralizer cannot guess from the kind alone.
func metricsClient(t *testing.T, objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	t.Helper()
	s := runtime.NewScheme()
	gvk := schema.GroupVersionKind{Group: "metrics.k8s.io", Version: "v1beta1", Kind: "NodeMetrics"}
	s.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(gvk.GroupVersion().WithKind("NodeMetricsList"), &unstructured.UnstructuredList{})
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		s,
		map[schema.GroupVersionResource]string{nodeMetricsGVR: "NodeMetricsList"},
	)
	for _, obj := range objs {
		if err := client.Tracker().Create(nodeMetricsGVR, obj, ""); err != nil {
			t.Fatalf("seeding node metrics: %v", err)
		}
	}
	return client
}

// TestNodesReadsCapacityAndUsage: the whole answer — identity, readiness,
// capacity, allocatable, live usage — sorted by name.
func TestNodesReadsCapacityAndUsage(t *testing.T) {
	src := NodeSource{
		Typed: ksfake.NewSimpleClientset(
			testNode("worker-b", true, nil),
			testNode("control-a", true, map[string]string{"node-role.kubernetes.io/control-plane": ""}),
		),
		Dynamic: metricsClient(t,
			nodeMetrics("control-a", "250m", "1Gi"),
			nodeMetrics("worker-b", "1", "2Gi"),
		),
	}
	inv, err := src.Nodes(context.Background())
	if err != nil {
		t.Fatalf("Nodes: %v", err)
	}
	if inv.UsageGap != "" {
		t.Fatalf("UsageGap = %q, want none", inv.UsageGap)
	}
	if len(inv.Nodes) != 2 || inv.Nodes[0].Name != "control-a" || inv.Nodes[1].Name != "worker-b" {
		t.Fatalf("nodes = %+v, want control-a then worker-b", inv.Nodes)
	}
	control := inv.Nodes[0]
	if len(control.Roles) != 1 || control.Roles[0] != "control-plane" {
		t.Fatalf("roles = %v, want [control-plane]", control.Roles)
	}
	if control.CPUCapacityMilli != 4000 || control.CPUAllocatableMilli != 3800 {
		t.Fatalf("cpu capacity/allocatable = %d/%d", control.CPUCapacityMilli, control.CPUAllocatableMilli)
	}
	if control.MemoryCapacityBytes != 8*1024*1024*1024 {
		t.Fatalf("memory capacity = %d", control.MemoryCapacityBytes)
	}
	if control.CPUUsageMilli == nil || *control.CPUUsageMilli != 250 {
		t.Fatalf("cpu usage = %v, want 250m", control.CPUUsageMilli)
	}
	worker := inv.Nodes[1]
	if worker.CPUUsageMilli == nil || *worker.CPUUsageMilli != 1000 {
		t.Fatalf("worker cpu usage = %v, want 1000m", worker.CPUUsageMilli)
	}
	if worker.MemoryUsageBytes == nil || *worker.MemoryUsageBytes != 2*1024*1024*1024 {
		t.Fatalf("worker memory usage = %v", worker.MemoryUsageBytes)
	}
	if !worker.Ready || len(worker.Roles) != 0 {
		t.Fatalf("worker = %+v, want ready with no invented role", worker)
	}
}

// TestNodesWithoutMetricsAPI: usage is nil — never zero — and the gap names
// the fix. A missing optional API must not fail the inventory.
func TestNodesWithoutMetricsAPI(t *testing.T) {
	src := NodeSource{
		Typed:   ksfake.NewSimpleClientset(testNode("solo", false, nil)),
		Dynamic: metricsClient(t), // serves the list kind but no objects
	}
	// An empty metrics list is "no sample yet", not "not served": the fake
	// cannot 404 a registered list kind, so the not-served path is asserted on
	// the gap text below via a nil dynamic client instead.
	inv, err := src.Nodes(context.Background())
	if err != nil {
		t.Fatalf("Nodes: %v", err)
	}
	node := inv.Nodes[0]
	if node.CPUUsageMilli != nil || node.MemoryUsageBytes != nil {
		t.Fatalf("usage = %v/%v, want nil when nothing was read", node.CPUUsageMilli, node.MemoryUsageBytes)
	}
	if inv.UsageGap == "" {
		t.Fatal("UsageGap is empty; a node with no usage must be explained")
	}
	if node.Ready {
		t.Fatal("a NotReady node was reported ready")
	}

	src.Dynamic = nil
	inv, err = src.Nodes(context.Background())
	if err != nil {
		t.Fatalf("Nodes without metrics client: %v", err)
	}
	if inv.UsageGap == "" {
		t.Fatal("a build with no metrics client must say usage was not read")
	}
}
