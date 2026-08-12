// Package detect captures a ClusterProfile from a live cluster.
//
// It is deliberately a sibling of — and never imported by — internal/renderer:
// the renderer is a pure function whose ClusterProfile arrives as an input
// (issue #20, ADR-0001). Everything that talks to a cluster lives out here.
package detect

import (
	"errors"

	"github.com/dafrie/kelson/internal/clusterprofile"
)

// ErrNotImplemented is returned while live profile capture is unbuilt (the
// renderer milestone ships offline rendering only; detection lands with the
// controller, issue #20).
var ErrNotImplemented = errors.New("capturing a ClusterProfile from a live cluster is not implemented yet — it requires cluster access; supply --profile <file> instead")

// FromCluster will probe a live cluster for its capabilities (Gateway API,
// ingress classes, cert-manager, ...) and return them as a ClusterProfile.
// kubeconfig context selection will follow the usual precedence
// (flag > KUBECONFIG > in-cluster).
func FromCluster(_ string) (clusterprofile.ClusterProfile, error) {
	return clusterprofile.ClusterProfile{}, ErrNotImplemented
}
