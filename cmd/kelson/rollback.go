package main

import (
	"github.com/spf13/cobra"

	"github.com/dafrie/kelson/internal/delivery"
)

// newRollbackCmd builds `kelson rollback`, which is gated (issue #224).
//
// # What was here, and why it is not
//
// Rollback used to read the rendered-history journal, print the
// irreversibility preview and replay the recorded bytes through the
// environment's adapter. All three halves are gone: the journal was
// internal/serverstate and internal/delivery/direct, the preview was
// internal/delivery/rollback, and the replay was an adapter's Apply.
//
// [ADR-0028](docs/adr/0028-delivery-spine.md) decision 5 replaces the whole
// operation with a pointer move. Every revision kelson publishes is an
// immutable OCI artifact that cannot have changed since, so rolling back is
// repointing an `OCIRepository` at a tag that already exists — expressed as a
// `kelson.dev/rollback-to` annotation on the `Environment`, which also suspends
// re-rendering so the controller cannot immediately republish the thing you
// just rolled away from. `kelson rollback` becomes porcelain over that
// annotation.
//
// # The irreversibility preview is the part worth saying out loud
//
// The old preview named what a rollback could not restore — immutable fields
// the API server will refuse to change back, PVCs whose data is gone either
// way, state a data operator owns — and it was computed from two sets of
// recorded manifests. Nothing in the new spine has re-implemented it yet, which
// is a real loss and exactly why this command refuses rather than offering a
// rollback with the warning silently dropped. A rollback is what people reach
// for when they are already in trouble; the one thing that must not happen is
// discovering afterwards that it could not restore what they thought.
func newRollbackCmd() *cobra.Command {
	opts := &rollbackOptions{}
	cmd := &cobra.Command{
		Use:   "rollback -f spec.yaml --env <name> [--to <revision>]",
		Short: "Return an environment to a recorded revision (rebuilding on the controller — see issue #224)",
		Long: "Rollback is being rebuilt on the delivery spine (ADR-0028 decision 5, issue #224) and refuses\n" +
			"in the meantime.\n\n" +
			"What it becomes: every revision kelson publishes is an immutable OCI artifact, so a rollback\n" +
			"repoints the environment's OCIRepository at a tag that already exists and suspends re-render\n" +
			"until the pin clears. It replays nothing, because there is nothing to replay.\n\n" +
			"What refusing protects: the irreversibility preview — the immutable fields, the volumes and\n" +
			"the operator-owned state a rollback cannot restore — was computed from the recorded manifests\n" +
			"this rebuild deleted. Rolling back without it would be the failure this command exists to\n" +
			"prevent.",
		Example: "  kelson diff -f project.yaml -f production.yaml --env production   # what is different now",
		Args:    cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return delivery.NotImplemented("rollback",
				"kelson cannot roll back: the recorded rendered history and the irreversibility preview "+
					"were deleted with the old delivery machinery, and the annotation-driven rollback that "+
					"replaces them is not built",
				"#224")
		},
	}
	f := cmd.Flags()
	f.StringArrayVarP(&opts.files, "file", "f", nil, "spec YAML file holding Project and/or Environment documents (repeatable)")
	f.StringVar(&opts.env, "env", "", "name of the Environment to roll back (optional when the input holds exactly one)")
	f.StringVar(&opts.to, "to", "", "revision to restore (default: the entry before the current one)")
	f.StringVar(&opts.profile, "profile", "", "ClusterProfile YAML file, or from-cluster to capture a live profile (requires cluster access)")
	f.StringVar(&opts.kubeconfig, "kubeconfig", "", "path to a kubeconfig (default: $KUBECONFIG, in-cluster credentials, then ~/.kube/config)")
	f.StringVar(&opts.image, "image", "", imageFlagUsage)
	f.BoolVar(&opts.yes, "yes", false, "apply the rollback without asking for confirmation; the preview is printed either way")
	cobra.CheckErr(cmd.MarkFlagRequired("file"))
	return cmd
}

type rollbackOptions struct {
	specInput
	to  string
	yes bool
}
