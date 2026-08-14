package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/delivery/kube"
	"github.com/dafrie/kelson/internal/model"
	"github.com/dafrie/kelson/internal/observation"
	"github.com/dafrie/kelson/internal/renderer"
)

// newDeployCmd builds `kelson deploy`, which is gated (issue #224).
//
// # What was here, and why it is not
//
// This command used to render the spec, select the environment's delivery
// adapter and drive the deployment state machine until it settled. Both
// adapters are gone: [ADR-0028](docs/adr/0028-delivery-spine.md) deleted the
// direct applier and the git writer and replaced them with one path —
// kelson-controller renders, publishes an immutable OCI artifact, and Flux
// reconciles it — which makes deploying an `Environment` you apply rather than
// a command you run.
//
// # It refuses by name rather than disappearing
//
// The command stays, and stays wired, because the alternative teaches the wrong
// thing: `unknown command "deploy"` says kelson never had the verb, and a
// script that runs it would fail with a usage error indistinguishable from a
// typo. A structured [delivery.Error] carrying `delivery/not-implemented` and
// the tracking issue says exactly what happened and when it changes — the same
// discipline internal/model's gate table applies to a field it cannot render
// (notimplemented.go).
func newDeployCmd() *cobra.Command {
	opts := &deployOptions{}
	cmd := &cobra.Command{
		Use:   "deploy -f spec.yaml --env <name>",
		Short: "Make a rendered spec live (rebuilding on the controller — see issue #224)",
		Long: "Deploy is being rebuilt on the delivery spine (ADR-0028, issue #224) and refuses in the\n" +
			"meantime.\n\n" +
			"The path it is being rebuilt on: kelson-controller validates and renders an Environment,\n" +
			"publishes the rendered set as an immutable OCI artifact, and applies the Flux OCIRepository\n" +
			"and Kustomization that reconcile it. Deploying becomes applying an Environment resource\n" +
			"rather than running this command against a spec file.\n\n" +
			"What still works today, unchanged and offline: `kelson render`, `kelson diff`, `kelson build`,\n" +
			"`kelson profile`, `kelson explain` and `kelson status` (the workload half — see their help).",
		Example: "  kelson render -f project.yaml -f production.yaml --env production   # the manifests, offline\n" +
			"  kelson diff -f project.yaml -f production.yaml --env production     # what would change",
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error { return deployUnavailable() },
	}
	f := cmd.Flags()
	f.StringArrayVarP(&opts.files, "file", "f", nil, "spec YAML file holding Project and/or Environment documents (repeatable)")
	f.StringVar(&opts.env, "env", "", "name of the Environment to deploy (optional when the input holds exactly one)")
	f.StringVar(&opts.profile, "profile", "", "ClusterProfile YAML file, or from-cluster to capture a live profile (requires cluster access)")
	f.StringVar(&opts.kubeconfig, "kubeconfig", "", "path to a kubeconfig (default: $KUBECONFIG, in-cluster credentials, then ~/.kube/config)")
	f.StringVar(&opts.image, "image", "", imageFlagUsage)
	f.DurationVar(&opts.timeout, "timeout", defaultDeployTimeout, "budget for the deployment to reach a healthy phase")
	f.BoolVar(&opts.yes, "yes", false, "do not ask for confirmation before applying (already the default when stdin is not a terminal)")
	cobra.CheckErr(cmd.MarkFlagRequired("file"))
	return cmd
}

// deployUnavailable is the refusal every deleted apply path shares.
func deployUnavailable() error {
	return delivery.NotImplemented("deploy",
		"kelson cannot apply a rendered set: the direct applier and the git writer were deleted with "+
			"the old delivery machinery, and the controller that replaces them does not publish yet",
		"#224")
}

// defaultDeployTimeout is the budget a deployment gets to reach a healthy
// phase. It matches statemachine.DefaultTimeout so the CLI does not invent a
// second number for the same question.
const defaultDeployTimeout = 5 * time.Minute

type deployOptions struct {
	specInput
	timeout time.Duration
	yes     bool
}

// --- the observation plane seam ---------------------------------------------

// observationTarget is what an observing command resolved from its spec files:
// which application model, in which namespace, against which cluster.
//
// It used to carry a delivery mode, a git target and a local history directory
// as well, because it also chose an adapter. There is no adapter to choose
// (ADR-0028 decision 9), so what remains is addressing: the rendered set says
// which objects to look at, and this says where.
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
// command tests replace (the real one needs a cluster; none of the wiring under
// test does).
type observationConnector func(observationTarget) (*observationPlane, error)

// connectObservation is the production connector: one cluster connection and a
// workload probe over it.
//
// It used to assemble a delivery registry as well — the direct adapter over a
// JSONL journal, the flux adapter over a git writer — and that is exactly what
// ADR-0028 deleted. What survives is the half that reads the cluster and says
// what it sees, which needs no adapter and no history and is the half the
// commands below are actually built on.
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
// load the spec, render it, and derive what to observe. Keeping it in one place
// is what stops the two commands drifting on what "the current render" means.
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
