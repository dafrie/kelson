package main

import (
	"github.com/spf13/cobra"

	"github.com/dafrie/kelson/internal/explain"
)

// newExplainCmd builds `kelson explain` (issue #77, ADR-0023): "why is this
// component degraded?" answered with structured causes rather than three
// commands and a guess.
//
// # What it is next to `kelson status`
//
// `status` reports the facts and leaves the correlation to the reader.
// `explain` makes the correlation: it takes each workload's verdict, adds the
// container output the probe captured, and answers with causes that carry a
// confidence and the evidence behind them.
//
// # It degrades rather than refusing, which is what a diagnosis must do
//
// [ADR-0028](docs/adr/0028-delivery-spine.md) deleted the delivery adapters and
// the rendered-history store, which used to cost this command two of its four
// inputs: the delivery phase, and the recorded manifests that let a cause name
// the revision that introduced the change it blames. Both were already
// optional — internal/explain has carried a `notes` channel for exactly this
// since ADR-0023, because a tool people reach for when something is already
// broken must not itself break when a second source is unavailable — so the
// missing inputs arrive as notes and the verdict-derived causes, which are the
// ones that fire in a real incident, are unaffected.
//
// Issue #224 closed the first gap for ExplainService (the API and MCP
// surfaces, internal/api/explain.go): `Environment.status.revision` and the
// bounded `status.history` mirror exist now and answer "what is deployed, and
// when did it last change". This command still does not read either — not
// because #224 is unresolved, but because it composes internal/explain locally
// against the cluster and no client here reaches a stored `Environment` (see
// "Why it takes -f and not a project name" below) — so its notes say that,
// rather than blaming an issue that has since landed elsewhere. A rendered
// manifest diff (the revision that introduced a specific env-var change) is
// still unbuilt anywhere: the registry holds the bytes and nothing fetches them
// yet, one `flux pull artifact` away.
//
// # Why it takes -f and not a project name
//
// Every verb of this CLI is spec-file shaped, and cmd/kelson is denied the
// ConnectRPC libraries by the same depguard rule that keeps the renderer pure
// (.golangci.yml): there is no client here that could ask a server for a stored
// project. So this command composes internal/explain locally — against the
// direct adapter, its probe and its rendered-history store — which is the
// identical capability ExplainService serves, so the CLI and the API cannot
// disagree about what a cause is. A server-backed `kelson explain <project>` is
// a change to what this CLI is, and it belongs with the client work rather than
// here (ADR-0023, decision 6).
func newExplainCmd() *cobra.Command { return newExplainCmdFactory(connectObservation) }

func newExplainCmdFactory(connect observationConnector) *cobra.Command {
	opts := &explainOptions{connect: connect}
	cmd := &cobra.Command{
		Use:   "explain -f spec.yaml --env <name>",
		Short: "Explain why an environment is in the state it is in, with causes, confidence and evidence",
		Long: "Explain answers \"why is this degraded?\" with structured causes instead of a log dump.\n\n" +
			"Each cause carries a stable code, a confidence (high when a controller named the reason or two\n" +
			"independent signals agree; medium for a single signal or a heuristic; low for an audit finding),\n" +
			"the evidence behind it — the controller's own words, a bounded log excerpt, a field reference —\n" +
			"and the revision that introduced the change it blames, where the recorded history can show one.\n\n" +
			"The output is bounded on purpose: at most 6 causes, 4 evidence items each and 12 KiB in total,\n" +
			"so it can be pasted whole into a context window. Anything cut is listed under TRUNCATED.",
		Example: "  kelson explain -f project.yaml -f production.yaml --env production\n" +
			"  kelson explain -f spec.yaml --env development --kubeconfig ./kubeconfig",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runExplain(cmd, opts)
		},
	}
	f := cmd.Flags()
	f.StringArrayVarP(&opts.files, "file", "f", nil, "spec YAML file holding Project and/or Environment documents (repeatable)")
	f.StringVar(&opts.env, "env", "", "name of the Environment to explain (optional when the input holds exactly one)")
	f.StringVar(&opts.profile, "profile", "", "ClusterProfile YAML file, or from-cluster to capture a live profile (requires cluster access)")
	f.StringVar(&opts.kubeconfig, "kubeconfig", "", "path to a kubeconfig (default: $KUBECONFIG, in-cluster credentials, then ~/.kube/config)")
	f.StringVar(&opts.image, "image", "", imageFlagUsage)
	cobra.CheckErr(cmd.MarkFlagRequired("file"))
	return cmd
}

type explainOptions struct {
	specInput
	connect observationConnector
}

// runExplain reports and exits 0, for the reason `kelson status` does: this is
// the command you reach for when something is already broken, and a non-zero
// exit would make it unusable in the `set -e` scripts that need it most.
// `kelson deploy` is the command that gates on health.
func runExplain(cmd *cobra.Command, opts *explainOptions) error {
	target, set, err := resolveObservationTarget(opts.specInput, cmd.ErrOrStderr())
	if err != nil {
		return err
	}
	plane, err := connectPlane(opts.connect, target)
	if err != nil {
		return err
	}

	ctx := cmd.Context()
	verdicts, err := workloadVerdicts(ctx, plane, set, target.namespace)
	if err != nil {
		return err
	}

	in := explain.Input{
		Project:     set.Project,
		Environment: set.Environment,
		Namespace:   target.namespace,
		Verdicts:    verdicts,
	}
	// Status and History are left zero, and the notes say so rather than the
	// output implying kelson looked and found nothing. Which sources were
	// consulted is part of the answer (ADR-0023): a diagnosis that quietly
	// omits an input is a diagnosis a reader cannot weigh.
	//
	// Both are Environment.status now (ADR-0028), not a source issue #224 left
	// broken: ExplainService reads them (internal/api/explain.go). This command
	// composes locally against the cluster and has no client for a stored
	// Environment, so it still cannot read them — a `kelson explain` that needs
	// the revision correlation calls the server-backed surface (the MCP
	// diagnose_component tool, or ExplainService directly) instead.
	in.Notes = append(in.Notes,
		"the delivery phase was not read: this command has no client for Environment.status — it composes "+
			"against the cluster directly, not against the server (see ExplainService for the delivery phase "+
			"and revision correlation)",
		"no revision history was available, so no change was correlated, for the same reason: status.history "+
			"is Environment.status, which this command does not read")
	if plane.health == nil {
		in.Notes = append(in.Notes,
			"no observation probe was available, so no workload health was read and no verdict-derived cause could be found")
	}

	out := &printer{w: cmd.OutOrStdout()}
	out.printf("%s\n", explain.Explain(ctx, in).Text())
	return out.err
}
