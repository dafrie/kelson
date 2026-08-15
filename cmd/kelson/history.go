package main

import (
	"context"
	"strings"

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
//
// Every column is a field of `HistoryEntry`; none of them is read out of the
// entry's prose `message`, which the server still fills only for clients built
// against the schema that had nowhere else to put them. OUTCOME is what the
// controller recorded when that revision stopped being the current one, so it
// says how that deployment ended — `kelson status` is what answers for now.
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
	out.printf("%-16s %-22s %-12s %-20s %s\n", "REVISION", "COMMITTED", "OUTCOME", "DIGEST", "IMAGES")
	for _, e := range entries {
		out.printf("%-16s %-22s %-12s %-20s %s\n",
			e.GetRevision(), orDash(e.GetCommittedAt()), orDash(e.GetOutcome()),
			orDash(shortDigest(e.GetDigest())), orDash(historyImages(e)))
	}
	return out.err
}

// shortDigest is a digest at reading length: sha256:0123456789ab. Only an
// algorithm:hex spelling is shortened, and only when there is more hex than the
// short form would show — a value this does not recognise is far likelier to be
// a format change than something safe to truncate.
func shortDigest(digest string) string {
	algorithm, hex, ok := strings.Cut(digest, ":")
	if !ok || len(hex) <= shortDigestLength {
		return digest
	}
	return algorithm + ":" + hex[:shortDigestLength]
}

const shortDigestLength = 12

// historyImages is one revision's images, each labelled with the component that
// resolved it. A component name is missing only on an entry the controller
// recorded before it attributed images; the image is printed on its own then,
// rather than under a fabricated label.
func historyImages(e *kelsonv1alpha1.HistoryEntry) string {
	images := e.GetImages()
	parts := make([]string, 0, len(images))
	for _, i := range images {
		if component := i.GetComponent(); component != "" {
			parts = append(parts, component+"="+i.GetImage())
			continue
		}
		parts = append(parts, i.GetImage())
	}
	return strings.Join(parts, " ")
}
