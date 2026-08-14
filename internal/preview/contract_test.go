package preview_test

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/dafrie/kelson/internal/model"
	"github.com/dafrie/kelson/internal/preview/naming"
	"github.com/dafrie/kelson/internal/renderer"
)

// The publisher/consumer contract.
//
// ADR-0017 shipped stage 1 with this stated as a negative: "the publisher and
// the renderer agree by convention, not by type. Nothing checks that, and the
// failure mode of getting it wrong — a stray namespace, or an OCIRepository
// pointing at a tag that will never exist — is quiet."
//
// The convention is now shared code (internal/preview/naming), and this test is
// the other half of the mitigation: it renders the environment's ResourceSet,
// substitutes the inputs flux-operator would substitute, and asserts that what
// the cluster will ask for is exactly what a publish produces. It fails if
// either side changes alone, which is the only property that matters here.

// resourcesTemplate renders the parent environment and returns the
// ResourceSet's template with flux-operator's inputs filled in for one change
// request, as the cluster would see it.
func resourcesTemplate(t *testing.T, pr, sha string) string {
	t.Helper()
	resolved, errs := model.Resolve(testProject(), testEnvironment())
	if len(errs) > 0 {
		t.Fatalf("resolving the parent environment: %v", errs)
	}
	manifests, err := renderer.Render(resolved, testProfile(), nil)
	if err != nil {
		t.Fatalf("rendering the parent environment: %v", err)
	}

	for _, m := range manifests {
		if m.Kind != "ResourceSet" {
			continue
		}
		body, err := m.YAML()
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Spec struct {
				ResourcesTemplate string `yaml:"resourcesTemplate"`
			} `yaml:"spec"`
		}
		if err := yaml.Unmarshal(body, &doc); err != nil {
			t.Fatalf("parsing the ResourceSet: %v", err)
		}
		out := strings.ReplaceAll(doc.Spec.ResourcesTemplate, naming.InputID, pr)
		return strings.ReplaceAll(out, naming.InputSHA, sha)
	}
	t.Fatal("the parent environment rendered no ResourceSet")
	return ""
}

// TestPublishedTagIsTheTagTheClusterPins is the contract in one line: the
// OCIRepository the ResourceSet templates asks for a tag, and a publish creates
// exactly that tag.
func TestPublishedTagIsTheTagTheClusterPins(t *testing.T) {
	set := mustRender(t, testOptions())
	artifact := mustPackage(t, set)
	template := resourcesTemplate(t, set.PR, set.SHA)

	wanted := field(t, template, "tag")
	if wanted != artifact.Tag {
		t.Errorf("the cluster fetches tag %q and the publisher pushes %q", wanted, artifact.Tag)
	}
	if url := field(t, template, "url"); url != "oci://"+artifact.Repository {
		t.Errorf("the cluster fetches from %q and the publisher pushes to %q", url, artifact.Repository)
	}
}

// TestPublishedNamespaceIsTheNamespaceTheClusterCreates closes the other half:
// the artifact carries the Namespace object, so a publisher that rendered a
// different name would leave a stray namespace behind and put the preview's
// resources somewhere the Kustomization's targetNamespace then overrides.
func TestPublishedNamespaceIsTheNamespaceTheClusterCreates(t *testing.T) {
	set := mustRender(t, testOptions())
	template := resourcesTemplate(t, set.PR, set.SHA)

	if got := field(t, template, "targetNamespace"); got != set.Namespace {
		t.Errorf("the Kustomization applies into %q and the publisher rendered for %q", got, set.Namespace)
	}
	if got := field(t, template, "name"); got != set.Namespace {
		t.Errorf("the templated object name is %q and the preview namespace is %q; ADR-0017 decision 3 makes them one string", got, set.Namespace)
	}
}

// TestTemplateCarriesNoUnsubstitutedHoles guards the substitution itself: if
// the renderer grew a third input, the two tests above would silently compare
// strings that still contain a hole.
func TestTemplateCarriesNoUnsubstitutedHoles(t *testing.T) {
	template := resourcesTemplate(t, "412", testSHA)
	if strings.Contains(template, "<<") {
		t.Errorf("the template still holds an input this contract does not know about:\n%s", template)
	}
}

// TestPreviewSetIsWhatTheKustomizationBuilds: the template says `path: ./`, so
// every file in the artifact has to be at its root — which the packaging test
// asserts from the other side.
func TestPreviewSetIsWhatTheKustomizationBuilds(t *testing.T) {
	template := resourcesTemplate(t, "412", testSHA)
	if got := field(t, template, "path"); got != "./" {
		t.Errorf("the Kustomization builds %q; the artifact is a flat set at its root", got)
	}
}

// field reads the first value of a key out of the substituted template. The
// template is generated text, not a document to model, so a line scan is the
// honest way to read it.
func field(t *testing.T, template, key string) string {
	t.Helper()
	for _, line := range strings.Split(template, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok || k != key {
			continue
		}
		return strings.TrimSpace(v)
	}
	t.Fatalf("the template has no %s:\n%s", key, template)
	return ""
}
