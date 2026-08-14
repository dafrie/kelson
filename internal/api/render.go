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

// revisionDiff is gated (issue #224).
//
// Diffing against a recorded revision compared today's render with the bytes
// that were actually applied then, read through the rendered-history store.
// ADR-0027 decision 7 deleted that store, and its replacement — pull the
// immutable OCI artifact for the revision and compare against its contents
// (ADR-0028 decision 4) — is not built.
//
// It refuses rather than falling back to re-rendering the old spec, which would
// answer a different question: what that spec produces under TODAY's renderer
// and ClusterProfile, which is not what was applied. The two other diff modes
// are unaffected — `from` re-renders documents the caller supplies, and
// dry_run=SERVER is the live cluster's verdict on the current set — and the
// refusal names them.
func (s *Server) revisionDiff(_ context.Context, revision string, cur *rendered) (*diff.Diff, error) {
	return nil, delivery.NotImplemented("diff",
		fmt.Sprintf("kelson cannot compare against revision %q: the recorded manifests of a past revision "+
			"came from the rendered-history store, which was deleted with the old delivery machinery. "+
			"Diff against documents you supply with `from`, or against the live cluster with "+
			"dry_run=SERVER, both of which are unaffected", revision),
		"#224")
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

// diffExitCode maps a preview onto the CI exit contract, mirroring
// cmd/kelson/diff.go's diffExitCode. The two must agree, because a gate that
// runs `kelson diff` and a gate that calls this RPC are asserting the same
// thing about the same preview — so the "is this blocked?" half lives in
// diff.Blocked and both call it.
func diffExitCode(d *diff.Diff) int32 {
	if diff.Blocked(d) {
		return exitSemanticsBlocked
	}
	if len(d.Resources) > 0 {
		return exitSemanticsDiff
	}
	return exitSemanticsClean
}
