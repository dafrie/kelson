package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"github.com/dafrie/kelson/internal/api"
	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
)

// The `reason` parameter of the mutating tools (issue #78, ADR-0026 §4).
//
// It is the half of "what did it actually do?" that no amount of server-side
// observation can reconstruct: kelson can see that an agent deployed, never why
// it decided to. So the tools take the agent's own sentence and carry it to the
// server, where it lands in the action's audit record — and kelson invents
// nothing when none was given.

// TestReasonHeaderMatchesTheServer: this package copies the header name rather
// than importing internal/api, so that a stdio sidecar does not link the server
// plane for one string. The copy must not drift, and only a test can say so.
func TestReasonHeaderMatchesTheServer(t *testing.T) {
	if ReasonHeader != api.ReasonHeader {
		t.Fatalf("mcp.ReasonHeader = %q but api.ReasonHeader = %q; a reason sent under the wrong name is a "+
			"reason silently dropped", ReasonHeader, api.ReasonHeader)
	}
}

// TestEveryMutatingToolCarriesAReason walks the registered surface and asserts
// that each mutating tool offers the parameter. A tool that changes the world
// and cannot say why leaves a record that only half answers the question the
// trail exists for — and the one that got forgotten would be the one that
// mattered.
func TestEveryMutatingToolCarriesAReason(t *testing.T) {
	h := start(t, &fakeServer{})
	listed, err := h.session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(listed.Tools) == 0 {
		t.Fatal("the surface registered no tools")
	}
	mutating := 0
	for _, tool := range listed.Tools {
		if tool.Annotations != nil && tool.Annotations.ReadOnlyHint {
			continue
		}
		mutating++
		schema, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatalf("marshalling %s's input schema: %v", tool.Name, err)
		}
		if !strings.Contains(string(schema), `"reason"`) {
			t.Errorf("the mutating tool %q has no `reason` parameter: %s", tool.Name, schema)
			continue
		}
		// The description has to say where it goes, or a model has no reason to
		// fill it in.
		if !strings.Contains(strings.ToLower(string(schema)), "audit trail") {
			t.Errorf("%q's reason does not tell the model where it goes: %s", tool.Name, schema)
		}
	}
	if mutating < 5 {
		t.Fatalf("found %d mutating tools; the surface has five and this check would be vacuous", mutating)
	}
}

// TestDeployCarriesTheReasonToTheServer is the end-to-end half: the parameter
// reaches the wire as the header the server reads.
func TestDeployCarriesTheReasonToTheServer(t *testing.T) {
	h := start(t, &fakeServer{
		deploy: func(_ *kelsonv1alpha1.DeployRequest, stream *connect.ServerStream[kelsonv1alpha1.DeployResponse]) error {
			return stream.Send(&kelsonv1alpha1.DeployResponse{
				Event: &kelsonv1alpha1.DeployResponse_Proposed_{
					Proposed: &kelsonv1alpha1.DeployResponse_Proposed{Resources: 1, Mode: "direct"},
				},
			})
		},
	})

	h.call(t, "deploy", map[string]any{
		"project":     "hello",
		"environment": "production",
		"reason":      "the checkout latency alert fired and PR 412 fixes it",
	})
	if got := firstReason(h.fake.statedReasons()); got != "the checkout latency alert fired and PR 412 fixes it" {
		t.Fatalf("the server received reason %q", got)
	}
}

// TestNoReasonSendsNoHeader: kelson records what the caller said and never
// invents it, so a tool call with no reason must put nothing on the wire rather
// than a placeholder that would read as a stated intent.
func TestNoReasonSendsNoHeader(t *testing.T) {
	h := start(t, &fakeServer{
		deploy: func(_ *kelsonv1alpha1.DeployRequest, stream *connect.ServerStream[kelsonv1alpha1.DeployResponse]) error {
			return stream.Send(&kelsonv1alpha1.DeployResponse{
				Event: &kelsonv1alpha1.DeployResponse_Proposed_{
					Proposed: &kelsonv1alpha1.DeployResponse_Proposed{Resources: 1, Mode: "direct"},
				},
			})
		},
	})

	h.call(t, "deploy", map[string]any{"project": "hello", "environment": "production"})
	for _, reason := range h.fake.statedReasons() {
		if reason != "" {
			t.Fatalf("a call with no reason sent %q", reason)
		}
	}
}

// TestASecretWriteCarriesTheReasonAndNeverTheValue: the reason travels for the
// one tool that also carries credentials, and the values stay in the body where
// the server's redaction registry receives them (issue #117).
func TestASecretWriteCarriesTheReasonAndNeverTheValue(t *testing.T) {
	const value = "pk_live_not_in_a_header"
	h := start(t, &fakeServer{
		setSecret: func(req *kelsonv1alpha1.SetSecretRequest) (*kelsonv1alpha1.SetSecretResponse, error) {
			return &kelsonv1alpha1.SetSecretResponse{
				Secret:      &kelsonv1alpha1.SecretSummary{Name: req.GetName(), Namespace: "hello-production"},
				WrittenKeys: []string{"api-key"},
			}, nil
		},
	})

	h.call(t, "set_secret", map[string]any{
		"project":     "hello",
		"environment": "production",
		"name":        "stripe",
		"values":      map[string]any{"api-key": value},
		"execute":     true,
		"reason":      "rotating the Stripe key after the vendor advisory",
	})

	got := firstReason(h.fake.statedReasons())
	if got != "rotating the Stripe key after the vendor advisory" {
		t.Fatalf("the server received reason %q", got)
	}
	if strings.Contains(got, value) {
		t.Fatal("the secret value reached the reason header")
	}
}

// TestAnOverlongReasonIsBoundedAndMarked: a model that pastes a stack trace
// into `reason` must not put it on the wire, and the truncation must be
// visible — a recorded reason that is quietly half a sentence is worse than a
// short one.
func TestAnOverlongReasonIsBoundedAndMarked(t *testing.T) {
	req := reasoned(connect.NewRequest(&kelsonv1alpha1.DeployRequest{}), strings.Repeat("x", 4000))
	got := req.Header().Get(ReasonHeader)
	if len(got) > maxReason+len("…[truncated]") {
		t.Errorf("the reason was sent at %d bytes, past the %d-byte bound", len(got), maxReason)
	}
	if !strings.HasSuffix(got, "…[truncated]") {
		t.Error("a shortened reason was not marked as shortened")
	}
}

// TestAReasonWithLineBreaksIsFolded: a header value cannot carry a line break,
// and prose might. Folding keeps the sentence; dropping it would lose the
// reason, and sending it raw would be rejected by the transport.
func TestAReasonWithLineBreaksIsFolded(t *testing.T) {
	req := reasoned(connect.NewRequest(&kelsonv1alpha1.DeployRequest{}), "  rolling back\nthe checkout\r\nregression  ")
	if got := req.Header().Get(ReasonHeader); got != "rolling back the checkout regression" {
		t.Fatalf("reason header = %q", got)
	}
}

// firstReason returns the first non-empty stated reason, since a tool may make
// more than one call and only the mutating one carries it.
func firstReason(reasons []string) string {
	for _, reason := range reasons {
		if reason != "" {
			return reason
		}
	}
	return ""
}
