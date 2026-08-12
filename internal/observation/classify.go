package observation

import (
	"context"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// logFn fetches a container's logs. It is the classifier's seam to the
// LogSource so the decision logic stays independent of how logs are fetched.
type logFn func(ctx context.Context, namespace, pod, container string, limit int) (string, error)

// classify reduces a live Deployment and the pods in its selector set to a
// Verdict. It is pure and deterministic: pods are considered in name order,
// containers in declaration order, and the most severe failure wins. This is
// the function that keeps an "Applied" Deployment from reading as healthy
// while a pod is failing — the conflation issue #53 exists to prevent.
//
// A Deployment is only healthy when its own conditions agree AND every pod in
// its selector set is ready with no failing container. Either alone is not
// enough: the Availability condition is reconciled by the controller and says
// nothing about an individual pod that CrashLoopBackOff'd after scheduling.
func classify(ctx context.Context, dep *unstructured.Unstructured, pods []*unstructured.Unstructured, logs logFn, maxLines int) Verdict {
	resource := "Deployment/" + dep.GetNamespace() + "/" + dep.GetName()

	if len(pods) == 0 {
		return Verdict{
			Healthy:  false,
			Code:     CodeProgressing,
			Reason:   "no pods in the deployment's selector set yet",
			Resource: resource,
		}
	}

	var worst podHealth
	haveWorst := false
	ready := 0
	for _, pod := range pods {
		ph := classifyPod(pod)
		if ph.failing {
			if !haveWorst || severity(ph.code) < severity(worst.code) {
				worst = ph
				haveWorst = true
			}
		}
		if ph.ready {
			ready++
		}
	}

	if haveWorst {
		v := Verdict{
			Healthy:     false,
			Code:        worst.code,
			Reason:      worst.reason,
			Resource:    resource,
			Remediation: worst.code.remediation(),
			Containers:  failingContainers(ctx, pods, dep.GetNamespace(), logs, maxLines),
		}
		return v
	}

	if ready == len(pods) && deploymentAvailable(dep) {
		return Verdict{Healthy: true, Code: CodeHealthy, Resource: resource}
	}

	// Pods exist but are not all ready, or the deployment is not yet available:
	// a wait state, never reported as a failure. If the rollout exceeded its own
	// deadline, note it — the Tracker turns a persistent wait into Stuck.
	reason := "some pods are not ready or the deployment is not available yet"
	if deploymentDeadline(dep) {
		reason = deploymentDeadlineMessage(dep)
	}
	return Verdict{Healthy: false, Code: CodeProgressing, Reason: reason, Resource: resource}
}

// podHealth is one pod's classification.
type podHealth struct {
	code       Code
	failing    bool
	ready      bool
	reason     string
	containers []containerHealth
}

// containerHealth is one container's classification inside a pod.
type containerHealth struct {
	pod     string
	name    string
	code    Code
	failing bool
	reason  string
}

// classifyPod reads one pod's status into a podHealth. Scheduling wins over
// container state (an unschedulable pod never ran its containers); among
// containers, the most severe failure wins.
func classifyPod(pod *unstructured.Unstructured) podHealth {
	ph := podHealth{code: CodeHealthy}

	if code, reason := schedulingCode(pod); code != nil {
		ph.code = *code
		ph.failing = true
		ph.ready = false
		ph.reason = reason
		return ph
	}

	name := pod.GetName()
	allContainersReady := true
	statuses, _, _ := unstructured.NestedSlice(pod.Object, "status", "containerStatuses")
	for _, raw := range statuses {
		cs, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		cn, _ := cs["name"].(string)
		ready, _ := containerReady(cs)
		if !ready {
			allContainersReady = false
		}
		code, reason, failing := containerCode(cs, pod)
		ch := containerHealth{pod: name, name: cn, code: code, failing: failing, reason: reason}
		if failing && (ph.code == CodeHealthy || severity(code) < severity(ph.code)) {
			ph.code = code
			ph.failing = true
			ph.reason = reason
		}
		ph.containers = append(ph.containers, ch)
	}

	// A pod is ready only when every container is ready and the pod itself is
	// Ready. Anything less is a wait state unless a container failed outright.
	switch {
	case ph.failing:
		ph.ready = false
	case allContainersReady && podReady(pod):
		ph.ready = true
	default:
		ph.code = CodeProgressing
		ph.ready = false
	}
	return ph
}

// containerCode classifies one container status into a code. The kubelet's
// named waiting reasons are precise; reading those exact strings back is how we
// report "CrashLoopBackOff is CrashLoopBackOff, not 'deployment failed'".
func containerCode(cs map[string]any, pod *unstructured.Unstructured) (code Code, reason string, failing bool) {
	if r := waitingReason(cs, "reason"); r != "" {
		msg := waitingReason(cs, "message")
		switch r {
		case "CrashLoopBackOff":
			return CodeCrashLoopBackOff, msgOr(r, msg), true
		case "ImagePullBackOff", "ErrImagePull":
			return CodeImagePullBackOff, msgOr(r, msg), true
		}
	}

	if terminated := terminatedInfo(cs); terminated.exitCode != 0 || terminated.restarts > 0 {
		// A terminated container on a Deployment (restartPolicy Always) is a
		// crash; with any restarts it is a loop. Heuristic, not kubelet-named.
		return CodeCrashLoopBackOff, msgOr(terminated.reason, terminated.message), true
	}

	ready, _ := containerReady(cs)
	if !ready && containerRunning(cs) && podScheduled(pod) {
		// Running but not ready, though scheduled: a probe is not passing.
		return CodeFailingProbe, "probe is failing", true
	}

	if ready {
		return CodeHealthy, "", false
	}
	return CodeProgressing, "", false
}

// failingContainers collects the failing containers across pods (in pod/name
// order), attaching logs for each. Logs are best-effort: a fetch error is
// recorded on the container, never propagated into a verdict error.
func failingContainers(ctx context.Context, pods []*unstructured.Unstructured, ns string, logs logFn, maxLines int) []Container {
	var out []Container
	for _, pod := range pods {
		ph := classifyPod(pod)
		for _, c := range ph.containers {
			if !c.failing {
				continue
			}
			cont := Container{Name: c.name, Pod: c.pod, Code: c.code, Reason: c.reason}
			if logs != nil {
				if txt, err := logs(ctx, ns, c.pod, c.name, maxLines); err != nil {
					cont.LogError = err.Error()
				} else {
					cont.Logs = txt
				}
			}
			out = append(out, cont)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Pod != out[j].Pod {
			return out[i].Pod < out[j].Pod
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// --- pod/container field accessors -----------------------------------------

func containerReady(cs map[string]any) (bool, bool) {
	v, found := cs["ready"]
	if !found {
		return false, false
	}
	b, ok := v.(bool)
	return b, ok
}

func containerRunning(cs map[string]any) bool {
	_, found, _ := unstructured.NestedMap(cs, "state", "running")
	return found
}

func waitingReason(cs map[string]any, key string) string {
	if waiting, found, _ := unstructured.NestedMap(cs, "state", "waiting"); found {
		if s, ok := waiting[key].(string); ok {
			return s
		}
	}
	return ""
}

type terminatedInfoT struct {
	exitCode, restarts int64
	reason, message    string
}

func terminatedInfo(cs map[string]any) terminatedInfoT {
	var out terminatedInfoT
	out.exitCode, _, _ = unstructured.NestedInt64(cs, "state", "terminated", "exitCode")
	out.restarts, _, _ = unstructured.NestedInt64(cs, "restartCount")
	if term, found, _ := unstructured.NestedMap(cs, "state", "terminated"); found {
		out.reason, _ = term["reason"].(string)
		out.message, _ = term["message"].(string)
	}
	return out
}

func schedulingCode(pod *unstructured.Unstructured) (*Code, string) {
	conds, _, _ := unstructured.NestedSlice(pod.Object, "status", "conditions")
	for _, raw := range conds {
		c, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if c["type"] == "PodScheduled" && c["status"] == "False" && c["reason"] == "Unschedulable" {
			msg, _ := c["message"].(string)
			code := CodeSchedulingFailed
			if containsFold(msg, "insufficient") {
				code = CodeInsufficientResources
			}
			return &code, msg
		}
	}
	return nil, ""
}

func podScheduled(pod *unstructured.Unstructured) bool {
	conds, _, _ := unstructured.NestedSlice(pod.Object, "status", "conditions")
	for _, raw := range conds {
		c, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if c["type"] == "PodScheduled" && c["status"] != "True" {
			return false
		}
	}
	return true
}

func podReady(pod *unstructured.Unstructured) bool {
	conds, _, _ := unstructured.NestedSlice(pod.Object, "status", "conditions")
	for _, raw := range conds {
		c, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if c["type"] == "Ready" {
			return c["status"] == "True"
		}
	}
	return false
}

// --- deployment-level helpers ----------------------------------------------

func deploymentAvailable(dep *unstructured.Unstructured) bool {
	if !generationObserved(dep) {
		return false
	}
	return deploymentCondition(dep, "Available") == "True"
}

func deploymentDeadline(dep *unstructured.Unstructured) bool {
	conds, _, _ := unstructured.NestedSlice(dep.Object, "status", "conditions")
	for _, raw := range conds {
		c, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if c["type"] == "Progressing" && c["status"] == "False" && c["reason"] == "ProgressDeadlineExceeded" {
			return true
		}
	}
	return false
}

func deploymentDeadlineMessage(dep *unstructured.Unstructured) string {
	conds, _, _ := unstructured.NestedSlice(dep.Object, "status", "conditions")
	for _, raw := range conds {
		c, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if c["type"] == "Progressing" && c["status"] == "False" && c["reason"] == "ProgressDeadlineExceeded" {
			if msg, ok := c["message"].(string); ok && msg != "" {
				return msg
			}
			return "deployment exceeded its progress deadline"
		}
	}
	return "deployment exceeded its progress deadline"
}

func generationObserved(dep *unstructured.Unstructured) bool {
	gen, _, _ := unstructured.NestedInt64(dep.Object, "metadata", "generation")
	obs, found, _ := unstructured.NestedInt64(dep.Object, "status", "observedGeneration")
	if !found {
		return false
	}
	return obs >= gen
}

func deploymentCondition(dep *unstructured.Unstructured, condType string) string {
	conds, _, _ := unstructured.NestedSlice(dep.Object, "status", "conditions")
	for _, raw := range conds {
		c, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if c["type"] == condType {
			s, _ := c["status"].(string)
			return s
		}
	}
	return ""
}

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

func msgOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// severity ranks failing codes so the classifier picks the most actionable
// diagnosis deterministically. Lower is more severe.
func severity(c Code) int {
	switch c {
	case CodeCrashLoopBackOff:
		return 1
	case CodeImagePullBackOff:
		return 2
	case CodeFailingProbe:
		return 3
	case CodeInsufficientResources:
		return 4
	case CodeSchedulingFailed:
		return 5
	default:
		return 9
	}
}
