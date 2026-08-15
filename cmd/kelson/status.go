package main

import (
	"context"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/observation"
)

// newStatusCmd builds `kelson status` (issue #135): the answer to "is my change
// live, and if not, why".
//
// # Two planes, two paths to reach them
//
// The observation half — whether the workloads WORK — reads the live cluster
// directly through internal/observation and classifies the workloads the
// current render declares. It needs no adapter, no history and no server, and
// it has answered this unchanged since before ADR-0028 deleted the old
// delivery adapters.
//
// The delivery half — whether the change ARRIVED — used to come from one of
// those adapters. ADR-0028 deleted them, and until R2 (#225) landed this
// command printed the gap rather than a guess: a "Healthy" or "Unknown" with
// nothing behind it would be the conflation issue #53 exists to prevent.
// `DeployService.Status` now answers it from `Environment.status`
// (internal/api/deploy.go), so this command is a ConnectRPC client of it for
// that half alone — the same --server/--password/--token conventions every
// other façade-backed verb uses (serverclient.go) — while the workload half
// keeps reading the cluster directly, unchanged.
//
// # The delivery half degrades, it never gates
//
// A server that does not answer must not turn a working command into a
// failing one: plenty of `kelson status` runs have no kelson-server up at
// all, and the workload half is the one people run this command in an
// incident to ask — the two questions fail independently, and the surviving
// one matters more. So an unreachable --server, a rejected credential, or any
// other RPC failure is folded into the same "delivery phase" line the missing
// phase used to occupy, naming --server, rather than failing the command.
func newStatusCmd() *cobra.Command { return newStatusCmdFactory(connectObservation) }

func newStatusCmdFactory(connect observationConnector) *cobra.Command {
	opts := &statusOptions{connect: connect}
	cmd := &cobra.Command{
		Use:   "status -f spec.yaml --env <name>",
		Short: "Report the delivery phase and the health verdict of the rendered spec",
		Long: "Status reports two things: whether the change arrived — the delivery phase, read from\n" +
			"DeployService.Status/Environment.status — and whether the workloads it declares work: the\n" +
			"observation plane's verdict for each, naming the specific reason (CrashLoopBackOff,\n" +
			"ImagePullBackOff, a failing probe) with the container logs behind it where they are\n" +
			"readable.\n\n" +
			"The delivery half needs kelson-server (--server/--password/--token, matching every other\n" +
			"façade-backed verb); the workload half reads the cluster directly and keeps working with no\n" +
			"server reachable, reporting that plainly rather than gating the whole command on it.",
		Example: "  kelson status -f project.yaml -f production.yaml --env production\n" +
			"  kelson status -f spec.yaml --env development --kubeconfig ./kubeconfig\n" +
			"  kelson status -f spec.yaml --env production --server http://127.0.0.1:8420",
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
	addServerFlags(cmd, &opts.server)
	return cmd
}

type statusOptions struct {
	specInput
	connect observationConnector
	server  serverOptions
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

	phase := resolveDeliveryPhase(cmd.Context(), opts)

	out := &printer{w: cmd.OutOrStdout()}
	out.printf("%s/%s in namespace %s\n", set.Project, set.Environment, target.namespace)
	printPhase(out, phase)
	printSummary(out, set, verdicts)
	printVerdicts(out, plane, verdicts)
	return out.err
}

// resolveDeliveryPhase asks kelson-server's DeployService.Status for the
// delivery half this command otherwise cannot answer: whether this revision
// arrived, its phase and cause, read straight from Environment.status
// (internal/api/deploy.go's reportDelivery).
//
// It never returns an error: a server that does not answer, or refuses this
// process's credential, is reported through the response's Cause field with
// Phase left empty — the same "phase empty means not reported" contract the
// server itself uses — via serverError, which is what names --server (or
// --password/--token) in the one line this prints. This command's workload
// half needs no server at all, and making the whole command depend on one
// would regress the one question status could always answer.
//
// Only Phase/Revision/Cause are read from the response; the server's own
// Verdicts and Namespace are not — the workload half above already computed
// those by reading the cluster directly, and reconciling two independently
// computed answers is exactly the disagreement issue #151 exists to avoid.
func resolveDeliveryPhase(ctx context.Context, opts *statusOptions) *kelsonv1alpha1.StatusResponse {
	// status has no --project flag (its input is always -f), so project is
	// always empty here; resolveSpecRef is still the one place that builds a
	// SpecRef from -f files, the same as every façade-backed verb uses.
	ref, env, err := resolveSpecRef(opts.files, "", opts.env)
	if err != nil {
		return &kelsonv1alpha1.StatusResponse{Cause: err.Error()}
	}
	// --profile/--image are passed through unchanged: the server renders this
	// RPC itself (inlineProfileRef's doc comment), the same as deploy/rollback,
	// so "from-cluster" here captures kelson-server's own cluster connection,
	// not this process's --kubeconfig — which is what the workload half above
	// already used for its own, separately-resolved render.
	profile, err := inlineProfileRef(opts.profile)
	if err != nil {
		return &kelsonv1alpha1.StatusResponse{Cause: err.Error()}
	}
	client, addr := opts.server.deployClient()

	reqCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	res, err := client.Status(reqCtx, connect.NewRequest(&kelsonv1alpha1.StatusRequest{
		Spec:        ref,
		Environment: env,
		Profile:     profile,
		Image:       opts.image,
	}))
	if err != nil {
		return &kelsonv1alpha1.StatusResponse{Cause: serverError("status", addr, err).Error()}
	}
	return res.Msg
}

// printPhase prints the delivery-phase line in the place the "not reported"
// gap used to occupy — unconditionally, including when everything is
// healthy, so a reader never learns "kelson did not tell me it wasn't
// checking" at the worst possible moment.
func printPhase(out *printer, res *kelsonv1alpha1.StatusResponse) {
	out.printf("  delivery phase: %s\n", phaseLine(res))
}

// phaseLine renders one delivery phase the way `kelson deploy`'s transition
// log does (deploy.go's transitionDetail): the revision and the cause folded
// in wherever the server reported one, so a stale status (internal/api's
// "status is at generation N, spec is at M") reads inline rather than as a
// separate line to reconcile.
//
// An empty phase is the server's "not reported" contract (StatusResponse.Phase
// is empty exactly when the delivery half could not be answered), and Cause
// carries why — the RPC failure serverError already worded, or the server's
// own reportDelivery cause.
func phaseLine(res *kelsonv1alpha1.StatusResponse) string {
	phase := res.GetPhase()
	if phase == "" {
		cause := res.GetCause()
		if cause == "" {
			cause = "no cause reported"
		}
		return "not reported — " + cause
	}
	switch {
	case res.GetCause() != "" && res.GetRevision() != "":
		return fmt.Sprintf("%s (revision %s): %s", phase, res.GetRevision(), res.GetCause())
	case res.GetCause() != "":
		return fmt.Sprintf("%s: %s", phase, res.GetCause())
	case res.GetRevision() != "":
		return fmt.Sprintf("%s (revision %s)", phase, res.GetRevision())
	default:
		return phase
	}
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
// judged CronJobs — and this counter answers the workload half only; printPhase
// is what carries whether the revision itself arrived.
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
