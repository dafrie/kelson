package mcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1/kelsonv1alpha1connect"
)

// clients is the generated ConnectRPC client set every tool composes, plus the
// address they point at — which the connection error text needs, because "the
// server did not answer" is useless without saying which server.
//
// Only the services the surface actually uses are here. RenderService has no
// tool: rendering a spec to YAML is not a task an agent does — it is an input to
// tasks the tools already compose — and every tool that exists costs selection
// accuracy for the ones that matter (ADR-0008).
//
// ProfileService has no tool of its own for the same reason, and is still here:
// capturing a cluster profile is not a task, but the version skew in it is
// something an agent needs *while* diagnosing one, which is precisely when
// ADR-0008 says to extend an existing tool rather than add a read tool beside
// it. diagnose_application composes it (issue #57).
type clients struct {
	addr    string
	spec    kelsonv1alpha1connect.SpecServiceClient
	deploy  kelsonv1alpha1connect.DeployServiceClient
	logs    kelsonv1alpha1connect.LogServiceClient
	events  kelsonv1alpha1connect.EventServiceClient
	secrets kelsonv1alpha1connect.SecretServiceClient
	profile kelsonv1alpha1connect.ProfileServiceClient
	explain kelsonv1alpha1connect.ExplainServiceClient
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
		r.addf("kelson-server at %s did not accept this process's credential. Either set --token (or "+
			"KELSON_AGENT_TOKEN) to an agent credential from `kelson agent create`, or set --password (or "+
			"KELSON_PASSWORD) to the shared password the server was started with; both are sent as an "+
			"Authorization: Bearer header. An agent token that was revoked or has expired fails here too, and is "+
			"replaced rather than repaired.", c.addr)
	case connect.CodePermissionDenied:
		// The scope is enforced server-side and this process does not read it,
		// so the only useful thing to say is which identity was refused and
		// that widening it is an operator's act, not a retry.
		r.addf("this agent identity is not allowed to do that. The refusal is server-side and final for this " +
			"credential: an operator widens the identity's scope with a new `kelson agent create`, or the request " +
			"names a project and environment the identity covers. Retrying changes nothing.")
	case connect.CodeResourceExhausted:
		r.addf("this agent identity is over its request budget. Back off and retry — the budget refills " +
			"continuously — or ask an operator for a credential with a higher --rate.")
	default:
	}
	return errors.New(r.String())
}

// bearerOptions carries the process's credential on every call, or nothing at
// all. The value is whichever [Options.credential] selected — an agent token
// when there is one, the shared password otherwise.
//
// It is a client option rather than a header set at each call site so that a
// tool added later cannot forget it, and it covers streaming as well as unary
// because Watch and the deploy stream are exactly the calls a half-applied
// credential would break last and most confusingly (#84's interim cut, #74).
func bearerOptions(credential string) []connect.ClientOption {
	if credential == "" {
		return nil
	}
	return []connect.ClientOption{connect.WithInterceptors(bearer(credential))}
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

// ReasonHeader is the request header a caller's stated reason travels in. It
// mirrors api.ReasonHeader, which this package deliberately does not import: a
// stdio sidecar should not link the whole server plane for one string constant.
// TestReasonHeaderMatchesTheServer asserts the two are the same, so the copy
// cannot drift.
const ReasonHeader = "Kelson-Reason"

// maxReason bounds what this process will send. The server bounds it again on
// arrival; this bound exists so a model that pastes a stack trace into `reason`
// does not put it on the wire, and the truncation is marked so the recorded
// reason is never quietly half a sentence.
const maxReason = 512

// reasoned attaches the caller's stated reason to an outbound request (issue
// #78, ADR-0026 §4).
//
// It is why an agent's audit record can say *why* rather than only what. The
// server records what arrives and invents nothing, so a tool call with no
// reason produces a record with no reason — which is the honest answer, and the
// reason the parameter is optional on every tool that has it.
func reasoned[T any](req *connect.Request[T], reason string) *connect.Request[T] {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return req
	}
	if len(reason) > maxReason {
		reason = reason[:maxReason] + "…[truncated]"
	}
	// A header value cannot carry a line break, and a reason is prose that
	// might. Folding to spaces keeps the whole sentence rather than dropping
	// the value or the tail of it.
	req.Header().Set(ReasonHeader, strings.Join(strings.Fields(reason), " "))
	return req
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
