// Package mcp is kelson's agent surface: a Model Context Protocol server over
// the v1alpha1 ConnectRPC API (issue #73, ADR-0008).
//
// # Capability parity, not surface parity
//
// Every tool here decomposes into calls any other client could make — the CLI,
// the UI, curl. Nothing in this package reads a cluster, classifies a
// workload's health or decides what a delivery phase means: it composes
// SpecService, DeployService, LogService and EventService and relays their
// answers verbatim, including their structured errors. That is what keeps the
// agent surface from becoming a privileged backdoor with its own logic, its own
// bugs and capabilities nobody else can audit.
//
// The *shape* is deliberately not the API's. A one-to-one mapping would make
// sixty endpoints sixty tools, and model tool-selection accuracy degrades well
// before that, so the surface is a handful of task-shaped tools — what an agent
// is trying to do, not what a resource is called. Each declares the RPCs it
// composes (tools.go), and that table is asserted against the generated service
// descriptors: the Protobuf schema as a consistency check rather than as the
// design (ADR-0008).
//
// # One tool receives a credential, and none returns one
//
// set_secret carries values to the server (issue #116, ADR-0009). Nothing comes
// back: SecretService's responses have no field a value could arrive in, so a
// tool answer here reports names, keys and ages and cannot report a value even
// by accident. The value an agent passes in is a value the agent already had —
// this surface is not a way to read one out of a cluster.
//
// # Everything a tool returns is bounded
//
// The API streams and a context window cannot. Every list a tool renders is
// capped and says when it truncated, the log tools are windows over
// LogService.QueryLogs and never FollowLogs, and the streaming RPCs (Deploy,
// Rollback, Watch) are consumed to a settled answer rather than forwarded.
//
// # Two credentials, and only one of them is an identity
//
// A kelson-server started with --password requires every RPC to authenticate,
// and this server does it the way a non-browser client does: a secret as
// `Authorization: Bearer` on every call. Which secret is the whole of issue #74.
//
// [Options.Token] is an agent identity's credential ($KELSON_AGENT_TOKEN),
// issued by `kelson agent create`. With one set, every RPC this process makes is
// attributed to that identity, bounded by its scope and its expiry, and revoking
// it stops this process and nothing else. That is the flow: issue the token,
// configure this server with it, and the agent acts under its own principal.
//
// [Options.Password] is the fallback ($KELSON_PASSWORD, #84's interim cut). It
// is a shared secret, not a principal: it says the caller may reach the server,
// never who the caller is. The token wins when both are set.
//
// A server without either takes anything, and this server sends nothing — the
// pre-#84 behaviour, unchanged.
//
// Policy-aware tool exposure (ADR-0008 §4) needs the policy engine of issue #75.
// The scope on an agent token is enforced by the server on every call, but this
// process does not read it, so a tool the identity may not use is still offered
// and fails at call time with `auth/out-of-scope` rather than being hidden.
// docs/mcp.md says so rather than implying a boundary that is not there.
package mcp

import (
	"context"
	"net/http"

	"connectrpc.com/connect"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1/kelsonv1alpha1connect"
)

// DefaultServer is where kelson-server listens unless told otherwise
// (cmd/kelson-server's --listen default).
const DefaultServer = "http://127.0.0.1:8420"

// Options configures a [Server].
type Options struct {
	// Server is the base URL of a kelson-server. Empty selects [DefaultServer].
	Server string
	// HTTPClient carries the RPCs. Empty selects http.DefaultClient. It exists
	// for tests, which point the clients at an httptest server.
	HTTPClient connect.HTTPClient
	// Version is reported to the MCP client in the initialize handshake.
	Version string
	// Password is the kelson-server shared password (#84's interim cut). When
	// set it rides on every RPC as `Authorization: Bearer`. Empty sends no
	// header at all, which is what a server without a password expects.
	Password string
	// Token is an agent identity's credential (issue #74). It travels in the
	// same header as the password and takes precedence over it: an agent that
	// has been given an identity of its own must act as that identity, not as
	// whoever configured the process. Sending both would be a choice the server
	// makes rather than this one, and the wrong half could win.
	Token string
}

// credential picks which secret rides on the RPCs. The agent token wins
// whenever there is one — see [Options.Token].
func (o Options) credential() string {
	if o.Token != "" {
		return o.Token
	}
	return o.Password
}

// CredentialKind names the credential for a banner without revealing it. It is
// exported because cmd/kelson-mcp prints it and must not re-derive the
// precedence rule and get it wrong.
func (o Options) CredentialKind() string {
	switch {
	case o.Token != "":
		return "agent identity"
	case o.Password != "":
		return "shared password"
	default:
		return "no credential"
	}
}

// Server is the MCP server and its tool surface.
type Server struct {
	mcp *mcpsdk.Server
	// tools is the surface as registered, kept so the consistency check can
	// walk the same table the registration used rather than a second copy of it.
	tools []tool
}

// New builds the server and registers every tool.
func New(opts Options) *Server {
	addr := opts.Server
	if addr == "" {
		addr = DefaultServer
	}
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	version := opts.Version
	if version == "" {
		version = "0.0.0-dev"
	}

	auth := bearerOptions(opts.credential())
	c := &clients{
		addr:    addr,
		spec:    kelsonv1alpha1connect.NewSpecServiceClient(httpClient, addr, auth...),
		deploy:  kelsonv1alpha1connect.NewDeployServiceClient(httpClient, addr, auth...),
		logs:    kelsonv1alpha1connect.NewLogServiceClient(httpClient, addr, auth...),
		events:  kelsonv1alpha1connect.NewEventServiceClient(httpClient, addr, auth...),
		secrets: kelsonv1alpha1connect.NewSecretServiceClient(httpClient, addr, auth...),
		profile: kelsonv1alpha1connect.NewProfileServiceClient(httpClient, addr, auth...),
		explain: kelsonv1alpha1connect.NewExplainServiceClient(httpClient, addr, auth...),
	}

	srv := mcpsdk.NewServer(&mcpsdk.Implementation{
		Name:    "kelson",
		Title:   "kelson",
		Version: version,
		Description: "Deploy, diagnose and roll back workloads on a Kubernetes cluster through kelson-server. " +
			"Tools are task-shaped compositions of kelson's v1alpha1 API; mutating tools all offer a dry run.",
		WebsiteURL: "https://kelson.dev",
	}, nil)

	s := &Server{mcp: srv, tools: surface(c)}
	for _, t := range s.tools {
		t.add(srv)
	}
	return s
}

// Run serves one MCP session over transport until the peer disconnects or ctx
// is cancelled.
func (s *Server) Run(ctx context.Context, transport mcpsdk.Transport) error {
	return s.mcp.Run(ctx, transport)
}
