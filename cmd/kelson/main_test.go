package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runKelson(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := newRootCmd()
	var outBuf, errBuf bytes.Buffer
	cmd.SetOut(&outBuf)
	cmd.SetErr(&errBuf)
	cmd.SetArgs(args)
	err = cmd.Execute()
	return outBuf.String(), errBuf.String(), err
}

func examplesHello(t *testing.T) (project, env string) {
	t.Helper()
	return filepath.Join("..", "..", "examples", "hello-single", "project.yaml"),
		filepath.Join("..", "..", "examples", "hello-single", "development.yaml")
}

// profileFile writes a ClusterProfile to a temp file and returns its path.
// The hello example declares a domain suffix, and since #140 a profile with no
// Gateway API cannot route it, so CLI render tests must say what the cluster
// provides instead of relying on the zero profile.
func profileFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "profile.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// gatewayProfileYAML is the cluster the example walk renders against: what the
// published examples actually require.
//
// CloudNativePG joined it with #89, for the same reason Gateway API is here.
// A profile with no CNPG is not "we could not tell" — the judgement in
// internal/clusterprofile/postgres reads a nil component as a checked absence
// and refuses a managed postgres service on it, which is the honest answer and
// exactly what #140 established for routing. So the fix is the same one #140
// used: say what the cluster provides rather than weakening the judgement so a
// zero profile passes. Rendering on Unknown is a different case and is
// exercised in internal/renderer (docs/data-services.md).
const gatewayProfileYAML = "gatewayAPI:\n  version: v1.6.0\n  classes: [envoy]\n" +
	"cnpg:\n  version: 1.30.0\n  namespace: cnpg-system\n  crds: [clusters, databases]\n"

func TestRenderToStdout(t *testing.T) {
	project, env := examplesHello(t)
	stdout, _, err := runKelson(t, "render", "-f", project, "-f", env,
		"--profile", profileFile(t, gatewayProfileYAML))
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}
	for _, want := range []string{
		"kind: Deployment",
		"kind: Service",
		"kelson.dev/environment: development",
		"kelson.dev/project: hello",
		"kelson.dev/renderer-version:",
		"kelson.dev/spec-hash: sha256:",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout missing %q:\n%s", want, stdout)
		}
	}
}

// TestRenderDeterministicAcrossRuns: two CLI invocations render byte-identical
// output — previews and diffs are trustworthy (ADR-0001).
func TestRenderDeterministicAcrossRuns(t *testing.T) {
	project, env := examplesHello(t)
	profile := profileFile(t, gatewayProfileYAML)
	a, _, err := runKelson(t, "render", "-f", project, "-f", env, "--profile", profile)
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}
	b, _, err := runKelson(t, "render", "-f", project, "-f", env, "--profile", profile)
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}
	if a != b {
		t.Fatalf("two CLI renders differed")
	}
}

func TestRenderWithProfile(t *testing.T) {
	project, env := examplesHello(t)
	stdout, _, err := runKelson(t, "render", "-f", project, "-f", env,
		"--profile", profileFile(t, gatewayProfileYAML))
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}
	if !strings.Contains(stdout, "kind: HTTPRoute") || !strings.Contains(stdout, "name: envoy") {
		t.Fatalf("expected an HTTPRoute attached to the detected gateway:\n%s", stdout)
	}
}

// TestRenderIngressOnlyProfileFails is #140 at the CLI boundary: a cluster
// with only an ingress class cannot serve a spec that declares domains, and
// the user must be told so with the fix, never handed an Ingress.
func TestRenderIngressOnlyProfileFails(t *testing.T) {
	project, env := examplesHello(t)
	stdout, _, err := runKelson(t, "render", "-f", project, "-f", env,
		"--profile", profileFile(t, "ingressClasses:\n  - { name: nginx, default: true }\n"))
	if err == nil {
		t.Fatalf("expected a capability-gap error, got:\n%s", stdout)
	}
	for _, want := range []string{"render/gateway-api-missing", "Envoy Gateway"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error must contain %q, got: %v", want, err)
		}
	}
	if strings.Contains(stdout, "kind: Ingress") {
		t.Fatalf("an Ingress was rendered; kelson renders Gateway API only:\n%s", stdout)
	}
}

// TestRenderFromCluster: capture is a CLI concern outside the renderer. The
// command must not silently fall back to a zero profile when it cannot reach a
// cluster — an explicit error is the only honest answer, so `--profile
// from-cluster` without usable credentials fails loudly instead of rendering
// offline as if nothing were installed.
func TestRenderFromCluster(t *testing.T) {
	project, env := examplesHello(t)
	stdout, _, err := runKelson(t, "render", "-f", project, "-f", env, "--profile", "from-cluster")
	if err == nil {
		t.Fatalf("expected from-cluster to fail without cluster credentials")
	}
	if !strings.Contains(err.Error(), "credentials") && !strings.Contains(err.Error(), "refused") {
		t.Fatalf("expected a credentials/connection error, got: %v", err)
	}
	if strings.Contains(stdout, "kind: Deployment") {
		t.Fatalf("from-cluster without a cluster must not render offline:\n%s", stdout)
	}
}

func TestRenderUnknownEnvironment(t *testing.T) {
	project, env := examplesHello(t)
	_, _, err := runKelson(t, "render", "-f", project, "-f", env, "--env", "nope")
	if err == nil || !strings.Contains(err.Error(), `environment "nope" not found`) {
		t.Fatalf("expected environment-not-found error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "development") {
		t.Fatalf("error should list available environments: %v", err)
	}
}

func TestRenderMissingEnvFlag(t *testing.T) {
	project, env := examplesHello(t)
	// Same Environment supplied twice → ambiguity requires --env.
	_, _, err := runKelson(t, "render", "-f", project, "-f", env, "-f", env)
	if err == nil || !strings.Contains(err.Error(), "select one with --env") {
		t.Fatalf("expected ambiguous-environment error, got: %v", err)
	}
}

func TestRenderToDirectory(t *testing.T) {
	project, env := examplesHello(t)
	dir := filepath.Join(t.TempDir(), "out")
	stdout, _, err := runKelson(t, "render", "-f", project, "-f", env, "-o", dir,
		"--profile", profileFile(t, gatewayProfileYAML))
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading output dir: %v", err)
	}
	// hello/development renders Namespace + ServiceAccount + Service +
	// Deployment + HTTPRoute against a Gateway API profile.
	if len(entries) != 5 {
		t.Fatalf("expected 5 manifest files, got %d: %v", len(entries), entries)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".yaml") {
			t.Fatalf("unexpected file in output dir: %s", e.Name())
		}
		if !strings.Contains(stdout, e.Name()) {
			t.Fatalf("stdout should name written files; missing %s:\n%s", e.Name(), stdout)
		}
	}
}

func TestVersion(t *testing.T) {
	stdout, _, err := runKelson(t, "--version")
	if err != nil {
		t.Fatalf("--version failed: %v", err)
	}
	if !strings.Contains(stdout, "kelson version") {
		t.Fatalf("unexpected --version output: %q", stdout)
	}
}
