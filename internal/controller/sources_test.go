package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/dafrie/kelson/api/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/model"
)

// The global tier at the reconciler (ADR-0035 decisions 2 and 3): a component
// binds to a GitSource by name, and the deploy path has to hold the same list
// the build path resolved against — otherwise a component builds and then
// refuses to deploy with ref/unknown-source, which is one join written twice.

// fakeGitSources is the instance's tier in memory. It is the controller-side
// twin of internal/api's fake over the same seam, and it can fail, because a
// listing that failed is a different answer from an instance that declares
// nothing.
type fakeGitSources struct {
	sources []model.Source
	err     error
	calls   int
}

func (f *fakeGitSources) ListSources(context.Context) ([]model.Source, error) {
	f.calls++
	return f.sources, f.err
}

// boundProject declares no sources of its own: its component builds from
// `tools`, a name only the instance can supply.
func boundProject() *v1alpha1.Project {
	return project("checkout", model.ProjectSpec{
		Image: "ghcr.io/acme/checkout:1.0.0",
		Components: []model.Component{
			{Name: "web", Port: 8080, Source: &model.ComponentSource{Name: "tools"}},
		},
	})
}

// The gap this closes: the reconciler resolved with no globals, so a project
// whose component was bound to a GitSource built fine and then failed to deploy.
func TestEnvironmentResolvesAgainstTheInstancesGitSources(t *testing.T) {
	sources := &fakeGitSources{sources: []model.Source{
		{Name: "tools", Git: "https://github.com/acme/build-tools", Ref: "v2", Connection: "acme-github"},
	}}
	spy := &spyDeliverer{}
	c := newClient(t, boundProject(), validEnvironment())
	r := &EnvironmentReconciler{Client: c, Profiles: StaticProfileSource{}, Sources: sources, Delivery: spy}

	if _, err := r.Reconcile(context.Background(), request("production")); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	env := readEnvironment(t, c, "production")
	condition := ready(t, env.Status.Conditions)
	if condition.Status != metav1.ConditionTrue {
		t.Fatalf("Ready is %s (%s: %s), want True — the binding names a GitSource the instance declares",
			condition.Status, condition.Reason, condition.Message)
	}
	if sources.calls == 0 {
		t.Fatal("the instance's GitSources were never listed")
	}

	// The binding travelled all the way into the Revision, with the GitSource's
	// own ref and connection: what deploys and what builds must be the same
	// repository.
	if spy.got.Resolved == nil {
		t.Fatal("the deliverer was handed no resolved spec")
	}
	bound := spy.got.Resolved.SourceFor("web")
	if bound == nil {
		t.Fatalf("component web resolved to no source; sources = %+v", spy.got.Resolved.Sources)
	}
	if bound.Git != "https://github.com/acme/build-tools" || bound.Ref != "v2" || bound.Connection != "acme-github" {
		t.Errorf("web is bound to %+v, want the GitSource's repository, ref and connection", *bound)
	}
}

// A listing that failed and an instance that declares nothing produce the same
// empty list and mean opposite things. Resolving against the empty one would
// tell the author their binding is wrong, which is the one thing that is not
// true here.
func TestAFailedGitSourceListingIsARefusalAndNotAnEmptyTier(t *testing.T) {
	spy := &spyDeliverer{}
	c := newClient(t, boundProject(), validEnvironment())
	r := &EnvironmentReconciler{
		Client:   c,
		Profiles: StaticProfileSource{},
		Sources:  &fakeGitSources{err: errors.New("the API server said no")},
		Delivery: spy,
	}

	// An error return, so controller-runtime backs off and lists again: the
	// answer is expected to change without anybody editing anything.
	if _, err := r.Reconcile(context.Background(), request("production")); err == nil {
		t.Fatal("a failed listing must be an error return, so the reconcile is retried")
	}
	if spy.calls != 0 {
		t.Errorf("delivery ran %d times on a reconcile that could not resolve", spy.calls)
	}

	env := readEnvironment(t, c, "production")
	condition := ready(t, env.Status.Conditions)
	if condition.Status != metav1.ConditionFalse || condition.Reason != v1alpha1.ReasonSourcesUnavailable {
		t.Fatalf("Ready is %s/%s, want False/%s", condition.Status, condition.Reason,
			v1alpha1.ReasonSourcesUnavailable)
	}
	if !strings.Contains(condition.Message, "the API server said no") {
		t.Errorf("the message does not carry what failed: %q", condition.Message)
	}
	if len(env.Status.ValidationErrors) != 0 {
		t.Errorf("a failed listing must not be reported as the author's mistake, got %+v",
			env.Status.ValidationErrors)
	}
	// Nothing is in flight after a refusal, and a caller waiting on this
	// generation has to learn that from Progressing rather than from a timeout.
	prog := progressing(t, env.Status.Conditions)
	if prog.Status != metav1.ConditionFalse || prog.Reason != v1alpha1.ReasonSettled {
		t.Errorf("Progressing is %s/%s, want False/%s", prog.Status, prog.Reason, v1alpha1.ReasonSettled)
	}
}

// A controller with no lister is the pre-ADR-0035 posture — projects resolve
// against their own declared sources — and a binding that names nothing in
// scope is the resolver's refusal, naming both halves of the scope it searched.
func TestWithoutAGlobalTierABoundComponentIsRefusedByName(t *testing.T) {
	c := newClient(t, boundProject(), validEnvironment())
	r := &EnvironmentReconciler{Client: c, Profiles: StaticProfileSource{}}

	result, err := r.Reconcile(context.Background(), request("production"))
	if err != nil {
		t.Fatalf("a spec that does not resolve is a status, never an error return: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("requeue = %s, want none: nothing changes until somebody edits the spec", result.RequeueAfter)
	}
	env := readEnvironment(t, c, "production")
	condition := ready(t, env.Status.Conditions)
	if condition.Status != metav1.ConditionFalse || condition.Reason != v1alpha1.ReasonSpecInvalid {
		t.Fatalf("Ready is %s/%s, want False/%s", condition.Status, condition.Reason, v1alpha1.ReasonSpecInvalid)
	}
	if len(env.Status.ValidationErrors) != 1 ||
		env.Status.ValidationErrors[0].Code != string(model.ErrUnknownSource) {
		t.Fatalf("validationErrors = %+v, want one %s", env.Status.ValidationErrors, model.ErrUnknownSource)
	}
}

// A project-local name shadows a global one (ADR-0035 decision 3), and the
// reconciler is the caller that has to hand the resolver both halves for the
// rule to have anything to apply.
func TestAProjectSourceShadowsTheInstances(t *testing.T) {
	local := project("checkout", model.ProjectSpec{
		Image: "ghcr.io/acme/checkout:1.0.0",
		Sources: []model.Source{
			{Name: "tools", Git: "https://github.com/acme/our-own-tools", Ref: "main"},
		},
		Components: []model.Component{
			{Name: "web", Port: 8080, Source: &model.ComponentSource{Name: "tools"}},
		},
	})
	spy := &spyDeliverer{}
	c := newClient(t, local, validEnvironment())
	r := &EnvironmentReconciler{
		Client:   c,
		Profiles: StaticProfileSource{},
		Sources:  &fakeGitSources{sources: []model.Source{{Name: "tools", Git: "https://github.com/acme/build-tools"}}},
		Delivery: spy,
	}

	if _, err := r.Reconcile(context.Background(), request("production")); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	bound := spy.got.Resolved.SourceFor("web")
	if bound == nil || bound.Git != "https://github.com/acme/our-own-tools" {
		t.Fatalf("web is bound to %+v, want the project's own repository", bound)
	}
}
