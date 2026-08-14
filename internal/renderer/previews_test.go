package renderer

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/dafrie/kelson/internal/model"
)

// PR previews (ADR-0017). The rendered shapes are pinned by the golden fixtures
// under testdata/render/previews-*; these tests pin what a golden file cannot
// hold — the delivery-mode gate, the boundary that keeps application manifests
// out of the template, the name cap, and the fact that the template string is
// the same bytes every time.

func previewsFixture(previews *model.ResolvedPreviews) *model.Resolved {
	r := resolvedFixture()
	r.Environment.Mode = model.DeliveryFlux
	r.Environment.Previews = previews
	return r
}

func githubPreviews() *model.ResolvedPreviews {
	return &model.ResolvedPreviews{
		Provider:  model.PreviewGitHub,
		Repo:      "https://github.com/acme/checkout",
		SecretRef: "github-auth",
		Interval:  "10m",
		Filter: model.ResolvedPreviewFilter{
			Labels: []string{"deploy/preview"},
			Limit:  model.PreviewDefaultLimit,
		},
		Artifacts: model.PreviewArtifacts{Repository: "oci://ghcr.io/acme/checkout-previews"},
	}
}

// TestPreviewsRequireFluxMode is the gate ADR-0017 takes from ADR-0016
// decision 4, citing it deliberately as that decision demands. Outside Flux
// mode there is nothing to reconcile a ResourceSet — usually not even a served
// CRD — so kelson refuses rather than emitting one.
func TestPreviewsRequireFluxMode(t *testing.T) {
	for _, mode := range []model.DeliveryMode{model.DeliveryDirect, ""} {
		r := previewsFixture(githubPreviews())
		r.Environment.Mode = mode
		_, err := Render(r, gatewayProfile(), nil)
		if err == nil {
			t.Fatalf("mode %q rendered a ResourceSet nothing would reconcile", mode)
		}
		errs, ok := err.(Errors)
		if !ok || len(errs) != 1 {
			t.Fatalf("mode %q: expected one structured error, got %#v", mode, err)
		}
		e := errs[0]
		if e.Code != ErrPreviewsRequireFlux {
			t.Errorf("mode %q: code = %q, want %q", mode, e.Code, ErrPreviewsRequireFlux)
		}
		if !strings.Contains(e.Message, "production") {
			t.Errorf("mode %q: the message must name the environment: %s", mode, e.Message)
		}
		if mode != "" && !strings.Contains(e.Message, string(mode)) {
			t.Errorf("mode %q: the message must name the mode: %s", mode, e.Message)
		}
		for _, want := range []string{"delivery.mode: flux", "ADR-0017"} {
			if !strings.Contains(e.Remediation, want) {
				t.Errorf("mode %q: remediation must contain %q: %s", mode, want, e.Remediation)
			}
		}
	}
}

// TestPreviewsGateIgnoresProfile is the other half of the gate's contract: it
// is decided from spec data alone. A cluster with no flux-operator detected
// still renders — whether the operator is installed is a capability finding
// (issue #157), not a rendering decision, or the same document would render
// differently against two clusters.
func TestPreviewsGateIgnoresProfile(t *testing.T) {
	ms, err := Render(previewsFixture(githubPreviews()), gatewayProfile(), nil)
	if err != nil {
		t.Fatalf("a flux-mode environment with previews must render against any profile: %v", err)
	}
	for _, kind := range []string{"ResourceSetInputProvider", "ResourceSet"} {
		if !hasKind(ms, kind) {
			t.Fatalf("no %s in %v", kind, kinds(ms))
		}
	}
}

// TestPreviewsRenderNothingWhenUnset: an environment without a previews block
// renders exactly what it always did. The pair is opt-in and there is no
// default forge.
func TestPreviewsRenderNothingWhenUnset(t *testing.T) {
	ms, err := Render(resolvedFixture(), gatewayProfile(), nil)
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}
	if hasKind(ms, "ResourceSet") || hasKind(ms, "ResourceSetInputProvider") {
		t.Fatalf("previews are opt-in, got %v", kinds(ms))
	}
}

// TestPreviewsPairIsTheWholeInventory. kelson's inventory for a previews block
// is two objects in the environment's namespace. Everything a preview runs is
// created by the Kustomization the ResourceSet templates, from an artifact this
// render does not contain.
func TestPreviewsPairIsTheWholeInventory(t *testing.T) {
	r := previewsFixture(githubPreviews())
	r.Components = nil
	ms, err := Render(r, gatewayProfile(), nil)
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}
	want := []string{
		"Namespace/checkout-prod",
		"ResourceSetInputProvider/checkout-production-previews",
		"ResourceSet/checkout-production-previews",
	}
	got := kinds(ms)
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	for _, m := range ms[1:] {
		if m.Namespace != "checkout-prod" {
			t.Errorf("%s must live in the environment's namespace, got %q", m.Kind, m.Namespace)
		}
	}
}

// TestPreviewTemplateCarriesNoApplicationManifests is ADR-0016 decision 5
// asserted mechanically. The template instantiates an OCIRepository and a
// Kustomization and nothing else; the day a Deployment appears in it,
// artifact-per-PR has failed rather than grown, and this test is the alarm.
func TestPreviewTemplateCarriesNoApplicationManifests(t *testing.T) {
	tmpl := renderedTemplate(t, previewsFixture(githubPreviews()))
	var kinds []string
	for _, doc := range strings.Split(tmpl, "---\n") {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		var head struct {
			APIVersion string `yaml:"apiVersion"`
			Kind       string `yaml:"kind"`
		}
		if err := yaml.Unmarshal([]byte(doc), &head); err != nil {
			t.Fatalf("the template must be parseable YAML before substitution: %v\n%s", err, doc)
		}
		kinds = append(kinds, head.Kind)
	}
	want := []string{"OCIRepository", "Kustomization"}
	if len(kinds) != len(want) {
		t.Fatalf("the template must contain exactly %v, got %v", want, kinds)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("the template must contain exactly %v, got %v", want, kinds)
		}
	}
}

// TestPreviewTemplateAddressesArtifactsByCommit is ADR-0017 decision 2. A tag
// of pr-<id> would be mutable by construction, which makes "what is running in
// preview 412" depend on when you ask.
func TestPreviewTemplateAddressesArtifactsByCommit(t *testing.T) {
	tmpl := renderedTemplate(t, previewsFixture(githubPreviews()))
	if !strings.Contains(tmpl, "tag: << inputs.sha >>") {
		t.Errorf("the artifact tag must be the head commit:\n%s", tmpl)
	}
	if strings.Contains(tmpl, "tag: pr") || strings.Contains(tmpl, "tag: << inputs.id >>") {
		t.Errorf("the artifact tag must not be the change request number:\n%s", tmpl)
	}
	// The id is still what names things: it is what a human types when they go
	// looking, and it is stable across pushes in a way a SHA is not.
	if !strings.Contains(tmpl, "name: checkout-production-pr<< inputs.id >>") {
		t.Errorf("preview names must carry the change request number:\n%s", tmpl)
	}
}

// TestPreviewTemplateConfinesThePreview: prune and wait are what make the
// teardown of issue #103 hold, and targetNamespace is what keeps a
// mis-published artifact inside the pull request's own namespace.
func TestPreviewTemplateConfinesThePreview(t *testing.T) {
	tmpl := renderedTemplate(t, previewsFixture(githubPreviews()))
	for _, want := range []string{
		"  prune: true\n",
		"  wait: true\n",
		"  targetNamespace: checkout-production-pr<< inputs.id >>\n",
	} {
		if !strings.Contains(tmpl, want) {
			t.Errorf("the Kustomization must set %q:\n%s", strings.TrimSpace(want), tmpl)
		}
	}
}

// TestPreviewArtifactSecretIsOmittedWhenUnset. An empty secretRef would be a
// reference to a Secret that does not exist, which source-controller reports as
// a fetch failure rather than as "you left a field blank".
func TestPreviewArtifactSecretIsOmittedWhenUnset(t *testing.T) {
	tmpl := renderedTemplate(t, previewsFixture(githubPreviews()))
	if strings.Contains(tmpl, "secretRef") {
		t.Errorf("a public artifact repository needs no pull secret:\n%s", tmpl)
	}

	previews := githubPreviews()
	previews.Artifacts.SecretRef = "ghcr-auth"
	tmpl = renderedTemplate(t, previewsFixture(previews))
	if !strings.Contains(tmpl, "  secretRef:\n    name: ghcr-auth\n") {
		t.Errorf("a private artifact repository must carry its pull secret:\n%s", tmpl)
	}
}

// TestPreviewTemplateIsDeterministic. The template is assembled as text, so it
// gets its own determinism assertion rather than relying on the encoder's.
func TestPreviewTemplateIsDeterministic(t *testing.T) {
	previews := githubPreviews()
	previews.Filter.Labels = []string{"deploy/preview", "team/platform"}
	previews.Skip = []string{"deploy/pause", "!ci/passed"}
	previews.Artifacts.SecretRef = "ghcr-auth"

	first := renderedTemplate(t, previewsFixture(previews))
	for i := 0; i < 25; i++ {
		if got := renderedTemplate(t, previewsFixture(previews)); got != first {
			t.Fatalf("run %d produced a different template:\n%s\n---\n%s", i, first, got)
		}
	}
}

// TestPreviewProviderMapping pins the enum onto flux-operator's provider types.
// A merge request and a pull request are the same input and a different API to
// reach it through.
func TestPreviewProviderMapping(t *testing.T) {
	for provider, want := range map[model.PreviewProvider]string{
		model.PreviewGitHub: "GitHubPullRequest",
		model.PreviewGitLab: "GitLabMergeRequest",
	} {
		if got := previewProviderType(provider); got != want {
			t.Errorf("provider %q → %q, want %q", provider, got, want)
		}
	}
}

// TestPreviewNameCap. A preview's name is also its namespace, which is a
// DNS-1123 label capped at 63 characters, and the change request number is
// added by flux-operator long after the render. Refusing at the field beats a
// ResourceSet condition nobody connects back to the spec.
func TestPreviewNameCap(t *testing.T) {
	// 54 characters of <project>-<environment> is the limit, so this pair is
	// exactly at it and must render.
	r := previewsFixture(githubPreviews())
	r.Project = strings.Repeat("p", 40)
	r.Environment.Name = strings.Repeat("e", 13)
	r.Components = nil
	if _, err := Render(r, gatewayProfile(), nil); err != nil {
		t.Fatalf("a 54-character base must render: %v", err)
	}

	r.Environment.Name = strings.Repeat("e", 14) // 55
	_, err := Render(r, gatewayProfile(), nil)
	errs, ok := err.(Errors)
	if !ok || len(errs) != 1 || errs[0].Code != ErrPreviewName {
		t.Fatalf("a 55-character base must be refused as %s, got %#v", ErrPreviewName, err)
	}
	if !strings.Contains(errs[0].Remediation, "63") {
		t.Errorf("the remediation must name the limit it is protecting: %s", errs[0].Remediation)
	}
}

// TestPreviewsHashCoversTheBlock: editing the previews block must move the
// spec-hash, or a changed poller would be applied under an annotation claiming
// nothing changed.
func TestPreviewsHashCoversTheBlock(t *testing.T) {
	before := previewsSpecHash(t, githubPreviews())

	changed := githubPreviews()
	changed.Filter.Limit = 25
	if after := previewsSpecHash(t, changed); after == before {
		t.Errorf("the spec-hash must move when the filter does")
	}

	changed = githubPreviews()
	changed.Artifacts.Repository = "oci://ghcr.io/acme/other-previews"
	if after := previewsSpecHash(t, changed); after == before {
		t.Errorf("the spec-hash must move when the artifact repository does")
	}
}

// renderedTemplate returns the ResourceSet's resourcesTemplate string.
func renderedTemplate(t *testing.T, r *model.Resolved) string {
	t.Helper()
	ms, err := Render(r, gatewayProfile(), nil)
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}
	for _, m := range ms {
		if m.Kind != "ResourceSet" {
			continue
		}
		tmpl := mapGet(mapGet(docRoot(m.doc), "spec"), "resourcesTemplate")
		if tmpl == nil {
			t.Fatalf("the ResourceSet carries no resourcesTemplate")
		}
		if tmpl.Style != yaml.LiteralStyle {
			t.Errorf("the template must be a literal block scalar, or it is the one part of kelson's output nobody can read")
		}
		return tmpl.Value
	}
	t.Fatalf("no ResourceSet in %v", kinds(ms))
	return ""
}

func previewsSpecHash(t *testing.T, previews *model.ResolvedPreviews) string {
	t.Helper()
	ms, err := Render(previewsFixture(previews), gatewayProfile(), nil)
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}
	for _, m := range ms {
		if m.Kind != "ResourceSet" {
			continue
		}
		ann := mapGet(mapGet(docRoot(m.doc), "metadata"), "annotations")
		return mapGet(ann, "kelson.dev/spec-hash").Value
	}
	t.Fatalf("no ResourceSet in %v", kinds(ms))
	return ""
}
