package api

import (
	"context"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/observation"
)

// ExplainService is assembly over internal/explain, which owns the causal
// machinery and is tested on its own. What these tests assert is the assembly:
// that the handler resolves all four sources, hands them across intact, asks
// the log engine for the crash-loop window, and reports a source it could not
// read as a note rather than as a failure.

// recordedRevision renders the one Deployment of the fixture with the given env
// entries, in the shape the rendered-history store keeps.
func recordedRevision(image string, env string) []delivery.Manifest {
	yaml := "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web\n  namespace: hello-development\n" +
		"spec:\n  template:\n    spec:\n      containers:\n        - name: web\n          image: " + image + "\n"
	if env != "" {
		yaml += "          env:\n" + env
	}
	return []delivery.Manifest{{
		APIVersion: "apps/v1", Kind: "Deployment", Name: "web", Namespace: "hello-development", YAML: []byte(yaml),
	}}
}

const databaseURLEnv = "            - name: DATABASE_URL\n              value: postgres://db/app\n"

// TestExplainAnswersTheAcceptanceCase: a CrashLoopBackOff caused by a missing
// environment variable comes back over the wire with the variable and the
// revision that introduced it, at high confidence, with the evidence attached.
func TestExplainAnswersTheAcceptanceCase(t *testing.T) {
	adapter := newFakeAdapter("direct")
	adapter.statuses = []delivery.Status{{Phase: delivery.PhaseDegraded, Revision: "rev-00000043"}}
	adapter.history = []delivery.Entry{
		{Revision: "rev-00000043", CommittedAt: "2026-08-13T09:20:00Z", Message: "deploy hello development"},
		{Revision: "rev-00000042", CommittedAt: "2026-08-12T17:02:00Z", Message: "deploy hello development"},
	}
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
	recorded := &fakeRecorded{revisions: map[string][]delivery.Manifest{
		"rev-00000043": recordedRevision("ghcr.io/acme/hello:1.4.3", ""),
		"rev-00000042": recordedRevision("ghcr.io/acme/hello:1.4.2", databaseURLEnv),
	}}
	connector, _ := connectorFor(adapter, recorded, health)
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

	if msg.GetSubject().GetNamespace() != "hello-development" || msg.GetSubject().GetRevision() != "rev-00000043" {
		t.Errorf("subject = %+v, want the resolved namespace and the live revision", msg.GetSubject())
	}
	if msg.GetPhase() != string(delivery.PhaseDegraded) {
		t.Errorf("phase = %q, want the adapter's own", msg.GetPhase())
	}

	var cause *kelsonv1alpha1.ExplainCause
	for _, c := range msg.GetCauses() {
		if c.GetCode() == "explain/missing-env-var" {
			cause = c
		}
	}
	if cause == nil {
		t.Fatalf("no missing-env-var cause in %+v", msg.GetCauses())
	}
	if cause.GetConfidence() != "high" {
		t.Errorf("confidence = %q, want high: the change and the container's output agree", cause.GetConfidence())
	}
	if !strings.Contains(cause.GetMessage(), "DATABASE_URL") {
		t.Errorf("message does not name the variable: %q", cause.GetMessage())
	}
	if cause.GetIntroducedBy().GetRevision() != "rev-00000043" {
		t.Errorf("introduced_by = %+v, want rev-00000043", cause.GetIntroducedBy())
	}
	if len(cause.GetEvidence()) == 0 {
		t.Errorf("a high-confidence cause with no evidence is exactly what #77 forbids")
	}

	change := msg.GetRecentChange()
	if change == nil || len(change.GetEnv()) != 1 || change.GetEnv()[0].GetName() != "DATABASE_URL" {
		t.Fatalf("recent change = %+v, want the one env-var removal", change)
	}
	if change.GetEnv()[0].GetKind() != "removed" || change.GetPrevious().GetRevision() != "rev-00000042" {
		t.Errorf("env change = %+v, previous = %+v", change.GetEnv()[0], change.GetPrevious())
	}
	if len(change.GetImages()) != 1 || change.GetImages()[0].GetAfter() != "ghcr.io/acme/hello:1.4.3" {
		t.Errorf("image changes = %+v, want the tag bump", change.GetImages())
	}
}

// TestExplainAsksForTheCrashLoopLogWindow: the window is the one the failure
// needs — the lines before the container terminated — not a plain tail, and it
// is bounded by the capability's own cap rather than by a number invented here.
func TestExplainAsksForTheCrashLoopLogWindow(t *testing.T) {
	adapter := newFakeAdapter("direct")
	adapter.statuses = []delivery.Status{{Phase: delivery.PhaseDegraded, Revision: "rev-00000001"}}
	health := fakeEvaluator{"web": {
		Healthy:  false,
		Code:     observation.CodeCrashLoopBackOff,
		Reason:   "CrashLoopBackOff",
		Resource: "Deployment/hello-development/web",
	}}
	engine := &fakeLogEngine{result: observation.Result{Lines: []observation.Line{
		{Pod: "web-1", Container: "web", Message: "fatal: STRIPE_API_KEY is not set"},
	}}}
	connector, _ := connectorFor(adapter, nil, health)
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

// TestExplainDegradesToNotes: a server with no log engine, a plane with no
// recorded manifests and an adapter whose history fails still answers. Every
// missing source is stated, which is what keeps a partial answer from reading
// as a complete one.
func TestExplainDegradesToNotes(t *testing.T) {
	adapter := newFakeAdapter("direct")
	adapter.statuses = []delivery.Status{{Phase: delivery.PhaseDegraded, Revision: "rev-00000001"}}
	adapter.historyErr = errors.New("the history store is unreachable")
	health := fakeEvaluator{"web": {
		Healthy:  false,
		Code:     observation.CodeCrashLoopBackOff,
		Reason:   "CrashLoopBackOff",
		Resource: "Deployment/hello-development/web",
	}}
	connector, _ := connectorFor(adapter, nil, health)
	c := serve(t, Options{Delivery: connector})

	res, err := c.explain.Explain(context.Background(), connect.NewRequest(&kelsonv1alpha1.ExplainRequest{
		Spec:        inlineSpec(projectDoc, map[string]string{"development": developmentDoc}),
		Environment: "development",
		Profile:     profileRef(),
	}))
	if err != nil {
		t.Fatalf("a diagnosis must not fail because a second source did: %v", err)
	}
	if len(res.Msg.GetCauses()) == 0 {
		t.Errorf("the crash loop is still a cause without history or logs")
	}
	notes := strings.Join(res.Msg.GetNotes(), "\n")
	if !strings.Contains(notes, "history store is unreachable") {
		t.Errorf("notes = %q, want the adapter's own history failure stated", notes)
	}
	if res.Msg.GetRecentChange() != nil {
		t.Errorf("recent change = %+v, want none when no history could be read", res.Msg.GetRecentChange())
	}
}

// TestExplainReportsAHealthyEnvironment: the RPC is not a failure detector. A
// healthy environment gets a summary saying so and no causes.
func TestExplainReportsAHealthyEnvironment(t *testing.T) {
	adapter := newFakeAdapter("direct")
	adapter.statuses = []delivery.Status{{Phase: delivery.PhaseHealthy, Revision: "rev-00000007"}}
	connector, _ := connectorFor(adapter, nil, fakeEvaluator{})
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
	adapter := newFakeAdapter("direct")
	connector, _ := connectorFor(adapter, nil, nil)
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
