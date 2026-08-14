package api

import (
	"context"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/observation"
)

// NodeService served: the node inventory behind the Cluster screen.
//
// The seam is the same shape every cluster-facing capability here has: the
// observation plane owns the reading (internal/observation/nodes.go), this
// file only projects it onto the wire. Usage is optional per node and the gap
// travels as data — "no usage" and "we could not read usage" are different
// answers, and the response says which one the client is looking at.

// NodeReader is the node-inventory seam. observation.NodeSource implements it
// against a live cluster; the tests here supply fixtures.
type NodeReader interface {
	Nodes(ctx context.Context) (observation.NodeInventory, error)
}

// GetNodes reports every node: identity, readiness, capacity, allocatable,
// and — when the cluster serves metrics.k8s.io — live usage.
func (s *Server) GetNodes(ctx context.Context, _ *connect.Request[kelsonv1alpha1.GetNodesRequest]) (*connect.Response[kelsonv1alpha1.GetNodesResponse], error) {
	if s.nodes == nil {
		return nil, unimplemented("the node inventory")
	}
	inv, err := s.nodes.Nodes(ctx)
	if err != nil {
		return nil, fail(connect.CodeUnavailable, err)
	}
	res := &kelsonv1alpha1.GetNodesResponse{UsageGap: inv.UsageGap}
	for _, n := range inv.Nodes {
		node := &kelsonv1alpha1.NodeInfo{
			Name:                   n.Name,
			Roles:                  n.Roles,
			KubeletVersion:         n.KubeletVersion,
			Architecture:           n.Architecture,
			OsImage:                n.OSImage,
			Ready:                  n.Ready,
			CpuCapacityMilli:       n.CPUCapacityMilli,
			CpuAllocatableMilli:    n.CPUAllocatableMilli,
			MemoryCapacityBytes:    n.MemoryCapacityBytes,
			MemoryAllocatableBytes: n.MemoryAllocatableBytes,
		}
		// nil stays absent on the wire: proto3 optional is the same "not read
		// is not zero" statement the observation type makes with pointers.
		node.CpuUsageMilli = n.CPUUsageMilli
		node.MemoryUsageBytes = n.MemoryUsageBytes
		res.Nodes = append(res.Nodes, node)
	}
	return connect.NewResponse(res), nil
}
