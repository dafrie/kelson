package serverstate

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
	sigsyaml "sigs.k8s.io/yaml"

	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/delivery/direct"
)

// Both history implementations must satisfy the seam ADR-0013 §1 introduced:
// the CLI's local JSONL journal and kelson-server's ConfigMaps. If either
// drifts, the direct adapter stops being written once.
var (
	_ direct.History = (*direct.Store)(nil)
	_ direct.History = (*HistoryStore)(nil)
)

// TestDirectAdapterOverClusterHistory runs the real direct adapter — the same
// apply, history and rollback code the CLI runs — with the ConfigMap-backed
// store in place of the JSONL one. The adapter is what the seam exists for, so
// "the store implements the interface" is not enough: the deploy/rollback loop
// has to close through it.
func TestDirectAdapterOverClusterHistory(t *testing.T) {
	ctx := context.Background()
	history := newHistoryStore(t, newFakeClient(), 5)
	dyn := newFakeDynamic()

	adapter, err := direct.New(direct.Options{
		Client:  dyn,
		Mapper:  testMapper(),
		History: history,
		Now:     fixedClock(),
	})
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}

	first := manifestSet(t, "sha256:one", 1)
	res, err := adapter.Apply(ctx, first)
	if err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if res.Revision != "rev-00000001" || !res.Applied {
		t.Fatalf("first apply = %+v, want rev-00000001 applied", res)
	}

	second := manifestSet(t, "sha256:two", 2)
	if res, err = adapter.Apply(ctx, second); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if res.Revision != "rev-00000002" {
		t.Fatalf("second apply revision = %q, want rev-00000002", res.Revision)
	}
	if got := livePort(t, dyn); got != 2 {
		t.Fatalf("live port after the second deploy = %d, want 2", got)
	}

	entries, err := adapter.History(ctx, second)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("history = %d entries, want 2", len(entries))
	}
	if entries[0].Revision != "rev-00000002" || entries[1].Revision != "rev-00000001" {
		t.Fatalf("history is not newest-first: %+v", entries)
	}
	if entries[1].SpecHash != "sha256:one" {
		t.Errorf("recorded spec hash = %q, want sha256:one", entries[1].SpecHash)
	}

	// Rollback replays the bytes recorded in the ConfigMap, not a re-render.
	if _, err := adapter.Rollback(ctx, second, entries[1]); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if got := livePort(t, dyn); got != 1 {
		t.Fatalf("live port after rollback = %d, want 1", got)
	}
	entries, err = adapter.History(ctx, second)
	if err != nil {
		t.Fatalf("history after rollback: %v", err)
	}
	if len(entries) != 3 || entries[0].Revision != "rev-00000003" {
		t.Fatalf("history after rollback = %+v, want a third revision on top", entries)
	}
}

// --- fixtures ---------------------------------------------------------------

const testWorkloadNS = "shop-production"

func manifestSet(t *testing.T, specHash string, port int64) delivery.ManifestSet {
	t.Helper()
	obj := map[string]any{
		"apiVersion": "v1",
		"kind":       "Service",
		"metadata": map[string]any{
			"name":      "web",
			"namespace": testWorkloadNS,
			"labels": map[string]any{
				"app.kubernetes.io/managed-by": "kelson",
				labelProject:                   testProject,
				labelEnvironment:               testEnv,
			},
			"annotations": map[string]any{"kelson.dev/spec-hash": specHash},
		},
		"spec": map[string]any{"ports": []any{map[string]any{"port": port}}},
	}
	y, err := sigsyaml.Marshal(obj)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	return delivery.ManifestSet{
		Project:     testProject,
		Environment: testEnv,
		SpecHash:    specHash,
		Manifests: []delivery.Manifest{{
			APIVersion: "v1", Kind: "Service", Name: "web", Namespace: testWorkloadNS, YAML: y,
		}},
	}
}

func livePort(t *testing.T, dyn *dynamicfake.FakeDynamicClient) int64 {
	t.Helper()
	obj, err := dyn.Tracker().Get(serviceGVR, testWorkloadNS, "web")
	if err != nil {
		t.Fatalf("read the live Service: %v", err)
	}
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		t.Fatalf("tracker returned %T", obj)
	}
	ports, _, err := unstructured.NestedSlice(u.Object, "spec", "ports")
	if err != nil || len(ports) == 0 {
		t.Fatalf("live Service has no ports: %v", err)
	}
	port, ok := ports[0].(map[string]any)["port"].(int64)
	if !ok {
		t.Fatalf("live port is %T", ports[0].(map[string]any)["port"])
	}
	return port
}

var serviceGVR = schema.GroupVersionResource{Version: "v1", Resource: "services"}

// newFakeDynamic mirrors the direct adapter's own test cluster: the fake
// dynamic client's tracker implements apply as a merge onto an existing object
// and cannot create, so the create-or-replace half is prepended here.
func newFakeDynamic() *dynamicfake.FakeDynamicClient {
	c := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(dynamicScheme(), nil)
	c.PrependReactor("patch", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		pa, ok := action.(k8stesting.PatchActionImpl)
		if !ok || pa.PatchType != types.ApplyPatchType {
			return false, nil, nil
		}
		obj := &unstructured.Unstructured{}
		if err := obj.UnmarshalJSON(pa.Patch); err != nil {
			return true, nil, err
		}
		tracker := c.Tracker()
		gvr, ns := pa.GetResource(), pa.GetNamespace()
		_, err := tracker.Get(gvr, ns, obj.GetName())
		switch {
		case apierrors.IsNotFound(err):
			if err := tracker.Create(gvr, obj, ns); err != nil {
				return true, nil, err
			}
		case err != nil:
			return true, nil, err
		default:
			if err := tracker.Update(gvr, obj, ns); err != nil {
				return true, nil, err
			}
		}
		return true, obj, nil
	})
	return c
}

var serviceGVK = schema.GroupVersionKind{Version: "v1", Kind: "Service"}

func dynamicScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	s.AddKnownTypeWithName(serviceGVK, &unstructured.Unstructured{})
	listGVK := serviceGVK
	listGVK.Kind += "List"
	s.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})
	return s
}

func testMapper() meta.RESTMapper {
	m := meta.NewDefaultRESTMapper([]schema.GroupVersion{serviceGVK.GroupVersion()})
	m.Add(serviceGVK, meta.RESTScopeNamespace)
	return m
}
