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
		{Group: "apps", Version: "v1", Kind: "Deployment"},
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
		resourceList("postgresql.cnpg.io/v1", "clusters", "clusters/status", "databases", "poolers"),
		resourceList("source.toolkit.fluxcd.io/v1"),
		resourceList("fluxcd.controlplane.io/v1"),
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
	f.seed(t, deploymentGVR, cnpgOperatorDeployment("cnpg-system", "ghcr.io/cloudnative-pg/cloudnative-pg:1.26.0", nil))

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
	// issue #91: snapshot support and what a snapshot costs are two answers.
	if sc := prof.StorageClasses[0]; sc.SnapshotDriver != "pd.csi.storage.gke.io" ||
		sc.CloneCapability != clusterprofile.CloneFullCopy ||
		sc.CloneConfidence != clusterprofile.CloneConfidenceKnownDriver {
		t.Fatalf("storage class clone capability = %+v, want full-copy from a known driver", sc)
	}
	if prof.CloudNativePG == nil || prof.Flux == nil || prof.ArgoCD == nil || prof.MetricsServer == nil {
		t.Fatalf("expected cnpg/flux/argocd/metrics present, got %+v", prof)
	}
	// issue #90: the version and the served CRDs are the two facts the postgres
	// judgement needs, and subresources are not CRDs a manifest targets.
	if cnpg := prof.CloudNativePG; cnpg.Version != "1.26.0" || cnpg.Namespace != "cnpg-system" ||
		!reflect.DeepEqual(cnpg.CRDs, []string{"clusters", "databases", "poolers"}) {
		t.Fatalf("cnpg = %+v, want 1.26.0 in cnpg-system serving clusters/databases/poolers", cnpg)
	}
	if prof.FluxOperator == nil {
		t.Fatalf("expected flux-operator present, got %+v", prof.FluxOperator)
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

// TestProbeFluxWithoutOperator is the distinction issue #157 exists for: the
// majority of Flux installs have no flux-operator, and a probe that saw the
// Flux source group must not imply the operator's FluxReport is there to read.
func TestProbeFluxWithoutOperator(t *testing.T) {
	f := newFakeProber(t, []*metav1.APIResourceList{resourceList("source.toolkit.fluxcd.io/v1")})
	prof, err := f.probe(context.Background())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if prof.Flux == nil {
		t.Fatal("Flux must be present when its source group is registered")
	}
	if prof.FluxOperator != nil {
		t.Fatalf("flux-operator must stay absent without its own group: %+v", prof.FluxOperator)
	}
	if len(prof.Incomplete) != 0 {
		t.Fatalf("a readable cluster without flux-operator is a finding, not a gap: %+v", prof.Incomplete)
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
	// And the clone capability must be unknown, not none: local-path really has
	// no snapshot driver, but we could not read the snapshot classes, so
	// reporting "none" here would be right by luck and wrong by method (#91).
	if sc := prof.StorageClasses[0]; sc.CloneCapability != clusterprofile.CloneUnknown ||
		sc.CloneConfidence != clusterprofile.CloneConfidenceUnreadable {
		t.Fatalf("clone capability = %+v, want unknown/unreadable behind a gap", sc)
	}
}

// TestProbeStorageCloneCapabilities is issue #91's detection acceptance: three
// storage classes, three different answers, and the unrecognised driver gets an
// unknown rather than a guess in either direction.
func TestProbeStorageCloneCapabilities(t *testing.T) {
	f := newFakeProber(t, []*metav1.APIResourceList{
		resourceList("storage.k8s.io/v1"), resourceList("snapshot.storage.k8s.io/v1"),
	})
	storageClass := func(name, provisioner string, isDefault bool) *unstructured.Unstructured {
		body := map[string]any{"provisioner": provisioner}
		if isDefault {
			body["metadata"] = map[string]any{"annotations": map[string]any{"storageclass.kubernetes.io/is-default-class": "true"}}
		}
		return unstruct(schema.GroupVersionKind{Group: "storage.k8s.io", Version: "v1", Kind: "StorageClass"}, name, body)
	}
	snapshotClass := func(name, driver string) *unstructured.Unstructured {
		return unstruct(schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotClass"},
			name, map[string]any{"driver": driver})
	}
	f.seed(t, storageClassGVR, storageClass("local-path", "rancher.io/local-path", true))
	f.seed(t, storageClassGVR, storageClass("ceph-rbd", "rbd.csi.ceph.com", false))
	f.seed(t, storageClassGVR, storageClass("mystery", "storage.example.com", false))
	f.seed(t, volumeSnapshotClassGVR, snapshotClass("csi-rbd-snap", "rbd.csi.ceph.com"))
	f.seed(t, volumeSnapshotClassGVR, snapshotClass("mystery-snap", "storage.example.com"))

	prof, err := f.probe(context.Background())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	byName := map[string]clusterprofile.StorageClass{}
	for _, sc := range prof.StorageClasses {
		byName[sc.Name] = sc
	}
	want := map[string]struct {
		capability clusterprofile.CloneCapability
		confidence clusterprofile.CloneConfidence
		snapshot   string
	}{
		"local-path": {clusterprofile.CloneNone, clusterprofile.CloneConfidenceKnownDriver, ""},
		"ceph-rbd":   {clusterprofile.CloneThin, clusterprofile.CloneConfidenceKnownDriver, "csi-rbd-snap"},
		"mystery":    {clusterprofile.CloneUnknown, clusterprofile.CloneConfidenceUnknownDriver, "mystery-snap"},
	}
	for name, w := range want {
		got, ok := byName[name]
		if !ok {
			t.Fatalf("storage class %q missing from %+v", name, prof.StorageClasses)
		}
		if got.CloneCapability != w.capability || got.CloneConfidence != w.confidence || got.VolumeSnapshotClass != w.snapshot {
			t.Errorf("%s = %+v, want %s/%s via %q", name, got, w.capability, w.confidence, w.snapshot)
		}
	}
	if byName["ceph-rbd"].SnapshotDriver != "rbd.csi.ceph.com" {
		t.Errorf("snapshot driver = %q, want the CSI driver that decides the cost", byName["ceph-rbd"].SnapshotDriver)
	}
	if byName["local-path"].SnapshotDriver != "" {
		t.Errorf("a class with no snapshot class must report no snapshot driver, got %q", byName["local-path"].SnapshotDriver)
	}
}

// --- CloudNativePG: version and served CRDs (issue #90) -----------------------

// cnpgOperatorDeployment builds an operator Deployment shaped like both
// supported installs: the upstream manifest and the Helm chart label it
// app.kubernetes.io/name=cloudnative-pg, and the chart additionally carries an
// app.kubernetes.io/version label.
func cnpgOperatorDeployment(namespace, image string, extraLabels map[string]string) *unstructured.Unstructured {
	labels := map[string]any{"app.kubernetes.io/name": "cloudnative-pg"}
	for k, v := range extraLabels {
		labels[k] = v
	}
	d := unstruct(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}, "cnpg-controller-manager", map[string]any{
		"metadata": map[string]any{"namespace": namespace, "labels": labels},
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"containers": []any{
				map[string]any{"name": "manager", "image": image},
			},
		}}},
	})
	d.SetNamespace(namespace)
	return d
}

func cnpgProber(t *testing.T) *fakeProber {
	t.Helper()
	return newFakeProber(t, []*metav1.APIResourceList{
		resourceList("postgresql.cnpg.io/v1", "clusters", "databases", "poolers"),
	})
}

// TestProbeCNPGVersionFromImageTag: the operator's running version is the image
// tag, which is the one source present in both install methods.
func TestProbeCNPGVersionFromImageTag(t *testing.T) {
	f := cnpgProber(t)
	f.seed(t, deploymentGVR, cnpgOperatorDeployment("cnpg-system", "ghcr.io/cloudnative-pg/cloudnative-pg:1.30.1", nil))

	prof, err := f.probe(context.Background())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if prof.CloudNativePG == nil || prof.CloudNativePG.Version != "1.30.1" {
		t.Fatalf("cnpg = %+v, want version 1.30.1", prof.CloudNativePG)
	}
	if len(prof.Incomplete) != 0 {
		t.Fatalf("a fully readable CNPG install is a finding, not a gap: %+v", prof.Incomplete)
	}
}

// TestProbeCNPGVersionFromChartLabel: a digest-pinned image has no readable
// tag, so the Helm chart's version label is the fallback — and a guess is never
// the fallback.
func TestProbeCNPGVersionFromChartLabel(t *testing.T) {
	f := cnpgProber(t)
	f.seed(t, deploymentGVR, cnpgOperatorDeployment("postgres-operator",
		"ghcr.io/cloudnative-pg/cloudnative-pg@sha256:0000000000000000000000000000000000000000000000000000000000000000",
		map[string]string{"app.kubernetes.io/version": "1.29.2"}))

	prof, err := f.probe(context.Background())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if prof.CloudNativePG.Version != "1.29.2" || prof.CloudNativePG.Namespace != "postgres-operator" {
		t.Fatalf("cnpg = %+v, want 1.29.2 in postgres-operator", prof.CloudNativePG)
	}
}

// TestProbeCNPGUnrecognisedInstallKeepsPresence: no Deployment matches the
// operator label (an install shape this probe does not recognise). CNPG stays
// present with an empty version — "installed, version unknown" — and that is a
// finding about the deployment, not a gap in permissions.
func TestProbeCNPGUnrecognisedInstallKeepsPresence(t *testing.T) {
	f := cnpgProber(t)
	prof, err := f.probe(context.Background())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if prof.CloudNativePG == nil {
		t.Fatal("CNPG must stay present when its API group is registered")
	}
	if prof.CloudNativePG.Version != "" {
		t.Fatalf("version = %q, want empty rather than a guess", prof.CloudNativePG.Version)
	}
	if !reflect.DeepEqual(prof.CloudNativePG.CRDs, []string{"clusters", "databases", "poolers"}) {
		t.Fatalf("crds = %v", prof.CloudNativePG.CRDs)
	}
	if len(prof.Incomplete) != 0 {
		t.Fatalf("an unlabeled operator is not a permission gap: %+v", prof.Incomplete)
	}
}

// TestProbeForbiddenDeploymentsGapsCNPGVersion is the contract that matters
// most for CNPG: an unreadable version must never demote the operator to
// absent, because absent is the one answer that would tell a caller to install
// a second operator into a cluster that can only have one (ADR-0005).
func TestProbeForbiddenDeploymentsGapsCNPGVersion(t *testing.T) {
	f := cnpgProber(t)
	f.dyn.PrependReactor("list", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "deployments"}, "", nil)
	})

	prof, err := f.probe(context.Background())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if prof.CloudNativePG == nil {
		t.Fatal("CNPG must stay present when only its version is forbidden")
	}
	if prof.CloudNativePG.Version != "" {
		t.Fatalf("version = %q, want empty", prof.CloudNativePG.Version)
	}
	if !hasGap(prof.Incomplete, "cnpg.version") {
		t.Fatalf("expected a gap on cnpg.version, got %+v", prof.Incomplete)
	}
}

// TestProbeCNPGWithoutDatabaseCRD: an operator too old for the Database CRD
// serves clusters and poolers only, and the profile records exactly that — the
// served set is what a Database manifest would meet.
func TestProbeCNPGWithoutDatabaseCRD(t *testing.T) {
	f := newFakeProber(t, []*metav1.APIResourceList{
		resourceList("postgresql.cnpg.io/v1", "clusters", "poolers"),
	})
	f.seed(t, deploymentGVR, cnpgOperatorDeployment("cnpg-system", "ghcr.io/cloudnative-pg/cloudnative-pg:1.24.2", nil))

	prof, err := f.probe(context.Background())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if prof.CloudNativePG.ServesCRD("databases") {
		t.Fatalf("crds = %v, must not claim databases", prof.CloudNativePG.CRDs)
	}
	if !prof.CloudNativePG.ServesCRD("clusters") {
		t.Fatalf("crds = %v, want clusters", prof.CloudNativePG.CRDs)
	}
}

// --- The Valkey operator: version and served resources (issue #98) -----------

// valkeyOperatorDeployment builds a controller Deployment shaped like both
// install methods: the kustomize manifests and the project's Helm chart label
// it app.kubernetes.io/name=valkey-operator.
func valkeyOperatorDeployment(namespace, image string, extraLabels map[string]string) *unstructured.Unstructured {
	labels := map[string]any{"app.kubernetes.io/name": "valkey-operator"}
	for k, v := range extraLabels {
		labels[k] = v
	}
	d := unstruct(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}, "valkey-operator-controller-manager", map[string]any{
		"metadata": map[string]any{"namespace": namespace, "labels": labels},
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"containers": []any{
				map[string]any{"name": "manager", "image": image},
			},
		}}},
	})
	d.SetNamespace(namespace)
	return d
}

func valkeyProber(t *testing.T) *fakeProber {
	t.Helper()
	return newFakeProber(t, []*metav1.APIResourceList{
		resourceList("valkey.io/v1alpha1", "valkeyclusters", "valkeyclusters/status", "valkeynodes"),
	})
}

// TestProbeValkeyOperator: the operator is a finding of its own, with the same
// three facts CNPG records — version from the image tag, namespace, and the
// resources the API server actually serves. Subresources are not CRDs a
// manifest targets and must not appear in the served set.
func TestProbeValkeyOperator(t *testing.T) {
	f := valkeyProber(t)
	f.seed(t, deploymentGVR, valkeyOperatorDeployment("valkey-operator-system", "ghcr.io/valkey-io/valkey-operator:0.5.0", nil))

	prof, err := f.probe(context.Background())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if prof.Valkey == nil {
		t.Fatal("the valkey.io group is registered, so the operator is present")
	}
	if prof.Valkey.Version != "0.5.0" || prof.Valkey.Namespace != "valkey-operator-system" {
		t.Fatalf("valkey = %+v, want 0.5.0 in valkey-operator-system", prof.Valkey)
	}
	if !reflect.DeepEqual(prof.Valkey.CRDs, []string{"valkeyclusters", "valkeynodes"}) {
		t.Fatalf("crds = %v, want valkeyclusters and valkeynodes and no subresource", prof.Valkey.CRDs)
	}
	if len(prof.Incomplete) != 0 {
		t.Fatalf("a fully readable install is a finding, not a gap: %+v", prof.Incomplete)
	}
}

// TestProbeValkeyVersionFromChartLabel: a digest-pinned image has no readable
// tag, so the chart's version label is the fallback — and a guess is never the
// fallback.
func TestProbeValkeyVersionFromChartLabel(t *testing.T) {
	f := valkeyProber(t)
	f.seed(t, deploymentGVR, valkeyOperatorDeployment("valkey-system",
		"ghcr.io/valkey-io/valkey-operator@sha256:abc",
		map[string]string{"app.kubernetes.io/version": "0.5.0"}))

	prof, err := f.probe(context.Background())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if prof.Valkey.Version != "0.5.0" || prof.Valkey.Namespace != "valkey-system" {
		t.Fatalf("valkey = %+v, want 0.5.0 in valkey-system", prof.Valkey)
	}
}

// TestProbeValkeyWithoutNodeCRD: a partially applied CRD set serves
// valkeyclusters and nothing else, and the profile records exactly that — the
// served set is what a manifest would meet, and what the judgement refuses on.
func TestProbeValkeyWithoutNodeCRD(t *testing.T) {
	f := newFakeProber(t, []*metav1.APIResourceList{
		resourceList("valkey.io/v1alpha1", "valkeyclusters"),
	})
	f.seed(t, deploymentGVR, valkeyOperatorDeployment("valkey-operator-system", "ghcr.io/valkey-io/valkey-operator:0.5.0", nil))

	prof, err := f.probe(context.Background())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if prof.Valkey.ServesCRD("valkeynodes") {
		t.Fatalf("crds = %v, must not claim valkeynodes", prof.Valkey.CRDs)
	}
	if !prof.Valkey.ServesCRD("valkeyclusters") {
		t.Fatalf("crds = %v, want valkeyclusters", prof.Valkey.CRDs)
	}
}

// TestProbeValkeyAbsentIsAbsent: no registered group means no operator, and
// that is a finding rather than a gap — the whole shape contract of detection.
func TestProbeValkeyAbsentIsAbsent(t *testing.T) {
	f := newFakeProber(t, nil)
	prof, err := f.probe(context.Background())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if prof.Valkey != nil {
		t.Fatalf("valkey = %+v, want absent", prof.Valkey)
	}
	if hasGap(prof.Incomplete, "valkey") {
		t.Fatalf("a successful look at an operator-free cluster is not a gap: %+v", prof.Incomplete)
	}
}

// --- helm-controller: the prerequisite for a chart component (ADR-0016) ------

// helmControllerDeploy builds the controller Deployment as Flux labels it.
// The selector is app.kubernetes.io/component rather than .../name, because the
// Flux manifests set `name` to `flux` for the whole suite and `component` to
// the individual controller — the label that tells helm-controller apart from
// source-controller is the one detection has to match.
func helmControllerDeploy(namespace, image string, extraLabels map[string]string) *unstructured.Unstructured {
	labels := map[string]any{
		"app.kubernetes.io/component": "helm-controller",
		"app.kubernetes.io/part-of":   "flux",
	}
	for k, v := range extraLabels {
		labels[k] = v
	}
	d := unstruct(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}, "helm-controller", map[string]any{
		"metadata": map[string]any{"namespace": namespace, "labels": labels},
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"containers": []any{
				map[string]any{"name": "manager", "image": image},
			},
		}}},
	})
	d.SetNamespace(namespace)
	return d
}

// TestProbeHelmController: the controller is a finding of its own, with the
// same three facts every adopted operator records.
func TestProbeHelmController(t *testing.T) {
	f := newFakeProber(t, []*metav1.APIResourceList{
		resourceList("helm.toolkit.fluxcd.io/v2", "helmreleases", "helmreleases/status"),
	})
	f.seed(t, deploymentGVR, helmControllerDeploy("flux-system", "ghcr.io/fluxcd/helm-controller:v1.3.0", nil))

	prof, err := f.probe(context.Background())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if prof.HelmController == nil {
		t.Fatal("the helm.toolkit.fluxcd.io group is registered, so helm-controller is present")
	}
	if prof.HelmController.Version != "v1.3.0" || prof.HelmController.Namespace != "flux-system" {
		t.Fatalf("helmController = %+v, want v1.3.0 in flux-system", prof.HelmController)
	}
	if !reflect.DeepEqual(prof.HelmController.CRDs, []string{"helmreleases"}) {
		t.Fatalf("crds = %v, want helmreleases and no subresource", prof.HelmController.CRDs)
	}
	if len(prof.Incomplete) != 0 {
		t.Fatalf("a fully readable install is a finding, not a gap: %+v", prof.Incomplete)
	}
}

// TestProbeHelmControllerIsNotFlux: source-controller and helm-controller are
// separate findings, because a FluxInstance may install one without the other
// (issue #60). A cluster with Flux's source group and no helm group must not
// report a controller it does not run.
func TestProbeHelmControllerIsNotFlux(t *testing.T) {
	f := newFakeProber(t, []*metav1.APIResourceList{
		resourceList("source.toolkit.fluxcd.io/v1", "gitrepositories", "helmrepositories", "ocirepositories"),
	})
	prof, err := f.probe(context.Background())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if prof.Flux == nil {
		t.Fatal("the source.toolkit.fluxcd.io group is registered, so Flux is present")
	}
	if prof.HelmController != nil {
		t.Fatalf("helmController = %+v, want absent — nothing serves helm.toolkit.fluxcd.io", prof.HelmController)
	}
	if hasGap(prof.Incomplete, "helmController") {
		t.Fatalf("a successful look at a cluster without helm-controller is not a gap: %+v", prof.Incomplete)
	}
}

func TestImageTag(t *testing.T) {
	cases := map[string]string{
		"ghcr.io/cloudnative-pg/cloudnative-pg:1.26.0":       "1.26.0",
		"registry.local:5000/cloudnative-pg/cloudnative-pg":  "",
		"ghcr.io/cloudnative-pg/cloudnative-pg:1.26.0@sha25": "1.26.0",
		"ghcr.io/cloudnative-pg/cloudnative-pg@sha256:abc":   "",
		"cloudnative-pg": "",
	}
	for in, want := range cases {
		if got := imageTag(in); got != want {
			t.Errorf("imageTag(%q) = %q, want %q", in, got, want)
		}
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
	for _, want := range []string{"gatewayAPI", "certManager", "externalSecrets", "cnpg", "valkey", "flux", "helmController", "fluxOperator", "argocd", "metricsServer", "prometheus", "policyEngines"} {
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
