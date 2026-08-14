package install

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

// The API surface these tests plan against: what an upstream install manifest
// actually contains (a Namespace, CRDs, RBAC, a controller Deployment, a webhook
// configuration), the FluxInstance kelson authors, and one custom resource kind
// so a CRD deletion has collateral to name.
var testKinds = []struct {
	gvk   schema.GroupVersionKind
	scope meta.RESTScope
}{
	{schema.GroupVersionKind{Version: "v1", Kind: "Namespace"}, meta.RESTScopeRoot},
	{schema.GroupVersionKind{Version: "v1", Kind: "ServiceAccount"}, meta.RESTScopeNamespace},
	{schema.GroupVersionKind{Version: "v1", Kind: "Service"}, meta.RESTScopeNamespace},
	{schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, meta.RESTScopeNamespace},
	{schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}, meta.RESTScopeNamespace},
	{schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole"}, meta.RESTScopeRoot},
	{schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRoleBinding"}, meta.RESTScopeRoot},
	{schema.GroupVersionKind{Group: "admissionregistration.k8s.io", Version: "v1", Kind: "ValidatingWebhookConfiguration"}, meta.RESTScopeRoot},
	{schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"}, meta.RESTScopeRoot},
	{schema.GroupVersionKind{Group: "fluxcd.controlplane.io", Version: "v1", Kind: "FluxInstance"}, meta.RESTScopeNamespace},
	{schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Cluster"}, meta.RESTScopeNamespace},
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

func gvrForKind(t *testing.T, kind string) schema.GroupVersionResource {
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

// testCatalog is the static Catalog the removal tests sweep with, in an order
// that is deliberately NOT the removal order.
type testCatalog struct {
	gaps []string
	err  error
}

func (c testCatalog) Resources(context.Context) ([]APIResource, []string, error) {
	if c.err != nil {
		return nil, nil, c.err
	}
	var out []APIResource
	for _, k := range testKinds {
		gvr, _ := meta.UnsafeGuessKindToResource(k.gvk)
		out = append(out, APIResource{
			GVR:        gvr,
			Kind:       k.gvk.Kind,
			Namespaced: k.scope.Name() == meta.RESTScopeNameNamespace,
		})
	}
	return out, c.gaps, nil
}

// cluster is the fake API server these tests apply into.
//
// The fake dynamic client's tracker implements apply as a merge onto an object
// that must already exist, so it can never exercise the create path an install
// is almost entirely made of. This prepends create-or-replace, and — because an
// install waits for a CRD it applied to become servable — it also does the one
// thing the real API server's CRD controller does that the tracker does not:
// mark a created CustomResourceDefinition Established.
type cluster struct {
	dyn *dynamicfake.FakeDynamicClient

	mu sync.Mutex
	// applies records the order objects reached the API, so a test can assert
	// that the manifest's document order survives.
	applies []string
	deletes []string
	// unestablished names CRDs the fake refuses to mark Established, standing
	// in for an API server that has not caught up.
	unestablished map[string]bool
	// applyErrors maps an object key to the error its apply returns.
	applyErrors map[string]error
}

func newCluster(objs ...*unstructured.Unstructured) *cluster {
	runtimeObjs := make([]runtime.Object, 0, len(objs))
	for _, o := range objs {
		runtimeObjs = append(runtimeObjs, o)
	}
	c := &cluster{
		dyn:           dynamicfake.NewSimpleDynamicClient(testScheme(), runtimeObjs...),
		unestablished: map[string]bool{},
		applyErrors:   map[string]error{},
	}
	c.dyn.PrependReactor("patch", "*", c.reactApply)
	c.dyn.PrependReactor("delete", "*", c.recordDelete)
	return c
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
	err := c.applyErrors[key]
	c.mu.Unlock()
	if err != nil {
		return true, nil, err
	}

	c.mu.Lock()
	c.applies = append(c.applies, key)
	c.mu.Unlock()

	tracker := c.dyn.Tracker()
	gvr, ns := pa.GetResource(), pa.GetNamespace()
	obj.SetUID(types.UID(key + "-uid"))
	if obj.GetKind() == "CustomResourceDefinition" && !c.isUnestablished(obj.GetName()) {
		markEstablished(obj)
	}
	existing, getErr := tracker.Get(gvr, ns, obj.GetName())
	switch {
	case apierrors.IsNotFound(getErr):
		if err := tracker.Create(gvr, obj, ns); err != nil {
			return true, nil, err
		}
	case getErr != nil:
		return true, nil, getErr
	default:
		if old, ok := existing.(*unstructured.Unstructured); ok {
			obj.SetUID(old.GetUID())
		}
		if err := tracker.Update(gvr, obj, ns); err != nil {
			return true, nil, err
		}
	}
	return true, obj, nil
}

func (c *cluster) isUnestablished(name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.unestablished[name]
}

func markEstablished(crd *unstructured.Unstructured) {
	_ = unstructured.SetNestedSlice(crd.Object, []any{
		map[string]any{"type": "Established", "status": "True"},
	}, "status", "conditions")
}

func (c *cluster) recordDelete(action k8stesting.Action) (bool, runtime.Object, error) {
	da, ok := action.(k8stesting.DeleteActionImpl)
	if !ok {
		return false, nil, nil
	}
	c.mu.Lock()
	c.deletes = append(c.deletes, da.GetResource().Resource+"/"+da.Name)
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

func (c *cluster) failApply(key string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.applyErrors[key] = err
}

// get reads a live object, or nil when it is absent.
func (c *cluster) get(t *testing.T, kind, namespace, name string) *unstructured.Unstructured {
	t.Helper()
	obj, err := c.dyn.Tracker().Get(gvrForKind(t, kind), namespace, name)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("get %s/%s/%s: %v", kind, namespace, name, err)
	}
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		t.Fatalf("tracker returned %T, want *unstructured.Unstructured", obj)
	}
	return u
}

// seed puts an object in the cluster that kelson did not apply.
func (c *cluster) seed(t *testing.T, obj *unstructured.Unstructured) {
	t.Helper()
	if err := c.dyn.Tracker().Create(gvrForKind(t, obj.GetKind()), obj, obj.GetNamespace()); err != nil {
		t.Fatalf("seed %s: %v", objKey(obj), err)
	}
}

func objKey(obj *unstructured.Unstructured) string {
	return fmt.Sprintf("%s/%s/%s", obj.GetKind(), obj.GetNamespace(), obj.GetName())
}

// --- fixtures ----------------------------------------------------------------

// fakeFetcher serves fixture manifests by URL, and records what was asked for.
// No test in this package opens a socket.
type fakeFetcher struct {
	bodies map[string][]byte
	err    error
	// asked records every URL requested, in order.
	asked []string
}

func (f *fakeFetcher) Fetch(_ context.Context, url string) ([]byte, error) {
	f.asked = append(f.asked, url)
	if f.err != nil {
		return nil, f.err
	}
	body, ok := f.bodies[url]
	if !ok {
		return nil, fmt.Errorf("fakeFetcher: no fixture for %s", url)
	}
	return body, nil
}

func digestOf(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// pinnedFixture builds a Component row whose digest matches the fixture bytes,
// plus a fetcher that serves them. Tests install against these rather than
// against the real pins table, because the real table's digests describe
// megabytes of upstream YAML that no unit test should carry or download.
func pinnedFixture(name, namespace, profileField string, body []byte) (Component, *fakeFetcher) {
	url := "https://example.invalid/" + name + "/v9.9.9/install.yaml"
	c := Component{
		Name:         name,
		Title:        name,
		Status:       StatusSupported,
		Version:      "v9.9.9",
		ManifestURL:  url,
		SHA256:       digestOf(body),
		Namespace:    namespace,
		ProfileField: profileField,
		Provides:     "the fixture's capability",
	}
	return c, &fakeFetcher{bodies: map[string][]byte{url: body}}
}

// withComponents swaps the pins table for the duration of one test. The table
// is a package-level var because it is documentation as much as data; swapping
// it here is what lets the plan-and-apply path be exercised end to end without
// a network fetch of the real upstream manifests.
func withComponents(t *testing.T, comps ...Component) {
	t.Helper()
	original := Components
	Components = comps
	t.Cleanup(func() { Components = original })
}

// certManagerFixture is a cert-manager-shaped install manifest: the shape of
// every row in the pins table, small enough to assert on.
const certManagerFixture = `apiVersion: v1
kind: Namespace
metadata:
  name: cert-manager
  labels:
    app.kubernetes.io/name: cert-manager
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: clusterissuers.cert-manager.io
spec:
  group: cert-manager.io
  names:
    kind: ClusterIssuer
    plural: clusterissuers
  scope: Cluster
  versions:
    - name: v1
      served: true
      storage: true
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: cert-manager
  namespace: cert-manager
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: cert-manager-controller
rules: []
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: cert-manager
  namespace: cert-manager
spec:
  replicas: 1
---
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingWebhookConfiguration
metadata:
  name: cert-manager-webhook
webhooks: []
`

// fluxOperatorFixture is flux-operator's install manifest in miniature: the
// namespace, the FluxInstance CRD, and the operator Deployment.
const fluxOperatorFixture = `apiVersion: v1
kind: Namespace
metadata:
  name: flux-system
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: fluxinstances.fluxcd.controlplane.io
spec:
  group: fluxcd.controlplane.io
  names:
    kind: FluxInstance
    plural: fluxinstances
  scope: Namespaced
  versions:
    - name: v1
      served: true
      storage: true
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: flux-operator
  namespace: flux-system
spec:
  replicas: 1
`

// cnpgFixture carries a CRD whose custom resources are databases, so a removal
// preview has real collateral to name.
const cnpgFixture = `apiVersion: v1
kind: Namespace
metadata:
  name: cnpg-system
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: clusters.postgresql.cnpg.io
spec:
  group: postgresql.cnpg.io
  names:
    kind: Cluster
    plural: clusters
  scope: Namespaced
  versions:
    - name: v1
      served: true
      storage: true
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: cnpg-controller-manager
  namespace: cnpg-system
spec:
  replicas: 1
`

// newInstaller wires an Installer over a fake cluster and fetcher.
func newInstaller(t *testing.T, c *cluster, f Fetcher) *Installer {
	t.Helper()
	i, err := New(Options{
		Client: c.dyn,
		Mapper: testMapper(),
		Fetch:  f,
		// The fake marks a CRD Established the moment it is applied, so the wait
		// resolves on its first read. A test that wants the timeout sets
		// unestablished and passes its own options.
		EstablishPoll: 1,
	})
	if err != nil {
		t.Fatalf("new installer: %v", err)
	}
	return i
}

func newRemover(t *testing.T, c *cluster, catalog Catalog) *Remover {
	t.Helper()
	r, err := NewRemover(RemoverOptions{Client: c.dyn, Catalog: catalog})
	if err != nil {
		t.Fatalf("new remover: %v", err)
	}
	return r
}

// object builds a live object with the given labels and annotations.
func object(apiVersion, kind, name, namespace string, mutate ...func(*unstructured.Unstructured)) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": apiVersion,
		"kind":       kind,
		"metadata": map[string]any{
			"name": name,
			"uid":  kind + "-" + name + "-uid",
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

// installed stamps the provenance kelson writes at install time.
func installed(component, version, ownership string) func(*unstructured.Unstructured) {
	return func(obj *unstructured.Unstructured) {
		lbls := obj.GetLabels()
		if lbls == nil {
			lbls = map[string]string{}
		}
		lbls[LabelComponent] = component
		lbls[LabelVersion] = version
		obj.SetLabels(lbls)
		if ownership == "" {
			return
		}
		ann := obj.GetAnnotations()
		if ann == nil {
			ann = map[string]string{}
		}
		ann[AnnOwnership] = ownership
		obj.SetAnnotations(ann)
	}
}

func refsOfTargets(targets []RemovalTarget) []string {
	out := make([]string, 0, len(targets))
	for _, t := range targets {
		out = append(out, t.Ref.String())
	}
	return out
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
