package clusterprofile

import (
	"strings"
	"testing"
)

// TestProfileYAMLSurfacesCapabilities is the "surfaced to the user, not only
// consumed internally" acceptance of issues #90 and #91. `kelson profile`
// prints exactly this document, so the fields the judgements read must appear
// in it, under names a human can read without the Go type in front of them.
func TestProfileYAMLSurfacesCapabilities(t *testing.T) {
	p := ClusterProfile{
		StorageClasses: []StorageClass{{
			Name:                "standard-rwo",
			Provisioner:         "pd.csi.storage.gke.io",
			Default:             true,
			VolumeSnapshotClass: "gke-snap",
			SnapshotDriver:      "pd.csi.storage.gke.io",
			CloneCapability:     CloneFullCopy,
			CloneConfidence:     CloneConfidenceKnownDriver,
		}},
		CloudNativePG: &CloudNativePG{
			Version:   "1.26.0",
			Namespace: "cnpg-system",
			CRDs:      []string{"clusters", "databases", "poolers"},
		},
	}
	data, err := Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(data)
	for _, want := range []string{
		"snapshotDriver: pd.csi.storage.gke.io",
		"cloneCapability: full-copy",
		"cloneConfidence: known-driver",
		"cnpg:",
		"version: 1.26.0",
		"namespace: cnpg-system",
		"crds:",
		"- databases",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("profile YAML does not contain %q:\n%s", want, got)
		}
	}

	// And the document round-trips, because `kelson profile > cluster.yaml`
	// followed by `kelson render --profile cluster.yaml` is the contract.
	back, err := Unmarshal(data)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.StorageClasses[0].Capability() != CloneFullCopy || back.CloudNativePG.Version != "1.26.0" {
		t.Fatalf("round-tripped profile lost capability data: %+v", back)
	}
}

// TestOmittedCloneCapabilityIsUnknown: a hand-written profile that says nothing
// about cloning must not read as a promise. Empty is unknown, and unknown is
// not a yes.
func TestOmittedCloneCapabilityIsUnknown(t *testing.T) {
	p, err := Unmarshal([]byte("storageClasses:\n  - name: local-path\n    provisioner: rancher.io/local-path\n"))
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := p.StorageClasses[0].Capability(); got != CloneUnknown {
		t.Fatalf("omitted capability = %q, want unknown", got)
	}
}
