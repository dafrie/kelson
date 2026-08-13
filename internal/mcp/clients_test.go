package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1/kelsonv1alpha1connect"
)

// TestFailNamesTheAddressOnAConnectionFailure: a server that did not answer is
// the failure an agent will hit most often, and "unauthorized" is exactly the
// wrong conclusion to let it draw. The address and how to change it are both in
// the message.
func TestFailNamesTheAddressOnAConnectionFailure(t *testing.T) {
	c := &clients{addr: "http://127.0.0.1:8420"}
	err := c.fail(rpcStatus, connect.NewError(connect.CodeUnavailable, errors.New("dial tcp: connection refused")))

	for _, want := range []string{
		"kelson.v1alpha1.DeployService.Status failed: unavailable",
		"kelson-server at http://127.0.0.1:8420 did not answer",
		"KELSON_SERVER",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the connection error does not mention %q:\n%s", want, err)
		}
	}
	if strings.Contains(err.Error(), "shared password") {
		t.Errorf("a connection failure was reported as a credential problem:\n%s", err)
	}
}

// TestFailNamesTheCredentialOnA401: the interim auth (#84) makes "you have no
// password" a failure an agent can now hit, and it is the one failure where
// retrying the same call forever is the wrong move. The message says what to
// set, and says the password is not an identity so an agent does not read it as
// having been granted one.
func TestFailNamesTheCredentialOnA401(t *testing.T) {
	c := &clients{addr: "http://127.0.0.1:8420"}
	err := c.fail(rpcStatus, connect.NewError(connect.CodeUnauthenticated, errors.New("kelson-server requires a session")))

	for _, want := range []string{
		"KELSON_PASSWORD",
		"Authorization: Bearer",
		"not an agent identity",
		"#74",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the 401 does not mention %q:\n%s", want, err)
		}
	}
	if strings.Contains(err.Error(), "did not answer") {
		t.Errorf("a rejected request was reported as a connection failure:\n%s", err)
	}
}

// TestBearerOptionsSendTheCredentialOnlyWhenThereIsOne: no password must mean
// no header at all, because a server without one has no way to distinguish
// "empty bearer" from a client bug, and an empty Authorization header is the
// kind of thing a proxy in between rejects on its own.
func TestBearerOptionsSendTheCredentialOnlyWhenThereIsOne(t *testing.T) {
	if got := bearerOptions(""); got != nil {
		t.Errorf("bearerOptions(\"\") = %v, want no client options", got)
	}
	if got := bearerOptions("hunter2"); len(got) != 1 {
		t.Errorf("bearerOptions with a password produced %d options, want 1", len(got))
	}

	// The interceptor's own contract: the header it sets on a unary call.
	req := connect.NewRequest(&kelsonv1alpha1.GetSpecRequest{})
	_, _ = bearer("hunter2").WrapUnary(func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
		return nil, nil //nolint:nilnil // the interceptor under test never looks at the answer
	})(context.Background(), req)
	if got := req.Header().Get("Authorization"); got != "Bearer hunter2" {
		t.Errorf("unary Authorization = %q, want the bearer password", got)
	}
}

// TestThePasswordRidesOnEveryCall drives a real tool through a real transport
// against a server that records what it was sent. Asserting the interceptor in
// isolation would not catch the mistake that matters — a client constructed
// without the option — and the streaming leg is asserted separately because a
// credential applied to unary calls alone fails last and most confusingly, on
// the watch an agent left running.
func TestThePasswordRidesOnEveryCall(t *testing.T) {
	fake := &fakeServer{
		listSpecs: func(*kelsonv1alpha1.ListSpecsRequest) (*kelsonv1alpha1.ListSpecsResponse, error) {
			return &kelsonv1alpha1.ListSpecsResponse{}, nil
		},
		watch: func(*kelsonv1alpha1.WatchRequest, *connect.ServerStream[kelsonv1alpha1.WatchResponse]) error {
			return nil
		},
	}
	h := startWith(t, fake, "hunter2")
	h.call(t, "list_applications", map[string]any{})

	events := kelsonv1alpha1connect.NewEventServiceClient(h.client, h.address, bearerOptions("hunter2")...)
	stream, err := events.Watch(context.Background(), connect.NewRequest(&kelsonv1alpha1.WatchRequest{
		Scopes: []*kelsonv1alpha1.WatchRequest_Scope{{Project: "hello"}},
	}))
	if err != nil {
		t.Fatalf("opening the watch stream: %v", err)
	}
	for stream.Receive() { //nolint:revive // draining is the point; the fake sends nothing
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("closing the watch stream: %v", err)
	}

	creds := fake.credentials()
	if len(creds) < 2 {
		t.Fatalf("the server saw %d requests, want the unary tool call and the stream", len(creds))
	}
	for i, got := range creds {
		if got != "Bearer hunter2" {
			t.Errorf("request %d (%s) carried Authorization %q, want the bearer password",
				i, fake.procedures()[i], got)
		}
	}
}

// TestNoPasswordSendsNoHeader: a server without a password must see exactly
// what it saw before #84 — nothing.
func TestNoPasswordSendsNoHeader(t *testing.T) {
	fake := &fakeServer{
		listSpecs: func(*kelsonv1alpha1.ListSpecsRequest) (*kelsonv1alpha1.ListSpecsResponse, error) {
			return &kelsonv1alpha1.ListSpecsResponse{}, nil
		},
	}
	h := start(t, fake)
	h.call(t, "list_applications", map[string]any{})

	for i, got := range fake.credentials() {
		if got != "" {
			t.Errorf("request %d carried Authorization %q with no password configured", i, got)
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
	if server := New(Options{}); server == nil || len(server.tools) != 8 {
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
