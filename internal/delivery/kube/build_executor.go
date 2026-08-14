// Package kube's build executor: a Kubernetes-backed implementation of the
// Cluster seam both build drivers declare (internal/build/buildkit and
// internal/build/buildpacks), living here because this plane is allowed the
// client-go libraries and internal/build deliberately is not (.golangci.yml).
// It satisfies both interfaces structurally, so it imports neither driver.
//
// One executor serves both strategies because nothing in it is
// strategy-specific: it submits a Job, streams the one container the Job
// declares, and reads what was pushed off the Job's own annotations
// (build.AnnotationImage) rather than out of a builder's command line. Teaching
// it to recognise buildctl's `--output name=` and the lifecycle's `-image`
// would have made a second strategy a second parser here.
package kube

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sync"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	sigsyaml "sigs.k8s.io/yaml"

	"github.com/dafrie/kelson/internal/build"
)

const (
	// maxBuildLogBytes caps how much build output this executor buffers and
	// forwards. Log streaming itself is unbounded (it is copied straight to
	// the caller's writer); the cap only bounds what we hold for error
	// messages and digest parsing.
	maxBuildLogBytes = 16 << 20
	// jobPollInterval is how often Wait re-reads the Job while it is not yet
	// terminal. Polling rather than watching keeps the executor testable
	// against the fake clientset (whose watch is awkward to drive) and is just
	// as responsive at CLI timescales.
	jobPollInterval = 2 * time.Second
)

// NewBuildExecutor returns a Kubernetes-backed buildkit.Cluster implementation
// that submits rendered build Jobs and waits on them, streaming pod logs while
// a build runs. It needs the typed clientset only; like every adapter in this
// plane it takes its client injected so tests drive a fake (issue #48).
func NewBuildExecutor(clientset kubernetes.Interface) *BuildExecutor {
	return &BuildExecutor{
		clientset: clientset,
		meta:      map[string]jobMeta{},
	}
}

// BuildExecutor implements the buildkit.Cluster seam with a live cluster
// (issue #48). Submit renders the Job from the manifest it is handed; Wait
// follows the build pod's logs, watches the Job to a terminal state, and
// returns the pushed reference.
type BuildExecutor struct {
	clientset kubernetes.Interface

	mu sync.Mutex
	// meta remembers, per Job name, where the Job is and what it pushes. Wait
	// only receives the Job name, so this is the executor's one piece of
	// state; a Job name is unique per build and mutation is mutex-guarded, so
	// concurrent builds for different Requests stay safe.
	meta map[string]jobMeta
}

// jobMeta is what Submit needs to retain so Wait can act without re-parsing
// the manifest: the namespace the Job lives in, what it pushes, and which
// container produces the build's output.
type jobMeta struct {
	namespace string
	// image is the repository with no tag/digest, e.g. ghcr.io/acme/web.
	image string
	// tag is the mutable tag also pushed, "" when the build pushed untagged.
	tag string
	// container is the build container's name, taken from the manifest rather
	// than assumed: "buildkit" for a Dockerfile build, "buildpack" for a
	// lifecycle build.
	container string
}

// Submit decodes the rendered Job manifest, creates the Job, and returns its
// name. It is idempotent: because Job names are a deterministic function of
// the Request, re-submitting an identical build finds the Job already present
// and treats that as success rather than a conflict.
func (e *BuildExecutor) Submit(ctx context.Context, manifest []byte) (string, error) {
	job, meta, err := decodeBuildJob(manifest)
	if err != nil {
		return "", err
	}
	if job.Name == "" || meta.namespace == "" {
		return "", errors.New("kube: build manifest has no name and namespace")
	}
	if meta.image == "" {
		// Without it Wait could only return a digest with nothing to hang it
		// on, and a Result with no Reference is what every caller checks for
		// last. Refuse before the Job runs rather than after it pushed.
		return "", fmt.Errorf("kube: build manifest %s carries no %s annotation, so its result could not be named", job.Name, build.AnnotationImage)
	}
	if _, err := e.clientset.BatchV1().Jobs(meta.namespace).Create(ctx, job, metav1.CreateOptions{}); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return "", fmt.Errorf("kube: submitting build Job %s: %w", job.Name, err)
		}
		// AlreadyExists: an identical build Job from the same Request is
		// already scheduled; adopt it instead of failing a retry.
	}
	e.remember(job.Name, meta)
	return job.Name, nil
}

// Wait streams the build pod's logs to w as they are produced, then waits for
// the Job to terminate. It distinguishes success, failure and timeout, returns
// the pushed digest, and cleans the Job up. It satisifies buildkit.Cluster.
//
// log streaming: pod logs are followed live, so output reaches the caller as
// the build produces it, not in one blast at the end. A failure mid-stream
// (an API connection drop, for example) does not change the build's outcome —
// Wait still reports the Job's terminal state — but it does truncate what is
// forwarded, and because the digest is scraped from the tail of the output, a
// truncated stream can turn a healthy build into a "no digest" error.
func (e *BuildExecutor) Wait(ctx context.Context, name string, w io.Writer) (build.Result, error) {
	meta, ok := e.metaFor(name)
	if !ok {
		return build.Result{}, fmt.Errorf("kube: build Job %q was not submitted by this executor", name)
	}

	// Buffer the full build output regardless of w so a failing build can
	// surface its own output; also copy it to the caller's writer live.
	var buildOut bytes.Buffer
	dst := io.Writer(&buildOut)
	if w != nil {
		dst = io.MultiWriter(&buildOut, w)
	}
	if err := streamPodLogs(ctx, e.clientset, meta, name, dst); err != nil {
		// Log streaming is best-effort next to the definitive job state; a
		// stream error is recorded, not fatal on its own.
		buildOut.WriteString("\n[kube] log stream interrupted: " + err.Error() + "\n")
	}

	job, err := waitJobTerminal(ctx, e.clientset, meta.namespace, name)
	if err != nil {
		return build.Result{}, err
	}

	cleanup := func() error {
		policy := metav1.DeletePropagationBackground
		return e.clientset.BatchV1().Jobs(meta.namespace).Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &policy})
	}

	switch outcome, msg := jobOutcome(job); outcome {
	case outcomeSucceeded:
		digest, err := parsePushedDigest(buildOut.String())
		if err != nil {
			_ = cleanup()
			return build.Result{}, err
		}
		res := build.Result{Reference: meta.image + "@" + digest, Digest: digest, Tag: meta.tag}
		if err := cleanup(); err != nil {
			// Cleanup is best-effort so finished builds do not leak; a failed
			// delete is reported distinctly from a failed build, and carries
			// the digest so a successful push is not lost in the noise.
			return build.Result{}, fmt.Errorf("kube: build %s succeeded (digest %s) but its Job could not be cleaned up: %w", name, digest, err)
		}
		return res, nil

	case outcomeTimeout:
		if err := cleanup(); err != nil {
			return build.Result{}, fmt.Errorf("kube: build %s timed out and its Job could not be cleaned up: %w", name, err)
		}
		// A deadline exceeded is a timeout, not a generic failure — distinct
		// wording and a distinct classification so callers can tell "ran out
		// of time" from "the build errored".
		return build.Result{}, fmt.Errorf("kube: build %s timed out (activeDeadlineSeconds exceeded): %s", name, msg)

	default: // outcomeFailed
		if err := cleanup(); err != nil {
			return build.Result{}, fmt.Errorf("kube: build %s failed and its Job could not be cleaned up: %w", name, err)
		}
		tail := tailOf(buildOut.String())
		if tail != "" {
			return build.Result{}, fmt.Errorf("kube: build %s failed: %s\nbuild output:\n%s", name, msg, tail)
		}
		return build.Result{}, fmt.Errorf("kube: build %s failed: %s", name, msg)
	}
}

func (e *BuildExecutor) remember(name string, meta jobMeta) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.meta[name] = meta
}

func (e *BuildExecutor) metaFor(name string) (jobMeta, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	m, ok := e.meta[name]
	return m, ok
}

// --- manifest decoding ------------------------------------------------------

// decodeBuildJob turns the rendered manifest back into a typed Job plus the
// bits Wait needs. The manifest is YAML that is also valid JSON once
// translated; sigs.k8s.io/yaml bridges it into a json.Unmarshal.
func decodeBuildJob(manifest []byte) (*batchv1.Job, jobMeta, error) {
	js, err := sigsyaml.YAMLToJSON(manifest)
	if err != nil {
		return nil, jobMeta{}, fmt.Errorf("kube: decoding build manifest as YAML: %w", err)
	}
	job := &batchv1.Job{}
	if err := json.Unmarshal(js, job); err != nil {
		return nil, jobMeta{}, fmt.Errorf("kube: decoding build manifest as a Job: %w", err)
	}
	meta := jobMeta{
		namespace: job.Namespace,
		image:     job.Annotations[build.AnnotationImage],
		tag:       job.Annotations[build.AnnotationTag],
	}
	if len(job.Spec.Template.Spec.Containers) > 0 {
		meta.container = job.Spec.Template.Spec.Containers[0].Name
	}
	return job, meta, nil
}

// --- log streaming ----------------------------------------------------------

// streamPodLogs follows the single build pod's logs into w, closing when the
// pod's container exits. A Job with BackoffLimit 0 has one pod, so the first
// pod marked with the Job's controller label is the build.
func streamPodLogs(ctx context.Context, client kubernetes.Interface, meta jobMeta, jobName string, w io.Writer) error {
	pods, err := client.CoreV1().Pods(meta.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "job-name=" + jobName,
	})
	if err != nil {
		return fmt.Errorf("kube: listing build pod for job %s: %w", jobName, err)
	}
	if len(pods.Items) == 0 {
		// The Job has not produced a pod yet; nothing to stream. Wait will
		// still surface the Job's terminal state.
		return nil
	}
	req := client.CoreV1().Pods(meta.namespace).GetLogs(pods.Items[0].Name, &corev1.PodLogOptions{
		Follow: true,
		// The build container's name is the Job's, not this package's: a
		// Dockerfile build calls it "buildkit" and a lifecycle build calls it
		// "buildpack", and streaming the wrong one is streaming nothing.
		Container: meta.container,
	})
	rc, err := req.Stream(ctx)
	if err != nil {
		return fmt.Errorf("kube: opening build pod log stream: %w", err)
	}
	defer func() { _ = rc.Close() }()
	if _, err := io.Copy(w, io.LimitReader(rc, maxBuildLogBytes)); err != nil {
		return fmt.Errorf("kube: copying build pod log stream: %w", err)
	}
	return nil
}

// waitJobTerminal polls the Job until it reaches a terminal condition or the
// context is cancelled.
func waitJobTerminal(ctx context.Context, client kubernetes.Interface, namespace, name string) (*batchv1.Job, error) {
	ticker := time.NewTicker(jobPollInterval)
	defer ticker.Stop()
	for {
		job, err := client.BatchV1().Jobs(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("kube: reading build Job %s: %w", name, err)
		}
		if outcome, _ := jobOutcome(job); outcome != outcomeRunning {
			return job, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("kube: waiting on build Job %s: %w", name, ctx.Err())
		case <-ticker.C:
		}
	}
}

// buildOutcome classifies a Job's terminal state.
type buildOutcome int

const (
	outcomeRunning buildOutcome = iota
	outcomeSucceeded
	outcomeFailed
	outcomeTimeout
)

// jobOutcome reads a Job's conditions. A deadline exceeded is classified as a
// timeout, not a plain failure, so .Wait can report it distinctly.
func jobOutcome(job *batchv1.Job) (buildOutcome, string) {
	for _, c := range job.Status.Conditions {
		switch {
		case c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue:
			return outcomeSucceeded, c.Message
		case c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue:
			if c.Reason == batchv1.JobReasonDeadlineExceeded {
				return outcomeTimeout, c.Message
			}
			return outcomeFailed, c.Message
		}
	}
	return outcomeRunning, ""
}

// tailOf reduces build output to a bounded trailing slice for error messages.
func tailOf(out string) string {
	const max = 2000
	if len(out) <= max {
		return out
	}
	return "…" + out[len(out)-max:]
}

// --- digest parsing ---------------------------------------------------------

// pushedDigestRe matches a `@sha256:<64 hex>` reference in the build log. Only
// digests referenced with '@' count — the push lines BuildKit emits
// (`pushing manifest for <ref>@sha256:…`) and the resulting image reference.
var pushedDigestRe = regexp.MustCompile(`@sha256:([0-9a-f]{64})`)

// parsePushedDigest extracts the digest BuildKit pushed. This is parsing a
// tool's stdout, which is honest-to-goodness brittle: BuildKit's log text is
// not an API, and this regex depends on the push line keeping the
// `@sha256:<hex>` shape and on images being sha256 (true for OCI/Docker, but
// a future non-sha256 manifest would not match). The mitigation is
// fail-closed: if no digest is found we return a clear error rather than
// silently fabricating an empty one, and we take the LAST pushed digest, which
// for a multi-platform push is the manifest list that `name@digest` resolves
// to.
func parsePushedDigest(out string) (string, error) {
	matches := pushedDigestRe.FindAllStringSubmatch(out, -1)
	if len(matches) == 0 {
		return "", errors.New("kube: no pushed image digest found in build output (BuildKit push line missing or its format changed)")
	}
	return "sha256:" + matches[len(matches)-1][1], nil
}
