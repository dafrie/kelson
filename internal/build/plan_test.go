package build_test

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/dafrie/kelson/internal/build"
	"github.com/dafrie/kelson/internal/build/detect"
	"github.com/dafrie/kelson/internal/model"
)

// This is the half of the build plan both callers share (cmd/kelson's `build`
// and internal/api's BuildService), so what is asserted here is the agreement
// itself: the same spec picks the same strategy, is refused with the same code,
// and derives the same tag no matter which one asked.

var (
	treeWithDockerfile fs.FS = fstest.MapFS{"Dockerfile": {Data: []byte("FROM scratch\n")}}
	treeWithGoMod      fs.FS = fstest.MapFS{"go.mod": {Data: []byte("module x\n")}}
)

// reasonOf unwraps the build plane's structured refusal, failing the test if
// the error is anything else — the point of the taxonomy is that a caller never
// has to read prose to know what happened.
func reasonOf(t *testing.T, err error) build.Error {
	t.Helper()
	var berr build.Error
	if !errors.As(err, &berr) {
		t.Fatalf("want a build.Error, got %v", err)
	}
	return berr
}

func TestResolveStrategyPrecedence(t *testing.T) {
	t.Run("an explicit dockerfile needs no tree", func(t *testing.T) {
		got, err := build.ResolveStrategy(&model.Build{Strategy: model.BuildDockerfile}, nil)
		if err != nil {
			t.Fatalf("ResolveStrategy: %v", err)
		}
		if got.Strategy != detect.StrategyDockerfile || got.Reason != detect.ReasonExplicit {
			t.Errorf("got %q/%q, want dockerfile/explicit-override", got.Strategy, got.Reason)
		}
	})

	t.Run("a Dockerfile in the tree decides", func(t *testing.T) {
		got, err := build.ResolveStrategy(nil, treeWithDockerfile)
		if err != nil {
			t.Fatalf("ResolveStrategy: %v", err)
		}
		if got.Strategy != detect.StrategyDockerfile || got.Reason != detect.ReasonDockerfileFound {
			t.Errorf("got %q/%q, want dockerfile/dockerfile-found", got.Strategy, got.Reason)
		}
	})

	t.Run("auto with no tree is a refusal, not a guess", func(t *testing.T) {
		_, err := build.ResolveStrategy(nil, nil)
		berr := reasonOf(t, err)
		if berr.Reason != build.ReasonDetectionNeedsSource {
			t.Errorf("reason = %q, want %q", berr.Reason, build.ReasonDetectionNeedsSource)
		}
		// The remediation has to serve both callers: a server has no checkout
		// to point -C at, so naming the spec field first is what makes it
		// actionable there, and -C still has to appear for the CLI's reader.
		for _, want := range []string{"spec.build.strategy", "-C", "#50"} {
			if !strings.Contains(berr.Remediation, want) {
				t.Errorf("remediation should mention %q: %s", want, berr.Remediation)
			}
		}
	})

	// buildpacks is no longer a refusal (#49): it is the zero-config default
	// ADR-0010 always said it was, and it resolves however it was reached —
	// named in the spec, or chosen because a tree has a language signal and no
	// Dockerfile.
	t.Run("buildpacks resolves, however it was reached", func(t *testing.T) {
		cases := map[string]struct {
			spec   *model.Build
			tree   fs.FS
			reason detect.Reason
		}{
			"explicit":  {spec: &model.Build{Strategy: model.BuildBuildpacks}, reason: detect.ReasonExplicit},
			"by signal": {tree: treeWithGoMod, reason: detect.ReasonLanguageSignal},
		}
		for name, tc := range cases {
			t.Run(name, func(t *testing.T) {
				got, err := build.ResolveStrategy(tc.spec, tc.tree)
				if err != nil {
					t.Fatalf("ResolveStrategy: %v", err)
				}
				if got.Strategy != detect.StrategyBuildpacks || got.Reason != tc.reason {
					t.Errorf("got %q/%q, want buildpacks/%s", got.Strategy, got.Reason, tc.reason)
				}
			})
		}
	})

	// A tree with neither a Dockerfile nor a language signal is still
	// buildpacks — ADR-0010's rule 3 has no "and only if" — and the reason
	// says the choice was made on no signal rather than pretending to evidence
	// it does not have.
	t.Run("no signal at all still resolves to buildpacks", func(t *testing.T) {
		got, err := build.ResolveStrategy(nil, fstest.MapFS{"README.md": {}})
		if err != nil {
			t.Fatalf("ResolveStrategy: %v", err)
		}
		if got.Strategy != detect.StrategyBuildpacks || got.Reason != detect.ReasonNoSignal {
			t.Errorf("got %q/%q, want buildpacks/no-signal", got.Strategy, got.Reason)
		}
	})

	t.Run("none is nothing to build", func(t *testing.T) {
		_, err := build.ResolveStrategy(&model.Build{Strategy: model.BuildNone}, nil)
		if got := reasonOf(t, err).Reason; got != build.ReasonNothingToBuild {
			t.Errorf("reason = %q, want %q", got, build.ReasonNothingToBuild)
		}
	})
}

// The tag repeats the project name because a kelson build is per-Project and
// has no single application to name, and it shortens the revision because the
// full commit lives in Request.Revision. Both are load-bearing conventions
// (docs/build.md), so they are pinned rather than left to drift.
func TestDestinationTag(t *testing.T) {
	const commit = "0123456789abcdef0123456789abcdef01234567"
	if got, want := build.DestinationTag("shop", commit), "shop-shop-0123456789ab"; got != want {
		t.Errorf("DestinationTag = %q, want %q", got, want)
	}
	// A revision that is already short is used whole, not padded.
	if got, want := build.DestinationTag("shop", "v1.4.0"), "shop-shop-v1.4.0"; got != want {
		t.Errorf("DestinationTag = %q, want %q", got, want)
	}
}

func TestIsCommit(t *testing.T) {
	const commit = "0123456789abcdef0123456789abcdef01234567"
	cases := map[string]bool{
		commit:                  true,
		strings.ToUpper(commit): true,
		strings.Repeat("f", 40): true,
		"main":                  false,
		"v1.4.0":                false,
		commit[:39]:             false,
		commit[:39] + "g":       false,
		strings.Repeat("f", 41): false,
	}
	for ref, want := range cases {
		if got := build.IsCommit(ref); got != want {
			t.Errorf("IsCommit(%q) = %v, want %v", ref, got, want)
		}
	}
	if got := build.NormalizeCommit(strings.ToUpper(commit)); got != commit {
		t.Errorf("NormalizeCommit = %q, want the lowercase commit", got)
	}
}

func TestDockerfilePath(t *testing.T) {
	if got := build.DockerfilePath(nil); got != "" {
		t.Errorf("DockerfilePath(nil) = %q, want the driver's default (empty)", got)
	}
	if got := build.DockerfilePath(&model.Build{Dockerfile: "docker/api.Dockerfile"}); got != "docker/api.Dockerfile" {
		t.Errorf("DockerfilePath = %q", got)
	}
}
