package main

import (
	"context"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
)

// newHistoryCmd builds `kelson history` (R2, issue #225): what an environment
// has published, newest first.
//
// DeployService.History reads Environment.status.history — a bounded mirror
// of the most recent revisions (ADR-0028 decision 4), not the local rendered
// history journal ADR-0027 deleted. Anything older than the mirror's window
// is still in the registry, immutably, and reading it is a registry query
// this command does not make.
func newHistoryCmd() *cobra.Command {
	opts := &historyOptions{}
	cmd := &cobra.Command{
		Use:   "history (-f spec.yaml | --project <name>) --env <name>",
		Short: "List what an environment has published, newest first",
		Long: "History lists the revisions an environment has published, newest first, from\n" +
			"Environment.status — a bounded mirror of the most recent revisions kept in the cluster\n" +
			"(ADR-0028 decision 4). Anything older is still in the OCI registry, immutably; reading it\n" +
			"is a registry query this command does not make.",
		Example: "  kelson history -f project.yaml -f production.yaml --env production\n" +
			"  kelson history --project shop --env production",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runHistory(cmd, opts)
		},
	}
	f := cmd.Flags()
	f.StringArrayVarP(&opts.files, "file", "f", nil, "spec YAML file holding Project and/or Environment documents (repeatable)")
	f.StringVar(&opts.project, "project", "", "name of a project already stored on kelson-server (alternative to -f)")
	f.StringVar(&opts.env, "env", "", "name of the Environment to report on (optional with -f when the input holds exactly one; required with --project)")
	addServerFlags(cmd, &opts.server)
	return cmd
}

type historyOptions struct {
	files   []string
	project string
	env     string
	server  serverOptions
}

func runHistory(cmd *cobra.Command, opts *historyOptions) error {
	ref, env, err := resolveSpecRef(opts.files, opts.project, opts.env)
	if err != nil {
		return err
	}
	client, addr := opts.server.deployClient()

	ctx, cancel := context.WithTimeout(cmd.Context(), requestTimeout)
	defer cancel()
	res, err := client.History(ctx, connect.NewRequest(&kelsonv1alpha1.HistoryRequest{Spec: ref, Environment: env}))
	if err != nil {
		return serverError("history", addr, err)
	}

	out := &printer{w: cmd.OutOrStdout()}
	entries := res.Msg.GetEntries()
	if len(entries) == 0 {
		out.printf("no revisions recorded for this environment\n")
		return out.err
	}
	out.printf("%-24s %-24s %s\n", "REVISION", "COMMITTED", "DETAIL")
	for _, e := range entries {
		out.printf("%-24s %-24s %s\n", e.GetRevision(), orDash(e.GetCommittedAt()), e.GetMessage())
	}
	return out.err
}
