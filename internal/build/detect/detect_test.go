package detect

import (
	"testing"
	"testing/fstest"

	"github.com/dafrie/kelson/internal/model"
)

func TestDetect(t *testing.T) {
	tests := []struct {
		name     string
		build    *model.Build
		files    map[string]string
		want     Strategy
		reason   Reason
		evidence string
	}{
		{
			name:     "dockerfile at context root",
			files:    map[string]string{"Dockerfile": "FROM scratch"},
			want:     StrategyDockerfile,
			reason:   ReasonDockerfileFound,
			evidence: "Dockerfile",
		},
		{
			name: "dockerfile at non-root context (monorepo)",
			files: map[string]string{
				"Dockerfile":   "FROM node:20",
				"package.json": "{}",
			},
			want:     StrategyDockerfile,
			reason:   ReasonDockerfileFound,
			evidence: "Dockerfile",
		},
		{
			name:     "explicit strategy overrides a present dockerfile",
			build:    &model.Build{Strategy: model.BuildBuildpacks},
			files:    map[string]string{"Dockerfile": "FROM scratch"},
			want:     StrategyBuildpacks,
			reason:   ReasonExplicit,
			evidence: "spec.build.strategy",
		},
		{
			name:     "explicit strategy none builds nothing",
			build:    &model.Build{Strategy: model.BuildNone},
			files:    map[string]string{"Dockerfile": "FROM scratch", "package.json": "{}"},
			want:     StrategyNone,
			reason:   ReasonExplicit,
			evidence: "spec.build.strategy",
		},
		{
			name:     "explicit dockerfile",
			build:    &model.Build{Strategy: model.BuildDockerfile},
			files:    map[string]string{},
			want:     StrategyDockerfile,
			reason:   ReasonExplicit,
			evidence: "spec.build.strategy",
		},
		{
			name:     "language signal package.json",
			files:    map[string]string{"package.json": "{}"},
			want:     StrategyBuildpacks,
			reason:   ReasonLanguageSignal,
			evidence: "package.json",
		},
		{
			name:     "language signal go.mod",
			files:    map[string]string{"go.mod": "module x"},
			want:     StrategyBuildpacks,
			reason:   ReasonLanguageSignal,
			evidence: "go.mod",
		},
		{
			name:     "language signal requirements.txt",
			files:    map[string]string{"requirements.txt": ""},
			want:     StrategyBuildpacks,
			reason:   ReasonLanguageSignal,
			evidence: "requirements.txt",
		},
		{
			name:     "language signal pyproject.toml",
			files:    map[string]string{"pyproject.toml": "[project]"},
			want:     StrategyBuildpacks,
			reason:   ReasonLanguageSignal,
			evidence: "pyproject.toml",
		},
		{
			name:     "language signal Gemfile",
			files:    map[string]string{"Gemfile": "source :rubygems"},
			want:     StrategyBuildpacks,
			reason:   ReasonLanguageSignal,
			evidence: "Gemfile",
		},
		{
			name:     "language signal pom.xml",
			files:    map[string]string{"pom.xml": "<project/>"},
			want:     StrategyBuildpacks,
			reason:   ReasonLanguageSignal,
			evidence: "pom.xml",
		},
		{
			name:     "language signal build.gradle",
			files:    map[string]string{"build.gradle": ""},
			want:     StrategyBuildpacks,
			reason:   ReasonLanguageSignal,
			evidence: "build.gradle",
		},
		{
			name:     "language signal composer.json",
			files:    map[string]string{"composer.json": "{}"},
			want:     StrategyBuildpacks,
			reason:   ReasonLanguageSignal,
			evidence: "composer.json",
		},
		{
			name:   "no signal at all",
			files:  map[string]string{"README.md": "hi"},
			want:   StrategyBuildpacks,
			reason: ReasonNoSignal,
		},
		{
			name:     "dockerfile named via build.dockerfile at custom path",
			build:    &model.Build{Dockerfile: "docker/web.Dockerfile"},
			files:    map[string]string{"docker/web.Dockerfile": "FROM node:20"},
			want:     StrategyDockerfile,
			reason:   ReasonDockerfileFound,
			evidence: "docker/web.Dockerfile",
		},
		{
			name:     "nil build defaults to auto detection",
			files:    map[string]string{"Gemfile": "x"},
			want:     StrategyBuildpacks,
			reason:   ReasonLanguageSignal,
			evidence: "Gemfile",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tree := fstest.MapFS{}
			for path, content := range tt.files {
				tree[path] = &fstest.MapFile{Data: []byte(content)}
			}

			got, err := Detect(tt.build, tree)
			if err != nil {
				t.Fatalf("Detect() error: %v", err)
			}
			if got.Strategy != tt.want {
				t.Errorf("Strategy = %q, want %q", got.Strategy, tt.want)
			}
			if got.Reason != tt.reason {
				t.Errorf("Reason = %q, want %q", got.Reason, tt.reason)
			}
			if got.Evidence != tt.evidence {
				t.Errorf("Evidence = %q, want %q", got.Evidence, tt.evidence)
			}
			if got.Message == "" {
				t.Error("Message is empty")
			}
		})
	}
}

// TestStructuredReason is the acceptance criterion for issue #50: an agent must
// be able to answer "why this strategy?" from the typed fields alone. It runs a
// real detection and asserts Strategy, Reason and Evidence exactly — not that
// Message happens to contain some words. The context is a monorepo sub-tree and
// the Dockerfile is addressed at a nested, non-default path, so this also pins
// the concrete decider (the evidence path) rather than the default.
func TestStructuredReason(t *testing.T) {
	tree := fstest.MapFS{
		"apps/web/Dockerfile":   &fstest.MapFile{Data: []byte("FROM node:20")},
		"apps/web/package.json": &fstest.MapFile{Data: []byte("{}")},
	}

	got, err := Detect(&model.Build{Dockerfile: "apps/web/Dockerfile"}, tree)
	if err != nil {
		t.Fatalf("Detect() error: %v", err)
	}

	if got.Strategy != StrategyDockerfile {
		t.Errorf("Strategy = %q, want %q", got.Strategy, StrategyDockerfile)
	}
	if got.Reason != ReasonDockerfileFound {
		t.Errorf("Reason = %q, want %q", got.Reason, ReasonDockerfileFound)
	}
	if got.Evidence != "apps/web/Dockerfile" {
		t.Errorf("Evidence = %q, want %q", got.Evidence, "apps/web/Dockerfile")
	}
}
