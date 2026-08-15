package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1/kelsonv1alpha1connect"
)

// deploy, rollback, promote and history (R2, issue #225) are a ConnectRPC
// client of kelson-server: the direct/git adapters they used to drive
// straight against a kubeconfig are deleted (ADR-0028), and what replaces
// them — SSA of Project/Environment custom resources, a status watch, the
// rollback annotation, the promotion splice — lives behind internal/api and is
// reached the same way kelson-mcp reaches it (internal/mcp/mcp.go): an address,
// an optional credential, over the wire surface the protos already define.
//
// This file is that client's one seam. It exists in cmd/kelson rather than
// importing internal/mcp's because the two binaries have nothing else in
// common — an MCP sidecar and a human's CLI render answers differently and
// carry no state between calls either way — and duplicating roughly forty
// lines of transport plumbing costs less than coupling a CLI's release cadence
// to an agent surface's.

// defaultServerAddr is where kelson-server listens unless told otherwise
// (cmd/kelson-server's --listen default, docs/server.md). It intentionally
// matches internal/mcp.DefaultServer rather than importing it — see the
// package comment above.
const defaultServerAddr = "http://127.0.0.1:8420"

// dialTimeout bounds how long a call waits to open the TCP connection to
// kelson-server. It is not the RPC's own deadline — Deploy can watch a
// rollout for minutes and must not be cut off mid-stream — it is the answer
// to "the server is not there": an unreachable --server must fail within a
// few seconds naming the flag, never hang (design constraint, R2).
const dialTimeout = 5 * time.Second

// requestTimeout bounds a unary call or a preview stream (dry_run=RENDER),
// none of which wait on a cluster reconcile. It is deliberately separate from
// a deploy's own --timeout, which bounds a real rollout and is set by the
// caller.
const requestTimeout = 30 * time.Second

// serverOptions is the flag/env surface every command that dials
// kelson-server carries, mirroring kelson-mcp's (cmd/kelson-mcp/main.go): the
// flag wins, then the environment, then (for the address only) the documented
// default.
type serverOptions struct {
	address  string
	password string
	token    string
}

// addServerFlags registers the connection flags shared by every façade-backed
// verb, so a change to one does not drift from the others.
func addServerFlags(cmd *cobra.Command, opts *serverOptions) {
	f := cmd.Flags()
	f.StringVar(&opts.address, "server", "",
		"base URL of a kelson-server (default $KELSON_SERVER, else "+defaultServerAddr+"); port-forward to reach one in a cluster")
	f.StringVar(&opts.password, "password", "",
		"kelson-server's shared password, sent as Authorization: Bearer (default $KELSON_PASSWORD)")
	f.StringVar(&opts.token, "token", "",
		"an agent identity's credential from `kelson agent create` (default $KELSON_AGENT_TOKEN); takes precedence over --password")
}

func (o serverOptions) resolvedAddress() string {
	if o.address != "" {
		return o.address
	}
	if v := strings.TrimSpace(os.Getenv("KELSON_SERVER")); v != "" {
		return v
	}
	return defaultServerAddr
}

func (o serverOptions) resolvedPassword() string {
	if o.password != "" {
		return o.password
	}
	return strings.TrimSpace(os.Getenv("KELSON_PASSWORD"))
}

func (o serverOptions) resolvedToken() string {
	if o.token != "" {
		return o.token
	}
	return strings.TrimSpace(os.Getenv("KELSON_AGENT_TOKEN"))
}

// credential picks which secret rides on the RPCs. The agent token wins
// whenever there is one, for the same reason internal/mcp's Options.credential
// does: an identity configured with a token of its own must act as that
// identity, never as whoever also knows the shared password.
func (o serverOptions) credential() string {
	if t := o.resolvedToken(); t != "" {
		return t
	}
	return o.resolvedPassword()
}

// deployClient builds the DeployService client for one command run, and
// returns the address it dials so callers can put it in an error message.
func (o serverOptions) deployClient() (kelsonv1alpha1connect.DeployServiceClient, string) {
	addr, opts := o.dial()
	return kelsonv1alpha1connect.NewDeployServiceClient(newServerHTTPClient(), addr, opts...), addr
}

// buildClient builds the BuildService client, which `kelson ci report-build`
// is the one caller of: `kelson build` still runs the build plane itself
// (build.go's buildConnector), and the CI hand-off is a wire call by
// construction — the whole point of ADR-0034 decision 3 is that CI reports and
// the *server* renders.
func (o serverOptions) buildClient() (kelsonv1alpha1connect.BuildServiceClient, string) {
	addr, opts := o.dial()
	return kelsonv1alpha1connect.NewBuildServiceClient(newServerHTTPClient(), addr, opts...), addr
}

// dial resolves where to call and how to authenticate, so a second service's
// client cannot drift from the first's on either.
func (o serverOptions) dial() (string, []connect.ClientOption) {
	var opts []connect.ClientOption
	if cred := o.credential(); cred != "" {
		opts = append(opts, connect.WithInterceptors(bearerAuth(cred)))
	}
	return o.resolvedAddress(), opts
}

// newServerHTTPClient bounds only the dial, never the request: a streaming
// Deploy or Rollback call can legitimately run for its whole --timeout, but
// nothing should ever hang trying to reach a --server that answers no SYN.
func newServerHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: (&net.Dialer{Timeout: dialTimeout}).DialContext,
		},
	}
}

// bearerAuth carries the process's credential on every call, unary and
// streaming alike — copied in shape from internal/mcp/clients.go's bearer,
// which cmd/kelson cannot import (see the package comment above).
type bearerAuth string

var _ connect.Interceptor = bearerAuth("")

func (b bearerAuth) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		req.Header().Set("Authorization", "Bearer "+string(b))
		return next(ctx, req)
	}
}

func (b bearerAuth) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		conn.RequestHeader().Set("Authorization", "Bearer "+string(b))
		return conn
	}
}

// WrapStreamingHandler is the server half of the interface and is never used:
// this type is a client interceptor only.
func (b bearerAuth) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

// serverError turns a failed RPC into the message a human reads at a
// terminal: what was being attempted, why it likely failed, and the flag that
// fixes it. The taxonomy code and remediation the server attached (errors.go
// in internal/api) ride through verbatim, because paraphrasing either would
// throw away the one machine-checked part of the failure.
func serverError(op, addr string, err error) error {
	var msg string
	switch connect.CodeOf(err) {
	case connect.CodeUnavailable, connect.CodeUnknown, connect.CodeDeadlineExceeded:
		msg = fmt.Sprintf("%s: kelson-server at %s did not answer. Start it and point --server (or $KELSON_SERVER) "+
			"at its address, or forward a port to it — see docs/server.md.", op, addr)
	case connect.CodeUnauthenticated:
		msg = fmt.Sprintf("%s: kelson-server at %s did not accept this process's credential. Set --password "+
			"(or $KELSON_PASSWORD) to the server's shared password, or --token (or $KELSON_AGENT_TOKEN) to an "+
			"agent credential from `kelson agent create`.", op, addr)
	case connect.CodePermissionDenied:
		msg = fmt.Sprintf("%s: kelson-server at %s refused this request.", op, addr)
	default:
		msg = fmt.Sprintf("%s: %s", op, connectMessage(err))
	}
	if detail := wireErrorDetails(err); detail != "" {
		msg += detail
	}
	return errors.New(msg)
}

// connectMessage strips connect's own "code: " prefix so it is not doubled
// with the taxonomy code already printed by wireErrorDetails.
func connectMessage(err error) string {
	var cerr *connect.Error
	if errors.As(err, &cerr) {
		return cerr.Message()
	}
	return err.Error()
}

// wireErrorDetails renders the structured kelson error attached to a failed
// RPC (internal/api/errors.go), the same detail internal/mcp surfaces to an
// agent — the code a script can branch on and the remediation a human acts on.
func wireErrorDetails(err error) string {
	var cerr *connect.Error
	if !errors.As(err, &cerr) {
		return ""
	}
	var b strings.Builder
	for _, d := range cerr.Details() {
		v, derr := d.Value()
		if derr != nil {
			continue
		}
		e, ok := v.(*kelsonv1alpha1.Error)
		if !ok {
			continue
		}
		fmt.Fprintf(&b, "\n  %s", wireErrSummary(e))
	}
	return b.String()
}

// wireErrSummary formats one structured kelson error as a line of the CLI's
// own report: the code so a script can branch on it, then the message and the
// fix — the same three pieces of detail every taxonomy in this repo carries.
func wireErrSummary(e *kelsonv1alpha1.Error) string {
	s := e.GetMessage()
	if e.GetCode() != "" {
		s = e.GetCode() + ": " + s
	}
	if e.GetRemediation() != "" {
		s += " (" + e.GetRemediation() + ")"
	}
	return s
}

// settledErr turns a stream's terminal Error (Settled/Rollback's own) into a
// Go error with the same shape serverError gives a transport failure — a
// caller reading resolveExit's message sees one vocabulary either way.
func settledErr(e *kelsonv1alpha1.Error) error {
	if e == nil {
		return errors.New("settled without reaching a healthy phase, and reported no cause")
	}
	return errors.New(wireErrSummary(e))
}

// orDash renders an optional value for a column, so an empty field reads as
// "there is nothing here" rather than as a misaligned table.
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// newIdempotencyKey mints the key a mutating call carries so a retry after a
// dropped connection is the same operation rather than a second one (issue
// #71), mirroring internal/mcp/clients.go's for the same reason: this process
// keeps no state between commands, so the caller has nothing else to reuse
// across their own retry other than the last key this printed.
func newIdempotencyKey() string {
	var buf [12]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand does not fail on any supported platform; if it somehow
		// does, an empty key means "no idempotency", which is safer than a
		// constant key shared by every retry of every command.
		return ""
	}
	return "cli-" + hex.EncodeToString(buf[:])
}
