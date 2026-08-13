package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"github.com/dafrie/kelson/internal/api"
	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1/kelsonv1alpha1connect"
	"github.com/dafrie/kelson/internal/version"
)

// TestCheckBindRefusesNonLoopback is the v0 trust model (ADR-0013 §3): the
// server has no authentication and no TLS, so binding beyond loopback must be
// a deliberate act with the risk named, not a flag anyone sets by accident.
func TestCheckBindRefusesNonLoopback(t *testing.T) {
	for _, listen := range []string{"0.0.0.0:8420", ":8420", "192.168.1.10:8420", "[::]:8420"} {
		err := checkBind(listen, false)
		if err == nil {
			t.Errorf("checkBind(%q) allowed a non-loopback bind", listen)
			continue
		}
		if !strings.Contains(err.Error(), "#84") {
			t.Errorf("checkBind(%q) does not name the threat model: %v", listen, err)
		}
		if !strings.Contains(err.Error(), "--insecure-bind") {
			t.Errorf("checkBind(%q) does not name the escape hatch: %v", listen, err)
		}
		if err := checkBind(listen, true); err != nil {
			t.Errorf("checkBind(%q, insecure) = %v, want nil", listen, err)
		}
	}
}

func TestCheckBindAllowsLoopback(t *testing.T) {
	for _, listen := range []string{"127.0.0.1:8420", "localhost:8420", "[::1]:8420", "127.0.0.5:1"} {
		if err := checkBind(listen, false); err != nil {
			t.Errorf("checkBind(%q) = %v, want nil", listen, err)
		}
	}
	if err := checkBind("not-an-address", false); err == nil {
		t.Error("checkBind accepted an address with no port")
	}
}

func TestParseFlagDefaults(t *testing.T) {
	cfg, err := parseFlags(nil, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if cfg.listen != "127.0.0.1:8420" {
		t.Errorf("listen = %q, want the loopback default", cfg.listen)
	}
	if cfg.namespace != defaultNamespace {
		t.Errorf("namespace = %q, want %q", cfg.namespace, defaultNamespace)
	}
	if cfg.insecureBind {
		t.Error("insecure-bind defaults to true")
	}
	if _, err := parseFlags([]string{"--namespace", ""}, &bytes.Buffer{}); err == nil {
		t.Error("an empty --namespace was accepted; it is where the state ConfigMaps live")
	}
	if _, err := parseFlags([]string{"serve"}, &bytes.Buffer{}); err == nil {
		t.Error("a positional argument was accepted")
	}
}

// TestHealthzServes: liveness plus the build it is reporting for — "the server
// is up" and "the server is the build you deployed" are asked at once.
func TestHealthzServes(t *testing.T) {
	srv := httptest.NewServer(newMux(api.New(api.Options{})))
	defer srv.Close()

	res, err := srv.Client().Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer res.Body.Close() //nolint:errcheck // read-only handle
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decoding health: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("status = %q", body["status"])
	}
	if body["version"] != version.Version || body["commit"] != version.Commit {
		t.Errorf("health = %v, want the build metadata", body)
	}
}

// TestMuxServesTheSchema is the #139 acceptance in its narrowest form: the
// binary's mux answers a v1alpha1 RPC. Render needs no cluster, so it is the
// one that proves routing, codec and schema without any wiring.
func TestMuxServesTheSchema(t *testing.T) {
	srv := httptest.NewServer(newMux(api.New(api.Options{})))
	defer srv.Close()

	client := kelsonv1alpha1connect.NewRenderServiceClient(srv.Client(), srv.URL)
	res, err := client.Render(context.Background(), connect.NewRequest(&kelsonv1alpha1.RenderRequest{
		Spec: &kelsonv1alpha1.SpecRef{Spec: &kelsonv1alpha1.SpecRef_Documents{
			Documents: &kelsonv1alpha1.SpecDocuments{
				Project: []byte(`apiVersion: kelson.dev/v1alpha1
kind: Project
metadata:
  name: hello
spec:
  image: ghcr.io/acme/hello:1.4.2
  components:
    - name: web
      port: 8080
`),
				Environments: map[string][]byte{"development": []byte(`apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata:
  name: development
spec:
  project: hello
`)},
			},
		}},
		Environment: "development",
	}))
	if err != nil {
		t.Fatalf("Render over the server's mux: %v", err)
	}
	if len(res.Msg.GetManifests()) == 0 {
		t.Fatalf("the mux served no manifests (errors: %v)", res.Msg.GetErrors())
	}
}
