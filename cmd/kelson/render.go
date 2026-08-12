package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/dafrie/kelson/internal/clusterprofile"
	"github.com/dafrie/kelson/internal/clusterprofile/detect"
	"github.com/dafrie/kelson/internal/model"
	"github.com/dafrie/kelson/internal/renderer"
)

// newRenderCmd builds `kelson render` (issue #30): fully offline rendering
// from spec documents plus a ClusterProfile. Cluster access lives strictly
// outside the renderer — capturing a live profile is a stub here and lands
// with the controller.
func newRenderCmd() *cobra.Command {
	opts := &renderOptions{}
	cmd := &cobra.Command{
		Use:   "render -f spec.yaml --env <name>",
		Short: "Render Kubernetes manifests from a kelson spec, offline",
		Long: "Render resolves a Project and Environment against a ClusterProfile and prints the resulting Kubernetes manifests.\n\n" +
			"The same inputs always produce the same bytes. No cluster is contacted: supply a ClusterProfile\nwith --profile <file>, or --profile from-cluster to capture one (requires cluster access).",
		Example: "  kelson render -f spec.yaml --env production --profile cluster.yaml\n" +
			"  kelson render -f project.yaml -f production.yaml --env production -o out/",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRender(cmd, opts)
		},
	}
	f := cmd.Flags()
	f.StringArrayVarP(&opts.files, "file", "f", nil, "spec YAML file holding Project and/or Environment documents (repeatable)")
	f.StringVar(&opts.env, "env", "", "name of the Environment to render (optional when the input holds exactly one)")
	f.StringVar(&opts.profile, "profile", "", "ClusterProfile YAML file, or from-cluster to capture a live profile (requires cluster access)")
	f.StringVar(&opts.kubeconfig, "kubeconfig", "", "path to a kubeconfig for --profile from-cluster (default: $KUBECONFIG, in-cluster credentials, then ~/.kube/config)")
	f.StringVarP(&opts.output, "output", "o", "", "directory to write one YAML file per manifest (default: stdout, multi-document)")
	cobra.CheckErr(cmd.MarkFlagRequired("file"))
	return cmd
}

type renderOptions struct {
	files      []string
	env        string
	profile    string
	kubeconfig string
	output     string
}

func runRender(cmd *cobra.Command, opts *renderOptions) error {
	_, _, manifests, err := resolveAndRender(opts.files, opts.env, opts.profile, opts.kubeconfig)
	if err != nil {
		return err
	}

	if opts.output == "" {
		out, err := renderer.Encode(manifests)
		if err != nil {
			return err
		}
		if _, err := cmd.OutOrStdout().Write(out); err != nil {
			return fmt.Errorf("writing output: %w", err)
		}
		return nil
	}
	return writeManifestDir(cmd, opts.output, manifests)
}

// resolveAndRender runs the shared spec pipeline: load the -f spec files,
// select the environment, resolve and render. It is the single place render
// and diff (issue #46) build the current manifest set, so the two commands
// cannot drift on what "the current render" means.
func resolveAndRender(files []string, env, profile, kubeconfig string) (*model.Project, *model.Environment, []renderer.Manifest, error) {
	profileValue, err := resolveProfile(profile, kubeconfig)
	if err != nil {
		return nil, nil, nil, err
	}

	project, environments, specDirs, err := loadSpecFiles(files)
	if err != nil {
		return nil, nil, nil, err
	}
	environment, err := selectEnvironment(environments, env)
	if err != nil {
		return nil, nil, nil, err
	}
	resolved, errs := model.Resolve(project, environment)
	if len(errs) > 0 {
		return nil, nil, nil, errs
	}

	manifests, err := renderer.Render(resolved, profileValue, overlayResolver(specDirs))
	if err != nil {
		return nil, nil, nil, err
	}
	return project, environment, manifests, nil
}

// resolveProfile loads the ClusterProfile input. An empty flag renders
// against a zero profile (nothing detected: no gateway API, no ingress, no
// cert-manager, no prometheus) — valid and deterministic.
func resolveProfile(flag, kubeconfig string) (clusterprofile.ClusterProfile, error) {
	switch flag {
	case "":
		return clusterprofile.ClusterProfile{}, nil
	case "from-cluster":
		return detect.FromCluster(kubeconfig)
	default:
		data, err := os.ReadFile(filepath.Clean(flag))
		if err != nil {
			return clusterprofile.ClusterProfile{}, fmt.Errorf("reading cluster profile: %w", err)
		}
		var p clusterprofile.ClusterProfile
		if err := yaml.Unmarshal(data, &p); err != nil {
			return clusterprofile.ClusterProfile{}, fmt.Errorf("parsing cluster profile %s: %w", flag, err)
		}
		return p, nil
	}
}

// loadSpecFiles decodes every -f file and returns the single Project, all
// Environment documents found, and the set of spec directories (used to
// resolve overlay paths, which are relative to the authoring files).
func loadSpecFiles(files []string) (*model.Project, []*model.Environment, []string, error) {
	var project *model.Project
	var environments []*model.Environment
	dirSeen := map[string]bool{}
	var dirs []string
	for _, f := range files {
		data, err := os.ReadFile(filepath.Clean(f))
		if err != nil {
			return nil, nil, nil, fmt.Errorf("reading %s: %w", f, err)
		}
		if dir := filepath.Dir(f); !dirSeen[dir] {
			dirSeen[dir] = true
			dirs = append(dirs, dir)
		}
		docs, derrs := model.DecodeDocuments(data)
		if len(derrs) > 0 {
			return nil, nil, nil, derrs
		}
		for _, d := range docs {
			switch v := d.(type) {
			case *model.Project:
				if project != nil {
					return nil, nil, nil, fmt.Errorf("multiple Project documents supplied (%s and %s); render takes exactly one project", project.Metadata.Name, v.Metadata.Name)
				}
				project = v
			case *model.Environment:
				environments = append(environments, v)
			}
		}
	}
	if project == nil {
		return nil, nil, nil, fmt.Errorf("no Project document found in %s", strings.Join(files, ", "))
	}
	if len(environments) == 0 {
		return nil, nil, nil, fmt.Errorf("no Environment document found in %s", strings.Join(files, ", "))
	}
	return project, environments, dirs, nil
}

func selectEnvironment(environments []*model.Environment, name string) (*model.Environment, error) {
	names := make([]string, len(environments))
	for i, e := range environments {
		names[i] = e.Metadata.Name
	}
	if name == "" {
		if len(environments) == 1 {
			return environments[0], nil
		}
		return nil, fmt.Errorf("multiple environments present (%s); select one with --env", strings.Join(names, ", "))
	}
	for _, e := range environments {
		if e.Metadata.Name == name {
			return e, nil
		}
	}
	return nil, fmt.Errorf("environment %q not found (available: %s)", name, strings.Join(names, ", "))
}

// overlayResolver resolves spec-relative overlay paths against the
// directories the spec files came from, in -f order.
func overlayResolver(dirs []string) renderer.OverlayResolver {
	return func(path string) ([]byte, error) {
		for _, dir := range dirs {
			candidate := filepath.Join(dir, filepath.FromSlash(path))
			data, err := os.ReadFile(candidate)
			if err == nil {
				return data, nil
			}
		}
		return nil, fmt.Errorf("overlay %s not found relative to %s", path, strings.Join(dirs, ", "))
	}
}

// writeManifestDir writes one file per manifest, named by position so the
// rendering order is preserved on disk.
func writeManifestDir(cmd *cobra.Command, dir string, manifests []renderer.Manifest) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating output directory: %w", err)
	}
	out := cmd.OutOrStdout()
	for i, m := range manifests {
		name := fmt.Sprintf("%02d-%s-%s.yaml", i+1, strings.ToLower(m.Kind), m.Name)
		data, err := m.YAML()
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			return fmt.Errorf("writing %s: %w", name, err)
		}
		if _, err := fmt.Fprintf(out, "wrote %s\n", filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	return nil
}
