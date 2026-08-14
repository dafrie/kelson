// Command kelson-mcp is kelson's Model Context Protocol server (issue #73,
// ADR-0008).
//
// # A sidecar, not a service
//
// It speaks MCP over stdio and holds no state: an MCP client (an agent, an
// editor) launches it as a child process, and it turns tool calls into
// ConnectRPC calls against a kelson-server (--server, KELSON_SERVER, default
// http://127.0.0.1:8420). There is no listener, no port and therefore no
// container image — the process lives and dies with the client that started it.
//
// # The credential it carries decides who the agent is
//
// Two are accepted and one of them is a principal (issue #74).
//
//   - --token / KELSON_AGENT_TOKEN is an agent identity's credential, issued by
//     `kelson agent create`. Every RPC this process makes is then attributed to
//     that identity, bounded by its scope and its expiry, and revoking it stops
//     this process and nothing else. This is the credential to give an agent.
//   - --password / KELSON_PASSWORD is kelson-server's shared password (#84's
//     interim cut). It says the caller may reach the server, never who the
//     caller is.
//
// The token wins when both are set. Without either, nothing is sent, which is
// what a server without a password expects. Either way the address must be one
// this process can reach directly.
//
// Policy-aware tool exposure (ADR-0008 §4) waits on the policy engine of issue
// #75: an identity's scope is enforced by the server on every call, but it does
// not change which tools this process offers, so an out-of-scope tool fails at
// call time rather than being hidden.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dafrie/kelson/internal/mcp"
	"github.com/dafrie/kelson/internal/version"
)

func main() { os.Exit(cli()) }

// cli exists so main holds no deferred work: os.Exit skips defers, and the
// signal handler's stop must run before the process leaves.
func cli() int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err) //nolint:errcheck // nothing to do if stderr is gone
		return 1
	}
	return 0
}

func run(ctx context.Context, args []string, stderr io.Writer) error {
	cfg, err := parseFlags(args, stderr)
	if err != nil {
		return err
	}

	// Diagnostics go to stderr, never stdout: stdout IS the protocol here, and
	// one stray line of prose on it corrupts the session. The credential itself
	// is never printed — only which kind it is, which is the part an operator
	// debugging a 401 needs.
	options := mcp.Options{Server: cfg.address, Version: version.String(), Password: cfg.password, Token: cfg.token}
	credential := options.CredentialKind()
	fmt.Fprintf(stderr, "kelson-mcp %s serving the MCP tool surface over stdio against %s (%s)\n", //nolint:errcheck // a lost banner must not stop the server
		version.String(), cfg.address, credential)

	server := mcp.New(options)
	if err := server.Run(ctx, &mcpsdk.StdioTransport{}); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// passwordEnv is the shared password kelson-server was started with. It is the
// preferred way to pass it — an MCP client configuration's `env` block keeps it
// out of the argv every `ps` on the machine can read.
const passwordEnv = "KELSON_PASSWORD"

// tokenEnv is the agent identity's credential (issue #74). The same argument as
// passwordEnv applies with more force: this one names a principal, so a token
// in the argv leaks a credential and an identity at once.
const tokenEnv = "KELSON_AGENT_TOKEN"

// config is this process's resolved command line.
type config struct {
	address  string
	password string
	token    string
}

// parseFlags resolves the server address and the credentials: the flag wins,
// then the environment, then (for the address) the default kelson-server
// listens on. An absent credential is not an error — a server without one takes
// anything, and the 401 from a server with one says exactly what to set.
func parseFlags(args []string, stderr io.Writer) (config, error) {
	fs := flag.NewFlagSet("kelson-mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	address := fs.String("server", "", "base URL of a kelson-server (default $KELSON_SERVER, then "+mcp.DefaultServer+")")
	password := fs.String("password", "", "kelson-server's shared password, sent as an Authorization: Bearer header (default $"+passwordEnv+")")
	token := fs.String("token", "", "an agent identity's credential from `kelson agent create`, sent as an Authorization: Bearer "+
		"header (default $"+tokenEnv+"); it takes precedence over --password and is what makes this process a principal of its own")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	if fs.NArg() > 0 {
		return config{}, fmt.Errorf("unexpected argument %q: kelson-mcp takes flags only", fs.Arg(0))
	}
	cfg := config{address: *address, password: *password, token: *token}
	if cfg.address == "" {
		cfg.address = os.Getenv("KELSON_SERVER")
	}
	if cfg.address == "" {
		cfg.address = mcp.DefaultServer
	}
	if cfg.password == "" {
		cfg.password = os.Getenv(passwordEnv)
	}
	if cfg.token == "" {
		cfg.token = os.Getenv(tokenEnv)
	}
	return cfg, nil
}
