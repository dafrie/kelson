package main

import (
	"context"
	"encoding/json"
	"fmt"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1/kelsonv1alpha1connect"
	"github.com/dafrie/kelson/internal/diff"
)

// newRollbackCmd builds `kelson rollback` (ADR-0028 decision 5, R2 #225):
// repoint an environment at a revision it has already published.
//
// # A rollback is a pointer move, previewed before it is ever committed
//
// Every revision kelson publishes is an immutable OCI artifact, so rolling
// back is repointing the environment's OCIRepository at a tag that already
// exists — expressed as a `kelson.dev/rollback-to` annotation the controller
// honours — never a replay of recorded bytes. DeployService.Rollback always
// sends a Preview event first, whatever dry_run is; this command asks with
// dry_run=RENDER first so the preview arrives with nothing yet written,
// prints it, and only then — after a human confirms, or --yes — makes the
// real call with the same target revision pinned, so a second resolution of
// "the previous revision" cannot pick a different one than what was shown.
//
// # The preview says less than it used to, and says so
//
// The irreversibility preview this rebuild has today is one finding: kelson
// cannot fetch two OCI artifacts to compute a byte-level comparison, so it
// says exactly that rather than reporting an empty list that would read as
// "nothing to worry about" (internal/api/deploy.go, rollbackPreviewGap).
//
// A target older than the cluster's bounded history adds a second finding
// (issue #241). Such a revision is confirmed against the registry's tag list
// rather than against `status.history`, so the restore is exactly as exact —
// the artifact is immutable — and everything kelson would otherwise say *about*
// the target is gone. Both findings print under "What a rollback cannot revert",
// which is where a reader is already looking for what they are agreeing to.
func newRollbackCmd() *cobra.Command {
	opts := &rollbackOptions{}
	cmd := &cobra.Command{
		Use:   "rollback (-f spec.yaml | --project <name>) --env <name> [--to <revision>]",
		Short: "Repoint an environment at a revision it has already published",
		Long: "Rollback repoints the environment's published artifact at a revision it has already run\n" +
			"(ADR-0028 decision 5): nothing is re-rendered and nothing is replayed, because every\n" +
			"revision kelson publishes is immutable. With no --to, the target is the revision before\n" +
			"the one currently serving.\n\n" +
			"--to may name a revision older than the bounded history the cluster keeps: the registry\n" +
			"holds every artifact ever published and kelson confirms the target against it, so an\n" +
			"aged-out revision is the same pointer move as a recent one. What it cannot tell you about\n" +
			"such a target — when it was published, what it ran, how that deployment ended — the\n" +
			"preview says out loud (`kelson history` lists them).\n\n" +
			"The preview always comes first, whether or not --yes is set: it names the target revision\n" +
			"and what kelson cannot tell you about it — the byte-level comparison this rebuild does not\n" +
			"compute yet (issue #225).",
		Example: "  kelson rollback -f project.yaml -f production.yaml --env production\n" +
			"  kelson rollback --project shop --env production --to 7-a1b2c3d4 --yes",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRollback(cmd, opts)
		},
	}
	f := cmd.Flags()
	f.StringArrayVarP(&opts.files, "file", "f", nil, "spec YAML file holding Project and/or Environment documents (repeatable)")
	f.StringVar(&opts.project, "project", "", "name of a project already stored on kelson-server (alternative to -f)")
	f.StringVar(&opts.env, "env", "", "name of the Environment to roll back (optional with -f when the input holds exactly one; required with --project)")
	f.StringVar(&opts.to, "to", "", "revision to restore, including one older than the cluster's bounded history (default: the revision before the one currently serving)")
	f.BoolVar(&opts.yes, "yes", false, "apply the rollback without asking for confirmation; the preview is printed either way")
	addServerFlags(cmd, &opts.server)
	return cmd
}

type rollbackOptions struct {
	files   []string
	project string
	env     string
	to      string
	yes     bool
	server  serverOptions
}

func runRollback(cmd *cobra.Command, opts *rollbackOptions) error {
	ref, env, err := resolveSpecRef(opts.files, opts.project, opts.env)
	if err != nil {
		return err
	}
	client, addr := opts.server.deployClient()
	out := &printer{w: cmd.OutOrStdout()}

	preview, err := rollbackPreview(cmd.Context(), client, ref, env, opts.to, addr)
	if err != nil {
		return err
	}
	printRollbackPreview(out, preview)
	if err := out.err; err != nil {
		return err
	}

	if !opts.yes && interactive(cmd) {
		ok, err := confirm(cmd, fmt.Sprintf("Restore revision %s?", preview.GetToRevision()))
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("rollback cancelled")
		}
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), defaultDeployTimeout+dialTimeout)
	defer cancel()
	req := connect.NewRequest(&kelsonv1alpha1.RollbackRequest{
		Spec:        ref,
		Environment: env,
		// The revision pinned from the preview, not opts.to verbatim: an empty
		// --to means "the previous revision", and re-resolving that on a second
		// call could pick a different one if something else deployed between
		// the two requests.
		ToRevision:     preview.GetToRevision(),
		DryRun:         kelsonv1alpha1.DryRun_DRY_RUN_NONE,
		IdempotencyKey: newIdempotencyKey(),
	})
	stream, err := client.Rollback(ctx, req)
	if err != nil {
		return serverError("rollback", addr, err)
	}
	defer func() { _ = stream.Close() }()

	var failure *kelsonv1alpha1.Error
	for stream.Receive() {
		switch event := stream.Msg().GetEvent().(type) {
		case *kelsonv1alpha1.RollbackResponse_Committed_:
			out.printf("%s restored revision %s\n", padPhase("Committed"), event.Committed.GetRestoredRevision())
		case *kelsonv1alpha1.RollbackResponse_Settled_:
			failure = event.Settled.GetError()
			if failure == nil {
				out.printf("%s restored\n", padPhase("Settled"))
			}
		}
	}
	if err := stream.Err(); err != nil {
		return serverError("rollback", addr, err)
	}
	if err := out.err; err != nil {
		return err
	}
	if failure != nil {
		return settledErr(failure)
	}
	return nil
}

// rollbackPreview runs the always-preview rung (dry_run=RENDER): the server
// sends exactly one Preview event and the stream ends there, having written
// nothing.
func rollbackPreview(ctx context.Context, client kelsonv1alpha1connect.DeployServiceClient, ref *kelsonv1alpha1.SpecRef, env, to, addr string) (*kelsonv1alpha1.RollbackResponse_Preview, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req := connect.NewRequest(&kelsonv1alpha1.RollbackRequest{
		Spec:        ref,
		Environment: env,
		ToRevision:  to,
		DryRun:      kelsonv1alpha1.DryRun_DRY_RUN_RENDER,
	})
	stream, err := client.Rollback(ctx, req)
	if err != nil {
		return nil, serverError("rollback preview", addr, err)
	}
	defer func() { _ = stream.Close() }()

	var preview *kelsonv1alpha1.RollbackResponse_Preview
	for stream.Receive() {
		if p, ok := stream.Msg().GetEvent().(*kelsonv1alpha1.RollbackResponse_Preview_); ok {
			preview = p.Preview
		}
	}
	if err := stream.Err(); err != nil {
		return nil, serverError("rollback preview", addr, err)
	}
	if preview == nil {
		return nil, fmt.Errorf("rollback preview: kelson-server at %s sent no preview", addr)
	}
	return preview, nil
}

// printRollbackPreview prints the target and what a rollback cannot revert —
// the report the pre-rebuild CLI printed before every rollback, adapted to
// the server's findings vocabulary (RollbackResponse.Finding) instead of a
// locally computed one.
func printRollbackPreview(out *printer, preview *kelsonv1alpha1.RollbackResponse_Preview) {
	out.printf("Target revision: %s\n", orDash(preview.GetToRevision()))
	if encoded := preview.GetDiffJson(); len(encoded) > 0 {
		var d diff.Diff
		if err := json.Unmarshal(encoded, &d); err == nil {
			out.printf("What changes: %d added, %d modified, %d removed (max risk %s)\n",
				d.Summary.Added, d.Summary.Modified, d.Summary.Removed, d.Summary.MaxRisk)
		}
	}
	out.printf("\nWhat a rollback cannot revert:\n")
	findings := preview.GetFindings()
	if len(findings) == 0 {
		out.printf("  (nothing identified)\n")
		return
	}
	for _, f := range findings {
		marker := "[warning]      "
		if f.GetUnrecoverable() {
			marker = "[unrecoverable]"
		}
		out.printf("  %s %s\n", marker, describeFinding(f))
	}
}

func describeFinding(f *kelsonv1alpha1.RollbackResponse_Finding) string {
	head := f.GetCause()
	if f.GetResource() != "" {
		head = f.GetResource() + " — " + head
	}
	if f.GetPath() != "" {
		head += " at " + f.GetPath()
	}
	if f.GetMessage() == "" {
		return head
	}
	return head + ": " + f.GetMessage()
}
