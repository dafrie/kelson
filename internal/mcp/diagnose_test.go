package mcp

import (
	"fmt"
	"testing"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
)

// TestDiagnoseComposesTheWholeAnswer: the flagship composition (ADR-0008 §1).
// One call returns the verdict, the status, the workload verdicts with the
// server's remediation, a log window around the failure, recent history and a
// spec summary — and the log query it issues is the crash-loop one, because the
// server called that workload unhealthy.
func TestDiagnoseComposesTheWholeAnswer(t *testing.T) {
	var query *kelsonv1alpha1.QueryLogsRequest
	h := start(t, &fakeServer{
		status: func(*kelsonv1alpha1.StatusRequest) (*kelsonv1alpha1.StatusResponse, error) {
			return crashLoopStatus(), nil
		},
		queryLogs: func(req *kelsonv1alpha1.QueryLogsRequest) (*kelsonv1alpha1.QueryLogsResponse, error) {
			query = req
			return logLines(3), nil
		},
		history: func(*kelsonv1alpha1.HistoryRequest) (*kelsonv1alpha1.HistoryResponse, error) {
			return &kelsonv1alpha1.HistoryResponse{Entries: []*kelsonv1alpha1.HistoryEntry{
				{Revision: "43", CommittedAt: "2026-08-13T09:20:00Z", Message: "deploy hello production"},
				{Revision: "42", CommittedAt: "2026-08-12T17:02:00Z", Message: "deploy hello production"},
			}}, nil
		},
		getSpec: func(*kelsonv1alpha1.GetSpecRequest) (*kelsonv1alpha1.GetSpecResponse, error) {
			return &kelsonv1alpha1.GetSpecResponse{Spec: &kelsonv1alpha1.Spec{
				Project:   "hello",
				Documents: &kelsonv1alpha1.SpecDocuments{Project: []byte(projectDoc)},
			}}, nil
		},
	})

	out := h.call(t, "diagnose_application", map[string]any{"project": "hello", "environment": "production"})

	mustContain(t, out,
		"hello/production: Degraded — 1 of 2 workloads failing, first is web (crash-loop-back-off)",
		"namespace  hello-production",
		"FAIL Deployment/hello-production/web",
		"fix: check the container command and the logs before termination",
		"ok   Deployment/hello-production/worker",
		"LOGS (web, at termination",
		"line 0",
		"HISTORY (last 5)",
		"43",
		"SPEC",
		"web",
		"ghcr.io/acme/hello:1.4.2",
		"port 8080",
		"worker",
	)
	// The spec summary is a summary: the authored YAML never comes back.
	mustNotContain(t, out, "apiVersion", "kind: Project")

	if query.GetAround() == nil || !query.GetAround().GetAtTermination() {
		t.Errorf("a failing workload must be diagnosed with Around.AtTermination, got %+v", query)
	}
	if query.GetAround().GetLines() != diagnoseLogLines {
		t.Errorf("log window = %d lines, want %d", query.GetAround().GetLines(), diagnoseLogLines)
	}
	if query.GetSelector().GetNamespace() != "hello-production" {
		t.Errorf("the log selector's namespace must come from Status, got %q", query.GetSelector().GetNamespace())
	}
	if query.GetSelector().GetApplication() != "web" {
		t.Errorf("the log selector must name the failing workload, got %q", query.GetSelector().GetApplication())
	}
}

// TestDiagnoseHealthyUsesTail: a healthy environment has no termination to look
// before, so the window is a plain tail. Which window is a relay of the
// server's verdict, never this tool's own classification.
func TestDiagnoseHealthyUsesTail(t *testing.T) {
	var query *kelsonv1alpha1.QueryLogsRequest
	h := start(t, &fakeServer{
		status: func(*kelsonv1alpha1.StatusRequest) (*kelsonv1alpha1.StatusResponse, error) {
			return healthyStatus(), nil
		},
		queryLogs: func(req *kelsonv1alpha1.QueryLogsRequest) (*kelsonv1alpha1.QueryLogsResponse, error) {
			query = req
			return logLines(1), nil
		},
		history: func(*kelsonv1alpha1.HistoryRequest) (*kelsonv1alpha1.HistoryResponse, error) {
			return &kelsonv1alpha1.HistoryResponse{}, nil
		},
		getSpec: func(*kelsonv1alpha1.GetSpecRequest) (*kelsonv1alpha1.GetSpecResponse, error) {
			return &kelsonv1alpha1.GetSpecResponse{Spec: &kelsonv1alpha1.Spec{
				Project:   "hello",
				Documents: &kelsonv1alpha1.SpecDocuments{Project: []byte(projectDoc)},
			}}, nil
		},
	})

	out := h.call(t, "diagnose_application", map[string]any{"project": "hello", "environment": "production"})
	mustContain(t, out, "1 of 1 workloads healthy", "no recorded revisions")
	if query.GetTail() != diagnoseLogLines || query.GetAround() != nil {
		t.Errorf("a healthy workload must be read with Tail, got %+v", query)
	}
}

// TestDiagnoseTruncatesLogs: the window is capped here as well as asked for,
// because the cap is this package's promise. The newest lines survive — they
// are the ones that explain a failure.
func TestDiagnoseTruncatesLogs(t *testing.T) {
	h := start(t, &fakeServer{
		status: func(*kelsonv1alpha1.StatusRequest) (*kelsonv1alpha1.StatusResponse, error) {
			return crashLoopStatus(), nil
		},
		queryLogs: func(*kelsonv1alpha1.QueryLogsRequest) (*kelsonv1alpha1.QueryLogsResponse, error) {
			return logLines(diagnoseLogLines + 20), nil
		},
		history: func(*kelsonv1alpha1.HistoryRequest) (*kelsonv1alpha1.HistoryResponse, error) {
			return &kelsonv1alpha1.HistoryResponse{}, nil
		},
		getSpec: func(*kelsonv1alpha1.GetSpecRequest) (*kelsonv1alpha1.GetSpecResponse, error) {
			return &kelsonv1alpha1.GetSpecResponse{Spec: &kelsonv1alpha1.Spec{
				Documents: &kelsonv1alpha1.SpecDocuments{Project: []byte(projectDoc)},
			}}, nil
		},
	})

	out := h.call(t, "diagnose_application", map[string]any{"project": "hello", "environment": "production"})
	mustContain(t, out, "… 20 more earlier lines (truncated)", fmt.Sprintf("line %d", diagnoseLogLines+19))
	mustNotContain(t, out, "line 19 ")
}

// TestDiagnoseDegradesWithoutTakingTheDiagnosisDown: status is the spine, and
// everything after it is additive. A server started without a log engine still
// answers the question it can answer.
func TestDiagnoseDegradesWithoutTakingTheDiagnosisDown(t *testing.T) {
	h := start(t, &fakeServer{
		status: func(*kelsonv1alpha1.StatusRequest) (*kelsonv1alpha1.StatusResponse, error) {
			return crashLoopStatus(), nil
		},
	})

	out := h.call(t, "diagnose_application", map[string]any{"project": "hello", "environment": "production"})
	mustContain(t, out,
		"1 of 2 workloads failing",
		"LOGS",
		"unavailable",
		"HISTORY",
		"SPEC",
	)
}

// TestDiagnoseStatusFailureIsTheAnswer: without status there is no diagnosis,
// and the structured error rides through with its code and remediation so an
// agent branches on the code rather than on prose.
func TestDiagnoseStatusFailureIsTheAnswer(t *testing.T) {
	h := start(t, &fakeServer{
		status: func(*kelsonv1alpha1.StatusRequest) (*kelsonv1alpha1.StatusResponse, error) {
			err := connect.NewError(connect.CodeNotFound, fmt.Errorf("serverstate: no spec is stored for hello"))
			detail, derr := connect.NewErrorDetail(&kelsonv1alpha1.Error{
				Code:        "store/not-found",
				Resource:    "spec/hello",
				Message:     "no spec is stored",
				Remediation: "store the project with put_spec first",
				DocsUrl:     "https://kelson.dev/model/errors",
			})
			if derr != nil {
				return nil, derr
			}
			err.AddDetail(detail)
			return nil, err
		},
	})

	out := h.callErr(t, "diagnose_application", map[string]any{"project": "hello", "environment": "production"})
	mustContain(t, out,
		"kelson.v1alpha1.DeployService.Status failed",
		"code: store/not-found",
		"resource: spec/hello",
		"remediation: store the project with put_spec first",
		"docs_url: https://kelson.dev/model/errors",
	)
}
