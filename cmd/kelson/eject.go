package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/dafrie/kelson/internal/delivery/direct"
	"github.com/dafrie/kelson/internal/delivery/eject"
	"github.com/dafrie/kelson/internal/delivery/git"
	"github.com/dafrie/kelson/internal/model"
)

// newEjectCmd builds `kelson eject --to-git` (issue #41): replay a direct-mode
// environment's rendered history into a real Git repository and switch the
// environment's delivery adapter.
//
// # Why the command is offline and spec-file shaped
//
// It takes the same -f spec files `kelson render` takes, and it reads the
// rendered history straight from the data directory. Nothing here contacts a
// cluster: the history is already on disk, the replay is a git push, and the
// spec change is a text edit. Keeping eject offline means the command can be
// rehearsed with --dry-run against production inputs without any credential
// beyond read access to the two things it reads.
//
// # Why the spec change defaults to printing
//
// Rewriting somebody's spec file is the one irreversible half of an eject, and
// spec files are commonly generated, templated or in a repository of their
// own. So the default is to print the updated Environment document and leave
// the file alone; --write opts in to an in-place rewrite. The rewrite goes
// through the YAML node tree, so comments, key order and the other documents
// in the file survive — only the delivery stanza changes.
func newEjectCmd() *cobra.Command {
	opts := &ejectOptions{}
	cmd := &cobra.Command{
		Use:   "eject --to-git <repository> -f spec.yaml --env <name> --history <dir>",
		Short: "Replay a direct-mode environment's rendered history into a Git repository",
		Long: "Eject replays the rendered history a direct-mode environment already keeps into a real Git\n" +
			"repository, one commit per recorded revision, preserving chronology and authorship, and\n" +
			"switches the environment to a Git-mode delivery adapter.\n\n" +
			"Nothing is re-rendered: direct mode is Git mode with an implicit repository (ADR-0001), so the\n" +
			"final commit is byte-identical to the latest recorded revision. The environment keeps running\n" +
			"across the switch with no redeploy and no manifest changes.",
		Example: "  kelson eject --to-git git@github.com:acme/deploy.git -f project.yaml -f production.yaml --env production --history ./data --dry-run\n" +
			"  kelson eject --to-git git@github.com:acme/deploy.git -f project.yaml -f production.yaml --env production --history ./data --mode argocd --write",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runEject(cmd, opts)
		},
	}
	f := cmd.Flags()
	f.StringArrayVarP(&opts.files, "file", "f", nil, "spec YAML file holding Project and/or Environment documents (repeatable)")
	f.StringVar(&opts.env, "env", "", "name of the Environment to eject (optional when the input holds exactly one)")
	f.StringVar(&opts.repo, "to-git", "", "git repository the rendered history is replayed into")
	f.StringVar(&opts.branch, "branch", git.DefaultBranch, "branch to replay onto")
	f.StringVar(&opts.path, "path", "", "directory within the repository kelson owns (default: <project>/<environment>)")
	f.StringVar(&opts.mode, "mode", eject.ModeFlux, "delivery mode to switch to: flux or argocd")
	f.StringVar(&opts.history, "history", "", "kelson data directory holding the direct-mode rendered history")
	f.BoolVar(&opts.dryRun, "dry-run", false, "print the planned commits and the updated Environment; write nothing")
	f.BoolVar(&opts.write, "write", false, "rewrite the Environment spec file in place instead of printing the updated document")
	f.BoolVar(&opts.noBootstrap, "no-bootstrap", false, "do not emit the Flux/Argo CD bootstrap manifests")
	f.StringVar(&opts.bootstrapOut, "bootstrap-out", "", "write the bootstrap manifests to this file instead of stdout")
	cobra.CheckErr(cmd.MarkFlagRequired("file"))
	cobra.CheckErr(cmd.MarkFlagRequired("to-git"))
	cobra.CheckErr(cmd.MarkFlagRequired("history"))
	return cmd
}

type ejectOptions struct {
	files        []string
	env          string
	repo         string
	branch       string
	path         string
	mode         string
	history      string
	dryRun       bool
	write        bool
	noBootstrap  bool
	bootstrapOut string
}

func runEject(cmd *cobra.Command, opts *ejectOptions) error {
	project, environments, _, err := loadSpecFiles(opts.files)
	if err != nil {
		return err
	}
	environment, err := selectEnvironment(environments, opts.env)
	if err != nil {
		return err
	}
	resolved, errs := model.Resolve(project, environment)
	if len(errs) > 0 {
		return errs
	}
	// Ejecting an environment that is already Git-backed would replay a
	// history it does not keep. Refusing here is clearer than an empty-history
	// error three steps later.
	if resolved.Environment.Mode != model.DeliveryDirect {
		return fmt.Errorf("environment %q is already in %q delivery mode; eject replays a direct-mode rendered history",
			environment.Metadata.Name, resolved.Environment.Mode)
	}

	mode, err := targetMode(opts.mode)
	if err != nil {
		return err
	}
	target := git.Target{
		Repo:   opts.repo,
		Branch: opts.branch,
		Path:   opts.path,
	}
	if target.Path == "" {
		target.Path = project.Metadata.Name + "/" + environment.Metadata.Name
	}

	store, err := direct.OpenStore(direct.StoreOptions{Dir: opts.history})
	if err != nil {
		return err
	}
	ejector, err := eject.New(eject.Options{
		Project:     project.Metadata.Name,
		Environment: environment.Metadata.Name,
		History:     store,
		Target:      target,
		Mode:        string(mode),
		Identity:    git.IdentityFromEnv(nil),
		Bootstrap:   eject.BootstrapOptions{Disabled: opts.noBootstrap},
	})
	if err != nil {
		return err
	}

	plan, err := ejector.Plan()
	if err != nil {
		return err
	}

	// The spec edit is computed up front so --dry-run can show exactly the
	// document a real run would produce, but it is applied only after the
	// replay lands: a spec that claims Git mode over a repository that was
	// never written is the one state worse than not ejecting at all.
	edit, err := planSpecEdit(opts.files, environment.Metadata.Name, mode, ejector.Target())
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	if opts.dryRun {
		return printDryRun(out, plan, edit, opts)
	}

	res, err := ejector.Write(cmd.Context(), plan)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "replayed %d revision(s) into %s (%s), tip %s is %s\n",
		res.Commits, plan.Target.Repo, res.Branch, short(res.Head), res.TipRevision); err != nil {
		return err
	}

	if err := applySpecEdit(out, edit, opts.write); err != nil {
		return err
	}
	return emitBootstrap(out, plan.Bootstrap, opts.bootstrapOut)
}

func targetMode(flag string) (model.DeliveryMode, error) {
	switch strings.ToLower(strings.TrimSpace(flag)) {
	case eject.ModeFlux:
		return model.DeliveryFlux, nil
	// "argo" is accepted because it is what people type; the spec value is
	// always the model's "argocd".
	case eject.ModeArgoCD, "argo":
		return model.DeliveryArgoCD, nil
	default:
		return "", fmt.Errorf("unknown --mode %q: eject switches an environment to flux or argocd", flag)
	}
}

// printDryRun shows exactly what a real run would write: every commit in
// replay order with its files, the resulting tip, the updated Environment
// document, and the bootstrap manifests.
func printDryRun(out io.Writer, plan eject.Plan, edit *specEdit, opts *ejectOptions) error {
	var b strings.Builder
	fmt.Fprintf(&b, "eject %s/%s: direct -> %s (dry run)\n", plan.Project, plan.Environment, plan.Mode)
	fmt.Fprintf(&b, "repository: %s (branch %s, path %s)\n\n", plan.Target.Repo, plan.Target.Branch, displayPath(plan.Target.Path))
	fmt.Fprintf(&b, "replaying %d revision(s), oldest first:\n", len(plan.Commits))
	for _, c := range plan.Commits {
		fmt.Fprintf(&b, "  %s  %s  %s\n", c.Revision, c.CommittedAt.Format("2006-01-02T15:04:05Z07:00"), c.Subject)
		fmt.Fprintf(&b, "    author: %s <%s>\n", c.AuthorName, c.AuthorEmail)
		for _, f := range c.Files {
			fmt.Fprintf(&b, "    + %s\n", f.Path)
		}
	}
	tip := plan.Tip()
	fmt.Fprintf(&b, "\ntip after eject: %s (%d file(s)) — byte-identical to the latest recorded revision\n", tip.Revision, len(tip.Files))

	if edit != nil {
		fmt.Fprintf(&b, "\nupdated Environment (%s):\n", specEditNote(edit, opts.write))
		b.WriteString(strings.TrimRight(string(edit.document), "\n"))
		b.WriteString("\n")
	}
	if plan.Bootstrap != nil {
		fmt.Fprintf(&b, "\n%s bootstrap — %s:\n", plan.Bootstrap.Mode, plan.Bootstrap.Summary)
		b.WriteString(strings.TrimRight(string(plan.Bootstrap.YAML), "\n"))
		b.WriteString("\n")
	}
	b.WriteString("\ndry run: nothing was written to the repository or to the spec files\n")
	_, err := io.WriteString(out, b.String())
	return err
}

func specEditNote(edit *specEdit, write bool) string {
	if write {
		return "written to " + edit.file
	}
	return "apply to " + edit.file + ", or re-run with --write"
}

// applySpecEdit either rewrites the spec file or prints the updated document.
func applySpecEdit(out io.Writer, edit *specEdit, write bool) error {
	if edit == nil {
		return nil
	}
	if !write {
		if _, err := fmt.Fprintf(out, "\nupdated Environment (apply to %s, or re-run with --write):\n%s",
			edit.file, ensureTrailingNewline(edit.document)); err != nil {
			return err
		}
		return nil
	}
	if err := os.WriteFile(edit.file, edit.content, 0o600); err != nil {
		return fmt.Errorf("rewriting %s: %w", edit.file, err)
	}
	_, err := fmt.Fprintf(out, "switched %s delivery to %s in %s\n", edit.environment, edit.mode, edit.file)
	return err
}

// emitBootstrap prints or writes the reconciler configuration. It is never
// committed into the delivery path — see internal/delivery/eject/bootstrap.go
// for why that would break the byte-identical-tip guarantee.
func emitBootstrap(out io.Writer, b *eject.Bootstrap, path string) error {
	if b == nil {
		return nil
	}
	if path == "" {
		_, err := fmt.Fprintf(out, "\n%s bootstrap — %s\napply with: kubectl apply -f -\n%s",
			b.Mode, b.Summary, ensureTrailingNewline(b.YAML))
		return err
	}
	if err := os.WriteFile(path, b.YAML, 0o600); err != nil {
		return fmt.Errorf("writing bootstrap manifests: %w", err)
	}
	_, err := fmt.Fprintf(out, "wrote %s bootstrap manifests to %s (apply with kubectl apply -f %s)\n", b.Mode, path, path)
	return err
}

func ensureTrailingNewline(b []byte) string {
	s := string(b)
	if !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	return s
}

func displayPath(p string) string {
	if p == "" {
		return "(repository root)"
	}
	return p
}

func short(s string) string {
	if len(s) > 10 {
		return s[:10]
	}
	return s
}

// --- the spec switch -------------------------------------------------------

// specEdit is the computed delivery-mode switch for one Environment: the file
// it lives in, the rewritten file, and the rewritten document on its own.
type specEdit struct {
	file        string
	environment string
	mode        model.DeliveryMode
	// content is the whole file with the Environment's delivery stanza
	// replaced; document is just that Environment document, for printing.
	content  []byte
	document []byte
}

// planSpecEdit finds the file holding the named Environment and computes the
// rewritten YAML. It does not touch the filesystem.
func planSpecEdit(files []string, envName string, mode model.DeliveryMode, target git.Target) (*specEdit, error) {
	for _, f := range files {
		data, err := os.ReadFile(filepath.Clean(f))
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", f, err)
		}
		content, document, found, err := switchDelivery(data, envName, mode, target)
		if err != nil {
			return nil, fmt.Errorf("rewriting %s: %w", f, err)
		}
		if found {
			return &specEdit{file: f, environment: envName, mode: mode, content: content, document: document}, nil
		}
	}
	return nil, fmt.Errorf("no Environment document named %q found in %s", envName, strings.Join(files, ", "))
}

// switchDelivery replaces the delivery stanza of the named Environment
// document in a YAML stream.
//
// It edits the node tree rather than round-tripping through the model structs:
// decoding into model.Environment and re-encoding would drop comments and
// reorder keys, which turns a one-stanza change into an unreviewable diff of
// somebody's hand-written spec. Everything except spec.delivery is copied
// through untouched.
func switchDelivery(data []byte, envName string, mode model.DeliveryMode, target git.Target) (content, document []byte, found bool, err error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var docs []*yaml.Node
	var edited *yaml.Node
	for {
		var doc yaml.Node
		decodeErr := dec.Decode(&doc)
		if errors.Is(decodeErr, io.EOF) {
			break
		}
		if decodeErr != nil {
			return nil, nil, false, decodeErr
		}
		if !found && isEnvironmentDoc(&doc, envName) {
			if err := setDelivery(&doc, mode, target); err != nil {
				return nil, nil, false, err
			}
			found = true
			edited = &doc
		}
		docs = append(docs, &doc)
	}
	if !found {
		return nil, nil, false, nil
	}
	content, err = encodeDocs(docs)
	if err != nil {
		return nil, nil, false, err
	}
	document, err = encodeDocs([]*yaml.Node{edited})
	if err != nil {
		return nil, nil, false, err
	}
	return content, document, true, nil
}

func encodeDocs(docs []*yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	for _, d := range docs {
		if err := enc.Encode(d); err != nil {
			return nil, err
		}
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func isEnvironmentDoc(doc *yaml.Node, name string) bool {
	root := documentRoot(doc)
	if root == nil {
		return false
	}
	if scalarValue(mapValue(root, "kind")) != "Environment" {
		return false
	}
	return scalarValue(mapValue(mapValue(root, "metadata"), "name")) == name
}

// setDelivery replaces (or inserts) spec.delivery. An existing stanza is
// replaced in place so the key keeps its position and its comment.
func setDelivery(doc *yaml.Node, mode model.DeliveryMode, target git.Target) error {
	root := documentRoot(doc)
	spec := mapValue(root, "spec")
	if spec == nil || spec.Kind != yaml.MappingNode {
		return errors.New("the Environment document has no spec mapping")
	}
	value := &yaml.Node{}
	if err := value.Encode(model.Delivery{
		Mode: mode,
		Git: &model.GitTarget{
			Repo:   target.Repo,
			Branch: target.Branch,
			Path:   target.Path,
		},
	}); err != nil {
		return err
	}
	setMapValue(spec, "delivery", value)
	return nil
}

func documentRoot(doc *yaml.Node) *yaml.Node {
	if doc.Kind == yaml.DocumentNode && len(doc.Content) == 1 {
		return doc.Content[0]
	}
	return doc
}

func mapValue(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

func setMapValue(n *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			// Preserve the comments attached to the replaced value; they
			// document the stanza, not the mode.
			value.HeadComment = n.Content[i+1].HeadComment
			value.FootComment = n.Content[i+1].FootComment
			n.Content[i+1] = value
			return
		}
	}
	n.Content = append(n.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		value)
}

func scalarValue(n *yaml.Node) string {
	if n == nil || n.Kind != yaml.ScalarNode {
		return ""
	}
	return n.Value
}
