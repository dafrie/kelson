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

func TestRenderToStdout(t *testing.T) {
	project, env := examplesHello(t)
	stdout, _, err := runKelson(t, "render", "-f", project, "-f", env)
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
	a, _, err := runKelson(t, "render", "-f", project, "-f", env)
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}
	b, _, err := runKelson(t, "render", "-f", project, "-f", env)
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}
	if a != b {
		t.Fatalf("two CLI renders differed")
	}
}

func TestRenderWithProfile(t *testing.T) {
	dir := t.TempDir()
	profilePath := filepath.Join(dir, "profile.yaml")
	if err := os.WriteFile(profilePath, []byte("ingressClasses:\n  - { name: nginx }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	project, env := examplesHello(t)
	stdout, _, err := runKelson(t, "render", "-f", project, "-f", env, "--profile", profilePath)
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}
	if !strings.Contains(stdout, "kind: Ingress") || !strings.Contains(stdout, "ingressClassName: nginx") {
		t.Fatalf("expected Ingress for the nginx profile:\n%s", stdout)
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
	stdout, _, err := runKelson(t, "render", "-f", project, "-f", env, "-o", dir)
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading output dir: %v", err)
	}
	// hello/development renders ServiceAccount + Service + Deployment (no
	// route: the default profile detects no routing substrate).
	if len(entries) != 3 {
		t.Fatalf("expected 3 manifest files, got %d: %v", len(entries), entries)
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
