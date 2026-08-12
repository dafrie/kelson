package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dafrie/kelson/internal/clusterprofile"
	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/delivery/dryrun"
	"github.com/dafrie/kelson/internal/delivery/kube"
	"github.com/dafrie/kelson/internal/diff"
	"github.com/dafrie/kelson/internal/model"
	"github.com/dafrie/kelson/internal/renderer"
)

// newDiffCmd builds `kelson diff` (issue #46): the preview surface usable as a
// CI gate. It renders the current spec and prints what would change —
// compared against either a previous render (L1, --dry-run=render, offline) or
// the live cluster (L2, --dry-run=server). It exits 0 when nothing changes,
// 2 when changes are present, and 3 when the preview finds a blocker, so a CI
// check can fail a pull request the cluster policy would reject.
func newDiffCmd() *cobra.Command {
	return newDiffCmdFactory(newServerDryRun)
}

// diffRunner is the L2 (--dry-run=server) preview seam. The only in-tree
// implementation is *dryrun.DryRun, which takes a Kubernetes dynamic client
// and REST mapper — libraries the command plane's lint allow-list forbids
// (they live behind the delivery plane). The seam is injected as a constructor
// on diffOptions so tests can drive a fake cluster and so --dry-run=render
// never touches a client.
type diffRunner interface {
	Preview(ctx context.Context, set delivery.ManifestSet) (*diff.Diff, error)
}

// newDiffCmdFactory builds `kelson diff` with an injectable L2 engine
// constructor. Production uses newServerDryRun; tests inject a dryrun engine
// backed by a fake cluster. The factory receives the resolved ClusterProfile
// so the L2 engine can consult HasPolicyEngine() when attributing a rejection
// (issue #45).
func newDiffCmdFactory(newServer func(kubeconfig string, profile clusterprofile.ClusterProfile) (diffRunner, error)) *cobra.Command {
	opts := &diffOptions{newServer: newServer}
	cmd := &cobra.Command{
		Use:   "diff -f spec.yaml --env <name> [--from <spec.yaml>] [--dry-run render|server]",
		Short: "Show what would change, as a CI gate",
		Long: "Diff renders the current spec and prints what would change.\n\n" +
			"Compared against the previous state, which depends on --dry-run and --from:\n" +
			"  --dry-run=render (default) is offline and needs no server and no cluster. The\n" +
			"    before side is --from <spec.yaml>, the previous spec rendered the same way; with\n" +
			"    no --from and no recorded history, every resource the spec produces is reported\n" +
			"    as an addition.\n" +
			"  --dry-run=server asks the live API server for its own verdict and diffs against\n" +
			"    current cluster state. It requires cluster access.\n\n" +
			"Exit codes (the CI contract):\n" +
			"  0  no changes\n" +
			"  1  usage or runtime error\n" +
			"  2  changes present\n" +
			"  3  a blocker — an enforce-mode policy violation, or a resource whose\n" +
			"     prerequisite is genuinely missing so the apply would fail",
		Example: "  kelson diff -f spec.yaml --env production\n" +
			"  kelson diff -f spec.yaml --env production --from spec.yaml.previous\n" +
			"  kelson diff -f spec.yaml --env production --dry-run=server --output json",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDiff(cmd, opts)
		},
	}
	f := cmd.Flags()
	f.StringArrayVarP(&opts.files, "file", "f", nil, "spec YAML file holding Project and/or Environment documents (repeatable)")
	f.StringVar(&opts.env, "env", "", "name of the Environment to compare (optional when the input holds exactly one)")
	f.StringVar(&opts.profile, "profile", "", "ClusterProfile YAML file (default: zero profile, rendered offline)")
	f.StringVar(&opts.from, "from", "", "previous spec YAML file to compare against (render mode); with no --from everything is reported as an addition")
	f.StringVar(&opts.dryRun, "dry-run", "render", "fidelity of the preview: render (offline, L1) or server (cluster access, L2)")
	f.StringVar(&opts.output, "output", "", "output format: empty for the terminal report, or json for the structured diff")
	f.StringVar(&opts.kubeconfig, "kubeconfig", "", "path to a kubeconfig for --dry-run=server (default: $KUBECONFIG, in-cluster credentials, then ~/.kube/config)")
	f.BoolVar(&opts.noColor, "no-color", false, "disable ANSI colour even on a terminal (also honoured via NO_COLOR)")
	cobra.CheckErr(cmd.MarkFlagRequired("file"))
	return cmd
}

type diffOptions struct {
	files      []string
	env        string
	profile    string
	from       string
	dryRun     string
	kubeconfig string
	output     string
	noColor    bool
	newServer  func(kubeconfig string, profile clusterprofile.ClusterProfile) (diffRunner, error)
}

// newServerDryRun constructs the live L2 engine. The Kubernetes client
// libraries are forbidden here by the command plane's lint allow-list, so the
// connection is built behind the delivery plane by kube.Connect and this
// function only assembles the engine from it (issue #46).
//
// It never falls back to render on failure: an unreachable cluster is a real
// error, and silently downgrading a requested server-side preview would give a
// CI gate a clean answer it did not earn. Losing dry-run *permission* is a
// different case, handled inside the engine as a reported degradation.
func newServerDryRun(kubeconfig string, profile clusterprofile.ClusterProfile) (diffRunner, error) {
	cluster, err := kube.Connect(kubeconfig)
	if err != nil {
		return nil, err
	}
	return dryrun.New(dryrun.Options{Client: cluster.Dynamic, Mapper: cluster.Mapper, ClusterProfile: profile})
}

func runDiff(cmd *cobra.Command, opts *diffOptions) error {
	project, environment, cur, profile, err := resolveAndRender(opts.files, opts.env, opts.profile, opts.kubeconfig)
	if err != nil {
		return err
	}

	var d *diff.Diff
	switch strings.ToLower(opts.dryRun) {
	case "server":
		d, err = opts.runServer(cmd.Context(), project, environment, cur, profile)
	case "render", "":
		d, err = runRenderedDiff(project, environment, opts, cur)
	default:
		return fmt.Errorf("unknown --dry-run %q: choose render or server", opts.dryRun)
	}
	if err != nil {
		return err
	}

	if err := printDiff(cmd, d, opts); err != nil {
		return err
	}
	if code := diffExitCode(d); code != exitOK {
		return &exitError{code: code}
	}
	return nil
}

// runRenderedDiff computes the L1 (offline) diff against a previous render.
// The before side comes from --from, a previous spec rendered with the same
// environment and profile as the current spec. With no --from there is no
// prior state, so every current resource is an addition — the honest answer
// when a CLI has no server and no recorded history (issue #46).
func runRenderedDiff(project *model.Project, environment *model.Environment, opts *diffOptions, cur []renderer.Manifest) (*diff.Diff, error) {
	var prev []renderer.Manifest
	if opts.from != "" {
		_, _, fromManifests, _, err := resolveAndRender([]string{opts.from}, environment.Metadata.Name, opts.profile, opts.kubeconfig)
		if err != nil {
			return nil, err
		}
		prev = fromManifests
	}
	return diff.Between(project.Metadata.Name, environment.Metadata.Name, prev, cur, nil)
}

// runServer drives the L2 engine (--dry-run=server). It is the only path that
// can touch a cluster, keeping --dry-run=render free of any client.
func (opts *diffOptions) runServer(ctx context.Context, project *model.Project, environment *model.Environment, cur []renderer.Manifest, profile clusterprofile.ClusterProfile) (*diff.Diff, error) {
	if opts.newServer == nil {
		return nil, errors.New("--dry-run=server is unavailable in this build")
	}
	engine, err := opts.newServer(opts.kubeconfig, profile)
	if err != nil {
		return nil, err
	}
	set, err := manifestsToSet(project, environment, cur)
	if err != nil {
		return nil, err
	}
	return engine.Preview(ctx, set)
}

// manifestsToSet lifts a renderer output into the delivery.ManifestSet shape
// the preview engines consume.
func manifestsToSet(project *model.Project, environment *model.Environment, ms []renderer.Manifest) (delivery.ManifestSet, error) {
	set := delivery.ManifestSet{
		Project:     project.Metadata.Name,
		Environment: environment.Metadata.Name,
	}
	for _, m := range ms {
		body, err := m.YAML()
		if err != nil {
			return set, fmt.Errorf("diff: encoding manifest %s/%s: %w", m.Kind, m.Name, err)
		}
		set.Manifests = append(set.Manifests, delivery.Manifest{
			APIVersion: m.APIVersion,
			Kind:       m.Kind,
			Name:       m.Name,
			Namespace:  m.Namespace,
			YAML:       body,
		})
	}
	return set, nil
}

// printDiff emits the report: the structured JSON form for --output json, and
// the human terminal report (with risk highlighting where colour is on)
// otherwise.
func printDiff(cmd *cobra.Command, d *diff.Diff, opts *diffOptions) error {
	if opts.output == "json" {
		data, err := diff.EncodeJSON(d)
		if err != nil {
			return err
		}
		if _, err := cmd.OutOrStdout().Write(data); err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout())
		return err
	}
	color := diff.DefaultColor(cmd.OutOrStdout()) && !opts.noColor
	return diff.Write(cmd.OutOrStdout(), d, color)
}

// diffExitCode maps a preview onto the CI exit contract. A blocker (an
// enforce-mode policy violation, or an Unvalidated resource whose prerequisite
// is genuinely absent) is 3; any change present is 2; otherwise 0.
func diffExitCode(d *diff.Diff) int {
	for _, v := range d.Violations {
		if v.Enforcement == diff.EnforcementEnforce {
			return exitBlk
		}
	}
	for _, u := range d.Unvalidated {
		if !u.InBatch {
			return exitBlk
		}
	}
	if len(d.Resources) > 0 {
		return exitDiff
	}
	return exitOK
}
