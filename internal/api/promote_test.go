package api

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/controlstore"
	"github.com/dafrie/kelson/internal/model"
	"github.com/dafrie/kelson/internal/promote"
)

// The promotion fixture: one project, two environments, no routing (so the
// zero ClusterProfile renders it), and a production document written the way a
// person writes one — comments, blank lines and a component override that does
// not carry an image yet.
const (
	promoteProjectDoc = `apiVersion: kelson.dev/v1alpha1
kind: Project
metadata:
  name: checkout
spec:
  image: ghcr.io/acme/checkout:main
  components:
    - name: web
      port: 8080
    - name: digest
      schedule: 30 6 * * 1-5
`

	promoteStagingDoc = `apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata:
  name: staging
spec:
  project: checkout
`

	promoteProductionDoc = `apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata:
  name: production

# Production is deliberately conservative; the comment below is load-bearing
# for this test, because a promotion must not disturb it.
spec:
  project: checkout

  components:
    - name: web
      replicas: { min: 3, max: 20 }   # sized for the front door
`
)

// Promotion over the spine (issue #225, ADR-0028 decision 6).
//
// ADR-0016 decision 2 defines a promotion as "three existing operations: read
// staging's deployed digest, write production's pin, deploy". The first now
// reads the source `Environment.status` — the revision it is serving and the
// images that revision resolved to — and the second is a server-side apply of
// the target's document, stamped with `kelson.dev/promoted-from`.
// internal/promote — the plan, the byte-faithful splice — is untouched and
// covered in its own package.

// promotedDigest is what staging is running: the same built image for both of
// the project's components, which is what a project-level `image:` produces.
const promotedDigest = "ghcr.io/acme/checkout@sha256:9f6ad2c19f6ad2c19f6ad2c19f6ad2c19f6ad2c19f6ad2c19f6ad2c19f6ad2c1"

// stagingDeployed is the status of a staging environment that published one
// revision and is serving it.
func stagingDeployed() controlstore.EnvironmentState {
	st := healthyEnvironment("checkout", "staging", "7-1a2b3c4d")
	st.History[0].Images = []string{promotedDigest, promotedDigest}
	st.History[0].ComponentImages = []controlstore.ComponentImage{
		{Component: "web", Image: promotedDigest},
		{Component: "digest", Image: promotedDigest},
	}
	return st
}

// promoteEnvironment wires a server whose spec store holds the fixture and
// whose status reader holds what staging runs.
func promoteEnvironment(t *testing.T, states ...controlstore.EnvironmentState) (clients, *fakeSpecStore, *fakeEnvironments) {
	t.Helper()
	store := newFakeSpecStore()
	if _, err := store.Put(context.Background(), "checkout", controlstore.Documents{
		Project: []byte(promoteProjectDoc),
		Environments: map[string][]byte{
			"staging":    []byte(promoteStagingDoc),
			"production": []byte(promoteProductionDoc),
		},
	}, controlstore.PutOptions{}); err != nil {
		t.Fatalf("seeding the spec store: %v", err)
	}
	if len(states) == 0 {
		states = []controlstore.EnvironmentState{stagingDeployed(),
			healthyEnvironment("checkout", "production", "2-0f0f0f0f")}
	}
	envs := newFakeEnvironments(states...)
	connector, _ := connectorFor(nil)
	return serve(t, Options{Specs: store, Delivery: connector, Environments: envs}), store, envs
}

func promoteRequest() *kelsonv1alpha1.PromoteRequest {
	return &kelsonv1alpha1.PromoteRequest{
		Project:         "checkout",
		FromEnvironment: "staging",
		ToEnvironment:   "production",
	}
}

// TestPromoteWritesThePinsAndStampsTheSource: the whole verb, end to end.
func TestPromoteWritesThePinsAndStampsTheSource(t *testing.T) {
	c, store, envs := promoteEnvironment(t)

	res, err := c.deploy.Promote(context.Background(), connect.NewRequest(promoteRequest()))
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if errs := res.Msg.GetErrors(); len(errs) > 0 {
		t.Fatalf("Promote reported findings: %v", errs)
	}
	if res.Msg.GetFromRevision() != "7-1a2b3c4d" {
		t.Errorf("from_revision = %q, want the revision staging is serving", res.Msg.GetFromRevision())
	}
	pinned := 0
	for _, comp := range res.Msg.GetComponents() {
		if comp.GetStatus() != kelsonv1alpha1.PromotionStatus_PROMOTION_STATUS_PINNED {
			continue
		}
		pinned++
		if comp.GetToImage() != promotedDigest {
			t.Errorf("%s pinned to %q, want what staging runs", comp.GetComponent(), comp.GetToImage())
		}
	}
	if pinned == 0 {
		t.Fatalf("nothing was pinned: %+v", res.Msg.GetComponents())
	}

	// The pin is written into the stored document, and the document is
	// otherwise the user's — the comment above it included.
	stored, err := store.Get(context.Background(), "checkout")
	if err != nil {
		t.Fatal(err)
	}
	production := string(stored.Documents.Environments["production"])
	if !strings.Contains(production, promotedDigest) {
		t.Errorf("the pin was not written:\n%s", production)
	}
	if !strings.Contains(production, "# sized for the front door") {
		t.Errorf("the promotion disturbed the document:\n%s", production)
	}
	if res.Msg.GetVersion() == "" || res.Msg.GetVersion() == stored.Version+"x" {
		t.Errorf("version = %q, want the store's version after the write", res.Msg.GetVersion())
	}

	// And the provenance stamp: where did this image come from, as a
	// `kubectl get` rather than an archaeology exercise.
	if len(envs.annotated) != 1 {
		t.Fatalf("annotations = %+v, want one stamp on the target", envs.annotated)
	}
	stamp := envs.annotated[0]
	if stamp.Environment != "production" {
		t.Errorf("the stamp went to %q, want the promoted environment", stamp.Environment)
	}
	if got := stamp.Annotations[annotationPromotedFrom]; got != "staging@7-1a2b3c4d" {
		t.Errorf("%s = %q, want <source>@<revision>", annotationPromotedFrom, got)
	}
}

// TestPromoteDryRunStoresNothing: a dry run computes the pins and the diff and
// writes neither the document nor the stamp.
func TestPromoteDryRunStoresNothing(t *testing.T) {
	c, store, envs := promoteEnvironment(t)
	before, err := store.Get(context.Background(), "checkout")
	if err != nil {
		t.Fatal(err)
	}

	req := promoteRequest()
	req.DryRun = kelsonv1alpha1.DryRun_DRY_RUN_RENDER
	res, err := c.deploy.Promote(context.Background(), connect.NewRequest(req))
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if res.Msg.GetVersion() != "" {
		t.Errorf("version = %q, want empty: a dry run stores nothing", res.Msg.GetVersion())
	}
	if len(res.Msg.GetComponents()) == 0 {
		t.Error("a dry run computed no plan, which is the whole thing it is for")
	}
	after, err := store.Get(context.Background(), "checkout")
	if err != nil {
		t.Fatal(err)
	}
	if after.Version != before.Version {
		t.Errorf("the store was written: version %q -> %q", before.Version, after.Version)
	}
	if len(envs.annotated) != 0 {
		t.Errorf("a dry run stamped %+v", envs.annotated)
	}
}

// TestPromoteFromAnEnvironmentThatRanNothing refuses by name. Re-rendering the
// source spec instead would promote an intention rather than a fact, which is
// the one thing a promotion must never do.
func TestPromoteFromAnEnvironmentThatRanNothing(t *testing.T) {
	empty := healthyEnvironment("checkout", "staging", "")
	empty.History, empty.Revision = nil, ""
	c, store, _ := promoteEnvironment(t, empty)
	before, err := store.Get(context.Background(), "checkout")
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.deploy.Promote(context.Background(), connect.NewRequest(promoteRequest()))
	if !hasCode(detailCodes(err), string(promote.ErrNothingDeployed)) {
		t.Fatalf("details = %v, want %s", detailCodes(err), promote.ErrNothingDeployed)
	}
	after, err := store.Get(context.Background(), "checkout")
	if err != nil {
		t.Fatal(err)
	}
	if after.Version != before.Version {
		t.Errorf("a refused promotion wrote the store: %q -> %q", before.Version, after.Version)
	}
}

// TestPromoteReadsTheRecordedComponentNames: the controller records which
// component resolved each image, so the promotion reads the attribution instead
// of inferring it — including the case no inference could get right, two
// components sharing one image repository.
func TestPromoteReadsTheRecordedComponentNames(t *testing.T) {
	resolved := &model.Resolved{Components: []model.ResolvedComponent{
		{Name: "web", Image: "ghcr.io/acme/app:v3"},
		{Name: "worker", Image: "ghcr.io/acme/app:v3"},
		{Name: "cache", Image: ""},
	}}
	// The recorded order is not the spec's order, and both components share one
	// repository: a positional zip would swap the two builds and a repository
	// match cannot separate them at all.
	recorded := controlstore.Revision{
		Images: []string{"ghcr.io/acme/app:v1", "ghcr.io/acme/app:v2"},
		ComponentImages: []controlstore.ComponentImage{
			{Component: "worker", Image: "ghcr.io/acme/app:v1"},
			{Component: "web", Image: "ghcr.io/acme/app:v2"},
		},
	}
	got := attributeImages(resolved, recorded)
	if got["web"] != "ghcr.io/acme/app:v2" || got["worker"] != "ghcr.io/acme/app:v1" {
		t.Fatalf("attribution = %v, want each image on the component that recorded it", got)
	}
	if _, ok := got["cache"]; ok {
		t.Errorf("a component the revision recorded no image for was attributed one: %v", got)
	}
	// What the same revision would have produced without the recorded names,
	// stated so the improvement is not a claim: the heuristic pairs the builds
	// the wrong way round.
	heuristic := attributeImagesByRepository(resolved, recorded.Images)
	if heuristic["web"] == got["web"] && heuristic["worker"] == got["worker"] {
		t.Errorf("the repository heuristic agreed (%v); this fixture is meant to be one it cannot get right", heuristic)
	}
}

// TestPromoteAttributesPreUpgradeEntriesByRepository: an entry written before
// the controller recorded component names has the flat positional list with the
// imageless components left out, so an image is attributed by what it *is* —
// the repository — and never by where it sat in the list. A component the
// revision carries no image for is skipped with a reason, never guessed.
func TestPromoteAttributesPreUpgradeEntriesByRepository(t *testing.T) {
	resolved := &model.Resolved{Components: []model.ResolvedComponent{
		{Name: "web", Image: "ghcr.io/acme/web:v3"},
		{Name: "worker", Image: "ghcr.io/acme/worker:v3"},
		{Name: "cache", Image: ""},
	}}
	// The revision ran an older tag of each, in the other order — which is what
	// a positional zip would get wrong.
	got := attributeImages(resolved, controlstore.Revision{
		Images: []string{"ghcr.io/acme/worker:v2", "ghcr.io/acme/web:v2"},
	})
	if got["web"] != "ghcr.io/acme/web:v2" || got["worker"] != "ghcr.io/acme/worker:v2" {
		t.Fatalf("attribution = %v, want each image on its own component", got)
	}
	if _, ok := got["cache"]; ok {
		t.Errorf("a component with no image was attributed one: %v", got)
	}
	// An image whose repository matches nothing the spec declares belongs to no
	// component, and is dropped rather than assigned to whatever was left.
	got = attributeImages(resolved, controlstore.Revision{
		Images: []string{"ghcr.io/acme/something-else:v9"},
	})
	if len(got) != 0 {
		t.Errorf("attribution = %v, want nothing attributed", got)
	}
}

// The refusals that are about the request come first. A caller who named a
// project that is not stored, or promoted an environment to itself, has a
// mistake they can fix.
func TestPromoteRequestRefusalsComeFirst(t *testing.T) {
	c, _, _ := promoteEnvironment(t)
	cases := []struct {
		name string
		req  func(*kelsonv1alpha1.PromoteRequest)
		want connect.Code
	}{
		{"to itself", func(r *kelsonv1alpha1.PromoteRequest) { r.ToEnvironment = "staging" }, connect.CodeInvalidArgument},
		{"unknown project", func(r *kelsonv1alpha1.PromoteRequest) { r.Project = "billing" }, connect.CodeNotFound},
		{"unknown source", func(r *kelsonv1alpha1.PromoteRequest) { r.FromEnvironment = "nope" }, connect.CodeInvalidArgument},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := promoteRequest()
			tc.req(req)
			_, err := c.deploy.Promote(context.Background(), connect.NewRequest(req))
			if got := connect.CodeOf(err); got != tc.want {
				t.Fatalf("code = %v, want %v (%v)", got, tc.want, err)
			}
		})
	}
}
