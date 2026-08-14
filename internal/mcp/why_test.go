package mcp

import (
	"strings"
	"testing"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
)

// The WHY section is composition: the causes are ExplainService's and this
// package renders them. What these tests assert is that the rendering keeps
// every load-bearing field — the code, the confidence, the evidence and the
// revision — and that a server which cannot answer costs one line rather than
// the diagnosis.

func explanation() *kelsonv1alpha1.ExplainResponse {
	return &kelsonv1alpha1.ExplainResponse{
		Subject: &kelsonv1alpha1.ExplainSubject{
			Project: "hello", Environment: "production", Namespace: "hello-production", Revision: "rev-00000043",
		},
		Phase:   "Degraded",
		Summary: "hello/production is Degraded: web crash-loops because the environment variable DATABASE_URL is missing (high confidence)",
		Causes: []*kelsonv1alpha1.ExplainCause{{
			Code:       "explain/missing-env-var",
			Message:    "web crash-loops because the environment variable DATABASE_URL is missing: the live revision removed it, and the container's own output names it",
			Confidence: "high",
			Resource:   "Deployment/hello-production/web",
			Evidence: []*kelsonv1alpha1.ExplainEvidence{
				{Kind: "log", Source: "Deployment/hello-production/web", Detail: "KeyError: 'DATABASE_URL'"},
				{Kind: "revision", Source: "recorded manifests", Detail: "DATABASE_URL removed on Deployment/web container web: was postgres://db/app"},
			},
			Remediation:  "restore DATABASE_URL in the component's env, or roll back",
			IntroducedBy: &kelsonv1alpha1.RevisionRef{Revision: "rev-00000043", CommittedAt: "2026-08-13T09:20:00Z", Author: "ada"},
		}},
		RecentChange: &kelsonv1alpha1.RecentChange{
			Revision: &kelsonv1alpha1.RevisionRef{Revision: "rev-00000043"},
			Previous: &kelsonv1alpha1.RevisionRef{Revision: "rev-00000042"},
			Summary:  "rev-00000042 → rev-00000043: 1 image change, 1 environment-variable change",
			Env: []*kelsonv1alpha1.EnvChange{
				{Workload: "Deployment/web", Container: "web", Name: "DATABASE_URL", Kind: "removed", Before: "postgres://db/app"},
			},
			Images: []*kelsonv1alpha1.ImageChange{
				{Workload: "Deployment/web", Container: "web", Before: "ghcr.io/acme/hello:1.4.2", After: "ghcr.io/acme/hello:1.4.3"},
			},
		},
		Notes:     []string{"the log window for worker could not be read: no log engine configured"},
		Truncated: []string{"2 of 8 causes dropped (the cap is 6)"},
	}
}

// TestDiagnoseRendersTheServersCauses: the causal answer of #77 reaches the
// agent whole — code, confidence, evidence, the revision that introduced the
// change, and what the server could not read.
func TestDiagnoseRendersTheServersCauses(t *testing.T) {
	var asked *kelsonv1alpha1.ExplainRequest
	h := start(t, &fakeServer{
		status: func(*kelsonv1alpha1.StatusRequest) (*kelsonv1alpha1.StatusResponse, error) {
			return crashLoopStatus(), nil
		},
		explain: func(req *kelsonv1alpha1.ExplainRequest) (*kelsonv1alpha1.ExplainResponse, error) {
			asked = req
			return explanation(), nil
		},
		queryLogs: func(*kelsonv1alpha1.QueryLogsRequest) (*kelsonv1alpha1.QueryLogsResponse, error) {
			return logLines(1), nil
		},
	})

	out := h.call(t, "diagnose_application", map[string]any{"project": "hello", "environment": "production"})

	mustContain(t, out,
		"WHY (the server's causes, with confidence and evidence)",
		"1. [high] explain/missing-env-var",
		"resource: Deployment/hello-production/web",
		"DATABASE_URL",
		"introduced by: rev-00000043  2026-08-13T09:20:00Z  by ada",
		"log (Deployment/hello-production/web): KeyError: 'DATABASE_URL'",
		"revision (recorded manifests): DATABASE_URL removed",
		"fix: restore DATABASE_URL",
		"recent change: rev-00000042 → rev-00000043",
		"image  Deployment/web/web  ghcr.io/acme/hello:1.4.2 → ghcr.io/acme/hello:1.4.3",
		"env    Deployment/web/web  DATABASE_URL removed (was postgres://db/app)",
		"could not be read:",
		"no log engine configured",
		"truncated:",
	)
	if asked.GetEnvironment() != "production" || asked.GetSpec().GetProject() != "hello" {
		t.Errorf("Explain was asked %+v, want the stored project and the named environment", asked)
	}

	// The sections that existed before the capability are unaffected.
	mustContain(t, out, "STATUS", "WORKLOADS", "LOGS (web", "HISTORY", "SPEC", "SECRETS", "VERSION SKEW")
	// WHY leads: it is the answer the rest of the report is evidence for.
	if strings.Index(out, "WHY") > strings.Index(out, "STATUS") {
		t.Errorf("the causal section must come before the raw status:\n%s", out)
	}
}

// TestDiagnoseWithoutExplainStillDiagnoses: a server too old to serve
// ExplainService, or one whose delivery plane cannot answer, costs one line.
// The diagnosis this tool gave before the capability existed is still there.
func TestDiagnoseWithoutExplainStillDiagnoses(t *testing.T) {
	h := start(t, &fakeServer{
		status: func(*kelsonv1alpha1.StatusRequest) (*kelsonv1alpha1.StatusResponse, error) {
			return crashLoopStatus(), nil
		},
		queryLogs: func(*kelsonv1alpha1.QueryLogsRequest) (*kelsonv1alpha1.QueryLogsResponse, error) {
			return logLines(1), nil
		},
	})

	out := h.call(t, "diagnose_application", map[string]any{"project": "hello", "environment": "production"})
	mustContain(t, out,
		"WHY (the server's causes, with confidence and evidence)",
		"unavailable — unimplemented:",
		"FAIL Deployment/hello-production/web",
		"LOGS (web",
	)
}

// TestDiagnoseReportsNoCauseFound: an explanation with no causes says so rather
// than printing an empty section, which reads as "nothing is wrong".
func TestDiagnoseReportsNoCauseFound(t *testing.T) {
	h := start(t, &fakeServer{
		status: func(*kelsonv1alpha1.StatusRequest) (*kelsonv1alpha1.StatusResponse, error) {
			return crashLoopStatus(), nil
		},
		explain: func(*kelsonv1alpha1.ExplainRequest) (*kelsonv1alpha1.ExplainResponse, error) {
			return &kelsonv1alpha1.ExplainResponse{Summary: "hello/production is Healthy with 2 workloads observed and no cause found"}, nil
		},
		queryLogs: func(*kelsonv1alpha1.QueryLogsRequest) (*kelsonv1alpha1.QueryLogsResponse, error) {
			return logLines(1), nil
		},
	})

	out := h.call(t, "diagnose_application", map[string]any{"project": "hello", "environment": "production"})
	mustContain(t, out, "no cause found:")
}
