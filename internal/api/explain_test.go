package api

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/observation"
)

// ExplainService is assembly over internal/explain, which owns the causal
// machinery and is tested on its own. What these tests assert is the assembly:
// that the handler resolves the sources it has, hands them across intact, asks
// the log engine for the crash-loop window, and reports the sources it does not
// have as notes rather than as a failure.

// TestExplainStatesTheSourcesItLost is what remains of the acceptance case.
//
// The full one asserted that a CrashLoopBackOff caused by a missing environment
// variable came back with the variable AND the revision that introduced it, at
// high confidence. The revision half needed the recorded manifests of the last
// two revisions, and ADR-0027 decision 7 deleted the store that kept them, so
// the correlation is gone until issue #224 — as is the delivery phase.
//
// What must not be gone is the statement that they are missing. A diagnosis
// that quietly stopped consulting a source reads exactly like one that
// consulted it and found nothing.
func TestExplainStatesTheSourcesItLost(t *testing.T) {
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
	// The container's own output still names the variable, which is the half of
	// the acceptance case that survives.
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
	for _, want := range []string{"delivery phase", "no change was correlated", "#224"} {
		if !strings.Contains(notes, want) {
			t.Errorf("notes = %q, want %q stated", notes, want)
		}
	}
	if msg.GetRecentChange() != nil {
		t.Errorf("recent change = %+v, want none when no history could be read", msg.GetRecentChange())
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
	if engine.query.Namespace != "hello-development" || engine.query.Application != "web" {
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
