package main

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
)

func TestRollbackPreviewsThenApplies(t *testing.T) {
	spec := deploySpec(t)
	var previewCalls, applyCalls int
	fake := &fakeDeployService{
		rollback: func(_ context.Context, req *kelsonv1alpha1.RollbackRequest, stream *connect.ServerStream[kelsonv1alpha1.RollbackResponse]) error {
			preview := &kelsonv1alpha1.RollbackResponse_Preview{
				ToRevision: "6-11112222",
				Findings: []*kelsonv1alpha1.RollbackResponse_Finding{{
					Resource: "hello/development", Cause: "rollback/preview-unavailable", Message: "kelson cannot fetch both artifacts",
				}},
			}
			if err := stream.Send(&kelsonv1alpha1.RollbackResponse{Event: &kelsonv1alpha1.RollbackResponse_Preview_{Preview: preview}}); err != nil {
				return err
			}
			if req.GetDryRun() == kelsonv1alpha1.DryRun_DRY_RUN_RENDER {
				previewCalls++
				return nil
			}
			applyCalls++
			if req.GetToRevision() != "6-11112222" {
				t.Errorf("the apply call carried to_revision %q, want the previewed target 6-11112222", req.GetToRevision())
			}
			if err := stream.Send(&kelsonv1alpha1.RollbackResponse{Event: &kelsonv1alpha1.RollbackResponse_Committed_{
				Committed: &kelsonv1alpha1.RollbackResponse_Committed{RestoredRevision: "6-11112222"},
			}}); err != nil {
				return err
			}
			return stream.Send(&kelsonv1alpha1.RollbackResponse{Event: &kelsonv1alpha1.RollbackResponse_Settled_{
				Settled: &kelsonv1alpha1.RollbackResponse_Settled{},
			}})
		},
	}
	addr := serveFakeDeployService(t, fake)

	stdout, code, msg := runRootStdin(t, "", "rollback", "-f", spec, "--env", "development", "--yes", "--server", addr)
	if code != exitOK {
		t.Fatalf("exit = %d (%s), want 0\n%s", code, msg, stdout)
	}
	if previewCalls != 1 || applyCalls != 1 {
		t.Fatalf("preview calls = %d, apply calls = %d, want 1 and 1", previewCalls, applyCalls)
	}
	for _, want := range []string{"6-11112222", "preview-unavailable", "Committed", "restored revision 6-11112222"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout does not contain %q:\n%s", want, stdout)
		}
	}
}

func TestRollbackSettledErrorFailsTheCommand(t *testing.T) {
	spec := deploySpec(t)
	fake := &fakeDeployService{
		rollback: func(_ context.Context, req *kelsonv1alpha1.RollbackRequest, stream *connect.ServerStream[kelsonv1alpha1.RollbackResponse]) error {
			preview := &kelsonv1alpha1.RollbackResponse_Preview{ToRevision: "5-aaaa0000"}
			if err := stream.Send(&kelsonv1alpha1.RollbackResponse{Event: &kelsonv1alpha1.RollbackResponse_Preview_{Preview: preview}}); err != nil {
				return err
			}
			if req.GetDryRun() == kelsonv1alpha1.DryRun_DRY_RUN_RENDER {
				return nil
			}
			return stream.Send(&kelsonv1alpha1.RollbackResponse{Event: &kelsonv1alpha1.RollbackResponse_Settled_{
				Settled: &kelsonv1alpha1.RollbackResponse_Settled{
					Error: &kelsonv1alpha1.Error{Code: "delivery/rollback-target-unknown", Message: "the history moved"},
				},
			}})
		},
	}
	addr := serveFakeDeployService(t, fake)

	_, code, msg := runRootStdin(t, "", "rollback", "-f", spec, "--env", "development", "--yes", "--server", addr)
	if code != exitErr {
		t.Fatalf("exit = %d, want %d", code, exitErr)
	}
	if !strings.Contains(msg, "delivery/rollback-target-unknown") {
		t.Errorf("message does not carry the settled error's code: %s", msg)
	}
}

func TestRollbackNoFindingsIsReportedExplicitly(t *testing.T) {
	spec := deploySpec(t)
	fake := &fakeDeployService{
		rollback: func(_ context.Context, req *kelsonv1alpha1.RollbackRequest, stream *connect.ServerStream[kelsonv1alpha1.RollbackResponse]) error {
			preview := &kelsonv1alpha1.RollbackResponse_Preview{ToRevision: "2-bbbb0000"}
			if err := stream.Send(&kelsonv1alpha1.RollbackResponse{Event: &kelsonv1alpha1.RollbackResponse_Preview_{Preview: preview}}); err != nil {
				return err
			}
			if req.GetDryRun() == kelsonv1alpha1.DryRun_DRY_RUN_RENDER {
				return nil
			}
			return stream.Send(&kelsonv1alpha1.RollbackResponse{Event: &kelsonv1alpha1.RollbackResponse_Settled_{
				Settled: &kelsonv1alpha1.RollbackResponse_Settled{},
			}})
		},
	}
	addr := serveFakeDeployService(t, fake)

	stdout, code, msg := runRootStdin(t, "", "rollback", "-f", spec, "--env", "development", "--yes", "--server", addr)
	if code != exitOK {
		t.Fatalf("exit = %d (%s), want 0\n%s", code, msg, stdout)
	}
	if !strings.Contains(stdout, "(nothing identified)") {
		t.Errorf("an empty findings list must be reported explicitly, not silently:\n%s", stdout)
	}
}
