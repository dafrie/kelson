package flux

import (
	"context"
	"fmt"
	"strings"

	"github.com/dafrie/kelson/internal/delivery"
)

// ConditionState mirrors a Kubernetes condition status.
type ConditionState string

const (
	ConditionTrue    ConditionState = "True"
	ConditionFalse   ConditionState = "False"
	ConditionUnknown ConditionState = "Unknown"
)

// Kustomization is the slice of a Flux Kustomization kelson reads. It is a
// plain struct, not a typed client object, so the status source stays an
// interface: the dynamic client today (dynamic.go), a watch cache later, a
// fake in tests. None of that changes the phase mapping below.
type Kustomization struct {
	Name      string
	Namespace string
	// Path is spec.path, the directory in the source repository this
	// Kustomization applies (e.g. "./clusters/prod").
	Path string
	// SourceKind/SourceName reference the GitRepository (or OCIRepository).
	SourceKind string
	SourceName string
	// SourceURL/SourceBranch are resolved from the source object when
	// available; they let kelson tell "watching my repo" from "watching a
	// different repo with a similar path".
	SourceURL    string
	SourceBranch string

	Suspended bool

	// Ready is the Ready condition with its reason and message.
	Ready                 ConditionState
	Reason                string
	Message               string
	Reconciling           bool
	LastAppliedRevision   string
	LastAttemptedRevision string
}

// HelmRelease is the slice of a Flux HelmRelease kelson reads. Kustomization
// readiness alone does not answer "is my change live" for chart-based
// workloads: a Kustomization can be Ready while the HelmRelease it created is
// failing its upgrade.
type HelmRelease struct {
	Name      string
	Namespace string
	Ready     ConditionState
	Reason    string
	Message   string
}

// StatusReader reads Flux objects from the cluster. The production
// implementation is DynamicStatusReader (dynamic.go); tests use a fake.
type StatusReader interface {
	Kustomizations(ctx context.Context) ([]Kustomization, error)
	HelmReleases(ctx context.Context) ([]HelmRelease, error)
}

// Health is the state of the Flux control plane itself, which is a different
// question from the state of one Kustomization: a change that Flux has not
// observed looks identical whether Flux is merely slow or its source-controller
// is down (issue #137).
type Health struct {
	// Source names where the answer came from — HealthFromReport when
	// flux-operator publishes a FluxReport, HealthFromControllers when kelson
	// aggregated the controller Deployments itself. Reported so a human can
	// tell an authoritative answer from an inferred one.
	Source string
	// Version is the Flux distribution version when the source knows it.
	Version string
	Ready   bool
	// Unready names the components that are not ready, sorted.
	Unready []string
	// Message is the one-line human summary.
	Message string
}

// Health sources.
const (
	HealthFromReport      = "FluxReport"
	HealthFromControllers = "controllers"
)

// HealthReader is the optional half of a StatusReader. It is an extension
// rather than a StatusReader method because a status source that cannot see
// the Flux namespace is still a perfectly good status source: the adapter
// type-asserts for it and treats a missing or failing implementation as "no
// extra explanation available", never as a status failure.
type HealthReader interface {
	Health(ctx context.Context) (Health, error)
}

// Flux condition reasons kelson maps onto the delivery state machine. The
// distinction that matters (docs/delivery.md): a change the reconciler refused
// (Rejected) is a different user action from a change that is live and
// unhealthy (Degraded).
const (
	reasonBuildFailed      = "BuildFailed"
	reasonArtifactFailed   = "ArtifactFailed"
	reasonValidationFailed = "ValidationFailed"
	reasonReconcileFailed  = "ReconciliationFailed"
	reasonHealthCheckFail  = "HealthCheckFailed"
	reasonPruneFailed      = "PruneFailed"
	reasonDependencyNR     = "DependencyNotReady"
	reasonProgressing      = "Progressing"
	// reasonDecryptionFailed is kustomize-controller's own reason for a
	// decryption error where it reports one. Older releases fold the same
	// failure into BuildFailed with the detail in the message, which is why
	// decryptionCause matches on the message as well as on this reason.
	reasonDecryptionFailed = "DecryptionFailed"
)

// rejectedReasons are failures where Flux processed the change and refused it:
// the manifests never became live, so the user must fix the spec.
var rejectedReasons = map[string]bool{
	reasonBuildFailed:      true,
	reasonArtifactFailed:   true,
	reasonValidationFailed: true,
	reasonReconcileFailed:  true,
	reasonDecryptionFailed: true,
}

// degradedReasons are failures after the apply landed: it is live and wrong.
var degradedReasons = map[string]bool{
	reasonHealthCheckFail: true,
	reasonPruneFailed:     true,
}

// PhaseFor maps one Kustomization's observation onto the delivery phase for a
// specific revision.
//
// digest is the OCI digest kelson expects this revision to be — Revision.Digest
// / PinnedDigest as the spine's own observer knows it (internal/controller's
// observe) — and empty for a tag-only pin: a rollback to a history entry
// recorded before digests existed, or a Kustomization kelson merely observes
// rather than owns (a preview's, a git source). It is what lets revisionMatches
// tell a bare-digest Flux revision from one it simply cannot confirm; see
// there for why the two must not be conflated.
//
// It is exported because the spine's own observer calls it (ADR-0028 decision
// 1, step 6): internal/controller reads back the Kustomization it owns and has
// to arrive at the same phase, with the same causes, that `kelson status`
// arrives at for any other Kustomization. A second mapping would be a second
// opinion about what Degraded means.
func PhaseFor(k Kustomization, revision, digest string) delivery.Status {
	detail := map[string]string{
		"kustomization":       k.Namespace + "/" + k.Name,
		"path":                k.Path,
		"lastAppliedRevision": k.LastAppliedRevision,
	}
	ref := fmt.Sprintf("flux: Kustomization %s/%s", k.Namespace, k.Name)

	if k.Suspended {
		return delivery.Status{
			Phase:    delivery.PhaseCommitted,
			Revision: revision,
			Cause:    ref + " is suspended: it will not reconcile until resumed (flux resume kustomization)",
			Detail:   detail,
		}
	}

	applied := revisionMatches(k.LastAppliedRevision, revision, digest)
	attempted := revisionMatches(k.LastAttemptedRevision, revision, digest)

	// Failures are reported against whichever revision Flux is working on: if
	// it never attempted ours, our change is still merely committed.
	if k.Ready == ConditionFalse {
		switch {
		case rejectedReasons[k.Reason] && (attempted || applied):
			return delivery.Status{
				Phase: delivery.PhaseRejected, Revision: revision,
				Cause: fmt.Sprintf("%s rejected the change (%s): %s%s",
					ref, k.Reason, k.Message, decryptionCause(k)), Detail: detail,
			}
		case degradedReasons[k.Reason]:
			return delivery.Status{
				Phase: delivery.PhaseDegraded, Revision: revision,
				Cause: fmt.Sprintf("%s applied the change but it is unhealthy (%s): %s", ref, k.Reason, k.Message), Detail: detail,
			}
		case k.Reason == reasonDependencyNR:
			return delivery.Status{
				Phase: delivery.PhaseReconciling, Revision: revision,
				Cause: fmt.Sprintf("%s is waiting on a dependency: %s", ref, k.Message), Detail: detail,
			}
		case attempted || applied:
			return delivery.Status{
				Phase: delivery.PhaseRejected, Revision: revision,
				Cause: fmt.Sprintf("%s is not ready (%s): %s%s",
					ref, k.Reason, k.Message, decryptionCause(k)), Detail: detail,
			}
		}
	}

	switch {
	case applied && k.Ready == ConditionTrue:
		// Flux reports Ready only once the apply succeeded and any configured
		// health checks passed.
		return delivery.Status{Phase: delivery.PhaseHealthy, Revision: revision, Detail: detail}
	case applied:
		return delivery.Status{Phase: delivery.PhaseApplied, Revision: revision, Detail: detail}
	case attempted || k.Reconciling || k.Reason == reasonProgressing:
		return delivery.Status{Phase: delivery.PhaseReconciling, Revision: revision, Detail: detail}
	default:
		// Committed, but Flux has not picked it up yet. This is the "keep
		// waiting" answer, deliberately distinct from Rejected.
		return delivery.Status{
			Phase:    delivery.PhaseCommitted,
			Revision: revision,
			Cause:    ref + " has not observed this revision yet",
			Detail:   detail,
		}
	}
}

// decryptionMarkers are the substrings that identify a kustomize-controller
// build failure as a SOPS decryption failure.
//
// They are matched on the controller's message rather than only on its reason
// because the reason is usually just `BuildFailed`: decryption happens inside
// the build, and which release folds it into which reason has changed. Each
// marker is specific enough not to fire on an ordinary build error — "age"
// deliberately is not one of them, because it is a substring of "image",
// "message" and "storage".
var decryptionMarkers = []string{
	"decrypt",        // "failed to decrypt secret", "decryption failed"
	"sops",           // "sops metadata not found", "cannot get sops data key"
	"data key",       // "Error getting data key: 0 successful groups required"
	"creation rules", // "no matching creation rules found"
	"age identity",   // "no age identity found"
	"identity file",  // the age key Secret is present but holds no usable key
}

// decryptionCause names a decryption failure as its own cause, appended to the
// controller's message.
//
// A Kustomization that cannot decrypt reports a build failure, and the
// controller's own message describes the *mechanism* ("Error getting data
// key") rather than the situation. The situation is almost always one of three
// setup mistakes, all of them far from where the reader is standing: the
// Kustomization has no `spec.decryption`, or the Secret it names is absent, or
// the identity in it is not one of the file's recipients. Naming them here is
// what turns "my deploy is red" into a next step, and it costs nothing when
// the failure is something else — the markers do not match and nothing is
// appended (issue #81, ADR-0022).
//
// The controller's message is relayed verbatim ahead of this, as every other
// reason's is. sops does not put secret material in its errors, and
// internal/redact covers the display path regardless.
func decryptionCause(k Kustomization) string {
	if k.Reason != reasonDecryptionFailed && !matchesAny(k.Message, decryptionMarkers) {
		return ""
	}
	return "\n  cause: this Kustomization could not decrypt the SOPS-encrypted manifests at " +
		normalizePath(k.Path) + "." +
		"\n  fix: check, in this order — (1) the Kustomization has spec.decryption: {provider: sops, secretRef: " +
		"{name: …}}; (2) that Secret exists in namespace " + k.Namespace +
		" and holds the age identity under a .agekey key; (3) the identity is one of the recipients the files " +
		"are encrypted to — `kelson secret rotate` lists them without needing a key."
}

func matchesAny(s string, markers []string) bool {
	lower := strings.ToLower(s)
	for _, m := range markers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// ociRevisionMarker is what makes a tag-and-digest OCI revision recognisable
// as one. source-controller writes an OCIRepository's revision as
// "<tag>@sha256:<digest>" when it has a tag to name, and a git v2 revision as
// "<branch>@sha1:<sha>" — same separator, different algorithm, and the
// algorithm is the only thing that tells them apart.
const ociRevisionMarker = "@sha256:"

// ociDigestPrefix marks the algorithm on its own, with no tag in front. This is
// the shape source-controller reports once an OCIRepository's ref pins a digest
// (fluxobjects.go's ociRepository, F8): the artifact's revision *is* the
// digest, so there is no tag half to write before an "@". CI run 64 caught the
// bug this shape exposed — kelson's comparison expected the "<tag>@sha256:…"
// form unconditionally, so a bare-digest revision never matched anything and a
// deployment Flux had already applied reported Committed forever.
const ociDigestPrefix = "sha256:"

// revisionMatches compares a Flux revision string against the revision kelson
// is waiting for. want is the tag; wantDigest is the OCI digest kelson expects
// (empty for a tag-only pin — see [PhaseFor]).
//
// Three shapes, and the difference is load-bearing:
//
//   - **OCI, tag and digest both pinned** writes "<tag>@sha256:<digest>". When
//     wantDigest is known the digest is the authoritative half — it is the
//     bytes, the tag is only ever a name for them, and it is what a rollback's
//     pin comparison and a settled republish check both ultimately rest on.
//     When wantDigest is empty (a tag-only pin, or a caller — a preview reading
//     someone else's OCIRepository — that never learned one) the tag is
//     compared instead, whole: a tag is not a prefix of anything, and
//     "7-1a2b3c4d" against "7-1a2b3c4de" is a different revision, not an
//     abbreviation of the same one.
//   - **OCI, digest only** writes bare "sha256:<digest>": what source-controller
//     reports once the OCIRepository's ref names a digest (fluxobjects.go).
//     There is no tag half to fall back on, so this shape matches only by
//     digest; with no wantDigest to compare it against, kelson cannot confirm
//     it and the answer is "no match" rather than a guess.
//   - **git** (the preview pipeline and any Kustomization kelson merely
//     observes) writes "<branch>@sha1:<sha>" (v2) or "<branch>/<sha>" (older),
//     and either side may be abbreviated, because a short sha is how humans
//     write commits.
//
// Before the spine existed there was only the git case, and applying it to an
// OCI revision reads the digest as the commit: "7-1a2b3c4d@sha256:beef…"
// compares "beef…" against the tag, never matches, and a deployment that is
// live and healthy reports as Committed forever.
func revisionMatches(fluxRevision, want, wantDigest string) bool {
	if fluxRevision == "" || want == "" {
		return false
	}
	if tag, digest, ok := splitOCIRevision(fluxRevision); ok {
		if wantDigest != "" {
			return strings.EqualFold(strings.TrimSpace(digest), strings.TrimSpace(wantDigest))
		}
		if tag == "" {
			// A bare digest and nothing to compare it against: this caller
			// never learned the expected digest, so there is no confident
			// answer — and no tag component here to fall back on.
			return false
		}
		return strings.EqualFold(strings.TrimSpace(tag), strings.TrimSpace(want))
	}
	got := fluxRevision
	if _, after, ok := cutLast(got, ":"); ok {
		got = after
	} else if _, after, ok := cutLast(got, "/"); ok {
		got = after
	}
	got, wantLower := strings.ToLower(strings.TrimSpace(got)), strings.ToLower(strings.TrimSpace(want))
	if got == "" {
		return false
	}
	return strings.HasPrefix(got, wantLower) || strings.HasPrefix(wantLower, got)
}

// splitOCIRevision recognises the two shapes an OCIRepository-backed
// Kustomization writes and pulls the digest out of either. ok is false for
// anything else — git's "<branch>@sha1:<sha>" and "<branch>/<sha>" — which is
// what sends revisionMatches down its other branch.
//
// tag is "" for the bare-digest shape: there is nothing before the algorithm
// to call a tag, and the caller must not read the empty string as a match for
// an empty want (revisionMatches never calls this with one).
func splitOCIRevision(s string) (tag, digest string, ok bool) {
	if before, after, cut := strings.Cut(s, ociRevisionMarker); cut {
		return before, ociDigestPrefix + after, true
	}
	if strings.HasPrefix(strings.ToLower(s), ociDigestPrefix) {
		return "", s, true
	}
	return "", "", false
}

func cutLast(s, sep string) (before, after string, found bool) {
	i := strings.LastIndex(s, sep)
	if i < 0 {
		return s, "", false
	}
	return s[:i], s[i+len(sep):], true
}

// covers reports whether this Kustomization reconciles the given repository
// path. Flux paths are conventionally written "./dir"; a Kustomization at
// "./clusters/prod" covers "clusters/prod/apps/web".
func (k Kustomization) covers(repo, repoPath string) bool {
	kp := normalizePath(k.Path)
	target := normalizePath(repoPath)
	if kp != "" && target != kp && !strings.HasPrefix(target, kp+"/") {
		return false
	}
	// A source URL is only compared when Flux told us one: kelson must not
	// silently decide it is unwatched because it could not resolve the source.
	if k.SourceURL != "" && repo != "" && !sameRepo(k.SourceURL, repo) {
		return false
	}
	return true
}

func normalizePath(p string) string {
	p = strings.TrimSpace(p)
	p = strings.TrimPrefix(p, "./")
	return strings.Trim(p, "/")
}

// sameRepo compares git URLs across the scheme/credential/.git spellings of
// the same repository.
func sameRepo(a, b string) bool {
	return normalizeRepo(a) == normalizeRepo(b)
}

func normalizeRepo(u string) string {
	s := strings.ToLower(strings.TrimSpace(u))
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if _, after, ok := strings.Cut(s, "@"); ok {
		s = after
	}
	s = strings.ReplaceAll(s, ":", "/")
	s = strings.TrimSuffix(s, ".git")
	return strings.Trim(s, "/")
}

// readErr wraps a failed cluster read as a delivery error naming what could
// not be read.
func readErr(what string, err error) error {
	e := delivery.ApplyFailed("flux/status", "",
		"could not read Flux "+what+" from the cluster",
		"check cluster connectivity and that the Flux CRDs are installed (flux check)")
	e.Cause = err.Error()
	return e
}
