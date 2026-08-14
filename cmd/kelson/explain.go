package main

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/explain"
)

// newExplainCmd builds `kelson explain` (issue #77, ADR-0023): "why is this
// component degraded?" answered with structured causes rather than three
// commands and a guess.
//
// # What it is next to `kelson status`
//
// `status` reports two facts side by side — the delivery phase and each
// workload's verdict — and leaves the correlation to the reader. `explain`
// makes the correlation: it takes those same two facts, adds the recorded
// manifests of the last two revisions and the container output the probe
// captured, and answers with causes that carry a confidence, the evidence
// behind them and, where it can be determined, the revision that introduced
// the change being blamed.
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
func newExplainCmd() *cobra.Command { return newExplainCmdFactory(connectDelivery) }

func newExplainCmdFactory(connect deliveryConnector) *cobra.Command {
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
	f.StringVar(&opts.mode, "mode", "", "delivery adapter to query, overriding the environment's delivery mode (direct or flux)")
	f.StringVar(&opts.image, "image", "", imageFlagUsage)
	f.StringVar(&opts.history, "history", "", "kelson data directory holding the direct-mode rendered history (default: $KELSON_DATA_DIR, else $XDG_DATA_HOME/kelson)")
	cobra.CheckErr(cmd.MarkFlagRequired("file"))
	return cmd
}

type explainOptions struct {
	specInput
	mode    string
	history string
	connect deliveryConnector
}

// runExplain reports and exits 0, for the reason `kelson status` does: this is
// the command you reach for when something is already broken, and a non-zero
// exit would make it unusable in the `set -e` scripts that need it most.
// `kelson deploy` is the command that gates on health.
func runExplain(cmd *cobra.Command, opts *explainOptions) error {
	target, set, err := resolveDeliveryTarget(opts.specInput, opts.history, opts.mode, cmd.ErrOrStderr())
	if err != nil {
		return err
	}
	adapter, plane, err := selectAdapter(opts.connect, target)
	if err != nil {
		return err
	}

	ctx := cmd.Context()
	status, err := adapter.Status(ctx, set)
	if err != nil {
		return err
	}
	verdicts, err := workloadVerdicts(ctx, plane, set, target.namespace)
	if err != nil {
		return err
	}

	in := explain.Input{
		Project:     set.Project,
		Environment: set.Environment,
		Namespace:   target.namespace,
		Status:      status,
		Verdicts:    verdicts,
		Manifests:   recordedManifests(plane),
	}
	// History is what makes the change correlation possible. A mode that keeps
	// none, or a store that cannot be read, costs the correlation and says so —
	// it never costs the explanation.
	entries, err := adapter.History(ctx, set)
	if err != nil {
		in.Notes = append(in.Notes,
			"the "+adapter.Name()+" adapter could not report its history ("+err.Error()+"), so no change was correlated")
	}
	in.History = entries
	if plane.health == nil {
		in.Notes = append(in.Notes,
			"no observation probe was available, so no workload health was read and no verdict-derived cause could be found")
	}

	out := &printer{w: cmd.OutOrStdout()}
	out.printf("%s\n", explain.Explain(ctx, in).Text())
	return out.err
}

// recordedManifests adapts the plane's rollback source onto explain's seam. The
// recorded bytes are what was applied — never a re-render — which is the only
// thing a claim like "this revision removed DATABASE_URL" can be built from.
func recordedManifests(plane *deliveryPlane) explain.ManifestFn {
	if plane == nil || plane.recorded == nil {
		return nil
	}
	return func(ctx context.Context, revision string) ([]delivery.Manifest, error) {
		return plane.recorded.Revision(ctx, revision)
	}
}
