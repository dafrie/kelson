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
	// secrets are the per-run Secrets this build submitted beside its Job —
	// today exactly the clone credential (ADR-0033 decision 5). They are owned
	// by the Job so the API server's garbage collector removes them even if
	// this process never reaches Wait, and Wait deletes them explicitly anyway
	// so a finished build does not leave a live credential in the namespace for
	// as long as garbage collection takes.
	secrets []string
}

// Submit decodes the rendered build manifest, creates what it declares, and
// returns the Job's name. It is idempotent: because Job names are a
// deterministic function of the Request, re-submitting an identical build finds
// the objects already present and treats that as success rather than a
// conflict.
//
// # Why the manifest may be more than a Job
//
// A build that clones a private repository submits the credential beside the
// Job, in a Secret of its own (ADR-0033 decision 5, internal/build's
// CloneSecretManifest). It is a second document rather than a field on the pod
// spec because a credential in a pod spec is a credential in every
// `kubectl get job -o yaml`.
//
// The Secrets are created *before* the Job even though the Job owns them: a
// pod whose secret volume does not exist yet sits in ContainerCreating with a
// FailedMount event until the kubelet retries, and that would make the private
// path visibly slower than the public one for no reason. The ownership
// reference is set immediately afterwards, which is what ties the two
// lifecycles together for the case this process dies before Wait runs.
func (e *BuildExecutor) Submit(ctx context.Context, manifest []byte) (string, error) {
	job, secrets, meta, err := decodeBuildObjects(manifest)
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

	for _, secret := range secrets {
		secret.Namespace = meta.namespace
		if _, err := e.clientset.CoreV1().Secrets(meta.namespace).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				return "", fmt.Errorf("kube: writing the per-run Secret %s for build %s: %w", secret.Name, job.Name, err)
			}
			// A re-submitted build re-mints its credential, and the old value
			// is the one that is about to expire. Update rather than adopt.
			if _, err := e.clientset.CoreV1().Secrets(meta.namespace).Update(ctx, secret, metav1.UpdateOptions{}); err != nil {
				return "", fmt.Errorf("kube: refreshing the per-run Secret %s for build %s: %w", secret.Name, job.Name, err)
			}
		}
	}

	created, err := e.clientset.BatchV1().Jobs(meta.namespace).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		if !apierrors.IsAlreadyExists(err) {
			// The Secrets are already written and nothing will ever own them,
			// so they are removed here rather than left behind holding a live
			// credential.
			e.deleteSecrets(ctx, meta)
			return "", fmt.Errorf("kube: submitting build Job %s: %w", job.Name, err)
		}
		// AlreadyExists: an identical build Job from the same Request is
		// already scheduled; adopt it instead of failing a retry.
		created, err = e.clientset.BatchV1().Jobs(meta.namespace).Get(ctx, job.Name, metav1.GetOptions{})
		if err != nil {
			return "", fmt.Errorf("kube: reading the build Job %s this submit adopted: %w", job.Name, err)
		}
	}
	e.remember(job.Name, meta)
	e.own(ctx, created, meta)
	return job.Name, nil
}

// own makes the Job the owner of the per-run Secrets it was submitted with, so
// deleting the Job deletes them whoever does the deleting.
//
// A failure here is not fatal and is deliberately silent about the Secret's
// contents: the explicit delete in [BuildExecutor.Wait] is the primary cleanup,
// and this is the backstop for the process that never gets there. Failing the
// build over a missing backstop would trade a leaked object for no build at
// all.
func (e *BuildExecutor) own(ctx context.Context, job *batchv1.Job, meta jobMeta) {
	if len(meta.secrets) == 0 || job == nil || job.UID == "" {
		return
	}
	ref := metav1.OwnerReference{
		APIVersion: "batch/v1",
		Kind:       "Job",
		Name:       job.Name,
		UID:        job.UID,
	}
	for _, name := range meta.secrets {
		secret, err := e.clientset.CoreV1().Secrets(meta.namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			continue
		}
		if hasOwner(secret.OwnerReferences, ref) {
			continue
		}
		secret.OwnerReferences = append(secret.OwnerReferences, ref)
		_, _ = e.clientset.CoreV1().Secrets(meta.namespace).Update(ctx, secret, metav1.UpdateOptions{})
	}
}

func hasOwner(refs []metav1.OwnerReference, want metav1.OwnerReference) bool {
	for _, r := range refs {
		if r.UID == want.UID {
			return true
		}
	}
	return false
}

// deleteSecrets removes the per-run Secrets of one build. A missing one is
// success: this runs on every terminal path, and "already gone" is the state it
// is trying to reach.
func (e *BuildExecutor) deleteSecrets(ctx context.Context, meta jobMeta) {
	for _, name := range meta.secrets {
		err := e.clientset.CoreV1().Secrets(meta.namespace).Delete(ctx, name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			// Reported nowhere on purpose: the owner reference set in Submit
			// means the API server removes it with the Job regardless, and a
			// build that pushed successfully must not fail because a delete
			// that will happen anyway was slow.
			continue
		}
	}
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
		// The credential first, and unconditionally: it is short-lived but it
		// is live, and it must not outlive the build on any path — including
		// the one where deleting the Job itself fails.
		e.deleteSecrets(ctx, meta)
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

// decodeBuildObjects turns the rendered manifest back into the typed objects a
// build submits — exactly one Job, and any per-run Secrets beside it — plus the
// bits Wait needs. The manifest is YAML that is also valid JSON once
// translated; sigs.k8s.io/yaml bridges each document into a json.Unmarshal.
//
// A kind other than Job or Secret is refused rather than skipped. This decoder
// is the one thing standing between a rendered manifest and `create`, and a
// driver that started emitting something else should meet a sentence here
// rather than have it silently dropped.
func decodeBuildObjects(manifest []byte) (*batchv1.Job, []*corev1.Secret, jobMeta, error) {
	var job *batchv1.Job
	var secrets []*corev1.Secret

	for i, doc := range splitYAMLDocuments(manifest) {
		js, err := sigsyaml.YAMLToJSON(doc)
		if err != nil {
			return nil, nil, jobMeta{}, fmt.Errorf("kube: decoding build manifest document %d as YAML: %w", i, err)
		}
		var kind struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(js, &kind); err != nil {
			return nil, nil, jobMeta{}, fmt.Errorf("kube: reading the kind of build manifest document %d: %w", i, err)
		}
		switch kind.Kind {
		case "Job":
			if job != nil {
				return nil, nil, jobMeta{}, errors.New("kube: a build manifest declares more than one Job")
			}
			job = &batchv1.Job{}
			if err := json.Unmarshal(js, job); err != nil {
				return nil, nil, jobMeta{}, fmt.Errorf("kube: decoding build manifest as a Job: %w", err)
			}
		case "Secret":
			secret := &corev1.Secret{}
			if err := json.Unmarshal(js, secret); err != nil {
				// The error deliberately says nothing about the document: it is
				// a Secret, and a decode failure quoting its bytes would be the
				// leak this whole path exists to avoid.
				return nil, nil, jobMeta{}, fmt.Errorf("kube: decoding a per-run Secret of the build manifest: %w", err)
			}
			secrets = append(secrets, secret)
		default:
			return nil, nil, jobMeta{}, fmt.Errorf("kube: a build manifest may declare a Job and its per-run Secrets, not %q", kind.Kind)
		}
	}
	if job == nil {
		return nil, nil, jobMeta{}, errors.New("kube: the build manifest declares no Job")
	}

	meta := jobMeta{
		namespace: job.Namespace,
		image:     job.Annotations[build.AnnotationImage],
		tag:       job.Annotations[build.AnnotationTag],
	}
	if len(job.Spec.Template.Spec.Containers) > 0 {
		meta.container = job.Spec.Template.Spec.Containers[0].Name
	}
	for _, s := range secrets {
		meta.secrets = append(meta.secrets, s.Name)
	}
	return job, secrets, meta, nil
}

// splitYAMLDocuments cuts a multi-document stream on `---` at the start of a
// line, dropping empty documents.
//
// It is a split rather than a yaml.Decoder loop because the documents are
// handed straight to sigsyaml.YAMLToJSON, which takes bytes: decoding to
// yaml.Node and re-encoding would round-trip the very manifest whose bytes the
// drivers render deterministically.
func splitYAMLDocuments(manifest []byte) [][]byte {
	var out [][]byte
	for _, part := range bytes.Split(manifest, []byte("\n---")) {
		trimmed := bytes.TrimSpace(bytes.TrimPrefix(bytes.TrimSpace(part), []byte("---")))
		if len(trimmed) == 0 {
			continue
		}
		out = append(out, trimmed)
	}
	return out
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
