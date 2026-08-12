package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/dafrie/kelson/internal/clusterprofile"
	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/diff"
)

// rootWithEngine returns a root command whose `diff` subcommand uses the given
// L2 engine constructor. The real engine (a dryrun.DryRun over a dynamic
// client) is built and exercised in the delivery plane (dryrun_test.go), where
// the Kubernetes fake clients live; the command plane's lint allow-list keeps
// those imports out of cmd, so the CI-gate tests drive the command through
// this seam and assert the mapping from a preview verdict to exit code and
// rendering (issue #46).
func rootWithEngine(factory func(string, clusterprofile.ClusterProfile) (diffRunner, error)) *cobra.Command {
	root := newRootCmd()
	for _, c := range root.Commands() {
		if c.Name() == "diff" {
			root.RemoveCommand(c)
			break
		}
	}
	root.AddCommand(newDiffCmdFactory(factory))
	return root
}

func runEngineKelson(t *testing.T, factory func(string, clusterprofile.ClusterProfile) (diffRunner, error), args ...string) (stdout, stderr string, code int, msg string) {
	t.Helper()
	cmd := rootWithEngine(factory)
	var outBuf, errBuf bytes.Buffer
	cmd.SetOut(&outBuf)
	cmd.SetErr(&errBuf)
	cmd.SetArgs(args)
	err := cmd.Execute()
	msg, code = resolveExit(err)
	return outBuf.String(), errBuf.String(), code, msg
}

// --- spec fixtures ---------------------------------------------------------

// writeSpec writes a single multi-document spec file (Project + Environment)
// rendering one Deployment whose imageVersion feeds the image tag.
func writeSpec(t *testing.T, dir, name, imageVersion string) string {
	t.Helper()
	project := "apiVersion: kelson.dev/v1alpha1\nkind: Project\nmetadata:\n  name: hello\n\nspec:\n  image: ghcr.io/acme/hello:" + imageVersion + "\n\n  env:\n    LOG_LEVEL: info\n\n  applications:\n    - name: web\n      port: 8080\n      health: /healthz\n"
	environment := "apiVersion: kelson.dev/v1alpha1\nkind: Environment\nmetadata:\n  name: development\n\nspec:\n  project: hello\n"
	path := filepath.Join(dir, name)
	data := project + "---\n" + environment
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// --- L1 render mode --------------------------------------------------------

// TestDiffRenderWorksWithoutCluster is the acceptance "kelson diff works with
// no cluster and no server": the default --dry-run=render contacts nothing and
// reports every resource as an addition (no --from, no history).
func TestDiffRenderWorksWithoutCluster(t *testing.T) {
	spec := writeSpec(t, t.TempDir(), "spec.yaml", "1.4.2")
	stdout, _, code, _ := runEngineKelson(t, func(string, clusterprofile.ClusterProfile) (diffRunner, error) {
		t.Fatal("render mode must never construct a server engine")
		return nil, nil
	}, "diff", "-f", spec, "--dry-run=render")
	if code != exitDiff {
		t.Fatalf("exit code = %d, want %d (changes present)", code, exitDiff)
	}
	for _, want := range []string{"rendered diff hello/development", "+ Deployment/web additive", "+ Service/web additive"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout missing %q:\n%s", want, stdout)
		}
	}
}

// TestDiffEmptyExitsZero is the acceptance "empty diff exits 0": an unchanged
// previous spec renders the same bytes, so nothing is reported and the exit
// code is 0.
func TestDiffEmptyExitsZero(t *testing.T) {
	dir := t.TempDir()
	spec := writeSpec(t, dir, "spec.yaml", "1.4.2")
	stdout, _, code, _ := runEngineKelson(t, nil, "diff", "-f", spec, "--from", spec)
	if code != exitOK {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout, "summary: 0 added, 0 modified, 0 removed") {
		t.Fatalf("expected an empty summary:\n%s", stdout)
	}
}

// TestDiffSpecChangeExitsTwo is the acceptance "a spec change exits 2": an
// image bump produces a restart-required Deployment diff and exit 2.
func TestDiffSpecChangeExitsTwo(t *testing.T) {
	dir := t.TempDir()
	prev := writeSpec(t, dir, "prev.yaml", "1.4.2")
	cur := writeSpec(t, dir, "cur.yaml", "1.5.0")
	stdout, _, code, _ := runEngineKelson(t, nil, "diff", "-f", cur, "--from", prev)
	if code != exitDiff {
		t.Fatalf("exit code = %d, want %d (changes present)", code, exitDiff)
	}
	for _, want := range []string{
		"~ Deployment/web restart-required",
		`spec.template.spec.containers[0].image`,
		`"ghcr.io/acme/hello:1.4.2"`,
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout missing %q:\n%s", want, stdout)
		}
	}
}

// TestDiffJSONUnmarshalsIntoDiff is the acceptance "--output json emits valid
// JSON that unmarshals into diff.Diff".
func TestDiffJSONUnmarshalsIntoDiff(t *testing.T) {
	dir := t.TempDir()
	prev := writeSpec(t, dir, "prev.yaml", "1.4.2")
	cur := writeSpec(t, dir, "cur.yaml", "1.5.0")
	stdout, _, code, _ := runEngineKelson(t, nil, "diff", "-f", cur, "--from", prev, "--output", "json")
	if code != exitDiff {
		t.Fatalf("exit code = %d, want %d", code, exitDiff)
	}
	var d diff.Diff
	if err := json.Unmarshal([]byte(stdout), &d); err != nil {
		t.Fatalf("output is not valid JSON for diff.Diff: %v\n%s", err, stdout)
	}
	if d.Level != diff.LevelRendered || d.Project != "hello" || d.Environment != "development" {
		t.Fatalf("decoded diff = %+v", d)
	}
	if len(d.Resources) != 1 || d.Resources[0].Op != diff.OpModified {
		t.Fatalf("expected one modified resource, got %+v", d.Resources)
	}
	if d.Summary.Modified != 1 {
		t.Fatalf("summary.Modified = %d, want 1", d.Summary.Modified)
	}
}

// --- L2 server mode ----------------------------------------------------------

// previewEngine returns a diffRunner stub standing in for a dryrun.DryRun
// verdict, so the command's CI-gate behaviour is tested against a fixed server
// opinion without a cluster. verdict is returned as-is; err, when non-nil,
// is returned instead (simulating an unreachable cluster).
func previewEngine(verdict *diff.Diff, err error) func(string, clusterprofile.ClusterProfile) (diffRunner, error) {
	return func(string, clusterprofile.ClusterProfile) (diffRunner, error) {
		return diffRunnerFunc(func(context.Context, delivery.ManifestSet) (*diff.Diff, error) { return verdict, err }), nil
	}
}

type diffRunnerFunc func(context.Context, delivery.ManifestSet) (*diff.Diff, error)

func (f diffRunnerFunc) Preview(ctx context.Context, set delivery.ManifestSet) (*diff.Diff, error) {
	return f(ctx, set)
}

// TestDiffServerPolicyBlockerExitsThree is the acceptance "kelson diff
// --dry-run=server fails a pull request that would be rejected by cluster
// policy": an enforce-mode PolicyViolation from the server preview yields exit
// code 3 (blocker) and the policy name in the terminal output.
func TestDiffServerPolicyBlockerExitsThree(t *testing.T) {
	verdict := &diff.Diff{
		Level:       diff.LevelServer,
		Project:     "hello",
		Environment: "development",
		Resources: []diff.ResourceDiff{{
			APIVersion: "apps/v1", Kind: "Deployment", Name: "web",
			Namespace: "hello-development", Op: diff.OpModified, Risk: diff.RiskDisruptive,
		}},
		Violations: []diff.PolicyViolation{{
			Engine: "kyverno", Policy: "require-safe-image", Rule: "require-nonlatest-tag",
			Resource: "Deployment/hello-development/web", Message: "image uses the latest tag",
			Enforcement: diff.EnforcementEnforce,
		}},
		Summary: diff.Summary{Modified: 1, Disruptive: []string{"web"}, MaxRisk: diff.RiskDisruptive},
	}
	spec := writeSpec(t, t.TempDir(), "spec.yaml", "1.4.2")
	stdout, _, code, _ := runEngineKelson(t, previewEngine(verdict, nil), "diff", "-f", spec, "--dry-run=server")
	if code != exitBlk {
		t.Fatalf("exit code = %d, want %d (blocker)", code, exitBlk)
	}
	if !strings.Contains(stdout, "require-safe-image") {
		t.Fatalf("stdout missing the policy name:\n%s", stdout)
	}
	if !strings.Contains(stdout, "BLOCKED") {
		t.Fatalf("stdout should mark the finding as a blocker:\n%s", stdout)
	}
}

// TestDiffServerUnreachableFailsLoudly is issue #46's separation contract for
// L2: when the cluster cannot be reached the command fails with a clear error
// (exit 1) — it never silently falls back to an L1 render.
func TestDiffServerUnreachableFailsLoudly(t *testing.T) {
	spec := writeSpec(t, t.TempDir(), "spec.yaml", "1.4.2")
	stdout, _, code, msg := runEngineKelson(t, previewEngine(nil, errors.New("dial tcp 127.0.0.1:6443: connection refused")),
		"diff", "-f", spec, "--dry-run=server")
	if code != exitErr {
		t.Fatalf("exit code = %d, want %d", code, exitErr)
	}
	if !strings.Contains(msg, "connection refused") {
		t.Fatalf("expected the unreachable-cluster error, got %q", msg)
	}
	if strings.Contains(stdout, "degraded") {
		t.Fatalf("an unreachable cluster must not degrade to L1:\n%s", stdout)
	}
}

// TestDiffUnknownDryRunValue ensures a mistyped --dry-run is a usage error
// (exit 1), never interpreted as a render.
func TestDiffUnknownDryRunValue(t *testing.T) {
	spec := writeSpec(t, t.TempDir(), "spec.yaml", "1.4.2")
	_, _, code, msg := runEngineKelson(t, nil, "diff", "-f", spec, "--dry-run=bogus")
	if code != exitErr {
		t.Fatalf("exit code = %d, want %d", code, exitErr)
	}
	if !strings.Contains(msg, `unknown --dry-run "bogus"`) {
		t.Fatalf("expected a clear usage error, got: %q", msg)
	}
}

// TestResolveExitCodeContract pins the exit-code mapping main() relies on,
// so the CI contract (issue #46) is explicit and regression-guarded.
func TestResolveExitCodeContract(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code int
	}{
		{"nil", nil, 0},
		{"plain error", errors.New("boom"), 1},
		{"changes", &exitError{code: exitDiff}, 2},
		{"blocker", &exitError{code: exitBlk}, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, code := resolveExit(tc.err); code != tc.code {
				t.Fatalf("resolveExit code = %d, want %d", code, tc.code)
			}
		})
	}
}
