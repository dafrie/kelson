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

// jobManifest renders a build Job manifest matching the test constants, with a
// buildctl command that carries the output image/name (workload.go renders the
// real shape).
func jobManifest(t *testing.T) []byte {
	t.Helper()
	job := &batchv1.Job{
		TypeMeta:   metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{Name: testJobName, Namespace: testNS},
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
