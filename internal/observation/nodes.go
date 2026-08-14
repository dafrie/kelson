package observation

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// The node inventory behind the Cluster screen: how many machines, how big,
// and how loaded.
//
// Capacity and allocatable come from the Node objects and are always
// answerable. Usage comes from metrics.k8s.io, which is an optional API a
// cluster may simply not serve — the k3s default does, a kubeadm cluster
// without metrics-server does not — so usage follows the tri-state discipline
// (internal/clusterprofile, issue #144): a Node with no usage carries nil, not
// zero, and [NodeInventory.UsageGap] says why. A failed metrics read degrades
// the answer, never the whole inventory.
//
// The metrics API is read through the dynamic client rather than the typed
// k8s.io/metrics module on purpose: the module would be a new dependency for
// two quantity fields (.golangci.yml pins this plane's allow-list), and
// NodeMetrics is served whether or not any Go type for it is compiled in.

// nodeMetricsGVR is metrics.k8s.io's per-node resource, served by
// metrics-server (or an implementation of the same API).
var nodeMetricsGVR = schema.GroupVersionResource{Group: "metrics.k8s.io", Version: "v1beta1", Resource: "nodes"}

// Node is one node's identity, readiness, capacity and — when the cluster can
// answer — usage.
type Node struct {
	Name           string
	Roles          []string
	KubeletVersion string
	Architecture   string
	OSImage        string
	Ready          bool

	// CPU in millicores, memory in bytes. Capacity is the machine; allocatable
	// is what the kubelet offers pods after system reservations.
	CPUCapacityMilli       int64
	CPUAllocatableMilli    int64
	MemoryCapacityBytes    int64
	MemoryAllocatableBytes int64

	// Usage from metrics.k8s.io. Nil means "not read", never "zero" — the
	// inventory's UsageGap carries the reason.
	CPUUsageMilli    *int64
	MemoryUsageBytes *int64
}

// NodeInventory is every node, sorted by name, plus the honest gap when usage
// could not be read.
type NodeInventory struct {
	Nodes []Node
	// UsageGap is why usage is missing where it is missing: the metrics API is
	// not served, the read failed, or named nodes have no sample yet. Empty
	// when every node carries usage.
	UsageGap string
}

// NodeSource reads the inventory from a live cluster.
type NodeSource struct {
	// Typed lists the nodes.
	Typed kubernetes.Interface
	// Dynamic reads metrics.k8s.io, which has no compiled-in type here.
	Dynamic dynamic.Interface
}

// Nodes returns the inventory. Only the node list itself can fail the call;
// metrics degrade into UsageGap.
func (s NodeSource) Nodes(ctx context.Context) (NodeInventory, error) {
	if s.Typed == nil {
		return NodeInventory{}, errors.New("observation: a typed client is required to list nodes")
	}
	list, err := s.Typed.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return NodeInventory{}, fmt.Errorf("observation: listing nodes: %w", err)
	}

	usage, gap := s.nodeUsage(ctx)
	inv := NodeInventory{UsageGap: gap}
	var unsampled []string
	for i := range list.Items {
		node := readNode(&list.Items[i])
		if u, ok := usage[node.Name]; ok {
			node.CPUUsageMilli = &u.cpuMilli
			node.MemoryUsageBytes = &u.memoryBytes
		} else if gap == "" {
			unsampled = append(unsampled, node.Name)
		}
		inv.Nodes = append(inv.Nodes, node)
	}
	sort.Slice(inv.Nodes, func(a, b int) bool { return inv.Nodes[a].Name < inv.Nodes[b].Name })
	if gap == "" && len(unsampled) > 0 {
		sort.Strings(unsampled)
		inv.UsageGap = "metrics.k8s.io has no sample yet for: " + strings.Join(unsampled, ", ")
	}
	return inv, nil
}

func readNode(n *corev1.Node) Node {
	node := Node{
		Name:           n.Name,
		Roles:          nodeRoles(n.Labels),
		KubeletVersion: n.Status.NodeInfo.KubeletVersion,
		Architecture:   n.Status.NodeInfo.Architecture,
		OSImage:        n.Status.NodeInfo.OSImage,
		Ready:          nodeReady(n),

		CPUCapacityMilli:       n.Status.Capacity.Cpu().MilliValue(),
		CPUAllocatableMilli:    n.Status.Allocatable.Cpu().MilliValue(),
		MemoryCapacityBytes:    n.Status.Capacity.Memory().Value(),
		MemoryAllocatableBytes: n.Status.Allocatable.Memory().Value(),
	}
	return node
}

// nodeRoles reads the node-role.kubernetes.io/<role> labels. Most workers
// carry none, which is an empty slice rather than a guess of "worker" — the
// label is the cluster's claim and kelson does not invent one.
func nodeRoles(labels map[string]string) []string {
	var roles []string
	for key := range labels {
		if role, ok := strings.CutPrefix(key, "node-role.kubernetes.io/"); ok && role != "" {
			roles = append(roles, role)
		}
	}
	sort.Strings(roles)
	return roles
}

func nodeReady(n *corev1.Node) bool {
	for _, cond := range n.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

type nodeUsage struct {
	cpuMilli    int64
	memoryBytes int64
}

// nodeUsage reads metrics.k8s.io/v1beta1 nodes. The error surface is the gap
// string: "not served" and "could not read" are different sentences, and
// neither one fails the inventory.
func (s NodeSource) nodeUsage(ctx context.Context) (map[string]nodeUsage, string) {
	if s.Dynamic == nil {
		return nil, "this build has no metrics client; usage was not read"
	}
	list, err := s.Dynamic.Resource(nodeMetricsGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, "the cluster does not serve metrics.k8s.io — install metrics-server to see live CPU and memory usage"
		}
		return nil, "reading metrics.k8s.io failed: " + err.Error()
	}
	out := make(map[string]nodeUsage, len(list.Items))
	for _, item := range list.Items {
		name := item.GetName()
		usage, ok, _ := nestedStringMap(item.Object, "usage")
		if !ok {
			continue
		}
		u := nodeUsage{}
		if cpu, err := resource.ParseQuantity(usage["cpu"]); err == nil {
			u.cpuMilli = cpu.MilliValue()
		}
		if mem, err := resource.ParseQuantity(usage["memory"]); err == nil {
			u.memoryBytes = mem.Value()
		}
		out[name] = u
	}
	return out, ""
}

// nestedStringMap reads obj[field] as map[string]string, tolerating the
// map[string]any unstructured hands back.
func nestedStringMap(obj map[string]any, field string) (map[string]string, bool, error) {
	raw, ok := obj[field].(map[string]any)
	if !ok {
		return nil, false, nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		s, ok := v.(string)
		if !ok {
			return nil, false, nil
		}
		out[k] = s
	}
	return out, true, nil
}
