package mcp

import (
	"strings"
	"testing"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/diff"
)

func previewEvent(t *testing.T) *kelsonv1alpha1.RollbackResponse {
	t.Helper()
	encoded, err := diff.EncodeJSON(&diff.Diff{
		Level: diff.LevelRendered, Project: "hello", Environment: "production",
		Summary: diff.Summary{Added: 1, Modified: 2, Removed: 0, MaxRisk: diff.RiskDisruptive, Restarting: []string{"Deployment/web"}},
	})
	if err != nil {
		t.Fatalf("encoding the fixture diff: %v", err)
	}
	return &kelsonv1alpha1.RollbackResponse{
		Event: &kelsonv1alpha1.RollbackResponse_Preview_{Preview: &kelsonv1alpha1.RollbackResponse_Preview{
			ToRevision: "41",
			DiffJson:   encoded,
			Findings: []*kelsonv1alpha1.RollbackResponse_Finding{
				{
					Resource: "PersistentVolumeClaim/hello-production/data", Path: "spec.resources.requests.storage",
					Cause: "storage-shrink", Message: "a volume cannot be shrunk back", Unrecoverable: true,
				},
				{Resource: "Deployment/hello-production/web", Path: "spec.template", Cause: "restart", Message: "the pods will roll"},
			},
		}},
	}
}

// TestRollbackPreviewsByDefault: execute=false applies nothing and returns what
// the rollback cannot revert, unrecoverable findings flagged as such. This is
// the preview a rollback must never be attempted without (issues #38, #55).
func TestRollbackPreviewsByDefault(t *testing.T) {
	var request *kelsonv1alpha1.RollbackRequest
	h := start(t, &fakeServer{
		rollback: func(req *kelsonv1alpha1.RollbackRequest, stream *connect.ServerStream[kelsonv1alpha1.RollbackResponse]) error {
			request = req
			return stream.Send(previewEvent(t))
		},
	})

	out := h.call(t, "rollback", map[string]any{"project": "hello", "environment": "production"})
	mustContain(t, out,
		"to revision 41: PREVIEW ONLY, nothing was applied",
		"FINDINGS (2, 1 unrecoverable)",
		"UNRECOVERABLE PersistentVolumeClaim/hello-production/data",
		"cause: storage-shrink",
		"recoverable   Deployment/hello-production/web",
		"1 added, 2 modified, 0 removed (max risk disruptive)",
		"restarting: Deployment/web",
	)
	if request.GetDryRun() != kelsonv1alpha1.DryRun_DRY_RUN_RENDER {
		t.Errorf("dry run = %v, want DRY_RUN_RENDER when execute is false", request.GetDryRun())
	}
}

// TestRollbackExecutes: execute=true applies it and reports which revision was
// restored and what the history recorded it as.
func TestRollbackExecutes(t *testing.T) {
	var request *kelsonv1alpha1.RollbackRequest
	h := start(t, &fakeServer{
		rollback: func(req *kelsonv1alpha1.RollbackRequest, stream *connect.ServerStream[kelsonv1alpha1.RollbackResponse]) error {
			request = req
			if err := stream.Send(previewEvent(t)); err != nil {
				return err
			}
			if err := stream.Send(&kelsonv1alpha1.RollbackResponse{
				Event: &kelsonv1alpha1.RollbackResponse_Committed_{Committed: &kelsonv1alpha1.RollbackResponse_Committed{
					RestoredRevision: "41", AsRevision: "46",
				}},
			}); err != nil {
				return err
			}
			return stream.Send(&kelsonv1alpha1.RollbackResponse{
				Event: &kelsonv1alpha1.RollbackResponse_Settled_{Settled: &kelsonv1alpha1.RollbackResponse_Settled{}},
			})
		},
	})

	out := h.call(t, "rollback", map[string]any{
		"project": "hello", "environment": "production", "to_revision": "41", "execute": true,
	})
	mustContain(t, out, "EXECUTED", "restored revision 41, recorded as revision 46", "FINDINGS (2, 1 unrecoverable)")
	if request.GetDryRun() != kelsonv1alpha1.DryRun_DRY_RUN_NONE {
		t.Errorf("dry run = %v, want DRY_RUN_NONE when execute is true", request.GetDryRun())
	}
	if request.GetToRevision() != "41" {
		t.Errorf("to_revision = %q, want 41", request.GetToRevision())
	}
}

// TestRollbackFailureCarriesTheCode: a rollback that failed to apply settles
// with the structured error, which the tool relays verbatim.
func TestRollbackFailureCarriesTheCode(t *testing.T) {
	h := start(t, &fakeServer{
		rollback: func(_ *kelsonv1alpha1.RollbackRequest, stream *connect.ServerStream[kelsonv1alpha1.RollbackResponse]) error {
			if err := stream.Send(previewEvent(t)); err != nil {
				return err
			}
			return stream.Send(&kelsonv1alpha1.RollbackResponse{
				Event: &kelsonv1alpha1.RollbackResponse_Settled_{Settled: &kelsonv1alpha1.RollbackResponse_Settled{
					Error: &kelsonv1alpha1.Error{
						Code: "delivery/apply-failed", Resource: "Deployment/hello-production/web",
						Message: "admission webhook denied the request", Remediation: "check the cluster's admission policy",
					},
				}},
			})
		},
	})

	out := h.call(t, "rollback", map[string]any{"project": "hello", "environment": "production", "execute": true})
	mustContain(t, out, "code: delivery/apply-failed", "remediation: check the cluster's admission policy")
}

// TestRollbackWithoutRecordedManifests: a preview that carries no findings and
// no diff still says so explicitly. An empty diff must never read as "nothing
// changes" (the real server sends the preview-unavailable finding below when it
// could not compare the two revisions, but the tool must not assume that and
// skip the "none" answer if it ever did not).
func TestRollbackWithoutRecordedManifests(t *testing.T) {
	h := start(t, &fakeServer{
		rollback: func(_ *kelsonv1alpha1.RollbackRequest, stream *connect.ServerStream[kelsonv1alpha1.RollbackResponse]) error {
			return stream.Send(&kelsonv1alpha1.RollbackResponse{
				Event: &kelsonv1alpha1.RollbackResponse_Preview_{Preview: &kelsonv1alpha1.RollbackResponse_Preview{ToRevision: "41"}},
			})
		},
	})

	out := h.call(t, "rollback", map[string]any{"project": "hello", "environment": "production"})
	mustContain(t, out,
		"FINDINGS (0, 0 unrecoverable)",
		"none: the server found nothing this rollback cannot revert",
		"none: the server could not read both revisions' artifacts back",
	)
}

// TestRollbackPreviewUnavailableFindingReadsAsInformational: the one finding
// the server sends when it could not pull both artifacts
// (rollback/preview-unavailable, ADR-0028) is
// marked INFO rather than UNRECOVERABLE — it says what kelson could not
// compare, not something this rollback will fail to revert.
func TestRollbackPreviewUnavailableFindingReadsAsInformational(t *testing.T) {
	h := start(t, &fakeServer{
		rollback: func(_ *kelsonv1alpha1.RollbackRequest, stream *connect.ServerStream[kelsonv1alpha1.RollbackResponse]) error {
			return stream.Send(&kelsonv1alpha1.RollbackResponse{
				Event: &kelsonv1alpha1.RollbackResponse_Preview_{Preview: &kelsonv1alpha1.RollbackResponse_Preview{
					ToRevision: "41",
					Findings: []*kelsonv1alpha1.RollbackResponse_Finding{{
						Resource: "hello/production", Cause: "rollback/preview-unavailable",
						Message: "kelson cannot show what changes between 41 and 40", Unrecoverable: false,
					}},
				}},
			})
		},
	})

	out := h.call(t, "rollback", map[string]any{"project": "hello", "environment": "production"})
	mustContain(t, out,
		"FINDINGS (1, 0 unrecoverable)",
		"INFO          hello/production",
		"cause: rollback/preview-unavailable",
	)
	if strings.Contains(out, "UNRECOVERABLE") {
		t.Errorf("the preview-unavailable finding must not read as an unrecoverable risk:\n%s", out)
	}
}

// TestRollbackExecutesRecordsNoNewRevision: the real server always leaves
// Committed.as_revision empty for a rollback (it publishes nothing), and the
// tool must say so rather than print "recorded as revision " with nothing
// after it.
func TestRollbackExecutesRecordsNoNewRevision(t *testing.T) {
	h := start(t, &fakeServer{
		rollback: func(_ *kelsonv1alpha1.RollbackRequest, stream *connect.ServerStream[kelsonv1alpha1.RollbackResponse]) error {
			if err := stream.Send(previewEvent(t)); err != nil {
				return err
			}
			if err := stream.Send(&kelsonv1alpha1.RollbackResponse{
				Event: &kelsonv1alpha1.RollbackResponse_Committed_{Committed: &kelsonv1alpha1.RollbackResponse_Committed{
					RestoredRevision: "41",
				}},
			}); err != nil {
				return err
			}
			return stream.Send(&kelsonv1alpha1.RollbackResponse{
				Event: &kelsonv1alpha1.RollbackResponse_Settled_{Settled: &kelsonv1alpha1.RollbackResponse_Settled{}},
			})
		},
	})

	out := h.call(t, "rollback", map[string]any{
		"project": "hello", "environment": "production", "to_revision": "41", "execute": true,
	})
	mustContain(t, out, "restored revision 41 (a rollback publishes nothing, so no new revision was recorded)")
	if strings.Contains(out, "recorded as revision \n") || strings.Contains(out, "recorded as revision  ") {
		t.Errorf("an empty as_revision must not be printed as a blank value:\n%s", out)
	}
}
