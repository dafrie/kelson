package renderer

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestRenderNamespace is issue #150's acceptance: a render that targets a
// namespace also declares it, exactly once and first, carrying the ownership
// marker issue #59 needs to tell a namespace kelson declares from one it merely
// deploys into.
func TestRenderNamespace(t *testing.T) {
	ms, err := Render(resolvedFixture(), gatewayProfile(), nil)
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}

	var found []int
	for i, m := range ms {
		if m.Kind == "Namespace" {
			found = append(found, i)
		}
	}
	if len(found) != 1 {
		t.Fatalf("expected exactly one Namespace, got %d at %v", len(found), found)
	}
	if found[0] != 0 {
		t.Fatalf("Namespace must sort first, got index %d in %v", found[0], kinds(ms))
	}

	ns := ms[0]
	if ns.APIVersion != "v1" || ns.Name != "checkout-prod" {
		t.Fatalf("Namespace identity = %s %s, want v1 checkout-prod", ns.APIVersion, ns.Name)
	}
	// A cluster-scoped resource carries no namespace of its own; the delivery
	// plane would have to strip one, and kubectl would show it as a lie.
	if ns.Namespace != "" {
		t.Fatalf("Namespace manifest must not be namespaced, got %q", ns.Namespace)
	}

	body, err := ns.YAML()
	if err != nil {
		t.Fatalf("YAML failed: %v", err)
	}
	var doc struct {
		Metadata struct {
			Name        string            `yaml:"name"`
			Namespace   string            `yaml:"namespace"`
			Labels      map[string]string `yaml:"labels"`
			Annotations map[string]string `yaml:"annotations"`
		} `yaml:"metadata"`
	}
	if err := yaml.Unmarshal(body, &doc); err != nil {
		t.Fatalf("rendered Namespace does not parse: %v", err)
	}
	if doc.Metadata.Name != "checkout-prod" {
		t.Fatalf("metadata.name = %q, want checkout-prod", doc.Metadata.Name)
	}
	if doc.Metadata.Namespace != "" {
		t.Fatalf("metadata.namespace must be absent, got %q", doc.Metadata.Namespace)
	}
	if got := doc.Metadata.Annotations[AnnNamespaceOwnership]; got != NamespaceOwnershipDeclared {
		t.Fatalf("%s = %q, want %q", AnnNamespaceOwnership, got, NamespaceOwnershipDeclared)
	}
	// The direct adapter refuses to apply anything without these, and prunes
	// only by them (internal/delivery/direct).
	for key, want := range map[string]string{
		"app.kubernetes.io/managed-by": "kelson",
		"kelson.dev/project":           "checkout",
		"kelson.dev/environment":       "production",
	} {
		if got := doc.Metadata.Labels[key]; got != want {
			t.Fatalf("label %s = %q, want %q", key, got, want)
		}
	}
	// The Namespace spans every application, so claiming one would be wrong.
	if got, ok := doc.Metadata.Labels["kelson.dev/application"]; ok {
		t.Fatalf("Namespace must not claim an application, got %q", got)
	}
	if !strings.Contains(string(body), "kelson.dev/spec-hash: sha256:") {
		t.Fatalf("Namespace is missing a spec-hash:\n%s", body)
	}
}

// TestRenderNamespaceHashIsIndependent: the Namespace's spec-hash covers only
// what the Namespace document is built from, so editing an application must not
// churn it.
func TestRenderNamespaceHashIsIndependent(t *testing.T) {
	before, err := namespaceHash(resolvedFixture())
	if err != nil {
		t.Fatalf("namespaceHash failed: %v", err)
	}
	changed := resolvedFixture()
	changed.Components[0].Image = "ghcr.io/acme/checkout:9.9.9"
	after, err := namespaceHash(changed)
	if err != nil {
		t.Fatalf("namespaceHash failed: %v", err)
	}
	if before != after {
		t.Fatalf("an application edit changed the Namespace spec-hash: %s -> %s", before, after)
	}

	renamed := resolvedFixture()
	renamed.Environment.Namespace = "checkout-staging"
	other, err := namespaceHash(renamed)
	if err != nil {
		t.Fatalf("namespaceHash failed: %v", err)
	}
	if other == before {
		t.Fatalf("renaming the namespace left the spec-hash unchanged: %s", other)
	}
}
