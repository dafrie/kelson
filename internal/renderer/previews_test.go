package renderer

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/dafrie/kelson/internal/model"
	"github.com/dafrie/kelson/internal/preview/naming"
)

// PR previews (ADR-0017). The rendered shapes are pinned by the golden fixtures
// under testdata/render/previews-*; these tests pin what a golden file cannot
// hold — the boundary that keeps component manifests out of the template, the
// name cap, and the fact that the template string is the same bytes every time.
//
// The delivery-mode gate they used to pin is deleted (ADR-0028 decision 8):
// previews were Flux-only and Flux is the only path.

func previewsFixture(previews *model.ResolvedPreviews) *model.Resolved {
	r := resolvedFixture()
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

// TestPreviewsIgnoreProfile: the pair is rendered from spec data alone. A
// cluster with no flux-operator detected still renders — whether the operator is
// installed is a capability finding (issue #157), not a rendering decision, or
// the same document would render differently against two clusters.
func TestPreviewsIgnoreProfile(t *testing.T) {
	ms, err := Render(previewsFixture(githubPreviews()), gatewayProfile(), nil)
	if err != nil {
		t.Fatalf("an environment with previews must render against any profile: %v", err)
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

// TestPreviewTemplateCarriesNoComponentManifests is ADR-0016 decision 5
// asserted mechanically. The template instantiates an OCIRepository and a
// Kustomization and nothing else; the day a Deployment appears in it,
// artifact-per-PR has failed rather than grown, and this test is the alarm.
func TestPreviewTemplateCarriesNoComponentManifests(t *testing.T) {
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

// TestPreviewsSecretRefIsDerivedWhenUnset is the rendered half of ADR-0033
// decision 4. `previews.secretRef` is optional now: an author who names nothing
// gets a Secret kelson materializes at <project>-<environment>-previews, and
// the provider has to point at that name. A blank secretRef would be a
// reference to a Secret with no name and a forge polled anonymously beside a
// perfectly good credential.
//
// The golden fixture testdata/render/previews-materialized-secret pins the same
// thing in bytes; this pins the pairing itself — the renderer's name and
// internal/preview/naming's are the same string, which is what the controller's
// materializer spells its Secret with.
func TestPreviewsSecretRefIsDerivedWhenUnset(t *testing.T) {
	previews := githubPreviews()
	previews.SecretRef = ""
	r := previewsFixture(previews)

	got := renderedInputProviderSecret(t, r)
	want := naming.Lifecycle(r.Project, r.Environment.Name)
	if got != want {
		t.Errorf("secretRef.name = %q, want the derived %q", got, want)
	}
	if want != "checkout-production-previews" {
		t.Errorf("the derived name is %q; the materializer writes <project>-<environment>-previews", want)
	}
}

// The other direction: a named Secret is written verbatim and nothing is
// derived over it. "The field stays for anyone bringing their own Secret;
// nothing breaks" (ADR-0033 decision 4).
func TestPreviewsSecretRefIsVerbatimWhenSet(t *testing.T) {
	if got := renderedInputProviderSecret(t, previewsFixture(githubPreviews())); got != "github-auth" {
		t.Errorf("secretRef.name = %q, want the author's own %q", got, "github-auth")
	}
}

// renderedInputProviderSecret returns the ResourceSetInputProvider's
// spec.secretRef.name.
func renderedInputProviderSecret(t *testing.T, r *model.Resolved) string {
	t.Helper()
	ms, err := Render(r, gatewayProfile(), nil)
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}
	for _, m := range ms {
		if m.Kind != "ResourceSetInputProvider" {
			continue
		}
		ref := mapGet(mapGet(docRoot(m.doc), "spec"), "secretRef")
		if ref == nil {
			t.Fatalf("the provider carries no secretRef")
		}
		return mapGet(ref, "name").Value
	}
	t.Fatalf("no ResourceSetInputProvider in %v", kinds(ms))
	return ""
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

// TestPreviewTemplateSplitsForAReleaseHook is #104's second acceptance
// criterion, the one ADR-0019 recorded as *not met* and ADR-0028 left tracked on
// #227: a preview runs the project's migrations before its workloads roll.
//
// Nothing preview-specific was built for it. The preview's artifact comes out of
// the same renderer and the same publisher as the environment's (ADR-0028
// decision 2), so it already carries the release stage; the only thing that had
// to learn anything is the one Kustomization kelson writes as text, and it
// learns it from the same resolved spec. What this pins is that it did — because
// a preview that applied a staged artifact in one pass would run the migration
// beside the rollout, which is the failure the whole split exists to prevent,
// reintroduced through the one place the topology is not the controller's.
func TestPreviewTemplateSplitsForAReleaseHook(t *testing.T) {
	resolved := previewsFixture(githubPreviews())
	resolved.Components[0].Release = &model.ResolvedRelease{
		Command:        []string{"./manage.py", "migrate"},
		TimeoutSeconds: 900,
	}
	tmpl := renderedTemplate(t, resolved)
	for _, want := range []string{
		"  name: checkout-production-pr<< inputs.id >>-release\n",
		"  path: ./release\n",
		"  prune: false\n",
		"  timeout: 960s\n",
		"  dependsOn:\n    - name: checkout-production-pr<< inputs.id >>-release\n",
	} {
		if !strings.Contains(tmpl, want) {
			t.Errorf("a preview of a project with a release hook must carry %q:\n%s", want, tmpl)
		}
	}
	if n := strings.Count(tmpl, "kind: Kustomization"); n != 2 {
		t.Errorf("the template holds %d Kustomizations, want 2 — the release stage and the workloads:\n%s", n, tmpl)
	}
}

// TestPreviewTemplateStaysSingleWithoutAHook: the split must not tax a project
// that does not use it, in a preview exactly as in an environment.
func TestPreviewTemplateStaysSingleWithoutAHook(t *testing.T) {
	tmpl := renderedTemplate(t, previewsFixture(githubPreviews()))
	if strings.Contains(tmpl, "dependsOn") || strings.Contains(tmpl, "-release") {
		t.Errorf("a project with no release hook must template one Kustomization:\n%s", tmpl)
	}
}
