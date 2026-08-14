package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/diff"
	"github.com/dafrie/kelson/internal/serverstate"
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

const (
	stagingWebImage    = "ghcr.io/acme/checkout@sha256:aaaa11112222333344445555666677778888999900001111222233334444aaaa"
	stagingDigestImage = "ghcr.io/acme/checkout@sha256:bbbb11112222333344445555666677778888999900001111222233334444bbbb"
)

// promoteEnvironment wires a server whose spec store holds the fixture and
// whose delivery history has one recorded revision for staging.
func promoteEnvironment(t *testing.T, recorded []delivery.Manifest, history []delivery.Entry) (clients, *fakeSpecStore) {
	t.Helper()
	store := newFakeSpecStore()
	if _, err := store.Put(context.Background(), "checkout", serverstate.Documents{
		Project: []byte(promoteProjectDoc),
		Environments: map[string][]byte{
			"staging":    []byte(promoteStagingDoc),
			"production": []byte(promoteProductionDoc),
		},
	}, serverstate.PutOptions{}); err != nil {
		t.Fatalf("seeding the spec store: %v", err)
	}

	adapter := newFakeAdapter("direct")
	adapter.history = history
	source := &fakeRecorded{revisions: map[string][]delivery.Manifest{"rev-00000007": recorded}}
	connector, _ := connectorFor(adapter, source, nil)
	return serve(t, Options{Specs: store, Delivery: connector}), store
}

// stagingRevision is what staging's latest revision recorded: a Deployment and
// a CronJob, labelled the way the renderer labels them.
func stagingRevision() []delivery.Manifest {
	return []delivery.Manifest{
		{APIVersion: "apps/v1", Kind: "Deployment", Name: "web", Namespace: "checkout-staging", YAML: []byte(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: checkout-staging
  labels:
    kelson.dev/component: web
spec:
  template:
    spec:
      containers:
        - name: web
          image: ` + stagingWebImage + `
`)},
		{APIVersion: "batch/v1", Kind: "CronJob", Name: "digest", Namespace: "checkout-staging", YAML: []byte(`apiVersion: batch/v1
kind: CronJob
metadata:
  name: digest
  namespace: checkout-staging
  labels:
    kelson.dev/component: digest
spec:
  jobTemplate:
    spec:
      template:
        spec:
          containers:
            - name: digest
              image: ` + stagingDigestImage + `
`)},
	}
}

func oneRevision() []delivery.Entry {
	return []delivery.Entry{{Revision: "rev-00000007", CommittedAt: "2026-08-13T10:00:00Z"}}
}

func promoteRequest() *kelsonv1alpha1.PromoteRequest {
	return &kelsonv1alpha1.PromoteRequest{
		Project:         "checkout",
		FromEnvironment: "staging",
		ToEnvironment:   "production",
	}
}

func promoted(res *kelsonv1alpha1.PromoteResponse, component string) *kelsonv1alpha1.PromotedComponent {
	for _, c := range res.GetComponents() {
		if c.GetComponent() == component {
			return c
		}
	}
	return nil
}

// TestPromoteWritesTheSourcesDeployedDigests is the happy path: production is
// pinned to exactly what staging's latest revision runs, and the stored
// document keeps everything it was authored with.
func TestPromoteWritesTheSourcesDeployedDigests(t *testing.T) {
	c, store := promoteEnvironment(t, stagingRevision(), oneRevision())

	res, err := c.deploy.Promote(context.Background(), connect.NewRequest(promoteRequest()))
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if got := res.Msg.GetFromRevision(); got != "rev-00000007" {
		t.Errorf("from_revision = %q, want the source's latest revision", got)
	}
	web := promoted(res.Msg, "web")
	if web.GetStatus() != kelsonv1alpha1.PromotionStatus_PROMOTION_STATUS_PINNED || web.GetToImage() != stagingWebImage {
		t.Errorf("web: %+v", web)
	}
	if web.GetFromImage() != "" {
		t.Errorf("web was unpinned before the promotion; from_image = %q", web.GetFromImage())
	}
	if got := promoted(res.Msg, "digest").GetToImage(); got != stagingDigestImage {
		t.Errorf("the CronJob component was not promoted: %q", got)
	}

	stored, err := store.Get(context.Background(), "checkout")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(stored.Documents.Environments["production"])
	if !strings.Contains(doc, "image: "+stagingWebImage) {
		t.Errorf("the pin is not in the stored document:\n%s", doc)
	}
	if !strings.Contains(doc, "replicas: { min: 3, max: 20 }   # sized for the front door") {
		t.Errorf("the authored line and its comment did not survive:\n%s", doc)
	}
	if !strings.Contains(doc, "# Production is deliberately conservative;") {
		t.Errorf("the document's comments did not survive:\n%s", doc)
	}
	if res.Msg.GetVersion() == "" {
		t.Error("the response carries no spec version after a write")
	}
}

// TestPromoteReturnsTheResultingDiff: the response is the answer to "what will
// this change", not a promise that a second call will tell you.
func TestPromoteReturnsTheResultingDiff(t *testing.T) {
	c, _ := promoteEnvironment(t, stagingRevision(), oneRevision())

	res, err := c.deploy.Promote(context.Background(), connect.NewRequest(promoteRequest()))
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if len(res.Msg.GetDiffJson()) == 0 {
		t.Fatal("the promotion returned no diff")
	}
	var d diff.Diff
	if err := json.Unmarshal(res.Msg.GetDiffJson(), &d); err != nil {
		t.Fatalf("decoding diff_json: %v", err)
	}
	if res.Msg.GetExitSemantics() != exitSemanticsDiff {
		t.Errorf("exit_semantics = %d, want %d (the promotion changes something)", res.Msg.GetExitSemantics(), exitSemanticsDiff)
	}
	if !strings.Contains(string(res.Msg.GetDiffJson()), stagingWebImage) {
		t.Errorf("the diff does not mention the promoted image")
	}
}

// TestPromoteDryRunTouchesNothing: RENDER computes the whole answer and stores
// none of it.
func TestPromoteDryRunTouchesNothing(t *testing.T) {
	c, store := promoteEnvironment(t, stagingRevision(), oneRevision())
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
	if promoted(res.Msg, "web").GetToImage() != stagingWebImage {
		t.Error("a dry run must still say what it would pin")
	}
	if len(res.Msg.GetDiffJson()) == 0 {
		t.Error("a dry run must still return the diff it previews")
	}
	if res.Msg.GetVersion() != "" {
		t.Errorf("a dry run reported a stored version %q", res.Msg.GetVersion())
	}

	after, err := store.Get(context.Background(), "checkout")
	if err != nil {
		t.Fatal(err)
	}
	if after.Version != before.Version {
		t.Errorf("the store moved from version %q to %q during a dry run", before.Version, after.Version)
	}
	if string(after.Documents.Environments["production"]) != promoteProductionDoc {
		t.Error("a dry run rewrote the stored document")
	}
}

// TestPromoteRefusesWhenNothingHasBeenDeployedToTheSource, with the structured
// code an agent branches on.
func TestPromoteRefusesWhenNothingHasBeenDeployedToTheSource(t *testing.T) {
	c, store := promoteEnvironment(t, nil, nil)

	_, err := c.deploy.Promote(context.Background(), connect.NewRequest(promoteRequest()))
	if err == nil {
		t.Fatal("promoting from an environment with no history succeeded")
	}
	if got := connect.CodeOf(err); got != connect.CodeFailedPrecondition {
		t.Errorf("code = %s, want failed_precondition", got)
	}
	if code := detailCode(t, err); code != "promote/nothing-deployed" {
		t.Errorf("detail code = %q", code)
	}
	stored, _ := store.Get(context.Background(), "checkout")
	if string(stored.Documents.Environments["production"]) != promoteProductionDoc {
		t.Error("a refused promotion still wrote to the store")
	}
}

// TestPromoteReportsAnAlreadyPinnedComponentAsANoOp.
func TestPromoteReportsAnAlreadyPinnedComponentAsANoOp(t *testing.T) {
	c, store := promoteEnvironment(t, stagingRevision(), oneRevision())
	if _, err := c.deploy.Promote(context.Background(), connect.NewRequest(promoteRequest())); err != nil {
		t.Fatalf("first Promote: %v", err)
	}
	first, _ := store.Get(context.Background(), "checkout")

	res, err := c.deploy.Promote(context.Background(), connect.NewRequest(promoteRequest()))
	if err != nil {
		t.Fatalf("second Promote: %v", err)
	}
	for _, component := range []string{"web", "digest"} {
		if got := promoted(res.Msg, component).GetStatus(); got != kelsonv1alpha1.PromotionStatus_PROMOTION_STATUS_UNCHANGED {
			t.Errorf("%s: status = %s, want unchanged", component, got)
		}
	}
	if res.Msg.GetExitSemantics() != exitSemanticsClean {
		t.Errorf("exit_semantics = %d, want %d for a promotion that changes nothing", res.Msg.GetExitSemantics(), exitSemanticsClean)
	}
	second, _ := store.Get(context.Background(), "checkout")
	if string(second.Documents.Environments["production"]) != string(first.Documents.Environments["production"]) {
		t.Error("a no-op promotion rewrote the document")
	}
}

// TestPromoteRefusesAnUnknownComponent.
func TestPromoteRefusesAnUnknownComponent(t *testing.T) {
	c, _ := promoteEnvironment(t, stagingRevision(), oneRevision())
	req := promoteRequest()
	req.Components = []string{"webb"}

	_, err := c.deploy.Promote(context.Background(), connect.NewRequest(req))
	if err == nil {
		t.Fatal("an unknown component filter succeeded")
	}
	if code := detailCode(t, err); code != "promote/component-unknown" {
		t.Errorf("detail code = %q", code)
	}
}

// TestPromoteSkipsAComponentTheRevisionDoesNotCarry: the CronJob is missing
// from the recorded revision, so it is reported as skipped with a reason and
// the Deployment still promotes.
func TestPromoteSkipsAComponentTheRevisionDoesNotCarry(t *testing.T) {
	c, _ := promoteEnvironment(t, stagingRevision()[:1], oneRevision())

	res, err := c.deploy.Promote(context.Background(), connect.NewRequest(promoteRequest()))
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	digest := promoted(res.Msg, "digest")
	if digest.GetStatus() != kelsonv1alpha1.PromotionStatus_PROMOTION_STATUS_SKIPPED {
		t.Fatalf("digest: %+v", digest)
	}
	if digest.GetCode() != "promote/not-in-revision" || digest.GetReason() == "" {
		t.Errorf("a skip with no code or reason: %+v", digest)
	}
	if digest.GetToImage() != "" {
		t.Errorf("a skipped component carried an image: %q", digest.GetToImage())
	}
	if promoted(res.Msg, "web").GetStatus() != kelsonv1alpha1.PromotionStatus_PROMOTION_STATUS_PINNED {
		t.Error("one component's skip stopped another's promotion")
	}
}

// TestPromoteSurfacesAVersionConflict: the caller asserted a version the store
// has moved past, and the promotion refuses rather than clobbering.
func TestPromoteSurfacesAVersionConflict(t *testing.T) {
	c, _ := promoteEnvironment(t, stagingRevision(), oneRevision())
	req := promoteRequest()
	req.Version = "not-the-stored-version"

	_, err := c.deploy.Promote(context.Background(), connect.NewRequest(req))
	if err == nil {
		t.Fatal("a stale version was accepted")
	}
	if got := connect.CodeOf(err); got != connect.CodeFailedPrecondition {
		t.Errorf("code = %s, want failed_precondition", got)
	}
	if code := detailCode(t, err); code != "store/version-conflict" {
		t.Errorf("detail code = %q", code)
	}
}

// TestPromoteRefusesAnEnvironmentPromotedToItself.
func TestPromoteRefusesAnEnvironmentPromotedToItself(t *testing.T) {
	c, _ := promoteEnvironment(t, stagingRevision(), oneRevision())
	req := promoteRequest()
	req.ToEnvironment = "staging"

	if _, err := c.deploy.Promote(context.Background(), connect.NewRequest(req)); err == nil {
		t.Fatal("promoting an environment to itself succeeded")
	}
}

// TestPromoteHonoursTheComponentFilter.
func TestPromoteHonoursTheComponentFilter(t *testing.T) {
	c, store := promoteEnvironment(t, stagingRevision(), oneRevision())
	req := promoteRequest()
	req.Components = []string{"web"}

	res, err := c.deploy.Promote(context.Background(), connect.NewRequest(req))
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if len(res.Msg.GetComponents()) != 1 {
		t.Fatalf("the filter was not applied: %+v", res.Msg.GetComponents())
	}
	stored, _ := store.Get(context.Background(), "checkout")
	if strings.Contains(string(stored.Documents.Environments["production"]), stagingDigestImage) {
		t.Error("a filtered-out component was pinned anyway")
	}
}
