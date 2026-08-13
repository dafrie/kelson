package storage

import (
	"strings"
	"testing"

	"github.com/dafrie/kelson/internal/clusterprofile"
)

func localPathProfile() clusterprofile.ClusterProfile {
	return clusterprofile.ClusterProfile{
		StorageClasses: []clusterprofile.StorageClass{
			{
				Name: "local-path", Provisioner: "rancher.io/local-path", Default: true,
				CloneCapability: clusterprofile.CloneNone,
				CloneConfidence: clusterprofile.CloneConfidenceKnownDriver,
			},
		},
	}
}

// classProfile builds a one-class profile the way detection would record it,
// so the judgement tests exercise the same field combinations a real probe
// produces (issue #91).
func classProfile(name, provisioner, snapshotClass string) clusterprofile.ClusterProfile {
	cls := clusterprofile.StorageClass{
		Name: name, Provisioner: provisioner, Default: true, VolumeSnapshotClass: snapshotClass,
	}
	if snapshotClass != "" {
		cls.SnapshotDriver = provisioner
	}
	cls.CloneCapability, cls.CloneConfidence = Classify(provisioner, snapshotClass)
	return clusterprofile.ClusterProfile{StorageClasses: []clusterprofile.StorageClass{cls}}
}

// TestLocalPathIsNotCapableAndNudges is the issue's marquee case: the default
// bootstrap path (k3s local-path, no snapshot driver). Branching must be
// reported NotCapable and the nudge must name the class and its provisioner,
// say plainly that restore replaces snapshots, and point at the fix — all
// legible to someone who has not read the ADR.
func TestLocalPathIsNotCapableAndNudges(t *testing.T) {
	v := Branching(localPathProfile())
	if v.Outcome != clusterprofile.OutcomeNo {
		t.Fatalf("local-path branching outcome = %s, want not capable", v.Outcome)
	}
	if v.Class == nil || v.Class.Name != "local-path" {
		t.Fatalf("verdict class = %+v, want local-path", v.Class)
	}
	for _, want := range []string{
		"local-path",
		"rancher.io/local-path",
		"restore",
		"snapshot",
		"VolumeSnapshotClass",
	} {
		if !strings.Contains(v.Message, want) {
			t.Errorf("nudge %q does not mention %q", v.Message, want)
		}
	}
}

// TestOutcomesAreThree guards the central contract: a class with a snapshot
// class answers yes, one without answers no, and a gap-hidden profile is a
// third thing, unknown — never silently fine and never a failure.
func TestOutcomesAreThree(t *testing.T) {
	cases := []struct {
		name string
		p    clusterprofile.ClusterProfile
		want clusterprofile.Outcome
	}{
		{"capable", clusterprofile.ClusterProfile{StorageClasses: []clusterprofile.StorageClass{
			{Name: "standard-rwo", Provisioner: "pd.csi.storage.gke.io", Default: true, VolumeSnapshotClass: "gke-snap"},
		}}, clusterprofile.OutcomeYes},
		{"not-capable", localPathProfile(), clusterprofile.OutcomeNo},
		{"no-classes", clusterprofile.ClusterProfile{}, clusterprofile.OutcomeNo},
		{"gap-unknown", clusterprofile.ClusterProfile{
			Incomplete: []clusterprofile.Gap{{Field: "storageClasses", Reason: "forbidden: needs get,list on storageclasses.storage.k8s.io"}},
		}, clusterprofile.OutcomeUnknown},
	}
	for _, c := range cases {
		if got := Branching(c.p).Outcome; got != c.want {
			t.Errorf("%s: Branching outcome = %s, want %s", c.name, got, c.want)
		}
		if got := CanSnapshot(c.p); got != c.want {
			t.Errorf("%s: CanSnapshot outcome = %s, want %s", c.name, got, c.want)
		}
	}
}

// TestGapIsUnknown: a detection Gap hiding storageClasses (e.g. no RBAC to list
// snapshot classes) must be Unknown, and the message must say the capability
// could not be judged rather than guessing.
func TestGapIsUnknown(t *testing.T) {
	p := clusterprofile.ClusterProfile{
		StorageClasses: []clusterprofile.StorageClass{
			{Name: "local-path", Provisioner: "rancher.io/local-path", Default: true},
		},
		Incomplete: []clusterprofile.Gap{
			{Field: "storageClasses", Reason: "forbidden: needs get,list on volumesnapshotclasses.snapshot.storage.k8s.io"},
		},
	}
	v := Branching(p)
	if v.Outcome != clusterprofile.OutcomeUnknown {
		t.Fatalf("gap-hidden branching outcome = %s, want unknown", v.Outcome)
	}
	if !strings.Contains(v.Message, "cannot judge") {
		t.Errorf("gap message = %q, want an unknown-verdict framing", v.Message)
	}
}

// TestCapableNamesSnapshotClass: when branching is available the message names
// the matching snapshot class, so the caller (and a human reading the output)
// can see what enables it.
func TestCapableNamesSnapshotClass(t *testing.T) {
	p := clusterprofile.ClusterProfile{StorageClasses: []clusterprofile.StorageClass{
		{Name: "standard-rwo", Provisioner: "pd.csi.storage.gke.io", Default: true, VolumeSnapshotClass: "gke-snap"},
	}}
	v := Branching(p)
	if v.Outcome != clusterprofile.OutcomeYes || v.SnapshotClass != "gke-snap" {
		t.Fatalf("verdict = %+v, want capable on gke-snap", v)
	}
	if !strings.Contains(v.Message, "gke-snap") {
		t.Errorf("capable message %q does not name the snapshot class", v.Message)
	}
}

// TestDefaultClassSelection mirrors DefaultIngressClass: the marked default
// wins, else the first detected, else nil.
func TestDefaultClassSelection(t *testing.T) {
	def := clusterprofile.ClusterProfile{StorageClasses: []clusterprofile.StorageClass{
		{Name: "first"},
		{Name: "default-on", Default: true},
	}}
	if got := DefaultClass(def); got == nil || got.Name != "default-on" {
		t.Fatalf("DefaultClass with marked default = %+v, want default-on", got)
	}

	noDefault := clusterprofile.ClusterProfile{StorageClasses: []clusterprofile.StorageClass{
		{Name: "only"},
	}}
	if got := DefaultClass(noDefault); got == nil || got.Name != "only" {
		t.Fatalf("DefaultClass without marked default = %+v, want first (only)", got)
	}

	if got := DefaultClass(clusterprofile.ClusterProfile{}); got != nil {
		t.Fatalf("DefaultClass on empty profile = %+v, want nil", got)
	}
}

// TestLocalPathSaysBackupRestoreFallback is the motivating case of issue #91:
// k3s ships local-path, which has no snapshot driver at all. The verdict must
// name the backup-restore fallback as the mechanism and be honest that the
// fallback itself is not built yet (#94 for the destination, #100 for the
// restore), rather than implying a working path.
func TestLocalPathSaysBackupRestoreFallback(t *testing.T) {
	v := Branching(localPathProfile())
	if v.Outcome != clusterprofile.OutcomeNo {
		t.Fatalf("outcome = %s, want no", v.Outcome)
	}
	if v.Mechanism != MechanismBackupRestore {
		t.Fatalf("mechanism = %s, want backup-restore", v.Mechanism)
	}
	for _, want := range []string{"restore from backups", "issue #94", "issue #100"} {
		if !strings.Contains(v.Message, want) {
			t.Errorf("message %q does not mention %q", v.Message, want)
		}
	}
}

// TestMechanismIsNamed covers the tri-state the issue's acceptance asks for —
// no snapshots, full-copy snapshots, thin snapshots — plus the fourth honest
// answer for a driver the table does not know.
func TestMechanismIsNamed(t *testing.T) {
	cases := []struct {
		name       string
		profile    clusterprofile.ClusterProfile
		outcome    clusterprofile.Outcome
		mechanism  Mechanism
		confidence clusterprofile.CloneConfidence
		says       []string
	}{
		{
			name:       "thin",
			profile:    classProfile("ceph-rbd", "rbd.csi.ceph.com", "csi-rbd-snap"),
			outcome:    clusterprofile.OutcomeYes,
			mechanism:  MechanismThinClone,
			confidence: clusterprofile.CloneConfidenceKnownDriver,
			says:       []string{"thin copy-on-write", "seconds", "rbd.csi.ceph.com"},
		},
		{
			name:       "full-copy",
			profile:    classProfile("gp3", "ebs.csi.aws.com", "ebs-snap"),
			outcome:    clusterprofile.OutcomeYes,
			mechanism:  MechanismFullCopySnapshot,
			confidence: clusterprofile.CloneConfidenceKnownDriver,
			says: []string{
				"branching will use a full-copy snapshot — expect it to take time and space proportional to the database",
			},
		},
		{
			name:       "none",
			profile:    classProfile("local-path", "rancher.io/local-path", ""),
			outcome:    clusterprofile.OutcomeNo,
			mechanism:  MechanismBackupRestore,
			confidence: clusterprofile.CloneConfidenceKnownDriver,
			says:       []string{"no volume-snapshot class"},
		},
		{
			name:       "unknown-driver",
			profile:    classProfile("mystery", "storage.example.com", "mystery-snap"),
			outcome:    clusterprofile.OutcomeYes,
			mechanism:  MechanismSnapshotUnknownCost,
			confidence: clusterprofile.CloneConfidenceUnknownDriver,
			says:       []string{"not in kelson's clone-capability table", "unknown", "plan for a full copy"},
		},
	}
	for _, c := range cases {
		v := Branching(c.profile)
		if v.Outcome != c.outcome || v.Mechanism != c.mechanism || v.Confidence != c.confidence {
			t.Errorf("%s: verdict = %s/%s/%s, want %s/%s/%s",
				c.name, v.Outcome, v.Mechanism, v.Confidence, c.outcome, c.mechanism, c.confidence)
		}
		for _, want := range c.says {
			if !strings.Contains(v.Message, want) {
				t.Errorf("%s: message %q does not mention %q", c.name, v.Message, want)
			}
		}
	}
}

// TestGapHidesTheMechanismToo: a profile whose storage classes sat behind a
// detection gap has no mechanism, and the confidence says the inputs could not
// be read rather than implying a driver was looked up.
func TestGapHidesTheMechanismToo(t *testing.T) {
	p := clusterprofile.ClusterProfile{
		Incomplete: []clusterprofile.Gap{{Field: "storageClasses", Reason: "forbidden: needs get,list on volumesnapshotclasses.snapshot.storage.k8s.io"}},
	}
	v := Branching(p)
	if v.Mechanism != MechanismUnknown || v.Confidence != clusterprofile.CloneConfidenceUnreadable {
		t.Fatalf("verdict = %+v, want unknown mechanism with unreadable confidence", v)
	}
}

// TestPreCapabilityProfileStillJudges: a profile written before #91 (or by
// hand) records no clone capability. An absent snapshot class is still a
// definite no — the absence is the observation — and a present one is still a
// yes, with the cost unknown.
func TestPreCapabilityProfileStillJudges(t *testing.T) {
	noSnapshots := clusterprofile.ClusterProfile{StorageClasses: []clusterprofile.StorageClass{
		{Name: "local-path", Provisioner: "rancher.io/local-path", Default: true},
	}}
	if v := Branching(noSnapshots); v.Outcome != clusterprofile.OutcomeNo || v.Mechanism != MechanismBackupRestore {
		t.Errorf("legacy no-snapshot profile = %s/%s, want no/backup-restore", v.Outcome, v.Mechanism)
	}
	withSnapshots := clusterprofile.ClusterProfile{StorageClasses: []clusterprofile.StorageClass{
		{Name: "standard-rwo", Provisioner: "pd.csi.storage.gke.io", Default: true, VolumeSnapshotClass: "gke-snap"},
	}}
	if v := Branching(withSnapshots); v.Outcome != clusterprofile.OutcomeYes || v.Mechanism != MechanismSnapshotUnknownCost {
		t.Errorf("legacy snapshot profile = %s/%s, want yes/snapshot-unknown-cost", v.Outcome, v.Mechanism)
	}
}

// TestSnapshotClasses answers "which classes can snapshot": only the ones with
// a volume-snapshot class, regardless of which is default.
func TestSnapshotClasses(t *testing.T) {
	p := clusterprofile.ClusterProfile{StorageClasses: []clusterprofile.StorageClass{
		{Name: "local-path", Provisioner: "rancher.io/local-path", Default: true},
		{Name: "rbd", Provisioner: "rbd.csi.ceph.com", VolumeSnapshotClass: "csi-rbd"},
	}}
	got := SnapshotClasses(p)
	if len(got) != 1 || got[0].Name != "rbd" {
		t.Fatalf("SnapshotClasses = %+v, want just rbd", got)
	}
}
