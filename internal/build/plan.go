package build

import (
	"fmt"
	"io/fs"
	"strings"

	"github.com/dafrie/kelson/internal/build/detect"
	"github.com/dafrie/kelson/internal/build/registry"
	"github.com/dafrie/kelson/internal/model"
)

// Planning a build: everything between "here is a spec" and "here is a
// build.Request", with no cluster, no network and no filesystem of its own.
//
// This lives here rather than in cmd/kelson because it has two callers. The CLI
// runs it for `kelson build`; the API server runs it for BuildService.Build
// (internal/api/build.go). The two must agree about which strategy a spec
// selects, what the image is called and what a refusal is called, because a
// user who is told `build/strategy-not-implemented` by one and something else
// by the other has learned nothing. Duplicating it would have made that
// divergence a matter of time.
//
// The functions take an fs.FS rather than a directory path deliberately: the
// only impure step in strategy resolution is opening the tree, and leaving that
// to the caller is what keeps this file testable and importable by both planes
// (the command plane may not touch client-go, this one may not either).

// Reason codes for the build plane's own refusals, as opposed to a failure
// reported by a driver or an executor. They are stable strings so an agent
// branches on the reason instead of matching prose — the same contract
// model.Code and delivery.Code make for their planes, and the same strings
// travel over the API as kelson.v1alpha1.Error codes.
const (
	// ReasonNoSource: the Project names no source repository, so there is
	// nothing to build from.
	ReasonNoSource = "build/no-source"
	// ReasonNothingToBuild: the spec set build.strategy: none, which means
	// "deploy spec.image, build nothing".
	ReasonNothingToBuild = "build/nothing-to-build"
	// ReasonStrategyNotImplemented: the resolved strategy is real but this
	// release does not implement it.
	//
	// Nothing returns it today: dockerfile and buildpacks are both implemented
	// (#48, #49). It stays because the reason taxonomy is a compatibility
	// promise the API repeats (kelson.v1alpha1.Error) and because ADR-0010
	// anticipates a strategy this release would not have — railpack is the
	// named candidate — which is exactly the case it describes.
	ReasonStrategyNotImplemented = "build/strategy-not-implemented"
	// ReasonDetectionNeedsSource: the strategy is `auto` and detecting it needs
	// a source tree the caller does not have (#50).
	ReasonDetectionNeedsSource = "build/detection-needs-source"
)

// Error is a refusal by the build plane itself. It carries the shape the model
// and delivery taxonomies use: a code, what happened, and the action that fixes
// it.
type Error struct {
	Reason      string
	Message     string
	Remediation string
}

func (e Error) Error() string {
	return fmt.Sprintf("[%s] %s: %s", e.Reason, e.Message, e.Remediation)
}

// ResolveStrategy applies ADR-0010's precedence and reports what the build
// plane can actually run today.
//
// tree is the source checkout to detect from, or nil when the caller has none.
// Nil is the normal case for a remote repository: the tree only exists inside
// the build pod, after the clone. So `auto` needs either a tree or an explicit
// spec.build.strategy, and the refusal says so rather than guessing "probably a
// Dockerfile" — which is the magic ADR-0010 exists to avoid. Detecting inside
// the cluster before choosing a driver is issue #50.
func ResolveStrategy(spec *model.Build, tree fs.FS) (detect.Detection, error) {
	strategy := model.BuildAuto
	if spec != nil && spec.Strategy != "" {
		strategy = spec.Strategy
	}
	if strategy == model.BuildAuto && tree == nil {
		return detect.Detection{}, Error{
			Reason: ReasonDetectionNeedsSource,
			Message: "the build strategy is `auto` and detecting it needs to read the source tree, " +
				"which is not available for a remote repository before the build pod clones it",
			Remediation: "set spec.build.strategy (`dockerfile` needs no detection), or run the build from the " +
				"CLI with -C <dir> pointing at a local checkout of the source; a server has no checkout to " +
				"detect from, and in-cluster detection is tracked by issue #50",
		}
	}

	detection, err := detect.Detect(spec, tree)
	if err != nil {
		return detect.Detection{}, fmt.Errorf("detecting the build strategy: %w", err)
	}

	switch detection.Strategy {
	// Both strategies of ADR-0010 run: a Dockerfile build through BuildKit
	// (#48) and a Cloud Native Buildpacks build through the lifecycle (#49).
	// Which driver the caller then constructs is its own wiring; what this
	// function decides is the strategy, and it is the same decision on both
	// sides.
	case detect.StrategyDockerfile, detect.StrategyBuildpacks:
		return detection, nil

	default: // detect.StrategyNone
		return detection, Error{
			Reason:      ReasonNothingToBuild,
			Message:     "the spec sets build.strategy: none, which means build nothing and deploy spec.image",
			Remediation: "remove build.strategy: none to build from source, or deploy the pre-built image directly",
		}
	}
}

// DestinationTag names the human-readable tag pushed alongside the digest.
//
// registry.Tag's convention is <project>-<application>-<revision>, but a kelson
// build is per-Project, not per-Application: the source is project-level and
// model rule P3 gives every application without its own image: the Project's
// image, so one build feeds all of them. There is no single application to
// name. The project name is passed for that slot — redundant with the first
// component, but true; naming the first application instead would read as "this
// image belongs to web", which is exactly what it does not mean.
// Reproducibility comes from the digest either way; this tag is for humans
// reading a registry listing.
func DestinationTag(project, revision string) string {
	return registry.Tag(project, project, shortRevision(revision))
}

// shortRevision keeps the tag readable. The full commit stays in
// Request.Revision and in the Job's kelson.dev/revision annotation, so nothing
// is lost by shortening what humans read.
func shortRevision(revision string) string {
	if len(revision) > 12 {
		return revision[:12]
	}
	return revision
}

// DockerfilePath is the spec's build.dockerfile, or "" for the driver's default.
func DockerfilePath(b *model.Build) string {
	if b == nil {
		return ""
	}
	return b.Dockerfile
}

// commitLength is the length of a full git object id in hex.
const commitLength = 40

// IsCommit reports whether ref is already a full commit hash, which is the one
// ref that needs no remote lookup to resolve.
func IsCommit(ref string) bool {
	if len(ref) != commitLength {
		return false
	}
	for i := 0; i < len(ref); i++ {
		c := ref[i]
		hex := c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
		if !hex {
			return false
		}
	}
	return true
}

// NormalizeCommit lowercases a ref that is already a commit, so the recorded
// revision has one spelling regardless of how it was typed.
func NormalizeCommit(ref string) string { return strings.ToLower(ref) }
