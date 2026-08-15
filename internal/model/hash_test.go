package model_test

import (
	"testing"

	"github.com/dafrie/kelson/internal/model"
)

// SpecHash is half of an artifact tag, so what has to hold is exactly two
// things: the same resolved spec always hashes the same, and any change to what
// renders moves it.

func resolvedFixture(t *testing.T) *model.Resolved {
	t.Helper()
	p := &model.Project{
		TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindProject},
		Metadata: model.ObjectMeta{Name: "checkout"},
		Spec: model.ProjectSpec{
			Image: "ghcr.io/acme/checkout:1.0.0",
			Env:   map[string]model.EnvValue{"LOG_LEVEL": {Literal: "info"}, "REGION": {Literal: "eu"}},
			Components: []model.Component{
				{Name: "web", Port: 8080},
				{Name: "worker", Kind: model.ComponentWorker},
			},
		},
	}
	e := &model.Environment{
		TypeMeta: model.TypeMeta{APIVersion: model.APIVersion, Kind: model.KindEnvironment},
		Metadata: model.ObjectMeta{Name: "production"},
		Spec:     model.EnvironmentSpec{Project: "checkout"},
	}
	resolved, errs := model.Resolve(p, e)
	if len(errs) > 0 {
		t.Fatalf("resolving the fixture: %v", errs)
	}
	return resolved
}

// TestSpecHashIsDeterministic: two resolutions of the same documents must hash
// identically, or an unchanged spec would publish a new artifact on every
// reconcile.
func TestSpecHashIsDeterministic(t *testing.T) {
	first, err := model.SpecHash(resolvedFixture(t))
	if err != nil {
		t.Fatalf("SpecHash: %v", err)
	}
	for range 5 {
		got, err := model.SpecHash(resolvedFixture(t))
		if err != nil {
			t.Fatalf("SpecHash: %v", err)
		}
		if got != first {
			t.Fatalf("the same resolved spec hashed to %s and %s", first, got)
		}
	}
	if len(first) != 64 {
		t.Errorf("hash %q is %d characters, want a full sha256", first, len(first))
	}
}

// TestSpecHashMovesWithTheSpec is the other half: a hash that did not move
// would let a changed spec republish under a tag holding the old bytes.
func TestSpecHashMovesWithTheSpec(t *testing.T) {
	base, err := model.SpecHash(resolvedFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		mutate func(*model.Resolved)
	}{
		{"the image", func(r *model.Resolved) { r.Components[0].Image = "ghcr.io/acme/checkout:2.0.0" }},
		{"the namespace", func(r *model.Resolved) { r.Environment.Namespace = "elsewhere" }},
		{"a port", func(r *model.Resolved) { r.Components[0].Port = 9090 }},
		{"an env value", func(r *model.Resolved) { r.Components[0].Env["LOG_LEVEL"] = model.EnvValue{Literal: "debug"} }},
		{"the replica floor", func(r *model.Resolved) { r.Components[0].Replicas = model.Replicas{Min: 3} }},
		{"the component order", func(r *model.Resolved) {
			r.Components[0], r.Components[1] = r.Components[1], r.Components[0]
		}},
		{"the project name", func(r *model.Resolved) { r.Project = "checkout-2" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := resolvedFixture(t)
			tc.mutate(r)
			got, err := model.SpecHash(r)
			if err != nil {
				t.Fatal(err)
			}
			if got == base {
				t.Errorf("changing %s did not move the spec hash", tc.name)
			}
		})
	}
}

func TestSpecHashRefusesNothing(t *testing.T) {
	if _, err := model.SpecHash(nil); err == nil {
		t.Error("a nil resolved spec hashed to something")
	}
}

func TestShortHash(t *testing.T) {
	if got := model.ShortHash("0123456789abcdef"); got != "01234567" {
		t.Errorf("ShortHash = %q, want the leading 8", got)
	}
	// A truncation bug must not become a panic inside a reconcile loop.
	if got := model.ShortHash("abc"); got != "abc" {
		t.Errorf("ShortHash of a short hash = %q, want it whole", got)
	}
}
