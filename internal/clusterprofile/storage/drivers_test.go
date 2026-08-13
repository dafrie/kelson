package storage

import (
	"strings"
	"testing"

	"github.com/dafrie/kelson/internal/clusterprofile"
)

// TestClassifyRules pins the order the rules run in, which is where the
// honesty lives: a known non-snapshotting provisioner beats a stray snapshot
// class, a missing snapshot class is an observation rather than a lookup, and
// an unrecognised driver is never guessed at (issue #91).
func TestClassifyRules(t *testing.T) {
	cases := []struct {
		name          string
		provisioner   string
		snapshotClass string
		capability    clusterprofile.CloneCapability
		confidence    clusterprofile.CloneConfidence
	}{
		{"thin driver", "rbd.csi.ceph.com", "csi-rbd-snap", clusterprofile.CloneThin, clusterprofile.CloneConfidenceKnownDriver},
		{"full-copy driver", "ebs.csi.aws.com", "ebs-snap", clusterprofile.CloneFullCopy, clusterprofile.CloneConfidenceKnownDriver},
		{"lvm thin", "local.csi.openebs.io", "lvm-snap", clusterprofile.CloneThin, clusterprofile.CloneConfidenceKnownDriver},
		{"local-path is none by table", "rancher.io/local-path", "", clusterprofile.CloneNone, clusterprofile.CloneConfidenceKnownDriver},
		{"local-path stays none despite a stray snapshot class", "rancher.io/local-path", "odd-snap", clusterprofile.CloneNone, clusterprofile.CloneConfidenceKnownDriver},
		{"no snapshot class is observed", "ebs.csi.aws.com", "", clusterprofile.CloneNone, clusterprofile.CloneConfidenceObserved},
		{"unknown driver with snapshots", "storage.example.com", "example-snap", clusterprofile.CloneUnknown, clusterprofile.CloneConfidenceUnknownDriver},
		{"unknown driver without snapshots", "storage.example.com", "", clusterprofile.CloneNone, clusterprofile.CloneConfidenceObserved},
		{"no provisioner at all", "", "", clusterprofile.CloneNone, clusterprofile.CloneConfidenceObserved},
	}
	for _, c := range cases {
		gotCap, gotConf := Classify(c.provisioner, c.snapshotClass)
		if gotCap != c.capability || gotConf != c.confidence {
			t.Errorf("%s: Classify(%q, %q) = %s/%s, want %s/%s",
				c.name, c.provisioner, c.snapshotClass, gotCap, gotConf, c.capability, c.confidence)
		}
	}
}

// TestDriverTableEntriesAreRealDriverNames keeps the table maintainable: every
// key must look like a CSI driver (or, for the none group, a Kubernetes
// provisioner) name as the driver itself registers it — lowercase, dotted, no
// whitespace, no "csi.example" placeholders — and every value must be one of
// the three real capabilities. An entry that says "unknown" would be a row
// carrying no information.
func TestDriverTableEntriesAreRealDriverNames(t *testing.T) {
	if len(driverClone) == 0 {
		t.Fatal("the driver table is empty; classification would answer unknown for everything")
	}
	for driver, capability := range driverClone {
		switch {
		case driver == "":
			t.Error("empty driver name in the table")
		case strings.TrimSpace(driver) != driver, strings.ContainsAny(driver, " \t"):
			t.Errorf("driver %q has whitespace; CSI drivers register a bare name", driver)
		case strings.ToLower(driver) != driver:
			t.Errorf("driver %q is not lowercase; CSI driver names are DNS-style", driver)
		case !strings.Contains(driver, "."):
			t.Errorf("driver %q is not a domain-style name, so it cannot be a registered driver", driver)
		case strings.Contains(driver, "example.com"):
			t.Errorf("driver %q is a placeholder, not a real driver", driver)
		}
		switch capability {
		case clusterprofile.CloneThin, clusterprofile.CloneFullCopy, clusterprofile.CloneNone:
		default:
			t.Errorf("driver %q maps to %q, which is not a decided capability", driver, capability)
		}
	}
}

// TestDriverTableCoversTheCasesTheADRNames: ADR-0007 and issue #91 both name
// specific drivers when they explain why branching varies. Losing one of those
// rows would silently turn a documented case into an unknown.
func TestDriverTableCoversTheCasesTheADRNames(t *testing.T) {
	want := map[string]clusterprofile.CloneCapability{
		"rbd.csi.ceph.com":      clusterprofile.CloneThin,
		"zfs.csi.openebs.io":    clusterprofile.CloneThin,
		"local.csi.openebs.io":  clusterprofile.CloneThin,
		"ebs.csi.aws.com":       clusterprofile.CloneFullCopy,
		"pd.csi.storage.gke.io": clusterprofile.CloneFullCopy,
		"disk.csi.azure.com":    clusterprofile.CloneFullCopy,
		"rancher.io/local-path": clusterprofile.CloneNone,
	}
	for driver, capability := range want {
		got, ok := driverClone[driver]
		if !ok {
			t.Errorf("driver %q is named by ADR-0007 or issue #91 but is missing from the table", driver)
			continue
		}
		if got != capability {
			t.Errorf("driver %q = %s, want %s", driver, got, capability)
		}
	}
}
