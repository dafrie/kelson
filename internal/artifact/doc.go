// Package artifact is kelson's Flux OCI artifact publisher: the one that
// packages a rendered manifest set into an immutable OCI artifact and pushes it
// over the distribution API.
//
// # One publisher, two callers
//
// It arrived here from internal/preview, which built it for ADR-0017's PR
// previews. [ADR-0028](docs/adr/0028-delivery-spine.md) decision 2 makes the
// delivery spine publish the same artifact — "one media type, one determinism
// test, two callers" — so the code moved out of the preview pipeline and into a
// package neither caller owns. internal/preview keeps thin wrappers over it so
// nothing about `kelson preview publish` changed; internal/controller is the
// second caller, and the reason the move happened.
//
// The generalisation is one type: [Package] takes [Contents] — a repository, a
// tag, the files and the annotations — rather than a preview. Everything a
// preview knows that a spine reconcile does not (which change request, which
// head commit, which namespace) is an annotation the caller supplies, so this
// package holds no vocabulary from either side.
//
// # Determinism
//
// The artifact is a pure function of its [Contents], down to the digest: the
// tar is written in the caller's order with fixed modes and a fixed timestamp,
// gzip's own header carries that timestamp rather than the wall clock, and the
// config blob is dated the same epoch. So republishing an unchanged render
// produces a digest the registry already holds and uploads nothing, and "the
// same input produces the same artifact" is a property tests assert rather than
// a hope (ADR-0001's determinism rule, applied one layer out).
//
// The created annotation is set by [Package] and not by its caller, precisely
// so that no caller can put a wall clock in it.
//
// # The seam
//
// Packaging is offline and pure. Only [Pusher] touches the network, and only
// over the OCI distribution API with the credential its caller resolved —
// through internal/build/registry's credential vocabulary, so there is one
// shape for "a registry credential" in this codebase and one place
// (internal/redact) that knows it must never be printed.
package artifact
