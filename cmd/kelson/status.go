package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/observation"
)

// newStatusCmd builds `kelson status` (issue #135): the answer to "is my change
// live, and if not, why".
//
// Two planes answer that, and both are needed. The delivery adapter reports the
// state-machine phase for the revision, correlated through the provenance the
// renderer stamps — that says whether the change arrived. The observation plane
// classifies the live workloads — that says whether it works, naming the
// kubelet's own reason (CrashLoopBackOff, ImagePullBackOff) rather than a
// generic "deployment failed". A phase without a verdict is the conflation
// issue #53 exists to prevent, so status never prints one without the other.
func newStatusCmd() *cobra.Command { return newStatusCmdFactory(connectDelivery) }

func newStatusCmdFactory(connect deliveryConnector) *cobra.Command {
	opts := &statusOptions{connect: connect}
	cmd := &cobra.Command{
		Use:   "status -f spec.yaml --env <name>",
		Short: "Report whether the rendered spec is live, and the health verdict of its workloads",
		Long: "Status reports the delivery phase of the rendered spec and the observation plane's health\n" +
			"verdict for each workload it declares.\n\n" +
			"The phase says whether the change arrived; the verdict says whether it works, naming the\n" +
			"specific reason (CrashLoopBackOff, ImagePullBackOff, a failing probe) with the container\n" +
			"logs behind it where they are readable.",
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
	f.StringVar(&opts.mode, "mode", "", "delivery adapter to query, overriding the environment's delivery mode (direct or flux)")
	f.StringVar(&opts.image, "image", "", imageFlagUsage)
	f.StringVar(&opts.history, "history", "", "kelson data directory holding the direct-mode rendered history (default: $KELSON_DATA_DIR, else $XDG_DATA_HOME/kelson)")
	cobra.CheckErr(cmd.MarkFlagRequired("file"))
	return cmd
}

type statusOptions struct {
	specInput
	mode    string
	history string
	connect deliveryConnector
}

// runStatus reports and exits 0. A degraded workload is a successful report of
// a bad state, not a failed command: `kelson status` is the tool you reach for
// when something is already broken, and making it exit non-zero would make it
// unusable in the `set -e` scripts that need it most. `kelson deploy` is the
// command that gates on health.
func runStatus(cmd *cobra.Command, opts *statusOptions) error {
	target, set, err := resolveDeliveryTarget(opts.specInput, opts.history, opts.mode, cmd.ErrOrStderr())
	if err != nil {
		return err
	}
	adapter, plane, err := selectAdapter(opts.connect, target)
	if err != nil {
		return err
	}

	st, err := adapter.Status(cmd.Context(), set)
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
	out.printf("%s/%s via %s\n", set.Project, set.Environment, adapter.Name())
	out.printf("%s %s\n", padPhase(st.Phase), phaseSummary(st))
	printSummary(out, st, verdicts)
	printVerdicts(out, plane, verdicts)
	return out.err
}

// printSummary prints the per-resource counters under the phase line.
//
// The degraded counter is not the adapter's alone. The two planes answer
// different questions: the adapter reads a resource's own conditions and says
// whether the rollout of THIS revision finished, while observation classifies
// the pods behind each workload. They disagreed in exactly the case status
// exists for (issue #151) — a Deployment whose new pods CrashLoopBackOff keeps
// the previous ReplicaSet alive, so the adapter reports a rollout still in
// flight and counts "degraded: 0" directly above a verdict reading
// "degraded: crash-loop-back-off".
//
// Neither source sees every resource (the adapter also judges CronJobs; the
// probe classifies only Deployments), so the printed count is the larger of the
// two. That is a lower bound on the union, and it can never under-report a
// resource one of the planes has already called degraded.
func printSummary(out *printer, st delivery.Status, verdicts []observation.Verdict) {
	for _, key := range []string{"resources", "live"} {
		if v, ok := st.Detail[key]; ok {
			out.printf("  %-10s %s\n", key+":", v)
		}
	}
	reported, ok := st.Detail["degraded"]
	n := degradedCount(reported, verdicts)
	if ok || n > 0 {
		out.printf("  %-10s %d\n", "degraded:", n)
	}
}

// degradedCount reconciles the adapter's degraded count with the verdicts. A
// count the adapter did not report (or reported unparseably) contributes
// nothing, which leaves the verdicts as the answer.
func degradedCount(reported string, verdicts []observation.Verdict) int {
	n := 0
	for _, v := range verdicts {
		if isDegraded(v) {
			n++
		}
	}
	if fromAdapter, err := strconv.Atoi(reported); err == nil && fromAdapter > n {
		return fromAdapter
	}
	return n
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
func workloadVerdicts(ctx context.Context, plane *deliveryPlane, set delivery.ManifestSet, namespace string) ([]observation.Verdict, error) {
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

func printVerdicts(out *printer, plane *deliveryPlane, verdicts []observation.Verdict) {
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

// phaseSummary is the one-line explanation next to a phase: the adapter's cause
// when it has one, and otherwise the revision the phase is about.
func phaseSummary(st delivery.Status) string {
	switch {
	case st.Cause != "" && st.Revision != "":
		return fmt.Sprintf("revision %s: %s", st.Revision, st.Cause)
	case st.Cause != "":
		return st.Cause
	case st.Revision != "":
		return "revision " + st.Revision
	default:
		return "no revision recorded"
	}
}
