package observation

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

var classifyCtx = context.Background()

func classifyDeployment(dep *unstructured.Unstructured, pods []*unstructured.Unstructured) Verdict {
	return classify(classifyCtx, dep, pods, nil, DefaultMaxLogLines)
}

// --- Acceptance criterion 1 (part 1): CrashLoopBackOff is reported as such ---

func TestCrashLoopBackOffReportedAsSuch(t *testing.T) {
	dep := deployment(testNS, testApp, nil)
	p := pod(testNS, "web-0", func(m map[string]any) {
		podCrashLoop(m)
	})

	v := classifyDeployment(dep, []*unstructured.Unstructured{p})

	if v.Healthy {
		t.Fatal("a CrashLoopBackOff pod must not yield a healthy verdict")
	}
	if v.Code != CodeCrashLoopBackOff {
		t.Fatalf("Code = %q, want %q", v.Code, CodeCrashLoopBackOff)
	}
	if !IsFailure(v.Code) {
		t.Fatal("expected CodeCrashLoopBackOff to be a failure code")
	}
	if !strings.Contains(v.Reason, "CrashLoopBackOff") {
		t.Fatalf("Reason = %q, want it to name CrashLoopBackOff", v.Reason)
	}
	if v.Remediation == "" {
		t.Fatal("expected a remediation hint on a failure verdict")
	}
}

// --- Acceptance criterion 1 (part 2): the container logs are attached -------

func TestCrashLoopBackOffReportCarriesContainerLogs(t *testing.T) {
	dep := deployment(testNS, testApp, nil)
	p := pod(testNS, "web-0", func(m map[string]any) { podCrashLoop(m) })

	logs := func(_ context.Context, ns, podName, container string, _ int) (string, error) {
		if ns != testNS || podName != "web-0" || container != "web" {
			t.Fatalf("logs called with ns=%q pod=%q container=%q", ns, podName, container)
		}
		return "panic: nil pointer dereference", nil
	}

	v := classify(classifyCtx, dep, []*unstructured.Unstructured{p}, logs, DefaultMaxLogLines)

	if len(v.Containers) != 1 {
		t.Fatalf("got %d failing containers, want 1", len(v.Containers))
	}
	c := v.Containers[0]
	if c.Name != "web" {
		t.Fatalf("container Name = %q, want web", c.Name)
	}
	if c.Pod != "web-0" {
		t.Fatalf("container Pod = %q, want web-0", c.Pod)
	}
	if c.Code != CodeCrashLoopBackOff {
		t.Fatalf("container Code = %q, want crash-loop-back-off", c.Code)
	}
	if c.Logs != "panic: nil pointer dereference" {
		t.Fatalf("container Logs = %q, want the fetched logs", c.Logs)
	}
	if c.LogError != "" {
		t.Fatalf("container LogError = %q, want empty", c.LogError)
	}
}

// --- Acceptance criterion 2: the conflation is prevented --------------------

// TestNoDeploymentAppearsSuccessfulWhilePodsAreFailing is the exact failure
// mode issue #53 exists to prevent: a Deployment whose own status reads
// satisfied (generation observed, Available=True, Progressing=True) must NOT
// be reported healthy while one of its pods is in CrashLoopBackOff.
func TestNoDeploymentAppearsSuccessfulWhilePodsAreFailing(t *testing.T) {
	// A deployment that looks fully satisfied at the conditions level.
	dep := deployment(testNS, testApp, func(m map[string]any) {
		st := m["status"].(map[string]any)
		st["observedGeneration"] = int64(1)
		st["availableReplicas"] = int64(3)
		st["readyReplicas"] = int64(3)
		st["replicas"] = int64(3)
	})
	p := pod(testNS, "web-2", func(m map[string]any) { podCrashLoop(m) })

	v := classifyDeployment(dep, []*unstructured.Unstructured{p})

	if v.Healthy {
		t.Fatal("no deployment may appear healthy while a pod is failing: " + v.String())
	}
	if v.Code != CodeCrashLoopBackOff {
		t.Fatalf("Code = %q, want crash-loop-back-off", v.Code)
	}
}

// --- Additional coverage: ImagePullBackOff ----------------------------------

func TestImagePullBackOffReported(t *testing.T) {
	dep := deployment(testNS, testApp, nil)
	p := pod(testNS, "web-0", func(m map[string]any) {
		setContainerStatus(m, container("web", false, 0, map[string]any{
			"waiting": map[string]any{"reason": "ImagePullBackOff", "message": "Back-off pulling image"},
		}))
	})

	v := classifyDeployment(dep, []*unstructured.Unstructured{p})

	if v.Healthy {
		t.Fatal("ImagePullBackOff must not be healthy")
	}
	if v.Code != CodeImagePullBackOff {
		t.Fatalf("Code = %q, want image-pull-back-off", v.Code)
	}
	// ErrImagePull is the same class and must map the same way.
	p2 := pod(testNS, "web-1", func(m map[string]any) {
		setContainerStatus(m, container("web", false, 0, map[string]any{
			"waiting": map[string]any{"reason": "ErrImagePull", "message": "pull access denied"},
		}))
	})
	if v2 := classifyDeployment(dep, []*unstructured.Unstructured{p2}); v2.Code != CodeImagePullBackOff {
		t.Fatalf("ErrImagePull Code = %q, want image-pull-back-off", v2.Code)
	}
}

// --- Additional coverage: insufficient resources / FailedScheduling ---------

func TestFailedSchedulingInsufficientResourcesReported(t *testing.T) {
	dep := deployment(testNS, testApp, nil)
	p := pod(testNS, "web-0", func(m map[string]any) {
		podUnschedulable(m, "0/3 nodes are available: 3 Insufficient cpu, 1 Insufficient memory.")
	})

	v := classifyDeployment(dep, []*unstructured.Unstructured{p})

	if v.Healthy {
		t.Fatal("an unschedulable pod must not be healthy")
	}
	if v.Code != CodeInsufficientResources {
		t.Fatalf("Code = %q, want insufficient-resources", v.Code)
	}
}

func TestFailedSchedulingNamedForReason(t *testing.T) {
	dep := deployment(testNS, testApp, nil)
	p := pod(testNS, "web-0", func(m map[string]any) {
		podUnschedulable(m, "0/2 nodes are available: 2 node(s) didn't match node selector.")
	})

	v := classifyDeployment(dep, []*unstructured.Unstructured{p})

	if v.Code != CodeSchedulingFailed {
		t.Fatalf("Code = %q, want scheduling-failed", v.Code)
	}
}

// --- Additional coverage: a genuinely healthy rollout -----------------------

func TestGenuineHealthyRolloutIsHealthy(t *testing.T) {
	dep := deployment(testNS, testApp, nil)
	p1 := pod(testNS, "web-0", nil)
	p2 := pod(testNS, "web-1", nil)

	v := classifyDeployment(dep, []*unstructured.Unstructured{p1, p2})

	if !v.Healthy {
		t.Fatalf("a genuinely healthy rollout must be healthy, got %q", v.Code)
	}
	if v.Code != CodeHealthy {
		t.Fatalf("Code = %q, want healthy", v.Code)
	}
	if v.Remediation != "" {
		t.Fatalf("a healthy verdict must carry no remediation, got %q", v.Remediation)
	}
}

// --- Additional coverage: heuristic detections ------------------------------

func TestRunningNotReadyContainerIsFailingProbe(t *testing.T) {
	dep := deployment(testNS, testApp, nil)
	p := pod(testNS, "web-0", func(m map[string]any) {
		setContainerStatus(m, container("web", false, 0, map[string]any{"running": map[string]any{}}))
		m["status"].(map[string]any)["conditions"] = []any{
			cond("Ready", "False", "", ""),
			cond("PodScheduled", "True", "", ""),
		}
	})

	v := classifyDeployment(dep, []*unstructured.Unstructured{p})

	if v.Healthy {
		t.Fatal("a running-but-not-ready container must not be healthy")
	}
	if v.Code != CodeFailingProbe {
		t.Fatalf("Code = %q, want failing-probe", v.Code)
	}
}

// --- Additional coverage: deterministic ordering ----------------------------

func TestMostSevereFailureWins(t *testing.T) {
	dep := deployment(testNS, testApp, nil)
	crash := pod(testNS, "web-0", func(m map[string]any) { podCrashLoop(m) })
	pull := pod(testNS, "web-1", func(m map[string]any) {
		setContainerStatus(m, container("web", false, 0, map[string]any{
			"waiting": map[string]any{"reason": "ImagePullBackOff", "message": "Back-off"},
		}))
	})

	v := classifyDeployment(dep, []*unstructured.Unstructured{pull, crash})

	if v.Code != CodeCrashLoopBackOff {
		t.Fatalf("Code = %q, want the most severe failure (crash-loop-back-off)", v.Code)
	}
}

// --- Logless and log-error behaviour ----------------------------------------

func TestVerdictReturnedWithoutLogSource(t *testing.T) {
	// A caller with no log access must still get the verdict, not an error.
	dep := deployment(testNS, testApp, nil)
	p := pod(testNS, "web-0", func(m map[string]any) { podCrashLoop(m) })

	v := classifyDeployment(dep, []*unstructured.Unstructured{p})

	if v.Healthy {
		t.Fatal("verdict must not be healthy")
	}
	if v.Code != CodeCrashLoopBackOff {
		t.Fatalf("Code = %q", v.Code)
	}
	if len(v.Containers) == 0 {
		t.Fatal("a logless verdict should still list the failing container")
	}
	if v.Containers[0].Logs != "" || v.Containers[0].LogError != "" {
		t.Fatalf("no logs expected: Logs=%q LogError=%q", v.Containers[0].Logs, v.Containers[0].LogError)
	}
}

func TestLogFetchErrorRecordedNotFatal(t *testing.T) {
	dep := deployment(testNS, testApp, nil)
	p := pod(testNS, "web-0", func(m map[string]any) { podCrashLoop(m) })

	logs := func(context.Context, string, string, string, int) (string, error) {
		return "", context.DeadlineExceeded
	}

	v := classify(classifyCtx, dep, []*unstructured.Unstructured{p}, logs, DefaultMaxLogLines)

	if v.Code != CodeCrashLoopBackOff {
		t.Fatalf("a log fetch error must not change the verdict, got %q", v.Code)
	}
	if len(v.Containers) != 1 || v.Containers[0].LogError == "" {
		t.Fatalf("expected the log error recorded on the container, got %+v", v.Containers)
	}
}

// --- pod mutation helpers ---------------------------------------------------

func podCrashLoop(m map[string]any) {
	setContainerStatus(m, container("web", false, 3, map[string]any{
		"waiting": map[string]any{"reason": "CrashLoopBackOff", "message": "back-off restarting failed container"},
	}))
}

func podUnschedulable(m map[string]any, msg string) {
	m["status"] = map[string]any{
		"phase": "Pending",
		"conditions": []any{
			cond("PodScheduled", "False", "Unschedulable", msg),
		},
	}
}

func setContainerStatus(m map[string]any, cs map[string]any) {
	m["status"].(map[string]any)["containerStatuses"] = []any{cs}
}
