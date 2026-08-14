package api

import (
	"context"
	"fmt"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/delivery/statemachine"
	"github.com/dafrie/kelson/internal/diff"
	"github.com/dafrie/kelson/internal/model"
	"github.com/dafrie/kelson/internal/observation"
)

// Deploy renders the spec and answers with what it would do. The rungs that
// change the cluster are gated (issue #224); the rungs that do not are intact.
//
// # Which half of this RPC survives, and why that split is the honest one
//
// [ADR-0028](docs/adr/0028-delivery-spine.md) deleted the delivery adapters, so
// there is nothing left to call Apply on. What it did not touch is everything
// upstream of the apply: dry_run=RENDER is the offline rung — the manifests ARE
// the answer and no adapter was ever built — and dry_run=SERVER is the API
// server's own verdict on the rendered set through internal/delivery/dryrun,
// which is a cluster capability and not an adapter. Both keep working, exactly
// as before, and an agent whose mutations default to dry-run (ADR-0025, the MCP
// surface) notices nothing.
//
// The apply rung answers `CodeUnimplemented` carrying a
// `delivery/not-implemented` detail. It answers it AFTER the Proposed event,
// deliberately: a caller streaming this RPC learns what would have been
// deployed — the project, the environment, the resource count — and then learns
// that kelson cannot deploy it, which is strictly more than a bare refusal and
// is the shape a dry run already has.
//
// The event order for the rungs that still run is unchanged and never varies:
// Proposed (what is about to happen), then the rung's own answer.
func (s *Server) Deploy(ctx context.Context, req *connect.Request[kelsonv1alpha1.DeployRequest], stream *connect.ServerStream[kelsonv1alpha1.DeployResponse]) error {
	msg := req.Msg
	// The audit record is opened by the interceptor and enriched here, where
	// what actually happened is known (issue #78, audit.go).
	auditDryRun(ctx, msg.GetDryRun())
	auditIdempotencyKey(ctx, msg.GetIdempotencyKey())

	out, err := s.renderSpec(ctx, msg.GetSpec(), msg.GetEnvironment(), msg.GetImage(), msg.GetProfile())
	if err != nil {
		return failRequest(err)
	}
	set, err := manifestSet(out)
	if err != nil {
		return fail(connect.CodeInternal, err)
	}
	dryRun := msg.GetDryRun()
	auditChange(ctx, changeFromSet(set))

	// Agent policy (ADR-0025), before the first event and against the *stored*
	// environment this render resolved to — an inline document naming
	// shop/production is asking about the shop/production kelson holds. A dry
	// run is exempt on purpose: rendering and previewing change nothing, and
	// they are precisely what a propose-only agent is told to send instead.
	//
	// It still runs ahead of the gate below. A propose-only agent must be told
	// it may not deploy before it is told kelson cannot: the policy answer is
	// about them and is stable, the gate is about kelson and is temporary.
	var guard agentGuard
	if persists(dryRun) {
		if guard, err = s.guard(ctx, model.AgentOpDeploy, set.Project, set.Environment); err != nil {
			return err
		}
		if err := guard.blastRadius(out.resolved); err != nil {
			return err
		}
	}

	proposed := &kelsonv1alpha1.DeployResponse_Proposed{
		Project:     set.Project,
		Environment: set.Environment,
		Resources:   int32(len(set.Manifests)), //nolint:gosec // a rendered set is orders of magnitude below int32
	}
	if dryRun == kelsonv1alpha1.DryRun_DRY_RUN_RENDER {
		// RENDER is the offline rung: the manifests ARE the answer, so they
		// ride the Proposed event and the stream ends.
		if proposed.Manifests, err = wireManifests(out.manifests); err != nil {
			return fail(connect.CodeInternal, err)
		}
	}
	if err := stream.Send(&kelsonv1alpha1.DeployResponse{
		Event: &kelsonv1alpha1.DeployResponse_Proposed_{Proposed: proposed},
	}); err != nil {
		return err
	}
	if dryRun == kelsonv1alpha1.DryRun_DRY_RUN_RENDER {
		return nil
	}
	if dryRun == kelsonv1alpha1.DryRun_DRY_RUN_SERVER {
		return s.previewDeploy(ctx, out, set, stream)
	}

	// `require: [dry-run]` is satisfied by kelson running one here, on the set
	// that would be applied — never by a claim on the request that one was run
	// elsewhere (ADR-0025 §5). It still runs ahead of the gate for the reason
	// the doc comment gives: what an agent may do is a stable answer about
	// them, and what kelson can do is a temporary one about kelson.
	if err := s.requireDryRun(ctx, guard, out.profile, set); err != nil {
		return err
	}
	return fail(connect.CodeUnimplemented, deployUnavailable())
}

// deployUnavailable is the refusal every deleted apply path in this package
// shares, so the message a caller reads does not depend on which RPC they
// happened to call.
func deployUnavailable() error {
	return delivery.NotImplemented("deploy",
		"kelson cannot apply a rendered set: the direct applier and the git writer were deleted with "+
			"the old delivery machinery, and the controller that replaces them does not publish yet. "+
			"dry_run=RENDER and dry_run=SERVER are unaffected and still answer",
		"#224")
}

// previewDeploy is the dry_run=SERVER rung: the API server's own verdict on the
// rendered set, reported as a Transition and then done. A preview that finds a
// blocker reports Rejected — the same finding `kelson diff` exits 3 on — so a
// caller reading the stream learns the deploy would not land without having
// attempted it.
func (s *Server) previewDeploy(ctx context.Context, out *rendered, set delivery.ManifestSet, stream *connect.ServerStream[kelsonv1alpha1.DeployResponse]) error {
	if s.preview == nil {
		return unimplemented("the server-side dry-run engine")
	}
	engine, err := s.preview(ctx, out.profile)
	if err != nil {
		return failRequest(unavailable("api: building the server-side dry-run engine: %w", err))
	}
	d, err := engine.Preview(ctx, set)
	if err != nil {
		return failRequest(err)
	}
	// A server-side dry run computed a real comparison, so the record carries
	// the diff's own numbers rather than the applied set's shape.
	auditChange(ctx, changeFromDiff(d))
	auditDryRunResult(ctx, previewSummary(d))

	transition := &kelsonv1alpha1.DeployResponse_Transition{
		Phase:  string(delivery.PhaseProposed),
		Answer: string(statemachine.AnswerWaiting),
		Cause: &kelsonv1alpha1.DeployResponse_Cause{
			Component: "kelson",
			Reason:    "server-dry-run",
			Message:   previewSummary(d),
		},
	}
	if diffExitCode(d) == exitSemanticsBlocked {
		transition.Phase = string(delivery.PhaseRejected)
		transition.Answer = string(statemachine.AnswerRejected)
		transition.Cause.Reason = "server-dry-run-blocked"
	}
	return stream.Send(&kelsonv1alpha1.DeployResponse{
		Event: &kelsonv1alpha1.DeployResponse_Transition_{Transition: transition},
	})
}

func previewSummary(d *diff.Diff) string {
	return fmt.Sprintf("%d added, %d modified, %d removed (max risk %s)",
		d.Summary.Added, d.Summary.Modified, d.Summary.Removed, d.Summary.MaxRisk)
}

// Status reports the observation plane's verdict for each workload the
// rendered spec declares.
//
// It used to report two things and say that neither substituted for the other
// (issue #53): the delivery phase said whether the change ARRIVED, the verdicts
// said whether it WORKS. The phase came from an adapter, and ADR-0028 deleted
// the adapters; ADR-0027 decision 6 says where it comes back from — this handler
// reads `Environment.status` — and issue #224 is when.
//
// So the response carries an empty phase and an empty revision rather than a
// guess. Empty is a value a client can branch on and "Healthy" would not be;
// the UI reads the same field it always did and finds nothing in it, which is
// the truth. The verdicts, the namespace and the causes behind each verdict are
// unchanged, and they are the half a caller looks at when something is broken.
func (s *Server) Status(ctx context.Context, req *connect.Request[kelsonv1alpha1.StatusRequest]) (*connect.Response[kelsonv1alpha1.StatusResponse], error) {
	msg := req.Msg
	out, err := s.renderSpec(ctx, msg.GetSpec(), msg.GetEnvironment(), msg.GetImage(), msg.GetProfile())
	if err != nil {
		return nil, failRequest(err)
	}
	set, err := manifestSet(out)
	if err != nil {
		return nil, fail(connect.CodeInternal, err)
	}
	t := target(out)
	plane, err := s.plane(ctx, t)
	if err != nil {
		return nil, failRequest(err)
	}

	verdicts, err := workloadVerdicts(ctx, plane, set, t.Namespace)
	if err != nil {
		return nil, failRequest(err)
	}
	return connect.NewResponse(&kelsonv1alpha1.StatusResponse{
		Verdicts: verdicts,
		// The namespace the target resolved to, so a client addressing this
		// environment's workloads reads it rather than reconstructing the
		// model's default and missing a spec.namespace override (#161).
		Namespace: t.Namespace,
		Cause: "the delivery phase is not reported: the adapters that answered it were deleted with the " +
			"old delivery machinery (ADR-0028) and it returns with issue #224, read from Environment.status",
	}), nil
}

// workloadVerdicts evaluates the observation verdict for every resource in the
// rendered set that carries a health signal. The set IS the correlation: these
// are the resources kelson rendered for this project and environment. No probe
// means no verdicts, which is reported as an empty list rather than as "nothing
// is failing".
//
// Two kinds qualify today. Deployments carry the workload verdict. And under
// the externalSecrets backend, ExternalSecrets carry a sync verdict — which is
// listed first, because a Secret that never synced is the reason the pods below
// it are stuck, and the cause belongs above the symptom (issue #80, ADR-0020).
// Every other kind carries no signal the probe can classify, and reporting
// "unknown" for them as if it were a verdict would be worse than saying nothing.
func workloadVerdicts(ctx context.Context, plane *Plane, set delivery.ManifestSet, namespace string) ([]*kelsonv1alpha1.WorkloadVerdict, error) {
	verdicts, err := observeWorkloads(ctx, plane, set, namespace)
	if err != nil {
		return nil, err
	}
	out := make([]*kelsonv1alpha1.WorkloadVerdict, 0, len(verdicts))
	for _, verdict := range verdicts {
		out = append(out, &kelsonv1alpha1.WorkloadVerdict{
			Resource:    verdict.Resource,
			Code:        string(verdict.Code),
			Healthy:     verdict.Healthy,
			Degraded:    !verdict.Healthy && !verdict.Stuck && observation.IsFailure(verdict.Code),
			Message:     verdict.String(),
			Remediation: verdict.Remediation,
		})
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// observeWorkloads is workloadVerdicts before the wire projection: the same
// evaluation, returning the observation plane's own values. Explain needs those
// rather than the wire ones — a cause is built from the verdict's containers
// and their captured output, which the wire message deliberately does not carry
// (issue #77) — so the traversal lives here once and both callers share it.
func observeWorkloads(ctx context.Context, plane *Plane, set delivery.ManifestSet, namespace string) ([]observation.Verdict, error) {
	if plane.Health == nil {
		return nil, nil
	}
	nsOf := func(m delivery.Manifest) string {
		if m.Namespace != "" {
			return m.Namespace
		}
		return namespace
	}
	var verdicts []observation.Verdict
	// Sync verdicts are an optional capability of the configured Evaluator: a
	// health source that is only a workload probe keeps working and simply
	// reports none, rather than forcing every implementation to grow a method
	// for a backend it may never see.
	if sync, ok := plane.Health.(observation.SecretSyncEvaluator); ok {
		for _, m := range set.Manifests {
			if m.Kind != "ExternalSecret" {
				continue
			}
			verdict, err := sync.EvaluateSecretSync(ctx, nsOf(m), m.Name)
			if err != nil {
				return nil, err
			}
			verdicts = append(verdicts, verdict)
		}
	}
	for _, m := range set.Manifests {
		if m.Kind != "Deployment" {
			continue
		}
		verdict, err := plane.Health.Evaluate(ctx, nsOf(m), m.Name)
		if err != nil {
			return nil, err
		}
		verdicts = append(verdicts, verdict)
	}
	return verdicts, nil
}

// Rollback is gated (issue #224).
//
// Every one of its three parts is deleted. The recorded revisions came from the
// history store (ADR-0027 decision 7 deletes it), the irreversibility preview
// came from internal/delivery/rollback, and the replay was an adapter's
// Rollback. [ADR-0028](docs/adr/0028-delivery-spine.md) decision 5 replaces all
// three with a pointer move — repoint the environment's OCIRepository at an
// immutable tag that already exists, via a `kelson.dev/rollback-to` annotation
// that also suspends re-render.
//
// Even dry_run=RENDER is refused, unlike Deploy's. The preview rung of a
// rollback is not a render: it is the comparison of two recorded revisions, and
// answering it with "no findings" because there is nothing to compare would be
// the precise failure the preview exists to prevent — a rollback that looked
// safe because kelson could not look.
func (s *Server) Rollback(ctx context.Context, req *connect.Request[kelsonv1alpha1.RollbackRequest], _ *connect.ServerStream[kelsonv1alpha1.RollbackResponse]) error {
	msg := req.Msg
	auditDryRun(ctx, msg.GetDryRun())

	// Agent policy answers first, and the ordering is the same one Deploy
	// states: `forbid: [rollback]` and `propose-only` are stable statements
	// about this principal, and the gate is a temporary one about kelson. An
	// agent told "not implemented" would learn nothing about the rule that will
	// still refuse it when the capability returns.
	//
	// It needs the project and the environment, which come from the spec — the
	// only thing this handler still resolves.
	out, err := s.renderSpec(ctx, msg.GetSpec(), msg.GetEnvironment(), "", msg.GetProfile())
	if err != nil {
		return failRequest(err)
	}
	if msg.GetDryRun() != kelsonv1alpha1.DryRun_DRY_RUN_RENDER {
		t := target(out)
		if _, err := s.guard(ctx, model.AgentOpRollback, t.Project, t.Environment); err != nil {
			return err
		}
	}

	return fail(connect.CodeUnimplemented, delivery.NotImplemented("rollback",
		"kelson cannot roll back: the recorded rendered history and the irreversibility preview it is "+
			"computed from were deleted with the old delivery machinery, and the annotation-driven "+
			"rollback that replaces them is not built",
		"#224"))
}

// History is gated (issue #224).
//
// [ADR-0028](docs/adr/0028-delivery-spine.md) decision 4 moves the record out of
// kelson entirely: the registry holds every artifact ever published for an
// environment, immutably, and that IS the history — nothing stores rendered
// manifests a second time. `Environment.status.history[]` mirrors the most
// recent 20 entries for humans and for this RPC, and anything older is a
// registry query.
//
// Neither the mirror nor the publisher exists yet, so this answers with the
// refusal rather than with an empty list. An empty history and an unavailable
// history are different facts, and a caller that cannot tell them apart would
// conclude nothing was ever deployed.
func (s *Server) History(ctx context.Context, _ *connect.Request[kelsonv1alpha1.HistoryRequest]) (*connect.Response[kelsonv1alpha1.HistoryResponse], error) {
	_ = ctx
	return nil, fail(connect.CodeUnimplemented, delivery.NotImplemented("history",
		"kelson cannot list an environment's revisions: the rendered-history store was deleted with the "+
			"old delivery machinery, and the registry tag list and Environment.status mirror that replace "+
			"it are not built",
		"#224"))
}

// plane builds the cluster-reading plane for this request, mirroring
// cmd/kelson's connectPlane. It replaces selectAdapter, which additionally
// resolved a delivery mode to an adapter — a step with nothing left to resolve
// (ADR-0028 decision 9).
func (s *Server) plane(ctx context.Context, t Target) (*Plane, error) {
	if s.delivery == nil {
		return nil, unimplemented("the observation plane")
	}
	plane, err := s.delivery(ctx, t)
	if err != nil {
		return nil, unavailable("api: building the observation plane: %w", err)
	}
	return plane, nil
}
