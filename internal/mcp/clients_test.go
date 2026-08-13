package mcp

import (
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
)

// TestFailStatesTheV0Posture: a server that did not answer is the failure an
// agent will hit most often, and "unauthorized" is exactly the wrong conclusion
// to let it draw — v0 has no authentication at all. The address and the posture
// are both in the message.
func TestFailStatesTheV0Posture(t *testing.T) {
	c := &clients{addr: "http://127.0.0.1:8420"}
	err := c.fail(rpcStatus, connect.NewError(connect.CodeUnavailable, errors.New("dial tcp: connection refused")))

	for _, want := range []string{
		"kelson.v1alpha1.DeployService.Status failed: unavailable",
		"kelson-server at http://127.0.0.1:8420 did not answer",
		"KELSON_SERVER",
		"no authentication",
		"#74",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the connection error does not mention %q:\n%s", want, err)
		}
	}
}

// TestFailDoesNotAddTheConnectionNoteToARealAnswer: a rejected request is not a
// connection problem, and telling an agent to check its server address would
// send it looking in the wrong place.
func TestFailDoesNotAddTheConnectionNoteToARealAnswer(t *testing.T) {
	c := &clients{addr: "http://127.0.0.1:8420"}
	err := c.fail(rpcPutSpec, connect.NewError(connect.CodeInvalidArgument, errors.New("api: the spec carries no Project document")))
	if strings.Contains(err.Error(), "did not answer") {
		t.Errorf("an invalid argument was reported as a connection failure:\n%s", err)
	}
}

// TestWireErrorRendersEveryFieldItHas: the taxonomy is what an agent branches
// on, so no field of it is summarised away.
func TestWireErrorRendersEveryFieldItHas(t *testing.T) {
	var r report
	r.wireError("", &kelsonv1alpha1.Error{
		Code: "renderer/unsupported", Application: "web", Overlay: "overlays/patch.yaml",
		Target: "Deployment/web", Message: "no Gateway API on this cluster",
		Remediation: "install a Gateway implementation or drop spec.routing", Line: 4,
	})
	out := r.String()
	for _, want := range []string{
		"code: renderer/unsupported",
		"application: web",
		"overlay: overlays/patch.yaml",
		"target: Deployment/web",
		"message: no Gateway API on this cluster",
		"remediation: install a Gateway implementation or drop spec.routing",
		"position: line 4",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the rendered error does not carry %q:\n%s", want, out)
		}
	}
}

// TestNewDefaultsToTheLoopbackServer: the default address is the one
// kelson-server listens on, so the common case needs no configuration.
func TestNewDefaultsToTheLoopbackServer(t *testing.T) {
	if DefaultServer != "http://127.0.0.1:8420" {
		t.Errorf("DefaultServer = %q, want kelson-server's own --listen default", DefaultServer)
	}
	if server := New(Options{}); server == nil || len(server.tools) != 7 {
		t.Errorf("New with no options must still register the whole surface")
	}
}

// TestIdempotencyKeysAreUnique: a retry reuses a key deliberately; two separate
// calls must never share one by accident.
func TestIdempotencyKeysAreUnique(t *testing.T) {
	first, second := newIdempotencyKey(), newIdempotencyKey()
	if first == "" || first == second {
		t.Errorf("idempotency keys %q and %q are not distinct", first, second)
	}
	if !strings.HasPrefix(first, "mcp-") {
		t.Errorf("key %q does not say where it came from", first)
	}
}
