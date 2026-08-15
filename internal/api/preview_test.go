package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/delivery/flux"
)

// The environment previews are declared on. Flux mode, because ADR-0017
// decision 5 makes that the only mode they render in — and the mode gate is one
// of the things ListPreviews has to state rather than hide.
const previewEnvDoc = `apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata:
  name: staging
spec:
  project: hello
  previews:
    provider: github
    repo: https://github.com/acme/hello
    secretRef: github-auth
    filter:
      labels: [deploy/preview]
    artifacts:
      repository: oci://ghcr.io/acme/hello-previews
`

// The same environment in direct mode: a spec the renderer refuses.
// fakePreviewReader answers with a fixed set, recording the scope it was asked
// about. The reader itself is tested against a fake cluster in
// internal/delivery/flux; what matters here is the scope the handler derives
// and the projection onto the wire.
type fakePreviewReader struct {
	set   flux.PreviewSet
	err   error
	scope flux.PreviewScope
}

func (f *fakePreviewReader) Previews(_ context.Context, scope flux.PreviewScope) (flux.PreviewSet, error) {
	f.scope = scope
	if f.err != nil {
		return flux.PreviewSet{}, f.err
	}
	return f.set, nil
}

func previewConnectorFor(reader flux.PreviewReader) DeliveryConnector {
	return func(_ context.Context, _ Target) (*Plane, error) {
		return &Plane{Previews: reader}, nil
	}
}

func listPreviews(t *testing.T, c clients, env string) *kelsonv1alpha1.ListPreviewsResponse {
	t.Helper()
	res, err := c.previews.ListPreviews(context.Background(), connect.NewRequest(&kelsonv1alpha1.ListPreviewsRequest{
		Spec:        inlineSpec(projectDoc, map[string]string{"staging": env}),
		Environment: "staging",
	}))
	if err != nil {
		t.Fatalf("ListPreviews: %v", err)
	}
	return res.Msg
}

func TestListPreviewsReportsTheClustersPreviews(t *testing.T) {
	created := time.Now().Add(-90 * time.Minute)
	reader := &fakePreviewReader{set: flux.PreviewSet{
		Lifecycle: flux.PreviewLifecycle{
			Name: "hello-staging-previews", Served: true, Present: true,
			Provider: flux.ConditionTrue, Set: flux.ConditionTrue, SetMessage: "applied 2 resources",
		},
		Previews: []flux.Preview{{
			ID:        "412",
			Namespace: "hello-staging-pr412",
			SHA:       "abc123",
			Phase:     flux.PreviewReady,
			Artifact:  flux.ConditionTrue,
			Applied:   flux.ConditionTrue,
			Revision:  "abc123@sha256:dead",
			Hosts:     []string{"web-pr412.staging.acme.run"},
			CreatedAt: created,
		}},
	}}
	c := serve(t, Options{Delivery: previewConnectorFor(reader)})

	got := listPreviews(t, c, previewEnvDoc)

	if got.GetProject() != "hello" || got.GetEnvironment() != "staging" {
		t.Fatalf("identity = %q/%q", got.GetProject(), got.GetEnvironment())
	}
	if got.GetNamespace() != "hello-staging" || got.GetMode() != "flux" {
		t.Errorf("namespace/mode = %q/%q", got.GetNamespace(), got.GetMode())
	}
	// The scope is the *parent* environment's namespace: that is where the
	// lifecycle pair and every per-change-request object live (ADR-0017
	// decision 3), and reading the preview's own namespace would find nothing.
	if reader.scope != (flux.PreviewScope{Project: "hello", Environment: "staging", Namespace: "hello-staging"}) {
		t.Errorf("scope = %+v", reader.scope)
	}
	if len(got.GetErrors()) > 0 {
		t.Fatalf("errors = %v", got.GetErrors())
	}

	if l := got.GetLifecycle(); l == nil || !l.GetServed() || !l.GetPresent() || l.GetSetMessage() != "applied 2 resources" {
		t.Fatalf("lifecycle = %+v", got.GetLifecycle())
	}
	if len(got.GetPreviews()) != 1 {
		t.Fatalf("previews = %d, want 1", len(got.GetPreviews()))
	}
	p := got.GetPreviews()[0]
	if p.GetId() != "412" || p.GetNamespace() != "hello-staging-pr412" || p.GetSha() != "abc123" {
		t.Errorf("preview = %+v", p)
	}
	if p.GetPhase() != "ready" {
		t.Errorf("phase = %q", p.GetPhase())
	}
	if p.GetHosts()[0] != "web-pr412.staging.acme.run" {
		t.Errorf("hosts = %v", p.GetHosts())
	}
	// The age is computed server-side so every client renders the same number
	// without a synchronised clock.
	if p.GetAgeSeconds() < 5000 || p.GetAgeSeconds() > 5600 {
		t.Errorf("ageSeconds = %d, want about 5400", p.GetAgeSeconds())
	}
	if p.GetCreatedAt() == "" {
		t.Errorf("createdAt is empty")
	}
}

// The resolved settings, defaults included: the ceiling the cluster will
// enforce is the number a reader is shown (ADR-0017).
func TestListPreviewsReportsResolvedSettings(t *testing.T) {
	c := serve(t, Options{Delivery: previewConnectorFor(&fakePreviewReader{})})

	s := listPreviews(t, c, previewEnvDoc).GetSettings()
	if s == nil {
		t.Fatal("settings are nil")
	}
	if s.GetProvider() != "github" || s.GetRepo() != "https://github.com/acme/hello" {
		t.Errorf("settings = %+v", s)
	}
	if s.GetSecretRef() != "github-auth" {
		t.Errorf("secretRef = %q — a Secret name, which is all the spec holds", s.GetSecretRef())
	}
	if s.GetInterval() != "10m" {
		t.Errorf("interval = %q, want the model's default", s.GetInterval())
	}
	if s.GetLimit() != 10 {
		t.Errorf("limit = %d, want the default ceiling", s.GetLimit())
	}
	if len(s.GetFilterLabels()) != 1 || s.GetFilterLabels()[0] != "deploy/preview" {
		t.Errorf("filterLabels = %v", s.GetFilterLabels())
	}
	if s.GetArtifactsRepository() != "oci://ghcr.io/acme/hello-previews" {
		t.Errorf("artifacts = %q", s.GetArtifactsRepository())
	}
}

// An environment that declares no previews is the ordinary case: identity, no
// settings, no error. The empty state is the client's to write.
func TestListPreviewsOnAnEnvironmentWithoutPreviews(t *testing.T) {
	reader := &fakePreviewReader{}
	c := serve(t, Options{Delivery: previewConnectorFor(reader)})

	res, err := c.previews.ListPreviews(context.Background(), connect.NewRequest(&kelsonv1alpha1.ListPreviewsRequest{
		Spec:        inlineSpec(projectDoc, map[string]string{"development": developmentDoc}),
		Environment: "development",
	}))
	if err != nil {
		t.Fatalf("ListPreviews: %v", err)
	}
	if res.Msg.GetSettings() != nil {
		t.Errorf("settings = %+v, want none", res.Msg.GetSettings())
	}
	if res.Msg.GetLifecycle() != nil {
		t.Errorf("lifecycle = %+v: nothing should have been read", res.Msg.GetLifecycle())
	}
	if len(res.Msg.GetErrors()) > 0 {
		t.Errorf("errors = %v: declaring no previews is not a finding", res.Msg.GetErrors())
	}
	if reader.scope != (flux.PreviewScope{}) {
		t.Errorf("the cluster was read for an environment with no previews: %+v", reader.scope)
	}
}

// The Flux-only gate this used to surface — settings plus
// `render/previews-require-flux` for an environment whose delivery mode could
// not run previews — is deleted with the mode vocabulary (ADR-0028 decision 8).
// There is no spec an author can write that makes previews unavailable, so
// there is no third answer left for this service to give.

func TestListPreviewsWithoutADeliveryPlaneIsUnimplemented(t *testing.T) {
	c := serve(t, Options{})

	_, err := c.previews.ListPreviews(context.Background(), connect.NewRequest(&kelsonv1alpha1.ListPreviewsRequest{
		Spec:        inlineSpec(projectDoc, map[string]string{"staging": previewEnvDoc}),
		Environment: "staging",
	}))
	if connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("code = %v, want unimplemented", connect.CodeOf(err))
	}
}

// A plane with no preview reader is a build that cannot answer, not a cluster
// with no previews. Reporting an empty list would be a claim.
func TestListPreviewsWithoutAReaderIsUnimplemented(t *testing.T) {
	c := serve(t, Options{Delivery: func(context.Context, Target) (*Plane, error) {
		return &Plane{}, nil
	}})

	_, err := c.previews.ListPreviews(context.Background(), connect.NewRequest(&kelsonv1alpha1.ListPreviewsRequest{
		Spec:        inlineSpec(projectDoc, map[string]string{"staging": previewEnvDoc}),
		Environment: "staging",
	}))
	if connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("code = %v, want unimplemented", connect.CodeOf(err))
	}
}

func TestListPreviewsReportsAFailedRead(t *testing.T) {
	c := serve(t, Options{Delivery: previewConnectorFor(&fakePreviewReader{err: errors.New("forbidden")})})

	_, err := c.previews.ListPreviews(context.Background(), connect.NewRequest(&kelsonv1alpha1.ListPreviewsRequest{
		Spec:        inlineSpec(projectDoc, map[string]string{"staging": previewEnvDoc}),
		Environment: "staging",
	}))
	if err == nil {
		t.Fatal("want an error when the cluster read fails")
	}
}
