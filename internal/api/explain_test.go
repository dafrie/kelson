package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/controlstore"
	"github.com/dafrie/kelson/internal/observation"
)

// ExplainService is assembly over internal/explain, which owns the causal
// machinery and is tested on its own. What these tests assert is the assembly:
// that the handler resolves the sources it has, hands them across intact, asks
// the log engine for the crash-loop window, correlates the revision from
// `Environment.status` (ADR-0028), and reports the sources it still does not
// have as notes rather than as a failure.

// TestExplainStatesTheEnvironmentReaderIsMissing is what remains of the old
// acceptance case's other half, now that issue #224 closed the revision-history
// gate: a server built without an [EnvironmentStore] — the one seam Explain
// still cannot degrade around silently — says so in a note rather than
// implying it looked at `status` and found nothing there.
func TestExplainStatesTheEnvironmentReaderIsMissing(t *testing.T) {
	health := fakeEvaluator{"web": {
		Healthy:     false,
		Code:        observation.CodeCrashLoopBackOff,
		Reason:      "CrashLoopBackOff",
		Resource:    "Deployment/hello-development/web",
		Remediation: "read the container logs",
		Containers: []observation.Container{{
			Name: "web", Pod: "web-6b8f-2xq", Code: observation.CodeCrashLoopBackOff, Reason: "CrashLoopBackOff",
			Logs: "starting hello\nKeyError: 'DATABASE_URL'\n",
		}},
	}}
	connector, _ := connectorFor(health)
	c := serve(t, Options{Delivery: connector})

	res, err := c.explain.Explain(context.Background(), connect.NewRequest(&kelsonv1alpha1.ExplainRequest{
		Spec:        inlineSpec(projectDoc, map[string]string{"development": developmentDoc}),
		Environment: "development",
		Profile:     profileRef(),
	}))
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	msg := res.Msg

	if msg.GetSubject().GetNamespace() != "hello-development" {
		t.Errorf("subject = %+v, want the resolved namespace", msg.GetSubject())
	}
	// The container's own output still names the variable regardless of the
	// missing store: verdict-derived causes never depended on it.
	var named bool
	for _, cause := range msg.GetCauses() {
		if strings.Contains(cause.GetMessage(), "DATABASE_URL") {
			named = true
		}
	}
	if !named {
		t.Errorf("no cause names the variable the container complained about: %+v", msg.GetCauses())
	}
	notes := strings.Join(msg.GetNotes(), "\n")
	if !strings.Contains(notes, "no Environment status reader is configured") {
		t.Errorf("notes = %q, want the missing seam stated", notes)
	}
	if strings.Contains(notes, "#224") {
		t.Errorf("notes = %q, still cite issue #224 as if the revision-history gate were unresolved", notes)
	}
	if msg.GetRecentChange() != nil {
		t.Errorf("recent change = %+v, want none when the status reader is not configured", msg.GetRecentChange())
	}
}

// TestExplainCorrelatesTheServingRevision is the acceptance case #224 closed:
// what is deployed now, when it last changed, and what its images changed
// from — read from `Environment.status.revision` and `status.history`
// (ADR-0028 decision 4), the same source `kelson history` reads.
func TestExplainCorrelatesTheServingRevision(t *testing.T) {
	connector, _ := connectorFor(fakeEvaluator{})
	st := twoRevisions()
	st.History[0].Timestamp = time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	st.History[1].Timestamp = time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)
	c := serve(t, Options{Delivery: connector, Environments: newFakeEnvironments(st)})

	res, err := c.explain.Explain(context.Background(), connect.NewRequest(&kelsonv1alpha1.ExplainRequest{
		Spec:        inlineSpec(projectDoc, map[string]string{"development": developmentDoc}),
		Environment: "development",
		Profile:     profileRef(),
	}))
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	msg := res.Msg

	if msg.GetSubject().GetRevision() != "4-b2c3d4e5" {
		t.Errorf("subject revision = %q, want the deployed revision", msg.GetSubject().GetRevision())
	}
	if msg.GetPhase() != st.Phase {
		t.Errorf("phase = %q, want %q (the delivery phase gate #224 also closed)", msg.GetPhase(), st.Phase)
	}
	change := msg.GetRecentChange()
	if change == nil {
		t.Fatalf("recent change = nil, want the revision correlation now that status.history is read")
	}
	if change.GetRevision().GetRevision() != "4-b2c3d4e5" {
		t.Errorf("recent change revision = %q, want the serving one", change.GetRevision().GetRevision())
	}
	if change.GetRevision().GetCommittedAt() != "2026-08-14T10:00:00Z" {
		t.Errorf("recent change committedAt = %q, want the recorded timestamp (when it changed)", change.GetRevision().GetCommittedAt())
	}
	if change.GetPrevious().GetRevision() != "3-9f0a1b2c" {
		t.Errorf("previous revision = %q, want the one before it", change.GetPrevious().GetRevision())
	}
	if change.GetPrevious().GetCommittedAt() != "2026-08-10T09:00:00Z" {
		t.Errorf("previous committedAt = %q", change.GetPrevious().GetCommittedAt())
	}
	summary := change.GetSummary()
	if !strings.Contains(summary, "1.4.1") || !strings.Contains(summary, "1.4.2") {
		t.Errorf("summary = %q, want the image change named (did a deploy cause this?)", summary)
	}
	notes := strings.Join(msg.GetNotes(), "\n")
	if !strings.Contains(notes, "recorded manifests") {
		t.Errorf("notes = %q, want the still-missing manifest-level diff stated", notes)
	}
	if strings.Contains(notes, "#224") {
		t.Errorf("notes = %q, still cite issue #224 as if the gate were unresolved", notes)
	}
}

// TestExplainReportsNoHistoryYet: an Environment that exists but has never
// published is not a degradation — it is the honest state of "nothing has
// been deployed through kelson here", and internal/explain's own empty-history
// note says so without this handler inventing a second one.
func TestExplainReportsNoHistoryYet(t *testing.T) {
	connector, _ := connectorFor(fakeEvaluator{})
	st := controlstore.EnvironmentState{Project: "hello", Environment: "development"}
	c := serve(t, Options{Delivery: connector, Environments: newFakeEnvironments(st)})

	res, err := c.explain.Explain(context.Background(), connect.NewRequest(&kelsonv1alpha1.ExplainRequest{
		Spec:        inlineSpec(projectDoc, map[string]string{"development": developmentDoc}),
		Environment: "development",
		Profile:     profileRef(),
	}))
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	msg := res.Msg
	if msg.GetRecentChange() != nil {
		t.Errorf("recent change = %+v, want none: nothing has been deployed", msg.GetRecentChange())
	}
	notes := strings.Join(msg.GetNotes(), "\n")
	if !strings.Contains(notes, "nothing has been deployed through kelson here") {
		t.Errorf("notes = %q, want the empty-history state stated plainly", notes)
	}
}

// TestExplainReportsARollbackPinned: while `kelson.dev/rollback-to` is
// honoured, `status.revision` names an older tag than the newest published one
// (ADR-0028 decision 5). The correlation must be about the revision that is
// actually serving — what the verdicts above are verdicts of — and must say
// plainly that the newest publish is not what is live, so it is not what
// caused what follows.
func TestExplainReportsARollbackPinned(t *testing.T) {
	connector, _ := connectorFor(fakeEvaluator{})
	st := rolledBack(twoRevisions(), map[string]string{annotationRollbackTo: "3-9f0a1b2c"})
	c := serve(t, Options{Delivery: connector, Environments: newFakeEnvironments(st)})

	res, err := c.explain.Explain(context.Background(), connect.NewRequest(&kelsonv1alpha1.ExplainRequest{
		Spec:        inlineSpec(projectDoc, map[string]string{"development": developmentDoc}),
		Environment: "development",
		Profile:     profileRef(),
	}))
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	msg := res.Msg

	if msg.GetSubject().GetRevision() != "3-9f0a1b2c" {
		t.Errorf("subject revision = %q, want the pinned (serving) revision", msg.GetSubject().GetRevision())
	}
	change := msg.GetRecentChange()
	if change == nil {
		t.Fatalf("recent change = nil, want a correlation for the serving revision")
	}
	if change.GetRevision().GetRevision() != "3-9f0a1b2c" {
		t.Errorf("recent change revision = %q, want the pinned one, not the newer unreleased publish", change.GetRevision().GetRevision())
	}
	if change.GetPrevious().GetRevision() != "4-b2c3d4e5" {
		t.Errorf("previous = %q, want the superseded publish", change.GetPrevious().GetRevision())
	}
	notes := strings.Join(msg.GetNotes(), "\n")
	if !strings.Contains(notes, "rollback is pinned") || !strings.Contains(notes, "not what caused what follows") {
		t.Errorf("notes = %q, want the rollback stated honestly", notes)
	}
}

// TestExplainReportsAStaleStatus: observedGeneration trailing generation means
// the controller has not caught up, and a reader must be told before trusting
// the revision below.
func TestExplainReportsAStaleStatus(t *testing.T) {
	connector, _ := connectorFor(fakeEvaluator{})
	st := healthyEnvironment("hello", "development", "3-9f0a1b2c")
	st.Generation, st.ObservedGeneration = 2, 1
	c := serve(t, Options{Delivery: connector, Environments: newFakeEnvironments(st)})

	res, err := c.explain.Explain(context.Background(), connect.NewRequest(&kelsonv1alpha1.ExplainRequest{
		Spec:        inlineSpec(projectDoc, map[string]string{"development": developmentDoc}),
		Environment: "development",
		Profile:     profileRef(),
	}))
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	notes := strings.Join(res.Msg.GetNotes(), "\n")
	if !strings.Contains(notes, "generation 1") || !strings.Contains(notes, "generation 2") {
		t.Errorf("notes = %q, want the staleness stated with both generations", notes)
	}
}

// TestExplainAsksForTheCrashLoopLogWindow: the window is the one the failure
// needs — the lines before the container terminated — not a plain tail, and it
// is bounded by the capability's own cap rather than by a number invented here.
func TestExplainAsksForTheCrashLoopLogWindow(t *testing.T) {
	health := fakeEvaluator{"web": {
		Healthy:  false,
		Code:     observation.CodeCrashLoopBackOff,
		Reason:   "CrashLoopBackOff",
		Resource: "Deployment/hello-development/web",
	}}
	engine := &fakeLogEngine{result: observation.Result{Lines: []observation.Line{
		{Pod: "web-1", Container: "web", Message: "fatal: STRIPE_API_KEY is not set"},
	}}}
	connector, _ := connectorFor(health)
	c := serve(t, Options{Delivery: connector, Logs: engine})

	res, err := c.explain.Explain(context.Background(), connect.NewRequest(&kelsonv1alpha1.ExplainRequest{
		Spec:        inlineSpec(projectDoc, map[string]string{"development": developmentDoc}),
		Environment: "development",
		Profile:     profileRef(),
	}))
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if engine.query.Around == nil || !engine.query.Around.AtTermination {
		t.Fatalf("log query = %+v, want the at-termination window", engine.query)
	}
	if engine.query.Namespace != "hello-development" || engine.query.Component != "web" {
		t.Errorf("log query selector = %+v, want the failing workload in the resolved namespace", engine.query)
	}
	// The line the engine returned is what named the variable, so it reached
	// the causes rather than being fetched and dropped.
	var named bool
	for _, c := range res.Msg.GetCauses() {
		if strings.Contains(c.GetMessage(), "STRIPE_API_KEY") {
			named = true
		}
	}
	if !named {
		t.Errorf("the log window was fetched but did not reach the causes: %+v", res.Msg.GetCauses())
	}
}

// TestExplainReportsAHealthyEnvironment: the RPC is not a failure detector. A
// healthy environment gets a summary saying so and no causes.
func TestExplainReportsAHealthyEnvironment(t *testing.T) {
	connector, _ := connectorFor(fakeEvaluator{})
	c := serve(t, Options{Delivery: connector})

	res, err := c.explain.Explain(context.Background(), connect.NewRequest(&kelsonv1alpha1.ExplainRequest{
		Spec:        inlineSpec(projectDoc, map[string]string{"development": developmentDoc}),
		Environment: "development",
		Profile:     profileRef(),
	}))
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(res.Msg.GetCauses()) != 0 {
		t.Errorf("causes = %+v, want none", res.Msg.GetCauses())
	}
	if !strings.Contains(res.Msg.GetSummary(), "no cause found") {
		t.Errorf("summary = %q", res.Msg.GetSummary())
	}
}

// TestExplainRejectsAnInvalidSpecAsAnAnswer is the plane's contract: a spec
// that does not resolve is a request failure with the structured error riding
// along, not a panic and not an empty explanation.
func TestExplainRejectsAnUnknownEnvironment(t *testing.T) {
	connector, _ := connectorFor(nil)
	c := serve(t, Options{Delivery: connector})

	_, err := c.explain.Explain(context.Background(), connect.NewRequest(&kelsonv1alpha1.ExplainRequest{
		Spec:        inlineSpec(projectDoc, map[string]string{"development": developmentDoc}),
		Environment: "staging",
		Profile:     profileRef(),
	}))
	if err == nil {
		t.Fatalf("an environment the spec does not declare must not resolve")
	}
}
