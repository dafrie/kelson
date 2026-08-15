package preview_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/dafrie/kelson/internal/clusterprofile"
	"github.com/dafrie/kelson/internal/model"
	"github.com/dafrie/kelson/internal/preview"
)

// The publisher's render is the parent environment's render with the preview's
// identity substituted (ADR-0017 stage 2). These tests pin both halves of that
// claim: what must differ, and what must not.

const testSHA = "0123456789abcdef0123456789abcdef01234567"

func testProject() *model.Project {
	return &model.Project{
		Metadata: model.ObjectMeta{Name: "checkout"},
		Spec: model.ProjectSpec{
			Image: "ghcr.io/acme/checkout:1.4.2",
			Components: []model.Component{
				{Name: "web", Port: 8080, Health: "/healthz"},
			},
		},
	}
}

func testEnvironment() *model.Environment {
	return &model.Environment{
		Metadata: model.ObjectMeta{Name: "staging"},
		Spec: model.EnvironmentSpec{
			Project:   "checkout",
			Namespace: "checkout-staging",
			Routing:   &model.Routing{DomainSuffix: "staging.acme.run", GatewayClass: "envoy"},
			Delivery: &model.Delivery{
				Mode: model.DeliveryFlux,
				Git:  &model.GitTarget{Repo: "git@github.com:acme/deploy.git", Path: "checkout/staging"},
			},
			Previews: &model.Previews{
				Provider:  model.PreviewGitHub,
				Repo:      "https://github.com/acme/checkout",
				SecretRef: "github-auth",
				Artifacts: model.PreviewArtifacts{Repository: "oci://ghcr.io/acme/checkout-previews"},
			},
		},
	}
}

func testProfile() clusterprofile.ClusterProfile {
	return clusterprofile.ClusterProfile{
		GatewayAPI: &clusterprofile.GatewayAPI{Classes: []string{"envoy"}},
	}
}

func testOptions() preview.Options {
	return preview.Options{
		Project:     testProject(),
		Environment: testEnvironment(),
		PR:          "412",
		SHA:         testSHA,
		Profile:     testProfile(),
	}
}

func mustRender(t *testing.T, opts preview.Options) *preview.Set {
	t.Helper()
	set, err := preview.Render(opts)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	return set
}

// TestRenderTargetsThePreviewNamespace is ADR-0017 decision 3 from the
// publisher's side: every resource in the artifact belongs to the change
// request's namespace, and the cluster-scoped Namespace object that creates it
// is in the set — the ResourceSet's targetNamespace cannot create one.
func TestRenderTargetsThePreviewNamespace(t *testing.T) {
	set := mustRender(t, testOptions())
	if set.Namespace != "checkout-staging-pr412" {
		t.Fatalf("namespace = %q, want checkout-staging-pr412", set.Namespace)
	}
	if set.ParentNamespace != "checkout-staging" {
		t.Errorf("parent namespace = %q, want checkout-staging", set.ParentNamespace)
	}

	var namespaces int
	for _, m := range set.Manifests {
		if m.Kind == "Namespace" {
			namespaces++
			if m.Name != set.Namespace {
				t.Errorf("the set declares Namespace %q, want %q", m.Name, set.Namespace)
			}
			continue
		}
		if m.Namespace != set.Namespace {
			t.Errorf("%s/%s is in namespace %q, want %q", m.Kind, m.Name, m.Namespace, set.Namespace)
		}
	}
	if namespaces != 1 {
		t.Errorf("the set declares %d Namespaces, want exactly 1", namespaces)
	}
}

// TestRenderCarriesNoPreviewLifecycle is the recursion this publisher must not
// create: a preview that carried the previews block would render a
// ResourceSetInputProvider of its own and every pull request would spawn
// previews of itself.
func TestRenderCarriesNoPreviewLifecycle(t *testing.T) {
	for _, m := range mustRender(t, testOptions()).Manifests {
		if m.Kind == "ResourceSet" || m.Kind == "ResourceSetInputProvider" {
			t.Errorf("a preview's artifact contains a %s; the lifecycle belongs to the parent environment only", m.Kind)
		}
	}
}

// TestRenderKeepsParentProvenance is the "differs only where it must" half.
// Decision 7 rests on it: a preview is discoverable by the same selector as
// everything else kelson writes, differing by namespace and nothing else.
func TestRenderKeepsParentProvenance(t *testing.T) {
	set := mustRender(t, testOptions())
	want := map[string]string{
		"app.kubernetes.io/managed-by": "kelson",
		"kelson.dev/project":           "checkout",
		"kelson.dev/environment":       "staging",
	}
	for _, m := range set.Manifests {
		body, err := m.YAML()
		if err != nil {
			t.Fatalf("encoding %s/%s: %v", m.Kind, m.Name, err)
		}
		for k, v := range want {
			if !strings.Contains(string(body), k+": "+v) {
				t.Errorf("%s/%s carries no %s: %s", m.Kind, m.Name, k, v)
			}
		}
	}
}

// TestRenderGivesEachChangeRequestItsOwnHostnames is the collision the
// publisher would otherwise create: without it, every preview of an
// environment claims the environment's hostname.
func TestRenderGivesEachChangeRequestItsOwnHostnames(t *testing.T) {
	first := hostnames(t, mustRender(t, testOptions()))
	if len(first) == 0 {
		t.Fatal("the fixture renders no hostnames, so this test proves nothing")
	}
	for _, h := range first {
		if !strings.HasPrefix(h, "web-pr412.") {
			t.Errorf("hostname %q does not belong to change request 412", h)
		}
	}

	opts := testOptions()
	opts.PR = "413"
	second := hostnames(t, mustRender(t, opts))
	for i := range first {
		if first[i] == second[i] {
			t.Errorf("change requests 412 and 413 both claim %q", first[i])
		}
	}
}

// TestRenderRecordsTheHostsItRendered is what the publish's callers report
// (ADR-0034 decision 5: a preview's comment names its hosts). The claim is that
// the recorded list and the rendered routes cannot disagree — recording is not
// a second derivation of the hostnames, it is the same one.
func TestRenderRecordsTheHostsItRendered(t *testing.T) {
	set := mustRender(t, testOptions())
	if len(set.Hosts) == 0 {
		t.Fatal("the fixture renders hostnames but the set records none")
	}
	rendered := hostnames(t, set)
	slices.Sort(rendered)
	if !slices.Equal(set.Hosts, rendered) {
		t.Errorf("Hosts = %v, want the hostnames the HTTPRoutes claim %v", set.Hosts, rendered)
	}
}

// A component nothing routes has no host, and the set says so rather than
// promising a name that answers nothing.
func TestRenderRecordsNoHostsForAnUnroutedSpec(t *testing.T) {
	environment := testEnvironment()
	environment.Spec.Routing = nil

	opts := testOptions()
	opts.Environment = environment
	if got := mustRender(t, opts).Hosts; len(got) != 0 {
		t.Errorf("Hosts = %v, want none for a spec that declares no domains", got)
	}
}

// TestRenderDoesNotMutateTheAuthoredDocuments matters because the CLI may
// render the parent environment from the same documents in the same process,
// and resolution carries authored slices by reference.
func TestRenderDoesNotMutateTheAuthoredDocuments(t *testing.T) {
	project := testProject()
	project.Spec.Components[0].Domains = []string{"shop.acme.com"}
	environment := testEnvironment()

	opts := testOptions()
	opts.Project = project
	opts.Environment = environment
	opts.Image = "ghcr.io/acme/checkout@sha256:" + strings.Repeat("a", 64)
	mustRender(t, opts)

	if got := project.Spec.Components[0].Domains[0]; got != "shop.acme.com" {
		t.Errorf("the authored domain became %q; a render must not edit the caller's documents", got)
	}
	if project.Spec.Image != "ghcr.io/acme/checkout:1.4.2" {
		t.Errorf("the authored image became %q; --image must not edit the caller's documents", project.Spec.Image)
	}
	if environment.Spec.Namespace != "checkout-staging" {
		t.Errorf("the authored namespace became %q", environment.Spec.Namespace)
	}
	if environment.Spec.Previews == nil {
		t.Error("the authored previews block was dropped from the caller's document")
	}
}

// TestRenderPinsTheImageLikeDeploy: --image stands in for spec.image under the
// same precedence, which is what lets `kelson build` and this command compose
// in one CI job.
func TestRenderPinsTheImageLikeDeploy(t *testing.T) {
	opts := testOptions()
	opts.Image = "ghcr.io/acme/checkout@sha256:" + strings.Repeat("a", 64)
	set := mustRender(t, opts)
	found := false
	for _, m := range set.Manifests {
		if m.Kind != "Deployment" {
			continue
		}
		body, err := m.YAML()
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), opts.Image) {
			found = true
		}
	}
	if !found {
		t.Errorf("no Deployment runs %s", opts.Image)
	}
}

func TestRenderRefusals(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*preview.Options)
		reason string
	}{
		{
			name:   "no previews block",
			mutate: func(o *preview.Options) { o.Environment.Spec.Previews = nil },
			reason: preview.ReasonNoPreviews,
		},
		{
			name:   "no artifact repository",
			mutate: func(o *preview.Options) { o.Environment.Spec.Previews.Artifacts.Repository = "" },
			reason: preview.ReasonNoArtifactRepository,
		},
		{
			name:   "not flux mode",
			mutate: func(o *preview.Options) { o.Environment.Spec.Delivery = &model.Delivery{Mode: model.DeliveryDirect} },
			reason: preview.ReasonRequiresFlux,
		},
		{
			name:   "change request is not a number",
			mutate: func(o *preview.Options) { o.PR = "pr-412" },
			reason: preview.ReasonInvalidChangeRequest,
		},
		{
			name:   "change request exceeds the reservation",
			mutate: func(o *preview.Options) { o.PR = "1234567" },
			reason: preview.ReasonInvalidChangeRequest,
		},
		{
			name:   "abbreviated commit",
			mutate: func(o *preview.Options) { o.SHA = testSHA[:8] },
			reason: preview.ReasonInvalidSHA,
		},
		{
			name: "name too long",
			mutate: func(o *preview.Options) {
				o.Environment.Metadata.Name = strings.Repeat("e", 50)
				o.Environment.Spec.Project = "checkout"
			},
			reason: preview.ReasonNameTooLong,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := testOptions()
			tc.mutate(&opts)
			_, err := preview.Render(opts)
			if err == nil {
				t.Fatal("the publisher rendered something it should have refused")
			}
			var refusal preview.Error
			if !asPreviewError(err, &refusal) {
				t.Fatalf("error is %T (%v), want a structured preview.Error", err, err)
			}
			if refusal.Reason != tc.reason {
				t.Errorf("reason = %q, want %q", refusal.Reason, tc.reason)
			}
			if refusal.Remediation == "" {
				t.Error("a refusal must say what to do about it")
			}
		})
	}
}

// TestNoPreviewsRefusalNamesTheField is the house rule for a refusal about a
// missing field: name the field, or the reader has to guess what to write.
func TestNoPreviewsRefusalNamesTheField(t *testing.T) {
	opts := testOptions()
	opts.Environment.Spec.Previews = nil
	_, err := preview.Render(opts)
	if err == nil {
		t.Fatal("an environment with no previews was published anyway")
	}
	if !strings.Contains(err.Error(), "spec.previews") {
		t.Errorf("the refusal does not name spec.previews: %s", err)
	}
}

// TestRenderReportsTheDestination: the two addresses the artifact has to agree
// with are on the Set, so the CLI never re-derives them.
func TestRenderReportsTheDestination(t *testing.T) {
	set := mustRender(t, testOptions())
	if set.Repository != "ghcr.io/acme/checkout-previews" {
		t.Errorf("repository = %q, want the spec's repository without the oci:// prefix", set.Repository)
	}
	if set.Tag != testSHA {
		t.Errorf("tag = %q, want the head commit %s", set.Tag, testSHA)
	}
	if set.SourceRepo != "https://github.com/acme/checkout" {
		t.Errorf("source repo = %q, want the forge repository", set.SourceRepo)
	}
}

func hostnames(t *testing.T, set *preview.Set) []string {
	t.Helper()
	var out []string
	for _, m := range set.Manifests {
		if m.Kind != "HTTPRoute" {
			continue
		}
		body, err := m.YAML()
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(body), "\n") {
			if h := strings.TrimSpace(line); strings.HasPrefix(h, "- ") && strings.Contains(h, ".acme.run") {
				out = append(out, strings.TrimPrefix(h, "- "))
			}
		}
	}
	return out
}

// asPreviewError is errors.As without the import, because preview.Error is a
// value type and the publisher never wraps it.
func asPreviewError(err error, target *preview.Error) bool {
	e, ok := err.(preview.Error)
	if ok {
		*target = e
	}
	return ok
}
