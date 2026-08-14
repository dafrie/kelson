package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/dafrie/kelson/internal/clusterprofile"
	"github.com/dafrie/kelson/internal/delivery/install"
)

// errNoRemover is what the project-path harness in uninstall_test.go returns
// for a connector it must never call.
var errNoRemover = errors.New("no remover in this test")

// --- fakes -------------------------------------------------------------------

type fakeInstaller struct {
	plan     *install.Plan
	planErr  error
	report   *install.Report
	execErr  error
	executed bool
	// requested records the request Plan was called with, so a test can assert
	// that detection's profile actually reached the planner.
	requested install.Request
}

func (f *fakeInstaller) Plan(_ context.Context, req install.Request) (*install.Plan, error) {
	f.requested = req
	if f.planErr != nil {
		return nil, f.planErr
	}
	return f.plan, nil
}

func (f *fakeInstaller) Execute(context.Context, *install.Plan) (*install.Report, error) {
	f.executed = true
	return f.report, f.execErr
}

type fakeRemover struct {
	removal  *install.Removal
	planErr  error
	report   *install.RemovalReport
	execErr  error
	executed bool
}

func (f *fakeRemover) Plan(_ context.Context, _ string) (*install.Removal, error) {
	if f.planErr != nil {
		return nil, f.planErr
	}
	return f.removal, nil
}

func (f *fakeRemover) Execute(context.Context, *install.Removal) (*install.RemovalReport, error) {
	f.executed = true
	return f.report, f.execErr
}

func runInstallCmd(t *testing.T, engine *fakeInstaller, profile clusterprofile.ClusterProfile, stdin string, args ...string) (stdout, stderr string, code int, msg string) {
	t.Helper()
	root := &cobra.Command{Use: "kelson", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(newInstallCmdFactory(
		func(installOptions) (installer, error) { return engine, nil },
		func(string) (clusterprofile.ClusterProfile, error) { return profile, nil },
	))
	var outBuf, errBuf bytes.Buffer
	root.SetOut(&outBuf)
	root.SetErr(&errBuf)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(args)
	err := root.Execute()
	msg, code = resolveExit(err)
	return outBuf.String(), errBuf.String(), code, msg
}

func runComponentUninstallCmd(t *testing.T, engine *fakeRemover, stdin string, args ...string) (stdout string, code int, msg string) {
	t.Helper()
	root := &cobra.Command{Use: "kelson", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(newUninstallCmdFactory(
		func(uninstallOptions) (uninstaller, error) { return nil, errors.New("no uninstaller in this test") },
		func(uninstallOptions) (remover, error) { return engine, nil },
		func(string) (historyStore, error) { return nil, errors.New("no history in this test") },
	))
	var outBuf, errBuf bytes.Buffer
	root.SetOut(&outBuf)
	root.SetErr(&errBuf)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(args)
	err := root.Execute()
	msg, code = resolveExit(err)
	return outBuf.String(), code, msg
}

// --- fixtures ----------------------------------------------------------------

func certManagerPin(t *testing.T) install.Component {
	t.Helper()
	c, ok := install.Lookup("cert-manager")
	if !ok {
		t.Fatal("the pins table has no cert-manager row")
	}
	return c
}

func planWith(t *testing.T, objects ...install.Object) *install.Plan {
	t.Helper()
	c := certManagerPin(t)
	return &install.Plan{Items: []install.Item{{Component: c, Objects: objects, Digest: c.SHA256}}}
}

func objectRef(kind, namespace, name string) install.Object {
	return install.Object{Ref: install.Ref{APIVersion: "v1", Kind: kind, Namespace: namespace, Name: name}}
}

// --- install -----------------------------------------------------------------

// TestInstallPreviewNamesThePinAndTheDigest: what kelson is about to fetch and
// apply is checkable with curl before the prompt is answered.
func TestInstallPreviewNamesThePinAndTheDigest(t *testing.T) {
	pin := certManagerPin(t)
	engine := &fakeInstaller{plan: planWith(t,
		objectRef("Namespace", "", "cert-manager"),
		objectRef("Deployment", "cert-manager", "cert-manager"),
	)}
	out, _, code, msg := runInstallCmd(t, engine, clusterprofile.ClusterProfile{}, "", "install", "cert-manager", "--yes")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, msg)
	}
	for _, want := range []string{pin.ManifestURL, pin.SHA256, pin.Version, "namespace cert-manager",
		"Namespace/cert-manager", "Deployment/cert-manager/cert-manager", install.LabelComponent} {
		if !strings.Contains(out, want) {
			t.Fatalf("preview does not mention %q:\n%s", want, out)
		}
	}
	// The preview comes before what it previews, exactly as uninstall's does.
	if strings.Index(out, pin.SHA256) > strings.Index(out, "created,") && strings.Contains(out, "created,") {
		t.Fatalf("the preview printed after the applies:\n%s", out)
	}
}

// TestInstallConfirmation covers the three answers to step two. A terminal is
// not something a test has, so interactivity is passed in.
func TestInstallConfirmation(t *testing.T) {
	plan := planWith(t, objectRef("Namespace", "", "cert-manager"))
	cases := []struct {
		name   string
		yes    bool
		asks   bool
		answer string
		want   bool
		errors bool
		prints string
	}{
		{name: "--yes needs no answer", yes: true, asks: false, want: true},
		{name: "no terminal and no --yes is a refusal", asks: false, errors: true},
		{name: "declined", asks: true, answer: "n\n", prints: "aborted"},
		{name: "accepted", asks: true, answer: "y\n", want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			out := &printer{w: &buf}
			cmd := &cobra.Command{}
			cmd.SetOut(&buf)
			cmd.SetIn(strings.NewReader(tc.answer))

			ok, err := confirmInstall(cmd, &installOptions{yes: tc.yes}, plan, out, tc.asks)
			if tc.errors {
				if err == nil {
					t.Fatal("a non-terminal run with no --yes was allowed to proceed")
				}
				return
			}
			if err != nil {
				t.Fatalf("confirm: %v", err)
			}
			if ok != tc.want {
				t.Fatalf("confirm = %v, want %v", ok, tc.want)
			}
			if tc.prints != "" && !strings.Contains(buf.String(), tc.prints) {
				t.Errorf("output does not contain %q:\n%s", tc.prints, buf.String())
			}
		})
	}
}

// TestRemovalPromptNamesTheCollateral: the question a human answers has to
// carry the irreversible part — deleting a CRD deletes every custom resource of
// that kind — not just a resource count.
func TestRemovalPromptNamesTheCollateral(t *testing.T) {
	var buf bytes.Buffer
	out := &printer{w: &buf}
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)
	cmd.SetIn(strings.NewReader("n\n"))

	removal := removalWith(t, install.RemovalTarget{
		Ref:        install.Ref{Kind: "CustomResourceDefinition", Name: "clusters.postgresql.cnpg.io"},
		Tier:       install.TierCRD,
		Collateral: []string{"Cluster/checkout-production/checkout-db"},
	})
	if _, err := confirmRemoval(cmd, &uninstallOptions{}, removal, out, true); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if !strings.Contains(buf.String(), "custom resource(s) with them") {
		t.Fatalf("the question does not carry the collateral:\n%s", buf.String())
	}
}

// TestInstallRefusesUnattendedWithoutYes: an install writes cluster-scoped RBAC,
// CRDs and webhook configurations, and a pipe must not be able to say yes.
func TestInstallRefusesUnattendedWithoutYes(t *testing.T) {
	engine := &fakeInstaller{plan: planWith(t, objectRef("Namespace", "", "cert-manager"))}
	out, _, code, msg := runInstallCmd(t, engine, clusterprofile.ClusterProfile{}, "", "install", "cert-manager")
	if code == 0 {
		t.Fatalf("unattended install succeeded:\n%s", out)
	}
	if !strings.Contains(msg, "--yes") {
		t.Fatalf("error %q does not name the flag that would allow it", msg)
	}
	if engine.executed {
		t.Fatal("the plan was applied with nobody to ask")
	}
}

// TestInstallDryRunStopsAfterThePreview.
func TestInstallDryRunStopsAfterThePreview(t *testing.T) {
	engine := &fakeInstaller{plan: planWith(t, objectRef("Namespace", "", "cert-manager"))}
	out, _, code, msg := runInstallCmd(t, engine, clusterprofile.ClusterProfile{}, "", "install", "cert-manager", "--dry-run")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, msg)
	}
	if engine.executed {
		t.Fatal("--dry-run applied the plan")
	}
	if !strings.Contains(out, "nothing was applied") {
		t.Fatalf("output does not say nothing was applied:\n%s", out)
	}
}

// TestInstallPassesDetectionToThePlanner: eligibility is detection's answer,
// and the command must not decide it itself.
func TestInstallPassesDetectionToThePlanner(t *testing.T) {
	profile := clusterprofile.ClusterProfile{CertManager: &clusterprofile.CertManager{Version: "v1.20.0"}}
	engine := &fakeInstaller{plan: &install.Plan{Refusals: []install.Refusal{{
		Name:        "cert-manager",
		Outcome:     clusterprofile.OutcomeYes,
		Reason:      "cert-manager v1.20.0 is present",
		Remediation: "kelson never modifies a component it did not install",
	}}}}
	out, _, code, msg := runInstallCmd(t, engine, profile, "", "install", "cert-manager")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, msg)
	}
	if engine.requested.Profile.CertManager == nil {
		t.Fatal("the detected profile did not reach the planner")
	}
	if !strings.Contains(out, "not installed") || !strings.Contains(out, "v1.20.0") {
		t.Fatalf("refusal is not reported:\n%s", out)
	}
	if engine.executed {
		t.Fatal("a plan with no items was executed")
	}
}

// TestInstallWarnsAboutDetectionGaps on stderr, because a profile that could
// not be read completely changes what this command will do.
func TestInstallWarnsAboutDetectionGaps(t *testing.T) {
	profile := clusterprofile.ClusterProfile{
		Incomplete: []clusterprofile.Gap{{Field: "cnpg", Reason: "forbidden: needs get on /apis"}},
	}
	engine := &fakeInstaller{plan: &install.Plan{}}
	_, errOut, code, msg := runInstallCmd(t, engine, profile, "", "install", "--all-missing")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, msg)
	}
	if !strings.Contains(errOut, "needs get on /apis") {
		t.Fatalf("stderr does not carry the detection gap:\n%s", errOut)
	}
}

// TestInstallReportsAdoption: "kelson installed it" and "kelson created all of
// it" are different claims, and the second one is often false.
func TestInstallReportsAdoption(t *testing.T) {
	pin := certManagerPin(t)
	engine := &fakeInstaller{
		plan: planWith(t, objectRef("Namespace", "", "cert-manager")),
		report: &install.Report{Components: []install.ComponentReport{{
			Component: pin,
			Results: []install.Result{
				{Ref: install.Ref{Kind: "Namespace", Name: "cert-manager"}, Outcome: install.OutcomeAdopted,
					Detail: "it was already there"},
			},
			Adopted:   1,
			Installed: true,
		}}},
	}
	out, _, code, msg := runInstallCmd(t, engine, clusterprofile.ClusterProfile{}, "", "install", "cert-manager", "--yes")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, msg)
	}
	if !strings.Contains(out, "adopted") {
		t.Fatalf("report does not mention the adoption:\n%s", out)
	}
	if !strings.Contains(out, "NOT kelson's to") {
		t.Fatalf("report does not say adopted resources will not be deleted:\n%s", out)
	}
	if !strings.Contains(out, "ClusterIssuer") {
		t.Fatalf("the boundary does not say cert-manager is unconfigured:\n%s", out)
	}
}

// TestInstallEnvoyGatewayBoundary: "installed" is exactly the moment somebody
// assumes it is configured, and an Envoy Gateway with no GatewayClass routes
// nothing. The way out must say whose decision that is.
func TestInstallEnvoyGatewayBoundary(t *testing.T) {
	eg, ok := install.Lookup("envoy-gateway")
	if !ok {
		t.Fatal("the pins table has no envoy-gateway row")
	}
	engine := &fakeInstaller{
		plan: &install.Plan{Items: []install.Item{{
			Component: eg,
			Objects:   []install.Object{objectRef("Namespace", "", "envoy-gateway-system")},
			Digest:    eg.SHA256,
		}}},
		report: &install.Report{Components: []install.ComponentReport{{
			Component: eg,
			Results: []install.Result{{Ref: install.Ref{Kind: "Namespace", Name: "envoy-gateway-system"},
				Outcome: install.OutcomeCreated}},
			Created:   1,
			Installed: true,
		}}},
	}
	out, _, code, msg := runInstallCmd(t, engine, clusterprofile.ClusterProfile{}, "", "install", "envoy-gateway", "--yes")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, msg)
	}
	if !strings.Contains(out, "GatewayClass") || !strings.Contains(out, "gateway.envoyproxy.io/gatewayclass-controller") {
		t.Fatalf("the boundary does not say a GatewayClass is still the user's to create:\n%s", out)
	}
}

// TestInstallAddressing covers the two ways the request addresses nothing.
func TestInstallAddressing(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"nothing", []string{"install"}, "addressed by component"},
		{"both", []string{"install", "flux", "--all-missing"}, "disagree"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			engine := &fakeInstaller{plan: &install.Plan{}}
			_, _, code, msg := runInstallCmd(t, engine, clusterprofile.ClusterProfile{}, "", tc.args...)
			if code == 0 {
				t.Fatalf("%v succeeded", tc.args)
			}
			if !strings.Contains(msg, tc.want) {
				t.Fatalf("error %q does not contain %q", msg, tc.want)
			}
		})
	}
}

// --- component uninstall -------------------------------------------------------

func removalWith(t *testing.T, targets ...install.RemovalTarget) *install.Removal {
	t.Helper()
	c := certManagerPin(t)
	return &install.Removal{Component: c, Targets: targets}
}

// TestComponentUninstallPreviewNamesCRDCollateral: deleting an operator's CRDs
// deletes every custom resource of that kind in the cluster.
func TestComponentUninstallPreviewNamesCRDCollateral(t *testing.T) {
	engine := &fakeRemover{removal: removalWith(t,
		install.RemovalTarget{
			Ref:        install.Ref{APIVersion: "apiextensions.k8s.io/v1", Kind: "CustomResourceDefinition", Name: "clusters.postgresql.cnpg.io"},
			Tier:       install.TierCRD,
			Collateral: []string{"Cluster/checkout-production/checkout-db"},
		},
	)}
	engine.removal.Kept = []install.Bystander{{
		Ref:    install.Ref{APIVersion: "v1", Kind: "Namespace", Name: "cert-manager"},
		Reason: "it was already in the cluster when kelson installed cert-manager",
	}}
	out, code, msg := runComponentUninstallCmd(t, engine, "", "uninstall", "--component", "cert-manager")
	if code == 0 {
		t.Fatalf("a non-terminal run with no --yes deleted anyway:\n%s", out)
	}
	if !strings.Contains(msg, "--yes") {
		t.Fatalf("the refusal does not name the flag that would allow it: %q", msg)
	}
	if engine.executed {
		t.Fatal("it deleted anyway")
	}
	if !strings.Contains(out, "goes with it: Cluster/checkout-production/checkout-db") {
		t.Fatalf("preview does not name what the CRD deletion takes with it:\n%s", out)
	}
	if !strings.Contains(out, "Left alone") || !strings.Contains(out, "already in the cluster") {
		t.Fatalf("preview does not report what kelson refuses to delete:\n%s", out)
	}
	if !strings.Contains(out, install.Selector("cert-manager")) {
		t.Fatalf("preview does not print the selector:\n%s", out)
	}
}

// TestComponentUninstallNothingToDo: a component kelson did not install is not
// kelson's to remove, and saying so is a success.
func TestComponentUninstallNothingToDo(t *testing.T) {
	engine := &fakeRemover{removal: removalWith(t)}
	out, code, msg := runComponentUninstallCmd(t, engine, "", "uninstall", "--component", "cert-manager")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, msg)
	}
	if !strings.Contains(out, "kelson did not install cert-manager") {
		t.Fatalf("output does not say why there is nothing to do:\n%s", out)
	}
	if engine.executed {
		t.Fatal("an empty removal was executed")
	}
}

// TestComponentUninstallRefusesProjectFlags: silently ignoring a flag leaves a
// wrong belief about what is happening intact.
func TestComponentUninstallRefusesProjectFlags(t *testing.T) {
	engine := &fakeRemover{removal: removalWith(t)}
	_, code, msg := runComponentUninstallCmd(t, engine,
		"", "uninstall", "--component", "cert-manager", "--keep-data", "--project", "checkout")
	if code == 0 {
		t.Fatal("--component accepted the project flags")
	}
	if !strings.Contains(msg, "--keep-data") || !strings.Contains(msg, "--project") {
		t.Fatalf("error %q does not name both flags that do not apply", msg)
	}
}

// TestUninstallWithoutProjectOrComponent.
func TestUninstallWithoutProjectOrComponent(t *testing.T) {
	engine := &fakeRemover{removal: removalWith(t)}
	_, code, msg := runComponentUninstallCmd(t, engine, "", "uninstall")
	if code == 0 {
		t.Fatal("an uninstall that addresses nothing succeeded")
	}
	if !strings.Contains(msg, "--component") {
		t.Fatalf("error %q does not offer both ways to address an uninstall", msg)
	}
}

// TestComponentUninstallDeletes and reports the boundary.
func TestComponentUninstallDeletes(t *testing.T) {
	engine := &fakeRemover{
		removal: removalWith(t, install.RemovalTarget{
			Ref:  install.Ref{APIVersion: "apps/v1", Kind: "Deployment", Namespace: "cert-manager", Name: "cert-manager"},
			Tier: install.TierWorkload,
		}),
		report: &install.RemovalReport{
			Results: []install.RemovalResult{{
				Ref:     install.Ref{Kind: "Deployment", Namespace: "cert-manager", Name: "cert-manager"},
				Outcome: install.RemovalDeleted,
			}},
			Deleted: 1,
		},
	}
	out, code, msg := runComponentUninstallCmd(t, engine, "", "uninstall", "--component", "cert-manager", "--yes")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, msg)
	}
	if !engine.executed {
		t.Fatal("--yes did not execute the removal")
	}
	if !strings.Contains(out, "1 deleted") {
		t.Fatalf("report is missing:\n%s", out)
	}
	if !strings.Contains(out, "Still installed") || !strings.Contains(out, "adopted rather than created") {
		t.Fatalf("boundary is missing:\n%s", out)
	}
}
