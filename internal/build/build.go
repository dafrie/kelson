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
	Component   string

	// SourceGit is the repository the build clones: the `git` of the source its
	// components are bound to (ADR-0035 decision 3), which for the singular
	// `source:` spelling is the Project's own and unchanged.
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

	// SourceName is the name of the source this build clones — a project's
	// `sources:` entry, `default` for the singular spelling, or a GitSource the
	// instance offers (ADR-0035 decisions 1 and 2).
	//
	// It is carried so a plan a human reads can say *which* source was built
	// rather than only which URL, which is the difference between "why is this
	// cloning the tools repo" being answerable from the build's own output and
	// being answerable only by re-deriving the binding. Nothing downstream keys
	// on it: the Job's identity is still the project, the component and the
	// revision.
	SourceName string

	// SourceConnection is `connection:` on that source: the GitConnection the
	// author pinned this repository to, or empty for the host match that is the
	// common case (ADR-0033 decision 4).
	//
	// It is on the Request rather than on the plane because the plane is
	// assembled per build target and the connection is now per *source*: one
	// project may read two repositories through two connections. [CloneAuth]
	// receives the whole Request for exactly this reason, and the ref resolver
	// beside it must be given the same answer or a build would resolve a commit
	// it then cannot fetch.
	SourceConnection string

	// CloneSecret is the name of the per-run Secret holding the credential the
	// clone fetches with (ADR-0033 decision 5). It is a *name*: the value never
	// enters a Request, a manifest's command line or a pod spec.
	//
	// It is set by the driver at submit time from what [CloneAuth] minted, not
	// by whoever assembled the Request — the API plane builds Requests and
	// cannot mint. Empty means an anonymous fetch, which is correct for a public
	// repository and is what every build did before connections existed.
	CloneSecret string

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

// Annotations every driver writes onto the build workload it renders. They are
// how the executor learns what the build pushes without reading the builder's
// command line: buildctl spells the destination `--output name=<ref>` and the
// CNB lifecycle spells it as a positional `-image <ref>`, and an executor that
// scraped either would be coupled to one strategy's argv.
//
// They live here, in the contract package, because they are read by a plane
// that may not import a driver (internal/delivery/kube) and written by drivers
// that may not import it back.
const (
	// AnnotationImage is the destination repository, with no tag and no
	// digest — build.Request.Image as rendered.
	AnnotationImage = "kelson.dev/image"
	// AnnotationTag is the human-readable tag pushed alongside the digest,
	// absent when the build pushed untagged.
	AnnotationTag = "kelson.dev/tag"
	// AnnotationRevision is the source commit the build came from.
	AnnotationRevision = "kelson.dev/revision"
)

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
