package mcp

import (
	"testing"
	"time"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
)

func statusEvent(previous, phase, revision string) *kelsonv1alpha1.WatchResponse {
	return &kelsonv1alpha1.WatchResponse{Body: &kelsonv1alpha1.WatchResponse_Event_{
		Event: &kelsonv1alpha1.WatchResponse_Event{
			Cursor: phase, AtUnixMs: 1_755_000_000_000, Project: "hello", Environment: "production",
			Payload: &kelsonv1alpha1.WatchResponse_Event_StatusTransition{
				StatusTransition: &kelsonv1alpha1.WatchResponse_StatusTransition{
					Phase: phase, PreviousPhase: previous, Revision: revision,
				},
			},
		},
	}}
}

// TestWaitReturnsOnTheTerminalTransition: the wait ends at Healthy, and the
// events on the way are kept as a bounded trail. Which phases end a wait is
// delivery's vocabulary, not this tool's.
func TestWaitReturnsOnTheTerminalTransition(t *testing.T) {
	var request *kelsonv1alpha1.WatchRequest
	h := start(t, &fakeServer{
		watch: func(req *kelsonv1alpha1.WatchRequest, stream *connect.ServerStream[kelsonv1alpha1.WatchResponse]) error {
			request = req
			for _, event := range []*kelsonv1alpha1.WatchResponse{
				statusEvent("Committed", "Applied", "47"),
				statusEvent("Applied", "Healthy", "47"),
			} {
				if err := stream.Send(event); err != nil {
					return err
				}
			}
			return nil
		},
	})

	out := h.call(t, "wait_for_outcome", map[string]any{"project": "hello", "environment": "production"})
	mustContain(t, out,
		"OUTCOME",
		"the environment reached Healthy (from Applied), revision 47",
		"EVENTS (2)",
		"status Committed -> Applied",
	)
	if len(request.GetScopes()) != 1 || request.GetScopes()[0].GetProject() != "hello" {
		t.Errorf("the watch must be scoped to the named environment, got %+v", request.GetScopes())
	}
}

// TestWaitReturnsOnAHealthFailure: a workload turning unhealthy is terminal
// too — an agent waiting after a deploy needs to hear about a crash loop, not
// only about a phase.
func TestWaitReturnsOnAHealthFailure(t *testing.T) {
	h := start(t, &fakeServer{
		watch: func(_ *kelsonv1alpha1.WatchRequest, stream *connect.ServerStream[kelsonv1alpha1.WatchResponse]) error {
			return stream.Send(&kelsonv1alpha1.WatchResponse{Body: &kelsonv1alpha1.WatchResponse_Event_{
				Event: &kelsonv1alpha1.WatchResponse_Event{
					Project: "hello", Environment: "production",
					Payload: &kelsonv1alpha1.WatchResponse_Event_HealthChange{
						HealthChange: &kelsonv1alpha1.WatchResponse_HealthChange{
							Resource: "Deployment/hello-production/web", Code: "crash-loop-back-off",
							PreviousCode: "progressing", Message: "back-off restarting failed container",
						},
					},
				},
			}})
		},
	})

	out := h.call(t, "wait_for_outcome", map[string]any{"project": "hello", "environment": "production"})
	mustContain(t, out,
		"OUTCOME",
		"workload Deployment/hello-production/web turned unhealthy: crash-loop-back-off (was progressing)",
	)
}

// TestWaitHandlesResync: a Resync says the view has a gap. The contract is to
// relist DeployService.Status once and keep reading the same stream, which is
// what makes the following Healthy event reachable at all.
func TestWaitHandlesResync(t *testing.T) {
	statusCalls := 0
	h := start(t, &fakeServer{
		status: func(*kelsonv1alpha1.StatusRequest) (*kelsonv1alpha1.StatusResponse, error) {
			statusCalls++
			return healthyStatus(), nil
		},
		watch: func(_ *kelsonv1alpha1.WatchRequest, stream *connect.ServerStream[kelsonv1alpha1.WatchResponse]) error {
			if err := stream.Send(&kelsonv1alpha1.WatchResponse{Body: &kelsonv1alpha1.WatchResponse_Resync_{
				Resync: &kelsonv1alpha1.WatchResponse_Resync{Reason: "the cursor is older than the retained window"},
			}}); err != nil {
				return err
			}
			return stream.Send(statusEvent("Applied", "Healthy", "48"))
		},
	})

	out := h.call(t, "wait_for_outcome", map[string]any{"project": "hello", "environment": "production"})
	mustContain(t, out,
		"resync — the cursor is older than the retained window",
		"relisted status — phase Healthy, revision 42",
		"the environment reached Healthy (from Applied), revision 48",
	)
	if statusCalls != 1 {
		t.Errorf("status was relisted %d times, want exactly once per resync", statusCalls)
	}
}

// TestWaitTimesOut: a timeout is an answer about the environment, not a failure
// of the call, and it says what to do next.
func TestWaitTimesOut(t *testing.T) {
	h := start(t, &fakeServer{
		watch: func(_ *kelsonv1alpha1.WatchRequest, stream *connect.ServerStream[kelsonv1alpha1.WatchResponse]) error {
			if err := stream.Send(statusEvent("Committed", "Applied", "49")); err != nil {
				return err
			}
			// Hold the stream open past the caller's deadline, the way a real
			// watch on a slow deployment does.
			time.Sleep(3 * time.Second)
			return nil
		},
	})

	out := h.call(t, "wait_for_outcome", map[string]any{
		"project": "hello", "environment": "production", "timeout_seconds": 1,
	})
	mustContain(t, out,
		"TIMEOUT after 1s",
		"nothing terminal happened in the window",
		"diagnose_component",
		"status Committed -> Applied",
	)
}

// TestWaitOnAnEmptyScope: the server completes a watch whose scope matches
// nothing stored. An open stream over nothing looks live and never is, so the
// tool reports the closure and names the fix.
func TestWaitOnAnEmptyScope(t *testing.T) {
	h := start(t, &fakeServer{
		watch: func(*kelsonv1alpha1.WatchRequest, *connect.ServerStream[kelsonv1alpha1.WatchResponse]) error {
			return nil
		},
	})

	out := h.call(t, "wait_for_outcome", map[string]any{"project": "nope", "environment": "production"})
	mustContain(t, out, "STREAM ENDED", "list_components", "EVENTS (0)")
}
