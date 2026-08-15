package controller

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/dafrie/kelson/api/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/model"
)

// GitSourceReconciler is validation and a condition, and these tests are about
// what it writes and what it deliberately does not: no Reachable, no error
// return for a document that will never fix itself, and the generation stamped
// so a reader can tell a stale verdict from a current one.

func sourceClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(objects...).
		WithStatusSubresource(&v1alpha1.GitSource{}).
		Build()
}

func gitSource(name string, spec model.GitSourceSpec) *v1alpha1.GitSource {
	return &v1alpha1.GitSource{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, Generation: 1},
		Spec:       spec,
	}
}

func readSource(t *testing.T, c client.Client, name string) *v1alpha1.GitSource {
	t.Helper()
	var got v1alpha1.GitSource
	if err := c.Get(context.Background(), request(name).NamespacedName, &got); err != nil {
		t.Fatalf("reading back: %v", err)
	}
	return &got
}

func TestGitSourceValidIsReady(t *testing.T) {
	c := sourceClient(t, gitSource("platform", model.GitSourceSpec{
		Git: "https://github.com/acme/platform.git",
		Ref: "main",
	}))
	r := &GitSourceReconciler{Client: c}

	if _, err := r.Reconcile(context.Background(), request("platform")); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got := readSource(t, c, "platform")
	condition := ready(t, got.Status.Conditions)
	if condition.Status != metav1.ConditionTrue {
		t.Fatalf("Ready is %s (%s: %s), want True", condition.Status, condition.Reason, condition.Message)
	}
	if condition.Reason != v1alpha1.ReasonReady {
		t.Errorf("reason = %q, want %q", condition.Reason, v1alpha1.ReasonReady)
	}
	if got.Status.ObservedGeneration != 1 {
		t.Errorf("observedGeneration = %d, want 1", got.Status.ObservedGeneration)
	}
	if len(got.Status.ValidationErrors) != 0 {
		t.Errorf("a valid document leaves no validation errors: %+v", got.Status.ValidationErrors)
	}
}

// The absence is the decision (ADR-0035 decision 2): reachability belongs to
// the connection that serves the repository, and a second answer here could
// disagree with it.
func TestGitSourceNeverReportsReachability(t *testing.T) {
	c := sourceClient(t, gitSource("platform", model.GitSourceSpec{
		Git:        "https://github.com/acme/platform.git",
		Connection: "acme-github",
	}))
	r := &GitSourceReconciler{Client: c}

	if _, err := r.Reconcile(context.Background(), request("platform")); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	for _, condition := range readSource(t, c, "platform").Status.Conditions {
		if condition.Type == v1alpha1.ConditionReachable {
			t.Fatalf("a GitSource must carry no Reachable condition, got %+v", condition)
		}
	}
}

// An invalid document is a status and never an error return, for the reason
// the package doc gives: it will not become valid by being reconciled again.
func TestGitSourceInvalidIsSpecInvalid(t *testing.T) {
	c := sourceClient(t, gitSource("platform", model.GitSourceSpec{}))
	r := &GitSourceReconciler{Client: c}

	if _, err := r.Reconcile(context.Background(), request("platform")); err != nil {
		t.Fatalf("an invalid source must not be returned as an error: %v", err)
	}
	got := readSource(t, c, "platform")
	condition := ready(t, got.Status.Conditions)
	if condition.Status != metav1.ConditionFalse || condition.Reason != v1alpha1.ReasonSpecInvalid {
		t.Fatalf("Ready is %s/%s, want False/%s", condition.Status, condition.Reason, v1alpha1.ReasonSpecInvalid)
	}
	if len(got.Status.ValidationErrors) == 0 {
		t.Fatal("the errors themselves belong in status.validationErrors")
	}
	// One taxonomy: the same slash code the CLI and the wire carry
	// (ADR-0027 decision 5).
	if code := got.Status.ValidationErrors[0].Code; !strings.Contains(code, "/") {
		t.Errorf("code = %q, want the model's slash taxonomy", code)
	}
}

// A verdict that has been fixed leaves no evidence behind: the errors are
// recomputed from scratch on every reconcile rather than appended to.
func TestGitSourceClearsAStaleVerdict(t *testing.T) {
	invalid := gitSource("platform", model.GitSourceSpec{})
	c := sourceClient(t, invalid)
	r := &GitSourceReconciler{Client: c}

	if _, err := r.Reconcile(context.Background(), request("platform")); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	fixed := readSource(t, c, "platform")
	fixed.Spec.Git = "https://github.com/acme/platform.git"
	fixed.Generation = 2
	if err := c.Update(context.Background(), fixed); err != nil {
		t.Fatalf("updating the spec: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), request("platform")); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got := readSource(t, c, "platform")
	if len(got.Status.ValidationErrors) != 0 {
		t.Errorf("the old errors should be gone: %+v", got.Status.ValidationErrors)
	}
	if condition := ready(t, got.Status.Conditions); condition.Status != metav1.ConditionTrue {
		t.Errorf("Ready is %s, want True once the document is fixed", condition.Status)
	}
	if got.Status.ObservedGeneration != 2 {
		t.Errorf("observedGeneration = %d, want the generation just reconciled", got.Status.ObservedGeneration)
	}
}

// A deleted source is not an error and needs no cleanup: kelson holds no
// finalizer on one.
func TestGitSourceGoneIsNotAnError(t *testing.T) {
	r := &GitSourceReconciler{Client: sourceClient(t)}
	if _, err := r.Reconcile(context.Background(), request("platform")); err != nil {
		t.Fatalf("a missing GitSource must reconcile cleanly: %v", err)
	}
}
