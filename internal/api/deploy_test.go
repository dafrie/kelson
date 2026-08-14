package api

import (
	"context"
	"strings"
	"testing"

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

// TestDeployApplyRungIsGated: the rung that changes the cluster refuses with
// the tracked code, AFTER the Proposed event.
//
// The order is the point. A caller streaming this RPC learns what would have
// been deployed and then learns kelson cannot deploy it, which is strictly more
// than a bare refusal and is the shape a dry run already has.
func TestDeployApplyRungIsGated(t *testing.T) {
	connector, _ := connectorFor(nil)
	c := serve(t, Options{Delivery: connector})

	req := deployRequest()
	req.DryRun = kelsonv1alpha1.DryRun_DRY_RUN_NONE
	stream, err := c.deploy.Deploy(context.Background(), connect.NewRequest(req))
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	var kinds []string
	for stream.Receive() {
		if stream.Msg().GetProposed() != nil {
			kinds = append(kinds, "proposed")
		}
	}
	err = stream.Err()
	if connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("code = %v, want unimplemented (%v)", connect.CodeOf(err), err)
	}
	if !hasCode(detailCodes(err), string(delivery.ErrNotImplemented)) {
		t.Errorf("details = %v, want %s so an agent can branch on it", detailCodes(err), delivery.ErrNotImplemented)
	}
	if !strings.Contains(err.Error(), "#224") {
		t.Errorf("the refusal must name the tracking issue: %v", err)
	}
	if len(kinds) != 1 {
		t.Errorf("events before the refusal = %v, want the Proposed event", kinds)
	}
}

// TestRollbackAndHistoryAreGated: both refuse with the tracked code, and
// Rollback refuses even its preview rung — the preview compares two recorded
// revisions, and answering "no findings" because there is nothing to compare
// would be the exact failure the preview exists to prevent.
func TestRollbackAndHistoryAreGated(t *testing.T) {
	connector, _ := connectorFor(nil)
	c := serve(t, Options{Delivery: connector})

	for _, dry := range []kelsonv1alpha1.DryRun{
		kelsonv1alpha1.DryRun_DRY_RUN_RENDER,
		kelsonv1alpha1.DryRun_DRY_RUN_NONE,
	} {
		stream, err := c.deploy.Rollback(context.Background(), connect.NewRequest(&kelsonv1alpha1.RollbackRequest{
			Spec:        inlineSpec(projectDoc, map[string]string{"development": developmentDoc}),
			Environment: "development",
			Profile:     profileRef(),
			DryRun:      dry,
		}))
		if err != nil {
			t.Fatalf("Rollback: %v", err)
		}
		for stream.Receive() {
			t.Errorf("a gated rollback streamed an event: %+v", stream.Msg())
		}
		assertGated(t, stream.Err())
	}

	_, err := c.deploy.History(context.Background(), connect.NewRequest(&kelsonv1alpha1.HistoryRequest{
		Spec:        inlineSpec(projectDoc, map[string]string{"development": developmentDoc}),
		Environment: "development",
	}))
	assertGated(t, err)
}

func assertGated(t *testing.T, err error) {
	t.Helper()
	if connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("code = %v, want unimplemented (%v)", connect.CodeOf(err), err)
	}
	if !hasCode(detailCodes(err), string(delivery.ErrNotImplemented)) {
		t.Errorf("details = %v, want %s", detailCodes(err), delivery.ErrNotImplemented)
	}
	if !strings.Contains(err.Error(), "#224") {
		t.Errorf("the refusal must name the tracking issue: %v", err)
	}
}

// TestDeployDryRunRender: the RENDER rung ships the manifests on Proposed and
// ends the stream. It never needed an adapter, which is why it survives the
// deletion of them (ADR-0028) untouched.
func TestDeployDryRunRender(t *testing.T) {
	connector, _ := connectorFor(nil)
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
}

// TestDeployDryRunServer: the SERVER rung reports the API server's own verdict
// as a Transition and stops there. A preview that finds an enforce-mode
// violation reports Rejected — the same blocker `kelson diff` exits 3 on — so
// the caller learns the deploy would not land without attempting it.
func TestDeployDryRunServer(t *testing.T) {
	connector, _ := connectorFor(nil)
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
	if len(preview.sets) != 1 || preview.sets[0].Project != "hello" {
		t.Errorf("the preview engine received %+v", preview.sets)
	}
}

// TestStatusReportsVerdicts: the verdicts say whether the workloads work.
//
// The phase — whether the change arrived — is deliberately empty and stays
// empty: the adapter that reported it is deleted, and ADR-0027 decision 6 says
// where it comes back from (Environment.status, issue #224). Empty is a value a
// client can branch on; a guessed "Healthy" is not.
func TestStatusReportsVerdicts(t *testing.T) {
	connector, targets := connectorFor(fakeEvaluator{"web": {
		Healthy:  false,
		Code:     observation.CodeCrashLoopBackOff,
		Resource: "Deployment/hello-development/web",
		Reason:   "back-off restarting failed container",
	}})
	c := serve(t, Options{Delivery: connector})

	res, err := c.deploy.Status(context.Background(), connect.NewRequest(&kelsonv1alpha1.StatusRequest{
		Spec:        inlineSpec(projectDoc, map[string]string{"development": developmentDoc}),
		Environment: "development",
		Profile:     profileRef(),
	}))
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if res.Msg.GetPhase() != "" || res.Msg.GetRevision() != "" {
		t.Errorf("phase/revision = %q/%q, want empty until they have a source again (#224)",
			res.Msg.GetPhase(), res.Msg.GetRevision())
	}
	if !strings.Contains(res.Msg.GetCause(), "#224") {
		t.Errorf("cause = %q, want the missing half named rather than left blank", res.Msg.GetCause())
	}
	verdicts := res.Msg.GetVerdicts()
	if len(verdicts) != 1 || verdicts[0].GetCode() != string(observation.CodeCrashLoopBackOff) {
		t.Fatalf("verdicts = %+v, want the crash-loop verdict", verdicts)
	}
	if !verdicts[0].GetDegraded() {
		t.Errorf("verdict = %+v, want degraded", verdicts[0])
	}
	// The namespace the target resolved to, so a client addressing this
	// environment's workloads reads it rather than reconstructing it (#161).
	if res.Msg.GetNamespace() != "hello-development" {
		t.Errorf("namespace = %q", res.Msg.GetNamespace())
	}
	if len(*targets) != 1 || (*targets)[0].Project != "hello" {
		t.Errorf("targets = %+v", *targets)
	}
}

// TestStatusVerdictsIncludeSecretSync is issue #80's acceptance criterion on
// the wire: a failed ExternalSecret sync arrives as an ordinary degraded
// verdict with the controller's cause named, ahead of the workloads it broke.
// Nothing downstream — `kelson status`, the event stream, diagnose_application,
// the UI — learns a new shape for it.
func TestStatusVerdictsIncludeSecretSync(t *testing.T) {
	set := delivery.ManifestSet{Manifests: []delivery.Manifest{
		{Kind: "ExternalSecret", Name: "payments", Namespace: "hello-development"},
		{Kind: "Deployment", Name: "web", Namespace: "hello-development"},
	}}
	health := syncingEvaluator{
		fakeEvaluator: fakeEvaluator{},
		sync: map[string]observation.Verdict{"payments": {
			Healthy:     false,
			Code:        observation.CodeSecretSyncFailed,
			Resource:    "external-secrets.io/ExternalSecret/hello-development/payments",
			Reason:      `SecretSyncedError: cannot get secret "payments": permission denied`,
			Remediation: "check the SecretStore authenticates",
		}},
	}

	verdicts, err := workloadVerdicts(context.Background(), &Plane{Health: health}, set, "fallback")
	if err != nil {
		t.Fatalf("workloadVerdicts: %v", err)
	}
	if len(verdicts) != 2 {
		t.Fatalf("verdicts = %d, want one per ExternalSecret and Deployment", len(verdicts))
	}
	first := verdicts[0]
	if first.GetCode() != string(observation.CodeSecretSyncFailed) {
		t.Fatalf("the sync verdict must come first — the cause above the symptom; got %q", first.GetCode())
	}
	if !first.GetDegraded() || first.GetHealthy() {
		t.Errorf("verdict = %+v, want degraded", first)
	}
	if !strings.Contains(first.GetMessage(), "permission denied") {
		t.Errorf("message = %q, want the controller's own cause", first.GetMessage())
	}
	if first.GetRemediation() == "" {
		t.Errorf("a failure must state the fix")
	}
}

// TestStatusVerdictsWithoutASyncEvaluator: the capability is optional, so a
// health source that only classifies workloads reports exactly what it did
// before rather than failing the readback.
func TestStatusVerdictsWithoutASyncEvaluator(t *testing.T) {
	set := delivery.ManifestSet{Manifests: []delivery.Manifest{
		{Kind: "ExternalSecret", Name: "payments", Namespace: "hello-development"},
		{Kind: "Deployment", Name: "web", Namespace: "hello-development"},
	}}
	verdicts, err := workloadVerdicts(context.Background(), &Plane{Health: fakeEvaluator{}}, set, "fallback")
	if err != nil {
		t.Fatalf("workloadVerdicts: %v", err)
	}
	if len(verdicts) != 1 || verdicts[0].GetResource() != "Deployment/hello-development/web" {
		t.Fatalf("verdicts = %+v, want the Deployment alone", verdicts)
	}
}
