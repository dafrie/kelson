package kube_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	sigsyaml "sigs.k8s.io/yaml"

	"github.com/dafrie/kelson/internal/build"
	"github.com/dafrie/kelson/internal/build/buildkit"
	"github.com/dafrie/kelson/internal/build/buildpacks"
	"github.com/dafrie/kelson/internal/delivery/kube"
)

const (
	testNS        = "apps"
	testJobName   = "build-shop-checkout-abc12345"
	testPodName   = "build-shop-checkout-abc12345-pod1"
	testRepo      = "ghcr.io/acme/web"
	testTag       = "abc12345"
	testBuildLogs = "#13 [2/3] RUN go build ./...\n#13 CACHED\n#22 exporting to image\n#22 pushing layers done\n"
	overflowDgst  = "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
)

// newFakeBuildCluster returns a fake clientset that streams the given logs for
// any pod-log request. The fake's Pods.GetLogs hands the *runtime.Unknown a
// reactor returns to a fakerest client, so returning it here is the standard
// way to drive a real log Stream with no cluster.
func newFakeBuildCluster(t *testing.T, logs string, objs ...runtime.Object) *fake.Clientset {
	t.Helper()
	cli := fake.NewSimpleClientset(objs...)
	cli.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "log" {
			return false, nil, nil
		}
		return true, &runtime.Unknown{Raw: []byte(logs)}, nil
	})
	return cli
}

// buildPod is a pod whose controller label marks it as owned by the test Job,
// which is what the executor's log streaming selects on.
func buildPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testPodName,
			Namespace: testNS,
			Labels:    map[string]string{"job-name": testJobName},
		},
	}
}

// jobManifest renders a build Job manifest matching the test constants: the
// destination is annotated, which is where the executor reads it (both
// drivers' workload.go render the same annotations).
func jobManifest(t *testing.T) []byte {
	t.Helper()
	job := &batchv1.Job{
		TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      testJobName,
			Namespace: testNS,
			Annotations: map[string]string{
				build.AnnotationImage: testRepo,
				build.AnnotationTag:   testTag,
			},
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:    "buildkit",
						Command: []string{"sh", "-c", "exec buildctl build --local context=/workspace --output type=image,image-format=oci,name=" + testRepo + ":" + testTag + ",push=true"},
					}},
				},
			},
		},
	}
	b, err := sigsyaml.Marshal(job)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	return b
}

// markJobTerminal flips a submitted Job into a terminal condition, so Wait's
// poll sees it done (the submit path, not the tracker, set the original).
func markJobTerminal(t *testing.T, cli *fake.Clientset, cond batchv1.JobConditionType, reason string) {
	t.Helper()
	ctx := context.Background()
	job, err := cli.BatchV1().Jobs(testNS).Get(ctx, testJobName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	job.Status.Conditions = []batchv1.JobCondition{{
		Type:   cond,
		Status: corev1.ConditionTrue,
		Reason: reason,
	}}
	if _, err := cli.BatchV1().Jobs(testNS).UpdateStatus(ctx, job, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update status: %v", err)
	}
}

// runTerminalBuild drives Submit + Wait against a fake whose Job ends in the
// given condition, returning Wait's error.
func runTerminalBuild(t *testing.T, cli *fake.Clientset, cond batchv1.JobConditionType, reason string, w *bytes.Buffer) error {
	t.Helper()
	ex := kube.NewBuildExecutor(cli)
	ctx := context.Background()

	name, err := ex.Submit(ctx, jobManifest(t))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if name != testJobName {
		t.Fatalf("Submit returned %q, want %q", name, testJobName)
	}
	markJobTerminal(t, cli, cond, reason)
	_, err = ex.Wait(ctx, name, w)
	return err
}

// TestBuildExecutorSuccessDigestsAndStreams: a successful build returns a real
// digest and Result.Reference, and its logs reach the caller's writer live.
func TestBuildExecutorSuccessDigestsAndStreams(t *testing.T) {
	logs := testBuildLogs + "#22 pushing manifest for " + testRepo + "@" + overflowDgst + " done\n"
	cli := newFakeBuildCluster(t, logs, buildPod())

	var w bytes.Buffer
	ex := kube.NewBuildExecutor(cli)
	ctx := context.Background()
	name, err := ex.Submit(ctx, jobManifest(t))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	markJobTerminal(t, cli, batchv1.JobComplete, "")
	res, err := ex.Wait(ctx, name, &w)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if res.Digest != overflowDgst {
		t.Errorf("Digest = %q, want %q", res.Digest, overflowDgst)
	}
	if res.Reference != testRepo+"@"+overflowDgst {
		t.Errorf("Reference = %q, want %q", res.Reference, testRepo+"@"+overflowDgst)
	}
	if res.Tag != testTag {
		t.Errorf("Tag = %q, want %q", res.Tag, testTag)
	}
	if !strings.Contains(w.String(), "pushing layers") {
		t.Errorf("streamed logs did not reach the writer: %q", w.String())
	}
}

// TestBuildExecutorFailureSurfacesBuildOutput: a failed build reports its own
// output in the error, not a generic message.
func TestBuildExecutorFailureSurfacesBuildOutput(t *testing.T) {
	const buildErr = "ERROR: failed to solve: pull access denied"
	logs := testBuildLogs + buildErr + "\n"
	cli := newFakeBuildCluster(t, logs, buildPod())

	var w bytes.Buffer
	err := runTerminalBuild(t, cli, batchv1.JobFailed, "Error", &w)
	if err == nil {
		t.Fatal("expected a build failure")
	}
	if !strings.Contains(err.Error(), buildErr) {
		t.Errorf("failure did not surface the build's own output %q: %v", buildErr, err)
	}
	if strings.Contains(err.Error(), "timed out") {
		t.Errorf("a plain failure must not be reported as a timeout: %v", err)
	}
}

// TestBuildExecutorTimeoutIsDistinct: deadline-exceeded is reported as a
// timeout, not as a generic build failure.
func TestBuildExecutorTimeoutIsDistinct(t *testing.T) {
	cli := newFakeBuildCluster(t, testBuildLogs, buildPod())

	var w bytes.Buffer
	err := runTerminalBuild(t, cli, batchv1.JobFailed, batchv1.JobReasonDeadlineExceeded, &w)
	if err == nil {
		t.Fatal("expected a timeout")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("deadline-exceeded should be reported as a timeout, got: %v", err)
	}
	if strings.Contains(err.Error(), "build output") {
		t.Errorf("timeout report should not masquerade as a failed build with output: %v", err)
	}
}

// TestBuildExecutorNoDigestIsAnError: output without a pushed digest must be a
// clear error, never a silently-empty digest.
func TestBuildExecutorNoDigestIsAnError(t *testing.T) {
	cli := newFakeBuildCluster(t, testBuildLogs, buildPod())

	var w bytes.Buffer
	err := runTerminalBuild(t, cli, batchv1.JobComplete, "", &w)
	if err == nil {
		t.Fatal("expected a no-digest error")
	}
	if !strings.Contains(err.Error(), "digest") {
		t.Errorf("no-digest error should say so: %v", err)
	}
}

// TestBuildExecutorCleansUp verifies the Job is deleted after a successful
// build, so finished builds do not linger.
func TestBuildExecutorCleansUp(t *testing.T) {
	logs := testBuildLogs + "#22 pushing manifest for " + testRepo + "@" + overflowDgst + " done\n"
	cli := newFakeBuildCluster(t, logs, buildPod())

	var w bytes.Buffer
	if err := runTerminalBuild(t, cli, batchv1.JobComplete, "", &w); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if _, err := cli.BatchV1().Jobs(testNS).Get(context.Background(), testJobName, metav1.GetOptions{}); err == nil {
		t.Error("build Job was not cleaned up after success")
	}
}

// TestBuildExecutorCleanupFailureIsDistinct: when cleanup fails after a
// successful push, the error says cleanup failed, not that the build failed,
// and keeps the digest.
func TestBuildExecutorCleanupFailureIsDistinct(t *testing.T) {
	logs := testBuildLogs + "#22 pushing manifest for " + testRepo + "@" + overflowDgst + " done\n"
	cli := newFakeBuildCluster(t, logs, buildPod())
	cli.PrependReactor("delete", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("kube: forbidden")
	})

	var w bytes.Buffer
	err := runTerminalBuild(t, cli, batchv1.JobComplete, "", &w)
	if err == nil {
		t.Fatal("expected a cleanup failure")
	}
	if !strings.Contains(err.Error(), "cleaned up") {
		t.Errorf("cleanup failure should be reported distinctly, got: %v", err)
	}
	if !strings.Contains(err.Error(), overflowDgst) {
		t.Errorf("cleanup failure should carry the digest so the push is not lost, got: %v", err)
	}
}

// TestBuildExecutorSubmitsARenderedPushSecretJob closes the loop between the
// pure renderer and this executor for the credential path (#51). The other
// tests here hand-build a manifest; this one submits exactly what
// buildkit.Config renders with a PushSecret, because that manifest gained a
// Secret volume — a shape the round trip through sigs.k8s.io/yaml into a typed
// Job had never carried before, and the only place the two halves can drift.
func TestBuildExecutorSubmitsARenderedPushSecretJob(t *testing.T) {
	req := build.Request{
		Project:     "shop",
		Component:   "checkout",
		Environment: "production",
		Image:       testRepo,
		Tag:         testTag,
		Revision:    "abc12345",
	}
	manifest, err := buildkit.Config{Namespace: testNS, PushSecret: "ghcr-push"}.Workload(req)
	if err != nil {
		t.Fatalf("Workload: %v", err)
	}

	cli := fake.NewSimpleClientset()
	name, err := kube.NewBuildExecutor(cli).Submit(context.Background(), manifest)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	job, err := cli.BatchV1().Jobs(testNS).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get submitted job: %v", err)
	}
	var vol *corev1.Volume
	for i, v := range job.Spec.Template.Spec.Volumes {
		if v.Secret != nil {
			vol = &job.Spec.Template.Spec.Volumes[i]
		}
	}
	if vol == nil {
		t.Fatalf("the submitted Job lost its projected push credential: %+v", job.Spec.Template.Spec.Volumes)
	}
	if vol.Secret.SecretName != "ghcr-push" {
		t.Errorf("secretName = %q, want %q", vol.Secret.SecretName, "ghcr-push")
	}
	if vol.Secret.DefaultMode == nil || *vol.Secret.DefaultMode != 0o400 {
		t.Errorf("defaultMode = %v, want 0400 — the mode must survive the YAML round trip", vol.Secret.DefaultMode)
	}
	if len(vol.Secret.Items) != 1 || vol.Secret.Items[0].Key != corev1.DockerConfigJsonKey || vol.Secret.Items[0].Path != "config.json" {
		t.Errorf("projected items = %+v, want %s -> config.json", vol.Secret.Items, corev1.DockerConfigJsonKey)
	}

	ctr := job.Spec.Template.Spec.Containers[0]
	var mounted bool
	for _, m := range ctr.VolumeMounts {
		if m.Name == vol.Name {
			mounted = m.ReadOnly
		}
	}
	if !mounted {
		t.Errorf("the build container must mount the credential read-only, got %+v", ctr.VolumeMounts)
	}
	var dockerConfig string
	for _, e := range ctr.Env {
		if e.Name == "DOCKER_CONFIG" {
			dockerConfig = e.Value
		}
	}
	if dockerConfig == "" {
		t.Errorf("DOCKER_CONFIG must survive to the submitted Job, got env %+v", ctr.Env)
	}
}

// TestBuildExecutorRunsARenderedBuildpacksJob closes the same loop for the
// second strategy (#49): one executor serves both, so what it must not depend
// on is anything buildkit-shaped. The buildpacks Job names its container
// "buildpack" and reports the push as `kelson: pushed <repo>@<digest>` — the
// executor has to submit it, find the destination in the annotations rather
// than in a buildctl `name=`, and pin the same reference it does for BuildKit.
func TestBuildExecutorRunsARenderedBuildpacksJob(t *testing.T) {
	req := build.Request{
		Project:     "shop",
		Component:   "checkout",
		Environment: "production",
		Revision:    "abc12345",
		Image:       testRepo,
		Tag:         testTag,
	}
	manifest, err := buildpacks.Config{Namespace: testNS}.Workload(req)
	if err != nil {
		t.Fatalf("Workload: %v", err)
	}

	logs := "Paketo Buildpack for Node.js 1.2.3\nSaving " + testRepo + "...\n" +
		"kelson: pushed " + testRepo + "@" + overflowDgst + "\n"
	cli := newFakeBuildCluster(t, logs)

	ex := kube.NewBuildExecutor(cli)
	ctx := context.Background()
	name, err := ex.Submit(ctx, manifest)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	job, err := cli.BatchV1().Jobs(testNS).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get submitted job: %v", err)
	}
	if got := job.Spec.Template.Spec.Containers[0].Name; got != "buildpack" {
		t.Fatalf("build container = %q; the executor streams whatever the Job names, so this is the name it must follow", got)
	}
	if _, err := cli.CoreV1().Pods(testNS).Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-pod1", Namespace: testNS, Labels: map[string]string{"job-name": name}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create build pod: %v", err)
	}

	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if _, err := cli.BatchV1().Jobs(testNS).UpdateStatus(ctx, job, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update status: %v", err)
	}

	var w bytes.Buffer
	res, err := ex.Wait(ctx, name, &w)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if res.Reference != testRepo+"@"+overflowDgst {
		t.Errorf("Reference = %q, want %q", res.Reference, testRepo+"@"+overflowDgst)
	}
	if res.Tag != testTag {
		t.Errorf("Tag = %q, want %q", res.Tag, testTag)
	}
	if !strings.Contains(w.String(), "Paketo Buildpack") {
		t.Errorf("the lifecycle's output must reach the caller: %q", w.String())
	}
}
