package storage

// The maintained CSI driver table (issue #91).
//
// # Why a table, and not a probe
//
// Issue #91 asks for an explicit decision between a maintained lookup table and
// runtime probing, with the confidence recorded either way. This is the v0
// decision: a table.
//
// Probing is the only way to *measure* whether a driver produces
// copy-on-write clones — you would provision a volume, snapshot it, restore
// the snapshot, and watch how long it took and how much space it consumed.
// That means writing to the cluster during what is a read-only capability
// probe (deploy/rbac/detect-clusterrole.yaml grants get/list and nothing
// else), waiting minutes on the slow drivers, and leaving PVCs behind when it
// is interrupted. Detection would stop being something you can run against
// someone else's production cluster, which is the property that makes
// `kelson profile` safe to suggest.
//
// The table's cost is that it is wrong about drivers nobody has added to it.
// That cost is paid honestly rather than hidden: a driver absent from this
// table yields CloneUnknown with CloneConfidenceUnknownDriver, never a guess
// in either direction, so the branching verdict says "we cannot tell what this
// costs" instead of promising seconds or threatening hours. Revisit when the
// unknown-driver case shows up often enough in practice to justify an opt-in
// probe that provisions a small volume with the user's consent.
//
// # Maintaining it
//
// Entries are CSI driver names exactly as the driver registers them — the
// string that appears in a VolumeSnapshotClass's `driver` and (for CSI
// provisioners) a StorageClass's `provisioner`. Add a row when a driver's
// snapshot semantics are documented by its own project, not when they seem
// likely; an absent row is a supported state.

import (
	"sort"

	"github.com/dafrie/kelson/internal/clusterprofile"
)

// driverClone maps a CSI driver to what cloning a volume on it costs. The
// three groups are ADR-0007's mechanism table made concrete.
var driverClone = map[string]clusterprofile.CloneCapability{
	// Copy-on-write clones: seconds, near-zero extra space. CephFS is
	// deliberately absent — its subvolume clones are a full data copy despite
	// the snapshot itself being cheap, which is exactly the kind of assumption
	// this table must not make on a name alone.
	"rbd.csi.ceph.com":     clusterprofile.CloneThin, // Ceph RBD snapshots and clones are CoW
	"zfs.csi.openebs.io":   clusterprofile.CloneThin, // OpenEBS ZFS LocalPV: zfs snapshot/clone
	"local.csi.openebs.io": clusterprofile.CloneThin, // OpenEBS LVM LocalPV: LVM(-thin) snapshots
	"topolvm.io":           clusterprofile.CloneThin, // TopoLVM on an LVM thin pool
	"topolvm.cybozu.com":   clusterprofile.CloneThin, // TopoLVM's pre-0.10 driver name, still deployed

	// Full-copy snapshots: the volume is restored at full size, so a branch
	// costs time and space proportional to the database.
	"ebs.csi.aws.com":           clusterprofile.CloneFullCopy,
	"pd.csi.storage.gke.io":     clusterprofile.CloneFullCopy,
	"disk.csi.azure.com":        clusterprofile.CloneFullCopy,
	"dobs.csi.digitalocean.com": clusterprofile.CloneFullCopy,
	"linodebs.csi.linode.com":   clusterprofile.CloneFullCopy,

	// No snapshot support at all. These are provisioners rather than CSI
	// drivers, which is the point: they never appear in a VolumeSnapshotClass,
	// so a class using one can only ever branch by restoring a backup. k3s
	// ships the first one, which makes it the case ADR-0007 singles out.
	"rancher.io/local-path":        clusterprofile.CloneNone,
	"kubernetes.io/no-provisioner": clusterprofile.CloneNone,
	"openebs.io/local":             clusterprofile.CloneNone,
}

// Drivers returns every driver the table knows, sorted. The table is a map, so
// there is no authoring order to preserve; sorting is what makes the generated
// reference page (internal/clusterprofile/support/gendoc) deterministic.
func Drivers() []string {
	out := make([]string, 0, len(driverClone))
	for d := range driverClone {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

// Capability returns the table's recorded capability for a driver, and whether
// the driver is listed at all. Unlisted is a supported state — the answer is
// then CloneUnknown with unknown-driver confidence, not a guess.
func Capability(driver string) (clusterprofile.CloneCapability, bool) {
	c, ok := driverClone[driver]
	return c, ok
}

// Classify answers what cloning costs on a storage class, and how confidently.
//
// provisioner is the storage class's provisioner and snapshotClass is the name
// of a VolumeSnapshotClass matching it, or "" when none does. The order of the
// rules is the honesty of the answer:
//
//  1. A provisioner the table knows cannot snapshot is CloneNone on the table's
//     authority, even if some stray snapshot class names it.
//  2. No matching snapshot class is CloneNone, observed directly — without one
//     there is no CSI snapshot to take, whatever the driver could do.
//  3. A driver in the table gives its recorded capability.
//  4. A driver not in the table gives CloneUnknown: snapshots exist, their cost
//     is not something we know, and saying so is the whole point of recording
//     confidence.
//
// Detection calls this; it is exported so a hand-written profile can be
// classified the same way, and so the table is testable on its own.
func Classify(provisioner, snapshotClass string) (clusterprofile.CloneCapability, clusterprofile.CloneConfidence) {
	if known, ok := driverClone[provisioner]; ok && known == clusterprofile.CloneNone {
		return clusterprofile.CloneNone, clusterprofile.CloneConfidenceKnownDriver
	}
	if snapshotClass == "" {
		return clusterprofile.CloneNone, clusterprofile.CloneConfidenceObserved
	}
	if known, ok := driverClone[provisioner]; ok {
		return known, clusterprofile.CloneConfidenceKnownDriver
	}
	return clusterprofile.CloneUnknown, clusterprofile.CloneConfidenceUnknownDriver
}
