package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/artifact"
	"github.com/dafrie/kelson/internal/clusterprofile"
	"github.com/dafrie/kelson/internal/controlstore"
	"github.com/dafrie/kelson/internal/diff"
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
  components:
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
  components:
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
  components:
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

// TestRenderDefaultsToTheServersOwnProfile: a request that names no profile is
// answered against the cluster this server is attached to, because that is the
// cluster kelson-controller will render the same spec against. Rendering it
// against "nothing detected" instead refused a routed spec with
// render/gateway-api-missing on a cluster that has Gateway API, and made the
// resource count in `kelson deploy`'s confirmation prompt disagree with what
// the controller publishes.
func TestRenderDefaultsToTheServersOwnProfile(t *testing.T) {
	captured, err := clusterprofile.Unmarshal([]byte(gatewayProfile))
	if err != nil {
		t.Fatal(err)
	}
	c := serve(t, Options{Profile: fakeProfile(captured)})

	res, err := c.render.Render(context.Background(), connect.NewRequest(&kelsonv1alpha1.RenderRequest{
		Spec:        inlineSpec(projectDoc, map[string]string{"development": developmentDoc}),
		Environment: "development",
		// No Profile: the ordinary request, and what the CLI sends unless
		// --profile was given.
	}))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if errs := res.Msg.GetErrors(); len(errs) > 0 {
		t.Fatalf("Render reported errors against the server's own profile: %v", errs)
	}
	var kinds []string
	for _, m := range res.Msg.GetManifests() {
		kinds = append(kinds, m.GetKind())
	}
	if !slices.Contains(kinds, "HTTPRoute") {
		t.Errorf("kinds = %v, want an HTTPRoute: the detected profile has Gateway API", kinds)
	}
}

// TestRenderExplicitProfileStillWins: the default is a default. A request that
// names a profile is answered against that one, including the explicit
// `from_cluster: false` that asks for nothing detected.
func TestRenderExplicitProfileStillWins(t *testing.T) {
	captured, err := clusterprofile.Unmarshal([]byte(gatewayProfile))
	if err != nil {
		t.Fatal(err)
	}
	c := serve(t, Options{Profile: fakeProfile(captured)})

	res, err := c.render.Render(context.Background(), connect.NewRequest(&kelsonv1alpha1.RenderRequest{
		Spec:        inlineSpec(projectDoc, map[string]string{"development": developmentDoc}),
		Environment: "development",
		Profile:     &kelsonv1alpha1.ProfileRef{Profile: &kelsonv1alpha1.ProfileRef_FromCluster{FromCluster: false}},
	}))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(res.Msg.GetErrors()) != 1 || res.Msg.GetErrors()[0].GetCode() != "render/gateway-api-missing" {
		t.Fatalf("errors = %v, want one render/gateway-api-missing: the request asked for the zero profile",
			res.Msg.GetErrors())
	}
}

// TestRenderWithoutACaptureSeamKeepsTheZeroProfile: a server started with no
// profile capture (a test, a build with no cluster) still answers rather than
// refusing a request that named no profile — those callers had nothing better
// before the default existed either.
func TestRenderWithoutACaptureSeamKeepsTheZeroProfile(t *testing.T) {
	c := serve(t, Options{})

	res, err := c.render.Render(context.Background(), connect.NewRequest(&kelsonv1alpha1.RenderRequest{
		Spec:        inlineSpec(projectDoc, map[string]string{"development": developmentDoc}),
		Environment: "development",
	}))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(res.Msg.GetErrors()) != 1 || res.Msg.GetErrors()[0].GetCode() != "render/gateway-api-missing" {
		t.Fatalf("errors = %v, want one render/gateway-api-missing", res.Msg.GetErrors())
	}
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
	if _, err := store.Put(context.Background(), "hello", controlstore.Documents{
		Project:      []byte(projectDoc),
		Environments: map[string][]byte{"development": []byte(developmentDoc)},
	}, controlstore.PutOptions{}); err != nil {
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
	if got := detailCode(t, err); got != string(controlstore.ErrNotFound) {
		t.Errorf("detail code = %q, want %q", got, controlstore.ErrNotFound)
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

// The revision half of Diff (issue #247, #162's question finally answered):
// the before side is the bytes revision N actually published, pulled out of the
// registry, and never the old spec re-rendered — which would answer what that
// spec produces under today's renderer and ClusterProfile instead.

// fakeArtifacts is the registry's record with its bytes: a [RevisionLister]
// that is also a [RevisionFetcher].
type fakeArtifacts struct {
	// files is what each revision recorded, keyed by revision.
	files map[string][]artifact.File
	// digests is what each revision's artifact is pinned to, and asked records
	// the digest each Fetch was given — which is how "the recorded digest
	// travelled" is asserted rather than assumed.
	digests map[string]string
	asked   map[string]string
	err     error
}

func newFakeArtifacts() *fakeArtifacts {
	return &fakeArtifacts{
		files:   map[string][]artifact.File{},
		digests: map[string]string{},
		asked:   map[string]string{},
	}
}

func (f *fakeArtifacts) Revisions(context.Context, string, string) ([]string, error) {
	revisions := make([]string, 0, len(f.files))
	for revision := range f.files {
		revisions = append(revisions, revision)
	}
	slices.Sort(revisions)
	return revisions, f.err
}

func (f *fakeArtifacts) Resolve(_ context.Context, _, _, revision string) (string, bool, error) {
	if f.err != nil {
		return "", false, f.err
	}
	_, ok := f.files[revision]
	return f.digests[revision], ok, nil
}

func (f *fakeArtifacts) Fetch(_ context.Context, _, _, revision, digest string) (artifact.Pulled, bool, error) {
	f.asked[revision] = digest
	if f.err != nil {
		return artifact.Pulled{}, false, f.err
	}
	files, ok := f.files[revision]
	if !ok {
		return artifact.Pulled{}, false, nil
	}
	return artifact.Pulled{Digest: f.digests[revision], Files: files}, true, nil
}

// recordedSet is a published revision's flat directory of rendered manifests,
// in the shape internal/artifact's ManifestFiles writes it.
func recordedSet(docs ...string) []artifact.File {
	files := make([]artifact.File, 0, len(docs))
	for i, doc := range docs {
		files = append(files, artifact.File{Path: fmt.Sprintf("%03d-resource.yaml", i+1), Data: []byte(doc)})
	}
	return files
}

const retiredNamespace = `apiVersion: v1
kind: Namespace
metadata:
  name: retired
`

// TestDiffAgainstARevisionComparesItsRecordedBytes: the comparison is against
// what that revision rendered, so a resource it held and the current render
// does not is a removal — a fact no re-render of the old spec could produce.
func TestDiffAgainstARevisionComparesItsRecordedBytes(t *testing.T) {
	record := newFakeArtifacts()
	record.files["7-a1b2c3d4"] = recordedSet(retiredNamespace)
	c := serve(t, Options{Revisions: record})

	res, err := c.render.Diff(context.Background(), connect.NewRequest(&kelsonv1alpha1.DiffRequest{
		Spec:         inlineSpec(projectDoc, map[string]string{"development": developmentDoc}),
		Environment:  "development",
		Profile:      profileRef(),
		FromRevision: "7-a1b2c3d4",
	}))
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if got := res.Msg.GetExitSemantics(); got != exitSemanticsDiff {
		t.Errorf("exit_semantics = %d, want %d", got, exitSemanticsDiff)
	}
	body := string(res.Msg.GetDiffJson())
	if !strings.Contains(body, `"op": "removed"`) || !strings.Contains(body, `"name": "retired"`) {
		t.Errorf("the recorded revision's own resource is not reported as removed:\n%s", body)
	}
	if !strings.Contains(body, `"level": "rendered"`) {
		t.Errorf("level = not rendered, and a revision comparison is offline:\n%s", body)
	}
}

// A revision that has aged out of the twenty-entry mirror is still in the
// registry, and the diff serves it — the same posture rollback takes (ADR-0028
// decision 4). The mirror is consulted for the digest, never for permission.
func TestDiffAgainstARevisionTheMirrorHasForgotten(t *testing.T) {
	record := newFakeArtifacts()
	record.files["1-0badc0de"] = recordedSet(retiredNamespace)
	c := serve(t, Options{Revisions: record})

	if _, err := c.render.Diff(context.Background(), connect.NewRequest(&kelsonv1alpha1.DiffRequest{
		Spec:         inlineSpec(projectDoc, map[string]string{"development": developmentDoc}),
		Environment:  "development",
		Profile:      profileRef(),
		FromRevision: "1-0badc0de",
	})); err != nil {
		t.Fatalf("Diff against an aged-out revision: %v", err)
	}
	if digest, ok := record.asked["1-0badc0de"]; !ok || digest != "" {
		t.Errorf("fetched with digest %q, want the pull to proceed with no recorded digest to cross-check", digest)
	}
}

// Inside the window the mirror knows what bytes that revision was published as,
// and the pull is given them: a tag is a pointer and a digest is the content,
// so checking the second against the first is what makes this a comparison
// against what was published rather than against whatever the tag names today
// (ADR-0028 decision 4).
func TestDiffAgainstARevisionCarriesTheRecordedDigest(t *testing.T) {
	record := newFakeArtifacts()
	record.files["3-9f0a1b2c"] = recordedSet(retiredNamespace)
	c := serve(t, Options{Environments: newFakeEnvironments(twoRevisions()), Revisions: record})

	if _, err := c.render.Diff(context.Background(), connect.NewRequest(&kelsonv1alpha1.DiffRequest{
		Spec:         inlineSpec(projectDoc, map[string]string{"development": developmentDoc}),
		Environment:  "development",
		Profile:      profileRef(),
		FromRevision: "3-9f0a1b2c",
	})); err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if got := record.asked["3-9f0a1b2c"]; got != "sha256:deadbeef" {
		t.Errorf("fetched with digest %q, want the one status.history recorded", got)
	}
}

// Something that is not a revision at all is refused before the registry is
// asked, naming the grammar rather than the absence.
func TestDiffAgainstSomethingThatIsNotARevision(t *testing.T) {
	record := newFakeArtifacts()
	c := serve(t, Options{Revisions: record})

	_, err := c.render.Diff(context.Background(), connect.NewRequest(&kelsonv1alpha1.DiffRequest{
		Spec:         inlineSpec(projectDoc, map[string]string{"development": developmentDoc}),
		Environment:  "development",
		Profile:      profileRef(),
		FromRevision: "main",
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("code = %v, want invalid_argument (%v)", connect.CodeOf(err), err)
	}
	if len(record.asked) != 0 {
		t.Errorf("the registry was asked about %v, want nothing", record.asked)
	}
}

// A revision in neither the mirror nor the registry is the caller's argument
// being wrong, and the refusal names where the rest of the record is.
func TestDiffAgainstAnUnknownRevisionIsInvalidArgument(t *testing.T) {
	c := serve(t, Options{Revisions: newFakeArtifacts()})

	_, err := c.render.Diff(context.Background(), connect.NewRequest(&kelsonv1alpha1.DiffRequest{
		Spec:         inlineSpec(projectDoc, map[string]string{"development": developmentDoc}),
		Environment:  "development",
		Profile:      profileRef(),
		FromRevision: "9-deadbeef",
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("code = %v, want invalid_argument (%v)", connect.CodeOf(err), err)
	}
	if !strings.Contains(err.Error(), "9-deadbeef") {
		t.Errorf("the refusal does not name the revision: %v", err)
	}
}

// A registry that could not be read is this server's dependency failing.
// Reporting it as an invalid argument would tell a caller their revision is
// gone on the strength of a registry that never answered.
func TestDiffAgainstARevisionIsUnavailableWhenTheRegistryIsNot(t *testing.T) {
	record := newFakeArtifacts()
	record.err = errors.New("dial tcp: no route to host")
	c := serve(t, Options{Revisions: record})

	_, err := c.render.Diff(context.Background(), connect.NewRequest(&kelsonv1alpha1.DiffRequest{
		Spec:         inlineSpec(projectDoc, map[string]string{"development": developmentDoc}),
		Environment:  "development",
		Profile:      profileRef(),
		FromRevision: "7-a1b2c3d4",
	}))
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("code = %v, want unavailable (%v)", connect.CodeOf(err), err)
	}
}

// Bytes that do not match the digest naming them are neither a bad request nor
// a transient failure: no diff may be computed from them, and the refusal names
// both digests so the reader can see which two values disagree.
func TestDiffAgainstARevisionRefusesUnverifiedBytes(t *testing.T) {
	record := newFakeArtifacts()
	record.err = &artifact.IntegrityError{
		Doing:  "pulling ghcr.io/acme/kelson/hello-development:7-a1b2c3d4",
		Want:   "sha256:recorded",
		Got:    "sha256:served",
		Source: "the digest kelson recorded for this revision",
	}
	c := serve(t, Options{Revisions: record})

	_, err := c.render.Diff(context.Background(), connect.NewRequest(&kelsonv1alpha1.DiffRequest{
		Spec:         inlineSpec(projectDoc, map[string]string{"development": developmentDoc}),
		Environment:  "development",
		Profile:      profileRef(),
		FromRevision: "7-a1b2c3d4",
	}))
	if connect.CodeOf(err) != connect.CodeDataLoss {
		t.Fatalf("code = %v, want data_loss (%v)", connect.CodeOf(err), err)
	}
	for _, want := range []string{"sha256:recorded", "sha256:served"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %s: %v", want, err)
		}
	}
}

// A server with no registry says so, and says it as Unimplemented: a seam this
// build was not wired with is not a request the caller can fix by editing.
func TestDiffAgainstARevisionWithoutARegistry(t *testing.T) {
	c := serve(t, Options{})

	_, err := c.render.Diff(context.Background(), connect.NewRequest(&kelsonv1alpha1.DiffRequest{
		Spec:         inlineSpec(projectDoc, map[string]string{"development": developmentDoc}),
		Environment:  "development",
		Profile:      profileRef(),
		FromRevision: "7-a1b2c3d4",
	}))
	if connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("code = %v, want unimplemented (%v)", connect.CodeOf(err), err)
	}
}

// TestDiffFromAndFromRevisionRejected: two answers to "compare against what",
// and no written-down precedence between them. The request is refused rather
// than silently resolved — and refused for *that* reason, ahead of the gate,
// because it is a mistake the caller can fix.
func TestDiffFromAndFromRevisionRejected(t *testing.T) {
	connector, _ := connectorFor(nil)
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
}

// TestDiffFromRevisionServerRejected: a server dry-run is the live cluster's
// verdict on the current set. It has no before side a revision could occupy, so
// asking for both is refused rather than answered with the one it can do.
func TestDiffFromRevisionServerRejected(t *testing.T) {
	preview := &fakePreview{diff: &diff.Diff{Level: diff.LevelServer}}
	connector, _ := connectorFor(nil)
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
