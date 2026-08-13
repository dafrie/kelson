package mcp

import (
	"testing"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/diff"
)

const (
	promotedWeb    = "ghcr.io/acme/hello@sha256:aaaa"
	promotedWorker = "ghcr.io/acme/hello@sha256:bbbb"
)

// promoteAnswer is the shape the RPC returns: one component pinned, one
// unchanged, one skipped, and the diff of the target environment.
func promoteAnswer(t *testing.T, version string) *kelsonv1alpha1.PromoteResponse {
	t.Helper()
	encoded, err := diff.EncodeJSON(&diff.Diff{
		Level: diff.LevelRendered, Project: "hello", Environment: "production",
		Summary: diff.Summary{Modified: 2, MaxRisk: diff.RiskRestart, Restarting: []string{"Deployment/web"}},
	})
	if err != nil {
		t.Fatalf("encoding the fixture diff: %v", err)
	}
	return &kelsonv1alpha1.PromoteResponse{
		FromRevision:  "rev-00000012",
		Version:       version,
		DiffJson:      encoded,
		ExitSemantics: 2,
		Components: []*kelsonv1alpha1.PromotedComponent{
			{
				Component: "web", ToImage: promotedWeb,
				Status: kelsonv1alpha1.PromotionStatus_PROMOTION_STATUS_PINNED,
			},
			{
				Component: "worker", FromImage: promotedWorker, ToImage: promotedWorker,
				Status: kelsonv1alpha1.PromotionStatus_PROMOTION_STATUS_UNCHANGED,
			},
			{
				Component: "digest",
				Status:    kelsonv1alpha1.PromotionStatus_PROMOTION_STATUS_SKIPPED,
				Code:      "promote/not-in-revision",
				Reason:    "the source environment's latest revision records no image for this component",
			},
		},
	}
}

// TestPromotePreviewsByDefault: execute=false writes nothing, and still returns
// the whole answer — what would be pinned and the diff it produces.
func TestPromotePreviewsByDefault(t *testing.T) {
	var request *kelsonv1alpha1.PromoteRequest
	h := start(t, &fakeServer{
		promote: func(req *kelsonv1alpha1.PromoteRequest) (*kelsonv1alpha1.PromoteResponse, error) {
			request = req
			return promoteAnswer(t, ""), nil
		},
	})

	out := h.call(t, "promote_application", map[string]any{
		"project": "hello", "from_environment": "staging", "to_environment": "production",
	})
	mustContain(t, out,
		"PREVIEW ONLY, nothing was written (1 pinned, 1 unchanged, 1 skipped)",
		"source revision: rev-00000012",
		"COMPONENTS (3)",
		"pinned    - -> "+promotedWeb,
		"unchanged  already pinned to "+promotedWorker,
		"skipped    the source environment's latest revision records no image",
		"(promote/not-in-revision)",
		"0 added, 2 modified, 0 removed (max risk restart-required)",
		"exit semantics: 2",
	)
	if request.GetDryRun() != kelsonv1alpha1.DryRun_DRY_RUN_RENDER {
		t.Errorf("dry run = %v, want DRY_RUN_RENDER when execute is false", request.GetDryRun())
	}
	if request.GetIdempotencyKey() == "" {
		t.Error("a mutating tool sent no idempotency key")
	}
}

// TestPromoteExecutes: execute=true writes, reports the new spec version, and
// says plainly that nothing has been deployed yet.
func TestPromoteExecutes(t *testing.T) {
	var request *kelsonv1alpha1.PromoteRequest
	h := start(t, &fakeServer{
		promote: func(req *kelsonv1alpha1.PromoteRequest) (*kelsonv1alpha1.PromoteResponse, error) {
			request = req
			return promoteAnswer(t, "4711"), nil
		},
	})

	out := h.call(t, "promote_application", map[string]any{
		"project": "hello", "from_environment": "staging", "to_environment": "production",
		"components": []any{"web"}, "execute": true, "version": "4710",
	})
	mustContain(t, out,
		"WRITTEN to spec version 4711 (1 pinned, 1 unchanged, 1 skipped). NOT deployed.",
		"the pins are stored but not live. Call deploy for hello/production",
	)
	if request.GetDryRun() != kelsonv1alpha1.DryRun_DRY_RUN_NONE {
		t.Errorf("dry run = %v, want DRY_RUN_NONE when execute is true", request.GetDryRun())
	}
	if got := request.GetComponents(); len(got) != 1 || got[0] != "web" {
		t.Errorf("components = %v, want the filter passed through", got)
	}
	if request.GetVersion() != "4710" {
		t.Errorf("version = %q, want the caller's optimistic-concurrency token", request.GetVersion())
	}
}

// TestPromoteReportsFindingsAndSaysNothingWasWritten: an answer carrying errors
// means the promotion was refused, and the tool must not let that read as a
// successful write.
func TestPromoteReportsFindingsAndSaysNothingWasWritten(t *testing.T) {
	h := start(t, &fakeServer{
		promote: func(*kelsonv1alpha1.PromoteRequest) (*kelsonv1alpha1.PromoteResponse, error) {
			return &kelsonv1alpha1.PromoteResponse{
				Errors: []*kelsonv1alpha1.Error{{
					Code: "image/unresolved", Resource: "Environment/production",
					Message: "the promoted spec does not render", Remediation: "pin every component",
				}},
			}, nil
		},
	})

	out := h.call(t, "promote_application", map[string]any{
		"project": "hello", "from_environment": "staging", "to_environment": "production", "execute": true,
	})
	mustContain(t, out,
		"REFUSED, nothing was written",
		"code: image/unresolved",
		"nothing was written: a promotion whose result does not validate or render is refused.",
	)
}

// TestPromoteSurfacesTheStructuredRefusal: a failed RPC carries its code and
// remediation through verbatim, because that is the machine-readable half.
func TestPromoteSurfacesTheStructuredRefusal(t *testing.T) {
	h := start(t, &fakeServer{
		promote: func(*kelsonv1alpha1.PromoteRequest) (*kelsonv1alpha1.PromoteResponse, error) {
			cerr := connect.NewError(connect.CodeFailedPrecondition, errNothingDeployed{})
			detail, derr := connect.NewErrorDetail(&kelsonv1alpha1.Error{
				Code:        "promote/nothing-deployed",
				Resource:    "Environment/staging",
				Message:     "nothing has been deployed to hello/staging",
				Remediation: "deploy staging first",
			})
			if derr != nil {
				return nil, cerr
			}
			cerr.AddDetail(detail)
			return nil, cerr
		},
	})

	out := h.callErr(t, "promote_application", map[string]any{
		"project": "hello", "from_environment": "staging", "to_environment": "production",
	})
	mustContain(t, out,
		"kelson.v1alpha1.DeployService.Promote failed",
		"code: promote/nothing-deployed",
		"remediation: deploy staging first",
	)
}

type errNothingDeployed struct{}

func (errNothingDeployed) Error() string { return "nothing has been deployed to hello/staging" }
