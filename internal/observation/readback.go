package observation

import (
	"context"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// The classification vocabulary, offered to a caller that already holds the
// objects (issue #240, ADR-0028 decision 1 step 6).
//
// [Probe] is this package's own reader: it takes a dynamic client, fetches the
// Deployment, lists its pods and classifies the result. kelson-controller
// cannot use it — the reconciler's clients are controller-runtime's, and the
// alternative to this seam was a second dynamic client in the controller plane
// or a second copy of the classifier there. Both were worse: the first is a
// client shape the package fence would have to be argued into, and the second
// is the thing that makes "CrashLoopBackOff" mean one thing in `kelson status`
// and another in `kubectl get environment`.
//
// So what is exported is the *decision*, not the reading. [Classify] is pure —
// no client, no context, no clock — and every caller keeps its own way of
// getting the objects.

// Classify computes the health verdict for one Deployment from objects the
// caller already read: the Deployment itself and the pods in its selector set.
//
// It is [Probe.Evaluate] with the I/O removed, and it is deterministic: pods
// are considered in the order they are given (name order is the caller's job —
// [Probe.listPods] sorts, and so must anything else), containers in declaration
// order, and the most severe failure wins.
//
// No logs. [Probe] attaches a failing container's recent output when it was
// given a LogSource, because a human debugging a rollout wants it and the
// server holds the grant to read it. A caller of this function gets containers
// named — pod, container, code, reason — and nothing from inside them, which is
// the only shape safe to write somewhere a container's own output must never
// land (an Environment's status is world-readable to anyone who can read the
// custom resource, and a crash dump is exactly where a secret leaks).
func Classify(deployment *unstructured.Unstructured, pods []*unstructured.Unstructured) Verdict {
	// The context is inert: it reaches only the log fetcher, and there is none.
	return classify(context.Background(), deployment, pods, nil, 0)
}

// PodSelector is the Deployment's `spec.selector.matchLabels` — the labels a
// caller lists pods by before handing them to [Classify].
//
// An empty result means the Deployment declares no selector, and the *only*
// correct response to that is to list nothing. It is spelled out here because
// the naive reading goes the other way: an empty label selector passed to the
// API server matches every pod in the namespace, so a caller that forwards this
// map without checking it classifies its neighbours' pods as its own.
func PodSelector(deployment *unstructured.Unstructured) map[string]string {
	ml, found, err := unstructured.NestedStringMap(deployment.Object, "spec", "selector", "matchLabels")
	if !found || err != nil {
		return nil
	}
	return ml
}
