package main

import (
	"fmt"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/dafrie/kelson/internal/clusterprofile/detect"
)

// newProfileCmd builds `kelson profile` (issue #56): capture the current
// cluster's capabilities as a ClusterProfile and write it to stdout as YAML,
// so `kelson profile > cluster.yaml && kelson render --profile cluster.yaml`
// round-trips. This is the read side of the profile contract — what the
// renderer learns about a cluster without being part of it.
func newProfileCmd() *cobra.Command {
	opts := &profileOptions{}
	cmd := &cobra.Command{
		Use:   "profile",
		Short: "Capture the current cluster's capabilities as a ClusterProfile",
		Long: "Profile probes the live cluster (discovery for what is installed, lists\n" +
			"for classes, issuers and stores) and writes the result to stdout as YAML.\n" +
			"The output feeds \"kelson render --profile\" or \"kelson diff --profile\".\n\n" +
			"Detection is read-only and needs no cluster-admin. Anything the probe was\n" +
			"not allowed to look at is NOT reported as absent: it appears as a gap\n" +
			"under \"incomplete\" in the YAML and as a warning on stderr, so a partial\n" +
			"profile is loud instead of silently rendering without TLS or routing.",
		Example: "  kelson profile > cluster.yaml\n" +
			"  kelson profile --kubeconfig ./dev.kubeconfig",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runProfile(cmd, opts)
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.kubeconfig, "kubeconfig", "", "path to a kubeconfig (default: $KUBECONFIG, in-cluster credentials, then ~/.kube/config)")
	return cmd
}

type profileOptions struct {
	kubeconfig string
}

// runProfile captures a profile and writes it. Incomplete gaps are the failure
// mode this design exists to prevent — a user piping `profile > cluster.yaml`
// sees only stdout, so the warning must also reach stderr, and the gaps stay in
// the YAML so a later render can act on them rather than on a guess.
func runProfile(cmd *cobra.Command, opts *profileOptions) error {
	prof, err := detect.FromCluster(opts.kubeconfig)
	if err != nil {
		return err
	}
	if len(prof.Incomplete) > 0 {
		// The terminal report is addressed to the eye that is not watching the
		// pipe; the YAML below carries the same facts for anything that is.
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: profile is incomplete — %d gap(s) that could not be read as absent:\n", len(prof.Incomplete))
		for _, g := range prof.Incomplete {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "  - %s: %s\n", g.Field, g.Reason)
		}
	}
	data, err := yaml.Marshal(prof)
	if err != nil {
		return fmt.Errorf("encoding profile: %w", err)
	}
	if _, err := cmd.OutOrStdout().Write(data); err != nil {
		return fmt.Errorf("writing profile: %w", err)
	}
	return nil
}
