// Package storage judges a cluster's database-branching capability from its
// ClusterProfile alone, and reports the judgement explicitly rather than
// letting branching degrade silently (issue #108, ADR-0007).
//
// Database branching (M9) works by snapshotting a dedicated CNPG cluster and
// bootstrapping a new one from it. Whether that snapshot is cheap depends on
// the storage driver: Ceph RBD, ZFS and LVM-thin give thin copy-on-write
// clones, EBS and GCE PD give a full-size restore, and the local-path
// provisioner that the planned minimal k3s bootstrap (M5) defaults to has no
// snapshot driver at all (issue #142: that bootstrap path has no `kelson up`
// command yet). On that bootstrap path snapshot-based branching cannot work,
// and the decision is to say so at the point of use instead of quietly
// falling back.
//
// The clusterprofile.StorageClass carries the enablement: VolumeSnapshotClass
// is non-empty exactly when a CSI snapshot class matches that class's
// provisioner (detected in internal/clusterprofile/detect). This package turns
// that field into a decision and a message a human who has not read the ADR
// can act on.
//
// # Capable, not capable, and unknown are three different things
//
// A storage class with an empty VolumeSnapshotClass is "we checked and it
// cannot snapshot"; a profile whose storageClasses sat behind a detection Gap
// is "we could not check" — collapsing those into one answer is how a boring
// capability check becomes a branch that silently degrades later. The verdict
// is therefore a [clusterprofile.Outcome], the codebase's one vocabulary for
// that (issue #144, and see its package doc for the discipline); here the
// question is "can this cluster's storage snapshot?", so:
//
//   - OutcomeYes — a storage class could be judged and has a snapshot class.
//   - OutcomeNo — classes could be read and the relevant class cannot
//     snapshot: the restore-based fallback is the only path.
//   - OutcomeUnknown — storageClasses hid behind a detection Gap: not
//     confirmable, so neither reported as fine nor as a failure, left for the
//     caller to decide. [clusterprofile.ClusterProfile.GapFor] supplies the
//     reason, so the nudge names the permission that would settle it.
//
// This package is pure and imports nothing beyond the clusterprofile type: no
// client-go, no network, no clock, so internal/renderer and preview can import
// it without breaking purity (issue #20). Nothing wires it into a command yet;
// it exists to be consumed once branching lands.
package storage

import "github.com/dafrie/kelson/internal/clusterprofile"

// gapField is the ClusterProfile.Incomplete field that hides storage-class
// detection: VolumeSnapshotClass is empty for an unreadable class, exactly what
// marks the Unknown outcome, so the Gap must be consulted before reading it.
const gapField = "storageClasses"

// SnapshotClasses returns the storage classes that can snapshot — those with a
// matching VolumeSnapshotClass. This answers "which classes support branching"
// directly, without the gap judgement; pair it with CanSnapshot for the
// three-way verdict.
func SnapshotClasses(p clusterprofile.ClusterProfile) []clusterprofile.StorageClass {
	var out []clusterprofile.StorageClass
	for _, c := range p.StorageClasses {
		if c.VolumeSnapshotClass != "" {
			out = append(out, c)
		}
	}
	return out
}

// DefaultClass returns the class branching would pick: the one the cluster
// marks default, else the first detected, mirroring DefaultIngressClass's
// "the default is a decision the owner made, list order is an accident"
// reasoning. Nil when the profile reports no storage class at all.
func DefaultClass(p clusterprofile.ClusterProfile) *clusterprofile.StorageClass {
	for i := range p.StorageClasses {
		if p.StorageClasses[i].Default {
			return &p.StorageClasses[i]
		}
	}
	if len(p.StorageClasses) > 0 {
		return &p.StorageClasses[0]
	}
	return nil
}

// CanSnapshot is the three-way answer to "can any class on this cluster
// snapshot?". OutcomeUnknown when detection could not look at storageClasses.
func CanSnapshot(p clusterprofile.ClusterProfile) clusterprofile.Outcome {
	if _, hidden := p.GapFor(gapField); hidden {
		return clusterprofile.OutcomeUnknown
	}
	if len(SnapshotClasses(p)) > 0 {
		return clusterprofile.OutcomeYes
	}
	return clusterprofile.OutcomeNo
}

// Verdict is the branching judgement for one storage class: the three-way
// outcome, the class it was judged on, and a message a human can act on.
type Verdict struct {
	Outcome clusterprofile.Outcome
	// Class is the class the verdict is about: the default, else the first
	// detected. Its Provisioner is what the fix message must name.
	Class *clusterprofile.StorageClass
	// SnapshotClass is the class's detected VolumeSnapshotClass, or "" when
	// none applies — the restore-based fallback is the only path.
	SnapshotClass string
	Message       string
}

// Branching decides whether snapshot-based database branching is available for
// the class kelson would use (the default, ADR-0007's bootstrap path) and
// renders the nudge that says plainly what will happen when it is not. Called
// at the point of use, never silently.
func Branching(p clusterprofile.ClusterProfile) Verdict {
	if gap, hidden := p.GapFor(gapField); hidden {
		return Verdict{
			Outcome: clusterprofile.OutcomeUnknown,
			Message: "cannot judge storage-class snapshot capability (hidden by a detection gap: " +
				gap.Reason + "); confirm the snapshot driver before relying on branch speed",
		}
	}

	cls := DefaultClass(p)
	if cls == nil {
		return Verdict{
			Outcome: clusterprofile.OutcomeNo,
			Message: "no storage class was detected, so database branching will restore from backups instead of using snapshots",
		}
	}
	if cls.VolumeSnapshotClass != "" {
		return Verdict{
			Outcome:       clusterprofile.OutcomeYes,
			Class:         cls,
			SnapshotClass: cls.VolumeSnapshotClass,
			Message: "storage class " + quoted(cls.Name) + " can snapshot via " + quoted(cls.VolumeSnapshotClass) +
				", so database branching uses snapshots",
		}
	}
	return Verdict{
		Outcome: clusterprofile.OutcomeNo,
		Class:   cls,
		Message: nudge(cls),
	}
}

// nudge is the explicit-degradation message issue #108 demands: it names the
// class and its provisioner, says plainly that branching will restore instead
// of snapshot, and points at what would fix it — written for someone who has
// not read ADR-0007.
func nudge(cls *clusterprofile.StorageClass) string {
	return "storage class " + quoted(cls.Name) + " (provisioner " + quoted(cls.Provisioner) +
		") has no volume-snapshot class, so database branching will restore from backups instead of using snapshots. " +
		"To enable snapshot-based branching, add a CSI snapshot driver and a matching VolumeSnapshotClass for that provisioner, " +
		"or make a snapshot-capable storage class (e.g. Ceph RBD, EBS or GCE PD) the default."
}

// quoted wraps a name in double quotes for a message.
func quoted(s string) string { return `"` + s + `"` }
