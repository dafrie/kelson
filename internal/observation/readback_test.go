package observation

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestClassifyIsTheSameVerdictWithoutTheClient pins what the exported entry
// point is for (issue #240): kelson-controller cannot use [Probe] — its client
// is controller-runtime's, not a dynamic one — and the alternative to this was
// a second copy of the classifier over there, which is how "CrashLoopBackOff"
// comes to mean one thing in `kelson status` and another in `kubectl get
// environment`. So the exported function must produce the verdict the probe's
// own path produces, and this is what says so.
func TestClassifyIsTheSameVerdictWithoutTheClient(t *testing.T) {
	dep := deployment(testNS, testApp, nil)
	pods := []*unstructured.Unstructured{
		pod(testNS, "web-0", func(m map[string]any) { podCrashLoop(m) }),
	}

	want := classify(classifyCtx, dep, pods, nil, DefaultMaxLogLines)
	got := Classify(dep, pods)

	if got.Code != want.Code || got.Reason != want.Reason || got.Resource != want.Resource {
		t.Fatalf("Classify = %+v, want the probe's own verdict %+v", got, want)
	}
	if got.Code != CodeCrashLoopBackOff {
		t.Fatalf("Code = %q, want %q", got.Code, CodeCrashLoopBackOff)
	}
	if len(got.Containers) != 1 {
		t.Fatalf("got %d failing containers, want the pod and container named", len(got.Containers))
	}
}

// TestClassifyAttachesNoLogs is the property the controller depends on. It
// writes the verdict into an Environment's status, which is readable by anyone
// who can read the Environment, and a crash dump is exactly where a connection
// string appears. A verdict from this function must therefore carry no output
// from inside a container, whatever the package can do elsewhere.
func TestClassifyAttachesNoLogs(t *testing.T) {
	dep := deployment(testNS, testApp, nil)
	pods := []*unstructured.Unstructured{
		pod(testNS, "web-0", func(m map[string]any) { podCrashLoop(m) }),
	}

	for _, c := range Classify(dep, pods).Containers {
		if c.Logs != "" || c.LogError != "" {
			t.Errorf("container %s/%s carries logs (%q) or a log error (%q). Classify takes no "+
				"LogSource and must never grow one: its caller writes the result somewhere logs "+
				"cannot go.", c.Pod, c.Name, c.Logs, c.LogError)
		}
	}
}

// TestPodSelectorRefusesToMatchEverything is the bug this export closed.
//
// A Deployment with no spec.selector produced the empty label selector, and an
// empty label selector does not match no pods — it matches *every* pod in the
// namespace. The probe would then classify a selectorless Deployment against
// its neighbours' failures, and report somebody else's CrashLoopBackOff as this
// workload's.
func TestPodSelectorRefusesToMatchEverything(t *testing.T) {
	dep := deployment(testNS, testApp, nil)
	unstructured.RemoveNestedField(dep.Object, "spec", "selector")

	if got := PodSelector(dep); len(got) != 0 {
		t.Fatalf("PodSelector on a selectorless Deployment = %v, want nothing", got)
	}
	if got := podSelector(dep); got != "" {
		t.Fatalf("podSelector = %q, want the empty string listPods refuses to send", got)
	}

	p, err := NewProbe(ProbeConfig{Client: probeClient(dep, pod(testNS, "unrelated-0",
		func(m map[string]any) { podCrashLoop(m) }))})
	if err != nil {
		t.Fatal(err)
	}
	v, err := p.Evaluate(t.Context(), testNS, testApp)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if IsFailure(v.Code) {
		t.Fatalf("verdict = %s — a selectorless Deployment was classified against a pod it does not "+
			"own; an empty selector must list nothing, not everything", v)
	}
}
