package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dafrie/kelson/internal/clusterprofile"
	"github.com/dafrie/kelson/internal/clusterprofile/detect"
	"github.com/dafrie/kelson/internal/delivery/install"
	"github.com/dafrie/kelson/internal/delivery/kube"
)

// `kelson install` offers what detection found missing (issue #60).
//
// # Why it takes a component and not a spec
//
// Everything else in this CLI acts on a component. This acts on the
// cluster's platform layer — the operators kelson delegates to (ADR-0005) —
// which no spec describes and which is shared by every project in the cluster.
// So the addressing is a component name from one table (internal/delivery/
// install.Components) and the answer to "should this run at all" comes from
// detection, not from the user's intent.
//
// # Detection first, always
//
// A component detection already finds is REFUSED, not skipped and not upgraded.
// "Never modify a component kelson did not install" starts with never
// double-installing one, and it holds just as firmly for a component detection
// could not look for: a profile gap makes presence Unknown, and installing on
// an unknown is the guess the tri-state exists to prevent (issue #144).
//
// # Three steps, in this order, always
//
// Preview, confirm, apply — the shape `kelson uninstall` established. The
// preview names every object, the namespace it lands in, the pinned version and
// the digest that was verified, and it prints whether or not --yes was passed.
// A non-terminal stdin with no --yes is a refusal rather than an assumed yes.
func newInstallCmd() *cobra.Command {
	return newInstallCmdFactory(connectInstall, detectProfile)
}

// installer is what the command needs from the delivery plane.
type installer interface {
	Plan(ctx context.Context, req install.Request) (*install.Plan, error)
	Execute(ctx context.Context, plan *install.Plan) (*install.Report, error)
}

// installConnector builds the installer for one command run.
type installConnector func(opts installOptions) (installer, error)

// profileDetector captures what the cluster already has. It is a seam so the
// command tests drive detection results without a cluster.
type profileDetector func(kubeconfig string) (clusterprofile.ClusterProfile, error)

func detectProfile(kubeconfig string) (clusterprofile.ClusterProfile, error) {
	return detect.FromCluster(kubeconfig)
}

func connectInstall(opts installOptions) (installer, error) {
	cluster, err := kube.Connect(opts.kubeconfig)
	if err != nil {
		return nil, err
	}
	return install.New(install.Options{
		Client: cluster.Dynamic,
		Mapper: cluster.Mapper,
		Fetch:  install.HTTPFetcher{},
	})
}

type installOptions struct {
	components []string
	allMissing bool
	kubeconfig string
	dryRun     bool
	yes        bool
	connect    installConnector
	detect     profileDetector
}

func newInstallCmdFactory(connect installConnector, detector profileDetector) *cobra.Command {
	opts := &installOptions{connect: connect, detect: detector}
	cmd := &cobra.Command{
		Use:   "install [component...]",
		Short: "Install a platform component this cluster is missing, and nothing it already has",
		Long: "Install adds a platform component kelson delegates to — an operator, a controller, a CRD set —\n" +
			"when detection reports it absent. It refuses to touch one that is already there.\n\n" +
			"Each component is installed from the install manifest its own project publishes, at a version\n" +
			"pinned in kelson and verified against a recorded SHA-256 digest before anything is applied.\n" +
			"Nothing is vendored into kelson and no chart is re-packaged, so an upstream packaging change\n" +
			"cannot break an installation kelson performed (ADR-0005, ADR-0021).\n\n" +
			"It prints what it would apply before it applies anything: every object, the namespace it lands\n" +
			"in, the pinned version, and the digest that was checked. Without --yes it asks; with a\n" +
			"non-terminal stdin and no --yes it refuses rather than assuming an answer.\n\n" +
			"What it does NOT do, on purpose:\n" +
			"  upgrade                   a component kelson did not install is never modified, and a component\n" +
			"                            kelson DID install is upgraded by upgrading kelson's pin, not here\n" +
			"  configure                 no ClusterIssuer, no SecretStore, no Flux sync source — which CA to\n" +
			"                            trust and which repository to reconcile are your decisions\n" +
			"  work offline              it fetches the pinned manifest at install time and refuses loudly,\n" +
			"                            naming the URL, when it cannot",
		Example: "  kelson install cert-manager\n" +
			"  kelson install flux cnpg --yes\n" +
			"  kelson install --all-missing --dry-run",
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.components = args
			return runInstall(cmd, opts)
		},
	}
	f := cmd.Flags()
	f.BoolVar(&opts.allMissing, "all-missing", false,
		"install every component kelson can install that detection reports absent")
	f.StringVar(&opts.kubeconfig, "kubeconfig", "",
		"path to a kubeconfig (default: $KUBECONFIG, in-cluster credentials, then ~/.kube/config)")
	f.BoolVar(&opts.dryRun, "dry-run", false,
		"print the preview and stop; the manifests are still fetched and their digests verified")
	f.BoolVar(&opts.yes, "yes", false, "apply without asking for confirmation; the preview is printed either way")
	return cmd
}

func runInstall(cmd *cobra.Command, opts *installOptions) error {
	req := install.Request{Components: opts.components, AllMissing: opts.allMissing}
	if err := req.Validate(); err != nil {
		return err
	}
	if opts.connect == nil || opts.detect == nil {
		return fmt.Errorf("the delivery plane is unavailable in this build")
	}

	out := &printer{w: cmd.OutOrStdout()}
	profile, err := opts.detect(opts.kubeconfig)
	if err != nil {
		return err
	}
	req.Profile = profile
	printProfileGaps(cmd, profile)

	engine, err := opts.connect(*opts)
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	plan, err := engine.Plan(ctx, req)
	if err != nil {
		return err
	}

	printInstallPlan(out, plan)
	if err := out.err; err != nil {
		return err
	}
	if plan.Empty() {
		return nil
	}
	if opts.dryRun {
		out.printf("\n--dry-run: nothing was applied.\n")
		return out.err
	}

	ok, err := confirmInstall(cmd, opts, plan, out, interactive(cmd))
	if err != nil || !ok {
		return err
	}

	report, execErr := engine.Execute(ctx, plan)
	printInstallReport(out, report)
	if execErr != nil {
		return execErr
	}
	printInstallBoundary(out, plan)
	return out.err
}

// confirmInstall is the second of the three steps, and it refuses an unattended
// run for the reason uninstall does: an install writes cluster-scoped RBAC,
// CRDs and webhook configurations, and nothing that wide should be able to
// happen because a pipe answered a question by accident.
func confirmInstall(cmd *cobra.Command, opts *installOptions, plan *install.Plan, out *printer, asks bool) (bool, error) {
	if opts.yes {
		return true, nil
	}
	if !asks {
		return false, fmt.Errorf("refusing to install %d resources without --yes: stdin is not a terminal, so there is nobody to ask",
			plan.ObjectCount())
	}
	question := fmt.Sprintf("Apply these %d resources for %s?", plan.ObjectCount(), strings.Join(plannedNames(plan), ", "))
	ok, err := confirm(cmd, question)
	if err != nil {
		return false, err
	}
	if !ok {
		out.printf("aborted: nothing was applied.\n")
		return false, out.err
	}
	return true, nil
}

func plannedNames(plan *install.Plan) []string {
	out := make([]string, 0, len(plan.Items))
	for _, item := range plan.Items {
		out = append(out, item.Component.Name)
	}
	return out
}

// --- the preview -------------------------------------------------------------

// printProfileGaps repeats detection's own warning on stderr. An install
// decides eligibility from the profile, so a profile that could not be read
// completely changes what this command will and will not do, and that must not
// be visible only in a refusal further down.
func printProfileGaps(cmd *cobra.Command, profile clusterprofile.ClusterProfile) {
	if len(profile.Incomplete) == 0 {
		return
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
		"warning: detection is incomplete — %d gap(s), and kelson will not install over a component it could not look for:\n",
		len(profile.Incomplete))
	for _, g := range profile.Incomplete {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "  - %s: %s\n", g.Field, g.Reason)
	}
}

func printInstallPlan(out *printer, plan *install.Plan) {
	out.printf("kelson install\n")

	for _, r := range plan.Refusals {
		out.printf("\n%s: not installed\n", r.Name)
		out.printf("  %s\n", r.Reason)
		out.printf("  %s\n", r.Remediation)
	}

	if plan.Empty() {
		if len(plan.Refusals) == 0 {
			out.printf("\nnothing to do: every component kelson can install is already in this cluster.\n")
		}
		return
	}

	for _, item := range plan.Items {
		c := item.Component
		out.printf("\n%s %s → namespace %s\n", c.Title, c.Version, c.Namespace)
		out.printf("  from   %s\n", c.ManifestURL)
		out.printf("  sha256 %s (verified)\n", item.Digest)
		out.printf("  gives  %s\n", c.Provides)
		out.printf("  %d resources:\n", len(item.Objects))
		for _, o := range item.Objects {
			out.printf("    %s%s\n", o.Ref, installAnnotation(o))
		}
	}
	out.printf("\nkelson stamps %s=<name> on every resource above, and records per object whether this apply\n",
		install.LabelComponent)
	out.printf("created it. `kelson uninstall --component <name>` removes only what it created.\n")
}

// installAnnotation is the trailing note on one object: what makes this one
// different from its neighbours.
func installAnnotation(o install.Object) string {
	var notes []string
	if o.Exists {
		notes = append(notes, "already exists — kelson will apply over it and never delete it")
	}
	if o.Authored {
		notes = append(notes, "written by kelson, not by the upstream manifest")
	}
	if len(notes) == 0 {
		return ""
	}
	return "   — " + strings.Join(notes, "; ")
}

// --- the result --------------------------------------------------------------

func printInstallReport(out *printer, report *install.Report) {
	if report == nil {
		return
	}
	for _, c := range report.Components {
		out.printf("\n%s\n", c.Component.Name)
		for _, res := range c.Results {
			if res.Detail == "" {
				out.printf("  %-7s %s\n", res.Outcome, res.Ref)
				continue
			}
			out.printf("  %-7s %s — %s\n", res.Outcome, res.Ref, res.Detail)
		}
		out.printf("  %d created, %d adopted, %d failed\n", c.Created, c.Adopted, c.Failed)
	}
	if adopted := report.Adopted(); len(adopted) > 0 {
		out.printf("\nkelson adopted %d resource(s) that were already in the cluster. They are NOT kelson's to\n",
			len(adopted))
		out.printf("delete, and `kelson uninstall --component` will leave them standing.\n")
	}
}

// printInstallBoundary states what installing did not do, because "installed"
// is exactly the moment somebody assumes the component is configured.
func printInstallBoundary(out *printer, plan *install.Plan) {
	out.printf("\nInstalled, and not yet configured — that part is yours:\n")
	for _, item := range plan.Items {
		switch item.Component.Name {
		case "cert-manager":
			out.printf("  cert-manager has no ClusterIssuer. kelson renders a Certificate only when detection\n")
			out.printf("  reports one, because which ACME account or CA to trust is your decision.\n")
		case "flux":
			out.printf("  the FluxInstance sets no sync source. Flux is running and reconciling nothing until\n")
			out.printf("  `kelson deploy --mode flux` writes to a repository you point it at.\n")
		case "cnpg":
			out.printf("  CloudNativePG is running with no databases. `kind: postgres` components render against\n")
			out.printf("  it from the next deploy; run `kelson profile` to see the presets it can serve.\n")
		case "envoy-gateway":
			out.printf("  Envoy Gateway has no GatewayClass and carries no traffic. Create a GatewayClass naming\n")
			out.printf("  controller gateway.envoyproxy.io/gatewayclass-controller, and a Gateway with your\n")
			out.printf("  listeners — which class carries traffic is your decision. Routes render once detection\n")
			out.printf("  reports the class.\n")
		}
	}
	out.printf("\nRun `kelson profile` to see the cluster as kelson now reads it.\n")
}
