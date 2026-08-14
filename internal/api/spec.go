package api

import (
	"context"
	"fmt"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/clusterprofile"
	"github.com/dafrie/kelson/internal/controlstore"
	"github.com/dafrie/kelson/internal/model"
	"github.com/dafrie/kelson/internal/renderer"
)

// PutSpec validates the documents and stores them.
//
// Validation happens BEFORE the store is touched, and an invalid spec is
// reported in PutSpecResponse.errors with the RPC itself succeeding: a spec
// that does not validate is an answer to "would you accept this?", not a
// transport failure. Nothing is stored in that case, so a rejected write never
// leaves the store holding a spec no plane can render.
func (s *Server) PutSpec(ctx context.Context, req *connect.Request[kelsonv1alpha1.PutSpecRequest]) (*connect.Response[kelsonv1alpha1.PutSpecResponse], error) {
	msg := req.Msg
	auditDryRun(ctx, msg.GetDryRun())
	auditIdempotencyKey(ctx, msg.GetIdempotencyKey())

	docs := msg.GetDocuments()
	if docs == nil {
		return nil, fail(connect.CodeInvalidArgument, fmt.Errorf("api: PutSpec needs documents to store"))
	}

	spec, err := decodeSpec(docs.GetProject(), docs.GetEnvironments())
	if err != nil {
		if wire := specFindings(err); len(wire) > 0 {
			return connect.NewResponse(&kelsonv1alpha1.PutSpecResponse{Errors: wire}), nil
		}
		return nil, failRequest(err)
	}
	// The project name lives inside the YAML, which is why the scope table
	// cannot read a target out of a PutSpec request (scope.go). The audit
	// record can have one, because by here the document has been decoded.
	auditTarget(ctx, spec.project.Metadata.Name, "")

	if errs := model.ValidateSet(spec.project, spec.environments...); len(errs) > 0 {
		return connect.NewResponse(&kelsonv1alpha1.PutSpecResponse{Errors: wireErrors(errs)}), nil
	}

	// The dry-run ladder stops at RENDER for a spec store: SERVER adds no
	// fidelity here, because storing a spec applies nothing to the cluster for
	// the API server to dry-run. Both therefore validate, render and store
	// nothing (ADR-0013 §2).
	if dry := msg.GetDryRun(); dry == kelsonv1alpha1.DryRun_DRY_RUN_RENDER || dry == kelsonv1alpha1.DryRun_DRY_RUN_SERVER {
		return connect.NewResponse(&kelsonv1alpha1.PutSpecResponse{
			Spec:   &kelsonv1alpha1.Spec{Project: spec.project.Metadata.Name, Documents: docs, Environments: environmentNames(spec)},
			Errors: renderEveryEnvironment(spec),
		}), nil
	}

	if s.specs == nil {
		return nil, unimplemented("the spec store")
	}

	// Agent policy (ADR-0025), and the reason it is on a *spec* write at all:
	// the desired state of a propose-only environment includes the line that
	// says it is propose-only. An agent that could rewrite production's
	// document could set `agents: allow` and then deploy, which would make
	// every other refusal in this file advisory. Every stored environment is
	// checked against the policy the store holds for it *now*, never against
	// the one in the incoming documents — see guardStored for why the check
	// cannot be narrowed to the environments the request names.
	if err := s.guardStored(ctx, model.AgentOpSpecWrite, spec.project.Metadata.Name); err != nil {
		return nil, err
	}

	stored, err := s.specs.Put(ctx, spec.project.Metadata.Name, controlstore.Documents{
		Project:      docs.GetProject(),
		Environments: docs.GetEnvironments(),
	}, controlstore.PutOptions{
		ExpectedVersion: msg.GetVersion(),
		Force:           msg.GetForce(),
		IdempotencyKey:  msg.GetIdempotencyKey(),
	})
	if err != nil {
		return nil, failRequest(err)
	}
	auditChange(ctx, controlstore.AuditChange{Revision: stored.Version})
	return connect.NewResponse(&kelsonv1alpha1.PutSpecResponse{Spec: wireSpec(stored, true)}), nil
}

// renderEveryEnvironment is the extra fidelity dry_run=RENDER buys: a spec that
// validates can still fail to render (an unresolved image, routing with no
// Gateway API), and a caller asking for a dry run wants to know that before it
// stores. The zero profile is used deliberately — PutSpec carries no profile
// reference, and rendering against "nothing detected" is the strictest honest
// answer available without one.
func renderEveryEnvironment(spec decoded) []*kelsonv1alpha1.Error {
	var out []*kelsonv1alpha1.Error
	for _, env := range spec.environments {
		resolved, errs := model.Resolve(spec.project, env)
		if len(errs) > 0 {
			out = append(out, wireErrors(errs)...)
			continue
		}
		if err := checkOverlays(resolved); err != nil {
			out = append(out, wireErrors(err)...)
			continue
		}
		if _, err := renderer.Render(resolved, clusterprofile.ClusterProfile{}, nil); err != nil {
			out = append(out, wireErrors(err)...)
		}
	}
	return out
}

// GetSpec returns one stored project, documents included.
func (s *Server) GetSpec(ctx context.Context, req *connect.Request[kelsonv1alpha1.GetSpecRequest]) (*connect.Response[kelsonv1alpha1.GetSpecResponse], error) {
	if s.specs == nil {
		return nil, unimplemented("the spec store")
	}
	stored, err := s.specs.Get(ctx, req.Msg.GetProject())
	if err != nil {
		return nil, failRequest(err)
	}
	return connect.NewResponse(&kelsonv1alpha1.GetSpecResponse{Spec: wireSpec(stored, true)}), nil
}

// ListSpecs returns every stored project without its documents — the schema
// says so, and a list of every spec's full text is not a listing.
func (s *Server) ListSpecs(ctx context.Context, _ *connect.Request[kelsonv1alpha1.ListSpecsRequest]) (*connect.Response[kelsonv1alpha1.ListSpecsResponse], error) {
	if s.specs == nil {
		return nil, unimplemented("the spec store")
	}
	stored, err := s.specs.List(ctx)
	if err != nil {
		return nil, failRequest(err)
	}
	specs := make([]*kelsonv1alpha1.Spec, 0, len(stored))
	for _, st := range stored {
		specs = append(specs, wireSpec(st, false))
	}
	return connect.NewResponse(&kelsonv1alpha1.ListSpecsResponse{Specs: specs}), nil
}

// DeleteSpec removes a stored project under the same optimistic-concurrency
// contract as PutSpec.
func (s *Server) DeleteSpec(ctx context.Context, req *connect.Request[kelsonv1alpha1.DeleteSpecRequest]) (*connect.Response[kelsonv1alpha1.DeleteSpecResponse], error) {
	auditIdempotencyKey(ctx, req.Msg.GetIdempotencyKey())
	if s.specs == nil {
		return nil, unimplemented("the spec store")
	}
	// Deleting the project deletes every environment's desired state at once,
	// so every stored environment's policy gets a say.
	if err := s.guardStored(ctx, model.AgentOpSpecDelete, req.Msg.GetProject()); err != nil {
		return nil, err
	}
	err := s.specs.Delete(ctx, req.Msg.GetProject(), controlstore.DeleteOptions{
		ExpectedVersion: req.Msg.GetVersion(),
		Force:           req.Msg.GetForce(),
		IdempotencyKey:  req.Msg.GetIdempotencyKey(),
	})
	if err != nil {
		return nil, failRequest(err)
	}
	return connect.NewResponse(&kelsonv1alpha1.DeleteSpecResponse{}), nil
}

func wireSpec(stored controlstore.Stored, withDocuments bool) *kelsonv1alpha1.Spec {
	spec := &kelsonv1alpha1.Spec{
		Project:      stored.Project,
		Version:      stored.Version,
		Environments: stored.Environments,
	}
	if withDocuments {
		spec.Documents = &kelsonv1alpha1.SpecDocuments{
			Project:      stored.Documents.Project,
			Environments: stored.Documents.Environments,
		}
	}
	return spec
}

func environmentNames(spec decoded) []string {
	names := make([]string, 0, len(spec.environments))
	for _, e := range spec.environments {
		names = append(names, e.Metadata.Name)
	}
	return names
}
