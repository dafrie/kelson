package mcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1/kelsonv1alpha1connect"
)

// clients is the generated ConnectRPC client set every tool composes, plus the
// address they point at — which the connection error text needs, because "the
// server did not answer" is useless without saying which server.
//
// Only the four services the surface actually uses are here. RenderService and
// ProfileService have no tool: rendering a spec to YAML and capturing a cluster
// profile are not tasks an agent does — they are inputs to tasks the tools
// already compose — and every tool that exists costs selection accuracy for the
// ones that matter (ADR-0008).
type clients struct {
	addr   string
	spec   kelsonv1alpha1connect.SpecServiceClient
	deploy kelsonv1alpha1connect.DeployServiceClient
	logs   kelsonv1alpha1connect.LogServiceClient
	events kelsonv1alpha1connect.EventServiceClient
}

// fail renders a failed RPC as a tool error.
//
// The structured details ride through verbatim (errors.go in internal/api put
// them there for exactly this reader): an agent branches on `code` and acts on
// `remediation`, so paraphrasing either would replace the only machine-readable
// part of the failure with prose.
func (c *clients) fail(op rpc, err error) error {
	var r report
	r.addf("%s failed: %s", op, connectMessage(err))

	var cerr *connect.Error
	if errors.As(err, &cerr) {
		for _, detail := range cerr.Details() {
			value, derr := detail.Value()
			if derr != nil {
				continue
			}
			if wire, ok := value.(*kelsonv1alpha1.Error); ok {
				r.wireError("  ", wire)
			}
		}
	}

	switch connect.CodeOf(err) {
	case connect.CodeUnavailable, connect.CodeUnknown:
		r.addf("kelson-server at %s did not answer. Start it and point --server (or KELSON_SERVER) at its address. "+
			"The address must be one this process can reach directly — there is no proxy and no discovery here.", c.addr)
	case connect.CodeUnauthenticated:
		r.addf("kelson-server at %s requires its shared password. Set --password (or KELSON_PASSWORD) to the value "+
			"the server was started with; it is sent as an Authorization: Bearer header. The password is a shared "+
			"secret, not an agent identity — those are issue #74.", c.addr)
	default:
	}
	return errors.New(r.String())
}

// bearerOptions carries the shared password on every call, or nothing at all.
//
// It is a client option rather than a header set at each call site so that a
// tool added later cannot forget it, and it covers streaming as well as unary
// because Watch and the deploy stream are exactly the calls a half-applied
// credential would break last and most confusingly (#84's interim cut).
func bearerOptions(password string) []connect.ClientOption {
	if password == "" {
		return nil
	}
	return []connect.ClientOption{connect.WithInterceptors(bearer(password))}
}

// bearer is the interceptor that sets Authorization on outbound requests.
type bearer string

var _ connect.Interceptor = bearer("")

func (b bearer) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		req.Header().Set("Authorization", "Bearer "+string(b))
		return next(ctx, req)
	}
}

func (b bearer) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		conn.RequestHeader().Set("Authorization", "Bearer "+string(b))
		return conn
	}
}

// WrapStreamingHandler is the server half of the interface and is never used:
// this package is a client.
func (b bearer) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

// connectMessage is the failure without connect's own code prefix duplicated.
func connectMessage(err error) string {
	var cerr *connect.Error
	if errors.As(err, &cerr) {
		return connect.CodeOf(err).String() + ": " + cerr.Message()
	}
	return err.Error()
}

// specRef addresses a stored project. Tools never take inline documents for
// anything but put_spec: an agent that has to resend the whole spec on every
// call is paying context for something the server already has.
func specRef(project string) *kelsonv1alpha1.SpecRef {
	return &kelsonv1alpha1.SpecRef{Spec: &kelsonv1alpha1.SpecRef_Project{Project: project}}
}

// newIdempotencyKey mints the key a mutating call carries so a retry after a
// timeout is the same operation rather than a second one (issue #71). It is
// returned to the caller in the tool's answer, because an agent that retries
// must be able to reuse it.
func newIdempotencyKey() string {
	var buf [12]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand does not fail on any supported platform; if it somehow
		// does, an empty key means "no idempotency" — the pre-#71 behaviour —
		// which is safer than a constant key shared by every retry of every
		// deployment.
		return ""
	}
	return "mcp-" + hex.EncodeToString(buf[:])
}
