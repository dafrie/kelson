package direct

import (
	"context"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/dafrie/kelson/internal/delivery"
)

// The release-command hook, delivery half (issue #104, ADR-0019).
//
// # What direct mode adds that a rendered set cannot say
//
// The renderer puts the release Job after the data services and before every
// workload, which is the only sequencing an ordered set of manifests can
// express (issue #89). Applying a Job before a Deployment does not mean the Job
// finished first — Kubernetes creates both and gets on with it. So the barrier
// lives here: this adapter applies everything up to and including the release
// Job, WAITS for it to succeed, and only then applies the workloads.
//
// That is the whole reason the field is refused in Flux mode. There, kelson
// writes files and somebody else's Kustomization applies them in one pass; no
// commit can say "and stop here until this Job is Complete". Refusing is the
// honest answer, and internal/renderer/release.go states it as a render error
// rather than letting a migration race the rollout it was supposed to precede.
//
// # What a failure means, precisely
//
// A failed migration fails the DEPLOY and leaves the previous revision running:
// the workloads are never applied, no history entry is recorded, and nothing is
// pruned. The cluster is left with the namespace, the data services and a Job
// object whose logs say what happened — which is why the error carries the
// Job's name and the tail of its output rather than a generic apply failure.
//
// # Re-running, and not re-running
//
// The Job's name carries the revision's spec hash, so re-applying an unchanged
// revision addresses the Job that already ran. Two cases follow, and they are
// deliberately different:
//
//   - the Job is Complete: the migration for this exact spec already succeeded,
//     so it is not run again and the deploy proceeds. A completed release hook
//     is a fact about a revision, not about a deploy attempt.
//   - the Job Failed: the same deploy is being attempted again — the operator
//     fixed the database, or the outage passed — so the failed Job is deleted
//     and re-created. A migration that can never be retried without kubectl
//     would make the first transient failure permanent.

const (
	// labelReleaseHook is the label the renderer stamps on the release Job
	// (internal/renderer/release.go). The adapter finds the Job by this label
	// and never by parsing its name: the name's format is the renderer's
	// business and this plane must not learn to read it.
	labelReleaseHook = "kelson.dev/release-hook"

	// releaseContainer is the name of the Job's one container, mirroring the
	// renderer's constant so logs are streamed from the right one.
	releaseContainer = "release"

	// defaultReleasePoll is how often the release Job is re-read while it runs.
	// Polling rather than watching keeps this testable against a fake client and
	// is just as responsive at deploy timescales — the same trade the build
	// executor makes (internal/delivery/kube/build_executor.go).
	defaultReleasePoll = 2 * time.Second

	// releaseLogLines is how much of a failed migration's output travels in the
	// error. Enough for the stack trace that says which migration broke, little
	// enough that a chatty command does not bury the sentence above it.
	releaseLogLines = 20
)

// podsResource is the core/v1 pods resource, used to find the pod a failed
// release Job produced so its logs can be quoted back.
var podsResource = schema.GroupVersionResource{Version: "v1", Resource: "pods"}

// LogTailer fetches a container's recent output. It is satisfied structurally
// by observation.ClientGoLogSource, which is how the CLI wires it: this package
// keeps its own one-method interface rather than importing the observation
// plane, so the dependency runs one way and a caller with no log access still
// gets a verdict.
//
// Logs are best-effort everywhere they appear in kelson, and they are here too:
// a failed migration is reported with or without them.
type LogTailer interface {
	ContainerLogs(ctx context.Context, namespace, pod, container string) (string, error)
}

// isReleaseJob reports whether a decoded target is a release hook. The label is
// the contract; the Job's name is the renderer's business and is never parsed
// here.
func isReleaseJob(t target) bool {
	return t.ref.Kind == "Job" && t.obj.GetLabels()[labelReleaseHook] == "true"
}

// awaitRelease applies one release Job and waits for it to succeed.
//
// The apply is not the plain server-side apply the rest of the set gets: a Job
// that already exists for this revision has already answered the question, and
// what it answered decides whether it is adopted, skipped or re-run.
func (a *Adapter) awaitRelease(ctx context.Context, t target, revision string) error {
	res := a.resource(t)
	name := t.obj.GetName()

	existing, err := res.Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		// Nothing ran yet; the apply below creates it.
	case err != nil:
		return delivery.ApplyFailed(t.ref.String(), "",
			"reading the release Job failed: "+err.Error(),
			"check the API server connection and RBAC for jobs.batch")
	default:
		switch outcome, msg := jobOutcome(existing); outcome {
		case releaseSucceeded:
			a.report(revision, t, releaseSucceeded,
				fmt.Sprintf("release job %s already completed for this revision", name))
			return nil
		case releaseFailed, releaseTimedOut:
			// The same revision is being deployed again after a failure. Clear
			// the verdict and let it run: a Job object is not editable, so a
			// retry is a delete and a create.
			a.report(revision, t, releaseRunning,
				fmt.Sprintf("release job %s failed previously (%s); re-running it", name, first(msg)))
			if err := a.deleteRelease(ctx, res, t); err != nil {
				return err
			}
		case releaseRunning:
			// Left over from an interrupted deploy of this very revision; the
			// apply below is a no-op and the wait picks it up where it is.
		}
	}

	if _, err := res.Apply(ctx, name, t.obj, metav1.ApplyOptions{FieldManager: a.manager, Force: false}); err != nil {
		return applyError(t.ref, err)
	}
	return a.waitRelease(ctx, res, t, revision)
}

// deleteRelease removes a finished release Job and waits for it to be gone, so
// the apply that follows creates rather than colliding with a terminating one.
func (a *Adapter) deleteRelease(ctx context.Context, res dynamic.ResourceInterface, t target) error {
	policy := metav1.DeletePropagationBackground
	name := t.obj.GetName()
	if err := res.Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &policy}); err != nil && !apierrors.IsNotFound(err) {
		return delivery.ApplyFailed(t.ref.String(), "",
			"deleting the previously failed release Job failed: "+err.Error(),
			"delete it by hand and re-deploy: kubectl delete job "+name+" -n "+t.ref.Namespace)
	}
	for {
		_, err := res.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return delivery.ApplyFailed(t.ref.String(), "",
				"reading the release Job failed: "+err.Error(),
				"check the API server connection and RBAC for jobs.batch")
		}
		if err := a.sleep(ctx); err != nil {
			return a.releaseInterrupted(t, err)
		}
	}
}

// waitRelease polls the Job to a terminal state. Success returns nil and the
// caller proceeds to the workloads; failure returns the structured error that
// stops the deploy with the previous revision still serving.
func (a *Adapter) waitRelease(ctx context.Context, res dynamic.ResourceInterface, t target, revision string) error {
	name := t.obj.GetName()
	for {
		job, err := res.Get(ctx, name, metav1.GetOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return delivery.ApplyFailed(t.ref.String(), "",
				"reading the release Job failed: "+err.Error(),
				"check the API server connection and RBAC for jobs.batch")
		}
		if err == nil {
			switch outcome, msg := jobOutcome(job); outcome {
			case releaseSucceeded:
				a.report(revision, t, releaseSucceeded, fmt.Sprintf("release job %s completed", name))
				return nil
			case releaseFailed:
				return a.releaseFailure(ctx, t, revision,
					fmt.Sprintf("the release command failed: %s", first(msg)))
			case releaseTimedOut:
				return a.releaseFailure(ctx, t, revision,
					fmt.Sprintf("the release command ran past its timeout (activeDeadlineSeconds): %s", first(msg)))
			case releaseRunning:
				a.report(revision, t, releaseRunning, releaseProgress(name, job))
			}
		}
		if err := a.sleep(ctx); err != nil {
			return a.releaseInterrupted(t, err)
		}
	}
}

// releaseFailure builds the error a failed migration stops the deploy with. It
// names the Job, states plainly that nothing rolled, and carries the tail of
// the command's own output as the cause — the sentence the author actually
// needs is in there, not in anything kelson could write.
func (a *Adapter) releaseFailure(ctx context.Context, t target, revision, msg string) error {
	name := t.obj.GetName()
	a.report(revision, t, releaseFailed, fmt.Sprintf("release job %s failed: %s", name, msg))
	err := delivery.ReleaseFailed(t.ref.String(), "",
		msg+"; the workloads of this revision were not applied, so the previous revision is still running",
		"fix the release command and deploy again — the same deploy re-runs it. Its full output is in the Job's "+
			"pod: kubectl logs -n "+t.ref.Namespace+" job/"+name)
	if tail := a.releaseLogs(ctx, t); tail != "" {
		err.Cause = tail
	}
	return err
}

// releaseInterrupted reports a wait that ended without a verdict — a cancelled
// context, or the deploy's own budget expiring. Unknown must never read as
// success: the workloads are not applied either way.
func (a *Adapter) releaseInterrupted(t target, cause error) error {
	return delivery.ReleaseFailed(t.ref.String(), "",
		"waiting for the release command was interrupted before it finished: "+cause.Error()+
			"; the workloads of this revision were not applied",
		"raise --timeout if the migration needs longer, or watch it directly: kubectl get job -n "+
			t.ref.Namespace+" "+t.obj.GetName())
}

// releaseLogs fetches the tail of the release pod's output. Every step is
// best-effort: no log source, no pod, no permission — all of them return "" and
// the failure is still reported.
func (a *Adapter) releaseLogs(ctx context.Context, t target) string {
	if a.logs == nil {
		return ""
	}
	pods, err := a.client.Resource(podsResource).Namespace(t.ref.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "job-name=" + t.obj.GetName(),
	})
	if err != nil || len(pods.Items) == 0 {
		return ""
	}
	// The last pod is the last attempt, which is the one that failed.
	pod := pods.Items[len(pods.Items)-1]
	out, err := a.logs.ContainerLogs(ctx, t.ref.Namespace, pod.GetName(), releaseContainer)
	if err != nil {
		return ""
	}
	return tailLines(out, releaseLogLines)
}

// report emits one progress observation, in the delivery plane's own status
// vocabulary so a caller can feed it straight to the deployment state machine.
//
// The phase choice is deliberate and is what ADR-0019 records: a migration is
// not a new phase. Running one is Reconciling — something is actively working
// on this revision — and a failed one is Rejected, because that is exactly what
// Rejected means: the change was processed, refused, and is not live. The Job's
// name travels in Detail so a UI can link to it without parsing the cause.
//
// Repeated identical messages are dropped, so a five-minute migration prints
// once rather than every two seconds.
func (a *Adapter) report(revision string, t target, outcome releaseOutcome, msg string) {
	if a.progress == nil {
		return
	}
	phase := delivery.PhaseReconciling
	if outcome == releaseFailed || outcome == releaseTimedOut {
		phase = delivery.PhaseRejected
	}
	cause := AdapterName + ": " + msg
	a.mu.Lock()
	repeat := a.lastReleaseCause == cause
	a.lastReleaseCause = cause
	a.mu.Unlock()
	if repeat {
		return
	}
	a.progress(delivery.Status{
		Phase:    phase,
		Revision: revision,
		Cause:    cause,
		Detail: map[string]string{
			"releaseJob":   t.obj.GetName(),
			"releaseState": string(outcome),
		},
	})
}

// sleep waits one poll interval, or returns the reason it cannot.
func (a *Adapter) sleep(ctx context.Context) error {
	interval := a.releasePoll
	if interval <= 0 {
		interval = defaultReleasePoll
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// --- job state --------------------------------------------------------------

// releaseOutcome classifies a release Job's state. The timeout is kept apart
// from the plain failure for the reason the build executor keeps them apart:
// "ran out of time" and "the command errored" ask for different fixes.
type releaseOutcome string

const (
	releaseRunning   releaseOutcome = "running"
	releaseSucceeded releaseOutcome = "succeeded"
	releaseFailed    releaseOutcome = "failed"
	releaseTimedOut  releaseOutcome = "timeout"
)

// jobOutcome reads a Job's conditions through the unstructured API, which is
// the only client this plane has (the delivery plane talks to the API server
// dynamically — issue #137).
func jobOutcome(job *unstructured.Unstructured) (releaseOutcome, string) {
	conditions, _, _ := unstructured.NestedSlice(job.Object, "status", "conditions")
	for _, raw := range conditions {
		c, ok := raw.(map[string]any)
		if !ok || c["status"] != "True" {
			continue
		}
		switch c["type"] {
		case "Complete", "SuccessCriteriaMet":
			return releaseSucceeded, conditionText(c)
		case "Failed":
			if reason, _ := c["reason"].(string); reason == "DeadlineExceeded" {
				return releaseTimedOut, conditionText(c)
			}
			return releaseFailed, conditionText(c)
		}
	}
	return releaseRunning, ""
}

// releaseProgress describes a running Job in one line, using the counters the
// Job controller publishes so a long migration shows something moving.
func releaseProgress(name string, job *unstructured.Unstructured) string {
	active, _, _ := unstructured.NestedInt64(job.Object, "status", "active")
	failed, _, _ := unstructured.NestedInt64(job.Object, "status", "failed")
	msg := fmt.Sprintf("release job %s is running", name)
	if active == 0 {
		msg = fmt.Sprintf("release job %s has not started a pod yet", name)
	}
	if failed > 0 {
		msg += fmt.Sprintf(" (%d attempt(s) failed so far)", failed)
	}
	return msg
}

// releaseHealth is the Status() half: a release Job that has not completed
// keeps the revision out of Healthy, so `kelson status` on an interrupted
// deploy says the migration is still running rather than reporting a rollout
// that never happened.
func releaseHealth(obj *unstructured.Unstructured) (healthState, string) {
	switch outcome, msg := jobOutcome(obj); outcome {
	case releaseSucceeded:
		return healthOK, ""
	case releaseFailed:
		return healthDegraded, "the release command failed: " + first(msg)
	case releaseTimedOut:
		return healthDegraded, "the release command ran past its timeout: " + first(msg)
	default:
		return healthProgressing, "the release command has not finished"
	}
}

// --- text -------------------------------------------------------------------

// tailLines keeps the last n non-empty lines of a command's output.
func tailLines(out string, n int) string {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// first reduces a multi-line controller message to its first line, so a cause
// stays one sentence.
func first(msg string) string {
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	if msg == "" {
		return "no reason reported"
	}
	return msg
}
