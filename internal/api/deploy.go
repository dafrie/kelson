package api

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/controlstore"
	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/delivery/statemachine"
	"github.com/dafrie/kelson/internal/diff"
	"github.com/dafrie/kelson/internal/model"
	"github.com/dafrie/kelson/internal/observation"
	"github.com/dafrie/kelson/internal/promote"
)

// Deploy writes the spec and then streams what the controller does with it.
//
// # Deploying is a write and a watch (ADR-0028, issue #225)
//
// There is no apply left to call. kelson-controller reconciles an
// `Environment`: it validates, renders, publishes an immutable OCI artifact and
// applies the Flux pair, and it records every step in `Environment.status`. So
// the deploy is the *spec write* — a server-side apply through the spec store,
// which is what bumps `.metadata.generation` and starts a reconcile — and the
// rest of this RPC is a projection of the status that follows, until it settles
// or the caller goes away.
//
// # What the write touches, and what it deliberately leaves alone
//
// A deploy writes the project document it carries and the one environment it
// names. The other environments a project holds are left exactly as they are:
// PutSpec is the verb that replaces a document set, and a deploy of
// `-f project.yaml -f production.yaml` must not delete staging because it was
// not mentioned.
//
// # The dry-run rungs are unchanged
//
// dry_run=RENDER is the offline rung — the manifests ARE the answer — and
// dry_run=SERVER is the API server's own verdict on the rendered set through
// internal/delivery/dryrun. Neither ever needed an adapter and neither writes
// anything, so both behave exactly as they did before the spine (ADR-0025's
// propose-only agents notice nothing).
//
// The event order never varies: Proposed (what is about to happen), then the
// rung's own answer — for a real deploy, Committed once the controller reports
// a revision, a Transition per visible status change, and exactly one Settled.
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
		// A deploy is a spec write now (issue #225), so a deploy that carries
		// its own documents is also governed by the rule PutSpec is governed
		// by — and for the identical reason. The documents it writes include
		// the Project, and the Project carries `defaults.policy`: an agent that
		// could rewrite it through this RPC could set `agents: allow` and then
		// do anything, which would make every other refusal here advisory.
		//
		// A deploy of a *stored* spec is exempt only while it changes no
		// document, which is what the agent surface usually sends
		// (internal/mcp names a stored project). `--image` is the exception and
		// it is a real one: an image override has to be written to be honoured
		// — the render happens in the controller, from the custom resource —
		// so a stored deploy carrying one edits the environment's document
		// (writeSpec) and is a spec write like any other. Guarding only the
		// inline shape let an agent under `forbid: [spec-write]` change the
		// image every component runs, which is most of what a spec says.
		_, inline := msg.GetSpec().GetSpec().(*kelsonv1alpha1.SpecRef_Documents)
		if inline || msg.GetImage() != "" {
			if err := s.guardStored(ctx, model.AgentOpSpecWrite, set.Project); err != nil {
				return err
			}
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
	// elsewhere (ADR-0025 §5). It runs before the write, because a refused
	// deploy must change nothing.
	if err := s.requireDryRun(ctx, guard, out.profile, set); err != nil {
		return err
	}

	if err := s.writeSpec(ctx, msg, out); err != nil {
		return failRequest(err)
	}
	return s.followEnvironment(ctx, set.Project, set.Environment, msg.GetTimeoutSeconds(), stream)
}

// writeSpec is the deploy itself: the spec, applied.
//
// The version it asserts is the one it just read, which is the same
// read-modify-write Promote performs and for the same reason — DeployRequest
// carries no version field, so the alternative to reading one is a blind
// overwrite, and blind overwrites are what optimistic concurrency exists to
// refuse. A spec that changed between the read and the write comes back as
// store/version-conflict, which is the honest answer: something else deployed
// while this request was in flight.
func (s *Server) writeSpec(ctx context.Context, msg *kelsonv1alpha1.DeployRequest, out *rendered) error {
	if s.specs == nil {
		return unimplemented("the spec store")
	}
	project := out.project.Metadata.Name
	docs, version, err := s.deployDocuments(ctx, msg.GetSpec(), project, msg.GetEnvironment())
	if err != nil {
		return err
	}
	if docs, err = pinDeployImage(docs, out, msg.GetImage()); err != nil {
		return err
	}
	_, err = s.specs.Put(ctx, project, docs, controlstore.PutOptions{
		ExpectedVersion: version,
		IdempotencyKey:  msg.GetIdempotencyKey(),
	})
	return err
}

// pinDeployImage writes a deploy's `--image` into the one environment the
// deploy names, as the per-component pins rule P3 says it stands in for.
//
// # Why the image is written at all
//
// Under the spine the render happens in the controller, from the custom
// resource. An override applied only to this server's own pre-flight render
// would be silently dropped on the way to the cluster: the deploy would report
// one image in its Proposed event and the cluster would run another. Writing it
// is what makes it true.
//
// # Why it is written HERE and not on the Project
//
// It used to be `Project.spec.image` (controlstore's PutOptions.Image), and that
// is a project-wide, every-environment fact: `kelson deploy --env development
// --image …:pr-417` durably changed what production's next deploy would resolve
// to. A deploy names one environment; ADR-0016's whole point is that an image
// belongs to an environment, and `Environment.spec.components[].image` is the
// field that says so (docs/model.md rule P3, the innermost scope). So the
// override lands there, spliced by the same internal/promote.Pin a promotion
// uses — one line per component, the rest of the document byte-identical.
//
// # Which components it maps onto, and where that stops
//
// `--image` stands in for `Project.spec.image`, so it applies to exactly the
// components that would have resolved to it: workloads that name no image of
// their own and carry no pin in this environment already. A component with its
// own `image:` still wins, as rule P3 says, and an existing pin is untouched —
// pinning those would make `--image` beat scopes it is documented to lose to.
// Data and helm components are skipped because an image on their override is a
// validation error (`schema/mutually-exclusive`), not a no-op.
//
// One `--image` therefore lands on every component that took the project image,
// which is what writing `Project.spec.image` did too — a multi-component project
// deployed with one `--image` runs that image everywhere it applied, and now
// says so per component instead of once for every environment at the top.
//
// What this costs, stated: the pin outlives the deploy that wrote it, the same
// way the project-wide write did, and unlike the project-wide write it also
// outranks a component `image:` added to the Project later. That is the price of
// recording the override where it took effect, and `kelson promote` and an edit
// to the environment document are both ways back out of it.
func pinDeployImage(docs controlstore.Documents, out *rendered, image string) (controlstore.Documents, error) {
	if image == "" {
		return docs, nil
	}
	components := componentsTakingProjectImage(out.project, out.environment)
	if len(components) == 0 {
		// Every component names its own image or is already pinned here, so
		// `--image` changed nothing about this render either. Writing a pin
		// nobody resolved would be inventing a change the deploy did not make.
		return docs, nil
	}
	environment := out.environment.Metadata.Name
	key, doc, err := environmentDocument(docs, environment)
	if err != nil {
		return controlstore.Documents{}, err
	}
	for _, name := range components {
		if doc, err = promote.Pin(doc, environment, name, image); err != nil {
			return controlstore.Documents{}, err
		}
	}
	written := copyDocuments(docs)
	written.Environments[key] = doc
	return written, nil
}

// componentsTakingProjectImage names the components `--image` actually reaches,
// in spec order: workloads with no image of their own and no pin in this
// environment. It is rule P3 read backwards — the scopes that beat
// `Project.spec.image` are exactly the ones that beat `--image`.
func componentsTakingProjectImage(project *model.Project, environment *model.Environment) []string {
	pinned := make(map[string]bool, len(environment.Spec.Components))
	for _, ov := range environment.Spec.Components {
		if ov.Image != "" {
			pinned[ov.Name] = true
		}
	}
	var out []string
	for _, c := range project.Spec.Components {
		if !c.EffectiveKind().IsWorkload() || c.Image != "" || pinned[c.Name] {
			continue
		}
		out = append(out, c.Name)
	}
	return out
}

// deployDocuments assembles the document set the deploy writes, and the version
// the write asserts.
//
// A stored spec deploys itself: the documents are already the desired state, so
// the apply is a no-op at the API server and the stream that follows reports
// what the environment is doing. Inline documents are merged into whatever the
// store already holds — the project document and the named environment are
// replaced, every other environment is carried through untouched — because a
// deploy names one environment and a document set nobody sent is not a
// deletion request.
func (s *Server) deployDocuments(ctx context.Context, ref *kelsonv1alpha1.SpecRef, project, environment string) (controlstore.Documents, string, error) {
	docs, inline := ref.GetSpec().(*kelsonv1alpha1.SpecRef_Documents)
	stored, err := s.specs.Get(ctx, project)
	switch {
	case err == nil:
	case !controlstore.AsNotFound(err):
		return controlstore.Documents{}, "", err
	case !inline:
		// A stored spec that is not stored: the read is the answer.
		return controlstore.Documents{}, "", err
	default:
		// A first deploy of an inline spec creates the project.
		stored = controlstore.Stored{Documents: controlstore.Documents{Environments: map[string][]byte{}}}
	}
	if !inline {
		return stored.Documents, stored.Version, nil
	}

	out := copyDocuments(stored.Documents)
	if out.Environments == nil {
		out.Environments = map[string][]byte{}
	}
	out.Project = docs.Documents.GetProject()
	name, doc, err := requestedEnvironment(docs.Documents.GetEnvironments(), environment)
	if err != nil {
		return controlstore.Documents{}, "", err
	}
	out.Environments[name] = doc
	return out, stored.Version, nil
}

// requestedEnvironment picks the inline document the deploy is about, under the
// same rule --env has: an unnamed environment is unambiguous only when the
// request carries one.
func requestedEnvironment(docs map[string][]byte, environment string) (string, []byte, error) {
	if environment != "" {
		if doc, ok := docs[environment]; ok {
			return environment, doc, nil
		}
		return "", nil, fmt.Errorf("api: the request names environment %q and the documents do not carry it", environment)
	}
	if len(docs) != 1 {
		return "", nil, fmt.Errorf("api: the request carries %d environment documents and names none; name one", len(docs))
	}
	for name, doc := range docs {
		return name, doc, nil
	}
	return "", nil, fmt.Errorf("api: the request carries no environment document")
}

// followEnvironment streams `Environment.status` until it settles, the budget
// expires, or the caller goes away.
//
// # Which event each status write becomes
//
// The first status naming a revision is Committed: the artifact is published
// and the Flux pair points at it, which is what "the apply landed; revision
// assigned" means on this spine. Every status whose projection differs from the
// last one is a Transition. The settle is the controller's own
// `Progressing=False` for the current generation, which is the one place that
// knows the difference between "Flux is still working" and "nothing more will
// happen without a human".
//
// # A status that has not caught up is not an answer
//
// Every event is gated on `observedGeneration >= generation`. Between the write
// above and the controller's first reconcile the object still carries the
// *previous* deployment's phase, revision and conditions, and streaming those
// would report the last deploy's outcome as this one's.
func (s *Server) followEnvironment(ctx context.Context, project, environment string, timeoutSeconds int64, stream *connect.ServerStream[kelsonv1alpha1.DeployResponse]) error {
	environments, err := s.environmentStore()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	states, err := environments.Watch(ctx, project, environment)
	if err != nil {
		return failRequest(err)
	}

	budget := time.NewTimer(s.deployBudget(timeoutSeconds))
	defer budget.Stop()

	var last *kelsonv1alpha1.DeployResponse_Transition
	var committed bool
	var current controlstore.EnvironmentState
	for {
		select {
		case <-ctx.Done():
			// A cancelled client is not a failed deployment: the write landed
			// and the controller carries on without this stream.
			return ctx.Err()

		case <-budget.C:
			// The budget is this stream's, not the deployment's. Stuck is the
			// state machine's word for "no progress within the timeout", and
			// the phase it is stuck in is the diagnosis.
			state := deliveryState(current, true)
			return s.settle(stream, current, state)

		case st, ok := <-states:
			if !ok {
				return failRequest(unavailable("api: the watch on %s/%s ended before the deployment settled",
					project, environment))
			}
			current = st
			if !st.Current() {
				continue
			}
			if !committed && st.Revision != "" {
				committed = true
				// The audit record's revision is the *delivery* revision, and
				// this is where it becomes knowable: the controller assigns it
				// when it publishes, so a spec write that nothing has picked up
				// yet has produced no revision to record (audit.go).
				auditChange(ctx, controlstore.AuditChange{Revision: st.Revision})
				if err := stream.Send(&kelsonv1alpha1.DeployResponse{
					Event: &kelsonv1alpha1.DeployResponse_Committed_{Committed: &kelsonv1alpha1.DeployResponse_Committed{
						Revision: st.Revision,
						Adapter:  adapterName,
					}},
				}); err != nil {
					return err
				}
			}
			state := deliveryState(st, false)
			transition := wireTransition(state)
			if !sameTransition(last, transition) {
				last = transition
				if err := stream.Send(&kelsonv1alpha1.DeployResponse{
					Event: &kelsonv1alpha1.DeployResponse_Transition_{Transition: transition},
				}); err != nil {
					return err
				}
			}
			if st.Settled() {
				return s.settle(stream, st, state)
			}
		}
	}
}

// settle sends the one terminal event.
//
// An unhealthy deployment settles cleanly: the stream completes and the error
// rides *inside* Settled, because a deployment that failed is an answer and not
// a transport failure — statemachine.Run's contract, and the reason the wire
// has an error field here at all.
func (s *Server) settle(stream *connect.ServerStream[kelsonv1alpha1.DeployResponse], st controlstore.EnvironmentState, state statemachine.State) error {
	settled := &kelsonv1alpha1.DeployResponse_Settled{Final: wireTransition(state)}
	if err := settledError(st, state); err != nil {
		settled.Error = wireError(err)
	}
	return stream.Send(&kelsonv1alpha1.DeployResponse{
		Event: &kelsonv1alpha1.DeployResponse_Settled_{Settled: settled},
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

// Status reports two things and neither substitutes for the other (issue #53):
// the delivery phase says whether the change ARRIVED, the verdicts say whether
// it WORKS.
//
// The phase, the revision and the cause come from `Environment.status`
// (ADR-0027 decision 6) and the verdicts from the observation plane, exactly as
// before. A server with no status seam, or an environment kelson has never been
// given, reports the workload half and says in `cause` why the delivery half is
// missing — an empty phase is a value a client can branch on, and a guessed
// "Healthy" would not be.
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
	res := &kelsonv1alpha1.StatusResponse{
		Verdicts: verdicts,
		// The namespace the target resolved to, so a client addressing this
		// environment's workloads reads it rather than reconstructing the
		// model's default and missing a spec.namespace override (#161).
		Namespace: t.Namespace,
	}
	s.reportDelivery(ctx, res, t)
	return connect.NewResponse(res), nil
}

// reportDelivery fills in the delivery half of a status from
// `Environment.status`, or says why it is empty.
//
// It never fails the RPC. The workload verdicts are the half a caller looks at
// when something is broken, and losing them because the control plane could not
// answer the other half would be the wrong trade — so a missing seam, an
// environment that was never stored and a status that has not caught up are all
// reported in `cause` with the phase left empty.
func (s *Server) reportDelivery(ctx context.Context, res *kelsonv1alpha1.StatusResponse, t Target) {
	if s.environments == nil {
		res.Cause = "the delivery phase is not reported: this server was started without the Environment " +
			"status reader, so it can see the workloads but not what kelson delivered"
		return
	}
	st, err := s.environments.Get(ctx, t.Project, t.Environment)
	if err != nil {
		res.Cause = fmt.Sprintf("the delivery phase is not reported: %v", err)
		return
	}
	res.Phase, res.Revision = st.Phase, st.Revision
	state := deliveryState(st, false)
	res.Cause = state.Cause.String()
	if !st.Current() {
		// The status describes an older generation than the spec. Saying so is
		// the whole point of observedGeneration: the phase below is real, and
		// it is not about the spec the caller is holding.
		res.Cause = fmt.Sprintf("%s (status is at generation %d, the spec is at %d)",
			state.Cause.String(), st.ObservedGeneration, st.Generation)
	}
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

// Rollback pins the environment to a revision it has already published, by
// writing `kelson.dev/rollback-to` (ADR-0028 decision 5).
//
// # A rollback is a pointer move, and the preview says less than it used to
//
// It used to pick a revision, compute what replaying it could not revert, and
// replay it. Two of those three are gone: the artifact for every revision is in
// the registry, immutable, so there is nothing to replay — the controller
// repoints the OCIRepository at a tag that already exists — and the byte-level
// comparison that produced the findings would need this server to fetch two
// artifacts, which it does not do.
//
// So the Preview event carries the target and one finding that says the
// comparison is unavailable and where to get it (`flux pull artifact`). It does
// not carry an empty diff: "no findings" and "kelson did not look" are
// different facts, and reporting the first when the second is true is the
// precise failure a preview exists to prevent.
//
// # The target is checked against the same window the controller checks
//
// The controller refuses a target that is not in `status.history` and reports
// it as `RollbackTargetUnknown` — after the annotation is written, which would
// leave the environment carrying a pin nobody can honour. This handler checks
// the mirror first and refuses in the caller's own request, so a typo is an
// InvalidArgument with the known revisions listed and nothing is written.
// A target that passes that check and is still refused by the controller — the
// history moved under the request — arrives as the Settled event's error, which
// followRollback reads from the controller's own Ready condition.
//
// # Writing the pin is not always writing the annotation
//
// Re-requesting a pin the controller has declared inert is the case one
// annotation patch cannot express, because the patch would be a no-op. See
// [Server.armRollback], which is where the whole of that is decided.
func (s *Server) Rollback(ctx context.Context, req *connect.Request[kelsonv1alpha1.RollbackRequest], stream *connect.ServerStream[kelsonv1alpha1.RollbackResponse]) error {
	msg := req.Msg
	auditDryRun(ctx, msg.GetDryRun())
	auditIdempotencyKey(ctx, msg.GetIdempotencyKey())

	// Agent policy answers first: `forbid: [rollback]` and `propose-only` are
	// statements about this principal, and they must be reached before
	// anything about the request's own shape.
	//
	// The spec is resolved rather than rendered. A rollback is exactly the
	// operation an author reaches for when the current spec is bad, and
	// requiring it to render would refuse the rollback that fixes a render
	// failure (the one the environment is stuck on).
	project, environment, _, err := s.resolve(ctx, msg.GetSpec(), msg.GetEnvironment(), "")
	if err != nil {
		return failRequest(err)
	}
	name, envName := project.Metadata.Name, environment.Metadata.Name
	if msg.GetDryRun() != kelsonv1alpha1.DryRun_DRY_RUN_RENDER {
		if _, err := s.guard(ctx, model.AgentOpRollback, name, envName); err != nil {
			return err
		}
	}

	environments, err := s.environmentStore()
	if err != nil {
		return err
	}
	st, err := environments.Get(ctx, name, envName)
	if err != nil {
		return failRequest(err)
	}
	revision, err := rollbackTarget(st, msg.GetToRevision())
	if err != nil {
		return fail(connect.CodeInvalidArgument, err)
	}

	if err := stream.Send(&kelsonv1alpha1.RollbackResponse{
		Event: &kelsonv1alpha1.RollbackResponse_Preview_{Preview: &kelsonv1alpha1.RollbackResponse_Preview{
			ToRevision: revision.Revision,
			Findings:   []*kelsonv1alpha1.RollbackResponse_Finding{rollbackPreviewGap(st, revision)},
		}},
	}); err != nil {
		return err
	}
	// RENDER is preview-only, for the API as for the CLI.
	if msg.GetDryRun() == kelsonv1alpha1.DryRun_DRY_RUN_RENDER {
		return nil
	}

	if err := s.armRollback(ctx, environments, name, envName, st, revision.Revision); err != nil {
		return failRequest(err)
	}
	auditChange(ctx, controlstore.AuditChange{Revision: revision.Revision, From: st.Revision})

	if err := stream.Send(&kelsonv1alpha1.RollbackResponse{
		Event: &kelsonv1alpha1.RollbackResponse_Committed_{Committed: &kelsonv1alpha1.RollbackResponse_Committed{
			RestoredRevision: revision.Revision,
			// AsRevision is empty and stays empty: a rollback publishes
			// nothing and prepends no history entry (ADR-0028 decision 5,
			// internal/controller/history.go). There is no new revision it was
			// "recorded as", and naming the restored one twice would invent a
			// deployment that did not happen.
		}},
	}); err != nil {
		return err
	}
	return s.followRollback(ctx, name, envName, revision.Revision, st, stream)
}

// armRollback writes the pin, and re-arms one the controller has already
// declared inert.
//
// # The bug this exists for
//
// The controller reads the annotation against its own bookkeeping
// (internal/controller's rollbackFor). A pin whose value it has already acted
// on and whose generation has since moved is *inert*: the spec was edited after
// the rollback, which resumes normal publishing, and the annotation stays on
// the object doing nothing (ADR-0028 decision 5). The status keeps naming it,
// deliberately — dropping `status.rollbackRevision` would make the next
// reconcile read the annotation as a brand-new rollback and pin again, flapping
// the environment back off the edit that fixed it.
//
// So the obvious re-request is a silent no-op: roll back, edit the spec, watch
// the edit make things worse, roll back to the same revision again — and the
// second request merge-patches a value the object already carries. An identical
// annotation value changes nothing, annotations do not bump `.metadata.
// generation`, and the controller's verdict on the unchanged pair is still
// "inert". Nothing reconciles, and the stream times out claiming the controller
// "has not reported acting on it yet".
//
// # Clearing is what re-arms it
//
// Removing the annotation is the one input that makes the controller drop the
// bookkeeping (`env.Status.RollbackRevision, ... = "", 0` on the no-annotation
// path). Once it has, writing the pin back is case 1 of rollbackFor — a target
// nobody has acted on — and it pins at the *current* generation, which is
// exactly the new rollback the caller asked for.
//
// The wait between the two writes is not optional and it is not a race
// tolerance: reconciles are level-triggered, so a clear and a set the controller
// collapses into one reconcile leave it looking at the same annotation and the
// same status it already called inert. Waiting for it to let go is what makes
// the second write mean something.
//
// What the window costs is stated rather than hidden: with no annotation the
// environment tracks its spec again, so a controller that reconciles inside it
// may republish the current spec — the one the caller is rolling back *from* —
// before the pin returns. That is the same brief republish an operator gets from
// `kubectl annotate --remove` followed by `kubectl annotate`, it is bounded by
// this function, and the pin that follows restores the target. A wait that
// expires writes the pin anyway: no worse than today's single patch, and
// followRollback then reports what the controller does or does not do with it.
func (s *Server) armRollback(ctx context.Context, environments EnvironmentStore, project, environment string,
	st controlstore.EnvironmentState, revision string) error {
	pin := func() error {
		_, err := environments.Annotate(ctx, project, environment, map[string]string{annotationRollbackTo: revision})
		return err
	}
	if !inertPin(st, revision) {
		// A new target, or the standing active pin. Both are one patch: a new
		// target is case 1 for the controller whatever the status says, and
		// re-requesting the pin that is currently in force is a no-op the
		// controller is right to ignore — clearing and re-setting *that* one
		// would unpin a healthy environment onto its spec for no reason.
		return pin()
	}
	if _, err := environments.Annotate(ctx, project, environment,
		map[string]string{annotationRollbackTo: ""}); err != nil {
		return err
	}
	if err := s.awaitPinReleased(ctx, environments, project, environment, revision); err != nil {
		return err
	}
	return pin()
}

// inertPin reports whether the controller has already declared this exact pin
// inert. It is rollbackFor's second case read from the other side: the status
// names the requested revision, and the spec has moved since the reconcile that
// applied it.
func inertPin(st controlstore.EnvironmentState, revision string) bool {
	return st.RollbackRevision == revision && st.Generation > st.RollbackGeneration
}

// awaitPinReleased waits for the controller to drop the bookkeeping the cleared
// annotation invalidates, so the pin written after it reads as a new rollback.
//
// An expiry is not an error: the caller writes the pin regardless and the
// stream that follows is what reports the outcome. The budget is deliberately
// far shorter than a deployment's — this is one reconcile of one object, and a
// controller that cannot manage it in that time is a controller the rollback
// stream is about to report on anyway.
func (s *Server) awaitPinReleased(ctx context.Context, environments EnvironmentStore, project, environment, revision string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	states, err := environments.Watch(ctx, project, environment)
	if err != nil {
		return err
	}
	budget := time.NewTimer(s.rearmBudget())
	defer budget.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-budget.C:
			return nil
		case st, ok := <-states:
			if !ok {
				return nil
			}
			if st.RollbackRevision != revision {
				return nil
			}
		}
	}
}

// rearmBudget bounds the wait between the two halves of a re-arm.
func (s *Server) rearmBudget() time.Duration {
	if s.deployTimeout < rollbackRearmBudget {
		return s.deployTimeout
	}
	return rollbackRearmBudget
}

// rollbackRearmBudget is how long a re-arm waits for the controller to let go
// of a pin before writing the new one anyway.
const rollbackRearmBudget = 30 * time.Second

// followRollback waits for the controller to act on the annotation and reports
// how it went.
//
// The wait is bounded by the same budget a deploy gets. On expiry the rollback
// is *not* cancelled — the annotation is in force and the controller will
// honour it — so the Settled error says exactly that rather than claiming a
// failure.
//
// # Two ways the controller answers, and both have to end the wait
//
// The ordinary answer is the bookkeeping: `status.rollbackRevision` becomes the
// target, and the conditions beside it say whether it is serving. The other is a
// refusal — the target left the bounded history mirror between this server's
// pre-flight check and the reconcile — and a refusal writes `Ready=False` with
// reason [reasonRollbackTargetUnknown] and *no* rollbackRevision, because the
// controller refuses before it records anything (internal/controller's
// verifyRollbackTarget, reached from the Active branch). Gating everything on
// the bookkeeping alone would make that refusal invisible: the stream would run
// out its whole budget and settle with `delivery/not-watched` saying the
// controller had not reported acting on the pin, when it had reported declining
// it. This handler's own doc promises that answer "arrives as the Settled
// event's error", so it is read here.
//
// `before` is the state as it stood before the annotation was written, and it is
// carried in for one job: a refusal already sitting in the status when this
// request arrived is a previous request's answer, not this one's, and settling
// on it would report the wrong target as unknown.
func (s *Server) followRollback(ctx context.Context, project, environment, revision string,
	before controlstore.EnvironmentState, stream *connect.ServerStream[kelsonv1alpha1.RollbackResponse]) error {
	environments, err := s.environmentStore()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	states, err := environments.Watch(ctx, project, environment)
	if err != nil {
		return failRequest(err)
	}
	budget := time.NewTimer(s.deployBudget(0))
	defer budget.Stop()

	settle := func(err error) error {
		settled := &kelsonv1alpha1.RollbackResponse_Settled{}
		if err != nil {
			settled.Error = wireError(err)
		}
		return stream.Send(&kelsonv1alpha1.RollbackResponse{
			Event: &kelsonv1alpha1.RollbackResponse_Settled_{Settled: settled},
		})
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-budget.C:
			return settle(delivery.Error{
				Code:     delivery.ErrNotWatched,
				Resource: project + "/" + environment,
				Message: fmt.Sprintf("the rollback to %s is written and the controller has not reported acting on it yet",
					revision),
				Remediation: "the pin is in force and nothing is cancelled: watch it with `kelson status`, or " +
					"`kubectl describe environment " + environment + "`",
				DocsURL: "https://kelson.dev/delivery/errors/" + string(delivery.ErrNotWatched),
			})
		case st, ok := <-states:
			if !ok {
				return settle(unavailable("api: the watch on %s/%s ended before the rollback settled",
					project, environment))
			}
			if refusedRollback(before, st, revision) {
				return settle(settledError(st, deliveryState(st, false)))
			}
			if st.RollbackRevision != revision {
				continue
			}
			if ready, found := st.Ready(); found && !ready.True() {
				return settle(settledError(st, deliveryState(st, false)))
			}
			if st.Revision == revision {
				return settle(nil)
			}
		}
	}
}

// refusedRollback reports whether this status is the controller declining THIS
// rollback.
//
// The reason is the controller's own ([reasonRollbackTargetUnknown]) and a
// refusal records no rollbackRevision, so the reason is the whole signal — which
// makes staleness the only thing left to rule out. A refusal already present
// before the annotation was written, still verbatim identical, and written while
// the object carried some other pin, is the previous request's answer about a
// different target: this one has not been read yet, and settling on it would
// name the wrong revision as unknown. The same refusal under an annotation that
// already named this revision is not stale at all — the write was a no-op, no
// reconcile is coming, and the standing refusal *is* the answer.
func refusedRollback(before, st controlstore.EnvironmentState, revision string) bool {
	ready, ok := st.Ready()
	if !ok || ready.True() || ready.Reason != reasonRollbackTargetUnknown {
		return false
	}
	prior, had := before.Ready()
	if had && prior.Reason == ready.Reason && prior.Message == ready.Message &&
		before.Annotations[annotationRollbackTo] != revision {
		return false
	}
	return true
}

// rollbackTarget decides which revision a rollback restores, and refuses every
// target the controller would refuse.
//
// An empty to_revision is "the previous revision": the newest published one
// that is not the one being served. Anything else must be in the history
// mirror — kelson will not point an OCIRepository at a tag it cannot confirm it
// published, and the mirror is bounded, so a correct-but-ancient target reads
// exactly like a typo and both are refused with the window named.
func rollbackTarget(st controlstore.EnvironmentState, requested string) (controlstore.Revision, error) {
	if len(st.History) == 0 {
		return controlstore.Revision{}, fmt.Errorf(
			"api: %s/%s has published nothing, so there is no revision to roll back to",
			st.Project, st.Environment)
	}
	if requested == "" {
		previous, ok := st.PreviousRevision()
		if !ok {
			return controlstore.Revision{}, fmt.Errorf(
				"api: %s/%s has published one revision (%s) and it is the one running, so there is no previous one to roll back to",
				st.Project, st.Environment, st.History[0].Revision)
		}
		return previous, nil
	}
	if !revisionFormat.MatchString(requested) {
		return controlstore.Revision{}, fmt.Errorf(
			"api: %q is not a revision: a revision is <generation>-<spec-hash-short>, e.g. %s",
			requested, st.History[0].Revision)
	}
	if found, ok := st.FindRevision(requested); ok {
		return found, nil
	}
	return controlstore.Revision{}, fmt.Errorf(
		"api: %s/%s has no revision %q in its history (%s). The mirror holds the most recent %d revisions; "+
			"the registry holds every one ever published, so an older target is a registry query",
		st.Project, st.Environment, requested, strings.Join(knownRevisions(st), ", "), len(st.History))
}

// revisionFormat is the artifact tag grammar of ADR-0028 decision 2:
// <generation>-<spec-hash-short>. Checking it before the history lookup is what
// lets a caller who typed a branch name or a digest read that they typed the
// wrong *kind* of thing, rather than that their revision is not in the window.
var revisionFormat = regexp.MustCompile(`^[1-9][0-9]*-[0-9a-f]{8}$`)

func knownRevisions(st controlstore.EnvironmentState) []string {
	out := make([]string, 0, len(st.History))
	for _, r := range st.History {
		out = append(out, r.Revision)
	}
	return out
}

// rollbackPreviewGap is the one finding a rollback preview always carries: what
// this server cannot tell the caller, and where they can get it.
//
// It is a finding rather than an omission because the caller must not read an
// empty findings list as "nothing about this rollback is irreversible". It is
// not marked unrecoverable: nothing about the rollback is known to be
// unrevertible — what is missing is the knowledge, and saying otherwise would
// be a second lie in place of the first.
func rollbackPreviewGap(st controlstore.EnvironmentState, target controlstore.Revision) *kelsonv1alpha1.RollbackResponse_Finding {
	from := st.Revision
	if from == "" {
		from = "the current revision"
	}
	return &kelsonv1alpha1.RollbackResponse_Finding{
		Resource: st.Project + "/" + st.Environment,
		Cause:    "rollback/preview-unavailable",
		Message: fmt.Sprintf("kelson cannot show what changes between %s and %s: both revisions are immutable OCI "+
			"artifacts in the registry (ADR-0028 decision 4) and this server does not fetch them. The rollback "+
			"itself is exact — it repoints at bytes that already exist and cannot have changed.", from, target.Revision),
		Unrecoverable: false,
	}
}

// History lists what this environment has published, newest first.
//
// # The record is the registry; this is the window onto it
//
// `Environment.status.history[]` mirrors the most recent
// [v1alpha1.MaxHistoryEntries] revisions (ADR-0028 decision 4). Anything older
// is still in the registry, immutably, and reading it is a registry query this
// RPC deliberately does not make: a bounded, cheap answer that says how far it
// goes is more useful than an unbounded one that needs registry credentials.
//
// # What the wire cannot carry, and where it went
//
// The spine's entry has a digest, the images it resolved to and the outcome it
// reached; HistoryEntry has `message` and `author`. So the message carries the
// outcome, the digest and the images — it is the only slot they fit in — and
// the author stays empty, because the spine records who deployed nothing. Who
// did what is the audit trail's question (ADR-0026, QueryAudit).
func (s *Server) History(ctx context.Context, req *connect.Request[kelsonv1alpha1.HistoryRequest]) (*connect.Response[kelsonv1alpha1.HistoryResponse], error) {
	msg := req.Msg
	// Resolved, not rendered: history is a question about what ran, and a spec
	// whose render is currently broken is exactly when it gets asked.
	project, environment, _, err := s.resolve(ctx, msg.GetSpec(), msg.GetEnvironment(), "")
	if err != nil {
		return nil, failRequest(err)
	}
	environments, err := s.environmentStore()
	if err != nil {
		return nil, err
	}
	st, err := environments.Get(ctx, project.Metadata.Name, environment.Metadata.Name)
	if err != nil {
		return nil, failRequest(err)
	}

	entries := make([]*kelsonv1alpha1.HistoryEntry, 0, len(st.History))
	for _, r := range st.History {
		entries = append(entries, &kelsonv1alpha1.HistoryEntry{
			Revision:    r.Revision,
			SpecHash:    r.SpecHash,
			CommittedAt: committedAt(r.Timestamp),
			Message:     revisionSummary(r, r.Revision == st.Revision),
		})
	}
	return connect.NewResponse(&kelsonv1alpha1.HistoryResponse{Entries: entries}), nil
}

func committedAt(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// revisionSummary is the prose the wire's `message` carries for one revision:
// how that deployment ended, what it published and what it runs.
func revisionSummary(r controlstore.Revision, serving bool) string {
	parts := make([]string, 0, 4)
	if r.Outcome != "" {
		parts = append(parts, r.Outcome)
	}
	if serving {
		parts = append(parts, "serving")
	}
	if r.Digest != "" {
		parts = append(parts, r.Digest)
	}
	if len(r.Images) > 0 {
		parts = append(parts, strings.Join(r.Images, ", "))
	}
	return strings.Join(parts, " · ")
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
