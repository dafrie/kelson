package renderer

import (
	"strings"
	"testing"

	"github.com/dafrie/kelson/internal/model"
)

// buildFromSource is the spec shape every project under examples/ that builds
// its own image has: source + build, no image anywhere. Resolution leaves such
// components carrying model.ImageUnresolved.
func buildFromSource() *model.Resolved {
	project := &model.Project{
		TypeMeta: model.TypeMeta{APIVersion: "kelson.dev/v1alpha1", Kind: "Project"},
		Metadata: model.ObjectMeta{Name: "checkout"},
		Spec: model.ProjectSpec{
			Source: &model.Source{Git: "https://github.com/acme/checkout", Ref: "main"},
			Build:  &model.Build{Strategy: model.BuildDockerfile},
			Components: []model.Component{
				{Name: "web", Port: 8080, Health: "/healthz"},
				{Name: "worker", Command: []string{"bundle", "exec", "sidekiq"}},
			},
		},
	}
	environment := &model.Environment{
		TypeMeta: model.TypeMeta{APIVersion: "kelson.dev/v1alpha1", Kind: "Environment"},
		Metadata: model.ObjectMeta{Name: "production"},
		Spec:     model.EnvironmentSpec{Project: "checkout", Namespace: "checkout-production"},
	}
	resolved, errs := model.Resolve(project, environment)
	if len(errs) > 0 {
		panic("build-from-source fixture does not resolve: " + errs.Error())
	}
	return resolved
}

// TestRenderRejectsUnresolvedImage is issue #136: a spec whose image comes from
// a build the CLI has not run must fail loudly. Emitting the placeholder
// produced `image: "@"`, a manifest no cluster accepts, from a command that
// exited 0.
func TestRenderRejectsUnresolvedImage(t *testing.T) {
	resolved := buildFromSource()
	if got := resolved.Components[0].Image; got != model.ImageUnresolved {
		t.Fatalf("fixture precondition: image = %q, want the unresolved sentinel", got)
	}

	manifests, err := Render(resolved, gatewayProfile(), nil)
	if err == nil {
		t.Fatalf("expected an error, rendered %d manifests", len(manifests))
	}
	if manifests != nil {
		t.Fatalf("a failed render must emit nothing, got %d manifests", len(manifests))
	}
	rerrs, ok := err.(Errors)
	if !ok {
		t.Fatalf("expected renderer.Errors, got %#v", err)
	}
	// Both components are reported: one run lists all the work.
	if len(rerrs) != 2 {
		t.Fatalf("expected one error per component, got %d: %v", len(rerrs), rerrs)
	}
	for i, want := range []string{"web", "worker"} {
		e := rerrs[i]
		if e.Code != ErrImageUnresolved || e.Component != want {
			t.Fatalf("error %d: got %#v, want code %s for component %s", i, e, ErrImageUnresolved, want)
		}
		if e.Remediation == "" {
			t.Fatalf("error %d has no remediation: %#v", i, e)
		}
		if !strings.Contains(e.Error(), "--image") {
			t.Fatalf("error %d should point at the way out: %s", i, e.Error())
		}
	}
}

// TestRenderPromotionShape is the half of the promotion fixture no golden file
// can hold (testdata/render/image-pin-promotion): the same built-from-source
// Project, rendered for the environment that was promoted to and for the one
// that was not. The pin resolves the digest exactly the way `--image` does; the
// unpinned environment still fails with image/unresolved, which is what makes a
// promotion a deliberate act rather than a side effect of a build (ADR-0016).
func TestRenderPromotionShape(t *testing.T) {
	const pinned = "ghcr.io/acme/checkout@sha256:9f6ad2c1b4d5e8073a1c2f4b6d8e0a1c3e5f7091b2d4c6e8a0f2b4d6c8e0a2f4"
	project := &model.Project{
		TypeMeta: model.TypeMeta{APIVersion: "kelson.dev/v1alpha1", Kind: "Project"},
		Metadata: model.ObjectMeta{Name: "checkout"},
		Spec: model.ProjectSpec{
			Source:     &model.Source{Git: "https://github.com/acme/checkout", Ref: "main"},
			Build:      &model.Build{Strategy: model.BuildDockerfile},
			Components: []model.Component{{Name: "web", Port: 8080, Health: "/healthz"}},
		},
	}
	environment := func(name string, overrides ...model.ComponentOverride) *model.Environment {
		return &model.Environment{
			TypeMeta: model.TypeMeta{APIVersion: "kelson.dev/v1alpha1", Kind: "Environment"},
			Metadata: model.ObjectMeta{Name: name},
			Spec:     model.EnvironmentSpec{Project: "checkout", Namespace: "checkout-" + name, Components: overrides},
		}
	}

	production, errs := model.Resolve(project, environment("production", model.ComponentOverride{Name: "web", Image: pinned}))
	if len(errs) > 0 {
		t.Fatalf("promoted environment does not resolve: %v", errs)
	}
	manifests, err := Render(production, gatewayProfile(), nil)
	if err != nil {
		t.Fatalf("promoted environment failed to render: %v", err)
	}
	out, err := Encode(manifests)
	if err != nil {
		t.Fatalf("encode failed: %v", err)
	}
	if !strings.Contains(string(out), "image: "+pinned) {
		t.Fatalf("the pinned digest did not reach the container:\n%s", out)
	}

	staging, errs := model.Resolve(project, environment("staging"))
	if len(errs) > 0 {
		t.Fatalf("unpinned environment does not resolve: %v", errs)
	}
	_, err = Render(staging, gatewayProfile(), nil)
	rerrs, ok := err.(Errors)
	if !ok || len(rerrs) != 1 || rerrs[0].Code != ErrImageUnresolved || rerrs[0].Component != "web" {
		t.Fatalf("an unpinned environment must still fail with %s, got %#v", ErrImageUnresolved, err)
	}
}

// TestRenderRejectsEmptyImage: validation rejects a component with no image
// source, so an empty image reaching the renderer is a caller bug — but it
// still must not render `image: ""`.
func TestRenderRejectsEmptyImage(t *testing.T) {
	resolved := resolvedFixture()
	resolved.Components[1].Image = ""
	_, err := Render(resolved, gatewayProfile(), nil)
	rerrs, ok := err.(Errors)
	if !ok || len(rerrs) != 1 || rerrs[0].Code != ErrImageUnresolved || rerrs[0].Component != "worker" {
		t.Fatalf("expected one image/unresolved error for worker, got %#v", err)
	}
}

// TestRenderAcceptsSuppliedBuildImage: the same build-path spec renders once the
// built artifact is supplied (what `--image` does at the CLI), and the digest
// reaches every container verbatim.
func TestRenderAcceptsSuppliedBuildImage(t *testing.T) {
	const ref = "ghcr.io/acme/checkout@sha256:9f6ad2c1b4d5e8073a1c2f4b6d8e0a1c3e5f7091b2d4c6e8a0f2b4d6c8e0a2f4"
	resolved := buildFromSource()
	for i := range resolved.Components {
		resolved.Components[i].Image = ref
	}
	manifests, err := Render(resolved, gatewayProfile(), nil)
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}
	out, err := Encode(manifests)
	if err != nil {
		t.Fatalf("encode failed: %v", err)
	}
	if strings.Contains(string(out), `image: "@"`) {
		t.Fatalf("placeholder reached the output:\n%s", out)
	}
	if got := strings.Count(string(out), "image: "+ref); got != 2 {
		t.Fatalf("expected the supplied digest on both workloads, found %d:\n%s", got, out)
	}
}
