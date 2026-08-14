package mcp

import (
	"testing"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
)

// TestDeployDefaultsToRenderPreview: the default rung applies nothing, and the
// preview names the manifests and their sizes rather than returning their
// bodies. A tool whose default mutates is one an agent eventually fires by
// accident.
func TestDeployDefaultsToRenderPreview(t *testing.T) {
	var request *kelsonv1alpha1.DeployRequest
	h := start(t, &fakeServer{
		deploy: func(req *kelsonv1alpha1.DeployRequest, stream *connect.ServerStream[kelsonv1alpha1.DeployResponse]) error {
			request = req
			return stream.Send(&kelsonv1alpha1.DeployResponse{
				Event: &kelsonv1alpha1.DeployResponse_Proposed_{Proposed: &kelsonv1alpha1.DeployResponse_Proposed{
					Project: "hello", Environment: "production", Resources: 2, Mode: "direct",
					Manifests: []*kelsonv1alpha1.Manifest{
						{Kind: "Deployment", Name: "web", Namespace: "hello-production", Yaml: make([]byte, 2048)},
						{Kind: "Service", Name: "web", Namespace: "hello-production", Yaml: make([]byte, 300)},
					},
				}},
			})
		},
	})

	out := h.call(t, "deploy", map[string]any{"project": "hello", "environment": "production"})
	mustContain(t, out,
		"PREVIEW ONLY, nothing was applied",
		"idempotency_key: mcp-",
		"mode       direct",
		"resources  2",
		"MANIFESTS (2, bodies omitted)",
		"Deployment/hello-production/web",
		"2.0 kB",
		"Service/hello-production/web",
		"300 B",
	)
	if request.GetDryRun() != kelsonv1alpha1.DryRun_DRY_RUN_RENDER {
		t.Errorf("dry run = %v, want DRY_RUN_RENDER by default", request.GetDryRun())
	}
	if request.GetIdempotencyKey() == "" {
		t.Error("a mutating call must carry an idempotency key so a retry is not a second deployment")
	}
}

// TestDeploySettledOutcome: dry_run=none returns the settled answer, not a
// stream. The phases are the state machine's own words.
func TestDeploySettledOutcome(t *testing.T) {
	var request *kelsonv1alpha1.DeployRequest
	h := start(t, &fakeServer{
		deploy: func(req *kelsonv1alpha1.DeployRequest, stream *connect.ServerStream[kelsonv1alpha1.DeployResponse]) error {
			request = req
			events := []*kelsonv1alpha1.DeployResponse{
				{Event: &kelsonv1alpha1.DeployResponse_Proposed_{Proposed: &kelsonv1alpha1.DeployResponse_Proposed{Resources: 2, Mode: "direct"}}},
				{Event: &kelsonv1alpha1.DeployResponse_Committed_{Committed: &kelsonv1alpha1.DeployResponse_Committed{Revision: "44", Adapter: "direct"}}},
				{Event: &kelsonv1alpha1.DeployResponse_Transition_{Transition: &kelsonv1alpha1.DeployResponse_Transition{Phase: "Applied", Answer: "progressing"}}},
				{Event: &kelsonv1alpha1.DeployResponse_Settled_{Settled: &kelsonv1alpha1.DeployResponse_Settled{
					Final: &kelsonv1alpha1.DeployResponse_Transition{Phase: "Healthy", Answer: "live", ObservedRevision: "44"},
				}}},
			}
			for _, event := range events {
				if err := stream.Send(event); err != nil {
					return err
				}
			}
			return nil
		},
	})

	out := h.call(t, "deploy", map[string]any{
		"project": "hello", "environment": "production", "dry_run": "none", "idempotency_key": "agent-1",
	})
	mustContain(t, out,
		"SETTLED Healthy (live)",
		"idempotency_key: agent-1",
		"revision   44",
		"adapter    direct",
		"TRANSITIONS (1)",
		"Applied",
	)
	if request.GetDryRun() != kelsonv1alpha1.DryRun_DRY_RUN_NONE {
		t.Errorf("dry run = %v, want DRY_RUN_NONE", request.GetDryRun())
	}
	if request.GetIdempotencyKey() != "agent-1" {
		t.Errorf("idempotency key = %q, want the caller's own key back on the wire", request.GetIdempotencyKey())
	}
}

// TestDeploySettledUnhealthy: an unhealthy deployment completes the stream
// cleanly and is an answer, not a transport failure — including the stuck
// verdict, which is reported as the verdict it is rather than as an error.
func TestDeploySettledUnhealthy(t *testing.T) {
	h := start(t, &fakeServer{
		deploy: func(_ *kelsonv1alpha1.DeployRequest, stream *connect.ServerStream[kelsonv1alpha1.DeployResponse]) error {
			return stream.Send(&kelsonv1alpha1.DeployResponse{
				Event: &kelsonv1alpha1.DeployResponse_Settled_{Settled: &kelsonv1alpha1.DeployResponse_Settled{
					Final: &kelsonv1alpha1.DeployResponse_Transition{
						Phase: "Applied", Answer: "stuck", Stuck: true, ObservedRevision: "45",
						Cause: &kelsonv1alpha1.DeployResponse_Cause{Component: "direct", Reason: "progress-deadline", Message: "web has not become ready"},
					},
					Error: &kelsonv1alpha1.Error{
						Code:        "delivery/timeout",
						Resource:    "Deployment/hello-production/web",
						Message:     "the deployment did not become healthy within 5m0s",
						Remediation: "run diagnose_component to read the workload's logs",
					},
				}},
			})
		},
	})

	out := h.call(t, "deploy", map[string]any{"project": "hello", "environment": "production", "dry_run": "none"})
	mustContain(t, out,
		"SETTLED Applied (stuck)",
		"did not reach a healthy phase within the server's timeout",
		"stuck      true",
		"direct/progress-deadline: web has not become ready",
		"code: delivery/timeout",
		"remediation: run diagnose_component to read the workload's logs",
	)
}

// TestDeployServerDryRunReportsTheRejection: the server-side rung's verdict is
// the API server's own, relayed with the phase the handler assigned it.
func TestDeployServerDryRunReportsTheRejection(t *testing.T) {
	var request *kelsonv1alpha1.DeployRequest
	h := start(t, &fakeServer{
		deploy: func(req *kelsonv1alpha1.DeployRequest, stream *connect.ServerStream[kelsonv1alpha1.DeployResponse]) error {
			request = req
			if err := stream.Send(&kelsonv1alpha1.DeployResponse{
				Event: &kelsonv1alpha1.DeployResponse_Proposed_{Proposed: &kelsonv1alpha1.DeployResponse_Proposed{Resources: 2, Mode: "direct"}},
			}); err != nil {
				return err
			}
			return stream.Send(&kelsonv1alpha1.DeployResponse{
				Event: &kelsonv1alpha1.DeployResponse_Transition_{Transition: &kelsonv1alpha1.DeployResponse_Transition{
					Phase: "Rejected", Answer: "rejected",
					Cause: &kelsonv1alpha1.DeployResponse_Cause{Component: "kelson", Reason: "server-dry-run-blocked", Message: "1 added, 1 modified, 0 removed (max risk high)"},
				}},
			})
		},
	})

	out := h.call(t, "deploy", map[string]any{"project": "hello", "environment": "production", "dry_run": "server"})
	mustContain(t, out,
		"PREVIEW ONLY, nothing was applied",
		"Kubernetes API server's own",
		"Rejected",
		"server-dry-run-blocked",
	)
	if request.GetDryRun() != kelsonv1alpha1.DryRun_DRY_RUN_SERVER {
		t.Errorf("dry run = %v, want DRY_RUN_SERVER", request.GetDryRun())
	}
}

// TestDeployRejectsAnUnknownDryRun: the ladder has three rungs and an
// unrecognised one must not silently become the mutating rung.
func TestDeployRejectsAnUnknownDryRun(t *testing.T) {
	h := start(t, &fakeServer{})
	out := h.callErr(t, "deploy", map[string]any{"project": "hello", "environment": "production", "dry_run": "maybe"})
	mustContain(t, out, `dry_run must be "render", "server" or "none"`)
	if calls := h.fake.procedures(); len(calls) != 0 {
		t.Errorf("a rejected dry-run value must not reach the server; calls = %v", calls)
	}
}
