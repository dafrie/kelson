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
	"github.com/dafrie/kelson/internal/webui"
)

var nonLoopback = []string{"0.0.0.0:8420", ":8420", "192.168.1.10:8420", "[::]:8420"}

// TestCheckBindRefusesNonLoopbackWithoutAPassword is the trust model with no
// credential configured (ADR-0013 §3): nothing authenticates, so binding beyond
// loopback must be a deliberate act with the risk named, not a flag anyone sets
// by accident. Both ways out are in the message, because "set a password" is
// now the better one.
func TestCheckBindRefusesNonLoopbackWithoutAPassword(t *testing.T) {
	for _, listen := range nonLoopback {
		warning, err := checkBind(listen, false, false)
		if err == nil {
			t.Errorf("checkBind(%q) allowed a non-loopback bind with no password", listen)
			continue
		}
		if warning != "" {
			t.Errorf("checkBind(%q) both refused and warned: %q", listen, warning)
		}
		for _, want := range []string{"#84", "--insecure-bind", "--password"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("checkBind(%q) does not mention %q: %v", listen, want, err)
			}
		}
	}
}

// TestCheckBindAllowsNonLoopbackWithAPassword is the 2026-08-13 amendment: a
// password is what changes the fact on the ground, so it is what lifts the
// refusal. It does not lift the warning — there is still no TLS.
func TestCheckBindAllowsNonLoopbackWithAPassword(t *testing.T) {
	for _, listen := range nonLoopback {
		warning, err := checkBind(listen, false, true)
		if err != nil {
			t.Errorf("checkBind(%q, password) = %v, want it allowed", listen, err)
			continue
		}
		for _, want := range []string{"TLS", listen} {
			if !strings.Contains(warning, want) {
				t.Errorf("checkBind(%q, password) warning does not mention %q: %q", listen, want, warning)
			}
		}
	}
}

// TestCheckBindKeepsTheInsecureEscapeHatch: --insecure-bind still overrides the
// refusal for "I know, this is a private network", and it must not go quiet
// just because it worked.
func TestCheckBindKeepsTheInsecureEscapeHatch(t *testing.T) {
	for _, listen := range nonLoopback {
		warning, err := checkBind(listen, true, false)
		if err != nil {
			t.Errorf("checkBind(%q, insecure) = %v, want nil", listen, err)
			continue
		}
		if !strings.Contains(warning, "no authentication") {
			t.Errorf("checkBind(%q, insecure) warned without naming the exposure: %q", listen, warning)
		}
	}
}

func TestCheckBindAllowsLoopback(t *testing.T) {
	for _, listen := range []string{"127.0.0.1:8420", "localhost:8420", "[::1]:8420", "127.0.0.5:1"} {
		for _, password := range []bool{false, true} {
			warning, err := checkBind(listen, false, password)
			if err != nil {
				t.Errorf("checkBind(%q, password=%v) = %v, want nil", listen, password, err)
			}
			if warning != "" {
				t.Errorf("checkBind(%q) warned about a loopback bind: %q", listen, warning)
			}
		}
	}
	if _, err := checkBind("not-an-address", false, false); err == nil {
		t.Error("checkBind accepted an address with no port")
	}
	// The address is parsed before anything else: --insecure-bind means "I
	// accept the exposure", never "do not check my typing".
	if _, err := checkBind("not-an-address", true, true); err == nil {
		t.Error("--insecure-bind skipped address validation")
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
	if cfg.password != "" {
		t.Errorf("password = %q, want authentication off unless asked for", cfg.password)
	}
	if _, err := parseFlags([]string{"--namespace", ""}, &bytes.Buffer{}); err == nil {
		t.Error("an empty --namespace was accepted; it is where the state ConfigMaps live")
	}
	if _, err := parseFlags([]string{"serve"}, &bytes.Buffer{}); err == nil {
		t.Error("a positional argument was accepted")
	}
}

// TestPasswordComesFromTheEnvironment: an operator setting the password in a
// Deployment's env block is the intended path — a flag value is readable in
// every `ps` on the node — so the environment must work without the flag.
func TestPasswordComesFromTheEnvironment(t *testing.T) {
	t.Setenv(passwordEnv, "hunter2")
	cfg, err := parseFlags(nil, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if cfg.password != "hunter2" {
		t.Errorf("password = %q, want the environment's value", cfg.password)
	}
	cfg, err = parseFlags([]string{"--password", "from-flag"}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if cfg.password != "from-flag" {
		t.Errorf("password = %q, want the flag to win", cfg.password)
	}
}

// TestHealthzServes: liveness plus the build it is reporting for — "the server
// is up" and "the server is the build you deployed" are asked at once.
func TestHealthzServes(t *testing.T) {
	srv := httptest.NewServer(newMux(api.New(api.Options{}), openAuth(t)))
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
	srv := httptest.NewServer(newMux(api.New(api.Options{}), openAuth(t)))
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

// TestMuxServesTheWebUIWithoutDisturbingTheAPI is the routing half of "install
// it, port-forward the Service, click around": the same listener answers the
// SPA and the schema, and the SPA is the catch-all rather than a competitor —
// every route the server registered still wins, and a path under the API's
// prefix that it did not register answers as a missing endpoint rather than as
// a page (internal/webui).
func TestMuxServesTheWebUIWithoutDisturbingTheAPI(t *testing.T) {
	srv := httptest.NewServer(newMux(api.New(api.Options{}), openAuth(t)))
	defer srv.Close()

	// "/" is a page — the UI, or the placeholder saying this binary has none.
	res, err := srv.Client().Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	res.Body.Close() //nolint:errcheck // read-only handle
	if res.StatusCode != http.StatusOK {
		t.Errorf("GET / = %d, want 200", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("GET / Content-Type = %q, want text/html", ct)
	}
	// index.html must never be cached immutably: its name outlives its
	// contents, so a cached copy would point at a previous build's assets.
	if cc := res.Header.Get("Cache-Control"); strings.Contains(cc, "immutable") {
		t.Errorf("GET / Cache-Control = %q, want a revalidating page", cc)
	}

	// A client-side route the server knows nothing about still hands the
	// browser the application, so a reload of a deep link works.
	res, err = srv.Client().Get(srv.URL + "/projects/shop/environments/production")
	if err != nil {
		t.Fatalf("GET a client-side route: %v", err)
	}
	res.Body.Close() //nolint:errcheck // read-only handle
	if res.StatusCode != http.StatusOK {
		t.Errorf("GET /projects/… = %d, want the SPA fallback", res.StatusCode)
	}

	// /healthz keeps its own handler rather than falling into the catch-all.
	res, err = srv.Client().Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	res.Body.Close() //nolint:errcheck // read-only handle
	if ct := res.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("GET /healthz Content-Type = %q, want the health handler's JSON", ct)
	}

	// An unregistered path under the API's prefix is a missing endpoint, not a
	// page: a ConnectRPC client must not be handed HTML to decode.
	res, err = srv.Client().Get(srv.URL + "/kelson.v1alpha1.NoSuchService/Nope")
	if err != nil {
		t.Fatalf("GET an unregistered RPC path: %v", err)
	}
	res.Body.Close() //nolint:errcheck // read-only handle
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("GET an unregistered RPC path = %d, want 404", res.StatusCode)
	}
}

// TestWebBannerSaysWhetherTheUIIsInTheBinary: a server built without `make ui`
// serves the API perfectly and the UI not at all, so the only other way to find
// that out is to open a browser and be confused.
func TestWebBannerSaysWhetherTheUIIsInTheBinary(t *testing.T) {
	got := webBanner()
	if !strings.Contains(got, "web UI") {
		t.Fatalf("banner = %q, want it to name the UI", got)
	}
	if strings.Contains(got, "make ui") == webui.Built() {
		t.Errorf("banner = %q, want it to agree with webui.Built() = %v", got, webui.Built())
	}
}

// openAuth is the no-password gate: the pre-#84 posture, which is still what a
// server started without --password serves.
func openAuth(t *testing.T) *api.Auth {
	t.Helper()
	auth, err := api.NewAuth("")
	if err != nil {
		t.Fatalf("api.NewAuth: %v", err)
	}
	return auth
}

// TestMuxWithAPasswordGatesTheAPIAndOnlyTheAPI is the wiring assertion for the
// interim auth: the gate is mounted around the whole mux, so what it does and
// does not cover is a property of this file, not of internal/api.
func TestMuxWithAPasswordGatesTheAPIAndOnlyTheAPI(t *testing.T) {
	auth, err := api.NewAuth("hunter2")
	if err != nil {
		t.Fatalf("api.NewAuth: %v", err)
	}
	srv := httptest.NewServer(newMux(api.New(api.Options{}), auth))
	defer srv.Close()

	// /healthz stays open: a probe holds no credential and a server that fails
	// its liveness check because nobody logged in would be restarted forever.
	res, err := srv.Client().Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	res.Body.Close() //nolint:errcheck // read-only handle
	if res.StatusCode != http.StatusOK {
		t.Errorf("/healthz behind a password = %d, want 200", res.StatusCode)
	}

	// The UI stays open too, and for a reason worth stating: the login form is
	// part of the SPA, so serving it behind the login would be a loop. It is
	// the Vite dev server's trust model unchanged — the assets are public, the
	// cluster is not.
	res, err = srv.Client().Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	res.Body.Close() //nolint:errcheck // read-only handle
	if res.StatusCode != http.StatusOK {
		t.Errorf("the web UI behind a password = %d, want it served so a user can log in", res.StatusCode)
	}

	// An RPC without a credential is refused, and refused as Unauthenticated so
	// a client can tell "log in" from "the server broke".
	client := kelsonv1alpha1connect.NewRenderServiceClient(srv.Client(), srv.URL)
	_, err = client.Render(context.Background(), connect.NewRequest(&kelsonv1alpha1.RenderRequest{}))
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("Render with no credential = %v, want unauthenticated", err)
	}

	// The same RPC with the shared password as a bearer token reaches the
	// handler, which then rejects an empty request on its own terms. Any code
	// but unauthenticated is the assertion: the gate is out of the way.
	authed := kelsonv1alpha1connect.NewRenderServiceClient(srv.Client(), srv.URL,
		connect.WithInterceptors(connect.UnaryInterceptorFunc(
			func(next connect.UnaryFunc) connect.UnaryFunc {
				return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
					req.Header().Set("Authorization", "Bearer hunter2")
					return next(ctx, req)
				}
			})))
	_, err = authed.Render(context.Background(), connect.NewRequest(&kelsonv1alpha1.RenderRequest{}))
	if connect.CodeOf(err) == connect.CodeUnauthenticated {
		t.Fatalf("Render with the bearer password was still refused: %v", err)
	}
}

// TestAuthBannerSaysWhichPostureItStartedIn: an operator who typo'd the
// environment variable name must not have to discover it by finding the API
// open.
func TestAuthBannerSaysWhichPostureItStartedIn(t *testing.T) {
	if got := authBanner(openAuth(t)); !strings.Contains(got, "none") {
		t.Errorf("banner without a password = %q", got)
	}
	auth, err := api.NewAuth("hunter2")
	if err != nil {
		t.Fatalf("api.NewAuth: %v", err)
	}
	if got := authBanner(auth); !strings.Contains(got, "password") {
		t.Errorf("banner with a password = %q", got)
	}
	if strings.Contains(authBanner(auth), "hunter2") {
		t.Error("the banner printed the password")
	}
}
