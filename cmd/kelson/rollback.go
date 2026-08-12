package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/delivery/rollback"
)

// newRollbackCmd builds `kelson rollback` (issues #38, #55): return an
// environment to a recorded revision, replaying the exact bytes that were live
// then — never a re-render.
//
// # The preview comes first, always
//
// A rollback is the operation people reach for when they are already in
// trouble, and the one thing that must not happen is discovering afterwards
// that it could not restore what they thought. So the irreversibility preview
// (internal/delivery/rollback) is computed and printed BEFORE anything is
// applied: immutable fields the API server will refuse to change back, PVCs
// whose data is gone either way, state a data operator owns, and the standing
// caveat that kelson runs no migrations. --yes skips the confirmation, never
// the preview.
func newRollbackCmd() *cobra.Command { return newRollbackCmdFactory(connectDelivery) }

func newRollbackCmdFactory(connect deliveryConnector) *cobra.Command {
	opts := &rollbackOptions{connect: connect}
	cmd := &cobra.Command{
		Use:   "rollback -f spec.yaml --env <name> [--to <revision>]",
		Short: "Return an environment to a recorded revision, previewing what cannot be reverted",
		Long: "Rollback replays the rendered output recorded for a previous revision. Nothing is\n" +
			"re-rendered: the bytes that were live then are the bytes applied now (issue #38).\n\n" +
			"Before applying, it prints the irreversibility preview — every change the rollback cannot\n" +
			"safely revert and why. With no --to, the target is the entry before the current one.",
		Example: "  kelson rollback -f project.yaml -f production.yaml --env production\n" +
			"  kelson rollback -f spec.yaml --env production --to 000007 --yes",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRollback(cmd, opts)
		},
	}
	f := cmd.Flags()
	f.StringArrayVarP(&opts.files, "file", "f", nil, "spec YAML file holding Project and/or Environment documents (repeatable)")
	f.StringVar(&opts.env, "env", "", "name of the Environment to roll back (optional when the input holds exactly one)")
	f.StringVar(&opts.to, "to", "", "revision to restore (default: the entry before the current one)")
	f.StringVar(&opts.profile, "profile", "", "ClusterProfile YAML file, or from-cluster to capture a live profile (requires cluster access)")
	f.StringVar(&opts.kubeconfig, "kubeconfig", "", "path to a kubeconfig (default: $KUBECONFIG, in-cluster credentials, then ~/.kube/config)")
	f.StringVar(&opts.mode, "mode", "", "delivery adapter to use, overriding the environment's delivery mode (direct or flux)")
	f.StringVar(&opts.history, "history", "", "kelson data directory holding the direct-mode rendered history (default: $KELSON_DATA_DIR, else $XDG_DATA_HOME/kelson)")
	f.BoolVar(&opts.yes, "yes", false, "apply the rollback without asking for confirmation; the preview is printed either way")
	cobra.CheckErr(cmd.MarkFlagRequired("file"))
	return cmd
}

type rollbackOptions struct {
	files      []string
	env        string
	to         string
	profile    string
	kubeconfig string
	mode       string
	history    string
	yes        bool
	connect    deliveryConnector
}

func runRollback(cmd *cobra.Command, opts *rollbackOptions) error {
	target, set, err := resolveDeliveryTarget(opts.files, opts.env, opts.profile, opts.kubeconfig, opts.history, opts.mode)
	if err != nil {
		return err
	}
	adapter, plane, err := selectAdapter(opts.connect, target)
	if err != nil {
		return err
	}
	if !adapter.Capabilities().SupportsRollback {
		return fmt.Errorf("the %s adapter cannot roll back", adapter.Name())
	}

	ctx := cmd.Context()
	entries, err := adapter.History(ctx, set)
	if err != nil {
		return err
	}
	entry, err := rollbackTarget(entries, opts.to)
	if err != nil {
		return err
	}

	out := &printer{w: cmd.OutOrStdout()}
	out.printf("Rolling %s/%s back to revision %s%s\n", set.Project, set.Environment, entry.Revision, committedAt(entry))
	if err := previewRollback(ctx, plane, set, entry, out); err != nil {
		return err
	}
	if err := out.err; err != nil {
		return err
	}

	if !opts.yes {
		ok, err := confirm(cmd, fmt.Sprintf("Restore revision %s?", entry.Revision))
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("rollback cancelled")
		}
	}

	res, err := adapter.Rollback(ctx, set, entry)
	if err != nil {
		return err
	}
	if !res.Applied {
		return fmt.Errorf("%s: the adapter did not complete the rollback to %s", adapter.Name(), entry.Revision)
	}
	out.printf("Restored revision %s as %s\n", entry.Revision, res.Revision)
	return out.err
}

// rollbackTarget picks the revision to restore. With no --to it is the entry
// before the current one — "undo the last deploy", the overwhelmingly common
// intent. History is newest first, so that is index 1.
func rollbackTarget(entries []delivery.Entry, to string) (delivery.Entry, error) {
	if len(entries) == 0 {
		return delivery.Entry{}, fmt.Errorf("no recorded history: nothing has been deployed for this environment, so there is nothing to roll back to")
	}
	if to == "" {
		if len(entries) < 2 {
			return delivery.Entry{}, fmt.Errorf("only one recorded revision (%s): there is no previous state to restore", entries[0].Revision)
		}
		return entries[1], nil
	}
	for _, e := range entries {
		if e.Revision == to {
			return e, nil
		}
	}
	return delivery.Entry{}, fmt.Errorf("revision %q is not in the retained history (available: %s)", to, revisions(entries))
}

func revisions(entries []delivery.Entry) string {
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Revision
	}
	return strings.Join(names, ", ")
}

func committedAt(e delivery.Entry) string {
	if e.CommittedAt == "" {
		return ""
	}
	return " (recorded " + e.CommittedAt + ")"
}

// previewRollback prints what the rollback cannot safely revert. A mode whose
// recorded history kelson cannot read gets an explicit "no preview" line rather
// than silence: an absent warning must never be mistaken for "nothing to warn
// about".
func previewRollback(ctx context.Context, plane *deliveryPlane, set delivery.ManifestSet, entry delivery.Entry, out *printer) error {
	if plane.recorded == nil {
		out.printf("\nno rendered history is readable for this delivery mode: the irreversibility preview is unavailable\n")
		return nil
	}
	d, findings, err := rollback.PreviewRevision(ctx, plane.recorded, set.Project, set.Environment, entry.Revision)
	if err != nil {
		return err
	}

	out.printf("\nWhat changes: %d added, %d modified, %d removed (max risk %s)\n",
		d.Summary.Added, d.Summary.Modified, d.Summary.Removed, d.Summary.MaxRisk)
	if len(d.Summary.Disruptive) > 0 {
		out.printf("Disruptive: %s\n", strings.Join(d.Summary.Disruptive, ", "))
	}

	out.printf("\nWhat a rollback cannot revert:\n")
	if len(findings) == 0 {
		out.printf("  (nothing identified)\n")
		return nil
	}
	for _, f := range findings {
		out.printf("  %s %s\n", marker(f), describeFinding(f))
	}
	return nil
}

// marker separates the findings that are merely risky from the ones where the
// prior state is gone for good.
func marker(f rollback.Finding) string {
	if f.Never {
		return "[unrecoverable]"
	}
	return "[warning]     "
}

func describeFinding(f rollback.Finding) string {
	head := string(f.Cause)
	if f.Resource != "" {
		head = f.Resource + " — " + head
	}
	if f.Path != "" {
		head += " at " + f.Path
	}
	if f.Message == "" {
		return head
	}
	return head + ": " + f.Message
}

