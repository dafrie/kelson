package model

import (
	"net/url"
	"slices"
	"strings"
)

// autoDeploy: which components follow their source, and what a push moves
// (ADR-0036).
//
// The flag is two-level and the innermost wins (decision 1), which resolution
// collapses into one list of names on [Resolved] rather than leaving every
// consumer to merge the levels itself. The stale set (decision 2) is the
// question a trigger path actually asks — *this repository moved at this ref;
// what does that make out of date?* — and it is answered here, in the pure
// package, because every input to it is already in the resolved spec.
//
// # What this side of the fence does not do
//
// It does not parse a forge payload, and it does not know what `refs/heads/main`
// is. A ref reaches [Resolved.StaleComponents] already reduced to its short
// name, and the surface that received the delivery is the one that reduces it:
// this package takes no ambient input (ADR-0001, issue #20), and a second
// parser here would be a second answer to a question the webhook layer has
// already answered.
//
// It also does not decide what happens next. A stale set is a set of names;
// enqueuing the reconciliation, running the build, publishing the artifact and
// reporting the outcome belong to the trigger paths (ADR-0036 decision 3,
// issue #248), and an event never mutates state directly (ADR-0034 decision 1).

// effectiveAutoDeploy is ADR-0036 decision 1: the component's own setting if it
// declared one, else the environment's, else false.
//
// Nothing between the two levels is an error and no combination is refused —
// the same instinct as every other override in this model — so a component may
// track under an environment that does not, and stay manual under one that
// does. Both pointers are three-valued on purpose: "declined to decide" is what
// makes the inner level an override rather than a merge.
func effectiveAutoDeploy(environment, component *bool) bool {
	if component != nil {
		return *component
	}
	if environment != nil {
		return *environment
	}
	return false
}

// resolveTracking records the two per-component facts the trigger paths read:
// whether this component follows its source, and whether an image the spec
// names holds it still.
//
// Both are lost by the merges around them — a [ResolvedComponent] carries one
// image and no memory of which scope named it, and no memory of a two-level
// flag at all — so they are written down once, here, where both scopes are in
// hand.
func resolveTracking(r *Resolved, c Component, ov ComponentOverride, environment *bool) {
	if effectiveAutoDeploy(environment, ov.AutoDeploy) {
		r.AutoDeploy = append(r.AutoDeploy, c.Name)
	}
	// The Project's own `image:` is deliberately not a pin. `--image` stands in
	// for it (rule P3), which is exactly the mechanism by which a build moves a
	// component; the two inner scopes beat `--image`, so they are the ones that
	// hold a component still.
	//
	// Unless the override marks the image as tracked, which is ADR-0036
	// decision 5: a marked image renders as any other does — nothing here
	// touches [ResolvedComponent.Image] and rule P3 is untouched — but it is a
	// starting point rather than a hold, so it is left out of the pins and the
	// stale set may move it. The marker is the innermost scope's answer to the
	// question this list asks, so it cancels the component's own `image:` too.
	if !ov.ImageTracked && (ov.Image != "" || c.Image != "") {
		r.ImagePins = append(r.ImagePins, c.Name)
	}
}

// AutoDeploys reports whether component follows its source in this environment.
// It is the effective ADR-0036 decision 1 answer, asked the way
// [Resolved.SourceFor] is asked: of the resolved spec, so that no two callers
// re-derive the precedence rule and disagree.
func (r *Resolved) AutoDeploys(component string) bool {
	return slices.Contains(r.AutoDeploy, component)
}

// ImagePinned reports whether an image the spec names holds this component
// still — the environment's per-component pin (ADR-0016) or the component's own
// image, in neither case marked `imageTracked`. Such a component never
// auto-deploys regardless of the flag, and a trigger path that was asked to
// move it says so rather than moving it (ADR-0036 decisions 2, 3 and 5).
func (r *Resolved) ImagePinned(component string) bool {
	return slices.Contains(r.ImagePins, component)
}

// StaleComponents returns the components of this environment that a push of ref
// to repo makes out of date, in spec order — the stale set of ADR-0036
// decision 2. It is a pure function of the resolved spec: the same spec and the
// same push give the same answer, with no clone, no forge call and no clock.
//
// A component is in the set when all five hold:
//
//   - it is bound to a source whose repository is repo ([Resolved.Sources],
//     ADR-0035 decision 3) — the binding does the routing, so there is no
//     second per-component ref field that could disagree with it;
//   - that source's ref is ref;
//   - that source's ref is not a commit: a SHA-pinned source names one revision
//     forever and there is nothing about it to track;
//   - it tracks ([Resolved.AutoDeploy]);
//   - no image pin holds it ([Resolved.ImagePins]): a pinned component ignores
//     everything, which is the promotion posture ADR-0016 established and
//     ADR-0036 decision 2 restates rather than reopens. An image the override
//     marks `imageTracked` is not such a pin — it names where the component
//     starts, not that it stays there (decision 5), which is what lets a
//     second push move a component the first one moved.
//
// An empty answer is the ordinary case, and it means this environment does
// nothing — silently, because a push to a repository an environment happens to
// build from is not news.
//
// # The ref contract
//
// ref is the *short* name of what was pushed — `main`, `v1.2.3` — with
// `refs/heads/` or `refs/tags/` already stripped by whichever surface read the
// delivery. This half takes it as given for two reasons: the model parses no
// payloads, and the authored spec's own `ref:` is already namespace-free — a
// source at `v2` is a branch or a tag depending on what the repository has, and
// git decides that, not kelson. So the comparison is exact, case-sensitive
// equality over short names and the two namespaces collapse here exactly as
// they already do in the document. An empty repo or ref matches nothing rather
// than everything.
//
// repo is compared as a repository *reference* rather than as a string, by
// [SameRepository].
func (r *Resolved) StaleComponents(repo, ref string) []string {
	if repo == "" || ref == "" {
		return nil
	}
	var stale []string
	for _, b := range r.Sources {
		switch {
		case !SameRepository(b.Source.Git, repo):
		case b.Source.Ref != ref:
		case isCommitRef(b.Source.Ref):
		case !r.AutoDeploys(b.Component):
		case r.ImagePinned(b.Component):
		default:
			stale = append(stale, b.Component)
		}
	}
	return stale
}

// SameRepository reports whether two repository references name one repository.
//
// Host and path must agree, case-folded, with a `.git` suffix and any trailing
// slash off, and it accepts the spellings that actually appear: an https URL, a
// bare `host/path`, and the `git@host:path` form a `source.git` is often
// written in. The path alone would match `acme/checkout` on gitlab.com against
// the same path on github.com, which is a real collision for anyone mirroring,
// so the host is never dropped.
//
// It is exported and lives here because the question is about *spec-shaped*
// references — a source's `git:`, an environment's `previews.repo`, the URL a
// push names — and this package is where those fields are defined.
// internal/api holds a private copy of exactly this comparison, written before
// there was a home for it; collapsing that copy onto this one belongs to the
// next change that touches it. internal/forgehttp's is a different question and
// stays its own: it compares a *delivery* against a spec and brings the
// connection's host into the answer, which this one has no business knowing.
func SameRepository(a, b string) bool {
	aHost, aPath, aOK := splitRepository(a)
	bHost, bPath, bOK := splitRepository(b)
	return aOK && bOK && aHost == bHost && aPath == bPath
}

// splitRepository reduces a repository reference to its lowercased host and
// path, and reports whether it had both.
func splitRepository(raw string) (host, path string, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", false
	}
	if rest, found := strings.CutPrefix(raw, "git@"); found {
		// scp-like syntax: host and path are separated by a colon rather than a
		// slash, so url.Parse would read the path as a port.
		h, p, split := strings.Cut(rest, ":")
		if !split {
			return "", "", false
		}
		raw = "https://" + h + "/" + p
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", "", false
	}
	path = strings.ToLower(strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git"))
	if path == "" {
		return "", "", false
	}
	return strings.ToLower(u.Host), path, true
}

// commitHexLength is the length of a full git object id in hex.
const commitHexLength = 40

// isCommitRef reports whether a ref is a full commit hash — the one ref that
// tracks nothing, because it names one revision forever (ADR-0036 decision 2).
//
// internal/build.IsCommit is the same test one plane out, and this is not an
// import of it: that package is built on this one, so the dependency runs in
// exactly one direction. The definition is deliberately identical — a full
// 40-character hex object id, nothing abbreviated — so the two planes agree
// about what a pinned source is.
func isCommitRef(ref string) bool {
	if len(ref) != commitHexLength {
		return false
	}
	for i := range len(ref) {
		c := ref[i]
		hex := c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
		if !hex {
			return false
		}
	}
	return true
}
