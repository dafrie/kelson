package serverstate

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The stored documents are deliberately not canonical YAML: comments, blank
// lines, non-ASCII and a missing trailing newline are exactly what a
// normalizing store would quietly rewrite (ADR-0013 §1: the spec is the user's
// document).
var (
	projectDoc = []byte("# the shop\napiVersion: kelson.dev/v1\nkind: Project\nmetadata:\n  name: shop  # trailing comment\n\n# note: café ☕\nspec: {}")
	prodDoc    = []byte("kind: Environment\nmetadata:\n  name: production\nspec:\n  replicas: 3\n")
	stagingDoc = []byte("kind: Environment\nmetadata:\n  name: staging\n")
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

func TestSpecPutGetRoundTripsBytes(t *testing.T) {
	ctx := context.Background()
	client := newFakeClient()
	store := newSpecStore(t, client)

	put, err := store.Put(ctx, testProject, testDocuments(), PutOptions{})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if put.Version == "" {
		t.Fatal("put returned an empty version; optimistic concurrency has nothing to check")
	}

	got, err := store.Get(ctx, testProject)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Project != testProject {
		t.Errorf("project = %q, want %q", got.Project, testProject)
	}
	if got.Version != put.Version {
		t.Errorf("version = %q, want %q", got.Version, put.Version)
	}
	if string(got.Documents.Project) != string(projectDoc) {
		t.Errorf("project document round-tripped as:\n%q\nwant:\n%q", got.Documents.Project, projectDoc)
	}
	if string(got.Documents.Environments["production"]) != string(prodDoc) {
		t.Errorf("production document = %q, want %q", got.Documents.Environments["production"], prodDoc)
	}
	if string(got.Documents.Environments["staging"]) != string(stagingDoc) {
		t.Errorf("staging document = %q, want %q", got.Documents.Environments["staging"], stagingDoc)
	}
	if want := []string{"production", "staging"}; !equalStrings(got.Environments, want) {
		t.Errorf("environments = %v, want %v", got.Environments, want)
	}

	// The layout is the ADR's, not an implementation detail tests may ignore:
	// one ConfigMap per project, keyed by document, labelled for discovery.
	cm, err := client.CoreV1().ConfigMaps(testNamespace).Get(ctx, "kelson-spec-shop", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the stored ConfigMap: %v", err)
	}
	for key, want := range map[string]string{
		labelManagedBy: managedByKelson,
		labelProject:   testProject,
		labelState:     stateSpec,
	} {
		if got := cm.Labels[key]; got != want {
			t.Errorf("label %s = %q, want %q", key, got, want)
		}
	}
	for _, key := range []string{"project.yaml", "production.yaml", "staging.yaml"} {
		if _, ok := cm.Data[key]; !ok {
			t.Errorf("stored ConfigMap has no %q key; data keys are %v", key, keysOf(cm.Data))
		}
	}
}

func TestSpecPutRefusesBlindOverwrite(t *testing.T) {
	ctx := context.Background()
	store := newSpecStore(t, newFakeClient())
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
	store := newSpecStore(t, newFakeClient())
	first, err := store.Put(ctx, testProject, testDocuments(), PutOptions{})
	if err != nil {
		t.Fatalf("first put: %v", err)
	}
	second, err := store.Put(ctx, testProject, testDocuments(), PutOptions{ExpectedVersion: first.Version})
	if err != nil {
		t.Fatalf("second put: %v", err)
	}
	if second.Version == first.Version {
		t.Fatal("the version did not change across a write; the conflict test would be vacuous")
	}

	docs := testDocuments()
	docs.Project = []byte("kind: Project\n# lost update\n")
	_, err = store.Put(ctx, testProject, docs, PutOptions{ExpectedVersion: first.Version})
	if !AsVersionConflict(err) {
		t.Fatalf("stale put: err = %v, want store/version-conflict", err)
	}

	got, err := store.Get(ctx, testProject)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(got.Documents.Project) != string(projectDoc) {
		t.Error("the rejected write reached the store anyway")
	}
}

func TestSpecPutForceOverridesVersion(t *testing.T) {
	ctx := context.Background()
	store := newSpecStore(t, newFakeClient())
	if _, err := store.Put(ctx, testProject, testDocuments(), PutOptions{}); err != nil {
		t.Fatalf("first put: %v", err)
	}

	docs := testDocuments()
	docs.Project = []byte("kind: Project\n# forced\n")
	forced, err := store.Put(ctx, testProject, docs, PutOptions{ExpectedVersion: "stale", Force: true})
	if err != nil {
		t.Fatalf("forced put: %v", err)
	}
	if string(forced.Documents.Project) != string(docs.Project) {
		t.Errorf("forced put stored %q, want %q", forced.Documents.Project, docs.Project)
	}
}

func TestSpecPutReplaysIdempotencyKey(t *testing.T) {
	ctx := context.Background()
	client := newFakeClient()
	store := newSpecStore(t, client)

	first, err := store.Put(ctx, testProject, testDocuments(), PutOptions{IdempotencyKey: "key-1"})
	if err != nil {
		t.Fatalf("first put: %v", err)
	}

	// A retried request carries the same key and the version its first attempt
	// was written against — which that attempt has already superseded. The
	// replay must win over the version check and must not write.
	docs := testDocuments()
	docs.Project = []byte("kind: Project\n# a different body under the same key\n")
	replay, err := store.Put(ctx, testProject, docs, PutOptions{IdempotencyKey: "key-1"})
	if err != nil {
		t.Fatalf("replayed put: %v", err)
	}
	if replay.Version != first.Version {
		t.Errorf("replay version = %q, want the recorded %q — the replay wrote", replay.Version, first.Version)
	}
	if string(replay.Documents.Project) != string(projectDoc) {
		t.Errorf("replay returned %q, want the recorded outcome %q", replay.Documents.Project, projectDoc)
	}

	// A different key is a different request and is version-checked normally.
	_, err = store.Put(ctx, testProject, docs, PutOptions{IdempotencyKey: "key-2"})
	if !AsVersionConflict(err) {
		t.Fatalf("put under a new key without a version: err = %v, want store/version-conflict", err)
	}
	stored, err := store.Put(ctx, testProject, docs, PutOptions{IdempotencyKey: "key-2", ExpectedVersion: first.Version})
	if err != nil {
		t.Fatalf("put under a new key: %v", err)
	}
	if string(stored.Documents.Project) != string(docs.Project) {
		t.Error("the second key did not write")
	}
	cm, err := client.CoreV1().ConfigMaps(testNamespace).Get(ctx, "kelson-spec-shop", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the stored ConfigMap: %v", err)
	}
	if got := cm.Annotations[annIdempotencyKey]; got != "key-2" {
		t.Errorf("recorded idempotency key = %q, want %q", got, "key-2")
	}
}

func TestSpecGetAndDeleteReportNotFound(t *testing.T) {
	ctx := context.Background()
	store := newSpecStore(t, newFakeClient())

	if _, err := store.Get(ctx, "absent"); !AsNotFound(err) {
		t.Fatalf("get absent: err = %v, want store/not-found", err)
	}
	if err := store.Delete(ctx, "absent", DeleteOptions{}); !AsNotFound(err) {
		t.Fatalf("delete absent: err = %v, want store/not-found", err)
	}
	// A replayed delete of a project that is already gone is the recorded
	// outcome of a completed delete, not a failure.
	if err := store.Delete(ctx, "absent", DeleteOptions{IdempotencyKey: "key-1"}); err != nil {
		t.Fatalf("replayed delete: %v", err)
	}
}

func TestSpecDelete(t *testing.T) {
	ctx := context.Background()
	store := newSpecStore(t, newFakeClient())
	put, err := store.Put(ctx, testProject, testDocuments(), PutOptions{})
	if err != nil {
		t.Fatalf("put: %v", err)
	}

	if err := store.Delete(ctx, testProject, DeleteOptions{}); !AsVersionConflict(err) {
		t.Fatalf("delete without a version: err = %v, want store/version-conflict", err)
	}
	if err := store.Delete(ctx, testProject, DeleteOptions{ExpectedVersion: "stale"}); !AsVersionConflict(err) {
		t.Fatalf("delete with a stale version: err = %v, want store/version-conflict", err)
	}
	if err := store.Delete(ctx, testProject, DeleteOptions{ExpectedVersion: put.Version}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.Get(ctx, testProject); !AsNotFound(err) {
		t.Fatalf("get after delete: err = %v, want store/not-found", err)
	}
}

func TestSpecDeleteForce(t *testing.T) {
	ctx := context.Background()
	store := newSpecStore(t, newFakeClient())
	if _, err := store.Put(ctx, testProject, testDocuments(), PutOptions{}); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := store.Delete(ctx, testProject, DeleteOptions{Force: true}); err != nil {
		t.Fatalf("forced delete: %v", err)
	}
}

func TestSpecList(t *testing.T) {
	ctx := context.Background()
	client := newFakeClient()
	store := newSpecStore(t, client)

	for _, p := range []string{"warehouse", "shop", "billing"} {
		if _, err := store.Put(ctx, p, testDocuments(), PutOptions{}); err != nil {
			t.Fatalf("put %s: %v", p, err)
		}
	}
	// History ConfigMaps live in the same namespace; the listing must not see
	// them.
	hist := newHistoryStore(t, client, 5)
	if _, err := hist.Append(testProject, testEnv, record("rev-00000001"), []byte("---\nkind: Service\n")); err != nil {
		t.Fatalf("append history: %v", err)
	}

	got, err := store.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	names := make([]string, 0, len(got))
	for _, s := range got {
		names = append(names, s.Project)
	}
	if want := []string{"billing", "shop", "warehouse"}; !equalStrings(names, want) {
		t.Fatalf("list = %v, want %v", names, want)
	}
	if string(got[1].Documents.Project) != string(projectDoc) {
		t.Error("list did not carry the stored documents")
	}
}

func TestSpecRejectsUnsafeNames(t *testing.T) {
	ctx := context.Background()
	client := newFakeClient()
	store := newSpecStore(t, client)

	for _, project := range []string{"", "..", "../etc", "Shop", "shop/production", "shop_prod", "-shop"} {
		if _, err := store.Put(ctx, project, testDocuments(), PutOptions{}); err == nil {
			t.Errorf("put project %q: no error; a name that is not DNS-safe must not reach the API server", project)
		}
		if _, err := store.Get(ctx, project); err == nil {
			t.Errorf("get project %q: no error", project)
		}
		if err := store.Delete(ctx, project, DeleteOptions{Force: true}); err == nil {
			t.Errorf("delete project %q: no error", project)
		}
	}

	docs := testDocuments()
	docs.Environments = map[string][]byte{"Prod/uction": []byte("kind: Environment\n")}
	if _, err := store.Put(ctx, testProject, docs, PutOptions{}); err == nil {
		t.Error("put with an unsafe environment name: no error")
	}

	list, err := client.CoreV1().ConfigMaps(testNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Items) != 0 {
		t.Fatalf("rejected writes created %d ConfigMap(s)", len(list.Items))
	}
}

func TestSpecPutRequiresProjectDocument(t *testing.T) {
	store := newSpecStore(t, newFakeClient())
	_, err := store.Put(context.Background(), testProject, Documents{Environments: map[string][]byte{"production": prodDoc}}, PutOptions{})
	if err == nil {
		t.Fatal("put without a project document: no error")
	}
}

func TestSpecPutWithVersionAgainstAbsentProject(t *testing.T) {
	store := newSpecStore(t, newFakeClient())
	_, err := store.Put(context.Background(), testProject, testDocuments(), PutOptions{ExpectedVersion: "7"})
	if !AsNotFound(err) {
		t.Fatalf("err = %v, want store/not-found", err)
	}
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

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
