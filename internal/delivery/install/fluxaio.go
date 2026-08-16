package install

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"

	"github.com/dafrie/kelson/internal/delivery"
)

// The one component whose manifests kelson renders rather than fetches.
//
// flux-aio is published upstream ONLY as a timoni module. There is no
// install.yaml release asset for ADR-0021 decision 2's pin-a-URL-and-a-digest
// rule to apply to, and ADR-0030 decision 2 answers that by rendering the
// module at kelson RELEASE time, in CI, with a pinned timoni binary against a
// digest-pinned module, and committing the result. This file is the reading
// half of that: hack/flux-aio-render.sh writes rendered/flux-aio.yaml, the
// directory below is compiled into the binary, and Installer.load decodes it
// exactly as it decodes a fetched manifest — same provenance labels, same
// ownership stamping, same uninstall sweep.
//
// # Three refusals about timoni, and what they cost this file
//
// ADR-0030 decision 3 refuses timoni as a runtime dependency, as a Go
// dependency and as a user-visible concept. The consequence here is that this
// package knows nothing about timoni, CUE or OCI modules: it reads bytes out of
// an embedded filesystem. Everything timoni-shaped lives in one shell script
// that runs once per release on a CI runner, and the only trace of it in Go is
// the provenance recorded in the pins table row.
//
// # A build can be missing its snapshot, and must say so loudly
//
// The snapshot is rendered by a script that needs network access to ghcr.io,
// so a working tree can legitimately be without it: a fresh clone before
// `make flux-aio` has run, or a build made where the module could not be
// pulled. That is not a state to paper over — a `kelson install flux-aio` that
// silently did nothing, or a --all-missing sweep that quietly left a cluster
// with no reconciler at all, is precisely the half-installed outcome this
// package exists to prevent.
//
// So absence is handled in two places and hidden in neither:
//
//   - [Installer.Plan] refuses the row with a [Refusal] naming what is missing
//     and pointing at `kelson install flux`, which installs full Flux and needs
//     no snapshot (refuse, install.go);
//   - a sweep only prefers flux-aio over flux-operator when the snapshot is
//     actually there, so a build without one behaves exactly as kelson did
//     before this row existed rather than offering a cluster nothing.
//
// [TestRenderedSnapshotMatchesThePin] is the third place: it fails the build
// when the committed bytes and the pinned digest disagree, which is what makes
// "mechanically regenerated, never hand-edited" a property rather than a claim.

// renderedDir holds the snapshots hack/*-render.sh writes.
//
// The pattern names the directory rather than the file, so a working tree with
// no snapshot still compiles — README.md is committed and keeps the pattern
// non-empty. A `//go:embed rendered/flux-aio.yaml` would refuse to build
// without it, which would turn "this build cannot install flux-aio" into "this
// repository cannot be built", and those are very different problems.
//
//go:embed rendered
var renderedDir embed.FS

// renderedFS is the filesystem [renderedSnapshot] reads. It is a var, and the
// seam is deliberate: the tests for the present-snapshot path must exercise
// decoding, digest verification and apply against bytes they control, and the
// real snapshot is upstream's several-hundred-kilobyte Flux install. Swapping
// it is how those tests avoid asserting against bytes no reviewer reads.
var renderedFS fs.FS = renderedDir

// renderedSnapshot returns the committed bytes for a Rendered row, and whether
// there are any.
func renderedSnapshot(c Component) ([]byte, bool) {
	if !c.Rendered || c.RenderedPath == "" {
		return nil, false
	}
	body, err := fs.ReadFile(renderedFS, c.RenderedPath)
	if err != nil || len(body) == 0 {
		return nil, false
	}
	return body, true
}

// renderedAvailable reports whether this build can install a Rendered row at
// all. It is what the sweep consults before preferring flux-aio over
// flux-operator, and what [refuse] consults before promising an install it
// cannot perform.
func renderedAvailable(c Component) bool {
	_, ok := renderedSnapshot(c)
	return ok
}

// fluxAIOInstallable reports whether the flux-aio row exists in the table and
// this build carries its snapshot.
//
// It reads the table rather than a package-level constant because the table is
// swappable (cluster_test.go's withComponents), and because a row that is one
// day deleted should make the sweep fall back rather than panic.
func fluxAIOInstallable() bool {
	c, ok := Lookup(FluxAIOName)
	return ok && c.Status == StatusSupported && renderedAvailable(c)
}

// FluxAIOName is the catalog row ADR-0030 decision 1 makes the default offer on
// a cluster with no Flux. It is a constant because three separate decisions key
// off it — the sweep's preference over flux-operator, the request-level refusal
// to install both, and the missing-snapshot refusal — and a typo in any of them
// would silently disable one.
const FluxAIOName = "flux-aio"

// FluxOperatorName is the other Flux row: full Flux through flux-operator's own
// lifecycle, required for PR previews (ADR-0030 decision 4) and an explicit
// choice everywhere else.
const FluxOperatorName = "flux"

// renderedManifest is [renderedSnapshot] with the refusal spelled out, for the
// apply path. Reaching it with no snapshot is a bug rather than a user error —
// [refuse] catches the ordinary case first and turns it into a [Refusal] — so
// the message is written for whoever built the binary, not for whoever ran it.
func renderedManifest(c Component) ([]byte, error) {
	body, ok := renderedSnapshot(c)
	if !ok {
		return nil, delivery.ApplyFailed(source(c), "",
			"this kelson build carries no rendered manifests for "+c.Title+": "+c.RenderedPath+" is empty or absent",
			"the snapshot is produced at release time by hack/flux-aio-render.sh (ADR-0030 decision 2) and "+
				"committed. Run `make flux-aio` and rebuild, or use `kelson install "+FluxOperatorName+
				"`, which installs full Flux from flux-operator's published manifest and needs no snapshot. "+
				"Nothing was applied")
	}
	return body, nil
}

// verifyRenderedDigest is [verifyDigest] for bytes that were never fetched.
//
// The check is not ceremony. The pin answers "are these the bytes kelson
// shipped", and it is the half of ADR-0030 decision 2's argument that a
// reviewer cannot perform by eye: a snapshot edited by hand to make something
// pass would still decode, still apply, and still look like upstream's. Only
// the digest catches it, and it catches it before a byte reaches a cluster
// rather than at release time only.
func verifyRenderedDigest(c Component, body []byte) error {
	sum := sha256.Sum256(body)
	got := hex.EncodeToString(sum[:])
	if got == c.SHA256 {
		return nil
	}
	return delivery.ApplyFailed(source(c), "",
		fmt.Sprintf("the committed snapshot for %s %s does not match the pinned digest: got %s, pinned %s",
			c.Title, c.Version, got, c.SHA256),
		"nothing was applied. These bytes are a build artifact, not a file to edit: run "+
			"`hack/flux-aio-render.sh --check` to see what the pins actually render, then `make flux-aio` "+
			"and paste the printed block into internal/delivery/install/pins.go. Never resolve this by "+
			"editing the YAML")
}

// source names where a component's bytes come from, for an error message.
//
// One row's bytes are fetched from a URL, one row's are composed in Go, and one
// row's are read out of this binary. An error that said "the pinned install
// manifest at  could not be read" — the empty ManifestURL of a row that has
// none — would send the reader looking for a download that never happens.
func source(c Component) string {
	switch {
	case c.Rendered:
		return "internal/delivery/install/" + c.RenderedPath
	case c.Authored:
		return c.Image + "@sha256:" + c.ImageDigest
	default:
		return c.ManifestURL
	}
}
