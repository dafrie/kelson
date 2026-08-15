package controlstore

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/dafrie/kelson/api/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/model"
)

// The store's backend is now custom resources, so these tests are written
// against controller-runtime's fake client rather than the typed ConfigMap
// fixture the other stores in this package use — the same client the controller
// package's own tests are written against, and the only in-tree fake that
// implements server-side apply with a resourceVersion precondition (which is
// the whole of the store's concurrency contract).
//
// What they can no longer assert is byte fidelity: a Put followed by a Get
// returns an *equivalent* document, not the same bytes (ADR-0027 decision 6).
// So the round-trip assertions are semantic — decode both sides and compare the
// model — which is exactly the guarantee the store now makes.

var (
	projectDoc = []byte(`# the shop
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata:
  name: shop  # trailing comment
spec:
  image: ghcr.io/acme/shop:v1
  components:
    - name: web
      kind: service
      port: 8080
`)
	prodDoc = []byte(`apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata:
  name: production
spec:
  project: shop
  namespace: shop-prod
`)
	stagingDoc = []byte(`apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata:
  name: staging
spec:
  project: shop
`)
)

func testDocuments() Documents {
	return Documents{
		Project: projectDoc,
		Environments: map[string][]byte{
			"production": prodDoc,
			"staging":    stagingDoc,
		},
	}
}

func newCRClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("registering kelson.dev/v1alpha1: %v", err)
	}
	return crfake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

func newSpecStore(t *testing.T, c client.Client) *SpecStore {
	t.Helper()
	store, err := NewSpecStore(SpecStoreOptions{Client: c, Namespace: testNamespace})
	if err != nil {
		t.Fatalf("NewSpecStore: %v", err)
	}
	return store
}

func TestSpecPutWritesCustomResources(t *testing.T) {
	ctx := context.Background()
	c := newCRClient(t)
	store := newSpecStore(t, c)

	put, err := store.Put(ctx, testProject, testDocuments(), PutOptions{})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if put.Version == "" {
		t.Fatal("put returned an empty version; optimistic concurrency has nothing to check")
	}

	// The layout is ADR-0027's, not an implementation detail a test may ignore:
	// one Project and one Environment per environment, in the server's
	// namespace, bound by spec.project, discoverable by kelson's labels.
	var project v1alpha1.Project
	if err := c.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: testProject}, &project); err != nil {
		t.Fatalf("read the stored Project: %v", err)
	}
	for key, want := range map[string]string{
		labelManagedBy: managedByKelson,
		labelProject:   testProject,
		labelState:     stateSpec,
	} {
		if got := project.Labels[key]; got != want {
			t.Errorf("Project label %s = %q, want %q", key, got, want)
		}
	}
	if got := project.Spec.Image; got != "ghcr.io/acme/shop:v1" {
		t.Errorf("Project spec.image = %q, want the authored value", got)
	}

	var envs v1alpha1.EnvironmentList
	if err := c.List(ctx, &envs, client.InNamespace(testNamespace)); err != nil {
		t.Fatalf("list environments: %v", err)
	}
	if len(envs.Items) != 2 {
		t.Fatalf("stored %d Environments, want 2", len(envs.Items))
	}
	for i := range envs.Items {
		if got := envs.Items[i].Spec.Project; got != testProject {
			t.Errorf("Environment %s binds to project %q, want %q", envs.Items[i].Name, got, testProject)
		}
	}
}

// A Put followed by a Get returns an equivalent document, and "equivalent"
// means it decodes to the same model. That is the contract ADR-0027 decision 6
// replaced byte fidelity with, and a UI editor that reads, edits and writes
// back depends on nothing weaker and nothing stronger.
func TestSpecGetRoundTripsSemantically(t *testing.T) {
	ctx := context.Background()
	store := newSpecStore(t, newCRClient(t))
	if _, err := store.Put(ctx, testProject, testDocuments(), PutOptions{}); err != nil {
		t.Fatalf("put: %v", err)
	}

	got, err := store.Get(ctx, testProject)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Project != testProject {
		t.Errorf("project = %q, want %q", got.Project, testProject)
	}
	if want := []string{"production", "staging"}; !equalStrings(got.Environments, want) {
		t.Errorf("environments = %v, want %v", got.Environments, want)
	}

	project := decodeProject(t, got.Documents.Project)
	if project.Metadata.Name != testProject {
		t.Errorf("returned project document names %q", project.Metadata.Name)
	}
	if project.Spec.Image != "ghcr.io/acme/shop:v1" {
		t.Errorf("returned project document lost spec.image: %q", project.Spec.Image)
	}
	if len(project.Spec.Components) != 1 || project.Spec.Components[0].Name != "web" {
		t.Errorf("returned project document lost its components: %+v", project.Spec.Components)
	}

	prod := decodeEnvironment(t, got.Documents.Environments["production"])
	if prod.Metadata.Name != "production" || prod.Spec.Project != testProject || prod.Spec.Namespace != "shop-prod" {
		t.Errorf("returned production document = %+v", prod)
	}

	// And the round trip is stable: writing back what Get returned is a no-op
	// on the stored objects, which is what makes read-edit-write safe.
	if _, err := store.Put(ctx, testProject, got.Documents, PutOptions{ExpectedVersion: got.Version}); err != nil {
		t.Fatalf("writing back what Get returned: %v", err)
	}
	again, err := store.Get(ctx, testProject)
	if err != nil {
		t.Fatalf("second get: %v", err)
	}
	if string(again.Documents.Project) != string(got.Documents.Project) {
		t.Errorf("the project document is not stable across a round trip:\nfirst:\n%s\nsecond:\n%s",
			got.Documents.Project, again.Documents.Project)
	}
}

func TestSpecPutRefusesBlindOverwrite(t *testing.T) {
	ctx := context.Background()
	store := newSpecStore(t, newCRClient(t))
	if _, err := store.Put(ctx, testProject, testDocuments(), PutOptions{}); err != nil {
		t.Fatalf("first put: %v", err)
	}

	_, err := store.Put(ctx, testProject, testDocuments(), PutOptions{})
	if !AsVersionConflict(err) {
		t.Fatalf("second put without a version: err = %v, want store/version-conflict", err)
	}
}

func TestSpecPutRejectsStaleVersion(t *testing.T) {
	ctx := context.Background()
	store := newSpecStore(t, newCRClient(t))
	first, err := store.Put(ctx, testProject, testDocuments(), PutOptions{})
	if err != nil {
		t.Fatalf("first put: %v", err)
	}
	second, err := store.Put(ctx, testProject, testDocuments(), PutOptions{ExpectedVersion: first.Version})
	if err != nil {
		t.Fatalf("second put: %v", err)
	}
	if second.Version == first.Version {
		t.Fatal("the version did not move across a write; a stale write could not be detected")
	}

	_, err = store.Put(ctx, testProject, testDocuments(), PutOptions{ExpectedVersion: first.Version})
	if !AsVersionConflict(err) {
		t.Fatalf("put with a stale version: err = %v, want store/version-conflict", err)
	}
}

func TestSpecPutForceOverridesVersion(t *testing.T) {
	ctx := context.Background()
	store := newSpecStore(t, newCRClient(t))
	if _, err := store.Put(ctx, testProject, testDocuments(), PutOptions{}); err != nil {
		t.Fatalf("first put: %v", err)
	}
	if _, err := store.Put(ctx, testProject, testDocuments(), PutOptions{Force: true}); err != nil {
		t.Fatalf("forced put: %v", err)
	}
}

func TestSpecPutReplaysIdempotencyKey(t *testing.T) {
	ctx := context.Background()
	store := newSpecStore(t, newCRClient(t))
	first, err := store.Put(ctx, testProject, testDocuments(), PutOptions{IdempotencyKey: "abc"})
	if err != nil {
		t.Fatalf("first put: %v", err)
	}

	// The replay carries the version its first attempt was written against,
	// which that attempt has already superseded. It must still be a replay.
	replay, err := store.Put(ctx, testProject, testDocuments(), PutOptions{IdempotencyKey: "abc"})
	if err != nil {
		t.Fatalf("replayed put: %v", err)
	}
	if replay.Version != first.Version {
		t.Errorf("replay wrote: version %q, want the recorded %q", replay.Version, first.Version)
	}

	// A different key is a different request and is version-checked normally.
	if _, err := store.Put(ctx, testProject, testDocuments(), PutOptions{IdempotencyKey: "def"}); !AsVersionConflict(err) {
		t.Fatalf("a new key with no version: err = %v, want store/version-conflict", err)
	}
}

// An environment dropped from the document set is deleted from the cluster. The
// ConfigMap store got this for free; a set of objects has to do it on purpose,
// and getting it wrong would leave an Environment reconciling forever after its
// author removed it.
func TestSpecPutRemovesDroppedEnvironments(t *testing.T) {
	ctx := context.Background()
	c := newCRClient(t)
	store := newSpecStore(t, c)
	first, err := store.Put(ctx, testProject, testDocuments(), PutOptions{})
	if err != nil {
		t.Fatalf("first put: %v", err)
	}

	docs := testDocuments()
	delete(docs.Environments, "staging")
	got, err := store.Put(ctx, testProject, docs, PutOptions{ExpectedVersion: first.Version})
	if err != nil {
		t.Fatalf("second put: %v", err)
	}
	if want := []string{"production"}; !equalStrings(got.Environments, want) {
		t.Errorf("environments = %v, want %v", got.Environments, want)
	}
	var envs v1alpha1.EnvironmentList
	if err := c.List(ctx, &envs, client.InNamespace(testNamespace)); err != nil {
		t.Fatalf("list environments: %v", err)
	}
	if len(envs.Items) != 1 {
		t.Fatalf("%d Environments remain, want 1", len(envs.Items))
	}
}

func TestSpecGetAndDeleteReportNotFound(t *testing.T) {
	ctx := context.Background()
	store := newSpecStore(t, newCRClient(t))
	if _, err := store.Get(ctx, "missing"); !AsNotFound(err) {
		t.Errorf("get of an absent project: err = %v, want store/not-found", err)
	}
	if err := store.Delete(ctx, "missing", DeleteOptions{}); !AsNotFound(err) {
		t.Errorf("delete of an absent project: err = %v, want store/not-found", err)
	}
	// With an idempotency key the absence is the recorded outcome of a
	// completed delete, which is success.
	if err := store.Delete(ctx, "missing", DeleteOptions{IdempotencyKey: "k"}); err != nil {
		t.Errorf("replayed delete of an absent project: %v", err)
	}
}

func TestSpecDeleteRemovesTheWholeProject(t *testing.T) {
	ctx := context.Background()
	c := newCRClient(t)
	store := newSpecStore(t, c)
	put, err := store.Put(ctx, testProject, testDocuments(), PutOptions{})
	if err != nil {
		t.Fatalf("put: %v", err)
	}

	if err := store.Delete(ctx, testProject, DeleteOptions{ExpectedVersion: "stale"}); !AsVersionConflict(err) {
		t.Fatalf("delete with a stale version: err = %v, want store/version-conflict", err)
	}
	if err := store.Delete(ctx, testProject, DeleteOptions{ExpectedVersion: put.Version}); err != nil {
		t.Fatalf("delete: %v", err)
	}

	var projects v1alpha1.ProjectList
	if err := c.List(ctx, &projects, client.InNamespace(testNamespace)); err != nil {
		t.Fatalf("list projects: %v", err)
	}
	if len(projects.Items) != 0 {
		t.Errorf("%d Projects remain after the delete", len(projects.Items))
	}
	var envs v1alpha1.EnvironmentList
	if err := c.List(ctx, &envs, client.InNamespace(testNamespace)); err != nil {
		t.Fatalf("list environments: %v", err)
	}
	if len(envs.Items) != 0 {
		t.Errorf("%d Environments remain after the delete; they would reconcile with no Project", len(envs.Items))
	}
}

func TestSpecDeleteForce(t *testing.T) {
	ctx := context.Background()
	store := newSpecStore(t, newCRClient(t))
	if _, err := store.Put(ctx, testProject, testDocuments(), PutOptions{}); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := store.Delete(ctx, testProject, DeleteOptions{Force: true}); err != nil {
		t.Fatalf("forced delete: %v", err)
	}
}

func TestSpecList(t *testing.T) {
	ctx := context.Background()
	store := newSpecStore(t, newCRClient(t))
	if _, err := store.Put(ctx, testProject, testDocuments(), PutOptions{}); err != nil {
		t.Fatalf("put shop: %v", err)
	}
	// A different environment name on purpose: an Environment's object name is
	// its environment name, so one namespace holds one "production" (see
	// TestSpecPutRefusesAnEnvironmentAnotherProjectOwns).
	atlasProd := strings.ReplaceAll(string(prodDoc), "project: shop", "project: atlas")
	atlasProd = strings.ReplaceAll(atlasProd, "name: production", "name: atlas-production")
	other := Documents{
		Project: []byte(strings.ReplaceAll(string(projectDoc), "name: shop", "name: atlas")),
		Environments: map[string][]byte{
			"atlas-production": []byte(atlasProd),
		},
	}
	if _, err := store.Put(ctx, "atlas", other, PutOptions{}); err != nil {
		t.Fatalf("put atlas: %v", err)
	}

	list, err := store.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("listed %d projects, want 2", len(list))
	}
	if list[0].Project != "atlas" || list[1].Project != "shop" {
		t.Errorf("list is not ordered by name: %q, %q", list[0].Project, list[1].Project)
	}
	// The listing must not mix one project's environments into another's: they
	// share a namespace and are told apart by spec.project alone.
	if want := []string{"atlas-production"}; !equalStrings(list[0].Environments, want) {
		t.Errorf("atlas environments = %v, want %v", list[0].Environments, want)
	}
	if want := []string{"production", "staging"}; !equalStrings(list[1].Environments, want) {
		t.Errorf("shop environments = %v, want %v", list[1].Environments, want)
	}
}

// One namespace holds one Environment object per name, so a second project
// claiming "production" must be refused rather than silently rebinding the
// first project's environment.
func TestSpecPutRefusesAnEnvironmentAnotherProjectOwns(t *testing.T) {
	ctx := context.Background()
	store := newSpecStore(t, newCRClient(t))
	if _, err := store.Put(ctx, testProject, testDocuments(), PutOptions{}); err != nil {
		t.Fatalf("put shop: %v", err)
	}
	other := Documents{
		Project: []byte(strings.ReplaceAll(string(projectDoc), "name: shop", "name: atlas")),
		Environments: map[string][]byte{
			"production": []byte(strings.ReplaceAll(string(prodDoc), "project: shop", "project: atlas")),
		},
	}
	if _, err := store.Put(ctx, "atlas", other, PutOptions{}); !AsVersionConflict(err) {
		t.Fatalf("put atlas: err = %v, want store/version-conflict", err)
	}
	// And shop still has both of its environments.
	got, err := store.Get(ctx, testProject)
	if err != nil {
		t.Fatalf("get shop: %v", err)
	}
	if want := []string{"production", "staging"}; !equalStrings(got.Environments, want) {
		t.Errorf("shop environments = %v, want %v", got.Environments, want)
	}
}

func TestSpecRejectsUnsafeNames(t *testing.T) {
	ctx := context.Background()
	store := newSpecStore(t, newCRClient(t))
	for _, name := range []string{"", "../escape", "Shop", "a/b"} {
		if _, err := store.Put(ctx, name, testDocuments(), PutOptions{}); err == nil {
			t.Errorf("put with project name %q was accepted", name)
		}
		if _, err := store.Get(ctx, name); err == nil {
			t.Errorf("get with project name %q was accepted", name)
		}
	}
	docs := testDocuments()
	docs.Environments["../escape"] = prodDoc
	if _, err := store.Put(ctx, testProject, docs, PutOptions{}); err == nil {
		t.Error("put with an unsafe environment name was accepted")
	}
}

// The documents must agree with the request about what they are. A project
// document naming another project, or an environment bound to another project,
// would be stored under a name it does not claim and then reconciled against
// the wrong Project — a mismatch a byte store could not see and this one must.
func TestSpecPutRefusesDisagreeingDocuments(t *testing.T) {
	ctx := context.Background()
	store := newSpecStore(t, newCRClient(t))

	wrongProject := testDocuments()
	wrongProject.Project = []byte(strings.ReplaceAll(string(projectDoc), "name: shop", "name: atlas"))
	if _, err := store.Put(ctx, testProject, wrongProject, PutOptions{}); err == nil {
		t.Error("a project document naming another project was accepted")
	}

	wrongBinding := testDocuments()
	wrongBinding.Environments["production"] = []byte(strings.ReplaceAll(string(prodDoc), "project: shop", "project: atlas"))
	if _, err := store.Put(ctx, testProject, wrongBinding, PutOptions{}); err == nil {
		t.Error("an environment bound to another project was accepted")
	}

	wrongKey := testDocuments()
	wrongKey.Environments["preview"] = prodDoc
	if _, err := store.Put(ctx, testProject, wrongKey, PutOptions{}); err == nil {
		t.Error("an environment document filed under the wrong name was accepted")
	}
}

func TestSpecPutRequiresProjectDocument(t *testing.T) {
	ctx := context.Background()
	store := newSpecStore(t, newCRClient(t))
	if _, err := store.Put(ctx, testProject, Documents{}, PutOptions{}); err == nil {
		t.Error("put with no project document was accepted")
	}
}

func TestSpecPutWithVersionAgainstAbsentProject(t *testing.T) {
	ctx := context.Background()
	store := newSpecStore(t, newCRClient(t))
	_, err := store.Put(ctx, testProject, testDocuments(), PutOptions{ExpectedVersion: "7"})
	if !AsNotFound(err) {
		t.Fatalf("put with a version against an absent project: err = %v, want store/not-found", err)
	}
}

func decodeProject(t *testing.T, doc []byte) *model.Project {
	t.Helper()
	docs, errs := model.DecodeDocuments(doc)
	if len(errs) > 0 {
		t.Fatalf("decoding the returned project document: %v\n%s", errs, doc)
	}
	p, ok := docs[0].(*model.Project)
	if !ok {
		t.Fatalf("the returned project document decoded as %T", docs[0])
	}
	return p
}

func decodeEnvironment(t *testing.T, doc []byte) *model.Environment {
	t.Helper()
	docs, errs := model.DecodeDocuments(doc)
	if len(errs) > 0 {
		t.Fatalf("decoding the returned environment document: %v\n%s", errs, doc)
	}
	e, ok := docs[0].(*model.Environment)
	if !ok {
		t.Fatalf("the returned environment document decoded as %T", docs[0])
	}
	return e
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestSpecPutWritesTheAuthoredImage: the store edits nothing on the way in. It
// used to accept a PutOptions.Image and rewrite `Project.spec.image` with it
// for a Deploy carrying `--image`, which made a one-environment override a
// project-wide fact and left GetSpec returning a project document nobody
// authored. The override is a per-environment component pin now
// (internal/api's pinDeployImage), and what reaches this store is documents.
func TestSpecPutWritesTheAuthoredImage(t *testing.T) {
	ctx := context.Background()
	c := newCRClient(t)
	store := newSpecStore(t, c)

	if _, err := store.Put(ctx, testProject, testDocuments(), PutOptions{}); err != nil {
		t.Fatalf("put: %v", err)
	}
	var project v1alpha1.Project
	if err := c.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: testProject}, &project); err != nil {
		t.Fatal(err)
	}
	if project.Spec.Image != "ghcr.io/acme/shop:v1" {
		t.Errorf("spec.image = %q, want the authored value", project.Spec.Image)
	}
}
