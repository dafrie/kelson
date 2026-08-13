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
// # Snapshots existing is not the same as snapshots being cheap
//
// Issue #91 splits the question in two, and so does this package. Whether a
// snapshot can be taken at all is the VolumeSnapshotClass; what it costs is the
// CSI driver behind it, which drivers.go classifies from a maintained table
// (see there for why a table rather than a probe, and what the recorded
// confidence means). A verdict therefore names the [Mechanism] branching would
// use — thin clone, full copy, or a backup restore — because "expect seconds"
// and "expect a second full-size copy of your database" are the same feature
// only on paper.
//
// # What is not here: the backup destination
//
// ADR-0007 makes an object store the universal fallback, the thing that keeps
// branching from being a Ceph-only luxury. Nothing in kelson configures one
// yet — a destination per environment is issue #94, and the restore path that
// would use it is #100 — so no field records it and this package will not
// pretend otherwise. Where a verdict points at that fallback it points at the
// issues too, rather than implying a mechanism that exists today.
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

// Mechanism is how branching would copy the data, from ADR-0007's table. It is
// reported alongside the outcome because the outcome alone hides the difference
// between a branch that takes seconds and one that takes hours (issue #91).
type Mechanism string

const (
	// MechanismUnknown is the mechanism for a cluster whose storage could not
	// be judged: hidden by a detection gap, or an unrecognised CSI driver.
	MechanismUnknown Mechanism = "unknown"
	// MechanismThinClone is a copy-on-write clone: seconds, negligible space.
	MechanismThinClone Mechanism = "thin-snapshot-clone"
	// MechanismFullCopySnapshot is a snapshot restored into a full-size volume.
	MechanismFullCopySnapshot Mechanism = "full-copy-snapshot"
	// MechanismSnapshotUnknownCost is a snapshot whose driver is not in the
	// table: branching works, its cost is not knowable from here.
	MechanismSnapshotUnknownCost Mechanism = "snapshot-unknown-cost"
	// MechanismBackupRestore is the universal fallback for storage that cannot
	// snapshot. It is not implemented: the destination is issue #94 and the
	// restore path is #100.
	MechanismBackupRestore Mechanism = "backup-restore"
)

// Verdict is the branching judgement for one storage class: the three-way
// outcome, the class it was judged on, the mechanism branching would use, and
// a message a human can act on.
type Verdict struct {
	Outcome clusterprofile.Outcome
	// Class is the class the verdict is about: the default, else the first
	// detected. Its Provisioner is what the fix message must name.
	Class *clusterprofile.StorageClass
	// SnapshotClass is the class's detected VolumeSnapshotClass, or "" when
	// none applies — the restore-based fallback is the only path.
	SnapshotClass string
	// Mechanism is how a branch would actually be made.
	Mechanism Mechanism
	// Confidence is how the class's clone capability was determined, carried
	// through from detection so a caller can tell a table lookup from an
	// observation (issue #91).
	Confidence clusterprofile.CloneConfidence
	Message    string
}

// Branching decides whether snapshot-based database branching is available for
// the class kelson would use (the default, ADR-0007's bootstrap path) and
// renders the nudge that says plainly what will happen when it is not. Called
// at the point of use, never silently.
func Branching(p clusterprofile.ClusterProfile) Verdict {
	if gap, hidden := p.GapFor(gapField); hidden {
		return Verdict{
			Outcome:    clusterprofile.OutcomeUnknown,
			Mechanism:  MechanismUnknown,
			Confidence: clusterprofile.CloneConfidenceUnreadable,
			Message: "cannot judge storage-class snapshot capability (hidden by a detection gap: " +
				gap.Reason + "); confirm the snapshot driver before relying on branch speed",
		}
	}

	cls := DefaultClass(p)
	if cls == nil {
		return Verdict{
			Outcome:   clusterprofile.OutcomeNo,
			Mechanism: MechanismBackupRestore,
			Message: "no storage class was detected, so database branching will restore from backups instead of using snapshots. " +
				backupFallbackCaveat,
		}
	}

	v := Verdict{
		Class:         cls,
		SnapshotClass: cls.VolumeSnapshotClass,
		Confidence:    cls.CloneConfidence,
	}
	on := "storage class " + quoted(cls.Name) + " (provisioner " + quoted(cls.Provisioner) + ")"

	capability := cls.Capability()
	if capability == clusterprofile.CloneUnknown && cls.VolumeSnapshotClass == "" {
		// A profile written before #91, or by hand: no capability recorded and
		// no snapshot class. The absence of a snapshot class is itself the
		// observation (there is nothing to snapshot with), which is the same
		// rule Classify applies, so the answer is a definite no rather than an
		// unknown.
		capability = clusterprofile.CloneNone
		if v.Confidence == "" {
			v.Confidence = clusterprofile.CloneConfidenceObserved
		}
	}

	switch capability {
	case clusterprofile.CloneThin:
		v.Outcome = clusterprofile.OutcomeYes
		v.Mechanism = MechanismThinClone
		v.Message = on + " snapshots via " + quoted(cls.VolumeSnapshotClass) + " on driver " + quoted(cls.SnapshotDriver) +
			", which makes thin copy-on-write clones, so branching takes seconds and almost no extra space"
	case clusterprofile.CloneFullCopy:
		v.Outcome = clusterprofile.OutcomeYes
		v.Mechanism = MechanismFullCopySnapshot
		v.Message = on + " snapshots via " + quoted(cls.VolumeSnapshotClass) + " on driver " + quoted(cls.SnapshotDriver) +
			", so branching will use a full-copy snapshot — expect it to take time and space proportional to the database"
	case clusterprofile.CloneNone:
		v.Outcome = clusterprofile.OutcomeNo
		v.Mechanism = MechanismBackupRestore
		v.Message = nudge(cls)
	default:
		// A snapshot class exists but its driver is not in the maintained
		// table. Snapshots are possible; what they cost is not knowable from
		// here, and inventing either answer is how a branch surprises someone.
		v.Outcome = clusterprofile.OutcomeYes
		v.Mechanism = MechanismSnapshotUnknownCost
		v.Message = on + " snapshots via " + quoted(cls.VolumeSnapshotClass) + ", so branching uses snapshots, but driver " +
			quoted(cls.SnapshotDriver) + " is not in kelson's clone-capability table: whether the clone is thin or a full copy is unknown, " +
			"so plan for a full copy until it is confirmed"
	}
	return v
}

// backupFallbackCaveat is what honesty about the fallback costs: ADR-0007 makes
// an object store the universal path, but the destination (issue #94) and the
// restore mechanism (issue #100) are both unbuilt, so a verdict may point at it
// without implying it works today.
const backupFallbackCaveat = "That fallback needs an object-store backup destination, which kelson does not configure yet " +
	"(issue #94) and cannot yet restore from (issue #100)."

// nudge is the explicit-degradation message issue #108 demands: it names the
// class and its provisioner, says plainly that branching will restore instead
// of snapshot, and points at what would fix it — written for someone who has
// not read ADR-0007. This is the k3s local-path case, the default of the
// bootstrap path, so it is the message most people will meet.
func nudge(cls *clusterprofile.StorageClass) string {
	return "storage class " + quoted(cls.Name) + " (provisioner " + quoted(cls.Provisioner) +
		") has no volume-snapshot class, so database branching will restore from backups instead of using snapshots. " +
		backupFallbackCaveat + " " +
		"To enable snapshot-based branching, add a CSI snapshot driver and a matching VolumeSnapshotClass for that provisioner, " +
		"or make a snapshot-capable storage class (e.g. Ceph RBD, EBS or GCE PD) the default."
}

// quoted wraps a name in double quotes for a message.
func quoted(s string) string { return `"` + s + `"` }
