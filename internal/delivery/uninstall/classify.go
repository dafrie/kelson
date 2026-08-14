package uninstall

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Tier is a deletion stage. The zero value is TierRoute, which is deliberate:
// an unclassified kind must never sort into the data tier by accident.
type Tier int

const (
	// TierRoute is everything that carries traffic to the workloads. It goes
	// first so requests stop arriving at something that is disappearing,
	// instead of arriving and finding a Service with no endpoints.
	TierRoute Tier = iota
	// TierWorkload is what runs. It goes before the data services it reads, so
	// nothing is left holding a connection to a database being deleted.
	TierWorkload
	// TierConfig is everything else kelson labelled: Services, ServiceAccounts,
	// ConfigMaps, Secrets, policies, monitors, and any kind an overlay
	// contributed that this package has no opinion about.
	TierConfig
	// TierData is the irreversible tier. Everything above it is re-creatable
	// from the spec; a Postgres cluster is not.
	TierData
	// TierNamespace is the Namespace itself, deleted only when the delivery
	// plane recorded that kelson created it.
	TierNamespace
)

// String names the tier as the preview prints it.
func (t Tier) String() string {
	switch t {
	case TierRoute:
		return "Routes"
	case TierWorkload:
		return "Workloads"
	case TierConfig:
		return "Configuration"
	case TierData:
		return "Data"
	case TierNamespace:
		return "Namespace"
	default:
		return "Other"
	}
}

// Tiers is the deletion order, which is also the order the preview reads in.
var Tiers = []Tier{TierRoute, TierWorkload, TierConfig, TierData, TierNamespace}

// gk is shorthand for the GroupKind keys below.
func gk(group, kind string) schema.GroupKind { return schema.GroupKind{Group: group, Kind: kind} }

// routeKinds carry traffic. Gateway API is the only routing kelson renders
// (ADR-0003 as amended, issue #140); Ingress is here because an overlay or an
// older deployment may have left one labelled, and it carries traffic too.
var routeKinds = map[schema.GroupKind]bool{
	gk("gateway.networking.k8s.io", "HTTPRoute"):      true,
	gk("gateway.networking.k8s.io", "GRPCRoute"):      true,
	gk("gateway.networking.k8s.io", "TCPRoute"):       true,
	gk("gateway.networking.k8s.io", "TLSRoute"):       true,
	gk("gateway.networking.k8s.io", "Gateway"):        true,
	gk("gateway.networking.k8s.io", "ReferenceGrant"): true,
	gk("networking.k8s.io", "Ingress"):                true,
}

// workloadKinds run something. HelmRelease and ResourceSet are here because
// they are how kelson expresses a workload in Flux mode (ADR-0016, ADR-0017):
// deleting the HelmRelease is what stops what the chart runs.
var workloadKinds = map[schema.GroupKind]bool{
	gk("apps", "Deployment"):                     true,
	gk("apps", "StatefulSet"):                    true,
	gk("apps", "DaemonSet"):                      true,
	gk("batch", "CronJob"):                       true,
	gk("batch", "Job"):                           true,
	gk("autoscaling", "HorizontalPodAutoscaler"): true,
	gk("policy", "PodDisruptionBudget"):          true,
	gk("helm.toolkit.fluxcd.io", "HelmRelease"):  true,
	gk("fluxcd.controlplane.io", "ResourceSet"):  true,
}

// dataKinds hold state that deleting them destroys. The note is what the
// preview prints under the resource: the irreversible part of an uninstall has
// to say what it takes with it, in the same voice the rollback preview uses for
// what a rollback cannot revert (internal/delivery/rollback).
var dataKinds = map[schema.GroupKind]string{
	gk("postgresql.cnpg.io", "Cluster"): "a CloudNativePG Postgres cluster: the operator deletes the volumes it " +
		"created along with it, so the database and anything stored only in those volumes are gone for good",
	gk("valkey.io", "ValkeyCluster"): "a Valkey cache: it runs on emptyDir by design (ADR-0015), so everything " +
		"it holds is lost — which for a cache means a cold start, not lost records",
	gk("", "PersistentVolumeClaim"): "a PersistentVolumeClaim: whether the volume behind it survives is the " +
		"StorageClass's reclaim policy, not kelson's, and the default is Delete",
}

// derivedKinds are never swept, even though they carry kelson's provenance
// labels — the renderer stamps the same labels onto pod templates, so a
// ReplicaSet and every Pod under a Deployment matches the selector too.
//
// They are excluded because they are not independently kelson's: each one is
// garbage-collected by the owner that created it, which IS in the sweep.
// Listing them as separate deletions would misreport the size of the operation
// and would race the controller that is already removing them. An Endpoints or
// EndpointSlice object is the same case one level down, copied from its
// Service.
var derivedKinds = map[schema.GroupKind]bool{
	gk("", "Pod"):                           true,
	gk("", "Endpoints"):                     true,
	gk("", "Event"):                         true,
	gk("apps", "ReplicaSet"):                true,
	gk("apps", "ControllerRevision"):        true,
	gk("discovery.k8s.io", "EndpointSlice"): true,
	gk("events.k8s.io", "Event"):            true,
	gk("coordination.k8s.io", "Lease"):      true,
}

// tierOf places a kind in the deletion order. An unknown kind — anything an
// overlay contributed, anything a future renderer emits — lands in TierConfig,
// which is the safe default in both directions: it is deleted (so nothing
// kelson labelled survives an uninstall), and it is deleted after the workloads
// and before the data.
func tierOf(g schema.GroupKind) Tier {
	switch {
	case routeKinds[g]:
		return TierRoute
	case workloadKinds[g]:
		return TierWorkload
	default:
		if _, data := dataKinds[g]; data {
			return TierData
		}
		return TierConfig
	}
}

// dataNote is the irreversibility line a data resource prints in the preview,
// empty for everything else.
func dataNote(g schema.GroupKind) string { return dataKinds[g] }

// swept reports whether a kind is the sweep's business at all.
func swept(g schema.GroupKind) bool { return !derivedKinds[g] }
