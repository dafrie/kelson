package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/delivery/direct"
	"github.com/dafrie/kelson/internal/delivery/flux"
	"github.com/dafrie/kelson/internal/delivery/git"
	"github.com/dafrie/kelson/internal/delivery/kube"
	"github.com/dafrie/kelson/internal/delivery/rollback"
	"github.com/dafrie/kelson/internal/delivery/statemachine"
	"github.com/dafrie/kelson/internal/model"
	"github.com/dafrie/kelson/internal/observation"
	"github.com/dafrie/kelson/internal/renderer"
)

// newDeployCmd builds `kelson deploy` (issue #135): the command that finally
// joins the four planes end to end — render the spec, select the environment's
// delivery adapter, apply, and report the deployment state machine's phases as
// they happen.
//
// # Why the plane is built behind a seam
//
// The command plane's lint allow-list forbids the Kubernetes client libraries
// (.golangci.yml, the `main` depguard rule), so nothing here may construct a
// dynamic client. deliveryConnector is the same seam `kelson diff` uses for its
// L2 engine: production passes connectDelivery, which assembles the registry
// behind the delivery plane, and tests pass a connector backed by fake adapters
// so command wiring is testable with no cluster.
func newDeployCmd() *cobra.Command { return newDeployCmdFactory(connectDelivery) }

func newDeployCmdFactory(connect deliveryConnector) *cobra.Command {
	opts := &deployOptions{connect: connect}
	cmd := &cobra.Command{
		Use:   "deploy -f spec.yaml --env <name>",
		Short: "Render a spec and make it live through the environment's delivery adapter",
		Long: "Deploy renders the spec, hands the rendered manifests to the adapter selected by the\n" +
			"environment's delivery mode, and follows the deployment until it settles.\n\n" +
			"Rendering is the same pure function `kelson render` runs — the adapter never influences it\n" +
			"(ADR-0001). Phases print as they happen, and the command exits 0 only when the deployment\n" +
			"reaches a healthy terminal phase within --timeout.",
		Example: "  kelson deploy -f project.yaml -f production.yaml --env production --profile from-cluster\n" +
			"  kelson deploy -f spec.yaml --env development --timeout 10m --yes",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDeploy(cmd, opts)
		},
	}
	f := cmd.Flags()
	f.StringArrayVarP(&opts.files, "file", "f", nil, "spec YAML file holding Project and/or Environment documents (repeatable)")
	f.StringVar(&opts.env, "env", "", "name of the Environment to deploy (optional when the input holds exactly one)")
	f.StringVar(&opts.profile, "profile", "", "ClusterProfile YAML file, or from-cluster to capture a live profile (requires cluster access)")
	f.StringVar(&opts.kubeconfig, "kubeconfig", "", "path to a kubeconfig (default: $KUBECONFIG, in-cluster credentials, then ~/.kube/config)")
	f.StringVar(&opts.mode, "mode", "", "delivery adapter to use, overriding the environment's delivery mode (direct or flux)")
	f.StringVar(&opts.image, "image", "", imageFlagUsage)
	f.DurationVar(&opts.timeout, "timeout", defaultDeployTimeout, "budget for the deployment to reach a healthy phase")
	f.BoolVar(&opts.yes, "yes", false, "do not ask for confirmation before applying (already the default when stdin is not a terminal)")
	f.StringVar(&opts.history, "history", "", "kelson data directory holding the direct-mode rendered history (default: $KELSON_DATA_DIR, else $XDG_DATA_HOME/kelson)")
	cobra.CheckErr(cmd.MarkFlagRequired("file"))
	return cmd
}

// defaultDeployTimeout is the budget a deployment gets to reach a healthy
// phase. It matches statemachine.DefaultTimeout so the CLI does not invent a
// second number for the same question.
const defaultDeployTimeout = 5 * time.Minute

// deployPollInterval is how often the adapter is asked for its status while a
// deployment is in flight. The state machine is event-driven and owns the only
// timeout; adapters that expose no watch are sampled through statemachine.Poll,
// which is what this interval feeds.
const deployPollInterval = 2 * time.Second

type deployOptions struct {
	specInput
	mode    string
	history string
	timeout time.Duration
	yes     bool
	connect deliveryConnector
}

func runDeploy(cmd *cobra.Command, opts *deployOptions) error {
	target, set, err := resolveDeliveryTarget(opts.specInput, opts.history, opts.mode)
	if err != nil {
		return err
	}
	if opts.timeout <= 0 {
		return fmt.Errorf("--timeout must be positive, got %s", opts.timeout)
	}

	out := &printer{w: cmd.OutOrStdout()}
	out.printf("%s %s/%s: %d resources, mode %s\n", padPhase(delivery.PhaseProposed), set.Project, set.Environment, len(set.Manifests), target.mode)
	if err := out.err; err != nil {
		return err
	}

	if !opts.yes && interactive(cmd) {
		ok, err := confirm(cmd, fmt.Sprintf("Apply %d resources to %s/%s?", len(set.Manifests), set.Project, set.Environment))
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("deploy cancelled")
		}
	}

	adapter, _, err := selectAdapter(opts.connect, target)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), opts.timeout)
	defer cancel()

	res, err := adapter.Apply(ctx, set)
	if err != nil {
		return err
	}
	if !res.Applied {
		return fmt.Errorf("%s: the adapter did not complete the apply for %s/%s", adapter.Name(), set.Project, set.Environment)
	}
	set.Revision = res.Revision
	out.printf("%s revision %s committed by %s\n", padPhase(delivery.PhaseCommitted), res.Revision, adapter.Name())

	state, err := watchDeployment(ctx, adapter, set, opts.timeout, out)
	if err != nil {
		return err
	}
	if err := out.err; err != nil {
		return err
	}
	if state.Phase == delivery.PhaseHealthy {
		return nil
	}
	return notHealthy(state, set, opts.timeout)
}

// watchDeployment drives the deployment state machine off the adapter's status
// and prints every phase change as it happens. The engine is the single owner
// of the transitions (issue #37): the CLI only renders them, so `kelson deploy`
// and a future UI cannot disagree about what a phase means.
func watchDeployment(ctx context.Context, adapter delivery.Adapter, set delivery.ManifestSet, timeout time.Duration, out *printer) (statemachine.State, error) {
	last := delivery.PhaseCommitted
	lastStuck := false
	engine, err := statemachine.New(statemachine.Config{
		Target:    statemachine.TargetFromSet(set),
		Source:    adapterSource(adapter, set, deployPollInterval),
		Timeout:   timeout,
		Component: adapter.Name(),
		OnState: func(s statemachine.State) {
			if s.Phase == last && s.Stuck == lastStuck {
				return
			}
			last, lastStuck = s.Phase, s.Stuck
			out.printf("%s %s\n", padPhase(s.Phase), phaseDetail(s))
		},
	})
	if err != nil {
		return statemachine.State{}, err
	}

	state, err := engine.Run(ctx)
	switch {
	case err == nil:
		return state, nil
	case ctx.Err() != nil:
		// The budget expired. That is an answer ("not healthy in time"), not a
		// machinery failure — the same answer the engine's own progress timer
		// gives, and the two expire together when nothing progresses, so which
		// fires first is scheduler jitter. Ask the engine for the stuck verdict
		// either way: it names the phase-specific cause the caller reports.
		return engine.MarkStuck(), nil
	default:
		return state, err
	}
}

// adapterSource feeds the state machine from an adapter's Status.
//
// It opens with a synthetic Committed observation because kelson watched the
// commit itself: Apply has already returned. That matters mechanically as well
// as semantically — Proposed may only be followed by Committed or Rejected, so
// an adapter that still reports Proposed (direct mode before the applied
// objects read back, a Git mode before the reconciler picks the commit up) is
// forwarded as Committed with its cause intact. Anything else would be a
// backwards transition the engine rejects as an adapter bug.
func adapterSource(adapter delivery.Adapter, set delivery.ManifestSet, interval time.Duration) statemachine.Source {
	poll := statemachine.Poll(statemachine.ObserverFunc(func(ctx context.Context) (delivery.Status, error) {
		st, err := adapter.Status(ctx, set)
		if err != nil {
			return delivery.Status{}, err
		}
		if st.Phase == delivery.PhaseProposed {
			st.Phase = delivery.PhaseCommitted
		}
		if st.Revision == "" {
			st.Revision = set.Revision
		}
		return st, nil
	}), interval)

	return statemachine.SourceFunc(func(ctx context.Context, out chan<- delivery.Status) error {
		committed := delivery.Status{Phase: delivery.PhaseCommitted, Revision: set.Revision}
		if err := statemachine.Send(ctx, out, committed); err != nil {
			return err
		}
		return poll.Watch(ctx, out)
	})
}

// notHealthy turns a settled-but-not-healthy deployment into the command's
// error. The state machine's own structured error is preferred: it distinguishes
// rejected from degraded from never-picked-up, which is the whole point of
// keeping those three answers apart.
func notHealthy(state statemachine.State, set delivery.ManifestSet, timeout time.Duration) error {
	if err := state.Err(); err != nil {
		return err
	}
	return fmt.Errorf("%s/%s did not reach a healthy phase within %s: %s",
		set.Project, set.Environment, timeout, state.String())
}

func phaseDetail(s statemachine.State) string {
	detail := string(s.Answer())
	if s.Stuck {
		detail = "stuck"
	}
	if !s.Cause.IsZero() {
		detail += ": " + s.Cause.String()
	}
	return detail
}

// padPhase keeps the phase column aligned so a deploy log reads as a sequence.
func padPhase(p delivery.Phase) string { return fmt.Sprintf("%-11s", p) }

// --- the delivery plane seam ------------------------------------------------

// deliveryTarget is what a delivery command resolved from its spec files: which
// application model, in which mode, against which cluster and history.
type deliveryTarget struct {
	kubeconfig  string
	history     string
	project     string
	environment string
	// namespace is the environment's resolved namespace, used when a rendered
	// manifest carries none of its own.
	namespace string
	mode      string
	git       *model.GitTarget
}

// deliveryPlane is the assembled delivery plane for one command run.
type deliveryPlane struct {
	// registry holds the adapters registered for this run. Selection is by
	// delivery mode (delivery.Registry.Select).
	registry *delivery.Registry
	// health is the observation-plane verdict source, used by `kelson status`.
	// Nil when the plane could not build one.
	health observation.Evaluator
	// recorded supplies the manifests a rollback preview compares. Nil for a
	// mode that keeps no rendered history kelson can read.
	recorded rollback.Source
}

// deliveryConnector builds the delivery plane for a target. It is the seam the
// command tests replace (the real one needs a cluster; none of the wiring
// under test does).
type deliveryConnector func(deliveryTarget) (*deliveryPlane, error)

// connectDelivery is the production connector: one cluster connection, the
// direct adapter over a rendered-history store, the flux adapter when the
// environment names a deployment repository, and an observation probe for
// status verdicts.
//
// This function is the first real caller of delivery.NewRegistry /
// RegisterDirect / RegisterFlux outside tests, which is what issue #135 exists
// to fix.
func connectDelivery(t deliveryTarget) (*deliveryPlane, error) {
	cluster, err := kube.Connect(t.kubeconfig)
	if err != nil {
		return nil, err
	}
	store, err := direct.OpenStore(direct.StoreOptions{Dir: t.history})
	if err != nil {
		return nil, err
	}

	reg := delivery.NewRegistry()
	if _, err := direct.RegisterDirect(reg, direct.Options{
		Client:  cluster.Dynamic,
		Mapper:  cluster.Mapper,
		History: store,
	}); err != nil {
		return nil, err
	}
	if t.git != nil && t.git.Repo != "" {
		if _, err := flux.RegisterFlux(reg, flux.Options{
			Writer: git.Config{
				Target:   git.Target{Repo: t.git.Repo, Branch: t.git.Branch, Path: t.git.Path},
				Mode:     git.ModeCommit,
				Identity: git.IdentityFromEnv(nil),
				Auth:     gitAuth(),
			},
			// Both halves ride the one cluster connection this function
			// already made, rather than a kubectl/flux binary on PATH (#137).
			// The Receiver webhook is the preferred trigger but has no flag to
			// configure it yet, so the annotation patch — what `flux reconcile`
			// does under the hood — is what the CLI wires today.
			Reconciler: flux.AnnotationReconciler{Client: cluster.Dynamic},
			Status:     flux.DynamicStatusReader{Client: cluster.Dynamic},
		}); err != nil {
			return nil, err
		}
	}

	probe, err := observation.NewProbe(observation.ProbeConfig{
		Client: cluster.Dynamic,
		Logs:   observation.ClientGoLogSource{Client: cluster.Typed},
	})
	if err != nil {
		return nil, err
	}

	return &deliveryPlane{
		registry: reg,
		health:   probe,
		recorded: &rollback.DirectSource{Store: store, Project: t.project, Environment: t.environment},
	}, nil
}

// gitAuth reads the delivery credential from the environment. A missing token
// is anonymous, which is correct for local-path and public remotes and fails
// loudly at push time for anything else.
func gitAuth() git.Auth {
	if token := strings.TrimSpace(os.Getenv("KELSON_GIT_TOKEN")); token != "" {
		return git.Token{Token: token}
	}
	return git.Anonymous{}
}

// selectAdapter builds the plane and resolves the delivery mode to an adapter.
// Mode selection stays a property of the Environment spec (delivery.Selector),
// with --mode as the deliberate override.
func selectAdapter(connect deliveryConnector, t deliveryTarget) (delivery.Adapter, *deliveryPlane, error) {
	if connect == nil {
		return nil, nil, fmt.Errorf("the delivery plane is unavailable in this build")
	}
	plane, err := connect(t)
	if err != nil {
		return nil, nil, err
	}
	adapter, err := plane.registry.Select(t.mode)
	if err != nil {
		return nil, nil, fmt.Errorf("delivery mode %q is not available: %w "+
			"(direct always is; flux needs spec.delivery.git on the Environment)", t.mode, err)
	}
	return adapter, plane, nil
}

// resolveDeliveryTarget is the shared front half of deploy, status and
// rollback: load the spec, render it, and derive the delivery target. Keeping
// it in one place is what stops the three commands drifting on what "the
// current render" or "this environment's mode" means.
func resolveDeliveryTarget(in specInput, history, mode string) (deliveryTarget, delivery.ManifestSet, error) {
	project, environment, manifests, _, err := resolveAndRender(in)
	if err != nil {
		return deliveryTarget{}, delivery.ManifestSet{}, err
	}
	// Resolve again for the delivery stanza: resolveAndRender keeps its
	// signature focused on the render, and model.Resolve is a pure function of
	// inputs that already validated, so this cannot fail or disagree.
	resolved, errs := model.Resolve(project, environment)
	if len(errs) > 0 {
		return deliveryTarget{}, delivery.ManifestSet{}, errs
	}

	set, err := manifestsToSet(project, environment, manifests)
	if err != nil {
		return deliveryTarget{}, delivery.ManifestSet{}, err
	}
	set.SpecHash = setSpecHash(manifests)

	dir, err := historyDir(history)
	if err != nil {
		return deliveryTarget{}, delivery.ManifestSet{}, err
	}
	t := deliveryTarget{
		kubeconfig:  in.kubeconfig,
		history:     dir,
		project:     project.Metadata.Name,
		environment: environment.Metadata.Name,
		namespace:   resolved.Environment.Namespace,
		mode:        mode,
		git:         resolved.Environment.Delivery.Git,
	}
	if t.mode == "" {
		t.mode = string(resolved.Environment.Mode)
	}
	return t, set, nil
}

// setSpecHash is the set-level provenance hash carried on ManifestSet.SpecHash:
// a digest of the rendered bytes, in render order.
//
// The renderer stamps a per-application kelson.dev/spec-hash on each resource
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

// historyDir resolves the direct-mode rendered-history location. An explicit
// flag wins, then $KELSON_DATA_DIR, then the XDG data directory. It is a
// per-user location rather than a per-repository one because the history
// records what is live in a cluster, not what is in a checkout.
func historyDir(flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	if dir := strings.TrimSpace(os.Getenv("KELSON_DATA_DIR")); dir != "" {
		return dir, nil
	}
	if base := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); base != "" {
		return filepath.Join(base, "kelson"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locating the kelson data directory: %w (pass --history)", err)
	}
	return filepath.Join(home, ".local", "share", "kelson"), nil
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
func interactive(cmd *cobra.Command) bool {
	f, ok := cmd.InOrStdin().(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// confirm asks a yes/no question. Anything but an explicit yes is no, and EOF
// (a closed or empty stdin) is no as well: an unattended run must not be able
// to answer "yes" by accident.
func confirm(cmd *cobra.Command, question string) (bool, error) {
	if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s [y/N]: ", question); err != nil {
		return false, err
	}
	var answer string
	if _, err := fmt.Fscanln(cmd.InOrStdin(), &answer); err != nil {
		return false, nil
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return true, nil
	default:
		return false, nil
	}
}
