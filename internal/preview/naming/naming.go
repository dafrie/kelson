// Package naming is the preview naming contract of ADR-0017, in one place.
//
// # Why this package exists
//
// ADR-0017 shipped its cluster-side machinery (stage 1) with a negative stated
// as plainly as its positives: "the publisher and the renderer agree by
// convention, not by type". The renderer templates a namespace called
// `<project>-<environment>-pr<< inputs.id >>` and an OCIRepository tag of
// `<< inputs.sha >>`; the publisher has to render into that namespace and push
// to that tag. Nothing checked it, and the failure mode — a stray namespace, or
// an OCIRepository pointing at a tag nobody will ever push — is quiet.
//
// So the scheme lives here and both sides call it. internal/renderer builds the
// ResourceSet's template out of these functions and internal/preview renders
// and tags out of the same ones, which turns "agree by convention" into "agree
// by shared code" without inventing a type nobody needed.
//
// # Purity
//
// The renderer imports this package, so it obeys the renderer's rules: no
// clock, no filesystem, no network, nothing but strings in and strings out
// (.golangci.yml, issue #20). Everything here is a pure function of the names
// already in the spec.
package naming

import (
	"errors"
	"fmt"
	"strings"
)

const (
	// Infix separates the environment from the change request number:
	// <project>-<environment>-pr<id>.
	Infix = "pr"
	// LifecycleSuffix names the pair that owns the per-pull-request lifecycle:
	// <project>-<environment>-previews, in the environment's own namespace.
	LifecycleSuffix = "previews"
)

// IDDigits is how many digits of change request number the scheme reserves.
// It is a real assumption and ADR-0017 names it: a repository that reaches
// pull request 1,000,000 renders a namespace one character too long, and the
// failure is upstream's rather than kelson's.
const IDDigits = 6

// MaxBase caps <project>-<environment>. One number covers both derived names
// because both add exactly nine characters: "-previews" for the lifecycle pair,
// and "-pr" plus up to IDDigits digits for the preview itself. A preview's name
// is also its namespace, and a namespace is a DNS-1123 label capped at 63.
const MaxBase = 63 - len("-"+Infix) - IDDigits

// The templated holes flux-operator substitutes per change request. They are
// constants rather than literals at each site because the renderer writes them
// into a ResourceSet and the publisher must produce exactly what they resolve
// to — see [Preview] and [Tag], whose contract test asserts that pairing.
const (
	InputID  = "<< inputs.id >>"
	InputSHA = "<< inputs.sha >>"
)

// Base is <project>-<environment>, the stem every preview name is derived
// from.
func Base(project, environment string) string {
	return project + "-" + environment
}

// BaseTooLong reports whether a stem exceeds [MaxBase]. Callers phrase the
// refusal themselves — the renderer raises a structured render error and the
// publisher a structured publish error — but they agree on the number.
func BaseTooLong(base string) bool { return len(base) > MaxBase }

// Lifecycle names the ResourceSetInputProvider and ResourceSet pair. They live
// in the environment's own namespace, which is what makes teardown of a preview
// a namespace delete and keeps a preview from touching the environment it
// previews.
func Lifecycle(project, environment string) string {
	return Base(project, environment) + "-" + LifecycleSuffix
}

// Preview names one preview: its OCIRepository, its Kustomization and its
// namespace are all this string (ADR-0017 decision 3). id is the change request
// number as written, so callers that took it from a forge pass it through
// [ValidateID] first.
func Preview(project, environment, id string) string {
	return Base(project, environment) + "-" + Infix + id
}

// PreviewTemplate is [Preview] with the change request number left as the hole
// flux-operator fills in. It is what the renderer writes into the ResourceSet's
// resourcesTemplate, and it is [Preview] with [InputID] substituted — the
// property the publisher/consumer contract test asserts.
func PreviewTemplate(project, environment string) string {
	return Preview(project, environment, InputID)
}

// Tag is the artifact tag for one publish: the change request's head commit,
// lowercased.
//
// ADR-0017 decision 2 chose the SHA over pr-<id> because a per-pull-request tag
// is mutable by construction — updating a preview would mean repointing it,
// which discards the artifact history and makes "what is running in preview
// 412" depend on when you ask. The renderer templates [InputSHA] for exactly
// this string.
func Tag(sha string) string { return strings.ToLower(strings.TrimSpace(sha)) }

// Revision is the artifact's org.opencontainers.image.revision annotation: the
// change request and the commit it was rendered from, in Flux's
// `<ref>@sha1:<commit>` shape so `flux pull artifact` prints something a human
// recognises.
func Revision(id, sha string) string {
	return Infix + "-" + id + "@sha1:" + Tag(sha)
}

// Host derives a preview's hostname from the hostname the same component would
// have in the parent environment, by suffixing the first DNS label:
//
//	web.staging.example.com → web-pr412.staging.example.com
//
// Only the first label changes, so a wildcard certificate and a wildcard DNS
// record that already serve the parent environment serve its previews too. The
// alternative — inserting a label — is outside a single-label wildcard and
// breaks TLS on the first preview.
//
// Rewriting is not a convenience, it is the safety property: two previews of
// one environment would otherwise claim the same hostname, and a preview of a
// component with an authored production hostname would claim production's.
func Host(host, id string) string {
	label, rest, found := strings.Cut(host, ".")
	label += "-" + Infix + id
	if !found {
		return label
	}
	return label + "." + rest
}

// ValidateID rejects a change request number the scheme cannot carry, before
// it becomes a namespace the API server refuses at reconcile time.
func ValidateID(id string) error {
	if id == "" {
		return errors.New("a change request number is required")
	}
	if len(id) > IDDigits {
		return fmt.Errorf("change request number %q has more than %d digits, which is more than the preview naming scheme reserves", id, IDDigits)
	}
	for i := 0; i < len(id); i++ {
		if id[i] < '0' || id[i] > '9' {
			return fmt.Errorf("change request number %q is not a number", id)
		}
	}
	if id[0] == '0' {
		// flux-operator exports the forge's own number, which never carries a
		// leading zero. One that does would name a namespace no input will ever
		// produce, so the artifact would be published where nothing looks.
		return fmt.Errorf("change request number %q has a leading zero; forges number change requests without one", id)
	}
	return nil
}

// shaLengths are the digest widths a git head commit can have: 40 for sha1,
// 64 for the sha256 object format. An abbreviated commit is rejected rather
// than expanded, because the tag has to match the full SHA flux-operator reads
// from the forge — an abbreviation publishes an artifact nothing will fetch.
var shaLengths = [...]int{40, 64}

// ValidateSHA rejects a head commit that is not a full git object id.
func ValidateSHA(sha string) error {
	s := Tag(sha)
	if s == "" {
		return errors.New("a head commit SHA is required")
	}
	ok := false
	for _, n := range shaLengths {
		if len(s) == n {
			ok = true
		}
	}
	if !ok {
		return fmt.Errorf("head commit %q is %d characters; a full git commit id is %d (sha1) or %d (sha256), and an abbreviated one would tag the artifact where nothing looks for it",
			sha, len(s), shaLengths[0], shaLengths[1])
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return fmt.Errorf("head commit %q is not hexadecimal", sha)
		}
	}
	return nil
}
