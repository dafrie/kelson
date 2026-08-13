package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/clusterprofile"
	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/diff"
	"github.com/dafrie/kelson/internal/serverstate"
)

// The fixture is examples/hello-single, inlined so the test states exactly what
// it renders. Its image is set explicitly (a spec that builds from source has
// none until a build produces one, #136) and it carries no fields the model
// gates as not-implemented (#141).
const (
	projectDoc = `apiVersion: kelson.dev/v1alpha1
kind: Project
metadata:
  name: hello
spec:
  image: ghcr.io/acme/hello:1.4.2
  env:
    LOG_LEVEL: info
  applications:
    - name: web
      port: 8080
      health: /healthz
`

	developmentDoc = `apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata:
  name: development
spec:
  project: hello
  routing:
    domainSuffix: dev.acme.run
`

	// The same project one image later: the diff between the two renders is a
	// single container image change.
	projectDocV2 = `apiVersion: kelson.dev/v1alpha1
kind: Project
metadata:
  name: hello
spec:
  image: ghcr.io/acme/hello:1.5.0
  env:
    LOG_LEVEL: info
  applications:
    - name: web
      port: 8080
      health: /healthz
`

	// gatewayProfile is the minimum a spec with domains needs: since #140
	// kelson renders Gateway API only and refuses to fall back to Ingress.
	gatewayProfile = `gatewayAPI:
  version: v1.6.0
  classes: [envoy]
`

	overlayProjectDoc = `apiVersion: kelson.dev/v1alpha1
kind: Project
metadata:
  name: hello
spec:
  image: ghcr.io/acme/hello:1.4.2
  applications:
    - name: web
      port: 8080
  overlays:
    - patch: k8s/patches/affinity.yaml
`
)

func inlineSpec(project string, envs map[string]string) *kelsonv1alpha1.SpecRef {
	return &kelsonv1alpha1.SpecRef{Spec: &kelsonv1alpha1.SpecRef_Documents{Documents: specDocuments(project, envs)}}
}

func specDocuments(project string, envs map[string]string) *kelsonv1alpha1.SpecDocuments {
	docs := &kelsonv1alpha1.SpecDocuments{Project: []byte(project), Environments: map[string][]byte{}}
	for name, body := range envs {
		docs.Environments[name] = []byte(body)
	}
	return docs
}

func profileRef() *kelsonv1alpha1.ProfileRef {
	return &kelsonv1alpha1.ProfileRef{Profile: &kelsonv1alpha1.ProfileRef_Yaml{Yaml: []byte(gatewayProfile)}}
}

// TestRenderInlineSpec is the #139 render smoke test: a known-good spec over
// the wire, manifests back.
func TestRenderInlineSpec(t *testing.T) {
	c := serve(t, Options{})

	res, err := c.render.Render(context.Background(), connect.NewRequest(&kelsonv1alpha1.RenderRequest{
		Spec:        inlineSpec(projectDoc, map[string]string{"development": developmentDoc}),
		Environment: "development",
		Profile:     profileRef(),
	}))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if errs := res.Msg.GetErrors(); len(errs) > 0 {
		t.Fatalf("Render reported errors: %v", errs)
	}

	kinds := map[string]bool{}
	for _, m := range res.Msg.GetManifests() {
		kinds[m.GetKind()] = true
		if len(m.GetYaml()) == 0 {
			t.Errorf("manifest %s/%s carries no YAML", m.GetKind(), m.GetName())
		}
	}
	for _, want := range []string{"Namespace", "Deployment", "Service", "HTTPRoute"} {
		if !kinds[want] {
			t.Errorf("render produced no %s (got %v)", want, kinds)
		}
	}
	// The Namespace leads the set: delivery documents apply order as
	// "namespaces first" and every following resource targets it (#150).
	if first := res.Msg.GetManifests()[0]; first.GetKind() != "Namespace" {
		t.Errorf("first manifest is %s, want Namespace", first.GetKind())
	}
	for _, m := range res.Msg.GetManifests() {
		if m.GetKind() == "Deployment" && !strings.Contains(string(m.GetYaml()), "ghcr.io/acme/hello:1.4.2") {
			t.Errorf("Deployment does not carry the project image:\n%s", m.GetYaml())
		}
	}
}

// TestRenderStoredSpec renders a spec addressed by project name — the store
// path, as opposed to the CLI's inline -f mode.
func TestRenderStoredSpec(t *testing.T) {
	store := newFakeSpecStore()
	if _, err := store.Put(context.Background(), "hello", serverstate.Documents{
		Project:      []byte(projectDoc),
		Environments: map[string][]byte{"development": []byte(developmentDoc)},
	}, serverstate.PutOptions{}); err != nil {
		t.Fatalf("seeding the store: %v", err)
	}
	c := serve(t, Options{Specs: store})

	res, err := c.render.Render(context.Background(), connect.NewRequest(&kelsonv1alpha1.RenderRequest{
		Spec:    &kelsonv1alpha1.SpecRef{Spec: &kelsonv1alpha1.SpecRef_Project{Project: "hello"}},
		Profile: profileRef(),
	}))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(res.Msg.GetManifests()) == 0 {
		t.Fatalf("stored spec rendered nothing (errors: %v)", res.Msg.GetErrors())
	}
}

// TestRenderStoredSpecNotFound checks the store's taxonomy survives the wire:
// store/not-found becomes CodeNotFound with the structured error attached.
func TestRenderStoredSpecNotFound(t *testing.T) {
	c := serve(t, Options{Specs: newFakeSpecStore()})

	_, err := c.render.Render(context.Background(), connect.NewRequest(&kelsonv1alpha1.RenderRequest{
		Spec: &kelsonv1alpha1.SpecRef{Spec: &kelsonv1alpha1.SpecRef_Project{Project: "absent"}},
	}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("code = %v, want NotFound (err %v)", connect.CodeOf(err), err)
	}
	if got := detailCode(t, err); got != string(serverstate.ErrNotFound) {
		t.Errorf("detail code = %q, want %q", got, serverstate.ErrNotFound)
	}
}

// TestRenderInvalidSpec: an invalid spec is an answer. The RPC succeeds and the
// findings travel in the response, with the model taxonomy's codes verbatim.
func TestRenderInvalidSpec(t *testing.T) {
	c := serve(t, Options{})

	broken := strings.Replace(projectDoc, "      health: /healthz", "      healthz: /healthz", 1)
	res, err := c.render.Render(context.Background(), connect.NewRequest(&kelsonv1alpha1.RenderRequest{
		Spec:        inlineSpec(broken, map[string]string{"development": developmentDoc}),
		Environment: "development",
		Profile:     profileRef(),
	}))
	if err != nil {
		t.Fatalf("Render should answer, not fail: %v", err)
	}
	if len(res.Msg.GetManifests()) != 0 {
		t.Errorf("an invalid spec produced %d manifests", len(res.Msg.GetManifests()))
	}
	if len(res.Msg.GetErrors()) == 0 {
		t.Fatal("an invalid spec reported no errors")
	}
	for _, e := range res.Msg.GetErrors() {
		if !strings.Contains(e.GetCode(), "/") {
			t.Errorf("error code %q is not a slash-separated taxonomy code", e.GetCode())
		}
	}
}

// TestRenderRefusesOverlays: overlay paths resolve against the authoring
// documents, and a spec that arrived over the API has none. The refusal must be
// structural and named rather than a silently overlay-free render.
func TestRenderRefusesOverlays(t *testing.T) {
	c := serve(t, Options{})

	res, err := c.render.Render(context.Background(), connect.NewRequest(&kelsonv1alpha1.RenderRequest{
		Spec:        inlineSpec(overlayProjectDoc, map[string]string{"development": developmentDoc}),
		Environment: "development",
		Profile:     profileRef(),
	}))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(res.Msg.GetManifests()) != 0 {
		t.Fatalf("a spec with overlays rendered %d manifests instead of failing", len(res.Msg.GetManifests()))
	}
	errs := res.Msg.GetErrors()
	if len(errs) != 1 {
		t.Fatalf("errors = %d, want 1: %v", len(errs), errs)
	}
	if errs[0].GetCode() != "overlay/load" {
		t.Errorf("code = %q, want overlay/load", errs[0].GetCode())
	}
	if errs[0].GetMessage() != "overlays are not supported over the API yet" {
		t.Errorf("message = %q", errs[0].GetMessage())
	}
	if errs[0].GetOverlay() != "k8s/patches/affinity.yaml" {
		t.Errorf("overlay = %q, want the declared path", errs[0].GetOverlay())
	}
}

// TestDiffRenderMode is the #139 diff smoke test: two document sets, both
// rendered server-side, compared offline.
func TestDiffRenderMode(t *testing.T) {
	c := serve(t, Options{})

	res, err := c.render.Diff(context.Background(), connect.NewRequest(&kelsonv1alpha1.DiffRequest{
		Spec:        inlineSpec(projectDocV2, map[string]string{"development": developmentDoc}),
		Environment: "development",
		Profile:     profileRef(),
		From:        specDocuments(projectDoc, map[string]string{"development": developmentDoc}),
		DryRun:      kelsonv1alpha1.DryRun_DRY_RUN_RENDER,
	}))
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if got := res.Msg.GetExitSemantics(); got != exitSemanticsDiff {
		t.Errorf("exit_semantics = %d, want %d (differences present)", got, exitSemanticsDiff)
	}

	var decoded struct {
		Level     string `json:"level"`
		Resources []struct {
			Kind string `json:"kind"`
			Op   string `json:"op"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(res.Msg.GetDiffJson(), &decoded); err != nil {
		t.Fatalf("diff_json is not the canonical encoding: %v", err)
	}
	if len(decoded.Resources) == 0 {
		t.Fatalf("diff reported no changed resources: %s", res.Msg.GetDiffJson())
	}
	found := false
	for _, r := range decoded.Resources {
		if r.Kind == "Deployment" && r.Op == "modified" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a modified Deployment, got %+v", decoded.Resources)
	}
}

// TestDiffNoFromIsAllAdditions: with no `from` there is no prior state, so
// every current resource is an addition — the honest answer when nothing has
// been recorded (the CLI's rule for a --from-less diff).
func TestDiffNoFrom(t *testing.T) {
	c := serve(t, Options{})

	res, err := c.render.Diff(context.Background(), connect.NewRequest(&kelsonv1alpha1.DiffRequest{
		Spec:        inlineSpec(projectDoc, map[string]string{"development": developmentDoc}),
		Environment: "development",
		Profile:     profileRef(),
	}))
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if got := res.Msg.GetExitSemantics(); got != exitSemanticsDiff {
		t.Errorf("exit_semantics = %d, want %d", got, exitSemanticsDiff)
	}
	if !strings.Contains(string(res.Msg.GetDiffJson()), `"op": "added"`) {
		t.Errorf("expected additions in the diff:\n%s", res.Msg.GetDiffJson())
	}
}

// recordAs renders a spec through the API and returns the manifests as a
// recorded revision would hold them. A recorded revision IS a past render's
// bytes (#38), so building the fixture this way keeps the before side of a
// from_revision diff the shape the delivery history actually stores.
func recordAs(t *testing.T, c clients, project string) []delivery.Manifest {
	t.Helper()
	res, err := c.render.Render(context.Background(), connect.NewRequest(&kelsonv1alpha1.RenderRequest{
		Spec:        inlineSpec(project, map[string]string{"development": developmentDoc}),
		Environment: "development",
		Profile:     profileRef(),
	}))
	if err != nil {
		t.Fatalf("Render (building the recorded fixture): %v", err)
	}
	out := make([]delivery.Manifest, 0, len(res.Msg.GetManifests()))
	for _, m := range res.Msg.GetManifests() {
		out = append(out, delivery.Manifest{
			APIVersion: m.GetApiVersion(),
			Kind:       m.GetKind(),
			Name:       m.GetName(),
			Namespace:  m.GetNamespace(),
			YAML:       m.GetYaml(),
		})
	}
	return out
}

// TestDiffAgainstDeployedRevision is #162: the stored-spec answer to `--from`.
// The before side is what revision N actually rendered, read from the delivery
// history, and the after side is today's render — so the single image change
// between the two shows up as a modified Deployment and nothing is reported as
// an addition.
func TestDiffAgainstDeployedRevision(t *testing.T) {
	adapter := newFakeAdapter("direct")
	adapter.history = []delivery.Entry{{Revision: "rev-00000001", SpecHash: "sha256:a"}}
	recorded := &fakeRecorded{revisions: map[string][]delivery.Manifest{}}
	connector, _ := connectorFor(adapter, recorded, nil)
	c := serve(t, Options{Delivery: connector})
	recorded.revisions["rev-00000001"] = recordAs(t, c, projectDoc)

	res, err := c.render.Diff(context.Background(), connect.NewRequest(&kelsonv1alpha1.DiffRequest{
		Spec:         inlineSpec(projectDocV2, map[string]string{"development": developmentDoc}),
		Environment:  "development",
		Profile:      profileRef(),
		FromRevision: "rev-00000001",
	}))
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if got := res.Msg.GetExitSemantics(); got != exitSemanticsDiff {
		t.Errorf("exit_semantics = %d, want %d (differences present)", got, exitSemanticsDiff)
	}

	var decoded struct {
		Resources []struct {
			Kind string `json:"kind"`
			Op   string `json:"op"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(res.Msg.GetDiffJson(), &decoded); err != nil {
		t.Fatalf("diff_json is not the canonical encoding: %v", err)
	}
	if len(decoded.Resources) != 1 {
		t.Fatalf("resources = %+v, want only the changed Deployment: a recorded revision is prior state, not an empty one", decoded.Resources)
	}
	if decoded.Resources[0].Kind != "Deployment" || decoded.Resources[0].Op != "modified" {
		t.Errorf("resource = %+v, want a modified Deployment", decoded.Resources[0])
	}
}

// TestDiffFromAndFromRevisionRejected: two answers to "compare against what",
// and no written-down precedence between them. The request is refused rather
// than silently resolved.
func TestDiffFromAndFromRevisionRejected(t *testing.T) {
	adapter := newFakeAdapter("direct")
	connector, _ := connectorFor(adapter, &fakeRecorded{}, nil)
	c := serve(t, Options{Delivery: connector})

	_, err := c.render.Diff(context.Background(), connect.NewRequest(&kelsonv1alpha1.DiffRequest{
		Spec:         inlineSpec(projectDocV2, map[string]string{"development": developmentDoc}),
		Environment:  "development",
		Profile:      profileRef(),
		From:         specDocuments(projectDoc, map[string]string{"development": developmentDoc}),
		FromRevision: "rev-00000001",
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument (err %v)", connect.CodeOf(err), err)
	}
	for _, call := range adapter.callLog() {
		t.Fatalf("a rejected request reached the adapter: %v", call)
	}
}

// TestDiffFromRevisionServerRejected: a server dry-run is the live cluster's
// verdict on the current set. It has no before side a revision could occupy, so
// asking for both is refused rather than answered with the one it can do.
func TestDiffFromRevisionServerRejected(t *testing.T) {
	preview := &fakePreview{diff: &diff.Diff{Level: diff.LevelServer}}
	connector, _ := connectorFor(newFakeAdapter("direct"), &fakeRecorded{}, nil)
	c := serve(t, Options{Delivery: connector, Preview: previewConnector(preview)})

	_, err := c.render.Diff(context.Background(), connect.NewRequest(&kelsonv1alpha1.DiffRequest{
		Spec:         inlineSpec(projectDoc, map[string]string{"development": developmentDoc}),
		Environment:  "development",
		Profile:      profileRef(),
		FromRevision: "rev-00000001",
		DryRun:       kelsonv1alpha1.DryRun_DRY_RUN_SERVER,
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument (err %v)", connect.CodeOf(err), err)
	}
	if len(preview.sets) != 0 {
		t.Errorf("the dry-run engine was called %d times for a refused request", len(preview.sets))
	}
}

// TestDiffFromRevisionUnknown: a revision that is not in the recorded history
// is not-found, and the answer says so in every mode — the revision is checked
// against the adapter's history before the source is read, so the code does not
// depend on which store the server was started with.
func TestDiffFromRevisionUnknown(t *testing.T) {
	adapter := newFakeAdapter("direct")
	adapter.history = []delivery.Entry{{Revision: "rev-00000002"}}
	connector, _ := connectorFor(adapter, &fakeRecorded{revisions: map[string][]delivery.Manifest{}}, nil)
	c := serve(t, Options{Delivery: connector})

	_, err := c.render.Diff(context.Background(), connect.NewRequest(&kelsonv1alpha1.DiffRequest{
		Spec:         inlineSpec(projectDoc, map[string]string{"development": developmentDoc}),
		Environment:  "development",
		Profile:      profileRef(),
		FromRevision: "rev-00000404",
	}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("code = %v, want NotFound (err %v)", connect.CodeOf(err), err)
	}
	if !strings.Contains(err.Error(), "rev-00000404") {
		t.Errorf("error does not name the revision asked for: %v", err)
	}
}

// TestDiffFromRevisionWithoutRecordedHistory: a mode whose rendered history
// kelson cannot read has no revision to compare against, and the refusal names
// the rung that still works rather than downgrading to one silently.
func TestDiffFromRevisionWithoutRecordedHistory(t *testing.T) {
	adapter := newFakeAdapter("direct")
	adapter.history = []delivery.Entry{{Revision: "rev-00000001"}}
	connector, _ := connectorFor(adapter, nil, nil)
	c := serve(t, Options{Delivery: connector})

	_, err := c.render.Diff(context.Background(), connect.NewRequest(&kelsonv1alpha1.DiffRequest{
		Spec:         inlineSpec(projectDoc, map[string]string{"development": developmentDoc}),
		Environment:  "development",
		Profile:      profileRef(),
		FromRevision: "rev-00000001",
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument (err %v)", connect.CodeOf(err), err)
	}
	if !strings.Contains(err.Error(), "dry_run=SERVER") {
		t.Errorf("the refusal does not name the rung that still works: %v", err)
	}
}

// TestDiffServerMode drives the L2 seam and asserts the blocked verdict travels
// as exit_semantics 3 — the CI contract `kelson diff` exits with.
func TestDiffServerMode(t *testing.T) {
	preview := &fakePreview{diff: &diff.Diff{
		Level:     diff.LevelServer,
		Resources: []diff.ResourceDiff{{Kind: "Deployment", Name: "web", Op: diff.OpModified}},
		Unvalidated: []diff.Unvalidated{{
			Resource: "gateway.networking.k8s.io/v1/HTTPRoute//web",
			Requires: "CustomResourceDefinition/httproutes.gateway.networking.k8s.io",
			InBatch:  false,
		}},
	}}
	c := serve(t, Options{Preview: previewConnector(preview)})

	res, err := c.render.Diff(context.Background(), connect.NewRequest(&kelsonv1alpha1.DiffRequest{
		Spec:        inlineSpec(projectDoc, map[string]string{"development": developmentDoc}),
		Environment: "development",
		Profile:     profileRef(),
		DryRun:      kelsonv1alpha1.DryRun_DRY_RUN_SERVER,
	}))
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if got := res.Msg.GetExitSemantics(); got != exitSemanticsBlocked {
		t.Errorf("exit_semantics = %d, want %d (a prerequisite that is genuinely absent blocks)", got, exitSemanticsBlocked)
	}
	if len(preview.sets) != 1 {
		t.Fatalf("the preview engine was called %d times", len(preview.sets))
	}
	if preview.sets[0].SpecHash == "" {
		t.Error("the previewed set carries no spec hash provenance")
	}
}

// TestDiffServerModeNeverDowngrades: an unreachable cluster is a real error. A
// requested server-side preview must never silently become a rendered one — a
// CI gate would get a clean answer it did not earn.
func TestDiffServerModeNeverDowngrades(t *testing.T) {
	c := serve(t, Options{Preview: func(context.Context, clusterprofile.ClusterProfile) (PreviewEngine, error) {
		return nil, errors.New("no usable cluster credentials")
	}})

	_, err := c.render.Diff(context.Background(), connect.NewRequest(&kelsonv1alpha1.DiffRequest{
		Spec:        inlineSpec(projectDoc, map[string]string{"development": developmentDoc}),
		Environment: "development",
		Profile:     profileRef(),
		DryRun:      kelsonv1alpha1.DryRun_DRY_RUN_SERVER,
	}))
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("code = %v, want Unavailable (err %v)", connect.CodeOf(err), err)
	}
}

// TestSelectEnvironmentAmbiguous: an unnamed environment is only unambiguous
// when the spec holds one, matching the CLI's --env rule.
func TestSelectEnvironmentAmbiguous(t *testing.T) {
	c := serve(t, Options{})

	staging := strings.ReplaceAll(developmentDoc, "development", "staging")
	_, err := c.render.Render(context.Background(), connect.NewRequest(&kelsonv1alpha1.RenderRequest{
		Spec:    inlineSpec(projectDoc, map[string]string{"development": developmentDoc, "staging": staging}),
		Profile: profileRef(),
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument (err %v)", connect.CodeOf(err), err)
	}
	if !strings.Contains(err.Error(), "several environments") {
		t.Errorf("message does not name the ambiguity: %v", err)
	}
}

// detailCode reads the structured code off the first error detail.
func detailCode(t *testing.T, err error) string {
	t.Helper()
	var cerr *connect.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("not a connect error: %v", err)
	}
	for _, d := range cerr.Details() {
		msg, verr := d.Value()
		if verr != nil {
			continue
		}
		if wire, ok := msg.(*kelsonv1alpha1.Error); ok {
			return wire.GetCode()
		}
	}
	t.Fatalf("no kelson.v1alpha1.Error detail on %v", err)
	return ""
}
