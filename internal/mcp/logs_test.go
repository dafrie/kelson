package mcp

import (
	"fmt"
	"testing"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
)

// TestLogsWindowBounds: whatever tail is asked for, at most maxLogLines come
// back, and the clamp is stated rather than applied silently.
func TestLogsWindowBounds(t *testing.T) {
	var query *kelsonv1alpha1.QueryLogsRequest
	h := start(t, &fakeServer{
		status: func(*kelsonv1alpha1.StatusRequest) (*kelsonv1alpha1.StatusResponse, error) {
			return healthyStatus(), nil
		},
		queryLogs: func(req *kelsonv1alpha1.QueryLogsRequest) (*kelsonv1alpha1.QueryLogsResponse, error) {
			query = req
			return logLines(maxLogLines + 5), nil
		},
	})

	out := h.call(t, "logs_window", map[string]any{
		"project": "hello", "environment": "production", "application": "web", "tail": 5000,
	})
	mustContain(t, out,
		"note: tail 5000 was clamped to the tool's cap of 200 lines.",
		"… 5 more earlier lines (truncated)",
		"namespace=hello-production",
	)
	if query.GetTail() != maxLogLines {
		t.Errorf("tail sent to the server = %d, want %d", query.GetTail(), maxLogLines)
	}
}

// TestLogsWindowNamespaceFallback: the namespace comes from Status because that
// is the only RPC that resolves a spec.namespace override (#161). When status
// cannot be read the model default is used — and the answer says so, because a
// spec that overrides it makes the fallback wrong.
func TestLogsWindowNamespaceFallback(t *testing.T) {
	var query *kelsonv1alpha1.QueryLogsRequest
	h := start(t, &fakeServer{
		status: func(*kelsonv1alpha1.StatusRequest) (*kelsonv1alpha1.StatusResponse, error) {
			return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("api: building the delivery plane: no cluster"))
		},
		queryLogs: func(req *kelsonv1alpha1.QueryLogsRequest) (*kelsonv1alpha1.QueryLogsResponse, error) {
			query = req
			return logLines(2), nil
		},
	})

	out := h.call(t, "logs_window", map[string]any{
		"project": "hello", "environment": "production", "application": "web",
	})
	mustContain(t, out,
		"namespace=hello-production",
		"the namespace is the model default",
		"spec.namespace",
	)
	if query.GetSelector().GetNamespace() != "hello-production" {
		t.Errorf("namespace = %q, want the model default hello-production", query.GetSelector().GetNamespace())
	}
}

// TestLogsWindowNeedsAnApplication: with no status to pick a workload from and
// no application named, the tool says what to do instead of querying with an
// empty selector.
func TestLogsWindowNeedsAnApplication(t *testing.T) {
	h := start(t, &fakeServer{
		status: func(*kelsonv1alpha1.StatusRequest) (*kelsonv1alpha1.StatusResponse, error) {
			return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("api: no cluster"))
		},
	})
	out := h.callErr(t, "logs_window", map[string]any{"project": "hello", "environment": "production"})
	mustContain(t, out, "cannot pick a workload", "diagnose_application")
}

// TestLogsWindowAtTermination: the crash-loop question is a first-class window,
// and a match filter is passed to the server so filtering happens before the
// window is cut.
func TestLogsWindowAtTermination(t *testing.T) {
	var query *kelsonv1alpha1.QueryLogsRequest
	h := start(t, &fakeServer{
		status: func(*kelsonv1alpha1.StatusRequest) (*kelsonv1alpha1.StatusResponse, error) {
			return crashLoopStatus(), nil
		},
		queryLogs: func(req *kelsonv1alpha1.QueryLogsRequest) (*kelsonv1alpha1.QueryLogsResponse, error) {
			query = req
			return logLines(1), nil
		},
	})

	out := h.call(t, "logs_window", map[string]any{
		"project": "hello", "environment": "production",
		"around_termination": true, "match": "panic", "tail": 20,
	})
	mustContain(t, out, "application=web", "before termination", `filtered to lines containing "panic"`)
	if query.GetAround() == nil || !query.GetAround().GetAtTermination() || query.GetAround().GetLines() != 20 {
		t.Errorf("around window = %+v, want 20 lines at termination", query.GetAround())
	}
	if query.GetTail() != 0 {
		t.Error("tail and around are mutually exclusive on the wire; both were set")
	}
	if query.GetMatch().GetSubstring() != "panic" {
		t.Errorf("match = %+v, want the substring panic", query.GetMatch())
	}
}
