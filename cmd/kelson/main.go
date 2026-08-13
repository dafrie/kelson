package main

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/dafrie/kelson/internal/version"
)

func main() {
	root := newRootCmd()
	if err := root.Execute(); err != nil {
		os.Exit(rootError(err))
	}
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:          "kelson",
		Short:        "kelson — the self-hosted Kubernetes PaaS CLI",
		Long:         "kelson renders, diffs and deploys applications described by Project and Environment documents. Rendering is offline and deterministic: the same spec always produces the same bytes.",
		Version:      version.String(),
		SilenceUsage: true,
		// Error output and exit codes are centralized in rootError so the diff
		// command's non-error codes 2/3 can exit without cobra printing a
		// spurious "Error:" line (issue #46).
		SilenceErrors: true,
	}
	root.AddCommand(newRenderCmd())
	root.AddCommand(newEjectCmd())
	root.AddCommand(newDiffCmd())
	root.AddCommand(newBuildCmd())
	root.AddCommand(newProfileCmd())
	root.AddCommand(newDeployCmd())
	root.AddCommand(newStatusCmd())
	root.AddCommand(newRollbackCmd())
	root.AddCommand(newPromoteCmd())
	return root
}
