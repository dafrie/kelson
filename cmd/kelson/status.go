package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/observation"
)

// newStatusCmd builds `kelson status` (issue #135): the answer to "is my change
// live, and if not, why".
//
// # It answers half of that today, and says which half
//
// Two planes used to answer it. The delivery adapter reported the state-machine
// phase for the revision — whether the change ARRIVED — and the observation
// plane classified the live workloads — whether it WORKS, naming the kubelet's
// own reason (CrashLoopBackOff, ImagePullBackOff) rather than a generic
// "deployment failed".
//
// [ADR-0028](docs/adr/0028-delivery-spine.md) deleted both adapters, and with
// them the phase. The observation half needs no adapter, no history and no
// delivery mode — it reads the live cluster through internal/observation and
// classifies the workloads the current render declares — so it keeps working
// unchanged, and this command keeps working with it. What it does NOT do is
// invent a phase: a "Healthy" or "Unknown" with nothing behind it would be the
// conflation issue #53 exists to prevent, so the missing half is printed as
// missing, once, naming the issue that restores it (#224, then
// `Environment.status` per ADR-0027 decision 6).
//
// The gap is worth carrying rather than gating the command, because the two
// questions fail independently and the surviving one is the one people run this
// command in an incident to ask.
func newStatusCmd() *cobra.Command { return newStatusCmdFactory(connectObservation) }

func newStatusCmdFactory(connect observationConnector) *cobra.Command {
	opts := &statusOptions{connect: connect}
	cmd := &cobra.Command{
		Use:   "status -f spec.yaml --env <name>",
		Short: "Report the health verdict of the workloads the rendered spec declares",
		Long: "Status reports the observation plane's health verdict for each workload the rendered spec\n" +
			"declares: whether it works, naming the specific reason (CrashLoopBackOff, ImagePullBackOff,\n" +
			"a failing probe) with the container logs behind it where they are readable.\n\n" +
			"It does NOT report the delivery phase — whether this revision arrived — because the adapters\n" +
			"that answered that were deleted with the old delivery machinery (ADR-0028). That half returns\n" +
			"with issue #224, read from Environment.status. Until then it is reported as missing rather\n" +
			"than guessed at.",
		Example: "  kelson status -f project.yaml -f production.yaml --env production\n" +
			"  kelson status -f spec.yaml --env development --kubeconfig ./kubeconfig",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runStatus(cmd, opts)
		},
	}
	f := cmd.Flags()
	f.StringArrayVarP(&opts.files, "file", "f", nil, "spec YAML file holding Project and/or Environment documents (repeatable)")
	f.StringVar(&opts.env, "env", "", "name of the Environment to report on (optional when the input holds exactly one)")
	f.StringVar(&opts.profile, "profile", "", "ClusterProfile YAML file, or from-cluster to capture a live profile (requires cluster access)")
	f.StringVar(&opts.kubeconfig, "kubeconfig", "", "path to a kubeconfig (default: $KUBECONFIG, in-cluster credentials, then ~/.kube/config)")
	f.StringVar(&opts.image, "image", "", imageFlagUsage)
	cobra.CheckErr(cmd.MarkFlagRequired("file"))
	return cmd
}

type statusOptions struct {
	specInput
	connect observationConnector
}

// runStatus reports and exits 0. A degraded workload is a successful report of
// a bad state, not a failed command: `kelson status` is the tool you reach for
// when something is already broken, and making it exit non-zero would make it
// unusable in the `set -e` scripts that need it most. `kelson deploy` is the
// command that gates on health.
func runStatus(cmd *cobra.Command, opts *statusOptions) error {
	target, set, err := resolveObservationTarget(opts.specInput, cmd.ErrOrStderr())
	if err != nil {
		return err
	}
	plane, err := connectPlane(opts.connect, target)
	if err != nil {
		return err
	}

	// The verdicts are read before anything is printed because the summary's
	// degraded counter is derived from them (issue #151).
	verdicts, err := workloadVerdicts(cmd.Context(), plane, set, target.namespace)
	if err != nil {
		return err
	}

	out := &printer{w: cmd.OutOrStdout()}
	out.printf("%s/%s in namespace %s\n", set.Project, set.Environment, target.namespace)
	printPhaseGap(out)
	printSummary(out, set, verdicts)
	printVerdicts(out, plane, verdicts)
	return out.err
}

// printPhaseGap states the half of this command that is missing, in the place
// the phase line used to be.
//
// It is printed unconditionally, including when everything is healthy. A gap
// that only announces itself on failure is a gap a reader learns about at the
// worst possible moment, and "kelson did not tell me it wasn't checking" is the
// complaint this line exists to make impossible.
func printPhaseGap(out *printer) {
	out.printf("  delivery phase: not reported — the adapters that answered \"did this revision arrive?\"\n")
	out.printf("                  were deleted with the old delivery machinery (ADR-0028); it returns with\n")
	out.printf("                  issue #224. What follows is workload health only.\n")
}

// printSummary prints the per-resource counters.
//
// The degraded count used to be reconciled with the adapter's own, taking the
// larger of the two, because the planes answered different questions and
// disagreed in exactly the case status exists for (issue #151): a Deployment
// whose new pods CrashLoopBackOff keeps the previous ReplicaSet alive, so the
// adapter reported a rollout still in flight and counted "degraded: 0" directly
// above a verdict reading "degraded: crash-loop-back-off".
//
// There is no second source now, so the verdicts are the count. That is a lower
// bound on what is wrong — the probe classifies Deployments and the adapter also
// judged CronJobs — and printPhaseGap is what keeps the bound from reading as a
// complete answer.
func printSummary(out *printer, set delivery.ManifestSet, verdicts []observation.Verdict) {
	out.printf("  %-10s %d\n", "resources:", len(set.Manifests))
	n := 0
	for _, v := range verdicts {
		if isDegraded(v) {
			n++
		}
	}
	out.printf("  %-10s %d\n", "degraded:", n)
}

// isDegraded applies the same test observation.Verdict.String uses when it
// prints the word "degraded", so the counter and the verdict lines below it
// cannot disagree by construction. Stuck is deliberately excluded: the
// observation plane keeps "made no progress" distinct from "broken".
func isDegraded(v observation.Verdict) bool {
	return !v.Healthy && !v.Stuck && observation.IsFailure(v.Code)
}

// workloadVerdicts evaluates the observation verdict for every workload the
// rendered set declares. The set IS the correlation: these are the resources
// kelson rendered for this project and environment, carrying the provenance the
// adapter just matched against the cluster. No probe means no verdicts, which
// printVerdicts reports as such rather than as "nothing is failing".
//
// The ExternalSecrets come first, and that order is the diagnosis. Under the
// externalSecrets backend a Secret that never synced is why the pods below it
// are stuck, and reading the cause before the symptom is what turns a
// CreateContainerConfigError into an answer (issue #80, ADR-0020).
func workloadVerdicts(ctx context.Context, plane *observationPlane, set delivery.ManifestSet, namespace string) ([]observation.Verdict, error) {
	if plane.health == nil {
		return nil, nil
	}
	var out []observation.Verdict
	// The sync evaluator is an optional capability: an Evaluator that is only a
	// workload probe (a test fake, or a future health source) keeps working and
	// simply reports no sync verdicts, rather than forcing every implementation
	// to grow a method for a backend it may never see.
	if sync, ok := plane.health.(observation.SecretSyncEvaluator); ok {
		for _, w := range externalSecrets(set, namespace) {
			verdict, err := sync.EvaluateSecretSync(ctx, w.namespace, w.name)
			if err != nil {
				return nil, err
			}
			out = append(out, verdict)
		}
	}
	for _, w := range deployments(set, namespace) {
		verdict, err := plane.health.Evaluate(ctx, w.namespace, w.name)
		if err != nil {
			return nil, err
		}
		out = append(out, verdict)
	}
	return out, nil
}

func printVerdicts(out *printer, plane *observationPlane, verdicts []observation.Verdict) {
	if plane.health == nil {
		out.printf("\nno observation probe available: workload health was not read\n")
		return
	}
	if len(verdicts) == 0 {
		return
	}
	out.printf("\n")
	for _, v := range verdicts {
		printVerdict(out, v)
	}
}

// printVerdict renders one workload verdict: the one-line summary (which
// carries the kubelet's own reason verbatim), the remediation, and the failing
// containers with their logs.
func printVerdict(out *printer, v observation.Verdict) {
	out.printf("%s\n", v.String())
	if v.Remediation != "" {
		out.printf("  fix: %s\n", v.Remediation)
	}
	for _, c := range v.Containers {
		out.printf("  container %s in pod %s: %s\n", c.Name, c.Pod, containerReason(c))
		switch {
		case c.LogError != "":
			out.printf("    logs unavailable: %s\n", c.LogError)
		case c.Logs != "":
			for _, line := range strings.Split(strings.TrimRight(c.Logs, "\n"), "\n") {
				out.printf("    | %s\n", line)
			}
		}
	}
}

func containerReason(c observation.Container) string {
	if c.Reason == "" {
		return c.Code.String()
	}
	return fmt.Sprintf("%s (%s)", c.Code, c.Reason)
}

// workloadRef is one resource the observation plane can be asked about.
type workloadRef struct {
	namespace string
	name      string
}

// externalSecrets picks the ExternalSecrets out of a rendered set. They are
// there only under the externalSecrets backend, so every other environment gets
// an empty list and pays nothing (ADR-0020).
func externalSecrets(set delivery.ManifestSet, fallbackNamespace string) []workloadRef {
	var out []workloadRef
	for _, m := range set.Manifests {
		if m.Kind != "ExternalSecret" {
			continue
		}
		ns := m.Namespace
		if ns == "" {
			ns = fallbackNamespace
		}
		out = append(out, workloadRef{namespace: ns, name: m.Name})
	}
	return out
}

// deployments picks the Deployments out of a rendered set. The observation
// probe reads Deployments and the pods in their selector set; other kinds carry
// no health signal it can classify, and reporting "unknown" for them as if it
// were a verdict would be worse than saying nothing.
func deployments(set delivery.ManifestSet, fallbackNamespace string) []workloadRef {
	var out []workloadRef
	for _, m := range set.Manifests {
		if m.Kind != "Deployment" {
			continue
		}
		ns := m.Namespace
		if ns == "" {
			ns = fallbackNamespace
		}
		out = append(out, workloadRef{namespace: ns, name: m.Name})
	}
	return out
}
