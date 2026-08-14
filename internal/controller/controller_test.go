package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/dafrie/kelson/api/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/clusterprofile"
	"github.com/dafrie/kelson/internal/model"
)

// These tests drive the reconcilers against controller-runtime's fake client.
// It is the right harness for what is being asserted: every branch here is a
// decision about what to write into `.status`, and none of them depends on the
// API server's own behaviour beyond storing what it is given. The envtest suite
// (envtest_test.go, opt-in) is what covers the parts a fake client cannot —
// that the generated CRDs install at all, that the schema accepts a real
// document, and that the CEL rules refuse what they claim to.

const testNamespace = "checkout"

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("registering the kelson scheme: %v", err)
	}
	return s
}

// newClient builds a fake client with the status subresource enabled, which is
// what makes Status().Patch behave the way it does against a real API server:
// a status write does not touch the spec, and a spec write does not touch the
// status.
func newClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(objects...).
		WithStatusSubresource(&v1alpha1.Project{}, &v1alpha1.Environment{}).
		Build()
}

func project(name string, spec model.ProjectSpec) *v1alpha1.Project {
	return &v1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, Generation: 1},
		Spec:       spec,
	}
}

func environment(name string, spec model.EnvironmentSpec) *v1alpha1.Environment {
	return &v1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, Generation: 1},
		Spec:       spec,
	}
}

// validProject is the smallest project that validates and renders: one web
// component off a pre-built image.
func validProject() *v1alpha1.Project {
	return project("checkout", model.ProjectSpec{
		Image: "ghcr.io/acme/checkout:1.0.0",
		Components: []model.Component{
			{Name: "web", Port: 8080},
		},
	})
}

func validEnvironment() *v1alpha1.Environment {
	return environment("production", model.EnvironmentSpec{Project: "checkout"})
}

func request(name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: name}}
}

func readEnvironment(t *testing.T, c client.Client, name string) *v1alpha1.Environment {
	t.Helper()
	var env v1alpha1.Environment
	if err := c.Get(context.Background(), request(name).NamespacedName, &env); err != nil {
		t.Fatalf("reading back environment %s: %v", name, err)
	}
	return &env
}

func ready(t *testing.T, conditions []metav1.Condition) *metav1.Condition {
	t.Helper()
	condition := meta.FindStatusCondition(conditions, v1alpha1.ConditionReady)
	if condition == nil {
		t.Fatalf("no Ready condition was set")
	}
	return condition
}

func TestEnvironmentValidPairIsReady(t *testing.T) {
	c := newClient(t, validProject(), validEnvironment())
	r := &EnvironmentReconciler{Client: c, Profiles: StaticProfileSource{}}

	result, err := r.Reconcile(context.Background(), request("production"))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("a settled environment must not ask to be requeued, got %+v", result)
	}

	env := readEnvironment(t, c, "production")
	condition := ready(t, env.Status.Conditions)
	if condition.Status != metav1.ConditionTrue {
		t.Fatalf("Ready is %s (%s: %s), want True", condition.Status, condition.Reason, condition.Message)
	}
	if condition.Reason != v1alpha1.ReasonReady {
		t.Errorf("Ready reason is %q, want %q", condition.Reason, v1alpha1.ReasonReady)
	}
	if env.Status.ObservedGeneration != 1 {
		t.Errorf("observedGeneration is %d, want 1", env.Status.ObservedGeneration)
	}
	if len(env.Status.ValidationErrors) != 0 {
		t.Errorf("a valid environment must carry no validationErrors, got %+v", env.Status.ValidationErrors)
	}
	// Nothing is published yet (issue #224), and the status must not pretend
	// otherwise.
	if env.Status.Revision != "" || env.Status.Phase != "" {
		t.Errorf("the no-op deliverer must leave revision and phase empty, got %q/%q",
			env.Status.Revision, env.Status.Phase)
	}
}

func TestEnvironmentInvalidSpecIsSpecInvalid(t *testing.T) {
	// An override naming a component the project does not declare. It is a
	// cross-document error, so it is exactly what the controller adds over the
	// API server's own schema check.
	env := environment("production", model.EnvironmentSpec{
		Project:    "checkout",
		Components: []model.ComponentOverride{{Name: "does-not-exist", Image: "ghcr.io/acme/x:1"}},
	})
	c := newClient(t, validProject(), env)
	r := &EnvironmentReconciler{Client: c, Profiles: StaticProfileSource{}}

	if _, err := r.Reconcile(context.Background(), request("production")); err != nil {
		t.Fatalf("an invalid spec must not be returned as an error (it would hot-loop): %v", err)
	}

	got := readEnvironment(t, c, "production")
	condition := ready(t, got.Status.Conditions)
	if condition.Status != metav1.ConditionFalse || condition.Reason != v1alpha1.ReasonSpecInvalid {
		t.Fatalf("Ready is %s/%s, want False/%s", condition.Status, condition.Reason, v1alpha1.ReasonSpecInvalid)
	}
	if len(got.Status.ValidationErrors) == 0 {
		t.Fatalf("status.validationErrors is empty; the codes are the point (ADR-0027 decision 5)")
	}
	first := got.Status.ValidationErrors[0]
	if first.Code != string(model.ErrUnknownComponent) {
		t.Errorf("first code is %q, want %q", first.Code, model.ErrUnknownComponent)
	}
	if first.Field == "" || first.Remediation == "" {
		t.Errorf("a validation error must carry its field and remediation, got %+v", first)
	}
	if !strings.Contains(first.DocsURL, "ref-unknown-component") {
		t.Errorf("docsUrl is %q, want the ref/unknown-component page", first.DocsURL)
	}
	if got.Status.ObservedGeneration != 1 {
		t.Errorf("observedGeneration is %d, want 1", got.Status.ObservedGeneration)
	}
}

func TestEnvironmentMissingProjectHasItsOwnReason(t *testing.T) {
	c := newClient(t, validEnvironment())
	r := &EnvironmentReconciler{Client: c, Profiles: StaticProfileSource{}}

	if _, err := r.Reconcile(context.Background(), request("production")); err != nil {
		t.Fatalf("a missing project must not be returned as an error: %v", err)
	}

	env := readEnvironment(t, c, "production")
	condition := ready(t, env.Status.Conditions)
	if condition.Status != metav1.ConditionFalse || condition.Reason != v1alpha1.ReasonProjectNotFound {
		t.Fatalf("Ready is %s/%s, want False/%s", condition.Status, condition.Reason, v1alpha1.ReasonProjectNotFound)
	}
	if len(env.Status.ValidationErrors) != 0 {
		t.Errorf("a missing project is not a validation error: %+v", env.Status.ValidationErrors)
	}
	if !strings.Contains(condition.Message, "checkout") {
		t.Errorf("the message must name the project it could not find, got %q", condition.Message)
	}
}

// TestEnvironmentRecoversWhenTheProjectAppears is the reason the missing-project
// case is a status and not an error: applying the two documents in the wrong
// order must converge, without anything having to retry on a timer.
func TestEnvironmentRecoversWhenTheProjectAppears(t *testing.T) {
	c := newClient(t, validEnvironment())
	r := &EnvironmentReconciler{Client: c, Profiles: StaticProfileSource{}}
	ctx := context.Background()

	if _, err := r.Reconcile(ctx, request("production")); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if err := c.Create(ctx, validProject()); err != nil {
		t.Fatalf("creating the project: %v", err)
	}

	// The watch would produce this request; the map function is tested below.
	if _, err := r.Reconcile(ctx, request("production")); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	condition := ready(t, readEnvironment(t, c, "production").Status.Conditions)
	if condition.Status != metav1.ConditionTrue {
		t.Fatalf("Ready is %s (%s: %s), want True once the project exists",
			condition.Status, condition.Reason, condition.Message)
	}
}

// TestEnvironmentStaleValidationErrorsAreCleared: a fixed document must stop
// carrying the evidence of the problem it no longer has.
func TestEnvironmentStaleValidationErrorsAreCleared(t *testing.T) {
	env := validEnvironment()
	env.Status.ValidationErrors = []v1alpha1.ValidationError{{Code: "ref/unknown-component", Message: "stale"}}
	c := newClient(t, validProject(), env)
	r := &EnvironmentReconciler{Client: c, Profiles: StaticProfileSource{}}

	if _, err := r.Reconcile(context.Background(), request("production")); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if errs := readEnvironment(t, c, "production").Status.ValidationErrors; len(errs) != 0 {
		t.Fatalf("validationErrors survived a successful reconcile: %+v", errs)
	}
}

// TestEnvironmentOverlaysAreRefusedByName: overlay paths resolve against the
// authoring files, and a custom resource has none.
func TestEnvironmentOverlaysAreRefusedByName(t *testing.T) {
	env := environment("production", model.EnvironmentSpec{
		Project:  "checkout",
		Overlays: []model.Overlay{{Patch: "overlays/patch.yaml"}},
	})
	c := newClient(t, validProject(), env)
	r := &EnvironmentReconciler{Client: c, Profiles: StaticProfileSource{}}

	if _, err := r.Reconcile(context.Background(), request("production")); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	condition := ready(t, readEnvironment(t, c, "production").Status.Conditions)
	if condition.Reason != v1alpha1.ReasonRenderFailed {
		t.Fatalf("Ready reason is %q, want %q", condition.Reason, v1alpha1.ReasonRenderFailed)
	}
	if !strings.Contains(condition.Message, "overlay") {
		t.Errorf("the message must say what it refused, got %q", condition.Message)
	}
}

// TestEnvironmentProfileFailureIsRetried is the other half of the "never an
// error return" rule: a transient cluster problem IS an error, because a retry
// is what fixes it.
func TestEnvironmentProfileFailureIsRetried(t *testing.T) {
	c := newClient(t, validProject(), validEnvironment())
	r := &EnvironmentReconciler{Client: c, Profiles: failingProfile{}}

	if _, err := r.Reconcile(context.Background(), request("production")); err == nil {
		t.Fatal("a profile that could not be read must be returned as an error so the reconcile is retried")
	}
}

type failingProfile struct{}

func (failingProfile) Profile(context.Context) (clusterprofile.ClusterProfile, error) {
	return clusterprofile.ClusterProfile{}, errors.New("the cluster is unreachable")
}

// TestEnvironmentDelivererReceivesTheRender pins the seam issue #224 will fill:
// the reconciler hands over the pair, the generation and the rendered set.
func TestEnvironmentDelivererReceivesTheRender(t *testing.T) {
	c := newClient(t, validProject(), validEnvironment())
	spy := &spyDeliverer{outcome: Outcome{Revision: "1-abcd1234", Phase: v1alpha1.PhaseCommitted}}
	r := &EnvironmentReconciler{Client: c, Profiles: StaticProfileSource{}, Delivery: spy}

	if _, err := r.Reconcile(context.Background(), request("production")); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if spy.calls != 1 {
		t.Fatalf("the deliverer was called %d times, want 1", spy.calls)
	}
	if spy.got.Project != "checkout" || spy.got.Environment != "production" || spy.got.Generation != 1 {
		t.Errorf("the deliverer got %+v, want the checkout/production pair at generation 1", spy.got)
	}
	if len(spy.got.Manifests) == 0 {
		t.Errorf("the deliverer got no manifests")
	}
	env := readEnvironment(t, c, "production")
	if env.Status.Revision != "1-abcd1234" || env.Status.Phase != v1alpha1.PhaseCommitted {
		t.Errorf("the outcome was not written to status: %q/%q", env.Status.Revision, env.Status.Phase)
	}
}

// TestEnvironmentDeliveryFailureIsRetried: publishing is I/O, so it is retried.
func TestEnvironmentDeliveryFailureIsRetried(t *testing.T) {
	c := newClient(t, validProject(), validEnvironment())
	r := &EnvironmentReconciler{
		Client:   c,
		Profiles: StaticProfileSource{},
		Delivery: &spyDeliverer{err: errors.New("the registry refused the push")},
	}
	if _, err := r.Reconcile(context.Background(), request("production")); err == nil {
		t.Fatal("a delivery failure must be returned as an error so the reconcile is retried")
	}
}

// spyDeliverer is the fake behind the [Deliverer] seam: it records what the
// reconciler decided to publish and returns whatever outcome a test needs, so
// every branch of validation, rollback, history and the finalizer is testable
// without a registry.
type spyDeliverer struct {
	calls   int
	got     Revision
	outcome Outcome
	err     error

	tornDown []string
	teardown error
}

func (s *spyDeliverer) Deliver(_ context.Context, rev Revision) (Outcome, error) {
	s.calls++
	s.got = rev
	return s.outcome, s.err
}

func (s *spyDeliverer) Teardown(_ context.Context, project, environment string) error {
	s.tornDown = append(s.tornDown, project+"/"+environment)
	return s.teardown
}

func TestEnvironmentDeletedIsNotAnError(t *testing.T) {
	c := newClient(t)
	r := &EnvironmentReconciler{Client: c, Profiles: StaticProfileSource{}}
	if _, err := r.Reconcile(context.Background(), request("gone")); err != nil {
		t.Fatalf("reconciling a deleted environment must be a no-op, got %v", err)
	}
}

// TestEnvironmentsOfProject is the watch's map function: a Project change must
// enqueue exactly the Environments that name it.
func TestEnvironmentsOfProject(t *testing.T) {
	other := environment("staging", model.EnvironmentSpec{Project: "other"})
	c := newClient(t, validProject(), validEnvironment(), other)
	r := &EnvironmentReconciler{Client: c}

	requests := r.environmentsOfProject(context.Background(), validProject())
	if len(requests) != 1 {
		t.Fatalf("got %d requests, want 1: %+v", len(requests), requests)
	}
	if requests[0].Name != "production" || requests[0].Namespace != testNamespace {
		t.Errorf("got %v, want %s/production", requests[0], testNamespace)
	}
}

func TestProjectValidIsReady(t *testing.T) {
	c := newClient(t, validProject())
	r := &ProjectReconciler{Client: c}

	if _, err := r.Reconcile(context.Background(), request("checkout")); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var got v1alpha1.Project
	if err := c.Get(context.Background(), request("checkout").NamespacedName, &got); err != nil {
		t.Fatalf("reading back: %v", err)
	}
	condition := ready(t, got.Status.Conditions)
	if condition.Status != metav1.ConditionTrue {
		t.Fatalf("Ready is %s (%s: %s), want True", condition.Status, condition.Reason, condition.Message)
	}
	if got.Status.ObservedGeneration != 1 {
		t.Errorf("observedGeneration is %d, want 1", got.Status.ObservedGeneration)
	}
}

func TestProjectInvalidIsSpecInvalid(t *testing.T) {
	// Two components with the same name: a project-only error, so it is the
	// ProjectReconciler's to find.
	p := project("checkout", model.ProjectSpec{
		Image: "ghcr.io/acme/checkout:1.0.0",
		Components: []model.Component{
			{Name: "web", Port: 8080},
			{Name: "web", Port: 9090},
		},
	})
	c := newClient(t, p)
	r := &ProjectReconciler{Client: c}

	if _, err := r.Reconcile(context.Background(), request("checkout")); err != nil {
		t.Fatalf("an invalid project must not be returned as an error: %v", err)
	}
	var got v1alpha1.Project
	if err := c.Get(context.Background(), request("checkout").NamespacedName, &got); err != nil {
		t.Fatalf("reading back: %v", err)
	}
	condition := ready(t, got.Status.Conditions)
	if condition.Status != metav1.ConditionFalse || condition.Reason != v1alpha1.ReasonSpecInvalid {
		t.Fatalf("Ready is %s/%s, want False/%s", condition.Status, condition.Reason, v1alpha1.ReasonSpecInvalid)
	}
	if len(got.Status.ValidationErrors) == 0 {
		t.Fatal("status.validationErrors is empty")
	}
	if code := got.Status.ValidationErrors[0].Code; code != string(model.ErrDuplicateName) {
		t.Errorf("first code is %q, want %q", code, model.ErrDuplicateName)
	}
}

// TestValidationErrorsCarryTheWholeTaxonomy: the status shape must not lose a
// field model.Error carries, or the status becomes a lossy dialect of the same
// vocabulary.
func TestValidationErrorsCarryTheWholeTaxonomy(t *testing.T) {
	in := model.Errors{{
		Code:        model.ErrUnknownField,
		Resource:    "Project/checkout",
		Field:       "$.spec.components[0].nope",
		Message:     "unknown field",
		Remediation: "remove it",
		DocsURL:     "https://kelson.dev/model/errors/schema-unknown-field",
		Line:        7,
		Column:      3,
	}}
	got := validationErrors(in)
	if len(got) != 1 {
		t.Fatalf("got %d errors, want 1", len(got))
	}
	want := v1alpha1.ValidationError{
		Code:        "schema/unknown-field",
		Resource:    "Project/checkout",
		Field:       "$.spec.components[0].nope",
		Message:     "unknown field",
		Remediation: "remove it",
		DocsURL:     "https://kelson.dev/model/errors/schema-unknown-field",
		Line:        7,
		Column:      3,
	}
	if got[0] != want {
		t.Errorf("got %+v, want %+v", got[0], want)
	}
	if validationErrors(nil) != nil {
		t.Errorf("no errors must convert to a nil slice, not an empty one")
	}
}
