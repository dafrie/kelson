package main

import (
	"context"
	"encoding/json"
	"fmt"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1/kelsonv1alpha1connect"
	"github.com/dafrie/kelson/internal/diff"
)

// newPromoteCmd builds `kelson promote` (issue #11, ADR-0016 decision 2, R2
// #225): pin one environment to the images another environment is running.
//
// # Why this is a stored-spec verb, and file-shaped commands are not
//
// Promotion reads what the source environment's *deployed* revision runs —
// never a re-render of its spec, because a promotion moves what ran, not what
// was intended — and writes the pin as part of the target environment's spec.
// PromoteRequest therefore names a project kelson-server already holds
// (`--project`) rather than carrying inline `-f` documents: there would be
// nowhere for an inline write to land (proto/kelson/v1alpha1/deploy.proto).
// Store the spec first (an agent's put_spec, or a `kelson deploy --project`
// of one already stored) and name it here.
//
// # Two calls, the same shape as deploy and rollback
//
// The plan and the diff are computed with dry_run=RENDER first — nothing is
// written — and printed for a human to confirm; only the second call, with
// dry_run=NONE, writes the pins. internal/api.Promote computes both the plan
// and the diff server-side now (it reads the images off the source
// Environment's status and renders the target's before/after), so this
// command is a thin client of one RPC rather than the local plan/splice/diff
// pipeline the pre-rebuild CLI ran against a rendered-history journal that
// ADR-0027 deleted.
func newPromoteCmd() *cobra.Command {
	opts := &promoteOptions{}
	cmd := &cobra.Command{
		Use:   "promote --project <name> --from <environment> --to <environment>",
		Short: "Pin one environment to the images another environment is running",
		Long: "Promote reads the images the source environment's latest served revision runs and writes\n" +
			"them as the target environment's per-component image pins — the whole of what promotion is\n" +
			"in kelson (ADR-0016). Nothing is rebuilt and nothing is deployed by this command: the pins\n" +
			"are written to the stored spec, and an ordinary `kelson deploy --project <name> --env <to>`\n" +
			"takes it from there.\n\n" +
			"The images come from the source's delivery history, not from its spec: a promotion moves\n" +
			"what ran, not what was intended. A component whose deployed revision does not carry an image\n" +
			"kelson can attribute to it is skipped with a reason and never guessed.\n\n" +
			"The project must already be stored on kelson-server (`kelson deploy --project` of one already\n" +
			"there, or an agent's put_spec) — promote has no `-f` form, because there would be nowhere for\n" +
			"an inline write to land.",
		Example: "  kelson promote --project shop --from staging --to production\n" +
			"  kelson promote --project shop --from staging --to production --component web --dry-run\n" +
			"  kelson promote --project shop --from staging --to production --yes",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runPromote(cmd, opts)
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.project, "project", "", "name of the project, as stored on kelson-server")
	f.StringVar(&opts.from, "from", "", "environment whose deployed images are promoted")
	f.StringVar(&opts.to, "to", "", "environment whose image pins are written")
	f.StringArrayVar(&opts.components, "component", nil, "restrict the promotion to this component (repeatable; default: every workload component)")
	f.StringVar(&opts.profile, "profile", "", "ClusterProfile YAML file, or from-cluster to capture kelson-server's own cluster (used for the promotion diff)")
	f.BoolVar(&opts.dryRun, "dry-run", false, "print what would be pinned and the diff it produces, and write nothing")
	f.BoolVar(&opts.yes, "yes", false, "write the pins without asking for confirmation; the plan and the diff are printed either way")
	f.BoolVar(&opts.noColor, "no-color", false, "disable ANSI colour even on a terminal (also honoured via NO_COLOR)")
	addServerFlags(cmd, &opts.server)
	cobra.CheckErr(cmd.MarkFlagRequired("project"))
	cobra.CheckErr(cmd.MarkFlagRequired("from"))
	cobra.CheckErr(cmd.MarkFlagRequired("to"))
	return cmd
}

type promoteOptions struct {
	project    string
	from       string
	to         string
	components []string
	profile    string
	dryRun     bool
	yes        bool
	noColor    bool
	server     serverOptions
}

func runPromote(cmd *cobra.Command, opts *promoteOptions) error {
	if opts.from == opts.to {
		return fmt.Errorf("--from and --to are both %q: promoting an environment to itself would pin it to what it already runs", opts.from)
	}
	profile, err := inlineProfileRef(opts.profile)
	if err != nil {
		return err
	}
	client, addr := opts.server.deployClient()

	plan, err := requestPromote(cmd.Context(), client, opts, profile, kelsonv1alpha1.DryRun_DRY_RUN_RENDER, addr)
	if err != nil {
		return err
	}

	out := &printer{w: cmd.OutOrStdout()}
	out.printf("Promoting %s %s → %s (revision %s)\n\n", opts.project, opts.from, opts.to, orDash(plan.GetFromRevision()))
	printPromotionPlan(out, plan.GetComponents())
	if err := out.err; err != nil {
		return err
	}

	if errs := plan.GetErrors(); len(errs) > 0 {
		printWireErrors(out, errs)
		if err := out.err; err != nil {
			return err
		}
		return fmt.Errorf("promotion refused: nothing was written")
	}

	if err := printPromotionDiff(cmd, opts, plan); err != nil {
		return err
	}

	pinned := countPinned(plan.GetComponents())
	if pinned == 0 {
		out.printf("\nNothing to write: %s is already running what %s deployed.\n", opts.to, opts.from)
		return out.err
	}
	if opts.dryRun {
		out.printf("\nDry run: nothing was written.\n")
		return out.err
	}
	if !opts.yes {
		ok, err := confirm(cmd, fmt.Sprintf("Write %d pin(s) to %s/%s?", pinned, opts.project, opts.to))
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("promotion cancelled")
		}
	}

	written, err := requestPromote(cmd.Context(), client, opts, profile, kelsonv1alpha1.DryRun_DRY_RUN_NONE, addr)
	if err != nil {
		return err
	}
	if errs := written.GetErrors(); len(errs) > 0 {
		printWireErrors(out, errs)
		if err := out.err; err != nil {
			return err
		}
		return fmt.Errorf("promotion refused: nothing was written")
	}
	out.printf("\nWrote %d pin(s) to %s/%s (spec version %s).\n", pinned, opts.project, opts.to, orDash(written.GetVersion()))
	out.printf("Next: kelson deploy --project %s --env %s\n", opts.project, opts.to)
	return out.err
}

func requestPromote(ctx context.Context, client kelsonv1alpha1connect.DeployServiceClient, opts *promoteOptions, profile *kelsonv1alpha1.ProfileRef, dryRun kelsonv1alpha1.DryRun, addr string) (*kelsonv1alpha1.PromoteResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	res, err := client.Promote(ctx, connect.NewRequest(&kelsonv1alpha1.PromoteRequest{
		Project:         opts.project,
		FromEnvironment: opts.from,
		ToEnvironment:   opts.to,
		Components:      opts.components,
		Profile:         profile,
		DryRun:          dryRun,
		IdempotencyKey:  newIdempotencyKey(),
	}))
	if err != nil {
		return nil, serverError("promote", addr, err)
	}
	return res.Msg, nil
}

func countPinned(components []*kelsonv1alpha1.PromotedComponent) int {
	n := 0
	for _, c := range components {
		if c.GetStatus() == kelsonv1alpha1.PromotionStatus_PROMOTION_STATUS_PINNED {
			n++
		}
	}
	return n
}

// printPromotionPlan lists every component the promotion considered,
// including the ones it did nothing to. A component missing from the output
// would be indistinguishable from a component kelson forgot.
func printPromotionPlan(out *printer, components []*kelsonv1alpha1.PromotedComponent) {
	out.printf("%-20s %-10s %s\n", "COMPONENT", "STATUS", "IMAGE")
	for _, c := range components {
		status, detail := "skipped", fmt.Sprintf("%s (%s)", c.GetReason(), c.GetCode())
		switch c.GetStatus() {
		case kelsonv1alpha1.PromotionStatus_PROMOTION_STATUS_PINNED:
			status = "pinned"
			detail = c.GetToImage()
			if c.GetFromImage() != "" {
				detail = c.GetFromImage() + " -> " + c.GetToImage()
			}
		case kelsonv1alpha1.PromotionStatus_PROMOTION_STATUS_UNCHANGED:
			status = "unchanged"
			detail = c.GetToImage() + "  (already pinned)"
		}
		out.printf("%-20s %-10s %s\n", c.GetComponent(), status, detail)
	}
}

// printPromotionDiff renders the target environment's before/after diff the
// same way `kelson diff` does, from the same diff.Write terminal renderer, so
// a promotion cannot show a different kind of preview than the command that
// previews everything else.
func printPromotionDiff(cmd *cobra.Command, opts *promoteOptions, res *kelsonv1alpha1.PromoteResponse) error {
	encoded := res.GetDiffJson()
	if len(encoded) == 0 {
		return nil
	}
	var d diff.Diff
	if err := json.Unmarshal(encoded, &d); err != nil {
		return fmt.Errorf("decoding the promotion diff: %w", err)
	}
	if _, err := fmt.Fprintln(cmd.OutOrStdout()); err != nil {
		return err
	}
	color := diff.DefaultColor(cmd.OutOrStdout()) && !opts.noColor
	return diff.Write(cmd.OutOrStdout(), &d, color)
}

// printWireErrors prints every structured error a promotion was refused with.
// A response carrying errors has stored nothing (the same contract PutSpec
// has), so these are the whole of why.
func printWireErrors(out *printer, errs []*kelsonv1alpha1.Error) {
	out.printf("\n")
	for _, e := range errs {
		out.printf("  %s\n", wireErrSummary(e))
	}
}
