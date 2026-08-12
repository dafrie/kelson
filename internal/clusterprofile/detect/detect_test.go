package detect

import (
	"context"
	"reflect"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/dafrie/kelson/internal/clusterprofile"
)

// fakeProber stands up a discovery fake and a dynamic fake sharing one set of
// "registered" groups and objects, so a probe exercises the same presence-via-
// /apis and details-via-list flow a real cluster drives.
type fakeProber struct {
	prober
	disco *fake.FakeDiscovery
	dyn   *dynamicfake.FakeDynamicClient
}

func newFakeProber(t *testing.T, resources []*metav1.APIResourceList) *fakeProber {
	t.Helper()
	fd := &fake.FakeDiscovery{
		Fake:               &k8stesting.Fake{},
		FakedServerVersion: &version.Info{GitVersion: "v1.31.2+k3s1"},
	}
	fd.Resources = resources
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(detectScheme(), nil)
	return &fakeProber{prober: prober{disco: fd, dyn: dyn}, disco: fd, dyn: dyn}
}

// detectScheme registers the List kinds detection lists, so the fake client can
// build an empty list even when nothing is seeded (exactly the absent case).
func detectScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	for _, k := range []schema.GroupVersionKind{
		{Version: "v1", Kind: "Node"},
		{Group: "networking.k8s.io", Version: "v1", Kind: "IngressClass"},
		{Group: "storage.k8s.io", Version: "v1", Kind: "StorageClass"},
		{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotClass"},
		{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "GatewayClass"},
		{Group: "cert-manager.io", Version: "v1", Kind: "ClusterIssuer"},
		{Group: "external-secrets.io", Version: "v1", Kind: "ClusterSecretStore"},
		{Group: "external-secrets.io", Version: "v1", Kind: "SecretStore"},
	} {
		s.AddKnownTypeWithName(k, &unstructured.Unstructured{})
		lk := k
		lk.Kind += "List"
		s.AddKnownTypeWithName(lk, &unstructured.UnstructuredList{})
	}
	return s
}

// seed inserts an unstructured object into the fake tracker, standing in for a
// live cluster resource.
func (f *fakeProber) seed(t *testing.T, gvr schema.GroupVersionResource, obj *unstructured.Unstructured) {
	t.Helper()
	if err := f.dyn.Tracker().Create(gvr, obj, obj.GetNamespace()); err != nil {
		t.Fatalf("seeding %s/%s: %v", gvr, obj.GetName(), err)
	}
}

func unstruct(gvk schema.GroupVersionKind, name string, body map[string]any) *unstructured.Unstructured {
	obj := map[string]any{"apiVersion": gvk.GroupVersion().String(), "kind": gvk.Kind}
	for k, v := range body {
		obj[k] = v
	}
	meta, _ := obj["metadata"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
	}
	meta["name"] = name
	obj["metadata"] = meta
	return &unstructured.Unstructured{Object: obj}
}

// resourceList is a discovery APIResourceList for one group/version, with an
// optional set of kind names (used for the monitoring group's kind detection).
func resourceList(groupVersion string, kinds ...string) *metav1.APIResourceList {
	l := &metav1.APIResourceList{GroupVersion: groupVersion}
	for _, k := range kinds {
		l.APIResources = append(l.APIResources, metav1.APIResource{Name: k})
	}
	return l
}

// --- happy path ------------------------------------------------------------

func TestProbeFullCluster(t *testing.T) {
	f := newFakeProber(t, []*metav1.APIResourceList{
		resourceList("gateway.networking.k8s.io/v1"),
		resourceList("cert-manager.io/v1"),
		resourceList("external-secrets.io/v1"),
		resourceList("kyverno.io/v1"),
		resourceList("templates.gatekeeper.sh/v1"),
		resourceList("postgresql.cnpg.io/v1"),
		resourceList("source.toolkit.fluxcd.io/v1"),
		resourceList("argoproj.io/v1alpha1"),
		resourceList("metrics.k8s.io/v1beta1"),
		resourceList("monitoring.coreos.com/v1", "servicemonitors", "podmonitors"),
	})
	f.seed(t, coreV1Nodes, unstruct(schema.GroupVersionKind{Version: "v1", Kind: "Node"}, "n1",
		map[string]any{"status": map[string]any{"nodeInfo": map[string]any{"architecture": "amd64"}}}))
	f.seed(t, coreV1Nodes, unstruct(schema.GroupVersionKind{Version: "v1", Kind: "Node"}, "n2",
		map[string]any{"status": map[string]any{"nodeInfo": map[string]any{"architecture": "arm64"}}}))
	f.seed(t, gatewayClassGVR, unstruct(schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "GatewayClass"}, "envoy", nil))
	f.seed(t, ingressClassGVR, unstruct(schema.GroupVersionKind{Group: "networking.k8s.io", Version: "v1", Kind: "IngressClass"}, "nginx", map[string]any{
		"metadata": map[string]any{"annotations": map[string]any{"ingressclass.kubernetes.io/is-default-class": "true"}},
		"spec":     map[string]any{"controller": "k8s.io/ingress-nginx"},
	}))
	f.seed(t, clusterIssuerGVR, unstruct(schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "ClusterIssuer"}, "letsencrypt-prod", nil))
	f.seed(t, clusterSecretStoreGVR, unstruct(schema.GroupVersionKind{Group: "external-secrets.io", Version: "v1", Kind: "ClusterSecretStore"}, "vault", nil))
	f.seed(t, secretStoreGVR, unstruct(schema.GroupVersionKind{Group: "external-secrets.io", Version: "v1", Kind: "SecretStore"}, "vault-ns", nil))
	f.seed(t, storageClassGVR, unstruct(schema.GroupVersionKind{Group: "storage.k8s.io", Version: "v1", Kind: "StorageClass"}, "standard", map[string]any{
		"metadata":    map[string]any{"annotations": map[string]any{"storageclass.kubernetes.io/is-default-class": "true"}},
		"provisioner": "pd.csi.storage.gke.io",
	}))
	f.seed(t, volumeSnapshotClassGVR, unstruct(schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotClass"}, "gke-snap",
		map[string]any{"driver": "pd.csi.storage.gke.io"}))

	prof, err := f.probe(context.Background())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if len(prof.Incomplete) != 0 {
		t.Fatalf("expected no gaps, got %+v", prof.Incomplete)
	}

	k := prof.Kubernetes
	if k == nil || k.Version != "v1.31.2+k3s1" || k.Platform != "k3s" {
		t.Fatalf("kubernetes = %+v, want v1.31.2+k3s1 on k3s", k)
	}
	if !reflect.DeepEqual(k.NodeArchitectures, []string{"amd64", "arm64"}) {
		t.Fatalf("node architectures = %v", k.NodeArchitectures)
	}
	if prof.GatewayAPI == nil || prof.GatewayAPI.Version != "v1" || !reflect.DeepEqual(prof.GatewayAPI.Classes, []string{"envoy"}) {
		t.Fatalf("gateway = %+v", prof.GatewayAPI)
	}
	if len(prof.IngressClasses) != 1 || prof.IngressClasses[0].Name != "nginx" || !prof.IngressClasses[0].Default || prof.IngressClasses[0].Controller != "k8s.io/ingress-nginx" {
		t.Fatalf("ingress classes = %+v", prof.IngressClasses)
	}
	if prof.CertManager == nil || !reflect.DeepEqual(prof.CertManager.ClusterIssuers, []string{"letsencrypt-prod"}) {
		t.Fatalf("cert manager = %+v", prof.CertManager)
	}
	if prof.ExternalSecrets == nil ||
		!reflect.DeepEqual(prof.ExternalSecrets.ClusterSecretStores, []string{"vault"}) ||
		!reflect.DeepEqual(prof.ExternalSecrets.SecretStores, []string{"vault-ns"}) {
		t.Fatalf("external secrets = %+v", prof.ExternalSecrets)
	}
	if !prof.HasPolicyEngine() || len(prof.PolicyEngines) != 2 {
		t.Fatalf("policy engines = %+v", prof.PolicyEngines)
	}
	if len(prof.StorageClasses) != 1 {
		t.Fatalf("storage classes = %+v", prof.StorageClasses)
	}
	if sc := prof.StorageClasses[0]; !sc.Default || sc.Provisioner != "pd.csi.storage.gke.io" || sc.VolumeSnapshotClass != "gke-snap" {
		t.Fatalf("storage class = %+v", sc)
	}
	if prof.CloudNativePG == nil || prof.Flux == nil || prof.ArgoCD == nil || prof.MetricsServer == nil {
		t.Fatalf("expected cnpg/flux/argocd/metrics present, got %+v", prof)
	}
	if prof.Prometheus == nil || !prof.Prometheus.ServiceMonitor || !prof.Prometheus.PodMonitor {
		t.Fatalf("prometheus = %+v", prof.Prometheus)
	}
}

// --- absent: a successful empty view is absence ------------------------------

func TestProbeEmptyCluster(t *testing.T) {
	f := newFakeProber(t, nil) // /apis succeeds but no groups registered
	prof, err := f.probe(context.Background())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if len(prof.Incomplete) != 0 {
		t.Fatalf("an empty but readable cluster is a finding, not a gap: %+v", prof.Incomplete)
	}
	if prof.Kubernetes == nil || prof.Kubernetes.Version != "v1.31.2+k3s1" {
		t.Fatalf("kubernetes = %+v", prof.Kubernetes)
	}
	if prof.GatewayAPI != nil || prof.CertManager != nil || prof.Prometheus != nil {
		t.Fatalf("expected absent components on an empty cluster: %+v", prof)
	}
	if len(prof.IngressClasses) != 0 || len(prof.PolicyEngines) != 0 || len(prof.StorageClasses) != 0 {
		t.Fatalf("expected empty lists on an empty cluster: %+v", prof)
	}
}

// --- forbidden details: present component, gap on the field -------------------

func TestProbeForbiddenClusterIssuersKeepsCertManagerPresent(t *testing.T) {
	f := newFakeProber(t, []*metav1.APIResourceList{resourceList("cert-manager.io/v1")})
	f.dyn.PrependReactor("list", "clusterissuers", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "cert-manager.io", Resource: "clusterissuers"}, "", nil)
	})

	prof, err := f.probe(context.Background())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	// The contract's critical property: CertManager stays present (its group was
	// discovered) and the inability to read issuers is recorded, never collapsed
	// into "no ClusterIssuers" which would make the renderer emit TLS-less Ingress.
	if prof.CertManager == nil {
		t.Fatal("CertManager must stay present when only its issuers are forbidden")
	}
	if len(prof.CertManager.ClusterIssuers) != 0 {
		t.Fatalf("forbidden issuers must not be reported as found: %v", prof.CertManager.ClusterIssuers)
	}
	if !hasGap(prof.Incomplete, "certManager.clusterIssuers") {
		t.Fatalf("expected a gap on certManager.clusterIssuers, got %+v", prof.Incomplete)
	}
}

func TestProbeForbiddenStorageKeepsSnapshotGap(t *testing.T) {
	f := newFakeProber(t, []*metav1.APIResourceList{resourceList("storage.k8s.io/v1"), resourceList("snapshot.storage.k8s.io/v1")})
	f.seed(t, storageClassGVR, unstruct(schema.GroupVersionKind{Group: "storage.k8s.io", Version: "v1", Kind: "StorageClass"}, "local",
		map[string]any{"provisioner": "rancher.io/local-path"}))
	f.dyn.PrependReactor("list", "volumesnapshotclasses", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "snapshot.storage.k8s.io", Resource: "volumesnapshotclasses"}, "", nil)
	})

	prof, err := f.probe(context.Background())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !hasGap(prof.Incomplete, "storageClasses") {
		t.Fatalf("expected a gap on storageClasses (snapshot), got %+v", prof.Incomplete)
	}
	// The class itself is still visible and its snapshot field stays empty rather
	// than being guessed to something.
	if len(prof.StorageClasses) != 1 || prof.StorageClasses[0].VolumeSnapshotClass != "" {
		t.Fatalf("storage classes = %+v", prof.StorageClasses)
	}
}

// --- forbidden /apis: every component becomes a gap, not absence -------------

func TestProbeForbiddenAPIsGapsEverything(t *testing.T) {
	f := newFakeProber(t, nil)
	// The discovery fake reports a forbidden /apis: ServerGroups must fail, which
	// means no component presence can be trusted.
	f.disco.PrependReactor("get", "group", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "groups"}, "", nil)
	})

	prof, err := f.probe(context.Background())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if prof.CertManager != nil || prof.GatewayAPI != nil || prof.Prometheus != nil {
		t.Fatalf("no component may claim presence when /apis is forbidden: %+v", prof)
	}
	for _, want := range []string{"gatewayAPI", "certManager", "externalSecrets", "cnpg", "flux", "argocd", "metricsServer", "prometheus", "policyEngines"} {
		if !hasGap(prof.Incomplete, want) {
			t.Fatalf("expected a gap on %q, got %+v", want, prof.Incomplete)
		}
	}
}

// --- platform and node architecture edge cases --------------------------------

func TestDetectPlatform(t *testing.T) {
	cases := map[string]string{
		"v1.30.3+k3s1":        "k3s",
		"v1.29.3-gke.1389000": "gke",
		"v1.30.4-eks-0d6f9b6": "eks",
		"v1.32.0":             "",
		"v2.0.0-talos":        "talos",
	}
	for in, want := range cases {
		if got := detectPlatform(in); got != want {
			t.Errorf("detectPlatform(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestProbeForbiddenNodesGapsArchitectures(t *testing.T) {
	f := newFakeProber(t, nil)
	f.dyn.PrependReactor("list", "nodes", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, "", nil)
	})
	prof, err := f.probe(context.Background())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if prof.Kubernetes == nil || prof.Kubernetes.Version == "" {
		t.Fatalf("kubernetes = %+v", prof.Kubernetes)
	}
	if !hasGap(prof.Incomplete, "kubernetes.nodeArchitectures") {
		t.Fatalf("expected a gap on kubernetes.nodeArchitectures, got %+v", prof.Incomplete)
	}
}

func hasGap(gaps []clusterprofile.Gap, field string) bool {
	for _, g := range gaps {
		if g.Field == field {
			return true
		}
	}
	return false
}
