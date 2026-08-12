// Package build is the BUILD plane: it turns a checked-out source tree into
// an image reference and pushes it, streaming logs as it does (ADR-0010).
//
// Strategies are thin drivers behind one narrow interface — the Builder
// contract below: (source, environment) → image reference + streamed logs. No
// strategy's configuration surface enters the kelson spec (ADR-0010); the spec
// only ever says strategy: dockerfile, never a BuildKit DSL.
//
// Doing this work here, behind an interface instead of in cmd/kelson, keeps
// the CLI a thin shell and lets future strategies (buildpacks, railpack) slot
// in as one new driver (issue #47, ADR-0010).
package build

import (
	"context"
	"io"
)

// Request is one build: where the source is, what to build from it, and where
// the result is pushed.
type Request struct {
	Project     string
	Environment string
	Application string

	// SourceGit is the repository the build clones, from Project.source.git.
	//
	// An in-cluster build has no local path to read: the build pod starts with
	// an empty workspace, so the source has to arrive somehow. Cloning inside
	// the pod is how, and it keeps the control plane out of the data path —
	// nothing uploads a tarball through kelson.
	SourceGit string
	// SourceRef is the branch, tag or commit to check out. Empty means the
	// repository's default branch. Prefer a commit: Revision records what was
	// built, and a moving ref makes that record a guess.
	SourceRef string

	// ContextDir is the build context within the checked-out tree, relative to
	// its root. Empty means the root. This is what makes a monorepo buildable:
	// the repository is cloned whole and only this subdirectory is built.
	ContextDir string
	// Dockerfile is the path relative to ContextDir. Empty means ./Dockerfile.
	Dockerfile string
	// Target selects a build stage. Empty builds the final stage.
	Target string
	// Args are build arguments. They are NEVER secrets: build args are visible
	// in image history, and ADR-0009 keeps secret values out of the spec.
	Args map[string]string
	// Platforms such as linux/amd64, linux/arm64. Empty builds one image for
	// the builder's own platform.
	Platforms []string

	// Image is the destination repository with no tag and no digest,
	// e.g. ghcr.io/acme/web.
	Image string
	// Tag is the human-readable tag pushed alongside the digest, derived from
	// the source revision (#51).
	Tag string
	// Revision is the source commit the build came from.
	Revision string
}

// Result is what a build produced. Reference is the field callers should use:
// deploying by digest is what makes a revision reproducible (#51).
type Result struct {
	// Reference is the fully-qualified image pinned by digest,
	// e.g. ghcr.io/acme/web@sha256:...
	Reference string
	// Digest is the manifest digest, sha256:...
	Digest string
	// Tag is the mutable tag also pushed, for humans.
	Tag string
	// Platforms actually built.
	Platforms []string
}

// Builder runs one build to completion, streaming logs as they are produced.
//
// Implementations are thin drivers over BuildKit (ADR-0010). Build must be
// safe to call concurrently for different Requests.
type Builder interface {
	// Name is the strategy id: "dockerfile", "buildpacks".
	Name() string
	// Build runs the build and returns the pushed image. logs receives build
	// output as it happens; it may be nil.
	Build(ctx context.Context, req Request, logs io.Writer) (Result, error)
}
