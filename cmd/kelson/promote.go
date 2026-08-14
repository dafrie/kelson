package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dafrie/kelson/internal/clusterprofile"
	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/diff"
	"github.com/dafrie/kelson/internal/model"
	"github.com/dafrie/kelson/internal/promote"
	"github.com/dafrie/kelson/internal/renderer"
)

// newPromoteCmd builds `kelson promote` (issue #11, ADR-0016 decision 2):
// pin one environment to the images another environment is running.
//
// # Why this edits files rather than calling a server
//
// Every other command in this binary is file-shaped — `-f` documents, a local
// ClusterProfile, the per-user rendered-history journal — and there is no
// kelson-server client in cmd/kelson at all. So promote is file-shaped too:
// it reads the source environment's deployed digests out of the *local*
// direct-mode history, edits the Environment document on disk, and stops.
// The server path exists and is the same operation, one layer up: the Promote
// RPC (internal/api) does exactly this against the cluster-backed history and
// the spec store, and `kelson-mcp`'s promote_application composes it.
//
// The edit is byte-faithful (internal/promote): the authored document comes
// back with one line changed and everything else — comments, blank lines,
// key order — untouched. A promotion that reformatted the file it promoted
// would make every promotion a review of the whole document.
//
// # Why it is gated (issue #224)
//
// [ADR-0016](docs/adr/0016-delivery-flows-v0.md) decision 2 defines promotion
// as "three existing operations: read staging's deployed digest, write
// production's pin, deploy", and [ADR-0028](docs/adr/0028-delivery-spine.md)
// decision 6 keeps that definition intact. The first of the three is what
// broke: the deployed digest came from the local rendered-history journal, and
// that journal is deleted. The rest of this file — the plan, the byte-faithful
// splice, the promotion diff — is untouched and is what the rebuild plugs a new
// digest source into.
//
// Reading the *spec* of the source environment instead would be the wrong fix
// and is deliberately not done: a promotion moves what ran, not what was
// intended, and a promotion that silently promoted an intention would be worse
// than one that refuses.
func newPromoteCmd() *cobra.Command { return newPromoteCmdFactory() }

func newPromoteCmdFactory() *cobra.Command {
	opts := &promoteOptions{}
	cmd := &cobra.Command{
		Use:   "promote -f spec.yaml --from <environment> --to <environment>",
		Short: "Pin one environment to the images another environment is running",
		Long: "Promote is gated while the delivery spine is rebuilt (ADR-0028, issue #224): the images it\n" +
			"promotes come from the source environment's deployed revision, and the recorded history that\n" +
			"answered that was deleted with the old delivery machinery. Everything else about the command —\n" +
			"the plan, the byte-faithful edit, the diff — is unchanged and waiting on that one input.\n\n" +
			"Promote reads the images the source environment's latest deployed revision runs and\n" +
			"writes them as the target environment's per-component image pins — the whole of what\n" +
			"promotion is in kelson (ADR-0016). Nothing is rebuilt and nothing is deployed: the\n" +
			"target Environment document gains one image line per promoted component, and the\n" +
			"ordinary `kelson deploy` takes it from there.\n\n" +
			"The images come from the delivery history, not from the source's spec: a promotion\n" +
			"moves what ran, not what was intended. A component whose image the recorded revision\n" +
			"does not carry is skipped with a reason and never guessed.",
		Example: "  kelson promote -f project.yaml -f staging.yaml -f production.yaml --from staging --to production\n" +
			"  kelson promote -f spec.yaml --from staging --to production --component web --dry-run\n" +
			"  kelson promote -f spec.yaml --from staging --to production --yes",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runPromote(cmd, opts)
		},
	}
	f := cmd.Flags()
	f.StringArrayVarP(&opts.files, "file", "f", nil, "spec YAML file holding Project and/or Environment documents (repeatable)")
	f.StringVar(&opts.from, "from", "", "environment whose deployed images are promoted")
	f.StringVar(&opts.to, "to", "", "environment whose image pins are written")
	f.StringArrayVar(&opts.components, "component", nil, "restrict the promotion to this component (repeatable; default: every workload component)")
	f.StringVar(&opts.profile, "profile", "", "ClusterProfile YAML file, or from-cluster to capture a live profile (requires cluster access)")
	f.StringVar(&opts.kubeconfig, "kubeconfig", "", "path to a kubeconfig (default: $KUBECONFIG, in-cluster credentials, then ~/.kube/config)")
	f.BoolVar(&opts.dryRun, "dry-run", false, "print what would be pinned and the diff it produces, and write nothing")
	f.BoolVar(&opts.yes, "yes", false, "write the pins without asking for confirmation; the plan and the diff are printed either way")
	f.BoolVar(&opts.noColor, "no-color", false, "disable ANSI colour even on a terminal (also honoured via NO_COLOR)")
	cobra.CheckErr(cmd.MarkFlagRequired("file"))
	cobra.CheckErr(cmd.MarkFlagRequired("from"))
	cobra.CheckErr(cmd.MarkFlagRequired("to"))
	return cmd
}

type promoteOptions struct {
	files      []string
	from       string
	to         string
	components []string
	profile    string
	kubeconfig string
	dryRun     bool
	yes        bool
	noColor    bool
}

func runPromote(cmd *cobra.Command, opts *promoteOptions) error {
	if opts.from == opts.to {
		return fmt.Errorf("--from and --to are both %q: promoting an environment to itself would pin it to what it already runs", opts.from)
	}
	spec, err := loadPromoteSpec(opts.files)
	if err != nil {
		return err
	}
	source, err := spec.environment(opts.from)
	if err != nil {
		return err
	}
	target, err := spec.environment(opts.to)
	if err != nil {
		return err
	}

	revision, deployed, err := promotedImages(spec.project, source)
	if err != nil {
		return err
	}
	changes, err := promote.Plan(spec.project, target, deployed, opts.components)
	if err != nil {
		return err
	}

	out := &printer{w: cmd.OutOrStdout()}
	out.printf("Promoting %s/%s → %s (revision %s)\n\n", spec.project.Metadata.Name, opts.from, opts.to, revision)
	printPromotionPlan(out, changes)

	pins := promote.Pinned(changes)
	edited, file, err := spec.pin(opts.to, pins)
	if err != nil {
		return err
	}
	if err := printPromotionDiff(cmd, opts, spec, edited, file); err != nil {
		return err
	}
	if err := out.err; err != nil {
		return err
	}

	if len(pins) == 0 {
		out.printf("\nNothing to write: %s is already running what %s deployed.\n", opts.to, opts.from)
		return out.err
	}
	if opts.dryRun {
		out.printf("\nDry run: %s was not modified.\n", file)
		return out.err
	}
	if !opts.yes {
		ok, err := confirm(cmd, fmt.Sprintf("Write %d pin(s) to %s?", len(pins), file))
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("promotion cancelled")
		}
	}
	if err := writeSpecFile(file, edited); err != nil {
		return err
	}
	out.printf("\nWrote %d pin(s) to %s.\n", len(pins), file)
	out.printf("Next: kelson deploy %s --env %s\n", fileFlags(opts.files), opts.to)
	return out.err
}

// promotedImages reads what the source environment's latest revision runs.
//
// It is the one step of the promotion that has no source any more. The seam it
// read through was the rendered-history journal, whose bytes were what was
// actually applied; the spine's replacement is the source Environment's
// `status.history[]` mirror and, beyond that window, the registry's tag list
// (ADR-0028 decision 4). Neither exists yet, so the promotion refuses here —
// before the plan, before the splice and before anything is written.
func promotedImages(project *model.Project, source *model.Environment) (string, map[string]string, error) {
	return "", nil, delivery.NotImplemented("promote",
		fmt.Sprintf("kelson cannot read what %s/%s is running: promotion moves the images of the source "+
			"environment's deployed revision, and the recorded history that answered that was deleted "+
			"with the old delivery machinery", project.Metadata.Name, source.Metadata.Name),
		"#224")
}

// printPromotionPlan lists every component the promotion considered, including
// the ones it did nothing to. A component missing from the output would be
// indistinguishable from a component kelson forgot.
func printPromotionPlan(out *printer, changes []promote.Change) {
	out.printf("%-20s %-10s %s\n", "COMPONENT", "STATUS", "IMAGE")
	for _, c := range changes {
		detail := c.To
		switch c.Status {
		case promote.StatusSkipped:
			detail = c.Reason + " (" + string(c.Code) + ")"
		case promote.StatusUnchanged:
			detail = c.To + "  (already pinned)"
		case promote.StatusPinned:
			if c.From != "" {
				detail = c.From + "\n" + strings.Repeat(" ", 32) + "→ " + c.To
			}
		}
		out.printf("%-20s %-10s %s\n", c.Component, c.Status, detail)
	}
	out.printf("\n%s\n", promote.Summary(changes))
}

// printPromotionDiff renders the target environment before and after the pins
// and prints what changes — the same offline diff `kelson diff` prints, from
// the same machinery, so a promotion cannot show a different kind of preview
// from the command that previews everything else.
func printPromotionDiff(cmd *cobra.Command, opts *promoteOptions, spec *promoteSpec, edited []byte, file string) error {
	profile, err := resolveProfile(opts.profile, opts.kubeconfig, cmd.ErrOrStderr())
	if err != nil {
		return err
	}
	before, err := spec.render(opts.to, profile, "", nil)
	if err != nil {
		// An environment with no resolvable image renders nothing until the
		// pin gives it one, and that is exactly the promotion this command is
		// about to make. The before side is then empty and the diff reads as
		// additions, which is what it is.
		before = nil
	}
	after, err := spec.render(opts.to, profile, file, edited)
	if err != nil {
		return err
	}
	d, err := diff.Between(spec.project.Metadata.Name, opts.to, before, after, nil)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(cmd.OutOrStdout()); err != nil {
		return err
	}
	color := diff.DefaultColor(cmd.OutOrStdout()) && !opts.noColor
	return diff.Write(cmd.OutOrStdout(), d, color)
}

// fileFlags echoes the -f arguments back so the "next" hint is a command the
// reader can paste.
func fileFlags(files []string) string {
	parts := make([]string, 0, len(files))
	for _, f := range files {
		parts = append(parts, "-f "+f)
	}
	return strings.Join(parts, " ")
}

// writeSpecFile replaces a spec file with the promoted bytes, keeping its mode.
func writeSpecFile(file string, body []byte) error {
	mode := os.FileMode(0o644)
	if info, err := os.Stat(file); err == nil {
		mode = info.Mode().Perm()
	}
	if err := os.WriteFile(file, body, mode); err != nil {
		return fmt.Errorf("writing %s: %w", file, err)
	}
	return nil
}

/* ------------------------------------------------------- the loaded -f spec */

// promoteSpec is the -f input with the file each document came from still
// attached. Every other command throws that away after decoding; a promotion
// cannot, because it has to write one of those files back.
type promoteSpec struct {
	project      *model.Project
	environments []*model.Environment
	// files maps an environment name to the file that declares it.
	files map[string]string
	// bodies holds each file's bytes, so the render of the promoted spec reads
	// the edit without a round trip through the filesystem.
	bodies map[string][]byte
	// order preserves the -f order for re-decoding.
	order []string
	dirs  []string
}

func loadPromoteSpec(files []string) (*promoteSpec, error) {
	spec := &promoteSpec{files: map[string]string{}, bodies: map[string][]byte{}}
	dirSeen := map[string]bool{}
	for _, f := range files {
		data, err := os.ReadFile(filepath.Clean(f))
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", f, err)
		}
		spec.order = append(spec.order, f)
		spec.bodies[f] = data
		if dir := filepath.Dir(f); !dirSeen[dir] {
			dirSeen[dir] = true
			spec.dirs = append(spec.dirs, dir)
		}
		docs, derrs := model.DecodeDocuments(data)
		if len(derrs) > 0 {
			return nil, derrs
		}
		for _, d := range docs {
			switch v := d.(type) {
			case *model.Project:
				if spec.project != nil {
					return nil, fmt.Errorf("multiple Project documents supplied (%s and %s); promote takes exactly one project",
						spec.project.Metadata.Name, v.Metadata.Name)
				}
				spec.project = v
			case *model.Environment:
				spec.environments = append(spec.environments, v)
				spec.files[v.Metadata.Name] = f
			}
		}
	}
	if spec.project == nil {
		return nil, fmt.Errorf("no Project document found in %s", strings.Join(files, ", "))
	}
	if len(spec.environments) == 0 {
		return nil, fmt.Errorf("no Environment document found in %s", strings.Join(files, ", "))
	}
	return spec, nil
}

func (s *promoteSpec) environment(name string) (*model.Environment, error) {
	names := make([]string, len(s.environments))
	for i, e := range s.environments {
		names[i] = e.Metadata.Name
		if e.Metadata.Name == name {
			return e, nil
		}
	}
	return nil, fmt.Errorf("environment %q not found in the supplied files (available: %s)", name, strings.Join(names, ", "))
}

// pin applies every pin to the file that declares the target environment and
// returns the edited bytes and that file's path.
func (s *promoteSpec) pin(environment string, pins []promote.Change) ([]byte, string, error) {
	file, ok := s.files[environment]
	if !ok {
		return nil, "", fmt.Errorf("no supplied file declares environment %q", environment)
	}
	body := s.bodies[file]
	for _, c := range pins {
		edited, err := promote.Pin(body, environment, c.Component, c.To)
		if err != nil {
			return nil, "", err
		}
		body = edited
	}
	return body, file, nil
}

// render renders one environment of the spec, optionally with one file's bytes
// replaced — which is how the "after" side of the promotion diff is produced
// without writing anything to disk first.
func (s *promoteSpec) render(environment string, profile clusterprofile.ClusterProfile, replaceFile string, replaceBody []byte) ([]renderer.Manifest, error) {
	var project *model.Project
	var environments []*model.Environment
	for _, f := range s.order {
		body := s.bodies[f]
		if f == replaceFile {
			body = replaceBody
		}
		docs, derrs := model.DecodeDocuments(body)
		if len(derrs) > 0 {
			return nil, derrs
		}
		for _, d := range docs {
			switch v := d.(type) {
			case *model.Project:
				project = v
			case *model.Environment:
				environments = append(environments, v)
			}
		}
	}
	env, err := selectEnvironment(environments, environment)
	if err != nil {
		return nil, err
	}
	resolved, errs := model.Resolve(project, env)
	if len(errs) > 0 {
		return nil, errs
	}
	return renderer.Render(resolved, profile, overlayResolver(s.dirs))
}
