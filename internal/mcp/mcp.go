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
// before that, so the surface is seven task-shaped tools — what an agent is
// trying to do, not what a resource is called. Each declares the RPCs it
// composes (tools.go), and that table is asserted against the generated service
// descriptors: the Protobuf schema as a consistency check rather than as the
// design (ADR-0008).
//
// # Everything a tool returns is bounded
//
// The API streams and a context window cannot. Every list a tool renders is
// capped and says when it truncated, the log tools are windows over
// LogService.QueryLogs and never FollowLogs, and the streaming RPCs (Deploy,
// Rollback, Watch) are consumed to a settled answer rather than forwarded.
//
// # v0 has no authentication
//
// kelson-server binds loopback and authenticates nobody (ADR-0013 §3), so this
// server holds no credential and passes none. Agent identities are issue #74.
// Policy-aware tool exposure (ADR-0008 §4) needs the policy engine of issue
// #75 — there is nothing yet that could answer "may this caller deploy to
// production", so every tool is exposed to every caller, and docs/mcp.md says
// so rather than implying a boundary that is not enforced.
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

	c := &clients{
		addr:   addr,
		spec:   kelsonv1alpha1connect.NewSpecServiceClient(httpClient, addr),
		deploy: kelsonv1alpha1connect.NewDeployServiceClient(httpClient, addr),
		logs:   kelsonv1alpha1connect.NewLogServiceClient(httpClient, addr),
		events: kelsonv1alpha1connect.NewEventServiceClient(httpClient, addr),
	}

	srv := mcpsdk.NewServer(&mcpsdk.Implementation{
		Name:    "kelson",
		Title:   "kelson",
		Version: version,
		Description: "Deploy, diagnose and roll back applications on a Kubernetes cluster through kelson-server. " +
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
