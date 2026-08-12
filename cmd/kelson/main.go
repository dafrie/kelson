package main

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/dafrie/kelson/internal/version"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:          "kelson",
		Short:        "kelson — the self-hosted Kubernetes PaaS CLI",
		Long:         "kelson renders, diffs and deploys applications described by Project and Environment documents. Rendering is offline and deterministic: the same spec always produces the same bytes.",
		Version:      version.String(),
		SilenceUsage: true,
	}
	root.AddCommand(newRenderCmd())
	root.AddCommand(newEjectCmd())
	return root
}
