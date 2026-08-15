package controller

import (
	"context"
	"fmt"
	"strings"

	"github.com/dafrie/kelson/api/kelson/v1alpha1"
)

// rollback is what the annotation means for this reconcile.
type rollback struct {
	// Requested is the annotation's value, trimmed. Empty means there is none.
	Requested string
	// Active means steps 3 and 4 are suspended and the pair is pinned to
	// Requested.
	Active bool
	// Inert means the annotation is present and deliberately ignored, because
	// the spec has been edited since it was applied. The Progressing condition
	// says so, because an annotation that silently stopped mattering is worse
	// than one that never worked.
	Inert bool
	// Generation is what status.rollbackGeneration should hold after this
	// reconcile.
	Generation int64
}

// Pin is the tag the pair should be pinned to, which is the requested target
// only while the rollback is in force. An inert rollback still carries its
// Requested value — the Progressing condition names it — and must not pin, so
// the two are read through different accessors rather than through the same
// field twice.
func (r rollback) Pin() string {
	if r.Active {
		return r.Requested
	}
	return ""
}

// rollbackFor decides what [v1alpha1.AnnotationRollbackTo] means for one
// Environment (ADR-0028 decision 5).
//
// # The three states, and why the third exists
//
// The annotation suspends re-rendering, and the ADR names exactly two ways out:
// removing it, or editing the spec. The first is trivial — no annotation, no
// rollback. The second is the interesting one, and it is why this function
// needs the status as well as the object.
//
// A spec edit bumps `.metadata.generation`. So the rollback records the
// generation it was applied at, and any generation past that one is an
// unambiguous statement of new intent made *after* the rollback: normal
// publishing resumes and the annotation goes inert, without the operator who
// has just fixed the bug having to remember to remove it as well.
//
// Re-applying the annotation with a *different* value is a new rollback, even
// at the same generation, because the operator has named a different target.
// Re-applying the same value is the standing one, so it is not re-verified and
// nothing is re-applied.
func rollbackFor(annotations map[string]string, generation int64, status v1alpha1.EnvironmentStatus) rollback {
	requested := strings.TrimSpace(annotations[v1alpha1.AnnotationRollbackTo])
	if requested == "" {
		return rollback{}
	}
	switch {
	case requested != status.RollbackRevision:
		// A target nobody has acted on yet: a new rollback, pinned at the
		// generation it arrived at.
		return rollback{Requested: requested, Active: true, Generation: generation}
	case generation > status.RollbackGeneration:
		// The standing rollback, and a spec edit since. Intent wins.
		return rollback{Requested: requested, Inert: true, Generation: status.RollbackGeneration}
	default:
		return rollback{Requested: requested, Active: true, Generation: status.RollbackGeneration}
	}
}

// rollbackTarget is a verified target: which bytes it names, and where kelson
// found out it exists.
type rollbackTarget struct {
	// Digest is the artifact digest the pair is pinned to alongside the tag.
	// Empty is a tag-only pin: an entry recorded before digests were, or a
	// registry that served the tag without naming one.
	Digest string

	// BeyondWindow means the bounded mirror does not hold this revision and the
	// registry does. The rollback is exactly as exact — the tag is immutable
	// either way — but everything the mirror would have said *about* that
	// revision is gone, and the status has to say so rather than leave a
	// reader to assume kelson still knows (see [rolledBackMessage]).
	BeyondWindow bool
}

// verifyRollbackTarget checks the target against the bounded history mirror,
// and then against the record the mirror is a mirror of.
//
// kelson will not point an OCIRepository at a tag it cannot confirm it
// published. The alternative — pin it and let source-controller fail on a
// manifest-unknown — turns a typo into a broken deployment reported in somebody
// else's vocabulary.
//
// # Why the mirror alone was the wrong question
//
// `status.history` is bounded at [v1alpha1.MaxHistoryEntries] and the registry
// holds every artifact ever published, immutably. So a *correct* target that
// had merely aged out of the window read exactly like a typo and was refused
// exactly like one (issue #241) — a revision that still existed, still
// deployable, unreachable because the thing that remembers it is twenty entries
// long. The mirror is checked first because it is free and it knows more; the
// registry is asked when the mirror comes up empty, which is what ADR-0028
// decision 4 means by "`kelson history` beyond the window is a registry query".
//
// # What each source can answer
//
// The mirror knows a revision's digest, its spec hash, when it was published,
// which images it ran and how that deployment ended. The registry knows that
// the tag exists and which bytes it names — and nothing else, because the rest
// were observations of a cluster that no registry ever saw. That difference
// travels back in [rollbackTarget.BeyondWindow] rather than being flattened,
// because a status that answered "outcome: " for a revision it cannot describe
// would be inventing a fact of the most misleading kind.
//
// A lister that fails is not a target that does not exist. The refusal keeps
// the registry's own reason (unreachable, read denied) so an operator is told
// the record could not be read, rather than told their revision is gone.
func (r *EnvironmentReconciler) verifyRollbackTarget(ctx context.Context, project, environment, target string,
	history []v1alpha1.HistoryEntry) (rollbackTarget, error) {
	for _, e := range history {
		if e.Revision == target {
			return rollbackTarget{Digest: e.Digest}, nil
		}
	}

	mirror := "status.history is empty: this environment has published nothing yet"
	if known := revisionsOf(history); len(known) > 0 {
		mirror = fmt.Sprintf("status.history holds %s", strings.Join(known, ", "))
	}
	if r.Revisions == nil {
		return rollbackTarget{}, newDeliveryError(v1alpha1.ReasonRollbackTargetUnknown, fmt.Sprintf(
			"%s names revision %q, and %s. The mirror is bounded at %d entries, and this controller has no "+
				"registry to check the record against — the registry holds every revision ever published "+
				"(ADR-0028 decision 4), so set --registry (or KELSON_REGISTRY) to reach past the window. "+
				"Remove the annotation to resume tracking the spec.",
			v1alpha1.AnnotationRollbackTo, target, mirror, v1alpha1.MaxHistoryEntries), nil)
	}

	digest, found, err := r.Revisions.Resolve(ctx, project, environment, target)
	if err != nil {
		return rollbackTarget{}, err
	}
	if found {
		return rollbackTarget{Digest: digest, BeyondWindow: true}, nil
	}

	// Neither place has it. Listing what the registry does hold is worth one
	// more request here: the reader has typed a revision that does not exist,
	// and the useful part of that answer is which ones do.
	detail := "and the registry does not hold it either"
	if revisions, listErr := r.Revisions.Revisions(ctx, project, environment); listErr == nil {
		detail = "and " + describeRevisions(revisions)
	}
	return rollbackTarget{}, newDeliveryError(v1alpha1.ReasonRollbackTargetUnknown, fmt.Sprintf(
		"%s names revision %q, %s, %s. Both places kelson can confirm a revision from have been asked: the "+
			"mirror bounded at %d entries, and the registry's tag list, which is the record it mirrors "+
			"(ADR-0028 decision 4). Remove the annotation to resume tracking the spec.",
		v1alpha1.AnnotationRollbackTo, target, mirror, detail, v1alpha1.MaxHistoryEntries), nil)
}

func revisionsOf(history []v1alpha1.HistoryEntry) []string {
	known := make([]string, 0, len(history))
	for _, e := range history {
		known = append(known, e.Revision)
	}
	return known
}

// rolledBackMessage is what Ready=True says while a rollback is serving.
//
// A target that came from the bounded mirror is described by it: kelson knows
// when that revision was published, what it ran and how it went. A target the
// mirror has forgotten is described by the registry, which knows the tag exists
// and nothing else — so the message says which of the two this is, in the same
// place a reader is already looking. The rollback itself is no less exact
// either way: the tag is immutable, and the pair is pinned to the same bytes.
func rolledBackMessage(revision string, beyondWindow bool) string {
	if !beyondWindow {
		return fmt.Sprintf("serving revision %s, which this environment published earlier", revision)
	}
	return fmt.Sprintf("serving revision %s, which is older than the %d entries status.history keeps. The "+
		"registry confirms the artifact and kelson pinned it, so the rollback is exact; what kelson cannot "+
		"say about it is when it was published, which images it ran or how that deployment ended — the "+
		"mirror held those and the registry never saw them (ADR-0028 decision 4).",
		revision, v1alpha1.MaxHistoryEntries)
}

// rollbackPinnedMessage is what Progressing=False says while a rollback is in
// force. It names the annotation and both ways out, because an operator looking
// at an environment that will not deploy their edit is asking exactly one
// question and this is the answer to it.
func rollbackPinnedMessage(target string) string {
	return fmt.Sprintf(
		"pinned to revision %s: re-rendering is suspended while %s is set, so the current spec cannot be "+
			"republished over it. Two things resume tracking: remove the annotation, or edit the spec.",
		target, v1alpha1.AnnotationRollbackTo)
}

// rollbackInertMessage is the other half. The annotation is still on the object
// and no longer does anything, which is a state that must be said out loud.
func rollbackInertMessage(target string, at int64) string {
	return fmt.Sprintf(
		"%s=%s is inert: the spec was edited after the rollback (generation is past %d), which resumes "+
			"normal publishing. Remove the annotation — it stays inert until then, and re-applying it "+
			"with a different target starts a new rollback.",
		v1alpha1.AnnotationRollbackTo, target, at)
}
