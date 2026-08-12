package rollback

import (
	"context"
	"fmt"

	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/diff"
)

// PreviewRevision answers "what happens if I roll back to this revision",
// end to end: it reads both sides from a Source, diffs them, and annotates the
// result with the changes a rollback cannot safely revert.
//
// The direction matters and is easy to get backwards. The *before* side is
// current state and the *after* side is the target revision, because the
// question is what applying the rollback would do — not how the deployment got
// here.
//
// Both sides are recorded rendered bytes, never a re-render (#38), so the
// comparison uses diff.BetweenDocuments. Re-rendering the old spec would report
// what that spec produces under today's renderer and ClusterProfile, which is a
// different question and a misleading answer for a rollback.
func PreviewRevision(ctx context.Context, src Source, project, environment, revision string) (*diff.Diff, []Finding, error) {
	if src == nil {
		return nil, nil, fmt.Errorf("rollback: a Source is required to preview revision %q", revision)
	}
	current, err := src.Current(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("rollback: reading current state: %w", err)
	}
	target, err := src.Revision(ctx, revision)
	if err != nil {
		return nil, nil, fmt.Errorf("rollback: reading revision %q: %w", revision, err)
	}

	d, err := diff.BetweenDocuments(project, environment, documents(current), documents(target), nil)
	if err != nil {
		return nil, nil, fmt.Errorf("rollback: diffing revision %q: %w", revision, err)
	}
	annotated, findings := Preview(d)
	return annotated, findings, nil
}

// documents extracts the recorded bytes, preserving apply order.
func documents(ms []delivery.Manifest) [][]byte {
	out := make([][]byte, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.YAML)
	}
	return out
}
