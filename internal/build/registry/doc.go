// Package registry provides the image-reference, digest-pinning and
// registry-credential machinery the build and deployment paths consume
// (issue #51).
//
// # The rule that motivates it
//
// Rendered manifests MUST reference digests, not tags. A mutable tag makes the
// rendered output non-reproducible and quietly breaks the guarantee that the
// same commit deploys the same thing — which undermines both GitOps and
// rollback. The Builder in internal/build pushes an image and returns a
// digest-pinned reference (build.Result.Reference); this package supplies the
// Pin and Mutable primitives that let the rest of the system enforce that rule,
// plus the tagging convention tied to the source revision and the credential-
// reference interface for registry auth (ADR-0009).
package registry
