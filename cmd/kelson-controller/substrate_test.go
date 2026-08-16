package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/dafrie/kelson/internal/clusterprofile"
	"github.com/dafrie/kelson/internal/delivery/install"
)

// --ensure-substrate's verdicts. Each one is a different promise:
//
//	present  → adopt, and touch nothing (ADR-0003 survives an automatic install)
//	absent   → install, wait, and restart the controller that detected too early
//	failing  → say so and exit non-zero, because a Helm hook that swallows an
//	           install failure is a release that reports success over a cluster
//	           that reconciles nothing — the exact outcome this mode exists for
//
// They run against fakes rather than a cluster: the delivery plane's own tests
// cover what an install does to an API server, and what is worth pinning here
// is which of the three branches the mode takes and what it says on the way.

// fakeEngine stands in for *install.Installer.
type fakeEngine struct {
	planned  []install.Request
	plan     *install.Plan
	planErr  error
	executed int
	report   *install.Report
	execErr  error
}

func (f *fakeEngine) Plan(_ context.Context, req install.Request) (*install.Plan, error) {
	f.planned = append(f.planned, req)
	if f.planErr != nil {
		return nil, f.planErr
	}
	if f.plan != nil {
		return f.plan, nil
	}
	row, _ := install.Substrate()
	return &install.Plan{Items: []install.Item{{Component: row}}}, nil
}

func (f *fakeEngine) Execute(_ context.Context, _ *install.Plan) (*install.Report, error) {
	f.executed++
	return f.report, f.execErr
}

// fakeCluster records what the mode did to the cluster beyond installing.
type fakeCluster struct {
	engine     *fakeEngine
	served     map[string]bool
	servesErr  error
	restarted  []string
	restartErr error
	connects   int
}

func newFakeCluster() *fakeCluster {
	return &fakeCluster{
		engine: &fakeEngine{},
		served: map[string]bool{
			kindName(0): true,
			kindName(1): true,
		},
	}
}

// kindName names one of the two kinds the spine writes, by index, so a test can say
// "this one is not served yet" without repeating a group string.
func kindName(i int) string { return substrateKinds[i].Resource + "." + substrateKinds[i].Group }

func (c *fakeCluster) deps(profile clusterprofile.ClusterProfile, detectErr error) substrateDeps {
	return substrateDeps{
		detect: func(string) (clusterprofile.ClusterProfile, error) { return profile, detectErr },
		connect: func(string) (*substrateCluster, error) {
			c.connects++
			return &substrateCluster{
				engine: c.engine,
				serves: func(_ context.Context, gvr schema.GroupVersionResource) (bool, error) {
					if c.servesErr != nil {
						return false, c.servesErr
					}
					return c.served[gvr.Resource+"."+gvr.Group], nil
				},
				restart: func(_ context.Context, namespace, name string) error {
					c.restarted = append(c.restarted, namespace+"/"+name)
					return c.restartErr
				},
			}, nil
		},
		poll: time.Millisecond,
	}
}

func ensureConfig() config {
	return config{substrateTimeout: 50 * time.Millisecond, substrateRestart: "kelson-system/kelson-controller"}
}

// TestEnsureSubstrateAdoptsAnExistingFlux is ADR-0003 under the one condition
// that makes it easy to break: the install is automatic, so nobody is there to
// read a preview and stop it. A cluster that already reconciles must come out
// of a `helm install` byte-identical.
func TestEnsureSubstrateAdoptsAnExistingFlux(t *testing.T) {
	cluster := newFakeCluster()
	profile := clusterprofile.ClusterProfile{
		Flux: &clusterprofile.Component{Version: "v2.9.4", Namespace: "flux-system"},
	}
	var out strings.Builder

	if err := ensureSubstrate(context.Background(), ensureConfig(), &out, cluster.deps(profile, nil)); err != nil {
		t.Fatalf("adopting an existing Flux failed: %v", err)
	}
	if cluster.connects != 0 {
		t.Error("the adopt path connected an install client; nothing should be applied, so nothing needs one")
	}
	if cluster.engine.executed != 0 {
		t.Error("the adopt path executed an install plan over a cluster that already had Flux")
	}
	if len(cluster.restarted) != 0 {
		t.Errorf("the adopt path restarted %v; the controller's own probe already saw this Flux", cluster.restarted)
	}
	if !strings.Contains(out.String(), "adopting") || !strings.Contains(out.String(), "v2.9.4") {
		t.Errorf("the adopt path did not say what it found:\n%s", out.String())
	}
}

// TestEnsureSubstrateInstallsWhenAbsent: the owner decision itself. It also
// pins the two things that make the install converge rather than merely apply —
// the wait on the kinds, and the restart of a controller whose one-shot
// detection ran before the substrate existed.
func TestEnsureSubstrateInstallsWhenAbsent(t *testing.T) {
	cluster := newFakeCluster()
	var out strings.Builder

	if err := ensureSubstrate(context.Background(), ensureConfig(), &out, cluster.deps(clusterprofile.ClusterProfile{}, nil)); err != nil {
		t.Fatalf("installing into a cluster with no Flux failed: %v", err)
	}
	if cluster.engine.executed != 1 {
		t.Fatalf("executed %d plans, want exactly 1", cluster.engine.executed)
	}
	if len(cluster.engine.planned) != 1 {
		t.Fatalf("planned %d times, want 1", len(cluster.engine.planned))
	}
	req := cluster.engine.planned[0]
	// Named, never swept: `--all-missing` would install cert-manager and
	// CloudNativePG too, and a `helm install` is nobody's decision to do that.
	if req.AllMissing {
		t.Error("the hook planned a --all-missing sweep; installing kelson installs the substrate and nothing else")
	}
	if want := install.SubstrateName(); len(req.Components) != 1 || req.Components[0] != want {
		t.Errorf("planned components = %v, want [%s] (ADR-0030's preference)", req.Components, want)
	}
	if len(cluster.restarted) != 1 || cluster.restarted[0] != "kelson-system/kelson-controller" {
		t.Errorf("restarted %v, want the one Deployment named by --substrate-restart-deployment", cluster.restarted)
	}
}

// TestEnsureSubstrateReportsAFailedInstall: the loud half. A hook that returned
// zero here would leave a green release over a cluster that reconciles nothing,
// which is the failure the whole mechanism exists to prevent.
func TestEnsureSubstrateReportsAFailedInstall(t *testing.T) {
	cluster := newFakeCluster()
	cluster.engine.execErr = errors.New("customresourcedefinitions.apiextensions.k8s.io is forbidden")
	cluster.engine.report = &install.Report{Components: []install.ComponentReport{{
		Component: mustSubstrate(t),
		Results: []install.Result{{
			Ref:     install.Ref{Kind: "CustomResourceDefinition", Name: "kustomizations.kustomize.toolkit.fluxcd.io"},
			Outcome: install.OutcomeFailed,
			Detail:  "is forbidden",
		}},
		Failed: 1,
	}}}
	var out strings.Builder

	err := ensureSubstrate(context.Background(), ensureConfig(), &out, cluster.deps(clusterprofile.ClusterProfile{}, nil))
	if err == nil {
		t.Fatal("a failed install exited 0; the Helm release would report success over a cluster that cannot reconcile")
	}
	if !strings.Contains(err.Error(), "is forbidden") {
		t.Errorf("the error hides the cluster's own reason: %v", err)
	}
	// The per-object detail is printed rather than only summarised: "the
	// install failed" is not something an operator can act on.
	if !strings.Contains(out.String(), "kustomizations.kustomize.toolkit.fluxcd.io") {
		t.Errorf("the report does not name the object that failed:\n%s", out.String())
	}
	if len(cluster.restarted) != 0 {
		t.Error("a failed install still restarted the controller; there is nothing new for it to detect")
	}
}

// TestEnsureSubstrateRefusesOnUnknownDetection: installing on an Unknown is the
// guess the tri-state exists to prevent (issue #144, ADR-0021). An unattended
// caller makes that mistake more easily than a person does, not less.
func TestEnsureSubstrateRefusesOnUnknownDetection(t *testing.T) {
	cluster := newFakeCluster()
	profile := clusterprofile.ClusterProfile{Incomplete: []clusterprofile.Gap{
		{Field: "flux", Reason: "listing apis is forbidden"},
	}}
	var out strings.Builder

	err := ensureSubstrate(context.Background(), ensureConfig(), &out, cluster.deps(profile, nil))
	if err == nil {
		t.Fatal("a detection gap was treated as absence; that installs a second Flux beside one kelson could not see")
	}
	if !strings.Contains(err.Error(), "forbidden") {
		t.Errorf("the refusal does not name the permission that would settle it: %v", err)
	}
	if cluster.engine.executed != 0 {
		t.Error("the Unknown path applied something")
	}
}

// TestEnsureSubstrateWaitsForTheKindsKelsonWrites: an install that applied and
// returned would hand back a green hook over a cluster that still cannot serve
// an OCIRepository — which is precisely the state the controller reports as
// FluxNotInstalled.
func TestEnsureSubstrateWaitsForTheKindsKelsonWrites(t *testing.T) {
	cluster := newFakeCluster()
	cluster.served[kindName(1)] = false
	var out strings.Builder

	err := ensureSubstrate(context.Background(), ensureConfig(), &out, cluster.deps(clusterprofile.ClusterProfile{}, nil))
	if err == nil {
		t.Fatal("the wait returned success while a kind kelson writes was not served")
	}
	if !strings.Contains(err.Error(), kindName(1)) {
		t.Errorf("the timeout does not name the kind that never appeared: %v", err)
	}
	if len(cluster.restarted) != 0 {
		t.Error("the controller was restarted after a wait that timed out; it would detect the same absence again")
	}
}

// TestEnsureSubstrateReportsAFailedRestart: the restart is not a nicety. The
// controller detects Flux once at start-up, so a bounce that silently failed
// leaves every environment reporting FluxNotInstalled with a green release over
// it — and the error has to carry the one command that fixes it.
func TestEnsureSubstrateReportsAFailedRestart(t *testing.T) {
	cluster := newFakeCluster()
	cluster.restartErr = errors.New("deployments.apps \"kelson-controller\" is forbidden")
	var out strings.Builder

	err := ensureSubstrate(context.Background(), ensureConfig(), &out, cluster.deps(clusterprofile.ClusterProfile{}, nil))
	if err == nil {
		t.Fatal("a failed restart exited 0; the controller would keep reporting FluxNotInstalled")
	}
	if !strings.Contains(err.Error(), "rollout restart") {
		t.Errorf("the error does not say how to finish the job by hand: %v", err)
	}
}

// TestEnsureSubstrateWithoutARestartTargetInstallsAnyway: the flag is optional,
// because a cluster installing kelson for the first time with the controller
// disabled has nothing to bounce.
func TestEnsureSubstrateWithoutARestartTargetInstallsAnyway(t *testing.T) {
	cluster := newFakeCluster()
	cfg := ensureConfig()
	cfg.substrateRestart = ""
	var out strings.Builder

	if err := ensureSubstrate(context.Background(), cfg, &out, cluster.deps(clusterprofile.ClusterProfile{}, nil)); err != nil {
		t.Fatalf("ensure with no restart target failed: %v", err)
	}
	if len(cluster.restarted) != 0 {
		t.Errorf("restarted %v with no --substrate-restart-deployment set", cluster.restarted)
	}
}

// TestEnsureSubstrateSurfacesARefusedPlan: a plan with no items is not an
// error inside the delivery plane — it is a Refusal carrying a reason and a
// remediation — and dropping either one leaves a hook that failed for no
// visible cause.
func TestEnsureSubstrateSurfacesARefusedPlan(t *testing.T) {
	cluster := newFakeCluster()
	cluster.engine.plan = &install.Plan{Refusals: []install.Refusal{{
		Name:        install.SubstrateName(),
		Outcome:     clusterprofile.OutcomeNo,
		Reason:      "this kelson build carries no rendered manifests for it",
		Remediation: "use a released kelson, or run `make flux-aio` and rebuild",
	}}}
	var out strings.Builder

	err := ensureSubstrate(context.Background(), ensureConfig(), &out, cluster.deps(clusterprofile.ClusterProfile{}, nil))
	if err == nil {
		t.Fatal("a plan that would install nothing was reported as success")
	}
	for _, want := range []string{"no rendered manifests", "make flux-aio"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal dropped %q: %v", want, err)
		}
	}
}

// TestEnsureSubstrateFailsOnAFailedProbe: detection failing is not detection
// saying "absent". The controller draws that distinction at start-up
// (startupProfile) and so does this: installing because the probe broke is how
// a cluster gets a second Flux.
func TestEnsureSubstrateFailsOnAFailedProbe(t *testing.T) {
	cluster := newFakeCluster()
	var out strings.Builder

	err := ensureSubstrate(context.Background(), ensureConfig(), &out,
		cluster.deps(clusterprofile.ClusterProfile{}, fmt.Errorf("dial tcp: connection refused")))
	if err == nil {
		t.Fatal("a failed probe was treated as a cluster with no Flux")
	}
	if cluster.engine.executed != 0 {
		t.Error("something was applied after the probe failed")
	}
}

func TestSplitDeploymentRef(t *testing.T) {
	ns, name, err := splitDeploymentRef("kelson-system/kelson-controller")
	if err != nil || ns != "kelson-system" || name != "kelson-controller" {
		t.Fatalf("splitDeploymentRef = (%q, %q, %v)", ns, name, err)
	}
	for _, bad := range []string{"", "kelson-controller", "/kelson-controller", "kelson-system/", "   "} {
		if _, _, err := splitDeploymentRef(bad); err == nil {
			t.Errorf("splitDeploymentRef(%q) was accepted; a half-specified reference restarts something else", bad)
		}
	}
}

// TestParseFlagsSubstrateMode: the mode's own command line, refused early
// rather than half-honoured. A Job that passed --substrate-restart-deployment
// and forgot --ensure-substrate would start a second controller and restart
// nothing, which looks from the outside exactly like the mode running.
func TestParseFlagsSubstrateMode(t *testing.T) {
	cfg, err := parseFlags([]string{"--ensure-substrate",
		"--substrate-restart-deployment=kelson-system/kelson-controller"}, io.Discard)
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if !cfg.ensureSubstrate {
		t.Error("--ensure-substrate did not resolve")
	}
	if cfg.substrateTimeout != defaultSubstrateTimeout {
		t.Errorf("substrate timeout is %s, want the default %s", cfg.substrateTimeout, defaultSubstrateTimeout)
	}

	for _, args := range [][]string{
		{"--substrate-restart-deployment=kelson-system/kelson-controller"},
		{"--ensure-substrate", "--substrate-restart-deployment=kelson-controller"},
		{"--ensure-substrate", "--substrate-timeout=0"},
	} {
		if _, err := parseFlags(args, io.Discard); err == nil {
			t.Errorf("parseFlags(%v) was accepted", args)
		}
	}

	// The default is off: an ordinary controller Deployment must not turn into
	// a one-shot installer because a flag flipped.
	base, err := parseFlags(nil, io.Discard)
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if base.ensureSubstrate {
		t.Error("--ensure-substrate defaults on; the running controller would exit after installing Flux")
	}
}

func mustSubstrate(t *testing.T) install.Component {
	t.Helper()
	c, ok := install.Substrate()
	if !ok {
		t.Fatal("the install catalog has no substrate row")
	}
	return c
}
