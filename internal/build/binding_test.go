package build_test

import (
	"strings"
	"testing"

	"github.com/dafrie/kelson/internal/build"
	"github.com/dafrie/kelson/internal/model"
)

// The other half of the shared plan (ADR-0035 decision 4): which repository a
// build clones, now that the answer is the components' rather than the
// Project's. Both callers run these functions, so what is asserted here is
// again the agreement — the same spec clones the same repository at the same
// ref through the same connection, and is refused with the same code, whichever
// one asked.
//
// The bindings are produced by model.Resolve rather than hand-built wherever
// the spelling is the point: what makes `sources:` work is that resolution
// answers the singular and the plural identically, and a fixture that wrote
// [model.Resolved] by hand would assert this file's opinion of that instead of
// the resolver's.

func projectWith(spec model.ProjectSpec) *model.Project {
	spec.Build = &model.Build{Strategy: model.BuildDockerfile}
	return &model.Project{
		TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindProject},
		Metadata: model.ObjectMeta{Name: "shop"},
		Spec:     spec,
	}
}

func productionEnvironment() *model.Environment {
	return &model.Environment{
		TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindEnvironment},
		Metadata: model.ObjectMeta{Name: "production"},
		Spec:     model.EnvironmentSpec{Project: "shop"},
	}
}

func resolveFor(t *testing.T, spec model.ProjectSpec, globals ...model.Source) *model.Resolved {
	t.Helper()
	resolved, errs := model.Resolve(projectWith(spec), productionEnvironment(), globals...)
	if len(errs) > 0 {
		t.Fatalf("Resolve: %v", errs)
	}
	return resolved
}

// The singular spelling is the plural with one entry, so the build path must
// not be able to tell them apart — which is the whole of "nothing existing
// changes meaning" (ADR-0035 decision 1).
func TestSourceToBuildReadsBothSpellings(t *testing.T) {
	singular := resolveFor(t, model.ProjectSpec{
		Source:     &model.Source{Git: "https://github.com/acme/shop.git", Ref: "main"},
		Components: []model.Component{{Name: "web", Port: 8080}},
	})
	plural := resolveFor(t, model.ProjectSpec{
		Sources: []model.Source{{
			Name: model.DefaultSourceName, Git: "https://github.com/acme/shop.git", Ref: "main",
		}},
		Components: []model.Component{{Name: "web", Port: 8080}},
	})

	for name, resolved := range map[string]*model.Resolved{"source": singular, "sources": plural} {
		t.Run(name, func(t *testing.T) {
			binding, err := build.SourceToBuild("shop", resolved)
			if err != nil {
				t.Fatalf("SourceToBuild: %v", err)
			}
			if binding.Source.Git != "https://github.com/acme/shop.git" || binding.Source.Ref != "main" {
				t.Errorf("git/ref = %q/%q, want the declared repository at main", binding.Source.Git, binding.Source.Ref)
			}
			if binding.Source.Name != model.DefaultSourceName {
				t.Errorf("name = %q, want %q", binding.Source.Name, model.DefaultSourceName)
			}
			if len(binding.Components) != 1 || binding.Components[0] != "web" {
				t.Errorf("components = %v, want [web]", binding.Components)
			}
		})
	}
}

// A component that names a source builds from *that* one — repository, ref and
// connection — and not from the project's default, which is the whole of
// ADR-0035 decision 4 at the build plane.
func TestSourceToBuildFollowsTheComponentsBinding(t *testing.T) {
	resolved := resolveFor(t, model.ProjectSpec{
		Sources: []model.Source{
			{Name: "default", Git: "https://github.com/acme/shop.git", Ref: "main"},
			{Name: "tools", Git: "https://gitlab.com/acme/build-tools.git", Ref: "v2", Connection: "acme-gitlab"},
		},
		Components: []model.Component{
			{Name: "worker", Source: &model.ComponentSource{Name: "tools"}},
		},
	})

	binding, err := build.SourceToBuild("shop", resolved)
	if err != nil {
		t.Fatalf("SourceToBuild: %v", err)
	}
	if binding.Source.Git != "https://gitlab.com/acme/build-tools.git" {
		t.Errorf("git = %q, want the bound source's repository and not the project's default", binding.Source.Git)
	}
	if binding.Source.Ref != "v2" {
		t.Errorf("ref = %q, want the bound source's own ref", binding.Source.Ref)
	}
	if binding.Source.Connection != "acme-gitlab" {
		t.Errorf("connection = %q, want the bound source's own connection", binding.Source.Connection)
	}
}

// A GitSource the instance offers is a source like any other: the build path
// receives it through the resolver's global tier and never learns where it came
// from.
func TestSourceToBuildResolvesAGlobalSource(t *testing.T) {
	global := (&model.GitSource{
		Metadata: model.ObjectMeta{Name: "platform"},
		Spec:     model.GitSourceSpec{Git: "https://github.com/acme/platform.git", Ref: "release"},
	}).AsSource()

	resolved := resolveFor(t, model.ProjectSpec{
		Components: []model.Component{{Name: "web", Port: 8080, Source: &model.ComponentSource{Name: "platform"}}},
	}, global)

	binding, err := build.SourceToBuild("shop", resolved)
	if err != nil {
		t.Fatalf("SourceToBuild: %v", err)
	}
	if binding.Source.Git != "https://github.com/acme/platform.git" || binding.Source.Ref != "release" {
		t.Errorf("git/ref = %q/%q, want the instance's GitSource", binding.Source.Git, binding.Source.Ref)
	}
}

// Two components on one source are one entry, hence one clone. This is the
// sharing rule pinned: it is decided by the source's name, so a build serves
// every component bound to it and the executor is asked for one Job.
func TestBindingsShareOneSourceAcrossComponents(t *testing.T) {
	resolved := resolveFor(t, model.ProjectSpec{
		Source: &model.Source{Git: "https://github.com/acme/shop.git", Ref: "main"},
		Components: []model.Component{
			{Name: "web", Port: 8080},
			{Name: "worker"},
		},
	})

	bindings := build.Bindings(resolved)
	if len(bindings) != 1 {
		t.Fatalf("got %d bindings, want one shared source: %+v", len(bindings), bindings)
	}
	if got := bindings[0].Components; len(got) != 2 || got[0] != "web" || got[1] != "worker" {
		t.Errorf("components = %v, want [web worker] in spec order", got)
	}
}

// Two names are two entries even when they name the same URL: they are two
// declarations and may carry different refs, and collapsing them would make a
// build's inputs depend on comparing URL spellings.
func TestBindingsKeepDistinctNamesApart(t *testing.T) {
	resolved := resolveFor(t, model.ProjectSpec{
		Sources: []model.Source{
			{Name: "app", Git: "https://github.com/acme/shop.git", Ref: "main"},
			{Name: "app-next", Git: "https://github.com/acme/shop.git", Ref: "next"},
		},
		Components: []model.Component{
			{Name: "web", Port: 8080, Source: &model.ComponentSource{Name: "app"}},
			{Name: "canary", Source: &model.ComponentSource{Name: "app-next"}},
		},
	})

	if got := build.Bindings(resolved); len(got) != 2 {
		t.Fatalf("got %d bindings, want one per declared source: %+v", len(got), got)
	}
}

// The refusals, each by code so a caller branches rather than reading prose.
func TestSourceToBuildRefusals(t *testing.T) {
	t.Run("nothing bound is build/no-source", func(t *testing.T) {
		resolved := resolveFor(t, model.ProjectSpec{
			Image:      "ghcr.io/acme/shop:1.0.0",
			Components: []model.Component{{Name: "web", Port: 8080}},
		})
		_, err := build.SourceToBuild("shop", resolved)
		refusal := reasonOf(t, err)
		if refusal.Reason != build.ReasonNoSource {
			t.Fatalf("reason = %q, want %q", refusal.Reason, build.ReasonNoSource)
		}
		if !strings.Contains(refusal.Remediation, "spec.sources") {
			t.Errorf("the remediation should name the field that fixes it: %s", refusal.Remediation)
		}
	})

	t.Run("several bound is build/several-sources", func(t *testing.T) {
		resolved := resolveFor(t, model.ProjectSpec{
			Sources: []model.Source{
				{Name: "app", Git: "https://github.com/acme/shop.git", Ref: "main"},
				{Name: "tools", Git: "https://github.com/acme/build-tools.git", Ref: "v2"},
			},
			Components: []model.Component{
				{Name: "web", Port: 8080, Source: &model.ComponentSource{Name: "app"}},
				{Name: "worker", Source: &model.ComponentSource{Name: "tools"}},
			},
		})
		_, err := build.SourceToBuild("shop", resolved)
		refusal := reasonOf(t, err)
		if refusal.Reason != build.ReasonSeveralSources {
			t.Fatalf("reason = %q, want %q", refusal.Reason, build.ReasonSeveralSources)
		}
		// The refusal has to be actionable without opening the spec, so it
		// names both sources and the component on each.
		for _, want := range []string{"app", "tools", "web", "worker"} {
			if !strings.Contains(refusal.Message, want) {
				t.Errorf("the message should name %q: %s", want, refusal.Message)
			}
		}
		if !strings.Contains(refusal.Remediation, "spec.build.by: ci") {
			t.Errorf("the remediation should point at the path that does produce per-component images: %s",
				refusal.Remediation)
		}
	})
}

// A ref is per source; --ref (and BuildRequest.ref) is per build and wins.
func TestSourceRefOverride(t *testing.T) {
	binding := build.SourceBinding{Source: model.ResolvedSource{Ref: "develop"}}
	if got := binding.SourceRef(""); got != "develop" {
		t.Errorf("SourceRef(\"\") = %q, want the source's own ref", got)
	}
	if got := binding.SourceRef("v1.4.0"); got != "v1.4.0" {
		t.Errorf("SourceRef override = %q, want v1.4.0", got)
	}
	if got := binding.SourceRef("  "); got != "develop" {
		t.Errorf("a blank override = %q, want the source's own ref", got)
	}
}
