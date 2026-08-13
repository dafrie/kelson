package mcp

import (
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
			"v0 has no authentication and no TLS: the server binds loopback only (ADR-0013 §3) and this MCP server "+
			"holds and sends no credential, so the address must be one this process can reach directly. "+
			"Agent identities are issue #74.", c.addr)
	default:
	}
	return errors.New(r.String())
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
