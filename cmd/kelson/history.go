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
// DeployService.History reads two sources and says which is which (ADR-0028
// decision 4, issue #241). `Environment.status.history` is a bounded mirror of
// the most recent revisions; the registry holds every artifact ever published,
// immutably, and *that* is the record. The server pages past the mirror into
// the registry's tag list, so this command prints revisions the cluster has
// forgotten — and prints them apart from the rest, because all it knows about
// one of those is that it exists.
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
		Long: "History lists the revisions an environment has published, newest first.\n\n" +
			"Environment.status keeps a bounded mirror of the most recent ones, with the digest,\n" +
			"the images and the outcome of each (ADR-0028 decision 4). The OCI registry keeps every\n" +
			"artifact ever published, immutably, and it is the record: revisions older than the\n" +
			"mirror are listed from it, separately, because the registry knows they exist and\n" +
			"nothing more. Any of them can still be restored with `kelson rollback --to`.",
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
	printHistory(out, entries)
	return out.err
}

// printHistory prints the two sources as two things, because they answer
// different amounts.
//
// A mirrored revision fills every column. A revision only the registry
// remembers fills one, and putting it in the same table would print four
// dashes that read as "this deployment had no outcome" rather than "nothing
// recorded what its outcome was" — the distinction `beyond_window` exists to
// carry (proto/kelson/v1alpha1/deploy.proto). So they get their own list, under
// a line that says what is known about them and that they are still restorable.
func printHistory(out *printer, entries []*kelsonv1alpha1.HistoryEntry) {
	var registryOnly []*kelsonv1alpha1.HistoryEntry
	mirrored := make([]*kelsonv1alpha1.HistoryEntry, 0, len(entries))
	for _, e := range entries {
		if e.GetBeyondWindow() {
			registryOnly = append(registryOnly, e)
			continue
		}
		mirrored = append(mirrored, e)
	}

	if len(mirrored) > 0 {
		out.printf("%-16s %-22s %-12s %-20s %s\n", "REVISION", "COMMITTED", "OUTCOME", "DIGEST", "IMAGES")
		for _, e := range mirrored {
			out.printf("%-16s %-22s %-12s %-20s %s\n",
				e.GetRevision(), orDash(e.GetCommittedAt()), orDash(e.GetOutcome()),
				orDash(shortDigest(e.GetDigest())), orDash(historyImages(e)))
		}
	}
	if len(registryOnly) == 0 {
		return
	}
	if len(mirrored) > 0 {
		out.printf("\n")
	}
	out.printf("Older than the cluster's mirror — the registry's tag list confirms these revisions and\n" +
		"nothing recorded when they were published, what they ran or how they ended. Each is still\n" +
		"restorable: kelson rollback --to <revision>.\n")
	for _, e := range registryOnly {
		out.printf("  %s\n", e.GetRevision())
	}
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
