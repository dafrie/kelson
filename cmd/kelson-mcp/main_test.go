package main

import (
	"io"
	"testing"

	"github.com/dafrie/kelson/internal/mcp"
)

// TestParseFlagsResolvesTheServerAddress: the flag wins, then the environment,
// then kelson-server's own default. The environment matters because an MCP
// client configuration is usually a command plus an env block, and asking a
// user to express the address twice is how the two drift apart.
func TestParseFlagsResolvesTheServerAddress(t *testing.T) {
	tests := []struct {
		name string
		args []string
		env  string
		want string
	}{
		{name: "default", want: mcp.DefaultServer},
		{name: "environment", env: "http://kelson.internal:8420", want: "http://kelson.internal:8420"},
		{name: "flag wins", args: []string{"--server", "http://127.0.0.1:9000"}, env: "http://kelson.internal:8420", want: "http://127.0.0.1:9000"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("KELSON_SERVER", tt.env)
			got, err := parseFlags(tt.args, io.Discard)
			if err != nil {
				t.Fatalf("parseFlags: %v", err)
			}
			if got != tt.want {
				t.Errorf("address = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestParseFlagsRejectsPositionalArguments: kelson-mcp is launched by an MCP
// client from a configuration file, and a stray argument there is a typo that
// must fail loudly rather than be ignored.
func TestParseFlagsRejectsPositionalArguments(t *testing.T) {
	if _, err := parseFlags([]string{"serve"}, io.Discard); err == nil {
		t.Error("a positional argument was accepted")
	}
}
