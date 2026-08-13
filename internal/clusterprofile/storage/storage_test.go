package storage

import (
	"strings"
	"testing"

	"github.com/dafrie/kelson/internal/clusterprofile"
)

func localPathProfile() clusterprofile.ClusterProfile {
	return clusterprofile.ClusterProfile{
		StorageClasses: []clusterprofile.StorageClass{
			{Name: "local-path", Provisioner: "rancher.io/local-path", Default: true},
		},
	}
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
