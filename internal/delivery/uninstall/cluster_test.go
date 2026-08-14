package uninstall

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/dafrie/kelson/internal/delivery"
)

// The fixture cluster these tests plan against.
const (
	testProject = "checkout"
	testEnv     = "production"
	testNS      = "checkout-production"
)

// testKinds is the slice of the API surface these tests exercise: what the
// renderer emits (workloads, routes, config, data services), the derived kinds
// that carry the same labels but must never be swept, and a Namespace.
var testKinds = []struct {
	gvk   schema.GroupVersionKind
	scope meta.RESTScope
}{
	{schema.GroupVersionKind{Version: "v1", Kind: "Namespace"}, meta.RESTScopeRoot},
	{schema.GroupVersionKind{Version: "v1", Kind: "Service"}, meta.RESTScopeNamespace},
	{schema.GroupVersionKind{Version: "v1", Kind: "ServiceAccount"}, meta.RESTScopeNamespace},
	{schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, meta.RESTScopeNamespace},
	{schema.GroupVersionKind{Version: "v1", Kind: "Secret"}, meta.RESTScopeNamespace},
	{schema.GroupVersionKind{Version: "v1", Kind: "PersistentVolumeClaim"}, meta.RESTScopeNamespace},
	{schema.GroupVersionKind{Version: "v1", Kind: "Pod"}, meta.RESTScopeNamespace},
	{schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}, meta.RESTScopeNamespace},
	{schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "ReplicaSet"}, meta.RESTScopeNamespace},
	{schema.GroupVersionKind{Group: "batch", Version: "v1", Kind: "CronJob"}, meta.RESTScopeNamespace},
	{schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "HTTPRoute"}, meta.RESTScopeNamespace},
	{schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Cluster"}, meta.RESTScopeNamespace},
	{schema.GroupVersionKind{Group: "valkey.io", Version: "v1alpha1", Kind: "ValkeyCluster"}, meta.RESTScopeNamespace},
}

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	for _, k := range testKinds {
		s.AddKnownTypeWithName(k.gvk, &unstructured.Unstructured{})
		listGVK := k.gvk
		listGVK.Kind += "List"
		s.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})
	}
	return s
}

// testCatalog is the static Catalog these tests plan against: every namespaced
// test kind, in an order that is deliberately NOT the deletion order, so a
// passing ordering assertion is about tierOf and not about input order.
type testCatalog struct {
	gaps []string
	err  error
}

func (c testCatalog) Namespaced(context.Context) ([]APIResource, []string, error) {
	if c.err != nil {
		return nil, nil, c.err
	}
	var out []APIResource
	for _, k := range testKinds {
		if k.scope.Name() == meta.RESTScopeNameRoot {
			continue
		}
		gvr, _ := meta.UnsafeGuessKindToResource(k.gvk)
		out = append(out, APIResource{GVR: gvr, Kind: k.gvk.Kind})
	}
	return out, c.gaps, nil
}

func gvrFor(t *testing.T, kind string) schema.GroupVersionResource {
	t.Helper()
	for _, k := range testKinds {
		if k.gvk.Kind == kind {
			gvr, _ := meta.UnsafeGuessKindToResource(k.gvk)
			return gvr
		}
	}
	t.Fatalf("no test kind %q", kind)
	return schema.GroupVersionResource{}
}

// object builds a live object carrying whatever labels and annotations the case
// needs. The provenance labels are the default because they are what almost
// every fixture wants; labelSet removes or overrides them for the bystanders.
func object(apiVersion, kind, name, namespace string, mutate ...func(*unstructured.Unstructured)) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": apiVersion,
		"kind":       kind,
		"metadata": map[string]any{
			"name": name,
			"uid":  string(kind + "-" + name + "-uid"),
			"labels": map[string]any{
				delivery.LabelManagedBy:   delivery.ManagedByKelson,
				delivery.LabelProject:     testProject,
				delivery.LabelEnvironment: testEnv,
			},
		},
	}}
	if namespace != "" {
		obj.SetNamespace(namespace)
	}
	for _, m := range mutate {
		m(obj)
	}
	return obj
}

// labels overrides or removes labels; an empty value removes the key.
func withLabels(kv map[string]string) func(*unstructured.Unstructured) {
	return func(obj *unstructured.Unstructured) {
		lbls := obj.GetLabels()
		if lbls == nil {
			lbls = map[string]string{}
		}
		for k, v := range kv {
			if v == "" {
				delete(lbls, k)
				continue
			}
			lbls[k] = v
		}
		obj.SetLabels(lbls)
	}
}

func withAnnotations(kv map[string]string) func(*unstructured.Unstructured) {
	return func(obj *unstructured.Unstructured) {
		ann := obj.GetAnnotations()
		if ann == nil {
			ann = map[string]string{}
		}
		for k, v := range kv {
			ann[k] = v
		}
		obj.SetAnnotations(ann)
	}
}

// namespaceObject is the Namespace as the renderer emits it plus the ownership
// value the delivery plane recorded.
func namespaceObject(name, ownership string, mutate ...func(*unstructured.Unstructured)) *unstructured.Unstructured {
	m := append([]func(*unstructured.Unstructured){
		withAnnotations(map[string]string{delivery.AnnNamespaceOwnership: ownership}),
	}, mutate...)
	return object("v1", "Namespace", name, "", m...)
}

func newFakeClient(objs ...*unstructured.Unstructured) *dynamicfake.FakeDynamicClient {
	runtimeObjs := make([]runtime.Object, 0, len(objs))
	for _, o := range objs {
		runtimeObjs = append(runtimeObjs, o)
	}
	return dynamicfake.NewSimpleDynamicClient(testScheme(), runtimeObjs...)
}

// newUninstaller wires an Uninstaller over a fake cluster holding objs.
func newUninstaller(t *testing.T, keepData bool, objs ...*unstructured.Unstructured) (*Uninstaller, *dynamicfake.FakeDynamicClient) {
	t.Helper()
	client := newFakeClient(objs...)
	u, err := New(Options{Client: client, Catalog: testCatalog{}, KeepData: keepData})
	if err != nil {
		t.Fatalf("new uninstaller: %v", err)
	}
	return u, client
}

func testScope() Scope {
	return Scope{Project: testProject, Environment: testEnv}
}

// planFor is the common "plan the fixture" step.
func planFor(t *testing.T, u *Uninstaller, scope Scope) *Plan {
	t.Helper()
	plan, err := u.Plan(context.Background(), scope)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	return plan
}

// refs renders a plan's targets in order, for assertions and failure messages.
func refs(targets []Target) []string {
	out := make([]string, 0, len(targets))
	for _, t := range targets {
		out = append(out, t.Ref.String())
	}
	return out
}

func has(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
