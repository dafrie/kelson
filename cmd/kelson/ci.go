package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
)

// `kelson ci` is the pipeline's whole side of the trigger pipeline
// ([ADR-0034](docs/adr/0034-forge-driven-delivery.md) decision 1, issue #248).
//
// # Why the group exists with one verb in it
//
// The verbs under here are the ones a workflow file runs and a human almost
// never does, and that is a different contract from the rest of the CLI: no
// prompts, no colour, no terminal, an exit code that decides whether a job goes
// red. Grouping them says which contract a verb keeps before its flags do —
// the same thing `kelson preview` and `kelson agent` do for their own
// audiences — and a group of one is what a group of several starts as. The
// other end of the same pipeline (a forge webhook, the poll) has no CLI surface
// at all and never will: those are the server's.
//
// # CI is a principal, and this is where that becomes concrete
//
// ADR-0034 decision 6 makes the reporter an agent identity (ADR-0024) rather
// than a holder of the instance's shared password: a scoped, expiring
// credential minted for the pipeline, auditable per ADR-0026, refused for a
// project it was not scoped to. Nothing new is invented here to do it — these
// verbs take the same --server/--password/--token flags every façade-backed
// verb takes (serverclient.go), and --token is the one to use.
func newCICmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ci",
		Short: "Verbs a pipeline runs: report what CI built and let kelson take it from there",
		Long: "The `ci` verbs are the pipeline's side of kelson's trigger pipeline (ADR-0034). They are written\n" +
			"for a workflow file rather than a terminal: they never prompt, they print machine-readable output\n" +
			"on stdout and narration on stderr, and their exit code is the job's.\n\n" +
			"They authenticate as an agent identity, not as the instance's shared password — mint one with\n" +
			"`kelson agent create --project <name> --allow mutate` and pass it as --token (or\n" +
			"$KELSON_AGENT_TOKEN). That is ADR-0034 decision 6: CI was always a principal, and a credential\n" +
			"scoped to one project cannot report for another.",
		Args: cobra.NoArgs,
	}
	cmd.AddCommand(newCIReportBuildCmd())
	return cmd
}

// newCIReportBuildCmd builds `kelson ci report-build` (ADR-0034 decision 3,
// issue #248): a thin ConnectRPC client of `BuildService.ReportBuild`.
//
// # What this command deliberately does not do
//
// It does not render, it does not package, and it does not push. That is the
// whole shrink ADR-0034 performs on ADR-0017 decision 8: CI's contract becomes
// "I built the image, you take it from here", one sentence carrying a commit
// and the digests that exist for it, and the server renders the preview from
// the spec it already holds. So this command needs no checkout of the spec, no
// ClusterProfile, no artifact-registry credential and no cluster access — only
// an address and a credential. `kelson preview publish` is the escape hatch
// that still does all of it, for a runner that cannot reach a kelson server at
// all.
//
// # The exit code is the point
//
// A pipeline's only reliable channel is its status, so the mapping is fixed and
// documented in the help text: a declined report and a failed RPC are non-zero,
// and a report that matched no environment is zero. That last one is not
// leniency — "a report for a ref no environment follows is recorded and
// triggers nothing, which is exactly what a report for a feature branch should
// do" (the schema's own words) — and a job that went red because nobody
// previews the repository would train everyone to ignore it.
func newCIReportBuildCmd() *cobra.Command {
	opts := &reportBuildOptions{}
	cmd := &cobra.Command{
		Use:   "report-build --project <name> --sha <commit> --image <component>=<reference>",
		Short: "Tell kelson which images exist for a commit, and let the server publish from them",
		Long: "Report-build is the CI hand-off of ADR-0034 decision 3: \"I built the image, you take it from\n" +
			"here\". It records the digest-pinned images your pipeline produced for one commit and triggers\n" +
			"kelson's own render → publish, server-side, from the spec kelson already holds. CI never runs\n" +
			"kelson's renderer, never needs a checkout of the spec and never holds the artifact-registry\n" +
			"credential.\n\n" +
			"It is what a project with `spec.build.by: ci` uses. A project whose images come from kelson's\n" +
			"own build plane (`by: kelson`, the default for a project with `source:`) declines the report\n" +
			"naming the field, because publishing it would publish over that plane.\n\n" +
			"Give it --pr to publish that change request's previews. A report with only --ref targets the\n" +
			"environments that track it (`autoDeploy`, ADR-0034 decision 4), which is not in the model yet:\n" +
			"the server answers that plainly rather than accepting a report it would drop.\n\n" +
			"Authentication is an agent credential (ADR-0034 decision 6): `kelson agent create ci --project\n" +
			"<name> --allow mutate` and pass its token as --token or $KELSON_AGENT_TOKEN. The instance's\n" +
			"shared password works too, but a pipeline that holds it holds everything.\n\n" +
			"Exit codes: 0 when the report was accepted — including when it matched no environment, so a\n" +
			"repository nothing previews does not fail your job — and 1 when kelson declined the report or\n" +
			"the call failed. Each identifier the report triggered is printed on its own line on stdout;\n" +
			"kelson's explanation of why they are what they are goes to stderr.",
		Example: "  kelson ci report-build --project checkout --sha \"$GITHUB_SHA\" \\\n" +
			"    --image web=ghcr.io/acme/checkout-web@sha256:abc --pr 412\n" +
			"  kelson ci report-build --project checkout --sha \"$GITHUB_SHA\" \\\n" +
			"    --image web=ghcr.io/acme/checkout-web@sha256:abc --image worker=ghcr.io/acme/checkout-worker@sha256:def \\\n" +
			"    --ref refs/heads/main",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runReportBuild(cmd, opts)
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.project, "project", "", "name of the project, as stored on kelson-server")
	f.StringVar(&opts.sha, "sha", "", "the commit the images were built from, as a full 40-character hexadecimal SHA")
	f.StringArrayVar(&opts.images, "image", nil,
		"an image the build produced, as `component=reference`, digest-pinned — e.g. web=ghcr.io/acme/web@sha256:abc (repeatable)")
	f.Int32Var(&opts.pr, "pr", 0, "change request number this build is for, as the forge numbers it; publishes that change request's previews")
	f.StringVar(&opts.ref, "ref", "", "the branch or tag the commit was built from, e.g. refs/heads/main; selects the environments that track it")
	f.DurationVar(&opts.timeout, "timeout", defaultPublishTimeout, "budget for the call, which includes the server's render and push")
	addServerFlags(cmd, &opts.server)
	cobra.CheckErr(cmd.MarkFlagRequired("project"))
	cobra.CheckErr(cmd.MarkFlagRequired("sha"))
	cobra.CheckErr(cmd.MarkFlagRequired("image"))
	return cmd
}

type reportBuildOptions struct {
	project string
	sha     string
	images  []string
	pr      int32
	ref     string
	timeout time.Duration
	server  serverOptions
}

// runReportBuild sends the report and turns the answer into a pipeline's exit
// code.
//
// The timeout is the publish budget rather than requestTimeout, because this
// unary call is not like the others: requestTimeout bounds calls that "wait on
// no cluster reconcile", and a report renders every matching environment's
// preview and pushes each artifact before it answers. It is the same work
// `kelson preview publish` budgets, so it gets the same budget.
func runReportBuild(cmd *cobra.Command, opts *reportBuildOptions) error {
	if opts.timeout <= 0 {
		return fmt.Errorf("--timeout must be positive, got %s", opts.timeout)
	}
	images, err := parseComponentImages(opts.images)
	if err != nil {
		return err
	}
	client, addr := opts.server.buildClient()

	// What was sent, echoed before it is sent, in the shape `kelson preview
	// publish` echoes its own plan. A job log that contains this can answer
	// "why didn't my preview update" without re-running anything: the commit,
	// the target and the components are the whole of what the report decided.
	sha := strings.TrimSpace(opts.sha)
	plan := &printer{w: cmd.ErrOrStderr()}
	plan.printf("report     %s at %s\n", opts.project, sha)
	plan.printf("target     %s\n", reportTarget(opts))
	plan.printf("components %s\n", reportedComponents(images))
	if err := plan.err; err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), opts.timeout)
	defer cancel()
	res, err := client.ReportBuild(ctx, connect.NewRequest(&kelsonv1alpha1.ReportBuildRequest{
		Project: opts.project,
		Sha:     sha,
		Ref:     strings.TrimSpace(opts.ref),
		Pr:      opts.pr,
		Images:  images,
		// A report's effect is not a function of its arguments — it triggers a
		// publish — so a retry after a dropped connection must be the same
		// report rather than a second one (#71, the proto's own reasoning for
		// the field).
		IdempotencyKey: newIdempotencyKey(),
	}))
	if err != nil {
		return reportError(addr, err)
	}
	return reportOutcome(cmd, res.Msg)
}

// reportTarget names what the report is aimed at, for the echoed plan. The
// third case is a report that says only "these images exist for this commit",
// which is well-formed and reaches the half of the pipeline that is not built
// yet; naming it here rather than only in the refusal means the log shows what
// was asked for beside what came back.
func reportTarget(opts *reportBuildOptions) string {
	if opts.pr > 0 {
		return fmt.Sprintf("change request %d", opts.pr)
	}
	if ref := strings.TrimSpace(opts.ref); ref != "" {
		return ref + " (the environments tracking it)"
	}
	return "none named — no --pr and no --ref"
}

// reportError turns a failed ReportBuild into what a pipeline's log shows.
// Every code but one goes to serverError, which is the vocabulary every
// façade-backed verb already speaks and which carries the server's taxonomy
// code and remediation through verbatim.
//
// Unimplemented is the exception, and it is the reason this function exists. A
// report with no --pr is well-formed, was understood, and named images kelson
// had no complaint about; what it reached is a half of the trigger pipeline
// that does not exist yet (`autoDeploy`, ADR-0034 decision 4, #248).
// serverError's default branch would render that identically to a spec kelson
// could not use or a registry that refused the push, and a pipeline author
// would go looking for a mistake they did not make. So it is framed as a gap in
// kelson, with the server's own sentence and its `delivery/not-implemented`
// detail underneath, unedited.
func reportError(addr string, err error) error {
	if connect.CodeOf(err) != connect.CodeUnimplemented {
		return serverError("report-build", addr, err)
	}
	msg := "report-build: nothing is wrong with this report — kelson has not built the half of the pipeline " +
		"it needs yet.\n  " + connectMessage(err)
	if detail := wireErrorDetails(err); detail != "" {
		msg += detail
	}
	return errors.New(msg)
}

// reportOutcome prints the answer and decides the exit code.
//
// The two channels are the ones `kelson build` and `kelson preview publish`
// already keep: stdout carries only what a later step consumes — one triggered
// identifier per line — and every word of explanation goes to stderr. A
// pipeline can therefore read the whole of what happened out of the stream it
// captured, without parsing prose that is written for a human.
func reportOutcome(cmd *cobra.Command, res *kelsonv1alpha1.ReportBuildResponse) error {
	if !res.GetAccepted() {
		// The schema pins `accepted: false` to exactly one meaning: a report
		// kelson understood and declined to act on. It is a spec-versus-pipeline
		// disagreement about who builds this project's images, so it fails the
		// job — a pipeline reporting into a project that builds its own images
		// is reporting into the void, and every run would do it again.
		return fmt.Errorf("report-build: kelson declined this report. %s", orUnexplained(res.GetMessage()))
	}

	out := &printer{w: cmd.OutOrStdout()}
	for _, id := range res.GetTriggered() {
		out.printf("%s\n", id)
	}
	if err := out.err; err != nil {
		return err
	}

	// Accepted with nothing triggered is an ordinary answer, and the server's
	// message is the whole of why — which environments matched, or that none did
	// and what would have. ADR-0034 names "why didn't my preview update" as the
	// question this pipeline must stay answerable for, so the explanation is
	// printed whether or not anything happened.
	note := &printer{w: cmd.ErrOrStderr()}
	if len(res.GetTriggered()) == 0 {
		note.printf("recorded, and nothing was triggered. %s\n", orUnexplained(res.GetMessage()))
		return note.err
	}
	if msg := res.GetMessage(); msg != "" {
		note.printf("%s\n", msg)
	}
	return note.err
}

// orUnexplained keeps a silent server from becoming a silent command. A
// response that says neither what it did nor why is a server bug, and reporting
// it as one is more useful than printing an empty line where the reason goes.
func orUnexplained(message string) string {
	if strings.TrimSpace(message) == "" {
		return "kelson-server gave no reason, which it is supposed to; report this."
	}
	return message
}

// parseComponentImages turns the repeated --image values into the request's
// component→reference map.
//
// This is the one judgement the CLI makes about a report rather than passing
// through: `component=reference` is a spelling that exists only at this
// boundary, so nothing on the far side could refuse a malformed one usefully.
// Everything the *contents* can be wrong about — a truncated SHA, a mutable
// tag, a component the Project never declared — is refused server-side with a
// `report/*` code and a remediation, and re-implementing those checks here
// would put two authorities on one rule and drift them.
func parseComponentImages(values []string) (map[string]string, error) {
	images := make(map[string]string, len(values))
	for _, value := range values {
		component, reference, ok := strings.Cut(value, "=")
		component, reference = strings.TrimSpace(component), strings.TrimSpace(reference)
		if !ok || component == "" || reference == "" {
			return nil, fmt.Errorf("--image %q is not component=reference: name the component the image is "+
				"for and the digest-pinned reference your push resolved to, e.g. "+
				"--image web=ghcr.io/acme/checkout-web@sha256:abc123", value)
		}
		if existing, seen := images[component]; seen {
			// Silently keeping one of them would publish an artifact naming an
			// image the pipeline did not think it reported, and which one it was
			// would depend on flag order.
			return nil, fmt.Errorf("--image names component %q twice, as %s and %s: report one image per "+
				"component, since a component runs one of them", component, existing, reference)
		}
		images[component] = reference
	}
	return images, nil
}

// reportedComponents lists the components a request carries, sorted, for a
// message a human reads. It is here rather than inlined so the order is fixed:
// a map's is not.
func reportedComponents(images map[string]string) string {
	names := make([]string, 0, len(images))
	for name := range images {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
