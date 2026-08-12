package detect

import (
	"errors"
	"fmt"
	"io/fs"

	"github.com/dafrie/kelson/internal/model"
)

// Strategy is the resolved build strategy. Its values are exactly the ids a
// build.Builder returns from Name() (internal/build/build.go), so a Detection
// maps directly onto a builder with no string translation.
type Strategy string

const (
	StrategyDockerfile Strategy = "dockerfile"
	StrategyBuildpacks Strategy = "buildpacks"
	StrategyNone       Strategy = "none"
)

// Reason is a stable, machine-actionable code for why a strategy was chosen.
// The taxonomy is a compatibility promise, same as model.Code. An agent
// branches on this field and never parses Message.
type Reason string

const (
	// ReasonExplicit means build.strategy was set in the spec; it always wins
	// (ADR-0010 rule 1). Evidence is the spec field path.
	ReasonExplicit Reason = "explicit-override"
	// ReasonDockerfileFound means a Dockerfile was present at the build context
	// root (or at build.dockerfile); it selects dockerfile (ADR-0010 rule 2).
	// Evidence is the Dockerfile path.
	ReasonDockerfileFound Reason = "dockerfile-found"
	// ReasonLanguageSignal means no Dockerfile was found but a language signal
	// (package.json, go.mod, ...) was, so buildpacks is used. Evidence is the
	// signal path.
	ReasonLanguageSignal Reason = "language-signal"
	// ReasonNoSignal means there was neither a Dockerfile nor a language
	// signal. "No signal" is a real, reportable answer, not a silent default.
	// Evidence is empty.
	ReasonNoSignal Reason = "no-signal"
)

// Detection is the outcome of choosing a build strategy. Strategy, Reason and
// Evidence are the machine contract; Message is derived from them for humans
// rather than replacing them.
type Detection struct {
	Strategy Strategy `json:"strategy"`
	Reason   Reason   `json:"reason"`
	// Evidence is the concrete path or field that decided the choice: the
	// Dockerfile path, the language-signal path, or the spec field
	// "spec.build.strategy" for an explicit override. Empty for no-signal.
	Evidence string `json:"evidence,omitempty"`
	// Message is a human-readable explanation built from the fields above.
	Message string `json:"message"`
}

// defaultDockerfile is the path used when build.dockerfile is not set
// (internal/build/build.go: ContextDir-relative, empty means ./Dockerfile).
const defaultDockerfile = "Dockerfile"

// explicitField is the evidence reported when the spec overrides detection.
const explicitField = "spec.build.strategy"

// Detect resolves the build strategy for a build context. build is the spec's
// build section (nil means the user wrote none, equivalent to auto); tree is
// the build context as a filesystem root, which need not be a repository root.
func Detect(build *model.Build, tree fs.FS) (Detection, error) {
	strategy := model.BuildAuto
	if build != nil && build.Strategy != "" {
		strategy = build.Strategy
	}

	switch strategy {
	case model.BuildDockerfile:
		return explicit(StrategyDockerfile), nil
	case model.BuildBuildpacks:
		return explicit(StrategyBuildpacks), nil
	case model.BuildNone:
		return explicit(StrategyNone), nil
	}

	dockerfile := defaultDockerfile
	if build != nil && build.Dockerfile != "" {
		dockerfile = build.Dockerfile
	}

	present, err := exists(tree, dockerfile)
	if err != nil {
		return Detection{}, err
	}
	if present {
		return Detection{
			Strategy: StrategyDockerfile,
			Reason:   ReasonDockerfileFound,
			Evidence: dockerfile,
			Message:  fmt.Sprintf("strategy %s selected: found Dockerfile at %s", StrategyDockerfile, dockerfile),
		}, nil
	}

	for _, signal := range languageSignals {
		present, err := exists(tree, signal)
		if err != nil {
			return Detection{}, err
		}
		if present {
			return Detection{
				Strategy: StrategyBuildpacks,
				Reason:   ReasonLanguageSignal,
				Evidence: signal,
				Message:  fmt.Sprintf("strategy %s selected: no Dockerfile; found %s", StrategyBuildpacks, signal),
			}, nil
		}
	}

	return Detection{
		Strategy: StrategyBuildpacks,
		Reason:   ReasonNoSignal,
		Message:  fmt.Sprintf("strategy %s selected: no Dockerfile and no language signal found", StrategyBuildpacks),
	}, nil
}

func explicit(strategy Strategy) Detection {
	return Detection{
		Strategy: strategy,
		Reason:   ReasonExplicit,
		Evidence: explicitField,
		Message:  fmt.Sprintf("strategy %s selected: explicit %s=%s", strategy, explicitField, strategy),
	}
}

// languageSignals is an ordered, deterministic probe list for the buildpacks
// case. Order is deliberate; it is never a map so detection cannot vary by
// iteration order. The first present signal is the one reported.
var languageSignals = []string{
	"package.json",
	"go.mod",
	"requirements.txt",
	"pyproject.toml",
	"Gemfile",
	"pom.xml",
	"build.gradle",
	"composer.json",
}

// exists reports whether path is present in tree. A missing file is "not
// found", not an error; anything else (e.g. an unreadable tree) is returned.
func exists(tree fs.FS, path string) (bool, error) {
	_, err := fs.Stat(tree, path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
}
