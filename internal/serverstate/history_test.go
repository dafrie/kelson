package serverstate

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/dafrie/kelson/internal/delivery/direct"
)

func record(revision string) direct.Record {
	return direct.Record{
		Revision: revision,
		SpecHash: "sha256:" + revision,
		Type:     direct.TypeDeploy,
		Message:  "deploy " + revision,
		Resources: []direct.ResourceRef{
			{APIVersion: "v1", Kind: "Service", Name: "web", Namespace: "shop-production"},
		},
	}
}

func renderedFor(revision string) []byte {
	return []byte(fmt.Sprintf("---\napiVersion: v1\nkind: Service\nmetadata:\n  name: web\n  annotations:\n    kelson.dev/revision: %s\n", revision))
}

func TestHistoryAppendRoundTrip(t *testing.T) {
	ctx := context.Background()
	client := newFakeClient()
	store := newHistoryStore(t, client, 5)

	rendered := renderedFor("rev-00000001")
	entry, err := store.Append(testProject, testEnv, record("rev-00000001"), rendered)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if entry.Revision != "rev-00000001" {
		t.Errorf("entry revision = %q, want rev-00000001", entry.Revision)
	}
	if entry.CommittedAt == "" {
		t.Error("append did not stamp a commit timestamp")
	}

	rec, err := store.Get(testProject, testEnv, "rev-00000001")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if rec == nil {
		t.Fatal("get returned no record for a revision that was just appended")
	}
	if rec.SpecHash != "sha256:rev-00000001" || rec.Type != direct.TypeDeploy || len(rec.Resources) != 1 {
		t.Errorf("record round-tripped as %+v", *rec)
	}

	got, err := store.Rendered(testProject, testEnv, "rev-00000001")
	if err != nil {
		t.Fatalf("rendered: %v", err)
	}
	if !bytes.Equal(got, rendered) {
		t.Errorf("rendered round-tripped as:\n%q\nwant:\n%q", got, rendered)
	}

	// The rendered payload is gzip in BinaryData, not text in Data: ConfigMap
	// Data must be valid UTF-8 and gzip is not (ADR-0013 §1).
	cm, err := client.CoreV1().ConfigMaps(testNamespace).Get(ctx, "kelson-hist-shop-production-rev-00000001", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the stored ConfigMap: %v", err)
	}
	if _, ok := cm.Data[renderedKey]; ok {
		t.Error("the rendered payload is in Data; it must be raw gzip in BinaryData")
	}
	blob := cm.BinaryData[renderedKey]
	if len(blob) < 2 || blob[0] != 0x1f || blob[1] != 0x8b {
		t.Errorf("BinaryData[%q] does not start with the gzip magic: % x", renderedKey, blob)
	}
	if _, ok := cm.Data[recordKey]; !ok {
		t.Errorf("the record is not stored as text in Data; keys are %v", keysOf(cm.Data))
	}
	for key, want := range map[string]string{
		labelManagedBy:   managedByKelson,
		labelState:       stateHistory,
		labelProject:     testProject,
		labelEnvironment: testEnv,
		labelRevision:    "rev-00000001",
	} {
		if got := cm.Labels[key]; got != want {
			t.Errorf("label %s = %q, want %q", key, got, want)
		}
	}
}

func TestHistoryListLatestNewestFirst(t *testing.T) {
	store := newHistoryStore(t, newFakeClient(), 10)
	for i := 1; i <= 3; i++ {
		rev := formatRevision(i)
		if _, err := store.Append(testProject, testEnv, record(rev), renderedFor(rev)); err != nil {
			t.Fatalf("append %s: %v", rev, err)
		}
	}
	// A second environment must not leak into the first's history.
	if _, err := store.Append(testProject, "staging", record("rev-00000001"), renderedFor("staging")); err != nil {
		t.Fatalf("append staging: %v", err)
	}

	recs, err := store.List(testProject, testEnv)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want := []string{"rev-00000003", "rev-00000002", "rev-00000001"}
	got := make([]string, 0, len(recs))
	for _, r := range recs {
		got = append(got, r.Revision)
	}
	if !equalStrings(got, want) {
		t.Fatalf("list = %v, want %v", got, want)
	}

	latest, err := store.Latest(testProject, testEnv)
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if latest == nil || latest.Revision != "rev-00000003" {
		t.Fatalf("latest = %v, want rev-00000003", latest)
	}

	empty, err := store.Latest(testProject, "canary")
	if err != nil {
		t.Fatalf("latest of an untouched environment: %v", err)
	}
	if empty != nil {
		t.Fatalf("latest of an untouched environment = %v, want nil", empty)
	}
}

func TestHistoryNextRevisionSequences(t *testing.T) {
	store := newHistoryStore(t, newFakeClient(), 10)
	for i := 1; i <= 3; i++ {
		next, err := store.NextRevision(testProject, testEnv)
		if err != nil {
			t.Fatalf("next revision: %v", err)
		}
		if want := formatRevision(i); next != want {
			t.Fatalf("next revision = %q, want %q", next, want)
		}
		if _, err := store.Append(testProject, testEnv, record(next), renderedFor(next)); err != nil {
			t.Fatalf("append %s: %v", next, err)
		}
	}
}

// A revision name already taken by another server is not a failed deploy: the
// append recomputes the next id and retries (ADR-0013 §1, concurrency).
func TestHistoryAppendRetriesOnRevisionCollision(t *testing.T) {
	store := newHistoryStore(t, newFakeClient(), 10)
	if _, err := store.Append(testProject, testEnv, record("rev-00000001"), renderedFor("rev-00000001")); err != nil {
		t.Fatalf("first append: %v", err)
	}

	entry, err := store.Append(testProject, testEnv, record("rev-00000001"), renderedFor("second"))
	if err != nil {
		t.Fatalf("colliding append: %v", err)
	}
	if entry.Revision != "rev-00000002" {
		t.Fatalf("colliding append recorded %q, want rev-00000002", entry.Revision)
	}
	got, err := store.Rendered(testProject, testEnv, "rev-00000002")
	if err != nil {
		t.Fatalf("rendered: %v", err)
	}
	if !bytes.Equal(got, renderedFor("second")) {
		t.Error("the retried append recorded the wrong payload")
	}
	// The record stored under the new name must name the new revision too,
	// or history would disagree with itself.
	rec, err := store.Get(testProject, testEnv, "rev-00000002")
	if err != nil || rec == nil {
		t.Fatalf("get rev-00000002: rec=%v err=%v", rec, err)
	}
	if rec.Revision != "rev-00000002" {
		t.Errorf("stored record revision = %q, want rev-00000002", rec.Revision)
	}
}

func TestHistoryRetentionPrunes(t *testing.T) {
	ctx := context.Background()
	client := newFakeClient()
	store := newHistoryStore(t, client, 3)

	for i := 1; i <= 5; i++ {
		rev := formatRevision(i)
		if _, err := store.Append(testProject, testEnv, record(rev), renderedFor(rev)); err != nil {
			t.Fatalf("append %s: %v", rev, err)
		}
	}

	recs, err := store.List(testProject, testEnv)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := make([]string, 0, len(recs))
	for _, r := range recs {
		got = append(got, r.Revision)
	}
	if want := []string{"rev-00000005", "rev-00000004", "rev-00000003"}; !equalStrings(got, want) {
		t.Fatalf("list after pruning = %v, want %v", got, want)
	}

	// A pruned revision is (nil, nil) from Get — "not retained", not "the
	// store failed" — matching the JSONL store the adapter was written against.
	rec, err := store.Get(testProject, testEnv, "rev-00000001")
	if err != nil {
		t.Fatalf("get a pruned revision: %v", err)
	}
	if rec != nil {
		t.Fatalf("get a pruned revision = %v, want nil", rec)
	}
	if _, err := store.Rendered(testProject, testEnv, "rev-00000001"); !AsNotFound(err) {
		t.Fatalf("rendered of a pruned revision: err = %v, want store/not-found", err)
	}

	list, err := client.CoreV1().ConfigMaps(testNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list ConfigMaps: %v", err)
	}
	if len(list.Items) != 3 {
		t.Fatalf("%d ConfigMaps remain, want 3; pruning left objects behind", len(list.Items))
	}

	// Retention must not reach across environments.
	if _, err := store.Append(testProject, "staging", record("rev-00000001"), renderedFor("staging")); err != nil {
		t.Fatalf("append staging: %v", err)
	}
	if recs, err := store.List(testProject, testEnv); err != nil || len(recs) != 3 {
		t.Fatalf("production history after a staging append: %d records, err=%v", len(recs), err)
	}
}

func TestHistoryAppendRejectsOversizedRendered(t *testing.T) {
	client := newFakeClient()
	store := newHistoryStore(t, client, 5)

	// Random bytes do not compress, so this is over the budget after gzip —
	// the case that must fail loudly rather than store a truncated revision.
	huge := make([]byte, 2*maxRenderedBytes)
	rng := rand.New(rand.NewSource(1))
	if _, err := rng.Read(huge); err != nil {
		t.Fatalf("build payload: %v", err)
	}

	_, err := store.Append(testProject, testEnv, record("rev-00000001"), huge)
	if !AsTooLarge(err) {
		t.Fatalf("append an oversized payload: err = %v, want store/too-large", err)
	}
	recs, err := store.List(testProject, testEnv)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("the rejected append recorded %d revision(s); a partial history is worse than none", len(recs))
	}

	// A large but compressible payload is fine: the budget is on the stored
	// bytes, not the rendered ones.
	compressible := bytes.Repeat([]byte("apiVersion: v1\nkind: Service\n"), 100_000)
	if _, err := store.Append(testProject, testEnv, record("rev-00000001"), compressible); err != nil {
		t.Fatalf("append a compressible payload: %v", err)
	}
	got, err := store.Rendered(testProject, testEnv, "rev-00000001")
	if err != nil {
		t.Fatalf("rendered: %v", err)
	}
	if !bytes.Equal(got, compressible) {
		t.Error("a large compressible payload did not round-trip")
	}
}

func TestHistoryRejectsUnsafeNames(t *testing.T) {
	ctx := context.Background()
	client := newFakeClient()
	store := newHistoryStore(t, client, 5)

	for _, tc := range []struct{ project, env, revision string }{
		{"", testEnv, "rev-00000001"},
		{"..", testEnv, "rev-00000001"},
		{"shop/x", testEnv, "rev-00000001"},
		{testProject, "Production", "rev-00000001"},
		{testProject, "prod/uction", "rev-00000001"},
		{testProject, testEnv, ""},
		{testProject, testEnv, "../../etc"},
		{testProject, testEnv, "rev 1"},
	} {
		if _, err := store.Append(tc.project, tc.env, record(tc.revision), renderedFor("x")); err == nil {
			t.Errorf("append(%q, %q, %q): no error", tc.project, tc.env, tc.revision)
		}
		if _, err := store.List(tc.project, tc.env); err == nil && tc.project != testProject {
			t.Errorf("list(%q, %q): no error", tc.project, tc.env)
		}
		if _, err := store.Rendered(tc.project, tc.env, tc.revision); err == nil {
			t.Errorf("rendered(%q, %q, %q): no error", tc.project, tc.env, tc.revision)
		}
	}

	list, err := client.CoreV1().ConfigMaps(testNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Items) != 0 {
		t.Fatalf("rejected appends created %d ConfigMap(s)", len(list.Items))
	}
}

// The name is bounded structurally: DNS-1123 labels cap project, environment
// and revision at 63 characters each, so the longest possible object name is
// far inside the 253-character limit and no hashing is needed.
func TestHistoryNameStaysWithinTheObjectNameLimit(t *testing.T) {
	long := func(n int) string { return string(bytes.Repeat([]byte("a"), n)) }
	name := historyName(long(63), long(63), long(63))
	if len(name) > 253 {
		t.Fatalf("longest history name is %d characters, over the 253 limit", len(name))
	}
}
