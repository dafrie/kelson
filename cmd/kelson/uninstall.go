package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dafrie/kelson/internal/delivery/install"
	"github.com/dafrie/kelson/internal/delivery/kube"
	"github.com/dafrie/kelson/internal/delivery/uninstall"
)

// `kelson uninstall` is the deletability claim made operable (issue #59).
//
// # Why it takes --project/--env and not a spec file
//
// Every other delivery command renders a spec and acts on the result. This one
// must not: what to delete is a fact about the CLUSTER, recorded in the
// provenance labels on the live objects, and the spec is neither necessary nor
// sufficient to establish it. A spec that changed since the deploy would under-
// or over-state the set, and someone uninstalling an environment has very often
// already deleted the spec that created it. So the addressing is
// `kelson secret`'s — a (project, environment) pair — and the source of truth is
// the label selector, which is printed so it can be checked with kubectl.
//
// # Three steps, in this order, always
//
// Preview, confirm, delete. The preview is computed from live objects and
// printed whether or not --yes was passed; --yes skips the question, never the
// preview. The data section is loud because it is the only irreversible part,
// and it names the volumes an operator will garbage-collect as well as the
// resources kelson deletes itself.
func newUninstallCmd() *cobra.Command {
	return newUninstallCmdFactory(connectUninstall, connectRemover)
}

// uninstaller is what the command needs from the delivery plane. It is declared
// here rather than imported as a concrete type for the same reason
// deliveryConnector is: the production implementation needs a live cluster, and
// none of the command wiring under test does.
type uninstaller interface {
	Plan(ctx context.Context, scope uninstall.Scope) (*uninstall.Plan, error)
	Execute(ctx context.Context, plan *uninstall.Plan) (*uninstall.Report, error)
}

// uninstallConnector builds the uninstaller for one command run.
type uninstallConnector func(opts uninstallOptions) (uninstaller, error)

// remover is the component half of this verb (issue #60): what removes a
// platform component kelson installed. It is a separate interface from
// uninstaller because the two answer different questions about ownership —
// one reads project provenance labels, the other reads per-object install
// provenance — and collapsing them would let a project uninstall reach the
// operators, which is exactly what must never happen.
type remover interface {
	Plan(ctx context.Context, component string) (*install.Removal, error)
	Execute(ctx context.Context, removal *install.Removal) (*install.RemovalReport, error)
}

// removerConnector builds the remover for one command run.
type removerConnector func(opts uninstallOptions) (remover, error)

func connectRemover(opts uninstallOptions) (remover, error) {
	cluster, err := kube.Connect(opts.kubeconfig)
	if err != nil {
		return nil, err
	}
	return install.NewRemover(install.RemoverOptions{
		Client: cluster.Dynamic,
		// A platform component is mostly cluster-scoped — CRDs, ClusterRoles,
		// webhook configurations — so this sweep is discovery-driven over both
		// scopes, unlike the project sweep above.
		Catalog: install.DiscoveryCatalog{Client: cluster.Typed.Discovery()},
	})
}

// connectUninstall is the production connector: one cluster connection, the
// dynamic client for the sweep and the discovery client for the catalog of
// kinds to sweep.
func connectUninstall(opts uninstallOptions) (uninstaller, error) {
	cluster, err := kube.Connect(opts.kubeconfig)
	if err != nil {
		return nil, err
	}
	return uninstall.New(uninstall.Options{
		Client: cluster.Dynamic,
		// The sweep is discovery-driven: an overlay can contribute any kind, so
		// a fixed list of what the renderer emits today would leave those
		// behind (internal/delivery/uninstall.Catalog).
		Catalog:  uninstall.DiscoveryCatalog{Client: cluster.Typed.Discovery()},
		KeepData: opts.keepData,
	})
}

type uninstallOptions struct {
	project         string
	env             string
	component       string
	allEnvironments bool
	namespace       string
	kubeconfig      string
	keepData        bool
	yes             bool
	connect         uninstallConnector
	connectRemover  removerConnector
}

func newUninstallCmdFactory(connect uninstallConnector, removers removerConnector) *cobra.Command {
	opts := &uninstallOptions{connect: connect, connectRemover: removers}
	cmd := &cobra.Command{
		Use:   "uninstall --project <name> --env <name> | --component <name>",
		Short: "Remove what kelson deployed for an environment, or a platform component kelson installed",
		Long: "Uninstall deletes the resources kelson deployed for one (project, environment) — the set its\n" +
			"provenance labels select, re-checked object by object — and leaves everything else in the\n" +
			"namespace exactly as it is.\n\n" +
			"With --component it removes a platform component instead, and only the parts of it kelson's own\n" +
			"`kelson install` created. A component kelson adopted, or one that was in the cluster before\n" +
			"kelson, is never touched (ADR-0021).\n\n" +
			"It prints what it would delete before it deletes anything, including a data section naming the\n" +
			"databases, caches and volumes whose contents do not come back. Without --yes it asks; with a\n" +
			"non-terminal stdin and no --yes it refuses rather than assuming an answer.\n\n" +
			"What a project uninstall does NOT remove, on purpose:\n" +
			"  the kelson server         `helm uninstall kelson` owns that install (docs/install.md)\n" +
			"  CRDs                      kelson's own Project and Environment CRDs are removed by the install\n" +
			"                            that created them; the CRDs a component brought belong to\n" +
			"                            `kelson uninstall --component`\n" +
			"  operators                 CloudNativePG, Valkey, Flux and cert-manager are never removed by a\n" +
			"                            project uninstall — other tenants depend on them\n" +
			"  adopted namespaces        a namespace kelson did not create stays, because deleting one\n" +
			"                            deletes everything inside it\n" +
			"  shared namespaces         one kelson did create stays too while another project or\n" +
			"                            environment still has resources in it; the preview says whose\n" +
			"  anything unlabelled       Secrets kelson did not write, and every resource that does not\n" +
			"                            carry kelson's provenance labels for this environment",
		Example: "  kelson uninstall --project checkout --env production\n" +
			"  kelson uninstall --project checkout --env production --keep-data --yes\n" +
			"  kelson uninstall --project checkout --all-environments --yes\n" +
			"  kelson uninstall --component cert-manager",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return runUninstall(cmd, opts) },
	}
	f := cmd.Flags()
	f.StringVar(&opts.project, "project", "", "name of the Project to uninstall")
	f.StringVar(&opts.component, "component", "",
		"remove a platform component kelson installed ("+strings.Join(install.Names(), ", ")+"), instead of a project environment")
	f.StringVar(&opts.env, "env", "", "name of the Environment to uninstall")
	f.BoolVar(&opts.allEnvironments, "all-environments", false,
		"uninstall every environment of the project, not one")
	f.StringVar(&opts.namespace, "namespace", "",
		"namespace to sweep when kelson may not list namespaces by label (only needed for a restricted kube context, or an Environment that sets spec.namespace)")
	f.StringVar(&opts.kubeconfig, "kubeconfig", "",
		"path to a kubeconfig (default: $KUBECONFIG, in-cluster credentials, then ~/.kube/config)")
	f.BoolVar(&opts.keepData, "keep-data", false,
		"leave the data services and their volumes in place; the namespace then stays too, because deleting it would delete them")
	f.BoolVar(&opts.yes, "yes", false, "delete without asking for confirmation; the preview is printed either way")
	// --project is no longer a cobra-required flag: --component addresses a
	// different thing entirely and needs none of the project addressing. The
	// "exactly one of them" rule is enforced in runUninstall, where it can say
	// which flags disagree instead of naming one of them.
	return cmd
}

func runUninstall(cmd *cobra.Command, opts *uninstallOptions) error {
	if opts.component != "" {
		return runComponentUninstall(cmd, opts)
	}
	if opts.project == "" {
		return fmt.Errorf("an uninstall is addressed by project or by component: " +
			"pass --project <name> --env <name>, or --component <name>")
	}
	scope := uninstall.Scope{
		Project:         opts.project,
		Environment:     opts.env,
		AllEnvironments: opts.allEnvironments,
		Namespace:       opts.namespace,
	}
	if err := scope.Validate(); err != nil {
		return err
	}
	if opts.connect == nil {
		return fmt.Errorf("the delivery plane is unavailable in this build")
	}
	engine, err := opts.connect(*opts)
	if err != nil {
		return err
	}

	ctx := cmd.Context()
	plan, err := engine.Plan(ctx, scope)
	if err != nil {
		return err
	}

	out := &printer{w: cmd.OutOrStdout()}
	printUninstallPlan(out, plan)
	if err := out.err; err != nil {
		return err
	}

	if plan.Empty() {
		return nil
	}
	ok, err := confirmUninstall(cmd, opts, plan, out, interactive(cmd))
	if err != nil || !ok {
		return err
	}

	report, execErr := engine.Execute(ctx, plan)
	printUninstallReport(out, report)
	if execErr != nil {
		return execErr
	}
	printUninstallBoundary(out, plan, report)
	return out.err
}

// confirmUninstall is the second of the three steps. A non-terminal stdin with
// no --yes is a refusal, not an assumed yes: an unattended run must never be
// able to answer this question by accident, and an unattended run that quietly
// did nothing would be just as bad, so it says so and exits non-zero.
//
// Interactivity is a parameter rather than read from cmd here, because "is
// there a human" and "what does the human say" are two different questions and
// only the second one is testable without a terminal.
func confirmUninstall(cmd *cobra.Command, opts *uninstallOptions, plan *uninstall.Plan, out *printer, asks bool) (bool, error) {
	if opts.yes {
		return true, nil
	}
	if !asks {
		return false, fmt.Errorf("refusing to delete %d resources without --yes: stdin is not a terminal, so there is nobody to ask",
			len(plan.Targets))
	}
	question := fmt.Sprintf("Delete these %d resources from %s?", len(plan.Targets), plan.Scope)
	if data := plan.Tier(uninstall.TierData); len(data) > 0 {
		question = fmt.Sprintf("Delete these %d resources from %s, including %d that hold data?",
			len(plan.Targets), plan.Scope, len(data))
	}
	ok, err := confirm(cmd, question)
	if err != nil {
		return false, err
	}
	if !ok {
		out.printf("aborted: nothing was deleted.\n")
		return false, out.err
	}
	return true, nil
}

// --- the component path -------------------------------------------------------

// runComponentUninstall removes a platform component, and only the parts of it
// kelson created (issue #60).
//
// It shares this verb rather than getting one of its own because it is the same
// promise at a different layer: kelson removes what kelson put there and
// nothing else. What it does NOT share is the project addressing — a component
// has no environment and no namespace to sweep — so the flags
// that describe those are refused rather than quietly ignored.
func runComponentUninstall(cmd *cobra.Command, opts *uninstallOptions) error {
	if err := opts.validateComponentScope(); err != nil {
		return err
	}
	if opts.connectRemover == nil {
		return fmt.Errorf("the delivery plane is unavailable in this build")
	}
	engine, err := opts.connectRemover(*opts)
	if err != nil {
		return err
	}

	ctx := cmd.Context()
	removal, err := engine.Plan(ctx, opts.component)
	if err != nil {
		return err
	}

	out := &printer{w: cmd.OutOrStdout()}
	printRemovalPlan(out, removal)
	if err := out.err; err != nil {
		return err
	}
	if removal.Empty() {
		return nil
	}

	ok, err := confirmRemoval(cmd, opts, removal, out, interactive(cmd))
	if err != nil || !ok {
		return err
	}

	report, execErr := engine.Execute(ctx, removal)
	printRemovalReport(out, report)
	if execErr != nil {
		return execErr
	}
	printRemovalBoundary(out, removal)
	return out.err
}

// validateComponentScope refuses the project flags rather than ignoring them.
// A user who typed --component and --keep-data together has a belief about what
// is about to happen, and silently dropping one of them leaves that belief
// intact and wrong.
func (o *uninstallOptions) validateComponentScope() error {
	var conflicting []string
	if o.project != "" {
		conflicting = append(conflicting, "--project")
	}
	if o.env != "" {
		conflicting = append(conflicting, "--env")
	}
	if o.allEnvironments {
		conflicting = append(conflicting, "--all-environments")
	}
	if o.namespace != "" {
		conflicting = append(conflicting, "--namespace")
	}
	if o.keepData {
		conflicting = append(conflicting, "--keep-data")
	}
	if len(conflicting) == 0 {
		return nil
	}
	return fmt.Errorf("--component removes a platform component, which has no project and no "+
		"environment: %s does not apply to it", strings.Join(conflicting, ", "))
}

func confirmRemoval(cmd *cobra.Command, opts *uninstallOptions, removal *install.Removal, out *printer, asks bool) (bool, error) {
	if opts.yes {
		return true, nil
	}
	if !asks {
		return false, fmt.Errorf("refusing to delete %d resources without --yes: stdin is not a terminal, so there is nobody to ask",
			len(removal.Targets))
	}
	question := fmt.Sprintf("Delete these %d resources of %s?", len(removal.Targets), removal.Component.Name)
	if collateral := removalCollateral(removal); collateral > 0 {
		question = fmt.Sprintf("Delete these %d resources of %s, taking %d custom resource(s) with them?",
			len(removal.Targets), removal.Component.Name, collateral)
	}
	ok, err := confirm(cmd, question)
	if err != nil {
		return false, err
	}
	if !ok {
		out.printf("aborted: nothing was deleted.\n")
		return false, out.err
	}
	return true, nil
}

func removalCollateral(removal *install.Removal) int {
	var n int
	for _, t := range removal.Targets {
		n += len(t.Collateral)
	}
	return n
}

func printRemovalPlan(out *printer, removal *install.Removal) {
	out.printf("kelson uninstall — component %s, selector %s\n",
		removal.Component.Name, install.Selector(removal.Component.Name))

	if removal.Empty() {
		out.printf("\nnothing to do: kelson did not install %s in this cluster.\n", removal.Component.Name)
		printKeptComponents(out, removal)
		printUnreadableComponents(out, removal)
		return
	}

	for _, tier := range install.Tiers {
		targets := removal.Tier(tier)
		if len(targets) == 0 {
			continue
		}
		out.printf("\n%s (%d)\n", tier, len(targets))
		for _, t := range targets {
			out.printf("  %s\n", t.Ref)
			// A CRD deletion takes every custom resource of that kind in the
			// cluster with it, including resources kelson never created. That is
			// the irreversible part of removing an operator, and it is named
			// object by object rather than counted.
			for _, c := range t.Collateral {
				out.printf("      goes with it: %s (the API server deletes it with the CRD; kelson does not touch it directly)\n", c)
			}
		}
	}

	printKeptComponents(out, removal)
	printUnreadableComponents(out, removal)
}

func printKeptComponents(out *printer, removal *install.Removal) {
	if len(removal.Kept) == 0 {
		return
	}
	out.printf("\nLeft alone (%d) — labelled for %s, but not kelson's to delete\n",
		len(removal.Kept), removal.Component.Name)
	for _, k := range removal.Kept {
		out.printf("  %s\n      %s\n", k.Ref, k.Reason)
	}
}

func printUnreadableComponents(out *printer, removal *install.Removal) {
	if len(removal.Unreadable) == 0 {
		return
	}
	out.printf("\nThe sweep was incomplete:\n")
	for _, u := range removal.Unreadable {
		out.printf("  %s\n", u)
	}
}

func printRemovalReport(out *printer, report *install.RemovalReport) {
	if report == nil {
		return
	}
	out.printf("\n")
	for _, res := range report.Results {
		if res.Detail == "" {
			out.printf("  %-7s %s\n", res.Outcome, res.Ref)
			continue
		}
		out.printf("  %-7s %s — %s\n", res.Outcome, res.Ref, res.Detail)
	}
	out.printf("\n%d deleted, %d already gone, %d left untouched, %d failed\n",
		report.Deleted, report.Gone, report.Left, report.Failed)
}

func printRemovalBoundary(out *printer, removal *install.Removal) {
	out.printf("\nStill installed, and not this command's to remove:\n")
	out.printf("  everything kelson adopted rather than created when it installed %s\n", removal.Component.Name)
	out.printf("  every component kelson deployed against it — `kelson uninstall --project <p> --env <e>`\n")
	out.printf("  every other platform component; each one is removed by name\n")
}

// --- the preview -------------------------------------------------------------

// printUninstallPlan is the first of the three steps: exactly what would be
// deleted, kind and name, one line each.
func printUninstallPlan(out *printer, plan *uninstall.Plan) {
	out.printf("kelson uninstall — %s\n", plan.Describe())

	for _, ns := range plan.Namespaces {
		switch {
		case ns.Delete:
			out.printf("  namespace %s: kelson created it, so it goes last\n", ns.Name)
		default:
			out.printf("  namespace %s stays: %s\n", ns.Name, ns.Reason)
		}
	}
	if len(plan.Namespaces) == 0 {
		out.printf("  no namespace carries kelson's labels for %s\n", plan.Scope)
	}

	if plan.Empty() {
		out.printf("\nnothing to do: kelson has nothing labelled for %s in this cluster.\n", plan.Scope)
		printUnreadable(out, plan)
		return
	}

	for _, tier := range uninstall.Tiers {
		targets := plan.Tier(tier)
		if len(targets) == 0 {
			continue
		}
		if tier == uninstall.TierData {
			printDataTier(out, targets)
			continue
		}
		out.printf("\n%s (%d)\n", tier, len(targets))
		for _, t := range targets {
			out.printf("  %s%s\n", t.Ref, uninstallAnnotation(t))
		}
	}

	if len(plan.Kept) > 0 {
		out.printf("\nKept by --keep-data (%d)\n", len(plan.Kept))
		for _, t := range plan.Kept {
			out.printf("  %s\n", t.Ref)
		}
	}

	printReconcilers(out, plan)
	printUnreadable(out, plan)
}

// printDataTier is the loud section. Deleting a workload costs a redeploy;
// deleting a database costs the database, and the preview says so in the same
// voice the rollback preview uses for what a rollback cannot revert.
func printDataTier(out *printer, targets []uninstall.Target) {
	out.printf("\nDATA (%d) — this is the part nothing can undo\n", len(targets))
	for _, t := range targets {
		out.printf("  %s\n", t.Ref)
		if t.Note != "" {
			out.printf("      %s\n", t.Note)
		}
		for _, c := range t.Collateral {
			out.printf("      goes with it: %s (its operator deletes it; kelson does not touch it directly)\n", c)
		}
	}
}

// uninstallAnnotation is the trailing note on a non-data target: what makes
// this one different from its neighbours on the same line.
func uninstallAnnotation(t uninstall.Target) string {
	var notes []string
	if t.ManagedSecret {
		notes = append(notes, "a Secret kelson wrote; kelson keeps no copy of the value (ADR-0009)")
	}
	if t.Reconciled != "" {
		notes = append(notes, "reconciled by "+t.Reconciled)
	}
	if len(notes) == 0 {
		return ""
	}
	return "   — " + strings.Join(notes, "; ")
}

// printReconcilers warns when something else will simply put back what this
// command deletes.
func printReconcilers(out *printer, plan *uninstall.Plan) {
	names := plan.Reconcilers()
	if len(names) == 0 {
		return
	}
	out.printf("\nWarning: %s reconcile some of these resources and will re-create them.\n",
		strings.Join(names, ", "))
	out.printf("  Remove the environment from the deployment repository first, or this uninstall is temporary.\n")
}

// printUnreadable states where the sweep could not look. An incomplete sweep
// reported as a complete one is a lie about what is left in the cluster.
func printUnreadable(out *printer, plan *uninstall.Plan) {
	if len(plan.Unreadable) == 0 {
		return
	}
	out.printf("\nThe sweep was incomplete:\n")
	for _, u := range plan.Unreadable {
		out.printf("  %s\n", u)
	}
}

// --- the result --------------------------------------------------------------

func printUninstallReport(out *printer, report *uninstall.Report) {
	if report == nil {
		return
	}
	out.printf("\n")
	for _, res := range report.Results {
		if res.Detail == "" {
			out.printf("  %-7s %s\n", res.Outcome, res.Ref)
			continue
		}
		out.printf("  %-7s %s — %s\n", res.Outcome, res.Ref, res.Detail)
	}
	out.printf("\n%d deleted, %d already gone, %d left untouched, %d failed\n",
		report.Deleted, report.Gone, report.Left, report.Failed)
	if bystanders := report.Bystanders(); len(bystanders) > 0 {
		out.printf("kelson left %d resource(s) alone because they are not its to delete.\n", len(bystanders))
	}
}

// printUninstallBoundary restates what is still installed, because "uninstall
// finished" is exactly the moment somebody assumes everything kelson-shaped is
// gone.
//
// It reads the report as well as the plan, because a namespace the plan meant
// to delete can still be standing: another deployment moving in between the
// preview and the delete refuses it (issue #215), and a survivor named in the
// per-object results but missing from this list would read as an oversight.
func printUninstallBoundary(out *printer, plan *uninstall.Plan, report *uninstall.Report) {
	out.printf("\nStill installed, and not this command's to remove:\n")
	out.printf("  the kelson server, if you run one — `helm uninstall kelson` (docs/install.md)\n")
	out.printf("  the operators kelson delegates to (CloudNativePG, Valkey, Flux, cert-manager); one kelson\n")
	out.printf("  installed itself goes with `kelson uninstall --component <name>`\n")
	for _, ns := range plan.Namespaces {
		if !ns.Delete {
			out.printf("  namespace %s and everything else in it\n", ns.Name)
		}
	}
	if report == nil {
		return
	}
	for _, res := range report.Bystanders() {
		if res.Ref.Kind == "Namespace" {
			out.printf("  namespace %s and everything else in it — %s\n", res.Ref.Name, res.Detail)
		}
	}
}
