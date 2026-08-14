package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/observation"
)

// The delivery commands are assembly: they render, read the cluster and report.
// The planes themselves are built and tested where the Kubernetes fake clients
// live — the command plane's lint allow-list keeps those imports out of cmd —
// so these tests drive the commands through the observationConnector seam and
// assert the wiring.
//
// Most of what used to be here tested `kelson deploy` and `kelson rollback`
// driving an adapter through the state machine, and it went with the adapters
// (ADR-0028). What replaces it is smaller and load-bearing in a different way:
// the deleted verbs must refuse in a shape a caller can act on, and the verbs
// that survived must still work.

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

// runRoot executes an unmodified root command, for the verbs that take no seam
// because they refuse before reaching one.
func runRoot(t *testing.T, args ...string) (stdout string, code int, msg string) {
	t.Helper()
	cmd := newRootCmd()
	var outBuf, errBuf bytes.Buffer
	cmd.SetOut(&outBuf)
	cmd.SetErr(&errBuf)
	cmd.SetIn(strings.NewReader(""))
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

// --- the gated verbs ---------------------------------------------------------

// The three verbs ADR-0028 deleted the machinery for must refuse in the shape
// the repo's taxonomy defines: a structured delivery.Error carrying
// `delivery/not-implemented` and, in its remediation, the issue that tracks the
// capability's return. A bare "not supported" would leave an agent to guess
// whether retrying could ever help.
func TestDeletedVerbsRefuseWithTheTrackedCode(t *testing.T) {
	spec := deploySpec(t)
	cases := []struct {
		name string
		args []string
	}{
		{"deploy", []string{"deploy", "-f", spec, "--env", "development"}},
		{"rollback", []string{"rollback", "-f", spec, "--env", "development", "--yes"}},
		// `promote` is refused for the same reason and asserted in
		// promote_test.go, where the fixture has two environments to promote
		// between.
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, code, msg := runRoot(t, tc.args...)
			if code != exitErr {
				t.Fatalf("exit = %d, want %d", code, exitErr)
			}
			if !strings.Contains(msg, string(delivery.ErrNotImplemented)) {
				t.Errorf("the message must carry the %s code so an agent can branch on it, got: %s",
					delivery.ErrNotImplemented, msg)
			}
			if !strings.Contains(msg, "#224") {
				t.Errorf("the message must name the tracking issue, got: %s", msg)
			}
		})
	}
}

// The refusals are still commands, not unknown ones. `kelson deploy --help`
// must explain what happened and what replaces it, because a reader who typed
// a verb kelson documented deserves better than a usage error.
func TestDeletedVerbsStayInTheCommandTree(t *testing.T) {
	root := newRootCmd()
	for _, name := range []string{"deploy", "rollback", "promote"} {
		cmd, _, err := root.Find([]string{name})
		if err != nil || cmd.Name() != name {
			t.Fatalf("`kelson %s` is not in the command tree: %v", name, err)
		}
		if !strings.Contains(cmd.Long, "224") {
			t.Errorf("`kelson %s --help` should name the tracking issue:\n%s", name, cmd.Long)
		}
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
