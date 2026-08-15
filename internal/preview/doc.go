// Package preview is the PR preview publisher: stage 2 of ADR-0017.
//
// Stage 1 rendered the cluster-side machinery — a ResourceSetInputProvider that
// finds a project's open change requests and a ResourceSet whose template
// instantiates an OCIRepository and a Kustomization per pull request, pinned to
// `oci://<previews.artifacts.repository>` at the head commit. It also shipped
// something that did not work yet: nothing published the artifacts those
// OCIRepositories point at. This package publishes them.
//
// # What a publish is
//
//	(spec documents, environment, change request, head commit, image)
//	    → the environment's render, for the preview's own namespace
//	    → a Flux OCI artifact
//	    → pushed to previews.artifacts.repository, tagged with the head commit
//
// Nothing here re-implements rendering. [Render] resolves the same documents
// with the preview's identity substituted and calls the same pure
// renderer.Render every other kelson output comes from, so a preview is the
// parent environment's manifest set with a different namespace and different
// hostnames, and nothing else. That is the whole reason ADR-0016 decision 5
// chose artifact-per-PR: what runs in a preview is a thing kelson rendered, not
// a thing a controller templated.
//
// # Where it runs: two callers now, and the server is one of them
//
// ADR-0017 decision 8 put the publisher in the application repository's CI, as
// `kelson preview publish`, because that is where the checkout, the fresh image
// and the registry credential already were — and deferred rather than rejected
// a server-side caller, promising it "will call the same package this CLI verb
// calls".
//
// [ADR-0034](docs/adr/0034-forge-driven-delivery.md) decision 3 calls it in.
// `BuildService.ReportBuild` is CI saying "I built the image, here is where it
// is", and the server answers it by rendering and publishing here — with the
// reported digests as [Options.Images]. [Publisher] is that composition;
// nothing about the render, the package or the tag differs between the two
// callers, which is the whole reason the deferral was cheap. `kelson preview
// publish` survives unchanged for pipelines that cannot reach a kelson server
// at all, demoted from the recommended path to the escape hatch.
//
// # Where the publisher lives
//
// Not here. [ADR-0028](docs/adr/0028-delivery-spine.md) decision 2 converges the
// preview pipeline and the delivery spine on one publisher — "one media type,
// one determinism test, two callers" — so the packaging, the push and the
// docker-config credential lookup moved to internal/artifact, a package neither
// caller owns. What stays is the half that is actually about previews: which
// annotations a preview artifact carries and which tag it goes under
// ([Package]), plus aliases so this package's vocabulary is unchanged for
// `kelson preview publish` and for the publisher/consumer contract test.
//
// # Determinism
//
// The artifact is a pure function of its inputs, down to the digest: the tar is
// built in render order with fixed modes and a fixed timestamp, and the config
// blob carries the same fixed timestamp rather than the wall clock. So
// republishing an unchanged pull request produces a digest the registry already
// has, and "the same render produces the same artifact" is a property tests can
// assert instead of a hope (ADR-0001's determinism rule, applied one layer out).
//
// # The seam
//
// Rendering and packaging are offline and pure. Only [Pusher] touches the
// network, and only over the OCI distribution API with the credential its
// caller resolved — through internal/build/registry's credential vocabulary, so
// there is one shape for "a registry credential" in this codebase and one place
// (internal/redact) that knows it must never be printed.
package preview
