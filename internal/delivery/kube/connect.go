// Package kube connects to a live cluster.
//
// It exists because everything that talks to an API server lives in the
// DELIVERY plane (docs/architecture.md): the renderer is pure and the command
// plane's lint allow-list forbids the Kubernetes client libraries outright, so
// `kelson diff --dry-run=server` cannot build its own client. It calls Connect
// and receives the two things every adapter here already takes as inputs — a
// dynamic client and a REST mapper.
//
// The adapters keep taking those as injected dependencies rather than
// connecting themselves, which is what lets their tests drive a fake cluster
// with no network. This package is the one place that turns a kubeconfig into
// them.
package kube

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
)

// Cluster is a live connection: the dynamic client used for applies and reads,
// the mapper that resolves a kind to the resource those calls need, and the
// typed clientset for the few reads the dynamic client cannot do.
type Cluster struct {
	Dynamic dynamic.Interface
	Mapper  meta.RESTMapper
	// Typed is the typed clientset. It exists because container logs are not
	// reachable through the dynamic client — observation.ClientGoLogSource
	// needs this and nothing else in-tree turns a kubeconfig into one.
	Typed kubernetes.Interface
}

// Connect resolves a kubeconfig and returns a live connection.
//
// Context selection follows the usual precedence — an explicit path wins, then
// $KUBECONFIG, then in-cluster credentials when running as a pod, then
// ~/.kube/config. Passing "" selects that default chain.
//
// The mapper is discovery-backed and caches in memory, so a preview that
// touches many kinds performs one discovery round trip rather than one per
// resource. It is not refreshed: a CRD registered after Connect is not visible
// to this connection, which is the right trade for a short-lived CLI
// invocation and something a long-running server should not reuse.
func Connect(kubeconfig string) (*Cluster, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		rules, &clientcmd.ConfigOverrides{},
	).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("kube: no usable cluster credentials: %w "+
			"(pass --kubeconfig, set $KUBECONFIG, or run inside the cluster)", err)
	}

	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kube: building the dynamic client: %w", err)
	}
	disco, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kube: building the discovery client: %w", err)
	}
	typed, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kube: building the typed client: %w", err)
	}

	return &Cluster{
		Dynamic: dyn,
		Mapper:  restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(disco)),
		Typed:   typed,
	}, nil
}
