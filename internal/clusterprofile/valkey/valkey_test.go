package valkey

import (
	"strings"
	"testing"

	"github.com/dafrie/kelson/internal/clusterprofile"
)

// installed builds a profile with a Valkey operator at the given version
// serving the given resources.
func installed(version string, crds ...string) clusterprofile.ClusterProfile {
	return clusterprofile.ClusterProfile{
		Valkey: &clusterprofile.ValkeyOperator{
			Version: version, Namespace: "valkey-operator-system", CRDs: crds,
		},
	}
}

func outcomes(t *testing.T, p clusterprofile.ClusterProfile) map[Capability]Result {
	t.Helper()
	out := map[Capability]Result{}
	for _, r := range Report(p) {
		out[r.Capability] = r
	}
	if len(out) != len(requirements) {
		t.Fatalf("Report returned %d results for %d capabilities", len(out), len(requirements))
	}
	return out
}

// TestSupportedOperatorHostsEveryPreset is the baseline: an operator at the
// declared floor serving both resources answers yes to everything, because the
// cache presets differ in shard count and sizing, never in capability.
func TestSupportedOperatorHostsEveryPreset(t *testing.T) {
	p := installed("0.5.0", "valkeyclusters", "valkeynodes")
	for capability, r := range outcomes(t, p) {
		if r.Outcome != clusterprofile.OutcomeYes {
			t.Errorf("%s = %s (%s), want yes", capability, r.Outcome, r.Message)
		}
	}
	for _, preset := range []Preset{PresetSmall, PresetHASmall, PresetHAMedium} {
		if v := SupportsPreset(p, preset); v.Outcome != clusterprofile.OutcomeYes {
			t.Errorf("preset %s = %s (%s), want yes", preset, v.Outcome, v.Message)
		}
	}
}

// TestAbsentOperatorIsNo: kelson never installs an operator as a side effect
// (ADR-0005), so the message has to be an instruction a human can follow.
func TestAbsentOperatorIsNo(t *testing.T) {
	v := SupportsPreset(clusterprofile.ClusterProfile{}, PresetSmall)
	if v.Outcome != clusterprofile.OutcomeNo {
		t.Fatalf("no operator = %s, want no", v.Outcome)
	}
	if len(v.Blocking) != len(requirements) {
		t.Errorf("every capability should block, got %v", v.Blocking)
	}
	if !strings.Contains(v.Message, "valkey-io/valkey-operator") {
		t.Errorf("the message must name the operator to install: %s", v.Message)
	}
}

// TestTooOldOperatorIsNo pins the reporting requirement shared with the
// postgres judgement: both numbers named, and the remediation an upgrade —
// never a second operator, whose CRDs would fight the first's.
func TestTooOldOperatorIsNo(t *testing.T) {
	p := installed("0.4.0", "valkeyclusters", "valkeynodes")
	r := Supports(p, CapabilityCluster)
	if r.Outcome != clusterprofile.OutcomeNo {
		t.Fatalf("0.4.0 = %s, want no", r.Outcome)
	}
	for _, want := range []string{"0.5.0", "0.4.0", "too old", "upgrade"} {
		if !strings.Contains(r.Message, want) {
			t.Errorf("message must contain %q: %s", want, r.Message)
		}
	}
}

// TestPartialCRDSetIsNo is why there are two capabilities rather than one. The
// operator runs every cache pod through a ValkeyNode; a cluster serving only
// valkeyclusters accepts the manifest kelson writes and then never creates a
// pod, which is precisely the silent half-success this check exists to catch.
func TestPartialCRDSetIsNo(t *testing.T) {
	p := installed("0.5.0", "valkeyclusters")

	if r := Supports(p, CapabilityCluster); r.Outcome != clusterprofile.OutcomeYes {
		t.Errorf("the ValkeyCluster API is served: %s (%s)", r.Outcome, r.Message)
	}
	r := Supports(p, CapabilityNodes)
	if r.Outcome != clusterprofile.OutcomeNo {
		t.Fatalf("valkeynodes unserved = %s, want no", r.Outcome)
	}
	if !strings.Contains(r.Message, "valkeynodes.valkey.io") {
		t.Errorf("the message must name the missing resource: %s", r.Message)
	}

	v := SupportsPreset(p, PresetSmall)
	if v.Outcome != clusterprofile.OutcomeNo {
		t.Fatalf("preset on a partial CRD set = %s, want no", v.Outcome)
	}
	if len(v.Blocking) != 1 || v.Blocking[0] != CapabilityNodes {
		t.Errorf("only the nodes capability should block, got %v", v.Blocking)
	}
}

// TestUnreadableVersionIsJudgedFromTheServedCRDs: both capabilities *are* a
// served resource, so discovery settles them even when the operator's version
// cannot be read. Unknown is only left when nothing could be read at all.
func TestUnreadableVersionIsJudgedFromTheServedCRDs(t *testing.T) {
	served := installed("", "valkeyclusters", "valkeynodes")
	for capability, r := range outcomes(t, served) {
		if r.Outcome != clusterprofile.OutcomeYes {
			t.Errorf("%s = %s (%s), want yes from the served resource", capability, r.Outcome, r.Message)
		}
	}

	blind := installed("")
	for capability, r := range outcomes(t, blind) {
		if r.Outcome != clusterprofile.OutcomeUnknown {
			t.Errorf("%s = %s (%s), want unknown when nothing is readable", capability, r.Outcome, r.Message)
		}
		if !strings.Contains(r.Message, "could not be read") {
			t.Errorf("the message must say the version was unreadable: %s", r.Message)
		}
	}
}

// TestUnparseableVersionSaysSo: an absent version and a version nobody can
// parse are different facts, and a human fixing it needs to know which.
func TestUnparseableVersionSaysSo(t *testing.T) {
	r := Supports(installed("nightly"), CapabilityCluster)
	if r.Outcome != clusterprofile.OutcomeUnknown {
		t.Fatalf("unparseable version = %s, want unknown", r.Outcome)
	}
	if !strings.Contains(r.Message, "could not be parsed") {
		t.Errorf("the message must distinguish unparseable from absent: %s", r.Message)
	}
}

// TestDetectionGapIsUnknown: a probe that was not allowed to look has not
// established anything, and the gap's reason is what makes that actionable.
func TestDetectionGapIsUnknown(t *testing.T) {
	p := clusterprofile.ClusterProfile{
		Incomplete: []clusterprofile.Gap{{
			Field:  "valkey.version",
			Reason: "forbidden: needs get,list on deployments.apps",
		}},
	}
	v := SupportsPreset(p, PresetSmall)
	if v.Outcome != clusterprofile.OutcomeUnknown {
		t.Fatalf("a gapped profile = %s, want unknown — absence of evidence is not a no", v.Outcome)
	}
	if !strings.Contains(v.Message, "deployments.apps") {
		t.Errorf("the message must carry the gap's reason: %s", v.Message)
	}
}

// TestUntrackedPresetIsUnknown: `shared` and `branch` are members of the shared
// preset vocabulary that describe topologies a cache does not have. This
// package does not refuse them — the renderer does, with a cache-specific
// reason — so the honest answer here is that no judgement was made.
func TestUntrackedPresetIsUnknown(t *testing.T) {
	p := installed("0.5.0", "valkeyclusters", "valkeynodes")
	for _, preset := range []Preset{PresetShared, PresetBranch, Preset("enormous")} {
		v := SupportsPreset(p, preset)
		if v.Outcome != clusterprofile.OutcomeUnknown {
			t.Errorf("preset %s = %s, want unknown", preset, v.Outcome)
		}
		if !strings.Contains(v.Message, "small, ha-small, ha-medium") {
			t.Errorf("the message must list the presets that exist: %s", v.Message)
		}
	}
}

// TestUnknownCapabilityIsNotANo: a caller asking about something this table
// does not track has not been told no.
func TestUnknownCapabilityIsNotANo(t *testing.T) {
	r := Supports(installed("0.5.0", "valkeyclusters", "valkeynodes"), Capability("tls"))
	if r.Outcome != clusterprofile.OutcomeUnknown {
		t.Fatalf("untracked capability = %s, want unknown", r.Outcome)
	}
}
