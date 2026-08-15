package model

import "slices"

// autoDeploy: which components follow their source (ADR-0036 decision 1).
//
// The flag is two-level and the innermost wins, which resolution collapses into
// one list of names on [Resolved] rather than leaving every consumer to merge
// the levels itself — a second derivation of a precedence rule is a second
// answer, which is the argument [Resolved.SourceFor] already makes for
// bindings.
//
// What resolution records here is *what is true of the spec*, not what to do
// about it. Enqueuing a reconciliation, running a build, publishing an artifact
// and reporting the outcome belong to the trigger paths (ADR-0036 decision 3,
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
	if ov.Image != "" || c.Image != "" {
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
// image. Such a component never auto-deploys regardless of the flag, and a
// trigger path that was asked to move it says so rather than moving it
// (ADR-0036 decisions 2 and 3).
func (r *Resolved) ImagePinned(component string) bool {
	return slices.Contains(r.ImagePins, component)
}
