package rollback

import (
	"context"
	"fmt"

	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/delivery/direct"
)

// Source supplies the two manifest sets a rollback preview compares: current
// state (the before side) and the rendered manifests recorded for the target
// revision (the after side).
//
// Direct mode reads both from its rendered-history store; the Git modes read
// them from the deployment repository. rollback/ provides DirectSource for the
// direct case; the Git adapters adapt their writer (whose FilesAt already
// resolves recorded files by revision) behind the same seam. Both sides must be
// the recorded rendered bytes, never a re-render: #38 defines rollback as
// replaying recorded output, and a re-render would change what is reverted.
type Source interface {
	// Current returns the manifests live now, or nil when nothing has been
	// deployed yet.
	Current(ctx context.Context) ([]delivery.Manifest, error)
	// Revision returns the rendered manifests recorded for one revision. A
	// revision pruned from retention is reported as an error, not silently
	// replayed as empty history.
	Revision(ctx context.Context, revision string) ([]delivery.Manifest, error)
}

// DirectSource reads recorded manifest sets from a direct-mode history store
// (internal/delivery/direct). It wraps the store's Rendered output and splits
// it back into manifests with direct.SplitDocuments — the inverse of the
// recorded stream, so the bytes replayed are exactly the bytes applied.
//
// Store is the direct.History seam, not the JSONL *direct.Store concretely, so
// a rollback preview reads the same recorded bytes whether the deploy was made
// by the CLI (local journal) or by kelson-server (ConfigMaps, ADR-0013 §1).
type DirectSource struct {
	Store       direct.History
	Project     string
	Environment string
}

// Current returns the manifests of the newest recorded revision, or nil when
// nothing is live yet.
func (s *DirectSource) Current(ctx context.Context) ([]delivery.Manifest, error) {
	rec, err := s.Store.Latest(s.Project, s.Environment)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, nil
	}
	return s.read(rec.Revision)
}

// Revision returns the recorded manifests for one retention-held revision.
func (s *DirectSource) Revision(ctx context.Context, revision string) ([]delivery.Manifest, error) {
	return s.read(revision)
}

// read fetches and splits the recorded rendered output for a revision. It
// checks retention first so a pruned revision is reported as such rather than
// as a bare filesystem error.
func (s *DirectSource) read(revision string) ([]delivery.Manifest, error) {
	rec, err := s.Store.Get(s.Project, s.Environment, revision)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, fmt.Errorf("rollback: revision %q is not in the retained history; list History() for the revisions that still exist", revision)
	}
	rendered, err := s.Store.Rendered(s.Project, s.Environment, revision)
	if err != nil {
		return nil, fmt.Errorf("rollback: read recorded manifests for %s: %w", revision, err)
	}
	return direct.SplitDocuments(rendered)
}
