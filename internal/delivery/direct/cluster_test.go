package direct

import (
	"fmt"
	"sync"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

// The fake dynamic client's object tracker implements apply as a strategic
// merge onto an existing object: it cannot create, and it tracks no field
// ownership, so it can neither exercise the create path nor produce a
// conflict. cluster prepends the missing behaviour — create-or-replace, plus
// seeded foreign field ownership — which is exactly the surface the adapter
// depends on. Everything else (get, list with label selectors, delete) is the
// real fake.
type cluster struct {
	dyn *dynamicfake.FakeDynamicClient

	mu sync.Mutex
	// applies/deletes record the order operations reached the API, so tests
	// can assert the renderer's apply order survives.
	applies  []string
	deletes  []string
	managers []string
	forced   []bool
	// conflicts maps a resource key to the fields another field manager owns.
	conflicts map[string]conflict
}

type conflict struct {
	owner  string
	fields []string
}

func newCluster(objs ...runtime.Object) *cluster {
	c := &cluster{
		dyn:       dynamicfake.NewSimpleDynamicClientWithCustomListKinds(testScheme(), nil, objs...),
		conflicts: map[string]conflict{},
	}
	c.dyn.PrependReactor("patch", "*", c.reactApply)
	c.dyn.PrependReactor("delete", "*", c.recordDelete)
	return c
}

// ownedElsewhere seeds a field owned by another field manager, so the next
// apply of that resource conflicts the way a real API server conflicts.
func (c *cluster) ownedElsewhere(key, owner string, fields ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.conflicts[key] = conflict{owner: owner, fields: fields}
}

func (c *cluster) reactApply(action k8stesting.Action) (bool, runtime.Object, error) {
	pa, ok := action.(k8stesting.PatchActionImpl)
	if !ok || pa.PatchType != types.ApplyPatchType {
		return false, nil, nil
	}
	obj := &unstructured.Unstructured{}
	if err := obj.UnmarshalJSON(pa.Patch); err != nil {
		return true, nil, err
	}
	key := objKey(obj)

	c.mu.Lock()
	conf, conflicted := c.conflicts[key]
	forced := pa.PatchOptions.Force != nil && *pa.PatchOptions.Force
	c.mu.Unlock()
	if conflicted && !forced {
		causes := make([]metav1.StatusCause, 0, len(conf.fields))
		for _, f := range conf.fields {
			causes = append(causes, metav1.StatusCause{
				Type:    metav1.CauseTypeFieldManagerConflict,
				Message: fmt.Sprintf("conflict with %q using %s", conf.owner, obj.GetAPIVersion()),
				Field:   f,
			})
		}
		return true, nil, apierrors.NewApplyConflict(causes,
			fmt.Sprintf("Apply failed with %d conflict(s)", len(causes)))
	}

	c.mu.Lock()
	c.applies = append(c.applies, key)
	c.managers = append(c.managers, pa.PatchOptions.FieldManager)
	c.forced = append(c.forced, forced)
	c.mu.Unlock()

	tracker := c.dyn.Tracker()
	gvr, ns := pa.GetResource(), pa.GetNamespace()
	existing, err := tracker.Get(gvr, ns, obj.GetName())
	switch {
	case apierrors.IsNotFound(err):
		if err := tracker.Create(gvr, obj, ns); err != nil {
			return true, nil, err
		}
	case err != nil:
		return true, nil, err
	default:
		// The API server owns status; an apply that does not mention it must
		// not wipe the readback Status() depends on.
		if old, ok := existing.(*unstructured.Unstructured); ok {
			if st, found, _ := unstructured.NestedMap(old.Object, "status"); found {
				if _, has, _ := unstructured.NestedMap(obj.Object, "status"); !has {
					if err := unstructured.SetNestedMap(obj.Object, st, "status"); err != nil {
						return true, nil, err
					}
				}
			}
		}
		if err := tracker.Update(gvr, obj, ns); err != nil {
			return true, nil, err
		}
	}
	return true, obj, nil
}

func (c *cluster) recordDelete(action k8stesting.Action) (bool, runtime.Object, error) {
	da, ok := action.(k8stesting.DeleteActionImpl)
	if !ok {
		return false, nil, nil
	}
	c.mu.Lock()
	c.deletes = append(c.deletes, fmt.Sprintf("%s/%s/%s", da.GetResource().Resource, da.GetNamespace(), da.Name))
	c.mu.Unlock()
	return false, nil, nil
}

func (c *cluster) applyLog() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.applies...)
}

func (c *cluster) deleteLog() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.deletes...)
}

// get reads a live object, or nil when it is absent.
func (c *cluster) get(t *testing.T, resource, namespace, name string) *unstructured.Unstructured {
	t.Helper()
	obj, err := c.dyn.Tracker().Get(gvrFor(t, resource), namespace, name)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("get %s/%s/%s: %v", resource, namespace, name, err)
	}
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		t.Fatalf("tracker returned %T, want *unstructured.Unstructured", obj)
	}
	return u
}

// update replaces a live object, standing in for whatever else writes to the
// cluster between two kelson deploys.
func (c *cluster) update(t *testing.T, resource string, obj *unstructured.Unstructured) {
	t.Helper()
	if err := c.dyn.Tracker().Update(gvrFor(t, resource), obj, obj.GetNamespace()); err != nil {
		t.Fatalf("update %s: %v", resource, err)
	}
}

func objKey(obj *unstructured.Unstructured) string {
	return fmt.Sprintf("%s/%s/%s", obj.GetKind(), obj.GetNamespace(), obj.GetName())
}

// --- scheme and mapper ------------------------------------------------------

// testKinds is the slice of the API the adapter tests exercise: the kinds the
// renderer emits today plus a namespace to prove cluster-scoped handling.
var testKinds = []struct {
	gvk   schema.GroupVersionKind
	scope meta.RESTScope
}{
	{schema.GroupVersionKind{Version: "v1", Kind: "Namespace"}, meta.RESTScopeRoot},
	{schema.GroupVersionKind{Version: "v1", Kind: "Service"}, meta.RESTScopeNamespace},
	{schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, meta.RESTScopeNamespace},
	{schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}, meta.RESTScopeNamespace},
	{schema.GroupVersionKind{Group: "batch", Version: "v1", Kind: "CronJob"}, meta.RESTScopeNamespace},
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

func testMapper() meta.RESTMapper {
	versions := make([]schema.GroupVersion, 0, len(testKinds))
	for _, k := range testKinds {
		versions = append(versions, k.gvk.GroupVersion())
	}
	m := meta.NewDefaultRESTMapper(versions)
	for _, k := range testKinds {
		m.Add(k.gvk, k.scope)
	}
	return m
}

func gvrFor(t *testing.T, resource string) schema.GroupVersionResource {
	t.Helper()
	for _, k := range testKinds {
		gvr, _ := meta.UnsafeGuessKindToResource(k.gvk)
		if gvr.Resource == resource {
			return gvr
		}
	}
	t.Fatalf("no test kind for resource %q", resource)
	return schema.GroupVersionResource{}
}
