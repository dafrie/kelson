package mcp

import (
	"fmt"
	"testing"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
)

// TestListComponentsComposition: the listing is ListSpecs plus one Status per
// environment, reduced to what an agent needs to decide where to look — and
// never a spec body.
func TestListComponentsComposition(t *testing.T) {
	h := start(t, &fakeServer{
		listSpecs: func(*kelsonv1alpha1.ListSpecsRequest) (*kelsonv1alpha1.ListSpecsResponse, error) {
			return &kelsonv1alpha1.ListSpecsResponse{Specs: []*kelsonv1alpha1.Spec{
				{Project: "hello", Environments: []string{"development", "production"}},
			}}, nil
		},
		status: func(req *kelsonv1alpha1.StatusRequest) (*kelsonv1alpha1.StatusResponse, error) {
			if req.GetEnvironment() == "production" {
				return crashLoopStatus(), nil
			}
			return healthyStatus(), nil
		},
	})

	out := h.call(t, "list_components", map[string]any{})
	mustContain(t, out,
		"hello",
		"development",
		"Healthy",
		"rev 42",
		"1/1 workloads healthy",
		"production",
		"Degraded",
		"1/2 workloads healthy",
		"1 degraded",
		"cause: ReplicaSet hello-web-7f6 has not progressed",
	)
	mustNotContain(t, out, "apiVersion")
}

// TestListComponentsStatusFailureIsALine: one unreadable environment must not
// hide the others, and the failure keeps the server's own words so an agent can
// tell "broken" from "could not look".
func TestListComponentsStatusFailureIsALine(t *testing.T) {
	h := start(t, &fakeServer{
		listSpecs: func(*kelsonv1alpha1.ListSpecsRequest) (*kelsonv1alpha1.ListSpecsResponse, error) {
			return &kelsonv1alpha1.ListSpecsResponse{Specs: []*kelsonv1alpha1.Spec{
				{Project: "hello", Environments: []string{"development", "production"}},
			}}, nil
		},
		status: func(req *kelsonv1alpha1.StatusRequest) (*kelsonv1alpha1.StatusResponse, error) {
			if req.GetEnvironment() == "development" {
				return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("api: building the delivery plane: no cluster"))
			}
			return healthyStatus(), nil
		},
	})

	out := h.call(t, "list_components", map[string]any{})
	mustContain(t, out, "status unavailable", "no cluster", "production", "Healthy")
}

// TestListComponentsTruncates: every list this package renders is capped and
// says so. An agent that read a truncated list as the whole list would draw a
// wrong conclusion from a correct answer.
func TestListComponentsTruncates(t *testing.T) {
	var specs []*kelsonv1alpha1.Spec
	for i := range maxProjects + 3 {
		environments := []string{"development"}
		if i == 0 {
			for j := range maxEnvironments + 2 {
				environments = append(environments, fmt.Sprintf("env-%d", j))
			}
		}
		specs = append(specs, &kelsonv1alpha1.Spec{Project: fmt.Sprintf("project-%02d", i), Environments: environments})
	}
	h := start(t, &fakeServer{
		listSpecs: func(*kelsonv1alpha1.ListSpecsRequest) (*kelsonv1alpha1.ListSpecsResponse, error) {
			return &kelsonv1alpha1.ListSpecsResponse{Specs: specs}, nil
		},
		status: func(*kelsonv1alpha1.StatusRequest) (*kelsonv1alpha1.StatusResponse, error) {
			return healthyStatus(), nil
		},
	})

	out := h.call(t, "list_components", map[string]any{})
	mustContain(t, out,
		"… 3 more projects (truncated)",
		"… 3 more environments (truncated)",
	)
	mustNotContain(t, out, "project-27")
}

// TestListComponentsEmpty: nothing stored is an answer with the next step in
// it, not an empty page.
func TestListComponentsEmpty(t *testing.T) {
	h := start(t, &fakeServer{
		listSpecs: func(*kelsonv1alpha1.ListSpecsRequest) (*kelsonv1alpha1.ListSpecsResponse, error) {
			return &kelsonv1alpha1.ListSpecsResponse{}, nil
		},
	})
	mustContain(t, h.call(t, "list_components", map[string]any{}), "No projects are stored", "put_spec")
}
