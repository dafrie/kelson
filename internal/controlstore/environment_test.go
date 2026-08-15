package controlstore

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/dafrie/kelson/api/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/model"
)

// The status reader's tests are written against controller-runtime's fake
// client for the reason the spec store's are: it is the only in-tree fake that
// implements the write semantics this package depends on, and it is the same
// one the controller's tests use — so a status written there and read here
// cannot disagree about the shape.

func newWatchClient(t *testing.T, objs ...client.Object) client.WithWatch {
	t.Helper()
	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("registering kelson.dev/v1alpha1: %v", err)
	}
	return crfake.NewClientBuilder().WithScheme(s).WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.Environment{}).Build()
}

func newEnvironmentStore(t *testing.T, c client.WithWatch) *EnvironmentStore {
	t.Helper()
	store, err := NewEnvironmentStore(EnvironmentStoreOptions{Client: c, Namespace: testNamespace})
	if err != nil {
		t.Fatalf("NewEnvironmentStore: %v", err)
	}
	return store
}

// deliveredEnvironment is an Environment as the controller leaves it: a spec, a
// settled status and a history mirror.
func deliveredEnvironment() *v1alpha1.Environment {
	env := &v1alpha1.Environment{}
	env.APIVersion, env.Kind = model.APIVersion, v1alpha1.KindEnvironment
	env.Name, env.Namespace = testEnv, testNamespace
	env.Generation = 4
	env.Spec.Project = testProject
	env.Spec.Namespace = "shop-prod"
	env.Status = v1alpha1.EnvironmentStatus{
		ObservedGeneration: 4,
		Phase:              v1alpha1.PhaseHealthy,
		Revision:           "4-b2c3d4e5",
		Conditions: []metav1.Condition{
			{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonReady,
				Message: "revision 4-b2c3d4e5 is live and healthy", ObservedGeneration: 4,
				LastTransitionTime: metav1.NewTime(time.Unix(1700000000, 0))},
			{Type: v1alpha1.ConditionProgressing, Status: metav1.ConditionFalse, Reason: v1alpha1.ReasonSettled,
				Message: "nothing is in flight for generation 4", ObservedGeneration: 4},
		},
		History: []v1alpha1.HistoryEntry{
			{Revision: "4-b2c3d4e5", Digest: "sha256:beef", Images: []string{"ghcr.io/acme/shop:v2"},
				Outcome: v1alpha1.PhaseHealthy, Timestamp: metav1.NewTime(time.Unix(1700000000, 0))},
			{Revision: "3-9f0a1b2c", Digest: "sha256:cafe", Images: []string{"ghcr.io/acme/shop:v1"},
				Outcome: v1alpha1.PhaseHealthy},
		},
	}
	return env
}

func TestEnvironmentGetReadsTheWholeStatus(t *testing.T) {
	store := newEnvironmentStore(t, newWatchClient(t, deliveredEnvironment()))

	st, err := store.Get(context.Background(), testProject, testEnv)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if st.Phase != v1alpha1.PhaseHealthy || st.Revision != "4-b2c3d4e5" {
		t.Errorf("phase/revision = %q/%q", st.Phase, st.Revision)
	}
	if st.Generation != 4 || st.ObservedGeneration != 4 || !st.Current() {
		t.Errorf("generation bookkeeping = %d/%d", st.Generation, st.ObservedGeneration)
	}
	if !st.Settled() {
		t.Error("an environment whose Progressing is False for the current generation is settled")
	}
	ready, ok := st.Ready()
	if !ok || !ready.True() || ready.Reason != v1alpha1.ReasonReady {
		t.Errorf("ready = %+v", ready)
	}
	if ready.Since.IsZero() {
		t.Error("the condition's transition time did not survive the projection")
	}
	if len(st.History) != 2 || st.History[0].Revision != "4-b2c3d4e5" {
		t.Fatalf("history = %+v, want newest first", st.History)
	}
	if st.Namespace != "shop-prod" {
		t.Errorf("namespace = %q, want the authored spec.namespace", st.Namespace)
	}
}

// A status that has not caught up is not an answer. Every consumer must read
// observedGeneration before believing the rest, so the projection carries it
// and Current() is what says so.
func TestEnvironmentStatusBehindTheSpecIsNotCurrent(t *testing.T) {
	env := deliveredEnvironment()
	env.Generation = 5
	store := newEnvironmentStore(t, newWatchClient(t, env))

	st, err := store.Get(context.Background(), testProject, testEnv)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if st.Current() {
		t.Error("a status at generation 4 describes a spec at generation 5")
	}
	if st.Settled() {
		t.Error("a status that has not caught up cannot have settled")
	}
}

// The object name is the environment name and the namespace is shared, so
// "production" exists at most once. Answering a question about shop/production
// with billing's would be the worst possible way to learn that.
func TestEnvironmentOfAnotherProjectIsNotFound(t *testing.T) {
	env := deliveredEnvironment()
	env.Spec.Project = "billing"
	store := newEnvironmentStore(t, newWatchClient(t, env))

	_, err := store.Get(context.Background(), testProject, testEnv)
	if !AsNotFound(err) {
		t.Fatalf("err = %v, want store/not-found", err)
	}
	if !strings.Contains(err.Error(), "billing") {
		t.Errorf("the refusal should name the owner: %v", err)
	}
}

func TestEnvironmentThatWasNeverStoredIsNotFound(t *testing.T) {
	store := newEnvironmentStore(t, newWatchClient(t))

	_, err := store.Get(context.Background(), testProject, testEnv)
	if !AsNotFound(err) {
		t.Fatalf("err = %v, want store/not-found", err)
	}
	if _, err := store.Watch(context.Background(), testProject, testEnv); !AsNotFound(err) {
		t.Fatalf("watch err = %v, want store/not-found", err)
	}
}

// The watch delivers the current state first and then every change, which is
// what lets a deploy stream report a deployment it did not start.
func TestEnvironmentWatchDeliversCurrentThenChanges(t *testing.T) {
	c := newWatchClient(t, deliveredEnvironment())
	store := newEnvironmentStore(t, c)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	states, err := store.Watch(ctx, testProject, testEnv)
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	first := <-states
	if first.Revision != "4-b2c3d4e5" {
		t.Fatalf("the first state is not the current one: %+v", first)
	}

	var env v1alpha1.Environment
	if err := c.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: testEnv}, &env); err != nil {
		t.Fatal(err)
	}
	env.Status.Phase = v1alpha1.PhaseDegraded
	if err := c.Status().Update(ctx, &env); err != nil {
		t.Fatal(err)
	}

	select {
	case next := <-states:
		if next.Phase != v1alpha1.PhaseDegraded {
			t.Errorf("the change was not delivered: %+v", next)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the watch delivered no event for a status write")
	}

	// Cancelling the caller's context ends the stream, which is what keeps a
	// per-request watch from outliving the request.
	cancel()
	for range states {
	}
}

// Annotating is a merge patch and never a server-side apply: an apply carrying
// only annotations would state a complete intent for the fields kelson-server
// owns, and delete the spec it wrote.
func TestEnvironmentAnnotateKeepsTheSpec(t *testing.T) {
	c := newWatchClient(t, deliveredEnvironment())
	store := newEnvironmentStore(t, c)
	ctx := context.Background()

	if _, err := store.Annotate(ctx, testProject, testEnv, map[string]string{
		v1alpha1.AnnotationRollbackTo: "3-9f0a1b2c",
	}); err != nil {
		t.Fatalf("annotate: %v", err)
	}

	var env v1alpha1.Environment
	if err := c.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: testEnv}, &env); err != nil {
		t.Fatal(err)
	}
	if got := env.Annotations[v1alpha1.AnnotationRollbackTo]; got != "3-9f0a1b2c" {
		t.Errorf("%s = %q", v1alpha1.AnnotationRollbackTo, got)
	}
	if env.Spec.Project != testProject || env.Spec.Namespace != "shop-prod" {
		t.Fatalf("the annotation patch disturbed the spec: %+v", env.Spec)
	}
	if env.Status.Revision != "4-b2c3d4e5" {
		t.Errorf("the annotation patch disturbed the status: %+v", env.Status)
	}
}

func TestEnvironmentAnnotateRefusesAnEnvironmentItDoesNotOwn(t *testing.T) {
	env := deliveredEnvironment()
	env.Spec.Project = "billing"
	store := newEnvironmentStore(t, newWatchClient(t, env))

	_, err := store.Annotate(context.Background(), testProject, testEnv,
		map[string]string{v1alpha1.AnnotationRollbackTo: "3-9f0a1b2c"})
	if !AsNotFound(err) {
		t.Fatalf("err = %v, want store/not-found before anything is patched", err)
	}
}

// Running and PreviousRevision are what a promotion and a default rollback ask
// for, and they differ exactly while a rollback is pinned: what an environment
// *runs* is not always the last thing it published.
func TestRunningAndPreviousRevision(t *testing.T) {
	st := EnvironmentState{
		Revision: "3-9f0a1b2c",
		History: []Revision{
			{Revision: "4-b2c3d4e5", Images: []string{"ghcr.io/acme/shop:v2"}},
			{Revision: "3-9f0a1b2c", Images: []string{"ghcr.io/acme/shop:v1"}},
		},
	}
	running, ok := st.Running()
	if !ok || running.Revision != "3-9f0a1b2c" {
		t.Fatalf("running = %+v, want the pinned revision and not the newest", running)
	}
	previous, ok := st.PreviousRevision()
	if !ok || previous.Revision != "4-b2c3d4e5" {
		t.Fatalf("previous = %+v, want the newest revision that is not the one running", previous)
	}

	// With nothing pinned, what runs is the head and there is one before it.
	st.Revision = "4-b2c3d4e5"
	running, _ = st.Running()
	previous, _ = st.PreviousRevision()
	if running.Revision != "4-b2c3d4e5" || previous.Revision != "3-9f0a1b2c" {
		t.Fatalf("running/previous = %q/%q", running.Revision, previous.Revision)
	}

	// One revision, and it is the one running: there is nothing to roll back to.
	single := EnvironmentState{Revision: "1-aaaaaaaa", History: []Revision{{Revision: "1-aaaaaaaa"}}}
	if _, ok := single.PreviousRevision(); ok {
		t.Error("an environment with one revision has no previous one")
	}
}

// The validation errors keep the model's taxonomy on the way out, so an agent
// that learned to branch on schema/unknown-field at the CLI branches on it here
// without translation (ADR-0027 decision 5).
func TestValidationErrorsKeepTheirTaxonomy(t *testing.T) {
	env := deliveredEnvironment()
	env.Status.ValidationErrors = []v1alpha1.ValidationError{{
		Code:     string(model.ErrUnknownField),
		Resource: "Environment/production",
		Field:    "$.spec.nope",
		Message:  "unknown field",
		Line:     7,
	}}
	store := newEnvironmentStore(t, newWatchClient(t, env))

	st, err := store.Get(context.Background(), testProject, testEnv)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(st.ValidationErrors) != 1 {
		t.Fatalf("validation errors = %+v", st.ValidationErrors)
	}
	first := st.ValidationErrors[0]
	if first.Code != model.ErrUnknownField || first.Field != "$.spec.nope" || first.Line != 7 {
		t.Errorf("error = %+v, want the model's own values", first)
	}
}
