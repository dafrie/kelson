package controlstore

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dafrie/kelson/api/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/model"
)

// fluxLabels is what kustomize-controller stamps on an object it applied.
func fluxLabels(name, namespace string) map[string]string {
	return map[string]string{
		labelManagedBy:         managedByKelson,
		labelProject:           testProject,
		labelState:             stateSpec,
		labelFluxKustomization: name,
		labelFluxNamespace:     namespace,
	}
}

func managedProject(labels map[string]string) *v1alpha1.Project {
	p := &v1alpha1.Project{Spec: model.ProjectSpec{Image: "ghcr.io/acme/shop:v1"}}
	p.APIVersion, p.Kind = model.APIVersion, v1alpha1.KindProject
	p.ObjectMeta = metav1.ObjectMeta{Name: testProject, Namespace: testNamespace, Labels: labels}
	return p
}

func managedEnvironment(name string, labels map[string]string) *v1alpha1.Environment {
	e := &v1alpha1.Environment{Spec: model.EnvironmentSpec{Project: testProject}}
	e.APIVersion, e.Kind = model.APIVersion, v1alpha1.KindEnvironment
	e.ObjectMeta = metav1.ObjectMeta{Name: name, Namespace: testNamespace, Labels: labels}
	return e
}

// TestGetReportsTheKustomizationThatOwnsEachDocument is the whole of #248's
// detection half: the evidence is a label, it is read per document, and a
// project whose documents are owned by different things says so.
func TestGetReportsTheKustomizationThatOwnsEachDocument(t *testing.T) {
	c := newCRClient(t,
		managedProject(fluxLabels("apps", "flux-system")),
		managedEnvironment("production", fluxLabels("apps", "flux-system")),
		managedEnvironment("development", specLabels(testProject)),
	)
	store := newSpecStore(t, c)

	stored, err := store.Get(context.Background(), testProject)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	want := []GitOpsOwner{
		{Document: DocumentProject, Kustomization: "apps", Namespace: "flux-system"},
		{Document: "production", Kustomization: "apps", Namespace: "flux-system"},
	}
	if len(stored.GitOps) != len(want) {
		t.Fatalf("GitOps = %+v, want %+v", stored.GitOps, want)
	}
	for i := range want {
		if stored.GitOps[i] != want[i] {
			t.Errorf("GitOps[%d] = %+v, want %+v", i, stored.GitOps[i], want[i])
		}
	}
	// `development` carries kelson's own labels and no Flux ones, so it is
	// absent — which is the point of reporting per document rather than per
	// project: editing it through kelson is safe and must stay offered.
	for _, owner := range stored.GitOps {
		if owner.Document == "development" {
			t.Error("an environment kelson wrote itself was reported as GitOps-managed")
		}
	}
}

func TestGetReportsNoOwnerForAnOrdinaryProject(t *testing.T) {
	ctx := context.Background()
	c := newCRClient(t)
	store := newSpecStore(t, c)
	if _, err := store.Put(ctx, testProject, testDocuments(), PutOptions{}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	stored, err := store.Get(ctx, testProject)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(stored.GitOps) != 0 {
		t.Errorf("GitOps = %+v, want empty: kelson's own store is the only writer here", stored.GitOps)
	}
}

// TestListReportsOwnershipWithoutDocuments pins the reason the field lives on
// Stored rather than beside Documents: ListSpecs omits the bytes and a listing
// still has to be able to say which projects kelson does not own.
func TestListReportsOwnershipWithoutDocuments(t *testing.T) {
	c := newCRClient(t,
		managedProject(fluxLabels("apps", "flux-system")),
		managedEnvironment("production", fluxLabels("apps", "flux-system")),
	)
	store := newSpecStore(t, c)

	all, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("List returned %d projects, want one", len(all))
	}
	if len(all[0].GitOps) != 2 {
		t.Errorf("GitOps = %+v, want the project and its production environment", all[0].GitOps)
	}
}

// TestOwnershipSurvivesAKelsonWrite is the awkward state the banner exists for:
// a document git owns that kelson has also written. Server-side apply leaves
// another manager's labels alone, so the evidence is still there afterwards and
// the next reader is still warned.
func TestOwnershipSurvivesAKelsonWrite(t *testing.T) {
	ctx := context.Background()
	c := newCRClient(t, managedProject(fluxLabels("apps", "flux-system")))
	store := newSpecStore(t, c)

	var before v1alpha1.Project
	if err := c.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: testProject}, &before); err != nil {
		t.Fatalf("read the seeded Project: %v", err)
	}
	if _, err := store.Put(ctx, testProject, testDocuments(), PutOptions{ExpectedVersion: before.ResourceVersion}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	stored, err := store.Get(ctx, testProject)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(stored.GitOps) == 0 {
		t.Fatal("the Flux ownership labels did not survive a kelson write; the warning would disappear the " +
			"first time somebody overrode it, which is exactly when it is most needed")
	}
	if stored.GitOps[0].Kustomization != "apps" {
		t.Errorf("GitOps[0] = %+v, want the Kustomization that applied the document", stored.GitOps[0])
	}
}

func TestGitOpsOwnerReadsHalfALabelSet(t *testing.T) {
	owner, ok := gitOpsOwner(DocumentProject, map[string]string{labelFluxKustomization: "apps"})
	if !ok {
		t.Fatal("a name with no namespace was read as unmanaged; half an answer is still true")
	}
	if owner.Namespace != "" {
		t.Errorf("namespace = %q, want empty", owner.Namespace)
	}
	if _, ok := gitOpsOwner(DocumentProject, map[string]string{labelFluxNamespace: "flux-system"}); ok {
		t.Error("a namespace with no name identifies no Kustomization and must not be reported as one")
	}
	if _, ok := gitOpsOwner(DocumentProject, nil); ok {
		t.Error("an object with no labels was reported as managed")
	}
}
