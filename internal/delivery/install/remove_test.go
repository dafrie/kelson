package install

import (
	"context"
	"errors"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"
)

func planRemoval(t *testing.T, r *Remover, name string) *Removal {
	t.Helper()
	removal, err := r.Plan(context.Background(), name)
	if err != nil {
		t.Fatalf("plan removal: %v", err)
	}
	return removal
}

// TestRemovalTakesOnlyWhatKelsonCreated is the acceptance criterion of issue
// #60, stated as a test: an adopted object and an unlabelled object both stay.
func TestRemovalTakesOnlyWhatKelsonCreated(t *testing.T) {
	cl := newCluster()
	cl.seed(t, object("v1", "Namespace", "cert-manager", "", installed("cert-manager", "v1.21.1", OwnershipAdopted)))
	cl.seed(t, object("apps/v1", "Deployment", "cert-manager", "cert-manager",
		installed("cert-manager", "v1.21.1", OwnershipCreated)))
	cl.seed(t, object("v1", "ServiceAccount", "cert-manager", "cert-manager",
		installed("cert-manager", "v1.21.1", OwnershipCreated)))
	// A bystander: the same namespace holds something nobody labelled.
	cl.seed(t, object("v1", "ConfigMap", "operator-notes", "cert-manager"))
	// And something labelled by an install that predates the annotation.
	cl.seed(t, object("rbac.authorization.k8s.io/v1", "ClusterRole", "cert-manager-legacy", "",
		installed("cert-manager", "v1.20.0", "")))

	r := newRemover(t, cl, testCatalog{})
	removal := planRemoval(t, r, "cert-manager")

	got := refsOfTargets(removal.Targets)
	if len(got) != 2 {
		t.Fatalf("targets = %v, want only the two objects kelson created", got)
	}
	if !contains(got, "Deployment/cert-manager/cert-manager") || !contains(got, "ServiceAccount/cert-manager/cert-manager") {
		t.Fatalf("targets = %v", got)
	}
	if len(removal.Kept) != 2 {
		t.Fatalf("kept = %+v, want the adopted namespace and the unannotated ClusterRole", removal.Kept)
	}

	report, err := r.Execute(context.Background(), removal)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if report.Deleted != 2 {
		t.Fatalf("deleted = %d, want 2", report.Deleted)
	}
	if ns := cl.get(t, "Namespace", "", "cert-manager"); ns == nil {
		t.Fatal("the adopted namespace was deleted")
	}
	if cm := cl.get(t, "ConfigMap", "cert-manager", "operator-notes"); cm == nil {
		t.Fatal("an unlabelled bystander was deleted")
	}
	if cr := cl.get(t, "ClusterRole", "", "cert-manager-legacy"); cr == nil {
		t.Fatal("a labelled object with no ownership annotation was deleted")
	}
}

// TestRemovalDeletesCreatedNamespaceLast, and everything in dependency order.
func TestRemovalOrder(t *testing.T) {
	cl := newCluster()
	own := installed("flux", "v0.58.0", OwnershipCreated)
	cl.seed(t, object("v1", "Namespace", "flux-system", "", own))
	cl.seed(t, object("apps/v1", "Deployment", "flux-operator", "flux-system", own))
	cl.seed(t, object("v1", "ServiceAccount", "flux-operator", "flux-system", own))
	cl.seed(t, crdObject("fluxinstances.fluxcd.controlplane.io", "fluxcd.controlplane.io", "fluxinstances",
		"FluxInstance", own))
	cl.seed(t, object("fluxcd.controlplane.io/v1", "FluxInstance", "flux", "flux-system", own))

	r := newRemover(t, cl, testCatalog{})
	removal := planRemoval(t, r, "flux")

	want := []string{
		"FluxInstance/flux-system/flux",
		"Deployment/flux-system/flux-operator",
		"ServiceAccount/flux-system/flux-operator",
		"CustomResourceDefinition/fluxinstances.fluxcd.controlplane.io",
		"Namespace/flux-system",
	}
	got := refsOfTargets(removal.Targets)
	if len(got) != len(want) {
		t.Fatalf("targets = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("removal order:\n got %v\nwant %v", got, want)
		}
	}
}

// TestRemovalNamesTheCustomResourcesACRDTakesWithIt: deleting CloudNativePG's
// CRD deletes every database in the cluster, and a preview that did not say so
// would be technically complete and practically a trap.
func TestRemovalNamesCRDCollateral(t *testing.T) {
	cl := newCluster()
	own := installed("cnpg", "v1.30.0", OwnershipCreated)
	cl.seed(t, crdObject("clusters.postgresql.cnpg.io", "postgresql.cnpg.io", "clusters", "Cluster", own))
	cl.seed(t, object("postgresql.cnpg.io/v1", "Cluster", "checkout-db", "checkout-production"))
	cl.seed(t, object("postgresql.cnpg.io/v1", "Cluster", "billing-db", "billing-production"))

	r := newRemover(t, cl, testCatalog{})
	removal := planRemoval(t, r, "cnpg")

	crds := removal.Tier(TierCRD)
	if len(crds) != 1 {
		t.Fatalf("CRD targets = %+v, want one", crds)
	}
	want := []string{"Cluster/billing-production/billing-db", "Cluster/checkout-production/checkout-db"}
	got := crds[0].Collateral
	if len(got) != len(want) {
		t.Fatalf("collateral = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("collateral = %v, want %v", got, want)
		}
	}
}

// TestRemovalRechecksLiveLabelsBeforeDeleting: a plan is a snapshot and the
// cluster is not.
func TestRemovalRechecksLiveState(t *testing.T) {
	cl := newCluster()
	cl.seed(t, object("apps/v1", "Deployment", "cert-manager", "cert-manager",
		installed("cert-manager", "v1.21.1", OwnershipCreated)))

	r := newRemover(t, cl, testCatalog{})
	removal := planRemoval(t, r, "cert-manager")

	// Somebody re-stamps the object between the preview and the delete.
	live := cl.get(t, "Deployment", "cert-manager", "cert-manager")
	ann := live.GetAnnotations()
	ann[AnnOwnership] = OwnershipAdopted
	live.SetAnnotations(ann)
	if err := cl.dyn.Tracker().Update(gvrForKind(t, "Deployment"), live, "cert-manager"); err != nil {
		t.Fatalf("update: %v", err)
	}

	report, err := r.Execute(context.Background(), removal)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if report.Left != 1 || report.Deleted != 0 {
		t.Fatalf("report = %+v, want the target left alone", report)
	}
	if !strings.Contains(report.Results[0].Detail, "created") {
		t.Fatalf("detail %q does not explain the refusal", report.Results[0].Detail)
	}
	if got := cl.deleteLog(); len(got) != 0 {
		t.Fatalf("deleted %v after the ownership record changed", got)
	}
}

// TestRemovalIsIdempotent: a second run reports everything gone and succeeds.
func TestRemovalIsIdempotent(t *testing.T) {
	cl := newCluster()
	cl.seed(t, object("apps/v1", "Deployment", "cert-manager", "cert-manager",
		installed("cert-manager", "v1.21.1", OwnershipCreated)))
	r := newRemover(t, cl, testCatalog{})

	removal := planRemoval(t, r, "cert-manager")
	if _, err := r.Execute(context.Background(), removal); err != nil {
		t.Fatalf("execute: %v", err)
	}
	second := planRemoval(t, r, "cert-manager")
	if !second.Empty() {
		t.Fatalf("second plan = %v, want nothing left", refsOfTargets(second.Targets))
	}
	report, err := r.Execute(context.Background(), removal)
	if err != nil {
		t.Fatalf("re-execute: %v", err)
	}
	if report.Gone != 1 {
		t.Fatalf("report = %+v, want everything already gone", report)
	}
}

// TestRemovalReportsAnIncompleteSweep: a kind that cannot be listed is named,
// never swallowed.
func TestRemovalReportsAnIncompleteSweep(t *testing.T) {
	cl := newCluster()
	cl.dyn.PrependReactor("list", "clusterroles", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "clusterroles"}, "",
			errors.New("not permitted"))
	})
	r := newRemover(t, cl, testCatalog{gaps: []string{"metrics.k8s.io/v1beta1"}})
	removal := planRemoval(t, r, "cert-manager")

	if len(removal.Unreadable) != 2 {
		t.Fatalf("unreadable = %v, want the forbidden kind and the discovery gap", removal.Unreadable)
	}
	joined := strings.Join(removal.Unreadable, "\n")
	if !strings.Contains(joined, "clusterroles") || !strings.Contains(joined, "metrics.k8s.io") {
		t.Fatalf("unreadable = %v", removal.Unreadable)
	}
}

// TestRemovalRejectsUnknownComponent.
func TestRemovalRejectsUnknownComponent(t *testing.T) {
	r := newRemover(t, newCluster(), testCatalog{})
	if _, err := r.Plan(context.Background(), "nginx"); err == nil {
		t.Fatal("planned a removal of a component kelson does not track")
	}
}

// TestSelectorIsTheWholeHandle: what kelson deletes is a kubectl query.
func TestSelector(t *testing.T) {
	if got, want := Selector("cnpg"), "kelson.dev/installed-component=cnpg"; got != want {
		t.Fatalf("Selector() = %q, want %q", got, want)
	}
}

// crdObject builds a CustomResourceDefinition the collateral lookup can read.
func crdObject(name, group, plural, kind string, mutate ...func(*unstructured.Unstructured)) *unstructured.Unstructured {
	obj := object("apiextensions.k8s.io/v1", "CustomResourceDefinition", name, "", mutate...)
	_ = unstructured.SetNestedMap(obj.Object, map[string]any{
		"group": group,
		"names": map[string]any{"kind": kind, "plural": plural},
		"versions": []any{
			map[string]any{"name": "v1", "served": true, "storage": true},
		},
	}, "spec")
	return obj
}
