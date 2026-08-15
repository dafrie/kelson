package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1/kelsonv1alpha1connect"
	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/delivery/kube"
	"github.com/dafrie/kelson/internal/model"
	"github.com/dafrie/kelson/internal/observation"
	"github.com/dafrie/kelson/internal/renderer"
)

// newDeployCmd builds `kelson deploy` (issue #135, R2 #225): write a spec and
// follow the controller until it settles.
//
// # Deploying is a write and a watch now (ADR-0028)
//
// There is no local applier any more: kelson-controller reconciles the
// Environment this command writes — render, publish an immutable OCI
// artifact, apply the Flux pair — and reports every step on its status
// (internal/api/deploy.go). So this command is a thin client of
// DeployService.Deploy: it addresses the spec, previews it (dry_run=RENDER,
// which touches nothing) so a human confirms a real number rather than a
// promise, then streams the real deploy's Committed/Transition/Settled events
// as they arrive.
//
// # Two ways to address the spec, because the request has one oneof
//
// `-f spec.yaml` sends the documents inline, exactly like `render` and
// `diff`. `--project <name>` deploys a spec already stored on kelson-server —
// what `kelson promote` just wrote, or what an agent's put_spec stored — with
// nothing to send but the environment name. resolveSpecRef (specdocs.go) is
// the one place that resolves which of the two a command meant.
func newDeployCmd() *cobra.Command {
	opts := &deployOptions{}
	cmd := &cobra.Command{
		Use:   "deploy (-f spec.yaml | --project <name>) --env <name>",
		Short: "Write a spec and follow the controller until it settles",
		Long: "Deploy addresses a spec — inline `-f` documents or a `--project` kelson-server already\n" +
			"holds — writes it, and follows kelson-controller's status until the deployment settles or\n" +
			"--timeout expires (ADR-0028, issue #225).\n\n" +
			"There is nothing left to apply directly: the controller renders, publishes an immutable OCI\n" +
			"artifact and lets Flux reconcile it, so this command only writes the spec and watches. It\n" +
			"talks to kelson-server, never to the cluster directly — point --server at one, or forward a\n" +
			"port to it in-cluster (docs/server.md).",
		Example: "  kelson deploy -f project.yaml -f production.yaml --env production --server http://127.0.0.1:8420\n" +
			"  kelson deploy --project shop --env production --timeout 10m --yes\n" +
			"  kelson deploy -f spec.yaml --env development --profile from-cluster",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDeploy(cmd, opts)
		},
	}
	f := cmd.Flags()
	f.StringArrayVarP(&opts.files, "file", "f", nil, "spec YAML file holding Project and/or Environment documents (repeatable)")
	f.StringVar(&opts.project, "project", "", "name of a project already stored on kelson-server (alternative to -f)")
	f.StringVar(&opts.env, "env", "", "name of the Environment to deploy (optional with -f when the input holds exactly one; required with --project)")
	f.StringVar(&opts.profile, "profile", "", "ClusterProfile YAML file, or from-cluster to capture kelson-server's own cluster (used for the render)")
	f.StringVar(&opts.image, "image", "", imageFlagUsage)
	f.DurationVar(&opts.timeout, "timeout", defaultDeployTimeout, "budget for the deployment to reach a settled phase")
	f.BoolVar(&opts.yes, "yes", false, "do not ask for confirmation before applying (already the default when stdin is not a terminal)")
	addServerFlags(cmd, &opts.server)
	return cmd
}

// defaultDeployTimeout is the budget a deployment gets to reach a settled
// phase. It matches statemachine.DefaultTimeout so the CLI does not invent a
// second number for the same question.
const defaultDeployTimeout = 5 * time.Minute

type deployOptions struct {
	files   []string
	project string
	env     string
	profile string
	image   string
	timeout time.Duration
	yes     bool
	server  serverOptions
}

func runDeploy(cmd *cobra.Command, opts *deployOptions) error {
	if opts.timeout <= 0 {
		return fmt.Errorf("--timeout must be positive, got %s", opts.timeout)
	}
	ref, env, err := resolveSpecRef(opts.files, opts.project, opts.env)
	if err != nil {
		return err
	}
	profile, err := inlineProfileRef(opts.profile)
	if err != nil {
		return err
	}
	client, addr := opts.server.deployClient()

	proposed, err := deployPreview(cmd.Context(), client, ref, env, profile, opts.image, addr)
	if err != nil {
		return err
	}

	out := &printer{w: cmd.OutOrStdout()}
	out.printf("%s %s/%s: %d resource(s)\n", padPhase("Proposed"), proposed.GetProject(), proposed.GetEnvironment(), proposed.GetResources())
	if err := out.err; err != nil {
		return err
	}

	if !opts.yes && interactive(cmd) {
		ok, err := confirm(cmd, fmt.Sprintf("Deploy %d resource(s) to %s/%s?", proposed.GetResources(), proposed.GetProject(), proposed.GetEnvironment()))
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("deploy cancelled")
		}
	}

	// The stream's own budget is the deployment's --timeout (the server times
	// its own watch identically, deployBudget in internal/api/deploy.go); the
	// dial timeout on top is headroom for the request to reach the server at
	// all, not part of the deployment's own clock.
	ctx, cancel := context.WithTimeout(cmd.Context(), opts.timeout+dialTimeout)
	defer cancel()
	req := connect.NewRequest(&kelsonv1alpha1.DeployRequest{
		Spec:           ref,
		Environment:    env,
		Profile:        profile,
		Image:          opts.image,
		TimeoutSeconds: int64(opts.timeout.Seconds()),
		DryRun:         kelsonv1alpha1.DryRun_DRY_RUN_NONE,
		IdempotencyKey: newIdempotencyKey(),
	})
	stream, err := client.Deploy(ctx, req)
	if err != nil {
		return serverError("deploy", addr, err)
	}
	defer func() { _ = stream.Close() }()

	var failure *kelsonv1alpha1.Error
	for stream.Receive() {
		switch event := stream.Msg().GetEvent().(type) {
		case *kelsonv1alpha1.DeployResponse_Committed_:
			out.printf("%s revision %s (%s)\n", padPhase("Committed"), event.Committed.GetRevision(), event.Committed.GetAdapter())
		case *kelsonv1alpha1.DeployResponse_Transition_:
			out.printf("%s %s\n", padPhase(event.Transition.GetPhase()), transitionDetail(event.Transition))
		case *kelsonv1alpha1.DeployResponse_Settled_:
			out.printf("%s %s\n", padPhase("Settled"), transitionDetail(event.Settled.GetFinal()))
			failure = event.Settled.GetError()
		}
	}
	if err := stream.Err(); err != nil {
		return serverError("deploy", addr, err)
	}
	if err := out.err; err != nil {
		return err
	}
	if failure != nil {
		return settledErr(failure)
	}
	return nil
}

// deployPreview runs the offline rung (dry_run=RENDER) to learn what a real
// deploy would do before asking a human to confirm it: the Proposed event
// carries the resource count, and the stream ends there having written
// nothing (internal/api/deploy.go). A confirmation prompt that could only ask
// "proceed?" with no number behind it would not be a confirmation.
func deployPreview(ctx context.Context, client kelsonv1alpha1connect.DeployServiceClient, ref *kelsonv1alpha1.SpecRef, env string, profile *kelsonv1alpha1.ProfileRef, image, addr string) (*kelsonv1alpha1.DeployResponse_Proposed, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req := connect.NewRequest(&kelsonv1alpha1.DeployRequest{
		Spec:        ref,
		Environment: env,
		Profile:     profile,
		Image:       image,
		DryRun:      kelsonv1alpha1.DryRun_DRY_RUN_RENDER,
	})
	stream, err := client.Deploy(ctx, req)
	if err != nil {
		return nil, serverError("deploy preview", addr, err)
	}
	defer func() { _ = stream.Close() }()

	var proposed *kelsonv1alpha1.DeployResponse_Proposed
	for stream.Receive() {
		if p, ok := stream.Msg().GetEvent().(*kelsonv1alpha1.DeployResponse_Proposed_); ok {
			proposed = p.Proposed
		}
	}
	if err := stream.Err(); err != nil {
		return nil, serverError("deploy preview", addr, err)
	}
	if proposed == nil {
		return nil, fmt.Errorf("deploy preview: kelson-server at %s sent no proposal", addr)
	}
	return proposed, nil
}

// transitionDetail renders one Transition (or a Settled event's final one) the
// way the pre-rebuild CLI rendered a statemachine.State: the answer, "stuck"
// when the engine gave up waiting for progress, and the cause when there is
// one.
func transitionDetail(t *kelsonv1alpha1.DeployResponse_Transition) string {
	if t == nil {
		return ""
	}
	detail := t.GetAnswer()
	if t.GetStuck() {
		detail = "stuck"
	}
	if c := t.GetCause(); c != nil && c.GetMessage() != "" {
		detail += ": " + c.GetMessage()
	}
	return detail
}

// padPhase keeps the phase column aligned so a deploy log reads as a
// sequence, the same alignment the pre-rebuild CLI used.
func padPhase(phase string) string { return fmt.Sprintf("%-11s", phase) }

// --- the observation plane seam ---------------------------------------------
//
// status and explain still read the cluster directly rather than through
// kelson-server (cmd/kelson/status.go's package doc explains why: the
// workload-health half needs no adapter and no delivery mode, so it never
// needed this rebuild). What follows is unchanged from before R2 and is what
// those two commands share.

// observationTarget is what an observing command resolved from its spec
// files: which component model, in which namespace, against which cluster.
type observationTarget struct {
	kubeconfig  string
	project     string
	environment string
	// namespace is the environment's resolved namespace, used when a rendered
	// manifest carries none of its own.
	namespace string
}

// observationPlane is what the observing commands get for one run.
type observationPlane struct {
	// health is the observation-plane verdict source. Nil when the plane could
	// not build one, which the commands report rather than treat as "healthy".
	health observation.Evaluator
}

// observationConnector builds the plane for a target. It is the seam the
// command tests replace (the real one needs a cluster; none of the wiring
// under test does).
type observationConnector func(observationTarget) (*observationPlane, error)

// connectObservation is the production connector: one cluster connection and a
// workload probe over it.
func connectObservation(t observationTarget) (*observationPlane, error) {
	cluster, err := kube.Connect(t.kubeconfig)
	if err != nil {
		return nil, err
	}
	probe, err := observation.NewProbe(observation.ProbeConfig{
		Client: cluster.Dynamic,
		Logs:   observation.ClientGoLogSource{Client: cluster.Typed},
	})
	if err != nil {
		return nil, err
	}
	return &observationPlane{health: probe}, nil
}

// connectPlane builds the plane for a target, refusing honestly when a build
// was assembled without one.
func connectPlane(connect observationConnector, t observationTarget) (*observationPlane, error) {
	if connect == nil {
		return nil, fmt.Errorf("the observation plane is unavailable in this build")
	}
	return connect(t)
}

// resolveObservationTarget is the shared front half of `status` and `explain`:
// load the spec, render it, and derive what to observe. Keeping it in one
// place is what stops the two commands drifting on what "the current render"
// means.
//
// warn takes the profile's version-skew statements (issue #57); nil silences
// them.
func resolveObservationTarget(in specInput, warn io.Writer) (observationTarget, delivery.ManifestSet, error) {
	project, environment, manifests, _, err := resolveAndRender(in, warn)
	if err != nil {
		return observationTarget{}, delivery.ManifestSet{}, err
	}
	// Resolve again for the namespace: resolveAndRender keeps its signature
	// focused on the render, and model.Resolve is a pure function of inputs
	// that already validated, so this cannot fail or disagree.
	resolved, errs := model.Resolve(project, environment)
	if len(errs) > 0 {
		return observationTarget{}, delivery.ManifestSet{}, errs
	}

	set, err := manifestsToSet(project, environment, manifests)
	if err != nil {
		return observationTarget{}, delivery.ManifestSet{}, err
	}
	set.SpecHash = setSpecHash(manifests)

	return observationTarget{
		kubeconfig:  in.kubeconfig,
		project:     project.Metadata.Name,
		environment: environment.Metadata.Name,
		namespace:   resolved.Environment.Namespace,
	}, set, nil
}

// setSpecHash is the set-level provenance hash carried on ManifestSet.SpecHash:
// a digest of the rendered bytes, in render order.
//
// The renderer stamps a per-component kelson.dev/spec-hash on each resource
// and that is what status correlation compares; this one identifies the whole
// delivered set, which is what history entries and the state machine's
// correlation fallback need. It is deterministic for the same reason the
// render is (ADR-0001): identical spec in, identical bytes out.
func setSpecHash(manifests []renderer.Manifest) string {
	h := sha256.New()
	for _, m := range manifests {
		body, err := m.YAML()
		if err != nil {
			// manifestsToSet encodes the same manifests and reports the error;
			// reaching here means the caller ignored it.
			continue
		}
		h.Write(body)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// --- terminal helpers -------------------------------------------------------

// printer writes a report as a sequence of lines, remembering the first write
// error instead of forcing every call site to check one. The state machine
// prints from a callback that cannot return an error, which is the reason this
// exists rather than checking each write.
type printer struct {
	w   io.Writer
	err error
}

func (p *printer) printf(format string, a ...any) {
	if p.err != nil {
		return
	}
	_, p.err = fmt.Fprintf(p.w, format, a...)
}

// interactive reports whether the command's input is a terminal. A command
// that cannot ask a human must never block on an answer, so a non-terminal
// stdin skips the prompt rather than waiting for input that will never come.
//
// The check is a real terminal probe (an ioctl), not a file-mode sniff:
// /dev/null — what a child process inherits when its parent wires up no
// stdin, which is exactly the unattended case this gate exists for — is a
// character device, so os.ModeCharDevice calls it a terminal and the refusal
// below never fires (the E2E suite caught this as an uninstall that exited 0
// with nobody to answer).
func interactive(cmd *cobra.Command) bool {
	f, ok := cmd.InOrStdin().(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

// confirm asks a yes/no question. Anything but an explicit yes is no, and a
// bare Enter is no as well: an unattended run must not be able to answer
// "yes" by accident. EOF is not an answer at all — a stdin that closes before
// the question is answered is the unattended case wearing a different hat, so
// it is an error the caller surfaces as a non-zero exit, never a quiet no.
func confirm(cmd *cobra.Command, question string) (bool, error) {
	if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s [y/N]: ", question); err != nil {
		return false, err
	}
	var answer string
	if _, err := fmt.Fscanln(cmd.InOrStdin(), &answer); err != nil {
		if errors.Is(err, io.EOF) {
			return false, errors.New("stdin closed before the question was answered: pass --yes to proceed without asking")
		}
		// Fscanln errors on a bare Enter ("unexpected newline"); that is a
		// human declining the default, not a broken stdin.
		return false, nil
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return true, nil
	default:
		return false, nil
	}
}
