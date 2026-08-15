package api

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/artifact"
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

// revisionDiff compares today's render against the manifests a recorded
// revision actually published (issue #247, ADR-0028 decisions 4 and 5).
//
// # Where the before side comes from, and where it does not
//
// From the registry, as bytes. Every revision's rendered set is an immutable
// OCI artifact (ADR-0028 decision 2), so the comparison is against what was
// applied rather than against what the old spec would produce under today's
// renderer and ClusterProfile — which is a different question, and the reason
// this handler refused for two milestones rather than re-rendering a stored
// spec and calling it a diff.
//
// The digest the mirror recorded for the revision is passed to the pull when
// there is one, so the bytes are checked against what this environment says it
// published and not only against what the tag points at today. A revision that
// has aged out of the twenty-entry mirror has no recorded digest here; the pull
// is still verified end to end against the digest the registry resolves the tag
// to, and against each layer descriptor.
//
// # Beyond the window is served, not refused
//
// The mirror is bounded and the registry is the record (ADR-0028 decision 4).
// Rollback already takes that posture — it *confirms* a target the mirror has
// forgotten and restores it — and a diff that refused the same revision would
// make the comparison unavailable for exactly the revisions somebody is
// auditing, which is what the bound was never meant to mean. So the mirror is
// consulted for the digest and never for permission: if the artifact is
// pullable, the diff is served.
//
// # What each failure is
//
// A revision neither the mirror nor the registry holds is the caller's argument
// being wrong. A registry that could not be read is this server's dependency
// failing — Unavailable, never InvalidArgument, because telling a caller their
// revision is gone on the strength of a registry that never answered is the one
// answer this path must not give. Bytes that do not match their digest are
// neither: nothing about them is retryable and no diff may be computed from
// them, so they are DataLoss and the message names both digests.
func (s *Server) revisionDiff(ctx context.Context, revision string, cur *rendered) (*diff.Diff, error) {
	project, environment := cur.project.Metadata.Name, cur.environment.Metadata.Name
	if s.revisions == nil {
		return nil, unimplemented("comparing against a published revision")
	}
	fetcher, ok := s.revisions.(RevisionFetcher)
	if !ok {
		return nil, unimplemented("reading a published revision's artifact back")
	}
	// The tag grammar, checked before the registry is asked, for the reason
	// rollbackTarget checks it: a caller who typed a branch name or a digest
	// reads that they named the wrong *kind* of thing rather than that their
	// revision does not exist.
	if !revisionFormat.MatchString(revision) {
		return nil, fmt.Errorf(
			"api: %q is not a revision: a revision is <generation>-<spec-hash-short>, e.g. 7-a1b2c3d4 "+
				"(`kelson history` lists what %s/%s has published)", revision, project, environment)
	}

	pulled, found, err := fetcher.Fetch(ctx, project, environment, revision, s.recordedDigest(ctx, project, environment, revision))
	if err != nil {
		return nil, fetchFailure(err, project, environment, revision)
	}
	if !found {
		return nil, fmt.Errorf(
			"api: %s/%s has no revision %q in the registry, which holds every artifact it ever published "+
				"(ADR-0028 decision 4). `kelson history` lists them; `from` compares against documents you "+
				"supply and dry_run=SERVER against the live cluster",
			project, environment, revision)
	}

	prev := make([][]byte, 0, len(pulled.Files))
	for _, f := range pulled.Files {
		prev = append(prev, f.Data)
	}
	curDocs, err := renderedDocuments(cur)
	if err != nil {
		return nil, err
	}
	// BetweenDocuments and not Between: the recorded side is bytes with no
	// renderer.Manifest behind them, and re-parsing them into one would be a
	// second reading of a document that already says what it is.
	return diff.BetweenDocuments(project, environment, prev, curDocs, nil)
}

// recordedDigest is what the bounded mirror remembers this revision's bytes to
// be, or empty when it has forgotten (or when this server has no environment
// store at all).
//
// A store that cannot be read costs the cross-check and nothing else, so it is
// not fatal: the pull still verifies the manifest against the digest the
// registry resolves the tag to and each layer against its descriptor. What the
// mirror adds is the stronger claim — that those bytes are the ones this
// environment recorded publishing — and losing it silently would be wrong only
// if it were the whole verification, which it is not.
func (s *Server) recordedDigest(ctx context.Context, project, environment, revision string) string {
	if s.environments == nil {
		return ""
	}
	environments, err := s.environmentStore()
	if err != nil {
		return ""
	}
	st, err := environments.Get(ctx, project, environment)
	if err != nil {
		return ""
	}
	if recorded, ok := st.FindRevision(revision); ok {
		return recorded.Digest
	}
	return ""
}

// fetchFailure maps a failed artifact read onto the code that tells the caller
// what to do about it. An integrity failure is DataLoss and carries both
// digests; everything else the registry did — refused, timed out, was never
// reachable — is this server's dependency failing.
func fetchFailure(err error, project, environment, revision string) error {
	var integrity *artifact.IntegrityError
	if errors.As(err, &integrity) {
		return connect.NewError(connect.CodeDataLoss, fmt.Errorf(
			"api: the artifact for %s/%s revision %s does not match the digest that names it, so kelson will "+
				"not diff against it: %w", project, environment, revision, err))
	}
	return unavailable("api: reading the artifact for %s/%s revision %s: %w", project, environment, revision, err)
}

// renderedDocuments is the current side of a comparison as bytes, in render
// order — the same encoding the artifact for this render would carry.
func renderedDocuments(cur *rendered) ([][]byte, error) {
	docs := make([][]byte, 0, len(cur.manifests))
	for _, m := range cur.manifests {
		body, err := m.YAML()
		if err != nil {
			return nil, fmt.Errorf("api: encoding manifest %s/%s: %w", m.Kind, m.Name, err)
		}
		docs = append(docs, body)
	}
	return docs, nil
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
