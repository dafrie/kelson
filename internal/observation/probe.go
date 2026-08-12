package observation

import (
	"context"
	"errors"
	"fmt"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// The dynamic clients can read list only a resource whose resource name is
// registered. Deployments and Pods are core to the health gate, so the probe
// addresses them by fixed GVRs rather than asking a mapper.
var (
	deploymentGVR = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	podGVR        = schema.GroupVersionResource{Version: "v1", Resource: "pods"}
)

// Probe reads one workload's live state and computes its health verdict. It
// takes an injected dynamic client (and an optional log source) exactly the way
// the delivery adapters take their clients: nothing connects inside the
// constructor, so a Probe is testable against a fake cluster with no network.
//
// The verdict is the deep signal #53 asks for: a Deployment whose own status
// reads satisfied is NOT reported healthy while one of its pods is failing —
// the conflation the issue exists to prevent.
type Probe struct {
	client      dynamic.Interface
	logs        LogSource
	maxLogLines int
}

// ProbeConfig configures a Probe.
type ProbeConfig struct {
	// Client is the dynamic client used to read the workload and list its
	// pods. Required.
	Client dynamic.Interface
	// Logs is the optional container-log source. When nil, verdicts carry no
	// logs but are still returned (never an error for lack of logs).
	Logs LogSource
	// MaxLogLines tails at most this many log lines per container.
	// 0 means DefaultMaxLogLines.
	MaxLogLines int
}

// NewProbe validates the config and returns a Probe.
func NewProbe(cfg ProbeConfig) (*Probe, error) {
	if cfg.Client == nil {
		return nil, errors.New("observation: a dynamic client is required")
	}
	p := &Probe{client: cfg.Client, logs: cfg.Logs, maxLogLines: cfg.MaxLogLines}
	if p.maxLogLines <= 0 {
		p.maxLogLines = DefaultMaxLogLines
	}
	return p, nil
}

// Evaluate computes the health verdict for a Deployment by name/namespace. The
// workload is read through the injected dynamic client and its pods are listed
// by the Deployment's own pod selector, so the verdict reflects real pod state
// rather than the Deployment's self-reported conditions alone.
func (p *Probe) Evaluate(ctx context.Context, namespace, name string) (Verdict, error) {
	dep, err := p.client.Resource(deploymentGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return Verdict{
				Healthy:     false,
				Code:        CodeMissing,
				Reason:      fmt.Sprintf("%s/%s is not in the cluster", namespace, name),
				Resource:    res(deploymentGVR.Group, "Deployment", namespace, name),
				Remediation: CodeMissing.remediation(),
			}, nil
		}
		return Verdict{}, fmt.Errorf("observation: reading %s/%s: %w", namespace, name, err)
	}

	selector := podSelector(dep)
	pods, err := p.listPods(ctx, namespace, selector)
	if err != nil {
		return Verdict{}, err
	}

	logFn := p.logFn
	return classify(ctx, dep, pods, logFn, p.maxLogLines), nil
}

// listPods lists the pods matching the selector in the namespace, in name
// order, so the classifier's output never depends on map iteration.
func (p *Probe) listPods(ctx context.Context, namespace, selector string) ([]*unstructured.Unstructured, error) {
	list, err := p.client.Resource(podGVR).Namespace(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("observation: listing pods for %s/%s: %w", namespace, selector, err)
	}
	out := make([]*unstructured.Unstructured, 0, len(list.Items))
	for i := range list.Items {
		item := &list.Items[i]
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].GetName() < out[j].GetName()
	})
	return out, nil
}

// logFn adapts the probe's optional LogSource into a classifier input that is
// best-effort: a nil source returns "" with no error, and a failing fetch is
// recorded on the Container, never propagated as a verdict error.
func (p *Probe) logFn(ctx context.Context, namespace, pod, container string, limit int) (string, error) {
	if p.logs == nil {
		return "", nil
	}
	return p.logs.ContainerLogs(ctx, namespace, pod, container)
}

func res(group, kind, namespace, name string) string {
	if group != "" {
		kind = group + "/" + kind
	}
	if namespace != "" {
		return kind + "/" + namespace + "/" + name
	}
	return kind + "/" + name
}

// podSelector extracts the Deployment's pod selector. A workload with no
// selector cannot be matched safely; the caller should have rendered one, so
// this returns the empty selector (which lists nothing) rather than guessing.
func podSelector(dep *unstructured.Unstructured) string {
	ml, found, _ := unstructured.NestedStringMap(dep.Object, "spec", "selector", "matchLabels")
	if !found {
		return ""
	}
	return labels.Set(ml).String()
}
