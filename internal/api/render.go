package api

import (
	"context"
	"fmt"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/diff"
	"github.com/dafrie/kelson/internal/renderer"
)

// Exit semantics of a diff, mirroring the CLI's exit contract (cmd/kelson/exit.go
// and diffExitCode in cmd/kelson/diff.go). The API reports the code rather than
// exiting with it, so a CI gate driving the server and a CI gate driving the
// binary reach the same verdict from the same preview.
const (
	exitSemanticsClean   int32 = 0
	exitSemanticsDiff    int32 = 2
	exitSemanticsBlocked int32 = 3
)

// Render renders one environment of a spec. Validation and render failures are
// returned in RenderResponse.errors with the RPC succeeding — the schema's
// contract is that a non-empty errors list means no manifests.
func (s *Server) Render(ctx context.Context, req *connect.Request[kelsonv1alpha1.RenderRequest]) (*connect.Response[kelsonv1alpha1.RenderResponse], error) {
	msg := req.Msg
	out, err := s.renderSpec(ctx, msg.GetSpec(), msg.GetEnvironment(), msg.GetImage(), msg.GetProfile())
	if err != nil {
		if wire := specFindings(err); len(wire) > 0 {
			return connect.NewResponse(&kelsonv1alpha1.RenderResponse{Errors: wire}), nil
		}
		return nil, failRequest(err)
	}
	manifests, err := wireManifests(out.manifests)
	if err != nil {
		return nil, fail(connect.CodeInternal, err)
	}
	return connect.NewResponse(&kelsonv1alpha1.RenderResponse{Manifests: manifests}), nil
}

// Diff previews what would change, at the fidelity the request selects: RENDER
// is offline and compares against the `from` documents rendered the same way or
// against the manifests a recorded revision actually rendered (`from_revision`,
// #162), SERVER asks the live API server for its own verdict through the
// dry-run engine — the same levels `kelson diff` offers (issue #46).
func (s *Server) Diff(ctx context.Context, req *connect.Request[kelsonv1alpha1.DiffRequest]) (*connect.Response[kelsonv1alpha1.DiffResponse], error) {
	msg := req.Msg
	if err := checkDiffBefore(msg); err != nil {
		return nil, fail(connect.CodeInvalidArgument, err)
	}
	cur, err := s.renderSpec(ctx, msg.GetSpec(), msg.GetEnvironment(), msg.GetImage(), msg.GetProfile())
	if err != nil {
		if wire := specFindings(err); len(wire) > 0 {
			return connect.NewResponse(&kelsonv1alpha1.DiffResponse{Errors: wire}), nil
		}
		return nil, failRequest(err)
	}

	var d *diff.Diff
	switch {
	case msg.GetFromRevision() != "":
		d, err = s.revisionDiff(ctx, msg.GetFromRevision(), cur)
	case msg.GetDryRun() == kelsonv1alpha1.DryRun_DRY_RUN_SERVER:
		d, err = s.serverDiff(ctx, cur)
	default:
		d, err = s.renderedDiff(ctx, msg, cur)
	}
	if err != nil {
		if wire := specFindings(err); len(wire) > 0 {
			return connect.NewResponse(&kelsonv1alpha1.DiffResponse{Errors: wire}), nil
		}
		return nil, failRequest(err)
	}

	encoded, err := diff.EncodeJSON(d)
	if err != nil {
		return nil, fail(connect.CodeInternal, err)
	}
	return connect.NewResponse(&kelsonv1alpha1.DiffResponse{
		DiffJson:      encoded,
		ExitSemantics: diffExitCode(d),
	}), nil
}

// renderedDiff computes the offline (L1) diff. The before side is the request's
// `from` documents rendered with the same environment, profile and image as the
// current spec; with no `from` there is no prior state, so every current
// resource is an addition — the honest answer when nothing has been recorded.
func (s *Server) renderedDiff(ctx context.Context, msg *kelsonv1alpha1.DiffRequest, cur *rendered) (*diff.Diff, error) {
	var prev []renderer.Manifest
	if from := msg.GetFrom(); from != nil {
		ref := &kelsonv1alpha1.SpecRef{Spec: &kelsonv1alpha1.SpecRef_Documents{Documents: from}}
		before, err := s.renderSpec(ctx, ref, cur.environment.Metadata.Name, msg.GetImage(), msg.GetProfile())
		if err != nil {
			return nil, err
		}
		prev = before.manifests
	}
	return diff.Between(cur.project.Metadata.Name, cur.environment.Metadata.Name, prev, cur.manifests, nil)
}

// checkDiffBefore enforces that a request names at most one prior state, and
// that a revision is only ever compared at the rendered level.
//
// `from` and `from_revision` are two answers to the same question and a request
// carrying both has no defensible reading — silently preferring one would make
// the verdict depend on a precedence rule nobody wrote down. dry_run=SERVER is
// refused with `from_revision` for a sharper reason: an L2 preview is the live
// cluster's verdict on the current set, so it has no "before" side a revision
// could occupy, and answering it anyway would report a comparison the caller
// did not ask for.
func checkDiffBefore(msg *kelsonv1alpha1.DiffRequest) error {
	if msg.GetFromRevision() == "" {
		return nil
	}
	if msg.GetFrom() != nil {
		return fmt.Errorf("api: from and from_revision both name the state to compare against; set one (from_revision reads what revision %q actually rendered, from re-renders documents you supply)",
			msg.GetFromRevision())
	}
	if msg.GetDryRun() == kelsonv1alpha1.DryRun_DRY_RUN_SERVER {
		return fmt.Errorf("api: from_revision is a rendered-level comparison and dry_run=SERVER is the live cluster's verdict on the current set; ask for one or the other")
	}
	return nil
}

// revisionDiff computes the rendered diff against what a recorded revision
// actually rendered: the before side is the manifests the delivery history kept
// for that revision, the after side is today's render of the spec.
//
// It reads the before side through the same seam the rollback preview uses
// (Plane.Recorded, a rollback.Source) and for the same reason (#38): the
// recorded bytes are what was applied, and re-rendering the old spec would
// report what that spec produces under today's renderer and ClusterProfile —
// a different question. rollback.PreviewRevision is not reused because its
// before side is the recorded *current* state rather than the spec being
// diffed, and because its irreversibility annotation is a rollback verdict, not
// a property of a diff. What is reused is the comparison underneath it,
// diff.BetweenDocuments, which is also the only entry point that takes recorded
// bytes on one side: diff.Between wants renderer.Manifest on both, and a
// recorded revision has none to offer.
func (s *Server) revisionDiff(ctx context.Context, revision string, cur *rendered) (*diff.Diff, error) {
	set, err := manifestSet(cur)
	if err != nil {
		return nil, err
	}
	adapter, plane, err := s.selectAdapter(ctx, target(cur, ""))
	if err != nil {
		return nil, err
	}
	if plane.Recorded == nil {
		return nil, fmt.Errorf("api: delivery mode %q keeps no rendered history kelson can read, so there is no revision to compare against; diff against the live cluster with dry_run=SERVER instead", adapter.Name())
	}

	// The revision is checked against the adapter's history first so "no such
	// revision" is the same answer in every mode. A rollback.Source reports a
	// missing revision differently depending on which store backs it, and a
	// caller branching on the code must not be reading which store the server
	// was started with.
	entries, err := adapter.History(ctx, delivery.ManifestSet{Project: set.Project, Environment: set.Environment})
	if err != nil {
		return nil, err
	}
	if _, ok := findRevision(entries, revision); !ok {
		return nil, fail(connect.CodeNotFound,
			fmt.Errorf("api: revision %q is not in the recorded history for %s/%s; call History for the revisions that still exist, older ones are pruned by the retention policy",
				revision, set.Project, set.Environment))
	}

	prev, err := plane.Recorded.Revision(ctx, revision)
	if err != nil {
		return nil, err
	}
	return diff.BetweenDocuments(set.Project, set.Environment, recordedDocuments(prev), recordedDocuments(set.Manifests), nil)
}

// recordedDocuments extracts the rendered bytes of a manifest list, preserving
// apply order.
func recordedDocuments(ms []delivery.Manifest) [][]byte {
	out := make([][]byte, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.YAML)
	}
	return out
}

// serverDiff drives the L2 engine. It never falls back to a rendered diff on
// failure: an unreachable cluster is a real error, and silently downgrading a
// requested server-side preview would give a CI gate a clean answer it did not
// earn (the same rule cmd/kelson's newServerDryRun states).
func (s *Server) serverDiff(ctx context.Context, cur *rendered) (*diff.Diff, error) {
	if s.preview == nil {
		return nil, unimplemented("the server-side dry-run engine")
	}
	engine, err := s.preview(ctx, cur.profile)
	if err != nil {
		return nil, unavailable("api: building the server-side dry-run engine: %w", err)
	}
	set, err := manifestSet(cur)
	if err != nil {
		return nil, err
	}
	return engine.Preview(ctx, set)
}

// diffExitCode maps a preview onto the CI exit contract. It is a deliberate
// replica of cmd/kelson/diff.go's diffExitCode — the two must agree, because a
// gate that runs `kelson diff` and a gate that calls this RPC are asserting the
// same thing about the same preview.
func diffExitCode(d *diff.Diff) int32 {
	for _, v := range d.Violations {
		if v.Enforcement == diff.EnforcementEnforce {
			return exitSemanticsBlocked
		}
	}
	for _, u := range d.Unvalidated {
		if !u.InBatch {
			return exitSemanticsBlocked
		}
	}
	if len(d.Resources) > 0 {
		return exitSemanticsDiff
	}
	return exitSemanticsClean
}
