package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"

	"github.com/dafrie/kelson/internal/clusterprofile"
	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/delivery/rollback"
	"github.com/dafrie/kelson/internal/observation"
)

// The delivery commands are assembly (issue #135): they render, select an
// adapter, apply and report. The adapters themselves are built and tested in
// the delivery plane, where the Kubernetes fake clients live — the command
// plane's lint allow-list keeps those imports out of cmd. So these tests drive
// the commands through the deliveryConnector seam and assert the wiring:
// which adapter is selected, what reaches it, which phases print, and what the
// exit code is.

// --- fakes ------------------------------------------------------------------

// fakeAdapter is a delivery.Adapter whose answers are scripted. Status is
// polled from the state machine's watch goroutine while the test's goroutine
// reads the call log, so every field is guarded.
type fakeAdapter struct {
	name string
	caps delivery.Capabilities

	mu       sync.Mutex
	calls    []string
	sets     []delivery.ManifestSet
	rolledTo []delivery.Entry

	applyResult delivery.Result
	applyErr    error
	// statuses are returned in order; the last one repeats forever.
	statuses    []delivery.Status
	statusErr   error
	history     []delivery.Entry
	rollbackRes delivery.Result
	rollbackErr error
}

func (f *fakeAdapter) Name() string { return f.name }

func (f *fakeAdapter) Capabilities() delivery.Capabilities { return f.caps }

func (f *fakeAdapter) record(call string, set delivery.ManifestSet) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
	f.sets = append(f.sets, set)
}

func (f *fakeAdapter) callLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeAdapter) lastSet() delivery.ManifestSet {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sets) == 0 {
		return delivery.ManifestSet{}
	}
	return f.sets[len(f.sets)-1]
}

func (f *fakeAdapter) Apply(_ context.Context, set delivery.ManifestSet) (delivery.Result, error) {
	f.record("apply", set)
	if f.applyErr != nil {
		return delivery.Result{}, f.applyErr
	}
	res := f.applyResult
	if res.Revision == "" {
		res = delivery.Result{Revision: "000001", Applied: true}
	}
	return res, nil
}

func (f *fakeAdapter) Status(_ context.Context, set delivery.ManifestSet) (delivery.Status, error) {
	f.mu.Lock()
	f.calls = append(f.calls, "status")
	f.sets = append(f.sets, set)
	if f.statusErr != nil {
		err := f.statusErr
		f.mu.Unlock()
		return delivery.Status{}, err
	}
	var st delivery.Status
	switch {
	case len(f.statuses) == 0:
		st = delivery.Status{Phase: delivery.PhaseHealthy}
	case len(f.statuses) == 1:
		st = f.statuses[0]
	default:
		st = f.statuses[0]
		f.statuses = f.statuses[1:]
	}
	f.mu.Unlock()
	return st, nil
}

func (f *fakeAdapter) History(_ context.Context, set delivery.ManifestSet) ([]delivery.Entry, error) {
	f.record("history", set)
	return f.history, nil
}

func (f *fakeAdapter) Rollback(_ context.Context, set delivery.ManifestSet, to delivery.Entry) (delivery.Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, "rollback")
	f.sets = append(f.sets, set)
	f.rolledTo = append(f.rolledTo, to)
	f.mu.Unlock()
	if f.rollbackErr != nil {
		return delivery.Result{}, f.rollbackErr
	}
	res := f.rollbackRes
	if res.Revision == "" {
		res = delivery.Result{Revision: "000009", Applied: true}
	}
	return res, nil
}

func newFakeAdapter(name string) *fakeAdapter {
	return &fakeAdapter{name: name, caps: delivery.Capabilities{SupportsRollback: true}}
}

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

// fakeRecorded is a rollback.Source over in-memory manifest bytes.
type fakeRecorded struct {
	current  []delivery.Manifest
	byRev    map[string][]delivery.Manifest
	revError error
}

func (f fakeRecorded) Current(context.Context) ([]delivery.Manifest, error) { return f.current, nil }

func (f fakeRecorded) Revision(_ context.Context, revision string) ([]delivery.Manifest, error) {
	if f.revError != nil {
		return nil, f.revError
	}
	return f.byRev[revision], nil
}

// planeOf builds a connector serving the given adapters, probe and history.
func planeOf(adapters []delivery.Adapter, health observation.Evaluator, recorded rollback.Source) deliveryConnector {
	return func(deliveryTarget) (*deliveryPlane, error) {
		reg := delivery.NewRegistry()
		for _, a := range adapters {
			if err := reg.Register(a); err != nil {
				return nil, err
			}
		}
		return &deliveryPlane{registry: reg, health: health, recorded: recorded}, nil
	}
}

// capturingConnector records the target it was handed, so tests can assert the
// spec-derived delivery mode reached adapter selection.
func capturingConnector(inner deliveryConnector, seen *deliveryTarget) deliveryConnector {
	return func(t deliveryTarget) (*deliveryPlane, error) {
		*seen = t
		return inner(t)
	}
}

// --- harness ----------------------------------------------------------------

func rootWithPlane(connect deliveryConnector) *cobra.Command {
	root := newRootCmd()
	replaced := map[string]bool{"deploy": true, "status": true, "rollback": true, "promote": true, "explain": true}
	for _, c := range root.Commands() {
		if replaced[c.Name()] {
			root.RemoveCommand(c)
		}
	}
	root.AddCommand(newDeployCmdFactory(connect))
	root.AddCommand(newStatusCmdFactory(connect))
	root.AddCommand(newRollbackCmdFactory(connect))
	root.AddCommand(newPromoteCmdFactory(connect))
	root.AddCommand(newExplainCmdFactory(connect))
	return root
}

// runDelivery executes a delivery command with stdin closed, which is what an
// unattended run looks like: no prompt for deploy, and an unconfirmed rollback.
func runDelivery(t *testing.T, connect deliveryConnector, args ...string) (stdout string, code int, msg string) {
	t.Helper()
	return runDeliveryStdin(t, connect, "", args...)
}

func runDeliveryStdin(t *testing.T, connect deliveryConnector, stdin string, args ...string) (stdout string, code int, msg string) {
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

// writeDeliverySpec writes a Project + Environment pair. mode/repo are the
// Environment's delivery stanza; an empty mode omits it, which resolves to
// direct through the P4 default chain.
func writeDeliverySpec(t *testing.T, dir, name, mode, repo string) string {
	t.Helper()
	project := "apiVersion: kelson.dev/v1alpha1\nkind: Project\nmetadata:\n  name: hello\n\nspec:\n  image: ghcr.io/acme/hello:1.0.0\n\n  components:\n    - name: web\n      port: 8080\n"
	environment := "apiVersion: kelson.dev/v1alpha1\nkind: Environment\nmetadata:\n  name: development\n\nspec:\n  project: hello\n"
	if mode != "" {
		environment += "  delivery:\n    mode: " + mode + "\n"
		if repo != "" {
			environment += "    git:\n      repo: " + repo + "\n      path: hello/development\n"
		}
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(project+"---\n"+environment), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// deploySpec is the common fixture: a direct-mode spec plus an isolated
// history directory so no test touches the user's data dir.
func deploySpec(t *testing.T) (spec, history string) {
	t.Helper()
	dir := t.TempDir()
	return writeDeliverySpec(t, dir, "spec.yaml", "", ""), filepath.Join(dir, "history")
}

// --- deploy -----------------------------------------------------------------

// TestDeployHealthyExitsZero is the harness contract: phases print to stdout as
// they happen and a deployment that reaches a healthy terminal phase exits 0.
func TestDeployHealthyExitsZero(t *testing.T) {
	spec, history := deploySpec(t)
	adapter := newFakeAdapter("direct")
	stdout, code, msg := runDelivery(t, planeOf([]delivery.Adapter{adapter}, nil, nil),
		"deploy", "-f", spec, "--env", "development", "--history", history, "--timeout", "10s")
	if code != exitOK {
		t.Fatalf("exit = %d (%s), want 0\n%s", code, msg, stdout)
	}
	for _, want := range []string{"Proposed", "Committed", "Healthy"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout missing phase %q:\n%s", want, stdout)
		}
	}
	if got := adapter.callLog(); len(got) == 0 || got[0] != "apply" {
		t.Fatalf("expected apply first, got %v", got)
	}
}

// TestDeployAppliesTheRenderedSet: the adapter receives the rendered manifests
// with the provenance a status readback needs — project, environment, a
// set-level spec hash, and the revision the apply returned.
func TestDeployAppliesTheRenderedSet(t *testing.T) {
	spec, history := deploySpec(t)
	adapter := newFakeAdapter("direct")
	_, code, msg := runDelivery(t, planeOf([]delivery.Adapter{adapter}, nil, nil),
		"deploy", "-f", spec, "--env", "development", "--history", history, "--timeout", "10s")
	if code != exitOK {
		t.Fatalf("exit = %d (%s), want 0", code, msg)
	}
	set := adapter.lastSet()
	switch {
	case set.Project != "hello":
		t.Fatalf("project = %q, want hello", set.Project)
	case set.Environment != "development":
		t.Fatalf("environment = %q, want development", set.Environment)
	case !strings.HasPrefix(set.SpecHash, "sha256:"):
		t.Fatalf("spec hash = %q, want a sha256 digest", set.SpecHash)
	case set.Revision != "000001":
		t.Fatalf("status was polled for revision %q, want the applied revision", set.Revision)
	case len(set.Manifests) == 0:
		t.Fatal("no manifests reached the adapter")
	}
}

// TestDeployRejectedExitsNonZero: a reconciler that refuses the revision is a
// failure with a cause, never a silent success.
func TestDeployRejectedExitsNonZero(t *testing.T) {
	spec, history := deploySpec(t)
	adapter := newFakeAdapter("direct")
	adapter.statuses = []delivery.Status{{Phase: delivery.PhaseRejected, Cause: "admission webhook denied the request"}}
	stdout, code, msg := runDelivery(t, planeOf([]delivery.Adapter{adapter}, nil, nil),
		"deploy", "-f", spec, "--env", "development", "--history", history, "--timeout", "10s")
	if code == exitOK {
		t.Fatalf("expected a non-zero exit for a rejected revision:\n%s", stdout)
	}
	if !strings.Contains(stdout, "Rejected") {
		t.Fatalf("stdout should report the Rejected phase:\n%s", stdout)
	}
	if !strings.Contains(msg, "rejected") {
		t.Fatalf("error should say the revision was rejected, got: %s", msg)
	}
}

// TestDeployDegradedExitsNonZero: applied-but-unhealthy must not exit 0 just
// because the apply succeeded — the deploy gate is health, not delivery.
func TestDeployDegradedExitsNonZero(t *testing.T) {
	spec, history := deploySpec(t)
	adapter := newFakeAdapter("direct")
	adapter.statuses = []delivery.Status{{Phase: delivery.PhaseDegraded, Cause: "web: CrashLoopBackOff"}}
	stdout, code, msg := runDelivery(t, planeOf([]delivery.Adapter{adapter}, nil, nil),
		"deploy", "-f", spec, "--env", "development", "--history", history, "--timeout", "150ms")
	if code == exitOK {
		t.Fatalf("expected a non-zero exit for a degraded deployment:\n%s", stdout)
	}
	if !strings.Contains(stdout, "Degraded") {
		t.Fatalf("stdout should report the Degraded phase:\n%s", stdout)
	}
	if !strings.Contains(msg, "unhealthy") {
		t.Fatalf("error should say the revision is applied but unhealthy, got: %s", msg)
	}
}

// TestDeployNeverPickedUpExitsNonZero: an adapter stuck reporting "not live
// yet" times out with the not-picked-up diagnosis rather than hanging or
// passing. It also covers the Proposed-after-commit mapping: a backwards
// transition would surface as an invalid-transition error instead.
func TestDeployNeverPickedUpExitsNonZero(t *testing.T) {
	spec, history := deploySpec(t)
	adapter := newFakeAdapter("direct")
	adapter.statuses = []delivery.Status{{Phase: delivery.PhaseProposed, Cause: "direct: Deployment/development/web is not live yet"}}
	stdout, code, msg := runDelivery(t, planeOf([]delivery.Adapter{adapter}, nil, nil),
		"deploy", "-f", spec, "--env", "development", "--history", history, "--timeout", "150ms")
	if code == exitOK {
		t.Fatalf("expected a non-zero exit when nothing picks the revision up:\n%s", stdout)
	}
	if strings.Contains(msg, "invalid transition") {
		t.Fatalf("a pre-pickup Proposed observation must not read as a backwards transition: %s", msg)
	}
	if !strings.Contains(stdout, "Committed") {
		t.Fatalf("stdout should report the Committed phase it is wedged in:\n%s", stdout)
	}
}

// TestDeployRolloutInFlightNeverExitsZero is the command half of the
// mid-rollout regression (see direct.TestStatusDoesNotReportHealthyMidRollout).
//
// A second, broken revision leaves the adapter reporting Applied indefinitely:
// the new pods never become available, so the rolling update keeps the previous
// revision's pods alive. The deploy gate is health, so that must time out
// non-zero rather than exit 0 the moment the apply lands.
func TestDeployRolloutInFlightNeverExitsZero(t *testing.T) {
	spec, history := deploySpec(t)
	adapter := newFakeAdapter("direct")
	adapter.statuses = []delivery.Status{{
		Phase: delivery.PhaseApplied,
		Cause: "Deployment/hello-development/web: 1 replicas of the previous revision are pending termination",
	}}
	stdout, code, msg := runDelivery(t, planeOf([]delivery.Adapter{adapter}, nil, nil),
		"deploy", "-f", spec, "--env", "development", "--history", history, "--timeout", "150ms")
	if code == exitOK {
		t.Fatalf("a rollout that never completes must not exit 0:\n%s", stdout)
	}
	if strings.Contains(stdout, "Healthy") {
		t.Fatalf("Healthy must not be reported while the rollout is in flight:\n%s", stdout)
	}
	if !strings.Contains(stdout, "Applied") {
		t.Fatalf("stdout should report the Applied phase it is wedged in:\n%s", stdout)
	}
	if !strings.Contains(msg, "never reported healthy") {
		t.Fatalf("error should say the revision was applied but never became healthy, got: %s", msg)
	}
}

// TestDeployApplyFailurePropagates: an apply that fails is the command's error,
// and nothing waits on a deployment that never started.
func TestDeployApplyFailurePropagates(t *testing.T) {
	spec, history := deploySpec(t)
	adapter := newFakeAdapter("direct")
	adapter.applyErr = errors.New("delivery/conflict: field .spec.replicas is owned by hpa-controller")
	_, code, msg := runDelivery(t, planeOf([]delivery.Adapter{adapter}, nil, nil),
		"deploy", "-f", spec, "--env", "development", "--history", history)
	if code != exitErr {
		t.Fatalf("exit = %d, want %d", code, exitErr)
	}
	if !strings.Contains(msg, "delivery/conflict") {
		t.Fatalf("apply error should reach the user verbatim, got: %s", msg)
	}
	for _, call := range adapter.callLog() {
		if call == "status" {
			t.Fatal("status must not be polled after a failed apply")
		}
	}
}

// TestDeploySelectsAdapterFromEnvironmentMode: the delivery mode is a property
// of the Environment spec, not of the command — the whole point of
// delivery.Registry.Select (issue #32).
func TestDeploySelectsAdapterFromEnvironmentMode(t *testing.T) {
	dir := t.TempDir()
	spec := writeDeliverySpec(t, dir, "spec.yaml", "flux", "https://example.test/deploy.git")
	directAdapter := newFakeAdapter("direct")
	fluxAdapter := newFakeAdapter("flux")
	var seen deliveryTarget
	connect := capturingConnector(planeOf([]delivery.Adapter{directAdapter, fluxAdapter}, nil, nil), &seen)

	stdout, code, msg := runDelivery(t, connect,
		"deploy", "-f", spec, "--env", "development", "--history", filepath.Join(dir, "history"), "--timeout", "10s")
	if code != exitOK {
		t.Fatalf("exit = %d (%s), want 0\n%s", code, msg, stdout)
	}
	if seen.mode != "flux" {
		t.Fatalf("selected mode = %q, want flux", seen.mode)
	}
	if seen.git == nil || seen.git.Repo != "https://example.test/deploy.git" {
		t.Fatalf("the git target should reach the connector, got %+v", seen.git)
	}
	if len(fluxAdapter.callLog()) == 0 {
		t.Fatal("the flux adapter was never used")
	}
	if len(directAdapter.callLog()) != 0 {
		t.Fatalf("the direct adapter must not be used for a flux environment: %v", directAdapter.callLog())
	}
}

// TestDeployModeFlagOverridesTheSpec covers the deliberate override.
func TestDeployModeFlagOverridesTheSpec(t *testing.T) {
	dir := t.TempDir()
	spec := writeDeliverySpec(t, dir, "spec.yaml", "flux", "https://example.test/deploy.git")
	directAdapter := newFakeAdapter("direct")
	fluxAdapter := newFakeAdapter("flux")
	_, code, msg := runDelivery(t, planeOf([]delivery.Adapter{directAdapter, fluxAdapter}, nil, nil),
		"deploy", "-f", spec, "--env", "development", "--mode", "direct",
		"--history", filepath.Join(dir, "history"), "--timeout", "10s")
	if code != exitOK {
		t.Fatalf("exit = %d (%s), want 0", code, msg)
	}
	if len(directAdapter.callLog()) == 0 {
		t.Fatal("--mode direct should have selected the direct adapter")
	}
}

// TestDeployUnknownModeIsAnError: an unavailable adapter fails loudly and says
// what is available, rather than silently falling back to direct.
func TestDeployUnknownModeIsAnError(t *testing.T) {
	spec, history := deploySpec(t)
	_, code, msg := runDelivery(t, planeOf([]delivery.Adapter{newFakeAdapter("direct")}, nil, nil),
		"deploy", "-f", spec, "--env", "development", "--mode", "argocd", "--history", history)
	if code != exitErr {
		t.Fatalf("exit = %d, want %d", code, exitErr)
	}
	if !strings.Contains(msg, "argocd") || !strings.Contains(msg, "not available") {
		t.Fatalf("error should name the unavailable mode, got: %s", msg)
	}
}

func TestDeployRejectsNonPositiveTimeout(t *testing.T) {
	spec, history := deploySpec(t)
	_, code, msg := runDelivery(t, planeOf([]delivery.Adapter{newFakeAdapter("direct")}, nil, nil),
		"deploy", "-f", spec, "--env", "development", "--history", history, "--timeout", "0s")
	if code != exitErr || !strings.Contains(msg, "--timeout") {
		t.Fatalf("expected a --timeout usage error, got %d: %s", code, msg)
	}
}

func TestDeployUnknownEnvironment(t *testing.T) {
	spec, history := deploySpec(t)
	_, code, msg := runDelivery(t, planeOf([]delivery.Adapter{newFakeAdapter("direct")}, nil, nil),
		"deploy", "-f", spec, "--env", "nope", "--history", history)
	if code != exitErr || !strings.Contains(msg, `environment "nope" not found`) {
		t.Fatalf("expected environment-not-found, got %d: %s", code, msg)
	}
}

// TestHistoryDirPrecedence: an explicit flag wins, then $KELSON_DATA_DIR, then
// the XDG data directory. The history is per-user because it records what is
// live in a cluster, not what is in a checkout.
func TestHistoryDirPrecedence(t *testing.T) {
	t.Setenv("KELSON_DATA_DIR", "/env/data")
	t.Setenv("XDG_DATA_HOME", "/xdg")

	got, err := historyDir("/flag/data")
	if err != nil || got != "/flag/data" {
		t.Fatalf("flag should win, got %q (%v)", got, err)
	}
	got, err = historyDir("")
	if err != nil || got != "/env/data" {
		t.Fatalf("KELSON_DATA_DIR should win over XDG, got %q (%v)", got, err)
	}

	t.Setenv("KELSON_DATA_DIR", "")
	got, err = historyDir("")
	if err != nil || got != filepath.Join("/xdg", "kelson") {
		t.Fatalf("XDG_DATA_HOME should be used, got %q (%v)", got, err)
	}
}

// TestFluxOperatorFinding: the flux adapter's FluxReport preference rides on a
// detection finding (issue #157), and "nobody looked" must stay distinct from
// "looked and found nothing" — a zero profile, or one whose probe could not
// read the group, leaves the adapter probing rather than skipping a source it
// was never told about.
func TestFluxOperatorFinding(t *testing.T) {
	present := clusterprofile.ClusterProfile{FluxOperator: &clusterprofile.Component{}}
	absent := clusterprofile.ClusterProfile{Flux: &clusterprofile.Component{}}
	gapped := clusterprofile.ClusterProfile{
		Incomplete: []clusterprofile.Gap{{Field: "fluxOperator", Reason: "forbidden: needs get on /apis"}},
	}

	cases := []struct {
		name    string
		flag    string
		profile clusterprofile.ClusterProfile
		want    *bool
	}{
		{"no profile flag", "", present, nil},
		{"detected", "cluster.yaml", present, boolPtr(true)},
		{"flux without the operator", "cluster.yaml", absent, boolPtr(false)},
		{"hidden by a gap", "from-cluster", gapped, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := fluxOperatorFinding(tc.flag, tc.profile)
			switch {
			case tc.want == nil && got != nil:
				t.Fatalf("got %v, want nil (unknown)", *got)
			case tc.want != nil && got == nil:
				t.Fatalf("got nil, want %v", *tc.want)
			case tc.want != nil && *got != *tc.want:
				t.Fatalf("got %v, want %v", *got, *tc.want)
			}
		})
	}
}

func boolPtr(b bool) *bool { return &b }
