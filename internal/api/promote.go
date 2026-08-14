package api

import (
	"context"
	"fmt"
	"sort"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/clusterprofile"
	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/diff"
	"github.com/dafrie/kelson/internal/model"
	"github.com/dafrie/kelson/internal/promote"
	"github.com/dafrie/kelson/internal/renderer"
	"github.com/dafrie/kelson/internal/serverstate"
)

// Promote pins the target environment to what the source environment's latest
// revision runs, and returns the diff that promotion produces (issue #11,
// ADR-0016 decision 2).
//
// # Three reads, one write
//
// The source environment's delivery history supplies the images — the manifests
// that revision recorded, not a re-render of its spec, because a promotion
// moves what ran and not what was intended. The stored spec supplies the target
// environment's document. internal/promote decides and splices; this handler
// only sequences and stores.
//
// # The diff is computed here, not left to the caller
//
// The response carries diff_json and exit_semantics exactly as DiffResponse
// does, because the alternative does not work for the case that matters: a dry
// run stores nothing, so there is no "after" a follow-up Diff call could
// compare against, and the caller would have to reconstruct the pinned
// documents itself to ask. Both document sets are already in this handler's
// hands, so the diff costs one extra render and removes a round trip and a
// class of client-side mistakes.
//
// # Nothing is written when anything is wrong
//
// Validation and render findings travel in `errors` with the RPC succeeding —
// the same contract PutSpec has — and a response carrying findings has stored
// nothing. dry_run RENDER and SERVER both compute and store nothing: as with
// PutSpec, a server-side dry-run adds no fidelity to a write that applies
// nothing to the cluster.
func (s *Server) Promote(ctx context.Context, req *connect.Request[kelsonv1alpha1.PromoteRequest]) (*connect.Response[kelsonv1alpha1.PromoteResponse], error) {
	msg := req.Msg
	if err := checkPromoteRequest(msg); err != nil {
		return nil, fail(connect.CodeInvalidArgument, err)
	}
	if s.specs == nil {
		return nil, unimplemented("the spec store")
	}

	stored, err := s.specs.Get(ctx, msg.GetProject())
	if err != nil {
		return nil, failRequest(err)
	}
	spec, err := decodeSpec(stored.Documents.Project, stored.Documents.Environments)
	if err != nil {
		if wire := specFindings(err); len(wire) > 0 {
			return connect.NewResponse(&kelsonv1alpha1.PromoteResponse{Errors: wire}), nil
		}
		return nil, failRequest(err)
	}
	source, err := selectEnvironment(spec.environments, msg.GetFromEnvironment())
	if err != nil {
		return nil, fail(connect.CodeInvalidArgument, err)
	}
	targetEnv, err := selectEnvironment(spec.environments, msg.GetToEnvironment())
	if err != nil {
		return nil, fail(connect.CodeInvalidArgument, err)
	}

	// Agent policy applies to the environment being *written*. Promoting reads
	// the source's history and rewrites the target's pins, so production's
	// policy governs a promotion into production regardless of where the images
	// came from (ADR-0025). A dry run stores nothing and is exempt, like every
	// other preview rung.
	if persists(msg.GetDryRun()) {
		if _, err := s.guard(ctx, model.AgentOpPromote, spec.project.Metadata.Name, targetEnv.Metadata.Name); err != nil {
			return nil, err
		}
	}

	revision, deployed, err := s.deployedImages(ctx, spec.project, source, msg.GetMode())
	if err != nil {
		return nil, failRequest(err)
	}
	changes, err := promote.Plan(spec.project, targetEnv, deployed, msg.GetComponents())
	if err != nil {
		return nil, failRequest(err)
	}

	after, err := pinnedDocuments(stored.Documents, targetEnv.Metadata.Name, changes)
	if err != nil {
		return nil, failRequest(err)
	}

	res := &kelsonv1alpha1.PromoteResponse{
		Components:   wirePromoted(changes),
		FromRevision: revision,
	}
	profile, err := s.resolveProfile(ctx, msg.GetProfile())
	if err != nil {
		return nil, failRequest(err)
	}
	if findings := s.promotionDiff(ctx, res, stored.Documents, after, targetEnv.Metadata.Name, profile); len(findings) > 0 {
		res.Errors = findings
		return connect.NewResponse(res), nil
	}
	if dry := msg.GetDryRun(); dry == kelsonv1alpha1.DryRun_DRY_RUN_RENDER || dry == kelsonv1alpha1.DryRun_DRY_RUN_SERVER {
		return connect.NewResponse(res), nil
	}

	// The version the write asserts: the caller's when it supplied one, so a
	// client that read the spec earlier still finds out it went stale, and
	// otherwise the version this handler just read — a promotion is a
	// read-modify-write and the read is right here.
	version := msg.GetVersion()
	if version == "" {
		version = stored.Version
	}
	written, err := s.specs.Put(ctx, stored.Project, after, serverstate.PutOptions{
		ExpectedVersion: version,
		IdempotencyKey:  msg.GetIdempotencyKey(),
	})
	if err != nil {
		return nil, failRequest(err)
	}
	res.Version = written.Version
	return connect.NewResponse(res), nil
}

// checkPromoteRequest refuses the shapes that have no reading.
func checkPromoteRequest(msg *kelsonv1alpha1.PromoteRequest) error {
	if msg.GetProject() == "" {
		return fmt.Errorf("api: Promote needs a stored project to promote within")
	}
	from, to := msg.GetFromEnvironment(), msg.GetToEnvironment()
	if from == "" || to == "" {
		return fmt.Errorf("api: Promote needs both from_environment and to_environment; a promotion has a source and a target")
	}
	if from == to {
		return fmt.Errorf("api: from_environment and to_environment are both %q; promoting an environment to itself would pin it to what it already runs", from)
	}
	return nil
}

// deployedImages reads what the source environment's latest revision runs.
//
// It reads the recorded manifests through the same seam the rollback preview
// and the revision diff use (Plane.Recorded), because those bytes are what was
// applied. A mode whose history kelson cannot read says so rather than falling
// back to a re-render, which would promote an intention.
func (s *Server) deployedImages(ctx context.Context, project *model.Project, source *model.Environment, mode string) (string, map[string]string, error) {
	resolved, errs := model.Resolve(project, source)
	if len(errs) > 0 {
		return "", nil, errs
	}
	t := Target{
		Project:     project.Metadata.Name,
		Environment: source.Metadata.Name,
		Namespace:   resolved.Environment.Namespace,
		Mode:        mode,
		Git:         resolved.Environment.Delivery.Git,
	}
	if t.Mode == "" {
		t.Mode = string(resolved.Environment.Mode)
	}
	adapter, plane, err := s.selectAdapter(ctx, t)
	if err != nil {
		return "", nil, err
	}

	entries, err := adapter.History(ctx, delivery.ManifestSet{Project: t.Project, Environment: t.Environment})
	if err != nil {
		return "", nil, err
	}
	if len(entries) == 0 {
		return "", nil, promote.NothingDeployed(t.Project, t.Environment)
	}
	if plane.Recorded == nil {
		return "", nil, fmt.Errorf("api: delivery mode %q keeps no rendered history kelson can read, so the images %s/%s runs cannot be read back; promote from an environment delivered in direct mode, or set the pin by hand",
			adapter.Name(), t.Project, t.Environment)
	}

	manifests, err := plane.Recorded.Revision(ctx, entries[0].Revision)
	if err != nil {
		return "", nil, err
	}
	images, err := promote.Deployed(manifests)
	if err != nil {
		return "", nil, err
	}
	return entries[0].Revision, images, nil
}

// pinnedDocuments applies every pin the plan writes to the environment's
// document, in plan order, and returns the whole document set with that one
// document replaced.
//
// The document is located by decoding, not by map key: the store's key is
// whatever the client named the document when it stored it, and a promotion
// that wrote to the wrong file because the two disagreed would be the worst
// possible way to find that out.
func pinnedDocuments(docs serverstate.Documents, environment string, changes []promote.Change) (serverstate.Documents, error) {
	pins := promote.Pinned(changes)
	if len(pins) == 0 {
		return copyDocuments(docs), nil
	}
	key, doc, err := environmentDocument(docs, environment)
	if err != nil {
		return serverstate.Documents{}, err
	}
	for _, c := range pins {
		if doc, err = promote.Pin(doc, environment, c.Component, c.To); err != nil {
			return serverstate.Documents{}, err
		}
	}
	out := copyDocuments(docs)
	out.Environments[key] = doc
	return out, nil
}

// environmentDocument finds the stored document declaring one environment.
func environmentDocument(docs serverstate.Documents, environment string) (string, []byte, error) {
	keys := make([]string, 0, len(docs.Environments))
	for key := range docs.Environments {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		parsed, errs := model.DecodeDocuments(docs.Environments[key])
		if len(errs) > 0 {
			continue
		}
		for _, d := range parsed {
			if env, ok := d.(*model.Environment); ok && env.Metadata.Name == environment {
				return key, docs.Environments[key], nil
			}
		}
	}
	return "", nil, fmt.Errorf("api: no stored document declares environment %q, so there is nothing to write the pin into", environment)
}

func copyDocuments(docs serverstate.Documents) serverstate.Documents {
	out := serverstate.Documents{Project: docs.Project, Environments: make(map[string][]byte, len(docs.Environments))}
	for k, v := range docs.Environments {
		out.Environments[k] = v
	}
	return out
}

// promotionDiff fills in the response's diff of the target environment, and
// returns the findings that must stop the write.
//
// The after side must validate and render: a promotion whose result the server
// would refuse to deploy is not a promotion, and storing it would leave the
// store holding a spec no plane can use. The before side is allowed to fail —
// an environment with no resolvable image renders nothing until the pin gives
// it one — and then the diff reads as additions, which is what it is.
func (s *Server) promotionDiff(ctx context.Context, res *kelsonv1alpha1.PromoteResponse, before, after serverstate.Documents, environment string, profile clusterprofile.ClusterProfile) []*kelsonv1alpha1.Error {
	spec, err := decodeSpec(after.Project, after.Environments)
	if err != nil {
		return wireErrors(err)
	}
	if errs := model.ValidateSet(spec.project, spec.environments...); len(errs) > 0 {
		return wireErrors(errs)
	}

	cur, err := s.renderWith(ctx, documentsRef(after), environment, "", profile)
	if err != nil {
		return wireErrors(err)
	}
	var prev []renderer.Manifest
	if old, err := s.renderWith(ctx, documentsRef(before), environment, "", profile); err == nil {
		prev = old.manifests
	}

	d, err := diff.Between(cur.project.Metadata.Name, environment, prev, cur.manifests, nil)
	if err != nil {
		return wireErrors(err)
	}
	encoded, err := diff.EncodeJSON(d)
	if err != nil {
		return wireErrors(err)
	}
	res.DiffJson = encoded
	res.ExitSemantics = diffExitCode(d)
	return nil
}

func documentsRef(docs serverstate.Documents) *kelsonv1alpha1.SpecRef {
	return &kelsonv1alpha1.SpecRef{Spec: &kelsonv1alpha1.SpecRef_Documents{
		Documents: &kelsonv1alpha1.SpecDocuments{Project: docs.Project, Environments: docs.Environments},
	}}
}

// wirePromoted projects the plan onto the wire, every component included —
// pinned, unchanged and skipped alike. A caller must be able to see that a
// component was considered and why nothing happened to it.
func wirePromoted(changes []promote.Change) []*kelsonv1alpha1.PromotedComponent {
	out := make([]*kelsonv1alpha1.PromotedComponent, 0, len(changes))
	for _, c := range changes {
		out = append(out, &kelsonv1alpha1.PromotedComponent{
			Component: c.Component,
			FromImage: c.From,
			ToImage:   c.To,
			Status:    promotionStatus(c.Status),
			Code:      string(c.Code),
			Reason:    c.Reason,
		})
	}
	return out
}

func promotionStatus(status promote.Status) kelsonv1alpha1.PromotionStatus {
	switch status {
	case promote.StatusPinned:
		return kelsonv1alpha1.PromotionStatus_PROMOTION_STATUS_PINNED
	case promote.StatusUnchanged:
		return kelsonv1alpha1.PromotionStatus_PROMOTION_STATUS_UNCHANGED
	case promote.StatusSkipped:
		return kelsonv1alpha1.PromotionStatus_PROMOTION_STATUS_SKIPPED
	default:
		return kelsonv1alpha1.PromotionStatus_PROMOTION_STATUS_UNSPECIFIED
	}
}
