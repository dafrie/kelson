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
// # v0 carries no credential
//
// kelson-server has no authentication in v0 and binds loopback (ADR-0013 §3),
// so this process sends none: it must be able to reach the server directly, on
// a machine that is allowed to. Agent identities are issue #74, and policy-aware
// tool exposure (ADR-0008 §4) waits on the policy engine of issue #75 — until
// then every tool is offered to every caller.
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
	address, err := parseFlags(args, stderr)
	if err != nil {
		return err
	}

	// Diagnostics go to stderr, never stdout: stdout IS the protocol here, and
	// one stray line of prose on it corrupts the session.
	fmt.Fprintf(stderr, "kelson-mcp %s serving the MCP tool surface over stdio against %s\n", //nolint:errcheck // a lost banner must not stop the server
		version.String(), address)

	server := mcp.New(mcp.Options{Server: address, Version: version.String()})
	if err := server.Run(ctx, &mcpsdk.StdioTransport{}); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// parseFlags resolves the server address: the flag wins, then the environment,
// then the default kelson-server listens on.
func parseFlags(args []string, stderr io.Writer) (string, error) {
	fs := flag.NewFlagSet("kelson-mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	address := fs.String("server", "", "base URL of a kelson-server (default $KELSON_SERVER, then "+mcp.DefaultServer+")")
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	if fs.NArg() > 0 {
		return "", fmt.Errorf("unexpected argument %q: kelson-mcp takes flags only", fs.Arg(0))
	}
	switch {
	case *address != "":
		return *address, nil
	case os.Getenv("KELSON_SERVER") != "":
		return os.Getenv("KELSON_SERVER"), nil
	default:
		return mcp.DefaultServer, nil
	}
}
