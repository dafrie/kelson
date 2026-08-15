package api

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/clusterprofile"
	"github.com/dafrie/kelson/internal/controlstore"
	"github.com/dafrie/kelson/internal/diff"
	"github.com/dafrie/kelson/internal/model"
	"github.com/dafrie/kelson/internal/promote"
	"github.com/dafrie/kelson/internal/renderer"
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
// # Where the deployed images come from now
//
// The first of the three reads is the one the spine changed: the images come
// from the source `Environment.status` — the revision it is serving and the
// images that revision resolved to (ADR-0028 decision 4) — where they used to
// come from the rendered-history store ADR-0027 decision 7 deleted.
// [deployedImages] does that read and [attributeImages] says which component
// each image belongs to. Everything else in this file — the plan, the splice,
// the promotion diff, the agent-policy guard — is unchanged.
//
// The write is unchanged too and is already what ADR-0028 decision 6 asks for:
// the pins are spliced into the target environment's document and stored, and
// the store applies that document as an `Environment` custom resource under
// field manager kelson-server. What is new beside it is the
// `kelson.dev/promoted-from` stamp.
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
	// Promote names two environments and the scope table records the first;
	// the one that changed is the destination, so the record is corrected here
	// (issue #78).
	auditTarget(ctx, msg.GetProject(), msg.GetToEnvironment())
	auditDryRun(ctx, msg.GetDryRun())
	auditIdempotencyKey(ctx, msg.GetIdempotencyKey())

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

	revision, deployed, err := s.deployedImages(ctx, spec.project, source)
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
	auditChange(ctx, controlstore.AuditChange{From: revision})
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
	written, err := s.specs.Put(ctx, stored.Project, after, controlstore.PutOptions{
		ExpectedVersion: version,
		IdempotencyKey:  msg.GetIdempotencyKey(),
	})
	if err != nil {
		return nil, failRequest(err)
	}
	res.Version = written.Version
	auditChange(ctx, controlstore.AuditChange{Revision: written.Version, From: revision})

	// The provenance stamp (ADR-0028 decision 6): where this environment's
	// images came from, as a `kubectl get` rather than an archaeology
	// exercise. It goes *after* the write and not into it, because the write is
	// a server-side apply of the document set — an apply that also carried the
	// annotation would own it, and the next apply, which mentions no
	// annotations, would silently delete it (controlstore.AnnotationManager
	// says the same thing from the other side).
	//
	// A failed stamp does not fail the promotion. The pins are written and the
	// environment will deploy them; losing the note about where they came from
	// is a smaller harm than reporting a promotion that happened as one that
	// did not.
	if s.environments != nil && len(promote.Pinned(changes)) > 0 {
		stamp := fmt.Sprintf("%s@%s", source.Metadata.Name, revision)
		if _, err := s.environments.Annotate(ctx, stored.Project, targetEnv.Metadata.Name,
			map[string]string{annotationPromotedFrom: stamp}); err != nil {
			s.authz.logger.Warn("promotion could not stamp its provenance",
				"project", stored.Project, "environment", targetEnv.Metadata.Name,
				"annotation", annotationPromotedFrom, "error", err)
		}
	}
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

// deployedImages reads what the source environment's latest revision runs, from
// `Environment.status` (ADR-0028 decision 4).
//
// The revision is the one the environment is *serving* rather than the last one
// it published — the two differ exactly while a rollback is pinned, and
// ADR-0016 decision 2 defines a promotion as moving what ran.
//
// Reading the source environment's *spec* instead would be the wrong answer and
// is deliberately not done: a spec says what should be deployed and a revision
// says what was, and a promotion that quietly moved an intention would promote
// an image the source has not proven.
func (s *Server) deployedImages(ctx context.Context, project *model.Project, source *model.Environment) (string, map[string]string, error) {
	if s.environments == nil {
		return "", nil, unimplemented("the Environment status reader")
	}
	st, err := s.environments.Get(ctx, project.Metadata.Name, source.Metadata.Name)
	if err != nil {
		return "", nil, err
	}
	running, ok := st.Running()
	if !ok {
		return "", nil, promote.NothingDeployed(project.Metadata.Name, source.Metadata.Name)
	}
	resolved, errs := model.Resolve(project, source)
	if len(errs) > 0 {
		return "", nil, errs
	}
	return running.Revision, attributeImages(resolved, running), nil
}

// attributeImages says which component each image of a recorded revision
// belongs to.
//
// # It is read, not derived
//
// `status.history[].componentImages` records the component name beside each
// image, written by the controller at the moment the revision was rendered,
// where the name is a fact rather than an inference (ADR-0028 decision 4). So
// the ordinary answer is a copy: no matching, no ambiguity, and a component
// whose image the revision genuinely did not carry stays absent — promote.Plan
// turns that into a skip with a reason (promote/not-in-revision), and skipping
// is never silent.
//
// # The fallback, and why it is only a fallback
//
// An entry recorded before the controller attributed images has the flat
// `images` list and nothing else: image references "in component order" with the
// imageless components left out, so position alone cannot name a component — a
// component added or removed since that revision shifts everything after it,
// and a promotion that mis-attributed an image would pin production to the
// wrong build, the one failure mode this whole path exists to prevent. For
// those entries the correlation is the image *repository*, which survives a tag
// change and identifies "the same thing, a different build"; the positional
// pairing is used only where it agrees with the repository, and a component
// whose repository matches no image, or matches more than one, is left out.
//
// The fallback goes away with the deprecated field, once no history mirror
// still holds an entry written before the upgrade.
func attributeImages(resolved *model.Resolved, revision controlstore.Revision) map[string]string {
	if len(revision.ComponentImages) > 0 {
		out := make(map[string]string, len(revision.ComponentImages))
		for _, ci := range revision.ComponentImages {
			if ci.Component != "" && ci.Image != "" {
				out[ci.Component] = ci.Image
			}
		}
		return out
	}
	return attributeImagesByRepository(resolved, revision.Images)
}

// attributeImagesByRepository is the pre-attribution fallback [attributeImages]
// documents.
func attributeImagesByRepository(resolved *model.Resolved, images []string) map[string]string {
	type candidate struct {
		name string
		repo string
	}
	components := make([]candidate, 0, len(resolved.Components))
	for _, c := range resolved.Components {
		if c.Image == "" {
			continue
		}
		components = append(components, candidate{name: c.Name, repo: imageRepository(c.Image)})
	}

	out := make(map[string]string, len(components))
	taken := make([]bool, len(images))
	// The agreeing positional pass: same length, same repository at the same
	// index, which is the ordinary case of a spec whose components have not
	// changed since the revision was published.
	if len(components) == len(images) {
		for i, c := range components {
			if imageRepository(images[i]) == c.repo {
				out[c.name] = images[i]
				taken[i] = true
			}
		}
	}
	for _, c := range components {
		if _, done := out[c.name]; done {
			continue
		}
		match, count := "", 0
		for i, image := range images {
			if taken[i] || imageRepository(image) != c.repo {
				continue
			}
			match, count = image, count+1
		}
		if count == 1 {
			out[c.name] = match
		}
	}
	return out
}

// imageRepository strips the tag or digest from an image reference, leaving
// what identifies the artifact rather than the build. A colon before the last
// slash is a port, not a tag separator, which is why this cannot be a
// strings.Split.
func imageRepository(image string) string {
	if at := strings.Index(image, "@"); at >= 0 {
		image = image[:at]
	}
	if colon := strings.LastIndex(image, ":"); colon > strings.LastIndex(image, "/") {
		image = image[:colon]
	}
	return image
}

// pinnedDocuments applies every pin the plan writes to the environment's
// document, in plan order, and returns the whole document set with that one
// document replaced.
//
// The document is located by decoding, not by map key: the store's key is
// whatever the client named the document when it stored it, and a promotion
// that wrote to the wrong file because the two disagreed would be the worst
// possible way to find that out.
func pinnedDocuments(docs controlstore.Documents, environment string, changes []promote.Change) (controlstore.Documents, error) {
	pins := promote.Pinned(changes)
	if len(pins) == 0 {
		return copyDocuments(docs), nil
	}
	key, doc, err := environmentDocument(docs, environment)
	if err != nil {
		return controlstore.Documents{}, err
	}
	for _, c := range pins {
		if doc, err = promote.Pin(doc, environment, c.Component, c.To); err != nil {
			return controlstore.Documents{}, err
		}
	}
	out := copyDocuments(docs)
	out.Environments[key] = doc
	return out, nil
}

// environmentDocument finds the stored document declaring one environment.
func environmentDocument(docs controlstore.Documents, environment string) (string, []byte, error) {
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

func copyDocuments(docs controlstore.Documents) controlstore.Documents {
	out := controlstore.Documents{Project: docs.Project, Environments: make(map[string][]byte, len(docs.Environments))}
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
func (s *Server) promotionDiff(ctx context.Context, res *kelsonv1alpha1.PromoteResponse, before, after controlstore.Documents, environment string, profile clusterprofile.ClusterProfile) []*kelsonv1alpha1.Error {
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

func documentsRef(docs controlstore.Documents) *kelsonv1alpha1.SpecRef {
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
