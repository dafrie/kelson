package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/delivery/direct"
	"github.com/dafrie/kelson/internal/delivery/uninstall"
)

// `kelson uninstall` is a destructive command, so what these tests assert is
// mostly what it does NOT do: it does not delete without printing the preview
// first, it does not delete without an answer, and it does not report a
// deletion it did not make.
//
// The uninstaller is faked for the reason every command test here fakes its
// plane — the real one needs a cluster. internal/delivery/uninstall's own tests
// drive the sweep against a fake clientset; these drive the wiring.

type fakeUninstaller struct {
	plan *uninstall.Plan
	// planned and executed record what the command asked for.
	planned  []uninstall.Scope
	executed []*uninstall.Plan
	planErr  error
	execErr  error
	report   *uninstall.Report
}

func (f *fakeUninstaller) Plan(_ context.Context, scope uninstall.Scope) (*uninstall.Plan, error) {
	f.planned = append(f.planned, scope)
	if f.planErr != nil {
		return nil, f.planErr
	}
	plan := f.plan
	if plan == nil {
		plan = &uninstall.Plan{Scope: scope}
	}
	plan.Scope = scope
	return plan, nil
}

func (f *fakeUninstaller) Execute(_ context.Context, plan *uninstall.Plan) (*uninstall.Report, error) {
	f.executed = append(f.executed, plan)
	if f.report != nil {
		return f.report, f.execErr
	}
	report := &uninstall.Report{}
	for _, t := range plan.Targets {
		report.Results = append(report.Results, uninstall.Result{Ref: t.Ref, Outcome: uninstall.OutcomeDeleted})
		report.Deleted++
	}
	return report, f.execErr
}

// fakeHistory is the local rendered-history seam.
type fakeHistory struct {
	records   []direct.Record
	envs      []string
	forgot    []string
	forgetErr error
}

func (f *fakeHistory) List(project, environment string) ([]direct.Record, error) {
	_, _ = project, environment
	return f.records, nil
}

func (f *fakeHistory) Environments(string) ([]string, error) { return f.envs, nil }

func (f *fakeHistory) Forget(project, environment string) (int, error) {
	f.forgot = append(f.forgot, project+"/"+environment)
	return len(f.records), f.forgetErr
}

func (f *fakeHistory) ForgetProject(project string) (int, error) {
	f.forgot = append(f.forgot, project+"/*")
	return len(f.records), f.forgetErr
}

func runUninstallCmd(t *testing.T, engine *fakeUninstaller, history *fakeHistory, stdin string, args ...string) (stdout string, code int, msg string) {
	t.Helper()
	root := &cobra.Command{Use: "kelson", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(newUninstallCmdFactory(
		func(uninstallOptions) (uninstaller, error) { return engine, nil },
		// The project path never reaches the component remover; the component
		// path has its own harness in install_test.go.
		func(uninstallOptions) (remover, error) { return nil, errNoRemover },
		func(string) (historyStore, error) { return history, nil },
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

// target builds a plan entry for these tests.
func target(kind, name string, tier uninstall.Tier, mutate ...func(*uninstall.Target)) uninstall.Target {
	t := uninstall.Target{
		Ref:  uninstall.Ref{APIVersion: "v1", Kind: kind, Name: name, Namespace: "checkout-production"},
		Tier: tier,
	}
	for _, m := range mutate {
		m(&t)
	}
	return t
}

func fullPlan() *uninstall.Plan {
	return &uninstall.Plan{
		Namespaces: []uninstall.Namespace{{
			Name:      "checkout-production",
			Ownership: delivery.NamespaceOwnershipCreated,
			Delete:    true,
		}},
		Targets: []uninstall.Target{
			target("HTTPRoute", "web", uninstall.TierRoute),
			target("Deployment", "web", uninstall.TierWorkload),
			target("Secret", "checkout-db", uninstall.TierConfig, func(t *uninstall.Target) { t.ManagedSecret = true }),
			target("Cluster", "checkout-production-db", uninstall.TierData, func(t *uninstall.Target) {
				t.Note = "a CloudNativePG Postgres cluster: the operator deletes the volumes it created along with it"
				t.Collateral = []string{"PersistentVolumeClaim/checkout-production-db-1"}
			}),
			target("Namespace", "checkout-production", uninstall.TierNamespace),
		},
	}
}

// TestUninstallPreviewsBeforeDeleting is step one of the three-step contract.
// The preview lists every object by kind and name, the data section is present
// and loud, and the managed Secret has its own line — all before the
// confirmation is even asked.
func TestUninstallPreviewsBeforeDeleting(t *testing.T) {
	engine := &fakeUninstaller{plan: fullPlan()}
	stdout, code, msg := runUninstallCmd(t, engine, &fakeHistory{}, "",
		"uninstall", "--project", "checkout", "--env", "production", "--yes")
	if code != exitOK {
		t.Fatalf("exit = %d (%s)\n%s", code, msg, stdout)
	}

	for _, want := range []string{
		"HTTPRoute/checkout-production/web",
		"Deployment/checkout-production/web",
		"Secret/checkout-production/checkout-db",
		"Cluster/checkout-production/checkout-production-db",
		"Namespace/checkout-production",
		"DATA",
		"nothing can undo",
		"PersistentVolumeClaim/checkout-production-db-1",
		"kelson keeps no copy of the value",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the preview does not mention %q\n%s", want, stdout)
		}
	}
	// The preview comes before the deletions it previews.
	if strings.Index(stdout, "DATA") > strings.Index(stdout, "deleted ") {
		t.Errorf("the preview printed after the deletions\n%s", stdout)
	}
}

// TestUninstallRefusesWithoutYesOnANonTerminal is step two. A non-terminal
// stdin has nobody to ask, and assuming either answer is wrong: assuming yes
// deletes a database unattended, assuming no makes a script that looks like it
// worked.
func TestUninstallRefusesWithoutYesOnANonTerminal(t *testing.T) {
	engine := &fakeUninstaller{plan: fullPlan()}
	stdout, code, msg := runUninstallCmd(t, engine, &fakeHistory{}, "",
		"uninstall", "--project", "checkout", "--env", "production")
	if code != exitErr {
		t.Fatalf("exit = %d, want %d\n%s", code, exitErr, stdout)
	}
	if !strings.Contains(msg, "--yes") {
		t.Errorf("the refusal does not name the flag that would allow it: %q", msg)
	}
	if len(engine.executed) != 0 {
		t.Fatalf("it deleted anyway: %d executions", len(engine.executed))
	}
	if !strings.Contains(stdout, "Deployment/checkout-production/web") {
		t.Errorf("the preview was not printed before the refusal\n%s", stdout)
	}
}

// TestUninstallReportsPerObjectResults is step three, including the bystanders
// it deliberately left.
func TestUninstallReportsPerObjectResults(t *testing.T) {
	plan := fullPlan()
	engine := &fakeUninstaller{
		plan: plan,
		report: &uninstall.Report{
			Results: []uninstall.Result{
				{Ref: uninstall.Ref{Kind: "Deployment", Name: "web", Namespace: "checkout-production"}, Outcome: uninstall.OutcomeDeleted},
				{Ref: uninstall.Ref{Kind: "ConfigMap", Name: "bystander", Namespace: "checkout-production"},
					Outcome: uninstall.OutcomeLeft, Detail: "it no longer carries kelson's labels"},
				{Ref: uninstall.Ref{Kind: "Service", Name: "web", Namespace: "checkout-production"}, Outcome: uninstall.OutcomeGone},
			},
			Deleted: 1, Left: 1, Gone: 1,
		},
	}
	stdout, code, msg := runUninstallCmd(t, engine, &fakeHistory{}, "",
		"uninstall", "--project", "checkout", "--env", "production", "--yes")
	if code != exitOK {
		t.Fatalf("exit = %d (%s)\n%s", code, msg, stdout)
	}
	for _, want := range []string{
		"deleted Deployment/checkout-production/web",
		"left    ConfigMap/checkout-production/bystander",
		"gone    Service/checkout-production/web",
		"1 deleted, 1 already gone, 1 left untouched, 0 failed",
		"kelson left 1 resource(s) alone",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the report does not contain %q\n%s", want, stdout)
		}
	}
}

// TestUninstallNothingToDo: the second run, and a project that was never
// deployed, are the same answer. Exit 0 and no confirmation prompt — there is
// nothing to confirm.
func TestUninstallNothingToDo(t *testing.T) {
	engine := &fakeUninstaller{plan: &uninstall.Plan{}}
	stdout, code, msg := runUninstallCmd(t, engine, &fakeHistory{}, "",
		"uninstall", "--project", "checkout", "--env", "production")
	if code != exitOK {
		t.Fatalf("exit = %d (%s)\n%s", code, msg, stdout)
	}
	if !strings.Contains(stdout, "nothing to do") {
		t.Errorf("an empty uninstall does not say so\n%s", stdout)
	}
	if len(engine.executed) != 0 {
		t.Errorf("it executed an empty plan")
	}
}

// TestUninstallConfirmation covers the three answers to step two. A terminal is
// not something a test has, so interactivity is passed in — "is there a human"
// and "what did the human say" are different questions and only the second is
// testable here.
func TestUninstallConfirmation(t *testing.T) {
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

			ok, err := confirmUninstall(cmd, &uninstallOptions{yes: tc.yes}, fullPlan(), out, tc.asks)
			if tc.errors {
				if err == nil {
					t.Fatalf("a non-terminal run with no --yes was allowed to proceed")
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

// TestUninstallPromptNamesTheDataCount: the question a human answers has to
// carry the irreversible part, not just a resource count.
func TestUninstallPromptNamesTheDataCount(t *testing.T) {
	var buf bytes.Buffer
	out := &printer{w: &buf}
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)
	cmd.SetIn(strings.NewReader("n\n"))

	if _, err := confirmUninstall(cmd, &uninstallOptions{}, fullPlan(), out, true); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if !strings.Contains(buf.String(), "hold data") {
		t.Errorf("the prompt does not mention the data resources:\n%s", buf.String())
	}
}

// TestUninstallForgetsLocalHistory: an environment whose resources are gone must
// not leave a journal describing them behind. --keep-history is the deliberate
// exception.
func TestUninstallForgetsLocalHistory(t *testing.T) {
	history := &fakeHistory{records: []direct.Record{{Revision: "rev-00000001"}, {Revision: "rev-00000002"}}}
	engine := &fakeUninstaller{plan: fullPlan()}
	stdout, code, msg := runUninstallCmd(t, engine, history, "",
		"uninstall", "--project", "checkout", "--env", "production", "--yes")
	if code != exitOK {
		t.Fatalf("exit = %d (%s)\n%s", code, msg, stdout)
	}
	if strings.Join(history.forgot, ",") != "checkout/production" {
		t.Fatalf("history forgotten = %v, want the uninstalled environment", history.forgot)
	}
	if !strings.Contains(stdout, "2 recorded revision(s)") {
		t.Errorf("the preview does not say the history goes too\n%s", stdout)
	}

	kept := &fakeHistory{records: history.records}
	stdout, code, _ = runUninstallCmd(t, &fakeUninstaller{plan: fullPlan()}, kept, "",
		"uninstall", "--project", "checkout", "--env", "production", "--yes", "--keep-history")
	if code != exitOK {
		t.Fatalf("exit = %d\n%s", code, stdout)
	}
	if len(kept.forgot) != 0 {
		t.Errorf("--keep-history removed the history anyway: %v", kept.forgot)
	}
	if !strings.Contains(stdout, "kept (--keep-history)") {
		t.Errorf("--keep-history is not reported\n%s", stdout)
	}
}

// TestUninstallAllEnvironments passes the scope through and forgets the whole
// project's history.
func TestUninstallAllEnvironments(t *testing.T) {
	history := &fakeHistory{envs: []string{"production", "staging"}}
	engine := &fakeUninstaller{plan: fullPlan()}
	stdout, code, msg := runUninstallCmd(t, engine, history, "",
		"uninstall", "--project", "checkout", "--all-environments", "--yes")
	if code != exitOK {
		t.Fatalf("exit = %d (%s)\n%s", code, msg, stdout)
	}
	if len(engine.planned) != 1 || !engine.planned[0].AllEnvironments {
		t.Fatalf("planned scope = %+v, want --all-environments", engine.planned)
	}
	if strings.Join(history.forgot, ",") != "checkout/*" {
		t.Fatalf("history forgotten = %v, want the whole project", history.forgot)
	}
	if !strings.Contains(stdout, "production, staging") {
		t.Errorf("the preview does not name the environments whose history goes\n%s", stdout)
	}
}

// TestUninstallRejectsContradictoryScope: the addressing rules are enforced
// before anything reaches the cluster, and they come back as structured
// delivery errors.
func TestUninstallRejectsContradictoryScope(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no environment", []string{"uninstall", "--project", "checkout"}, "addressed by environment"},
		{"both", []string{"uninstall", "--project", "checkout", "--env", "production", "--all-environments"}, "disagree"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			engine := &fakeUninstaller{plan: fullPlan()}
			_, code, msg := runUninstallCmd(t, engine, &fakeHistory{}, "", tc.args...)
			if code != exitErr {
				t.Fatalf("exit = %d, want %d (%s)", code, exitErr, msg)
			}
			if !strings.Contains(msg, tc.want) {
				t.Errorf("error %q does not mention %q", msg, tc.want)
			}
			if len(engine.planned) != 0 {
				t.Errorf("it reached the cluster with an invalid scope")
			}
		})
	}
}

// TestUninstallSurfacesExecutionFailures: a delete the API server refused is a
// non-zero exit, and the per-object report is still printed — the operator has
// to know what did go before deciding what to do about what did not.
func TestUninstallSurfacesExecutionFailures(t *testing.T) {
	engine := &fakeUninstaller{
		plan:    fullPlan(),
		execErr: delivery.ApplyFailed("HTTPRoute/web", "", "forbidden", "check RBAC"),
		report: &uninstall.Report{
			Results: []uninstall.Result{
				{Ref: uninstall.Ref{Kind: "HTTPRoute", Name: "web"}, Outcome: uninstall.OutcomeFailed, Detail: "forbidden"},
			},
			Failed: 1,
		},
	}
	stdout, code, msg := runUninstallCmd(t, engine, &fakeHistory{}, "",
		"uninstall", "--project", "checkout", "--env", "production", "--yes")
	if code != exitErr {
		t.Fatalf("exit = %d, want %d\n%s", code, exitErr, stdout)
	}
	if !strings.Contains(stdout, "failed  HTTPRoute/web") {
		t.Errorf("the failure is not reported per object\n%s", stdout)
	}
	var de delivery.Error
	if !errors.As(errors.New(msg), &de) && !strings.Contains(msg, "forbidden") {
		t.Errorf("the error does not carry the cause: %q", msg)
	}
}

// TestUninstallKeepDataIsReported: what --keep-data left behind is named, and so
// is the namespace it therefore could not remove. Silence would read as "it was
// not there".
func TestUninstallKeepDataIsReported(t *testing.T) {
	plan := fullPlan()
	plan.KeepData = true
	plan.Kept = []uninstall.Target{target("Cluster", "checkout-production-db", uninstall.TierData)}
	plan.Targets = plan.Targets[:2]
	plan.Namespaces[0].Delete = false
	plan.Namespaces[0].Reason = "--keep-data keeps the data resources in it"

	engine := &fakeUninstaller{plan: plan}
	stdout, code, msg := runUninstallCmd(t, engine, &fakeHistory{}, "",
		"uninstall", "--project", "checkout", "--env", "production", "--keep-data", "--yes")
	if code != exitOK {
		t.Fatalf("exit = %d (%s)\n%s", code, msg, stdout)
	}
	for _, want := range []string{
		"Kept by --keep-data",
		"Cluster/checkout-production/checkout-production-db",
		"namespace checkout-production stays: --keep-data",
		"Still installed",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the output does not contain %q\n%s", want, stdout)
		}
	}
}

// TestUninstallWarnsAboutReconciledResources: deleting what Flux reconciles is
// temporary, and the preview says so before the delete rather than after the
// resources come back.
func TestUninstallWarnsAboutReconciledResources(t *testing.T) {
	plan := fullPlan()
	plan.Targets[1].Reconciled = "Flux Kustomization flux-system/checkout"
	engine := &fakeUninstaller{plan: plan}
	stdout, code, _ := runUninstallCmd(t, engine, &fakeHistory{}, "",
		"uninstall", "--project", "checkout", "--env", "production", "--yes")
	if code != exitOK {
		t.Fatalf("exit = %d\n%s", code, stdout)
	}
	if !strings.Contains(stdout, "will re-create them") {
		t.Errorf("no warning that a reconciler puts these back\n%s", stdout)
	}
}

// TestUninstallNamesANamespaceLeftToAnotherDeployment: a namespace another
// kelson deployment moved into between the preview and the delete is refused by
// the delivery plane (issue #215), and the command has to carry that refusal all
// the way out — as a per-object result AND in the boundary, which is the list an
// operator reads to find out what is still standing.
func TestUninstallNamesANamespaceLeftToAnotherDeployment(t *testing.T) {
	engine := &fakeUninstaller{
		plan: fullPlan(),
		report: &uninstall.Report{
			Results: []uninstall.Result{{
				Ref:     uninstall.Ref{APIVersion: "v1", Kind: "Namespace", Name: "checkout-production"},
				Outcome: uninstall.OutcomeLeft,
				Detail: "another kelson deployment lives in this namespace — left behind: 2 resource(s) of " +
					"grocery/production live here, and deleting a namespace deletes everything inside it",
			}},
			Left: 1,
		},
	}
	stdout, code, msg := runUninstallCmd(t, engine, &fakeHistory{}, "",
		"uninstall", "--project", "checkout", "--env", "production", "--yes")
	if code != exitOK {
		t.Fatalf("exit = %d (%s)\n%s", code, msg, stdout)
	}
	for _, want := range []string{
		"left    Namespace/checkout-production",
		"grocery/production",
		"left 1 resource(s) alone",
		"namespace checkout-production and everything else in it",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the output does not contain %q; a namespace that survived without being named reads as a "+
				"namespace that was deleted\n%s", want, stdout)
		}
	}
}
