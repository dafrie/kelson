package api

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/diff"
	"github.com/dafrie/kelson/internal/observation"
)

func deployRequest() *kelsonv1alpha1.DeployRequest {
	return &kelsonv1alpha1.DeployRequest{
		Spec:        inlineSpec(projectDoc, map[string]string{"development": developmentDoc}),
		Environment: "development",
		Profile:     profileRef(),
	}
}

// eventKinds names the oneof arm of each streamed event, so the assertion reads
// as the deployment's story rather than as a struct comparison.
func eventKinds(t *testing.T, stream *connect.ServerStreamForClient[kelsonv1alpha1.DeployResponse]) ([]string, []*kelsonv1alpha1.DeployResponse) {
	t.Helper()
	var kinds []string
	var msgs []*kelsonv1alpha1.DeployResponse
	for stream.Receive() {
		msg := stream.Msg()
		msgs = append(msgs, msg)
		switch msg.GetEvent().(type) {
		case *kelsonv1alpha1.DeployResponse_Proposed_:
			kinds = append(kinds, "proposed")
		case *kelsonv1alpha1.DeployResponse_Committed_:
			kinds = append(kinds, "committed")
		case *kelsonv1alpha1.DeployResponse_Transition_:
			kinds = append(kinds, "transition")
		case *kelsonv1alpha1.DeployResponse_Settled_:
			kinds = append(kinds, "settled")
		default:
			kinds = append(kinds, "unknown")
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream: %v", err)
	}
	return kinds, msgs
}

// TestDeployStreamsToSettled is the #139 deploy smoke test: the event order is
// the deployment's story and never varies — Proposed, Committed, one Transition
// per phase change, exactly one Settled last.
func TestDeployStreamsToSettled(t *testing.T) {
	adapter := newFakeAdapter("direct")
	adapter.statuses = []delivery.Status{
		{Phase: delivery.PhaseReconciling, Revision: "rev-00000001"},
		{Phase: delivery.PhaseApplied, Revision: "rev-00000001"},
		{Phase: delivery.PhaseHealthy, Revision: "rev-00000001"},
	}
	connector, targets := connectorFor(adapter, nil, nil)
	c := serve(t, Options{Delivery: connector, PollInterval: time.Millisecond})

	stream, err := c.deploy.Deploy(context.Background(), connect.NewRequest(deployRequest()))
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	kinds, msgs := eventKinds(t, stream)

	if len(kinds) < 4 {
		t.Fatalf("events = %v, want at least proposed, committed, a transition and settled", kinds)
	}
	if kinds[0] != "proposed" || kinds[1] != "committed" || kinds[len(kinds)-1] != "settled" {
		t.Fatalf("event order = %v", kinds)
	}
	for _, k := range kinds[2 : len(kinds)-1] {
		if k != "transition" {
			t.Fatalf("event order = %v: only transitions may sit between committed and settled", kinds)
		}
	}

	proposed := msgs[0].GetProposed()
	if proposed.GetProject() != "hello" || proposed.GetEnvironment() != "development" {
		t.Errorf("proposed = %+v, want hello/development", proposed)
	}
	if proposed.GetResources() == 0 {
		t.Error("proposed reported no resources")
	}
	if proposed.GetMode() != "direct" {
		t.Errorf("mode = %q, want direct (the environment's resolved mode)", proposed.GetMode())
	}
	if len(proposed.GetManifests()) != 0 {
		t.Error("a live deploy shipped manifests on Proposed; those are the dry-run answer")
	}
	if rev := msgs[1].GetCommitted().GetRevision(); rev != "rev-00000001" {
		t.Errorf("committed revision = %q", rev)
	}
	if a := msgs[1].GetCommitted().GetAdapter(); a != "direct" {
		t.Errorf("committed adapter = %q", a)
	}

	settled := msgs[len(msgs)-1].GetSettled()
	if settled.GetFinal().GetPhase() != string(delivery.PhaseHealthy) {
		t.Errorf("final phase = %q, want Healthy", settled.GetFinal().GetPhase())
	}
	if settled.GetFinal().GetAnswer() != "live" {
		t.Errorf("final answer = %q, want live", settled.GetFinal().GetAnswer())
	}
	if settled.GetError() != nil {
		t.Errorf("a healthy deployment carried an error: %v", settled.GetError())
	}
	if got := adapter.callLog(); got[0] != "apply" {
		t.Errorf("adapter calls = %v, want apply first", got)
	}
	if len(*targets) != 1 || (*targets)[0].Mode != "direct" {
		t.Errorf("targets = %+v", *targets)
	}
}

// TestDeploySettledUnhealthy: an unhealthy deployment completes the stream
// cleanly and carries the structured error on Settled. "Not healthy" is an
// answer about the deployment, not a failure of the RPC reporting it.
//
// Rejected rather than Degraded because Degraded is deliberately not terminal
// (it can recover), so a degraded deployment settles on the progress timeout
// rather than on the observation.
func TestDeploySettledUnhealthy(t *testing.T) {
	adapter := newFakeAdapter("direct")
	adapter.statuses = []delivery.Status{
		{Phase: delivery.PhaseRejected, Revision: "rev-00000001", Cause: "kubernetes: admission webhook denied the request"},
	}
	connector, _ := connectorFor(adapter, nil, nil)
	c := serve(t, Options{Delivery: connector, PollInterval: time.Millisecond})

	stream, err := c.deploy.Deploy(context.Background(), connect.NewRequest(deployRequest()))
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	kinds, msgs := eventKinds(t, stream)
	if kinds[len(kinds)-1] != "settled" {
		t.Fatalf("event order = %v", kinds)
	}
	settled := msgs[len(msgs)-1].GetSettled()
	if settled.GetFinal().GetPhase() != string(delivery.PhaseRejected) {
		t.Errorf("final phase = %q, want Rejected", settled.GetFinal().GetPhase())
	}
	if settled.GetFinal().GetCause().GetMessage() == "" {
		t.Error("a rejected deployment named no cause")
	}
	if settled.GetError() == nil {
		t.Fatal("a rejected deployment carried no error")
	}
	if got := settled.GetError().GetCode(); got != string(delivery.ErrApplyFailed) {
		t.Errorf("error code = %q, want %q", got, delivery.ErrApplyFailed)
	}
}

// TestDeployDryRunRender: the RENDER rung ships the manifests on Proposed and
// ends the stream without an adapter ever being built.
func TestDeployDryRunRender(t *testing.T) {
	adapter := newFakeAdapter("direct")
	connector, _ := connectorFor(adapter, nil, nil)
	c := serve(t, Options{Delivery: connector})

	req := deployRequest()
	req.DryRun = kelsonv1alpha1.DryRun_DRY_RUN_RENDER
	stream, err := c.deploy.Deploy(context.Background(), connect.NewRequest(req))
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	kinds, msgs := eventKinds(t, stream)
	if len(kinds) != 1 || kinds[0] != "proposed" {
		t.Fatalf("events = %v, want exactly one proposed", kinds)
	}
	if len(msgs[0].GetProposed().GetManifests()) == 0 {
		t.Error("a render dry run shipped no manifests")
	}
	if len(adapter.callLog()) != 0 {
		t.Errorf("a render dry run called the adapter: %v", adapter.callLog())
	}
}

// TestDeployDryRunServer: the SERVER rung reports the API server's own verdict
// as a Transition and stops there. A preview that finds an enforce-mode
// violation reports Rejected — the same blocker `kelson diff` exits 3 on — so
// the caller learns the deploy would not land without attempting it.
func TestDeployDryRunServer(t *testing.T) {
	adapter := newFakeAdapter("direct")
	connector, _ := connectorFor(adapter, nil, nil)
	preview := &fakePreview{diff: &diff.Diff{
		Level:      diff.LevelServer,
		Resources:  []diff.ResourceDiff{{Kind: "Deployment", Name: "web", Op: diff.OpModified}},
		Violations: []diff.PolicyViolation{{Engine: "kyverno", Policy: "require-limits", Enforcement: diff.EnforcementEnforce}},
		Summary:    diff.Summary{Modified: 1, MaxRisk: diff.RiskDisruptive},
	}}
	c := serve(t, Options{Delivery: connector, Preview: previewConnector(preview)})

	req := deployRequest()
	req.DryRun = kelsonv1alpha1.DryRun_DRY_RUN_SERVER
	stream, err := c.deploy.Deploy(context.Background(), connect.NewRequest(req))
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	kinds, msgs := eventKinds(t, stream)
	if len(kinds) != 2 || kinds[0] != "proposed" || kinds[1] != "transition" {
		t.Fatalf("events = %v, want proposed then one preview transition", kinds)
	}
	transition := msgs[1].GetTransition()
	if transition.GetPhase() != string(delivery.PhaseRejected) {
		t.Errorf("phase = %q, want Rejected for a blocked preview", transition.GetPhase())
	}
	if transition.GetCause().GetComponent() != "kelson" {
		t.Errorf("cause = %+v", transition.GetCause())
	}
	if len(adapter.callLog()) != 0 {
		t.Errorf("a server dry run called the adapter: %v", adapter.callLog())
	}
	if len(preview.sets) != 1 || preview.sets[0].Project != "hello" {
		t.Errorf("the preview engine received %+v", preview.sets)
	}
}

// TestStatusReportsPhaseAndVerdicts: the phase says whether the change arrived,
// the verdicts say whether it works. Status never reports one without the other
// (issue #53).
func TestStatusReportsPhaseAndVerdicts(t *testing.T) {
	adapter := newFakeAdapter("direct")
	adapter.statuses = []delivery.Status{{
		Phase:    delivery.PhaseApplied,
		Revision: "rev-00000003",
		Detail:   map[string]string{"resources": "4", "live": "3"},
	}}
	health := fakeEvaluator{"web": {
		Healthy:     false,
		Code:        observation.CodeCrashLoopBackOff,
		Resource:    "Deployment/hello-development/web",
		Reason:      "back-off restarting failed container",
		Remediation: "read the container logs",
	}}
	connector, _ := connectorFor(adapter, nil, health)
	c := serve(t, Options{Delivery: connector})

	res, err := c.deploy.Status(context.Background(), connect.NewRequest(&kelsonv1alpha1.StatusRequest{
		Spec:        inlineSpec(projectDoc, map[string]string{"development": developmentDoc}),
		Environment: "development",
		Profile:     profileRef(),
	}))
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if res.Msg.GetPhase() != string(delivery.PhaseApplied) {
		t.Errorf("phase = %q", res.Msg.GetPhase())
	}
	if res.Msg.GetDetail()["live"] != "3" {
		t.Errorf("detail = %v", res.Msg.GetDetail())
	}
	verdicts := res.Msg.GetVerdicts()
	if len(verdicts) != 1 {
		t.Fatalf("verdicts = %d, want one per rendered Deployment", len(verdicts))
	}
	if verdicts[0].GetCode() != string(observation.CodeCrashLoopBackOff) {
		t.Errorf("verdict code = %q", verdicts[0].GetCode())
	}
	if !verdicts[0].GetDegraded() || verdicts[0].GetHealthy() {
		t.Errorf("verdict = %+v, want degraded", verdicts[0])
	}
	if res.Msg.GetNamespace() != "hello-development" {
		t.Errorf("namespace = %q, want the model's default for this pair", res.Msg.GetNamespace())
	}
}

// TestStatusReportsResolvedNamespace: the namespace on the response is the one
// the spec resolved to, not the `<project>-<environment>` default a client
// could have reconstructed. An Environment that sets spec.namespace is exactly
// the case a guess gets wrong, which is why #161 put the answer on the wire.
func TestStatusReportsResolvedNamespace(t *testing.T) {
	const overriddenDoc = `apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata:
  name: development
spec:
  project: hello
  namespace: hello-dev-sandbox
  routing:
    domainSuffix: dev.acme.run
`
	adapter := newFakeAdapter("direct")
	adapter.statuses = []delivery.Status{{Phase: delivery.PhaseHealthy, Revision: "rev-00000001"}}
	connector, targets := connectorFor(adapter, nil, nil)
	c := serve(t, Options{Delivery: connector})

	res, err := c.deploy.Status(context.Background(), connect.NewRequest(&kelsonv1alpha1.StatusRequest{
		Spec:        inlineSpec(projectDoc, map[string]string{"development": overriddenDoc}),
		Environment: "development",
		Profile:     profileRef(),
	}))
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if res.Msg.GetNamespace() != "hello-dev-sandbox" {
		t.Errorf("namespace = %q, want the Environment's spec.namespace", res.Msg.GetNamespace())
	}
	if len(*targets) != 1 || (*targets)[0].Namespace != "hello-dev-sandbox" {
		t.Errorf("targets = %+v, want the same namespace the delivery target carries", *targets)
	}
}

// TestHistoryDoesNotRender: history needs the project, environment and mode and
// nothing else. The request carries no profile, so a spec whose service
// declares a domain would fail to render (#140) — reading the past must not
// fail for a reason about the present.
func TestHistoryDoesNotRender(t *testing.T) {
	adapter := newFakeAdapter("direct")
	adapter.history = []delivery.Entry{
		{Revision: "rev-00000002", SpecHash: "sha256:b", CommittedAt: "2026-08-13T10:00:00Z"},
		{Revision: "rev-00000001", SpecHash: "sha256:a", CommittedAt: "2026-08-12T10:00:00Z"},
	}
	connector, _ := connectorFor(adapter, nil, nil)
	c := serve(t, Options{Delivery: connector})

	res, err := c.deploy.History(context.Background(), connect.NewRequest(&kelsonv1alpha1.HistoryRequest{
		Spec:        inlineSpec(projectDoc, map[string]string{"development": developmentDoc}),
		Environment: "development",
	}))
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	entries := res.Msg.GetEntries()
	if len(entries) != 2 || entries[0].GetRevision() != "rev-00000002" {
		t.Fatalf("entries = %+v, want the recorded revisions newest first", entries)
	}
}

// TestRollbackPreviewsFirst: the irreversibility preview is computed and
// streamed BEFORE anything is applied, and dry_run=RENDER stops there (#38, #55).
func TestRollbackPreviewsFirst(t *testing.T) {
	adapter := newFakeAdapter("direct")
	adapter.history = []delivery.Entry{
		{Revision: "rev-00000002"},
		{Revision: "rev-00000001"},
	}
	connector, _ := connectorFor(adapter, nil, nil)
	c := serve(t, Options{Delivery: connector})

	req := &kelsonv1alpha1.RollbackRequest{
		Spec:        inlineSpec(projectDoc, map[string]string{"development": developmentDoc}),
		Environment: "development",
		Profile:     profileRef(),
		DryRun:      kelsonv1alpha1.DryRun_DRY_RUN_RENDER,
	}
	stream, err := c.deploy.Rollback(context.Background(), connect.NewRequest(req))
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	var events []string
	var previews []*kelsonv1alpha1.RollbackResponse_Preview
	for stream.Receive() {
		switch e := stream.Msg().GetEvent().(type) {
		case *kelsonv1alpha1.RollbackResponse_Preview_:
			events = append(events, "preview")
			previews = append(previews, e.Preview)
		case *kelsonv1alpha1.RollbackResponse_Committed_:
			events = append(events, "committed")
		case *kelsonv1alpha1.RollbackResponse_Settled_:
			events = append(events, "settled")
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if len(events) != 1 || events[0] != "preview" {
		t.Fatalf("events = %v, want exactly one preview", events)
	}
	// No --to means "undo the last deploy": history is newest first, so the
	// target is index 1.
	if previews[0].GetToRevision() != "rev-00000001" {
		t.Errorf("to_revision = %q, want the entry before the current one", previews[0].GetToRevision())
	}
	for _, call := range adapter.callLog() {
		if call == "rollback" {
			t.Fatal("a render dry run applied the rollback")
		}
	}
}

// TestRollbackApplies: preview, then the apply outcome.
func TestRollbackApplies(t *testing.T) {
	adapter := newFakeAdapter("direct")
	adapter.history = []delivery.Entry{{Revision: "rev-00000002"}, {Revision: "rev-00000001"}}
	connector, _ := connectorFor(adapter, nil, nil)
	c := serve(t, Options{Delivery: connector})

	stream, err := c.deploy.Rollback(context.Background(), connect.NewRequest(&kelsonv1alpha1.RollbackRequest{
		Spec:        inlineSpec(projectDoc, map[string]string{"development": developmentDoc}),
		Environment: "development",
		Profile:     profileRef(),
		ToRevision:  "rev-00000001",
	}))
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	var events []string
	for stream.Receive() {
		switch stream.Msg().GetEvent().(type) {
		case *kelsonv1alpha1.RollbackResponse_Preview_:
			events = append(events, "preview")
		case *kelsonv1alpha1.RollbackResponse_Committed_:
			events = append(events, "committed")
		case *kelsonv1alpha1.RollbackResponse_Settled_:
			events = append(events, "settled")
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if len(events) != 3 || events[0] != "preview" || events[1] != "committed" || events[2] != "settled" {
		t.Fatalf("events = %v, want preview, committed, settled", events)
	}
	if len(adapter.rolledTo) != 1 || adapter.rolledTo[0].Revision != "rev-00000001" {
		t.Errorf("rolled to %+v", adapter.rolledTo)
	}
}

// TestRollbackRefusedByCapabilities: an adapter that cannot roll back says so
// up front rather than failing at apply time (issue #32).
func TestRollbackRefusedByCapabilities(t *testing.T) {
	adapter := newFakeAdapter("direct")
	adapter.caps = delivery.Capabilities{SupportsRollback: false}
	connector, _ := connectorFor(adapter, nil, nil)
	c := serve(t, Options{Delivery: connector})

	stream, err := c.deploy.Rollback(context.Background(), connect.NewRequest(&kelsonv1alpha1.RollbackRequest{
		Spec:        inlineSpec(projectDoc, map[string]string{"development": developmentDoc}),
		Environment: "development",
		Profile:     profileRef(),
	}))
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	for stream.Receive() { //nolint:revive // draining the stream is how the error surfaces
	}
	if connect.CodeOf(stream.Err()) != connect.CodeFailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition (err %v)", connect.CodeOf(stream.Err()), stream.Err())
	}
}
