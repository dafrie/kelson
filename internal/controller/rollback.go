package controller

import (
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

// verifyRollbackTarget checks the target against the bounded history mirror.
//
// kelson will not point an OCIRepository at a tag it cannot confirm it
// published. The alternative — pin it and let source-controller fail on a
// manifest-unknown — turns a typo into a broken deployment reported in
// somebody else's vocabulary, and a *correct* target that happens to be older
// than the window would look identical to a typo.
//
// So the window is the answer, and the refusal says so: `status.history` is
// bounded at [v1alpha1.MaxHistoryEntries], the registry holds everything ever
// published, and a target beyond the window is a registry query rather than
// something kelson can confirm from its own status (ADR-0028 decision 4).
// It returns the entry's digest, which is the other reason the lookup is here:
// the pair is pinned to the target's bytes and not only to its tag when the
// history knows them (fluxobjects.go).
func verifyRollbackTarget(target string, history []v1alpha1.HistoryEntry) (string, error) {
	for _, e := range history {
		if e.Revision == target {
			return e.Digest, nil
		}
	}
	known := make([]string, 0, len(history))
	for _, e := range history {
		known = append(known, e.Revision)
	}
	detail := "status.history is empty: this environment has published nothing yet"
	if len(known) > 0 {
		detail = fmt.Sprintf("status.history holds %s", strings.Join(known, ", "))
	}
	return "", newDeliveryError(v1alpha1.ReasonRollbackTargetUnknown, fmt.Sprintf(
		"%s names revision %q, and %s. The mirror is bounded at %d entries — the registry holds every "+
			"revision ever published, so a target older than the window is a registry query "+
			"(ADR-0028 decision 4). Remove the annotation to resume tracking the spec.",
		v1alpha1.AnnotationRollbackTo, target, detail, v1alpha1.MaxHistoryEntries), nil)
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
