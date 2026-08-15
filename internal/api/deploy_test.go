package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/controlstore"
	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/delivery/statemachine"
	"github.com/dafrie/kelson/internal/diff"
	"github.com/dafrie/kelson/internal/model"
	"github.com/dafrie/kelson/internal/observation"
)

// stagingDoc is the second environment of the same project: what a deploy of
// development must leave alone.
const stagingDoc = `apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata:
  name: staging
spec:
  project: hello
`

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

// TestDeployWritesTheSpecAndStreamsStatus is the R2 mapping end to end
// (issue #225): the write IS the deploy, and the stream is the controller's
// status read back.
func TestDeployWritesTheSpecAndStreamsStatus(t *testing.T) {
	connector, _ := connectorFor(nil)
	specs := newFakeSpecStore()
	envs := newFakeEnvironments(healthyEnvironment("hello", "development", "3-9f0a1b2c"))
	c := serve(t, Options{Delivery: connector, Specs: specs, Environments: envs})

	req := deployRequest()
	req.DryRun = kelsonv1alpha1.DryRun_DRY_RUN_NONE
	stream, err := c.deploy.Deploy(context.Background(), connect.NewRequest(req))
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	kinds, msgs := eventKinds(t, stream)
	want := []string{"proposed", "committed", "transition", "settled"}
	if len(kinds) != len(want) {
		t.Fatalf("events = %v, want %v", kinds, want)
	}
	for i, k := range want {
		if kinds[i] != k {
			t.Fatalf("events = %v, want %v", kinds, want)
		}
	}

	// The deploy wrote the spec: that write is what bumps the Environment's
	// generation and starts the reconcile this stream then reported.
	stored, err := specs.Get(context.Background(), "hello")
	if err != nil {
		t.Fatalf("the deploy stored nothing: %v", err)
	}
	if _, ok := stored.Documents.Environments["development"]; !ok {
		t.Errorf("the stored document set has no development environment: %v", stored.Environments)
	}

	committed := msgs[1].GetCommitted()
	if committed.GetRevision() != "3-9f0a1b2c" {
		t.Errorf("committed revision = %q, want the revision the status names", committed.GetRevision())
	}
	if committed.GetAdapter() != "flux" {
		t.Errorf("adapter = %q, want flux: there is one reconciliation path", committed.GetAdapter())
	}
	settled := msgs[3].GetSettled()
	if settled.GetError() != nil {
		t.Errorf("a healthy deployment settled with an error: %v", settled.GetError())
	}
	if got := settled.GetFinal().GetPhase(); got != string(delivery.PhaseHealthy) {
		t.Errorf("final phase = %q, want %s", got, delivery.PhaseHealthy)
	}
	if got := settled.GetFinal().GetAnswer(); got != string(statemachine.AnswerLive) {
		t.Errorf("final answer = %q, want %s", got, statemachine.AnswerLive)
	}
}

// TestDeployStreamsOneEventPerVisibleChange: a watch fires for every write to
// the object, including the ones that changed nothing a client can see, so the
// stream is the *transitions* and not the writes.
func TestDeployStreamsOneEventPerVisibleChange(t *testing.T) {
	connector, _ := connectorFor(nil)
	committed := healthyEnvironment("hello", "development", "3-9f0a1b2c")
	committed.Phase = string(delivery.PhaseCommitted)
	committed.Conditions[1] = controlstore.Condition{
		Type: "Progressing", Status: "True", Reason: "Reconciling",
		Message: "waiting for Flux to reconcile revision 3-9f0a1b2c", ObservedGeneration: 1,
	}
	reconciling := committed
	reconciling.Phase = string(delivery.PhaseReconciling)

	envs := newFakeEnvironments(committed)
	// The same state again (a status write that moved nothing visible), then a
	// real change, then the settle.
	envs.queue("hello", "development", committed, reconciling,
		healthyEnvironment("hello", "development", "3-9f0a1b2c"))
	c := serve(t, Options{Delivery: connector, Specs: newFakeSpecStore(), Environments: envs})

	req := deployRequest()
	req.DryRun = kelsonv1alpha1.DryRun_DRY_RUN_NONE
	stream, err := c.deploy.Deploy(context.Background(), connect.NewRequest(req))
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	kinds, msgs := eventKinds(t, stream)
	want := []string{"proposed", "committed", "transition", "transition", "transition", "settled"}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v, want %v (one per visible change, not one per write)", kinds, want)
	}
	phases := []string{
		msgs[2].GetTransition().GetPhase(),
		msgs[3].GetTransition().GetPhase(),
		msgs[4].GetTransition().GetPhase(),
	}
	wantPhases := []string{string(delivery.PhaseCommitted), string(delivery.PhaseReconciling), string(delivery.PhaseHealthy)}
	if strings.Join(phases, ",") != strings.Join(wantPhases, ",") {
		t.Errorf("phases = %v, want %v", phases, wantPhases)
	}
}

// TestDeployStreamsAFailureAsAnAnswer: an environment the controller refused
// settles with its errors, and the stream itself completes cleanly. A refused
// deployment is an answer, not a transport failure — and the answer keeps the
// model taxonomy's own code, because an author whose spec is wrong must read
// that and not "delivery/apply-failed".
func TestDeployStreamsAFailureAsAnAnswer(t *testing.T) {
	connector, _ := connectorFor(nil)
	invalid := healthyEnvironment("hello", "development", "")
	invalid.Phase, invalid.Revision = "", ""
	invalid.History = nil
	invalid.Conditions[0] = controlstore.Condition{
		Type: "Ready", Status: "False", Reason: "SpecInvalid",
		Message: "1 error", ObservedGeneration: 1,
	}
	invalid.ValidationErrors = model.Errors{{
		Code:     model.ErrUnknownField,
		Resource: "Environment/development",
		Field:    "$.spec.nope",
		Message:  "unknown field",
	}}
	c := serve(t, Options{Delivery: connector, Specs: newFakeSpecStore(),
		Environments: newFakeEnvironments(invalid)})

	req := deployRequest()
	req.DryRun = kelsonv1alpha1.DryRun_DRY_RUN_NONE
	stream, err := c.deploy.Deploy(context.Background(), connect.NewRequest(req))
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	kinds, msgs := eventKinds(t, stream)
	if kinds[len(kinds)-1] != "settled" {
		t.Fatalf("events = %v, want a settled event last", kinds)
	}
	settled := msgs[len(msgs)-1].GetSettled()
	if settled.GetError() == nil {
		t.Fatal("a refused deployment settled with no error")
	}
	if got := settled.GetError().GetCode(); got != string(model.ErrUnknownField) {
		t.Errorf("settled error code = %q, want the spec's own %q", got, model.ErrUnknownField)
	}
	if settled.GetFinal().GetAnswer() != string(statemachine.AnswerWaiting) {
		t.Errorf("answer = %q", settled.GetFinal().GetAnswer())
	}
}

// TestDeployStopsAtItsBudget: a deployment that never settles is reported as
// stuck rather than streamed forever, and stuck is the state machine's own
// word for it — the phase it is stuck in is the diagnosis.
func TestDeployStopsAtItsBudget(t *testing.T) {
	connector, _ := connectorFor(nil)
	inflight := healthyEnvironment("hello", "development", "3-9f0a1b2c")
	inflight.Phase = string(delivery.PhaseCommitted)
	inflight.Conditions[1] = controlstore.Condition{
		Type: "Progressing", Status: "True", Reason: "Reconciling",
		Message: "waiting for Flux to reconcile revision 3-9f0a1b2c", ObservedGeneration: 1,
	}
	c := serve(t, Options{Delivery: connector, Specs: newFakeSpecStore(),
		Environments: newFakeEnvironments(inflight), DeployTimeout: 20 * time.Millisecond})

	req := deployRequest()
	req.DryRun = kelsonv1alpha1.DryRun_DRY_RUN_NONE
	stream, err := c.deploy.Deploy(context.Background(), connect.NewRequest(req))
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	_, msgs := eventKinds(t, stream)
	settled := msgs[len(msgs)-1].GetSettled()
	if settled == nil {
		t.Fatalf("the stream ended without settling: %v", msgs)
	}
	if !settled.GetFinal().GetStuck() || settled.GetFinal().GetAnswer() != string(statemachine.AnswerStuck) {
		t.Errorf("final = %+v, want stuck", settled.GetFinal())
	}
	if got := settled.GetError().GetCode(); got != string(delivery.ErrNotWatched) {
		t.Errorf("error code = %q, want %s: committed and never picked up", got, delivery.ErrNotWatched)
	}
}

// TestDeployLeavesTheOtherEnvironmentsAlone: a deploy names one environment,
// and a document set nobody sent is not a deletion request. PutSpec replaces a
// document set; this must not.
func TestDeployLeavesTheOtherEnvironmentsAlone(t *testing.T) {
	connector, _ := connectorFor(nil)
	specs := newFakeSpecStore()
	if _, err := specs.Put(context.Background(), "hello", controlstore.Documents{
		Project: []byte(projectDoc),
		Environments: map[string][]byte{
			"development": []byte(developmentDoc),
			"staging":     []byte(stagingDoc),
		},
	}, controlstore.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	c := serve(t, Options{Delivery: connector, Specs: specs,
		Environments: newFakeEnvironments(healthyEnvironment("hello", "development", "3-9f0a1b2c"))})

	req := deployRequest()
	req.DryRun = kelsonv1alpha1.DryRun_DRY_RUN_NONE
	stream, err := c.deploy.Deploy(context.Background(), connect.NewRequest(req))
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	eventKinds(t, stream)

	stored, err := specs.Get(context.Background(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := stored.Documents.Environments["staging"]; !ok {
		t.Fatalf("deploying development deleted staging: %v", stored.Environments)
	}
}

// TestDeployWritesTheImageOverride: --image stands in for spec.image, and under
// the spine the render happens in the controller — so an override that was not
// written would report one image and run another.
func TestDeployWritesTheImageOverride(t *testing.T) {
	connector, _ := connectorFor(nil)
	specs := &recordingSpecStore{fakeSpecStore: newFakeSpecStore()}
	c := serve(t, Options{Delivery: connector, Specs: specs,
		Environments: newFakeEnvironments(healthyEnvironment("hello", "development", "3-9f0a1b2c"))})

	req := deployRequest()
	req.DryRun = kelsonv1alpha1.DryRun_DRY_RUN_NONE
	req.Image = "ghcr.io/acme/hello:2.0.0"
	stream, err := c.deploy.Deploy(context.Background(), connect.NewRequest(req))
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	eventKinds(t, stream)
	if len(specs.puts) != 1 || specs.puts[0].Image != "ghcr.io/acme/hello:2.0.0" {
		t.Fatalf("the store received %+v, want the image override carried into the write", specs.puts)
	}
}

// TestDeployWithoutTheStatusSeamNamesIt: a partially-wired server refuses by
// naming the seam it was started without — never with the #224 gate, which is
// a statement about kelson that is no longer true.
func TestDeployWithoutTheStatusSeamNamesIt(t *testing.T) {
	connector, _ := connectorFor(nil)
	c := serve(t, Options{Delivery: connector, Specs: newFakeSpecStore()})

	req := deployRequest()
	req.DryRun = kelsonv1alpha1.DryRun_DRY_RUN_NONE
	stream, err := c.deploy.Deploy(context.Background(), connect.NewRequest(req))
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	for stream.Receive() {
	}
	err = stream.Err()
	if connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("code = %v, want unimplemented (%v)", connect.CodeOf(err), err)
	}
	if !strings.Contains(err.Error(), "Environment status reader") {
		t.Errorf("the refusal must name the missing seam: %v", err)
	}
}

// --- rollback ---------------------------------------------------------------

// rolledBack is the status a controller writes once it has honoured the
// annotation: the pinned revision is serving and the environment deliberately
// does not track its spec.
func rolledBack(st controlstore.EnvironmentState, annotations map[string]string) controlstore.EnvironmentState {
	target := annotations[annotationRollbackTo]
	st.RollbackRevision, st.Revision = target, target
	st.Conditions = []controlstore.Condition{
		{Type: "Ready", Status: "True", Reason: "RolledBack",
			Message: "serving revision " + target, ObservedGeneration: st.Generation},
		{Type: "Progressing", Status: "False", Reason: "RollbackPinned",
			Message: "pinned to revision " + target, ObservedGeneration: st.Generation},
	}
	return st
}

// twoRevisions is an environment that has published twice and is serving the
// newer one — the shape every rollback question is asked against.
func twoRevisions() controlstore.EnvironmentState {
	st := healthyEnvironment("hello", "development", "4-b2c3d4e5")
	st.History = append(st.History, controlstore.Revision{
		Revision: "3-9f0a1b2c",
		Digest:   "sha256:deadbeef",
		Images:   []string{"ghcr.io/acme/hello:1.4.1"},
		Outcome:  string(delivery.PhaseHealthy),
	})
	return st
}

func rollbackRequest(to string) *kelsonv1alpha1.RollbackRequest {
	return &kelsonv1alpha1.RollbackRequest{
		Spec:        inlineSpec(projectDoc, map[string]string{"development": developmentDoc}),
		Environment: "development",
		Profile:     profileRef(),
		ToRevision:  to,
		DryRun:      kelsonv1alpha1.DryRun_DRY_RUN_NONE,
	}
}

func rollbackEvents(t *testing.T, stream *connect.ServerStreamForClient[kelsonv1alpha1.RollbackResponse]) ([]string, []*kelsonv1alpha1.RollbackResponse) {
	t.Helper()
	var kinds []string
	var msgs []*kelsonv1alpha1.RollbackResponse
	for stream.Receive() {
		msg := stream.Msg()
		msgs = append(msgs, msg)
		switch {
		case msg.GetPreview() != nil:
			kinds = append(kinds, "preview")
		case msg.GetCommitted() != nil:
			kinds = append(kinds, "committed")
		case msg.GetSettled() != nil:
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

// TestRollbackPatchesTheAnnotation: the whole of what a rollback does to the
// cluster is one annotation (ADR-0028 decision 5), and the stream reports the
// controller acting on it.
func TestRollbackPatchesTheAnnotation(t *testing.T) {
	envs := newFakeEnvironments(twoRevisions())
	envs.controller = rolledBack
	c := serve(t, Options{Specs: newFakeSpecStore(), Environments: envs})

	stream, err := c.deploy.Rollback(context.Background(), connect.NewRequest(rollbackRequest("3-9f0a1b2c")))
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	kinds, msgs := rollbackEvents(t, stream)
	want := []string{"preview", "committed", "settled"}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v, want %v", kinds, want)
	}

	if len(envs.annotated) != 1 {
		t.Fatalf("annotations written = %+v, want one", envs.annotated)
	}
	if got := envs.annotated[0].Annotations[annotationRollbackTo]; got != "3-9f0a1b2c" {
		t.Errorf("%s = %q, want the requested revision", annotationRollbackTo, got)
	}
	preview := msgs[0].GetPreview()
	if preview.GetToRevision() != "3-9f0a1b2c" {
		t.Errorf("preview target = %q", preview.GetToRevision())
	}
	// The preview says what it cannot say. "No findings" and "kelson did not
	// look" are different facts and must not read the same.
	if len(preview.GetFindings()) != 1 || preview.GetFindings()[0].GetCause() != "rollback/preview-unavailable" {
		t.Errorf("findings = %+v, want the unavailable-comparison finding", preview.GetFindings())
	}
	if len(preview.GetDiffJson()) != 0 {
		t.Errorf("the preview invented a diff: %s", preview.GetDiffJson())
	}
	committed := msgs[1].GetCommitted()
	if committed.GetRestoredRevision() != "3-9f0a1b2c" {
		t.Errorf("restored revision = %q", committed.GetRestoredRevision())
	}
	// A rollback publishes nothing and records no new history entry, so there
	// is no revision it was "recorded as".
	if committed.GetAsRevision() != "" {
		t.Errorf("as_revision = %q, want empty: a rollback prepends no history entry", committed.GetAsRevision())
	}
	if msgs[2].GetSettled().GetError() != nil {
		t.Errorf("a successful rollback settled with an error: %v", msgs[2].GetSettled().GetError())
	}
}

// TestRollbackDefaultsToThePreviousRevision: an empty to_revision is "the one
// before the one running".
func TestRollbackDefaultsToThePreviousRevision(t *testing.T) {
	envs := newFakeEnvironments(twoRevisions())
	envs.controller = rolledBack
	c := serve(t, Options{Specs: newFakeSpecStore(), Environments: envs})

	stream, err := c.deploy.Rollback(context.Background(), connect.NewRequest(rollbackRequest("")))
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	_, msgs := rollbackEvents(t, stream)
	if got := msgs[0].GetPreview().GetToRevision(); got != "3-9f0a1b2c" {
		t.Fatalf("target = %q, want the previous revision", got)
	}
}

// TestRollbackDryRunPreviewsOnly: RENDER stops after the preview and writes
// nothing, for the API as for the CLI.
func TestRollbackDryRunPreviewsOnly(t *testing.T) {
	envs := newFakeEnvironments(twoRevisions())
	c := serve(t, Options{Specs: newFakeSpecStore(), Environments: envs})

	req := rollbackRequest("3-9f0a1b2c")
	req.DryRun = kelsonv1alpha1.DryRun_DRY_RUN_RENDER
	stream, err := c.deploy.Rollback(context.Background(), connect.NewRequest(req))
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	kinds, _ := rollbackEvents(t, stream)
	if len(kinds) != 1 || kinds[0] != "preview" {
		t.Fatalf("events = %v, want the preview alone", kinds)
	}
	if len(envs.annotated) != 0 {
		t.Fatalf("a dry run wrote %+v", envs.annotated)
	}
}

// TestRollbackRefusesATargetTheControllerWouldRefuse: the check happens here,
// against the same bounded window the controller checks, so a typo is the
// caller's mistake and not an environment left carrying a pin nobody can
// honour.
func TestRollbackRefusesATargetTheControllerWouldRefuse(t *testing.T) {
	cases := map[string]string{
		"not a revision":     "v1.2.3",
		"outside the mirror": "9-aaaaaaaa",
	}
	for name, target := range cases {
		t.Run(name, func(t *testing.T) {
			envs := newFakeEnvironments(twoRevisions())
			c := serve(t, Options{Specs: newFakeSpecStore(), Environments: envs})

			stream, err := c.deploy.Rollback(context.Background(), connect.NewRequest(rollbackRequest(target)))
			if err != nil {
				t.Fatalf("Rollback: %v", err)
			}
			for stream.Receive() {
				t.Errorf("a refused rollback streamed an event: %+v", stream.Msg())
			}
			if got := connect.CodeOf(stream.Err()); got != connect.CodeInvalidArgument {
				t.Fatalf("code = %v, want InvalidArgument (%v)", got, stream.Err())
			}
			if len(envs.annotated) != 0 {
				t.Fatalf("a refused rollback wrote %+v", envs.annotated)
			}
		})
	}
}

// --- history ----------------------------------------------------------------

// TestHistoryReadsTheStatusMirror: history is `status.history[]`, newest first,
// and never a local journal (ADR-0028 decision 4).
func TestHistoryReadsTheStatusMirror(t *testing.T) {
	c := serve(t, Options{Specs: newFakeSpecStore(), Environments: newFakeEnvironments(twoRevisions())})

	res, err := c.deploy.History(context.Background(), connect.NewRequest(&kelsonv1alpha1.HistoryRequest{
		Spec:        inlineSpec(projectDoc, map[string]string{"development": developmentDoc}),
		Environment: "development",
	}))
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	entries := res.Msg.GetEntries()
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want the two published revisions", len(entries))
	}
	if entries[0].GetRevision() != "4-b2c3d4e5" {
		t.Errorf("entries are not newest-first: %+v", entries)
	}
	// The outcome, the digest and the images have no field of their own on the
	// wire, so the message is where they travel. An entry that dropped them
	// would make the history a list of tags.
	if !strings.Contains(entries[0].GetMessage(), "Healthy") ||
		!strings.Contains(entries[0].GetMessage(), "ghcr.io/acme/hello:1.4.2") {
		t.Errorf("message = %q, want the outcome and the images", entries[0].GetMessage())
	}
	if entries[0].GetAuthor() != "" {
		t.Errorf("author = %q, want empty: the spine records who deployed nothing", entries[0].GetAuthor())
	}
}

// TestHistoryOfAnEnvironmentKelsonNeverSaw is not an empty list: an empty
// history and an unknown environment are different facts, and a caller that
// could not tell them apart would conclude nothing was ever deployed.
func TestHistoryOfAnEnvironmentKelsonNeverSaw(t *testing.T) {
	c := serve(t, Options{Specs: newFakeSpecStore(), Environments: newFakeEnvironments()})

	_, err := c.deploy.History(context.Background(), connect.NewRequest(&kelsonv1alpha1.HistoryRequest{
		Spec:        inlineSpec(projectDoc, map[string]string{"development": developmentDoc}),
		Environment: "development",
	}))
	if got := connect.CodeOf(err); got != connect.CodeNotFound {
		t.Fatalf("code = %v, want NotFound (%v)", got, err)
	}
	if !hasCode(detailCodes(err), string(controlstore.ErrNotFound)) {
		t.Errorf("details = %v, want the store's own code", detailCodes(err))
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

// TestStatusReportsVerdicts: the two halves of a status, and neither
// substitutes for the other (issue #53). The verdicts say whether the workloads
// work; the phase and the revision — whether the change arrived — are read back
// from `Environment.status` (ADR-0027 decision 6).
func TestStatusReportsVerdicts(t *testing.T) {
	connector, targets := connectorFor(fakeEvaluator{"web": {
		Healthy:  false,
		Code:     observation.CodeCrashLoopBackOff,
		Resource: "Deployment/hello-development/web",
		Reason:   "back-off restarting failed container",
	}})
	c := serve(t, Options{Delivery: connector,
		Environments: newFakeEnvironments(healthyEnvironment("hello", "development", "3-9f0a1b2c"))})

	res, err := c.deploy.Status(context.Background(), connect.NewRequest(&kelsonv1alpha1.StatusRequest{
		Spec:        inlineSpec(projectDoc, map[string]string{"development": developmentDoc}),
		Environment: "development",
		Profile:     profileRef(),
	}))
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if res.Msg.GetPhase() != string(delivery.PhaseHealthy) || res.Msg.GetRevision() != "3-9f0a1b2c" {
		t.Errorf("phase/revision = %q/%q, want them read from Environment.status",
			res.Msg.GetPhase(), res.Msg.GetRevision())
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
// Nothing downstream — `kelson status`, the event stream, diagnose_component,
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
