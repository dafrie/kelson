package registry_test

import (
	"strings"
	"testing"

	"github.com/dafrie/kelson/internal/build/registry"
)

// TestParseInsecureSplitsHosts: the flag and the environment variable carry a
// comma-separated list, and both callers (the CLI and the server) have to read
// it identically — sloppy input is normalised, malformed input is refused.
func TestParseInsecureSplitsHosts(t *testing.T) {
	got, err := registry.ParseInsecure(" localhost:5000, registry.kelson-system.svc.cluster.local:5000 ,")
	if err != nil {
		t.Fatalf("ParseInsecure: %v", err)
	}
	want := []string{"localhost:5000", "registry.kelson-system.svc.cluster.local:5000"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("ParseInsecure = %v, want %v", got, want)
	}
	if got, err := registry.ParseInsecure(""); err != nil || got != nil {
		t.Errorf("an empty list is no hosts, got %v, %v", got, err)
	}
}

// TestParseInsecureRefusesNonHosts: an entry that is a URL or a repository
// would match no image, so the push would fail on TLS with nothing pointing at
// the typo that caused it.
func TestParseInsecureRefusesNonHosts(t *testing.T) {
	for _, entry := range []string{
		"http://localhost:5000",
		"localhost:5000/acme",
		"localhost:notaport",
		"local host",
	} {
		if _, err := registry.ParseInsecure(entry); err == nil {
			t.Errorf("ParseInsecure(%q) must be refused", entry)
		}
	}
}

// TestIsInsecureMatchesTheHostOnly: the decision is per registry host, using
// the same host/namespace split Parse applies, so a listed host does not make
// every other registry insecure.
func TestIsInsecureMatchesTheHostOnly(t *testing.T) {
	hosts := []string{"localhost:5000"}
	cases := map[string]bool{
		"localhost:5000/acme/web":            true,
		"localhost:5000/acme/web:v1":         true,
		"LOCALHOST:5000/acme/web":            true,
		"ghcr.io/acme/web":                   false,
		"localhost/acme/web":                 false,
		"registry.example.com:5000/acme/web": false,
		"":                                   false,
	}
	for ref, want := range cases {
		if got := registry.IsInsecure(hosts, ref); got != want {
			t.Errorf("IsInsecure(%q) = %v, want %v", ref, got, want)
		}
	}
	if registry.IsInsecure(nil, "localhost:5000/acme/web") {
		t.Error("with no hosts configured nothing is insecure")
	}
}
