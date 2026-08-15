package controller

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/dafrie/kelson/api/kelson/v1alpha1"
)

// recordHistory folds one reconcile's outcome into the bounded history mirror
// (ADR-0028 decision 4).
//
// # Three rules, and each of them is a bug that would otherwise be easy to write
//
//   - **Only a new publish prepends.** A reconcile that observed an unchanged
//     revision, or that skipped the publish because the tag was already
//     serving, must not add an entry — otherwise a healthy environment
//     reconciling on its interval fills the whole twenty-entry window with the
//     same revision inside two hours, and the history stops being one.
//   - **Dedupe by revision.** A tag is immutable, so two entries with the same
//     revision are the same artifact and one of them is noise. Republishing the
//     same tag (which the determinism guarantee makes a no-op at the registry)
//     therefore refreshes the entry rather than duplicating it.
//   - **The head entry's outcome stays live.** A revision is published as
//     Committed and becomes Healthy — or Degraded — some seconds later, and an
//     entry frozen at the moment of publication would record that every
//     deployment ended in "Committed". So the newest entry's outcome is
//     refreshed while it is the current revision, and frozen once a newer one
//     takes its place. That is what makes `status.history` answer "how did that
//     deployment go" rather than "what did it look like one second in".
//
// A rollback does not prepend either: it publishes nothing, and the revision it
// pins is already in the history by definition — that is what
// verifyRollbackTarget checked.
func recordHistory(history []v1alpha1.HistoryEntry, out Outcome, specHash string, now metav1.Time) []v1alpha1.HistoryEntry {
	if out.Revision == "" {
		return history
	}

	if out.Published {
		entry := v1alpha1.HistoryEntry{
			Revision:        out.Revision,
			Digest:          out.Digest,
			SpecHash:        specHash,
			Images:          flatImages(out.Images),
			ComponentImages: out.Images,
			Outcome:         out.Phase,
			Timestamp:       now,
		}
		// Drop any earlier entry for this revision before prepending, so the
		// refreshed one keeps the newest-first ordering rather than appearing
		// twice.
		kept := make([]v1alpha1.HistoryEntry, 0, len(history)+1)
		kept = append(kept, entry)
		for _, e := range history {
			if e.Revision == out.Revision {
				continue
			}
			kept = append(kept, e)
		}
		return trimHistory(kept)
	}

	// Not a new publish: refresh the head entry's outcome if it is still the
	// revision being observed, and change nothing otherwise.
	if len(history) > 0 && history[0].Revision == out.Revision && out.Phase != "" {
		history[0].Outcome = out.Phase
		if history[0].Digest == "" {
			history[0].Digest = out.Digest
		}
	}
	return trimHistory(history)
}

// flatImages is the deprecated [v1alpha1.HistoryEntry.Images] mirror of the
// attributed list: the same images, in the same order, with the component names
// dropped. It is written for one release so a status reader built against the
// older shape keeps working, and goes away with the field.
func flatImages(images []v1alpha1.ComponentImage) []string {
	if len(images) == 0 {
		return nil
	}
	out := make([]string, 0, len(images))
	for _, i := range images {
		out = append(out, i.Image)
	}
	return out
}

// trimHistory enforces the bound. It is the schema's bound too
// (internal/schemagen/status.go sets maxItems), so a status that exceeded it
// would be rejected by the API server rather than merely being large.
func trimHistory(history []v1alpha1.HistoryEntry) []v1alpha1.HistoryEntry {
	if len(history) <= v1alpha1.MaxHistoryEntries {
		return history
	}
	return history[:v1alpha1.MaxHistoryEntries]
}
