package api

import (
	"context"
	"fmt"
	"time"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/delivery/rollback"
	"github.com/dafrie/kelson/internal/delivery/statemachine"
	"github.com/dafrie/kelson/internal/diff"
	"github.com/dafrie/kelson/internal/observation"
)

// Deploy renders the spec, hands the manifests to the environment's adapter and
// streams the deployment until it settles.
//
// The event order is the deployment's own story and never varies: Proposed
// (what is about to happen), Committed (the apply landed, revision assigned),
// one Transition per state-machine phase change, and exactly one Settled last.
// The transitions are the engine's, not this handler's (issue #37) — the CLI
// prints the same ones, so `kelson deploy` and any UI cannot disagree about
// what a phase means.
//
// A deployment that settles unhealthy completes the stream cleanly with the
// error on the Settled event. That is statemachine.Run's contract: "not healthy"
// is an answer about the deployment, not a failure of the RPC carrying it.
func (s *Server) Deploy(ctx context.Context, req *connect.Request[kelsonv1alpha1.DeployRequest], stream *connect.ServerStream[kelsonv1alpha1.DeployResponse]) error {
	msg := req.Msg
	out, err := s.renderSpec(ctx, msg.GetSpec(), msg.GetEnvironment(), msg.GetImage(), msg.GetProfile())
	if err != nil {
		return failRequest(err)
	}
	set, err := manifestSet(out)
	if err != nil {
		return fail(connect.CodeInternal, err)
	}
	t := target(out, msg.GetMode())
	dryRun := msg.GetDryRun()

	proposed := &kelsonv1alpha1.DeployResponse_Proposed{
		Project:     set.Project,
		Environment: set.Environment,
		Resources:   int32(len(set.Manifests)), //nolint:gosec // a rendered set is orders of magnitude below int32
		Mode:        t.Mode,
	}
	if dryRun == kelsonv1alpha1.DryRun_DRY_RUN_RENDER {
		// RENDER is the offline rung: the manifests ARE the answer, so they
		// ride the Proposed event and the stream ends without an adapter ever
		// being built.
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

	adapter, _, err := s.selectAdapter(ctx, t)
	if err != nil {
		return failRequest(err)
	}

	timeout := s.deployTimeout
	if secs := msg.GetTimeoutSeconds(); secs > 0 {
		timeout = time.Duration(secs) * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	res, err := adapter.Apply(ctx, set)
	if err != nil {
		return failRequest(err)
	}
	if !res.Applied {
		return fail(connect.CodeInternal,
			fmt.Errorf("api: the %s adapter did not complete the apply for %s/%s", adapter.Name(), set.Project, set.Environment))
	}
	set.Revision = res.Revision
	if err := stream.Send(&kelsonv1alpha1.DeployResponse{
		Event: &kelsonv1alpha1.DeployResponse_Committed_{
			Committed: &kelsonv1alpha1.DeployResponse_Committed{Revision: res.Revision, Adapter: adapter.Name()},
		},
	}); err != nil {
		return err
	}

	state, err := s.watch(ctx, adapter, set, timeout, stream)
	if err != nil {
		return err
	}
	return stream.Send(&kelsonv1alpha1.DeployResponse{
		Event: &kelsonv1alpha1.DeployResponse_Settled_{
			Settled: &kelsonv1alpha1.DeployResponse_Settled{
				Final: wireTransition(state),
				Error: firstWireError(state.Err()),
			},
		},
	})
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

// watch drives the deployment state machine off the adapter's status, pushing
// one Transition per change. It is the streaming twin of cmd/kelson's
// watchDeployment, including the de-duplication: only a change of phase or
// stuckness is an event, so a two-second poll does not become a two-second
// heartbeat on the wire.
func (s *Server) watch(ctx context.Context, adapter delivery.Adapter, set delivery.ManifestSet, timeout time.Duration, stream *connect.ServerStream[kelsonv1alpha1.DeployResponse]) (statemachine.State, error) {
	last := delivery.PhaseCommitted
	lastStuck := false
	// OnState is called from the engine's Run goroutine, which is this
	// goroutine — Run blocks — so the send error needs no synchronisation.
	var sendErr error
	engine, err := statemachine.New(statemachine.Config{
		Target:    statemachine.TargetFromSet(set),
		Source:    adapterSource(adapter, set, s.pollInterval),
		Timeout:   timeout,
		Component: adapter.Name(),
		OnState: func(st statemachine.State) {
			if sendErr != nil || (st.Phase == last && st.Stuck == lastStuck) {
				return
			}
			last, lastStuck = st.Phase, st.Stuck
			sendErr = stream.Send(&kelsonv1alpha1.DeployResponse{
				Event: &kelsonv1alpha1.DeployResponse_Transition_{Transition: wireTransition(st)},
			})
		},
	})
	if err != nil {
		return statemachine.State{}, fail(connect.CodeInternal, err)
	}

	state, runErr := engine.Run(ctx)
	if sendErr != nil {
		return statemachine.State{}, sendErr
	}
	switch {
	case runErr == nil:
		return state, nil
	case ctx.Err() != nil:
		// The budget expired. That is an answer ("not healthy in time"), not a
		// machinery failure — the engine's own progress timer gives the same
		// answer and the two expire together, so which fires first is scheduler
		// jitter. Ask the engine for the stuck verdict either way.
		return engine.MarkStuck(), nil
	default:
		return statemachine.State{}, fail(connect.CodeInternal, runErr)
	}
}

// adapterSource feeds the state machine from an adapter's Status, opening with
// a synthetic Committed observation because kelson watched the commit itself:
// Apply has already returned. An adapter still reporting Proposed is forwarded
// as Committed with its cause intact, because Proposed may only be followed by
// Committed or Rejected and anything else would be a backwards transition the
// engine rejects as an adapter bug. This mirrors cmd/kelson's adapterSource.
func adapterSource(adapter delivery.Adapter, set delivery.ManifestSet, interval time.Duration) statemachine.Source {
	poll := statemachine.Poll(statemachine.ObserverFunc(func(ctx context.Context) (delivery.Status, error) {
		st, err := adapter.Status(ctx, set)
		if err != nil {
			return delivery.Status{}, err
		}
		if st.Phase == delivery.PhaseProposed {
			st.Phase = delivery.PhaseCommitted
		}
		if st.Revision == "" {
			st.Revision = set.Revision
		}
		return st, nil
	}), interval)

	return statemachine.SourceFunc(func(ctx context.Context, out chan<- delivery.Status) error {
		committed := delivery.Status{Phase: delivery.PhaseCommitted, Revision: set.Revision}
		if err := statemachine.Send(ctx, out, committed); err != nil {
			return err
		}
		return poll.Watch(ctx, out)
	})
}

// Status reports the delivery phase of the rendered spec and the observation
// plane's verdict for each workload it declares. Both are needed and neither
// substitutes for the other: the phase says whether the change arrived, the
// verdicts say whether it works (issue #53).
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
	t := target(out, msg.GetMode())
	adapter, plane, err := s.selectAdapter(ctx, t)
	if err != nil {
		return nil, failRequest(err)
	}

	st, err := adapter.Status(ctx, set)
	if err != nil {
		return nil, failRequest(err)
	}
	verdicts, err := workloadVerdicts(ctx, plane, set, t.Namespace)
	if err != nil {
		return nil, failRequest(err)
	}
	return connect.NewResponse(&kelsonv1alpha1.StatusResponse{
		Phase:    string(st.Phase),
		Revision: st.Revision,
		Cause:    st.Cause,
		Detail:   st.Detail,
		Verdicts: verdicts,
		// The namespace the target resolved to, so a client addressing this
		// environment's workloads reads it rather than reconstructing the
		// model's default and missing a spec.namespace override (#161).
		Namespace: t.Namespace,
	}), nil
}

// workloadVerdicts evaluates the observation verdict for every Deployment the
// rendered set declares. The set IS the correlation: these are the resources
// kelson rendered for this project and environment. No probe means no verdicts,
// which is reported as an empty list rather than as "nothing is failing".
func workloadVerdicts(ctx context.Context, plane *Plane, set delivery.ManifestSet, namespace string) ([]*kelsonv1alpha1.WorkloadVerdict, error) {
	if plane.Health == nil {
		return nil, nil
	}
	var out []*kelsonv1alpha1.WorkloadVerdict
	for _, m := range set.Manifests {
		// The probe reads Deployments and the pods in their selector set; other
		// kinds carry no health signal it can classify, and reporting "unknown"
		// for them as if it were a verdict would be worse than saying nothing.
		if m.Kind != "Deployment" {
			continue
		}
		ns := m.Namespace
		if ns == "" {
			ns = namespace
		}
		verdict, err := plane.Health.Evaluate(ctx, ns, m.Name)
		if err != nil {
			return nil, err
		}
		out = append(out, &kelsonv1alpha1.WorkloadVerdict{
			Resource:    verdict.Resource,
			Code:        string(verdict.Code),
			Healthy:     verdict.Healthy,
			Degraded:    !verdict.Healthy && !verdict.Stuck && observation.IsFailure(verdict.Code),
			Message:     verdict.String(),
			Remediation: verdict.Remediation,
		})
	}
	return out, nil
}

// Rollback returns an environment to a recorded revision, streaming the
// irreversibility preview FIRST and always.
//
// A rollback is what people reach for when they are already in trouble, and the
// one thing that must not happen is discovering afterwards that it could not
// restore what they thought (issues #38, #55). dry_run=RENDER stops after the
// preview; anything else applies it.
func (s *Server) Rollback(ctx context.Context, req *connect.Request[kelsonv1alpha1.RollbackRequest], stream *connect.ServerStream[kelsonv1alpha1.RollbackResponse]) error {
	msg := req.Msg
	// Rollback replays recorded bytes rather than a re-render, but the spec is
	// still what names the project, environment and delivery mode; no image is
	// carried, so a spec that builds from source resolves without one.
	out, err := s.renderSpec(ctx, msg.GetSpec(), msg.GetEnvironment(), "", msg.GetProfile())
	if err != nil {
		return failRequest(err)
	}
	set, err := manifestSet(out)
	if err != nil {
		return fail(connect.CodeInternal, err)
	}
	adapter, plane, err := s.selectAdapter(ctx, target(out, msg.GetMode()))
	if err != nil {
		return failRequest(err)
	}
	if !adapter.Capabilities().SupportsRollback {
		return fail(connect.CodeFailedPrecondition,
			delivery.UnsupportedError(adapter.Name(), "rollback"))
	}

	entries, err := adapter.History(ctx, set)
	if err != nil {
		return failRequest(err)
	}
	entry, err := rollbackTarget(entries, msg.GetToRevision())
	if err != nil {
		return fail(connect.CodeFailedPrecondition, err)
	}
	if err := s.sendPreview(ctx, plane, set, entry, stream); err != nil {
		return err
	}
	if msg.GetDryRun() == kelsonv1alpha1.DryRun_DRY_RUN_RENDER {
		return nil
	}

	res, err := adapter.Rollback(ctx, set, entry)
	if err != nil {
		return stream.Send(&kelsonv1alpha1.RollbackResponse{
			Event: &kelsonv1alpha1.RollbackResponse_Settled_{
				Settled: &kelsonv1alpha1.RollbackResponse_Settled{Error: firstWireError(err)},
			},
		})
	}
	if !res.Applied {
		return fail(connect.CodeInternal,
			fmt.Errorf("api: the %s adapter did not complete the rollback to %s", adapter.Name(), entry.Revision))
	}
	if err := stream.Send(&kelsonv1alpha1.RollbackResponse{
		Event: &kelsonv1alpha1.RollbackResponse_Committed_{
			Committed: &kelsonv1alpha1.RollbackResponse_Committed{
				RestoredRevision: entry.Revision,
				AsRevision:       res.Revision,
			},
		},
	}); err != nil {
		return err
	}
	return stream.Send(&kelsonv1alpha1.RollbackResponse{
		Event: &kelsonv1alpha1.RollbackResponse_Settled_{Settled: &kelsonv1alpha1.RollbackResponse_Settled{}},
	})
}

// sendPreview streams what the rollback cannot safely revert. A mode whose
// recorded history kelson cannot read still gets a Preview event, carrying the
// target revision and no findings: an absent warning must never be mistaken for
// "nothing to warn about", so the event is present and empty rather than
// skipped.
func (s *Server) sendPreview(ctx context.Context, plane *Plane, set delivery.ManifestSet, entry delivery.Entry, stream *connect.ServerStream[kelsonv1alpha1.RollbackResponse]) error {
	preview := &kelsonv1alpha1.RollbackResponse_Preview{ToRevision: entry.Revision}
	if plane.Recorded != nil {
		d, findings, err := rollback.PreviewRevision(ctx, plane.Recorded, set.Project, set.Environment, entry.Revision)
		if err != nil {
			return failRequest(err)
		}
		encoded, err := diff.EncodeJSON(d)
		if err != nil {
			return fail(connect.CodeInternal, err)
		}
		preview.DiffJson = encoded
		for _, f := range findings {
			preview.Findings = append(preview.Findings, &kelsonv1alpha1.RollbackResponse_Finding{
				Resource:      f.Resource,
				Path:          f.Path,
				Cause:         string(f.Cause),
				Message:       f.Message,
				Unrecoverable: f.Never,
			})
		}
	}
	return stream.Send(&kelsonv1alpha1.RollbackResponse{
		Event: &kelsonv1alpha1.RollbackResponse_Preview_{Preview: preview},
	})
}

// rollbackTarget picks the revision to restore, matching the CLI's rule: with
// no to_revision it is the entry before the current one — "undo the last
// deploy" — and history is newest first, so that is index 1.
func rollbackTarget(entries []delivery.Entry, to string) (delivery.Entry, error) {
	if len(entries) == 0 {
		return delivery.Entry{}, fmt.Errorf("api: no recorded history: nothing has been deployed for this environment, so there is nothing to roll back to")
	}
	if to == "" {
		if len(entries) < 2 {
			return delivery.Entry{}, fmt.Errorf("api: only one recorded revision (%s): there is no previous state to restore", entries[0].Revision)
		}
		return entries[1], nil
	}
	if e, ok := findRevision(entries, to); ok {
		return e, nil
	}
	return delivery.Entry{}, fmt.Errorf("api: revision %q is not in the retained history", to)
}

// findRevision looks one revision up in a history listing.
func findRevision(entries []delivery.Entry, revision string) (delivery.Entry, bool) {
	for _, e := range entries {
		if e.Revision == revision {
			return e, true
		}
	}
	return delivery.Entry{}, false
}

// History returns the recorded revisions for an environment.
//
// It resolves the spec but does not render it: history needs the project,
// environment and delivery mode, and nothing else. Rendering would additionally
// require an image for a spec that builds from source (#136), which would make
// reading the past fail for a reason about the present.
func (s *Server) History(ctx context.Context, req *connect.Request[kelsonv1alpha1.HistoryRequest]) (*connect.Response[kelsonv1alpha1.HistoryResponse], error) {
	msg := req.Msg
	project, environment, resolved, err := s.resolve(ctx, msg.GetSpec(), msg.GetEnvironment(), "")
	if err != nil {
		return nil, failRequest(err)
	}
	t := Target{
		Project:     project.Metadata.Name,
		Environment: environment.Metadata.Name,
		Namespace:   resolved.Environment.Namespace,
		Mode:        msg.GetMode(),
		Git:         resolved.Environment.Delivery.Git,
	}
	if t.Mode == "" {
		t.Mode = string(resolved.Environment.Mode)
	}
	adapter, _, err := s.selectAdapter(ctx, t)
	if err != nil {
		return nil, failRequest(err)
	}

	entries, err := adapter.History(ctx, delivery.ManifestSet{Project: t.Project, Environment: t.Environment})
	if err != nil {
		return nil, failRequest(err)
	}
	out := make([]*kelsonv1alpha1.HistoryEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, &kelsonv1alpha1.HistoryEntry{
			Revision:    e.Revision,
			SpecHash:    e.SpecHash,
			CommittedAt: e.CommittedAt,
			Message:     e.Message,
			Author:      e.Author,
		})
	}
	return connect.NewResponse(&kelsonv1alpha1.HistoryResponse{Entries: out}), nil
}

// selectAdapter builds the delivery plane for this request and resolves the
// mode to an adapter, mirroring cmd/kelson's function of the same name.
func (s *Server) selectAdapter(ctx context.Context, t Target) (delivery.Adapter, *Plane, error) {
	if s.delivery == nil {
		return nil, nil, unimplemented("the delivery plane")
	}
	plane, err := s.delivery(ctx, t)
	if err != nil {
		return nil, nil, unavailable("api: building the delivery plane: %w", err)
	}
	adapter, err := plane.Registry.Select(t.Mode)
	if err != nil {
		return nil, nil, fmt.Errorf("api: delivery mode %q is not available: %w "+
			"(direct always is; flux needs spec.delivery.git on the Environment)", t.Mode, err)
	}
	return adapter, plane, nil
}

// wireTransition projects a statemachine.State onto the wire. The stream speaks
// the engine's snapshots verbatim (ADR-0013 §2) so no phase gains a second
// meaning on the way out.
func wireTransition(st statemachine.State) *kelsonv1alpha1.DeployResponse_Transition {
	t := &kelsonv1alpha1.DeployResponse_Transition{
		Phase:            string(st.Phase),
		Answer:           string(st.Answer()),
		Stuck:            st.Stuck,
		ObservedRevision: st.ObservedRevision,
	}
	if !st.Cause.IsZero() {
		t.Cause = &kelsonv1alpha1.DeployResponse_Cause{
			Component: st.Cause.Component,
			Reason:    st.Cause.Reason,
			Message:   st.Cause.Message,
		}
	}
	if !st.Since.IsZero() {
		t.SinceUnixMs = st.Since.UnixMilli()
	}
	return t
}

// firstWireError projects a settled failure onto the single Error the Settled
// events carry.
func firstWireError(err error) *kelsonv1alpha1.Error {
	wire := wireErrors(err)
	if len(wire) == 0 {
		if err == nil {
			return nil
		}
		return &kelsonv1alpha1.Error{Code: string(delivery.ErrApplyFailed), Message: err.Error()}
	}
	return wire[0]
}
