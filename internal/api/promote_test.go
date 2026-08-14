package api

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/controlstore"
	"github.com/dafrie/kelson/internal/delivery"
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

// PromoteService is gated (issue #224).
//
// ADR-0016 decision 2 defines a promotion as "three existing operations: read
// staging's deployed digest, write production's pin, deploy", and the first is
// what broke: the digest came from the rendered-history store, which ADR-0027
// decision 7 deleted. internal/promote — the plan, the byte-faithful splice —
// is untouched and covered in its own package, so what these tests assert is
// the gate: it refuses with the tracked code, it stores nothing, and the
// refusals that are about the *request* still come first.

// promoteEnvironment wires a server whose spec store holds the fixture.
func promoteEnvironment(t *testing.T) (clients, *fakeSpecStore) {
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
	connector, _ := connectorFor(nil)
	return serve(t, Options{Specs: store, Delivery: connector}), store
}

func promoteRequest() *kelsonv1alpha1.PromoteRequest {
	return &kelsonv1alpha1.PromoteRequest{
		Project:         "checkout",
		FromEnvironment: "staging",
		ToEnvironment:   "production",
	}
}

// The gate refuses with the tracked code and the stored spec is untouched. A
// promotion that had already written half the pins before refusing would be
// worse than either outcome it is between.
func TestPromoteIsGatedAndStoresNothing(t *testing.T) {
	c, store := promoteEnvironment(t)
	before, err := store.Get(context.Background(), "checkout")
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.deploy.Promote(context.Background(), connect.NewRequest(promoteRequest()))
	if connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("code = %v, want unimplemented (%v)", connect.CodeOf(err), err)
	}
	if !hasCode(detailCodes(err), string(delivery.ErrNotImplemented)) {
		t.Errorf("details = %v, want %s", detailCodes(err), delivery.ErrNotImplemented)
	}
	if !strings.Contains(err.Error(), "#224") {
		t.Errorf("the refusal must name the tracking issue: %v", err)
	}
	if !strings.Contains(err.Error(), "staging") {
		t.Errorf("the refusal should name the environment whose deployed images it cannot read: %v", err)
	}

	after, err := store.Get(context.Background(), "checkout")
	if err != nil {
		t.Fatal(err)
	}
	if string(after.Documents.Environments["production"]) != string(before.Documents.Environments["production"]) {
		t.Errorf("the stored document changed under a refused promotion:\n%s", after.Documents.Environments["production"])
	}
	if after.Version != before.Version {
		t.Errorf("the store was written: version %q -> %q", before.Version, after.Version)
	}
}

// A dry run is refused too, and for the same reason: it computes the diff a
// promotion would produce, and that diff is built from the digests it cannot
// read. Answering with an empty one would report "nothing to promote".
func TestPromoteDryRunIsGatedToo(t *testing.T) {
	c, _ := promoteEnvironment(t)
	req := promoteRequest()
	req.DryRun = kelsonv1alpha1.DryRun_DRY_RUN_RENDER

	_, err := c.deploy.Promote(context.Background(), connect.NewRequest(req))
	if connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("code = %v, want unimplemented (%v)", connect.CodeOf(err), err)
	}
}

// The refusals that are about the request come first. A caller who named a
// project that is not stored, or promoted an environment to itself, has a
// mistake they can fix; sending them to an issue tracker instead would be
// worse than the missing feature.
func TestPromoteRequestRefusalsPrecedeTheGate(t *testing.T) {
	c, _ := promoteEnvironment(t)
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
			if hasCode(detailCodes(err), string(delivery.ErrNotImplemented)) {
				t.Errorf("a request mistake was reported as kelson's gap: %v", err)
			}
		})
	}
}
