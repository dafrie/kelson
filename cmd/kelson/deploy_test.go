package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/observation"
)

// The delivery commands are assembly: deploy/rollback/promote/history render
// nothing themselves and read no cluster — they are ConnectRPC clients of
// kelson-server (R2, issue #225) — while status and explain still read the
// cluster directly (cmd/kelson/status.go's package doc says why). So the two
// families are tested differently: this file's fakeDeployService drives the
// façade-backed verbs over a real HTTP server (facade_test.go), and the
// observationConnector seam below is what status and explain are still tested
// through.

// --- fakes ------------------------------------------------------------------

// fakeProbe answers with one verdict per workload name, defaulting to healthy.
type fakeProbe struct {
	verdicts map[string]observation.Verdict
	err      error
}

func (f fakeProbe) Evaluate(_ context.Context, namespace, name string) (observation.Verdict, error) {
	if f.err != nil {
		return observation.Verdict{}, f.err
	}
	if v, ok := f.verdicts[name]; ok {
		return v, nil
	}
	return observation.Verdict{Healthy: true, Code: observation.CodeHealthy, Resource: "Deployment/" + namespace + "/" + name}, nil
}

// planeOf builds a connector serving the given probe.
func planeOf(health observation.Evaluator) observationConnector {
	return func(observationTarget) (*observationPlane, error) {
		return &observationPlane{health: health}, nil
	}
}

// capturingConnector records the target it was handed, so a test can assert
// what the spec resolved to.
func capturingConnector(inner observationConnector, seen *observationTarget) observationConnector {
	return func(t observationTarget) (*observationPlane, error) {
		*seen = t
		return inner(t)
	}
}

// --- harness ----------------------------------------------------------------

func rootWithPlane(connect observationConnector) *cobra.Command {
	root := newRootCmd()
	replaced := map[string]bool{"status": true, "explain": true}
	for _, c := range root.Commands() {
		if replaced[c.Name()] {
			root.RemoveCommand(c)
		}
	}
	root.AddCommand(newStatusCmdFactory(connect))
	root.AddCommand(newExplainCmdFactory(connect))
	return root
}

// runDelivery executes a delivery command with stdin closed, which is what an
// unattended run looks like.
func runDelivery(t *testing.T, connect observationConnector, args ...string) (stdout string, code int, msg string) {
	t.Helper()
	return runDeliveryStdin(t, connect, "", args...)
}

func runDeliveryStdin(t *testing.T, connect observationConnector, stdin string, args ...string) (stdout string, code int, msg string) {
	t.Helper()
	cmd := rootWithPlane(connect)
	var outBuf, errBuf bytes.Buffer
	cmd.SetOut(&outBuf)
	cmd.SetErr(&errBuf)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(args)
	err := cmd.Execute()
	msg, code = resolveExit(err)
	return outBuf.String(), code, msg
}

// runRoot executes an unmodified root command with stdin closed.
func runRoot(t *testing.T, args ...string) (stdout string, code int, msg string) {
	t.Helper()
	return runRootStdin(t, "", args...)
}

func runRootStdin(t *testing.T, stdin string, args ...string) (stdout string, code int, msg string) {
	t.Helper()
	cmd := newRootCmd()
	var outBuf, errBuf bytes.Buffer
	cmd.SetOut(&outBuf)
	cmd.SetErr(&errBuf)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(args)
	err := cmd.Execute()
	msg, code = resolveExit(err)
	return outBuf.String(), code, msg
}

// writeDeliverySpec writes a Project + Environment pair.
func writeDeliverySpec(t *testing.T, dir, name string) string {
	t.Helper()
	project := "apiVersion: kelson.dev/v1alpha1\nkind: Project\nmetadata:\n  name: hello\n\nspec:\n  image: ghcr.io/acme/hello:1.0.0\n\n  components:\n    - name: web\n      port: 8080\n"
	environment := "apiVersion: kelson.dev/v1alpha1\nkind: Environment\nmetadata:\n  name: development\n\nspec:\n  project: hello\n"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(project+"---\n"+environment), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// deploySpec is the common fixture.
func deploySpec(t *testing.T) string {
	t.Helper()
	return writeDeliverySpec(t, t.TempDir(), "spec.yaml")
}

// --- kelson deploy ------------------------------------------------------------

// deployPreviewOK is the Deploy stub every happy-path test starts from: the
// preview call (dry_run=RENDER) reports one resource and nothing else.
func deployPreviewOK(t *testing.T, real func(*kelsonv1alpha1.DeployRequest, *connect.ServerStream[kelsonv1alpha1.DeployResponse]) error) func(context.Context, *kelsonv1alpha1.DeployRequest, *connect.ServerStream[kelsonv1alpha1.DeployResponse]) error {
	t.Helper()
	return func(_ context.Context, req *kelsonv1alpha1.DeployRequest, stream *connect.ServerStream[kelsonv1alpha1.DeployResponse]) error {
		if req.GetDryRun() == kelsonv1alpha1.DryRun_DRY_RUN_RENDER {
			return stream.Send(&kelsonv1alpha1.DeployResponse{Event: &kelsonv1alpha1.DeployResponse_Proposed_{
				Proposed: &kelsonv1alpha1.DeployResponse_Proposed{Project: "hello", Environment: "development", Resources: 3},
			}})
		}
		return real(req, stream)
	}
}

func TestDeployAppliesAndReportsSettled(t *testing.T) {
	spec := deploySpec(t)
	fake := &fakeDeployService{}
	fake.deploy = deployPreviewOK(t, func(_ *kelsonv1alpha1.DeployRequest, stream *connect.ServerStream[kelsonv1alpha1.DeployResponse]) error {
		if err := stream.Send(&kelsonv1alpha1.DeployResponse{Event: &kelsonv1alpha1.DeployResponse_Committed_{
			Committed: &kelsonv1alpha1.DeployResponse_Committed{Revision: "1-a1b2c3d4", Adapter: "flux"},
		}}); err != nil {
			return err
		}
		final := &kelsonv1alpha1.DeployResponse_Transition{Phase: "Healthy", Answer: "live"}
		return stream.Send(&kelsonv1alpha1.DeployResponse{Event: &kelsonv1alpha1.DeployResponse_Settled_{
			Settled: &kelsonv1alpha1.DeployResponse_Settled{Final: final},
		}})
	})
	addr := serveFakeDeployService(t, fake)

	stdout, code, msg := runRootStdin(t, "", "deploy", "-f", spec, "--env", "development", "--yes", "--server", addr)
	if code != exitOK {
		t.Fatalf("exit = %d (%s), want 0\n%s", code, msg, stdout)
	}
	for _, want := range []string{"Proposed", "3 resource", "Committed", "1-a1b2c3d4", "Settled"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout does not contain %q:\n%s", want, stdout)
		}
	}
}

func TestDeploySettledErrorFailsTheCommand(t *testing.T) {
	spec := deploySpec(t)
	fake := &fakeDeployService{}
	fake.deploy = deployPreviewOK(t, func(_ *kelsonv1alpha1.DeployRequest, stream *connect.ServerStream[kelsonv1alpha1.DeployResponse]) error {
		final := &kelsonv1alpha1.DeployResponse_Transition{Phase: "Rejected", Answer: "rejected"}
		return stream.Send(&kelsonv1alpha1.DeployResponse{Event: &kelsonv1alpha1.DeployResponse_Settled_{
			Settled: &kelsonv1alpha1.DeployResponse_Settled{
				Final: final,
				Error: &kelsonv1alpha1.Error{Code: "delivery/rejected", Message: "the admission webhook denied it"},
			},
		}})
	})
	addr := serveFakeDeployService(t, fake)

	_, code, msg := runRootStdin(t, "", "deploy", "-f", spec, "--env", "development", "--yes", "--server", addr)
	if code != exitErr {
		t.Fatalf("exit = %d, want %d", code, exitErr)
	}
	if !strings.Contains(msg, "delivery/rejected") {
		t.Errorf("message does not carry the settled error's code: %s", msg)
	}
}

func TestDeployUnreachableServerNamesTheFlag(t *testing.T) {
	spec := deploySpec(t)
	addr := unreachableServerAddr(t)

	start := time.Now()
	_, code, msg := runRootStdin(t, "", "deploy", "-f", spec, "--env", "development", "--yes", "--server", addr)
	elapsed := time.Since(start)

	if code != exitErr {
		t.Fatalf("exit = %d, want %d (%s)", code, exitErr, msg)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("an unreachable --server took %s to fail; it must fail fast, not hang", elapsed)
	}
	for _, want := range []string{"--server", "KELSON_SERVER"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal must name %q so a caller knows what to set: %s", want, msg)
		}
	}
}

func TestDeployRequiresFileOrProject(t *testing.T) {
	_, code, msg := runRoot(t, "deploy", "--env", "development", "--yes")
	if code != exitErr {
		t.Fatalf("exit = %d, want %d", code, exitErr)
	}
	if !strings.Contains(msg, "-f") || !strings.Contains(msg, "--project") {
		t.Errorf("the error should name both ways to address a spec: %s", msg)
	}
}

func TestDeployFileAndProjectAreMutuallyExclusive(t *testing.T) {
	spec := deploySpec(t)
	_, code, msg := runRoot(t, "deploy", "-f", spec, "--project", "hello", "--env", "development", "--yes")
	if code != exitErr {
		t.Fatalf("exit = %d, want %d", code, exitErr)
	}
	if !strings.Contains(msg, "mutually exclusive") {
		t.Errorf("the error should say the two flags conflict: %s", msg)
	}
}

func TestDeployProjectRequiresEnv(t *testing.T) {
	_, code, msg := runRoot(t, "deploy", "--project", "hello", "--yes")
	if code != exitErr {
		t.Fatalf("exit = %d, want %d", code, exitErr)
	}
	if !strings.Contains(msg, "--env") {
		t.Errorf("the error should name --env: %s", msg)
	}
}

// --- terminal helpers --------------------------------------------------------

// TestInteractiveRefusesDevNull pins the gate the E2E suite caught open: a
// child process whose parent wired up no stdin inherits /dev/null, which is a
// character device — a file-mode sniff calls it a terminal, and the
// confirmation gates then wait on a reader that answers only EOF. The probe
// must say "not a terminal" for exactly this input.
func TestInteractiveRefusesDevNull(t *testing.T) {
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("opening %s: %v", os.DevNull, err)
	}
	defer devnull.Close() //nolint:errcheck // read-only file, nothing to lose

	cmd := &cobra.Command{}
	cmd.SetIn(devnull)
	if interactive(cmd) {
		t.Fatalf("interactive() = true for %s; an unattended run would be prompted with nobody to answer", os.DevNull)
	}
}

// TestConfirmDistinguishesEOFFromDecline: a stdin that closes before the
// question is answered is the unattended case, and must surface as an error a
// caller turns into a non-zero exit. A bare Enter is a human declining the
// default — a quiet no, not an error.
func TestConfirmDistinguishesEOFFromDecline(t *testing.T) {
	ask := func(stdin string) (bool, error) {
		var buf bytes.Buffer
		cmd := &cobra.Command{}
		cmd.SetOut(&buf)
		cmd.SetIn(strings.NewReader(stdin))
		return confirm(cmd, "proceed?")
	}

	if _, err := ask(""); err == nil {
		t.Errorf("confirm on a closed stdin returned no error; EOF is not an answer")
	}
	ok, err := ask("\n")
	if err != nil {
		t.Errorf("confirm on a bare Enter errored: %v; that is a human declining the default", err)
	}
	if ok {
		t.Errorf("confirm on a bare Enter said yes")
	}
	ok, err = ask("y\n")
	if err != nil || !ok {
		t.Errorf("confirm on an explicit yes = (%v, %v), want (true, nil)", ok, err)
	}
}
