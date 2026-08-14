package main

import (
	"fmt"
	"io"
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
	f.StringVar(&opts.image, "image", "", imageFlagUsage)
	f.StringVarP(&opts.output, "output", "o", "", "directory to write one YAML file per manifest (default: stdout, multi-document)")
	cobra.CheckErr(cmd.MarkFlagRequired("file"))
	return cmd
}

type renderOptions struct {
	specInput
	output string
}

func runRender(cmd *cobra.Command, opts *renderOptions) error {
	_, _, manifests, _, err := resolveAndRender(opts.specInput, cmd.ErrOrStderr())
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

// specInput is the spec-loading half of every command that renders: which
// documents, which environment, which cluster shape, and which image to use
// for components the spec builds from source.
type specInput struct {
	files      []string
	env        string
	profile    string
	kubeconfig string
	image      string
}

// imageFlagUsage documents --image identically wherever a command renders. A
// spec with source + build has no image until a build produces one, so this
// flag is how a built artifact reaches a render. `kelson build` (issue #48)
// produces exactly such a reference — its last line of stdout is the
// digest-pinned image — and the two compose:
//
//	kelson deploy -f spec.yaml --image "$(kelson build -f spec.yaml --registry ghcr.io/acme | tail -1)"
//
// Without an image such a spec used to render `image: "@"` (issue #136); now it
// fails with image/unresolved.
const imageFlagUsage = "image reference for components the spec builds from source, e.g. ghcr.io/acme/app@sha256:abc123 (see `kelson build`)"

// resolveAndRender runs the shared spec pipeline: load the -f spec files,
// select the environment, resolve and render. It is the single place render
// and diff (issue #46) build the current manifest set, so the two commands
// cannot drift on what "the current render" means. It returns the resolved
// ClusterProfile too, so `kelson diff --dry-run=server` can hand it to the L2
// engine (issue #45) instead of resolving it a second time.
//
// warn receives the profile's version-skew statements; pass nil (or io.Discard)
// for a render whose profile is already being reported on elsewhere, so the
// same cluster is not warned about twice in one command.
func resolveAndRender(in specInput, warn io.Writer) (*model.Project, *model.Environment, []renderer.Manifest, clusterprofile.ClusterProfile, error) {
	profileValue, err := resolveProfile(in.profile, in.kubeconfig, warn)
	if err != nil {
		return nil, nil, nil, clusterprofile.ClusterProfile{}, err
	}

	project, environments, specDirs, err := loadSpecFiles(in.files)
	if err != nil {
		return nil, nil, nil, clusterprofile.ClusterProfile{}, err
	}
	if in.image != "" {
		// --image stands in for spec.image, so it is subject to the same
		// precedence: a component that names its own image still wins
		// (rule P3, docs/model.md).
		project.Spec.Image = in.image
	}
	environment, err := selectEnvironment(environments, in.env)
	if err != nil {
		return nil, nil, nil, clusterprofile.ClusterProfile{}, err
	}
	resolved, errs := model.Resolve(project, environment)
	if len(errs) > 0 {
		return nil, nil, nil, clusterprofile.ClusterProfile{}, errs
	}

	manifests, err := renderer.Render(resolved, profileValue, overlayResolver(specDirs))
	if err != nil {
		return nil, nil, nil, clusterprofile.ClusterProfile{}, err
	}
	return project, environment, manifests, profileValue, nil
}

// resolveProfile loads the ClusterProfile input. An empty flag renders
// against a zero profile (nothing detected: no Gateway API, no cert-manager,
// no prometheus) — valid and deterministic, but since #140 a spec whose
// services declare domains fails against it, because there is no routing
// substrate to attach them to and no Ingress fallback to hide behind.
//
// This is the one door every command's cluster shape comes through, which makes
// it the place to report version skew (issue #57): a component too old to serve
// the API kelson renders against is named here, at preview time, instead of
// failing confusingly at apply time. The statements go to warn — nil silences
// them, which is what a second, internal render wants. A captured profile is
// checked exactly like a live one: a stale `cluster.yaml` recording a
// since-upgraded operator is the same skew from the renderer's point of view.
func resolveProfile(flag, kubeconfig string, warn io.Writer) (clusterprofile.ClusterProfile, error) {
	switch flag {
	case "":
		// The zero profile detects nothing, so it has no versions to be skewed
		// against; skipping the report keeps offline renders quiet.
		return clusterprofile.ClusterProfile{}, nil
	case "from-cluster":
		p, err := detect.FromCluster(kubeconfig)
		if err != nil {
			return clusterprofile.ClusterProfile{}, err
		}
		writeSkew(warn, p)
		return p, nil
	default:
		data, err := os.ReadFile(filepath.Clean(flag))
		if err != nil {
			return clusterprofile.ClusterProfile{}, fmt.Errorf("reading cluster profile: %w", err)
		}
		var p clusterprofile.ClusterProfile
		if err := yaml.Unmarshal(data, &p); err != nil {
			return clusterprofile.ClusterProfile{}, fmt.Errorf("parsing cluster profile %s: %w", flag, err)
		}
		writeSkew(warn, p)
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
