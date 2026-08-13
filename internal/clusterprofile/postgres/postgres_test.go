package postgres

import (
	"strings"
	"testing"

	"github.com/dafrie/kelson/internal/clusterprofile"
)

// installed builds a profile with a CNPG operator at the given version serving
// the given CRDs.
func installed(version string, crds ...string) clusterprofile.ClusterProfile {
	return clusterprofile.ClusterProfile{
		CloudNativePG: &clusterprofile.CloudNativePG{
			Version: version, Namespace: "cnpg-system", CRDs: crds,
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

// TestLatestOperatorSupportsEverything is the baseline posture: kelson targets
// the latest CNPG, so a cluster running it answers yes to every declarative
// capability and can host every preset.
func TestLatestOperatorSupportsEverything(t *testing.T) {
	p := installed("1.30.0", "backups", "clusters", "databases", "poolers")
	for cap, r := range outcomes(t, p) {
		if r.Outcome != clusterprofile.OutcomeYes {
			t.Errorf("%s = %s (%s), want yes on the latest operator", cap, r.Outcome, r.Message)
		}
	}
	for _, preset := range []Preset{PresetShared, PresetSmall, PresetHASmall, PresetHAMedium, PresetBranch} {
		if v := SupportsPreset(p, preset); v.Outcome != clusterprofile.OutcomeYes {
			t.Errorf("preset %s = %s (%s), want yes", preset, v.Outcome, v.Message)
		}
	}
}

// TestTooOldForSharedPreset is the issue's reporting requirement: a detected
// CNPG that is too old for the requested preset is reported as such, with the
// detected version and the floor both named, and the remediation is an upgrade
// — never a second operator.
func TestTooOldForSharedPreset(t *testing.T) {
	p := installed("1.24.2", "clusters", "poolers")

	r := Supports(p, CapabilityDeclarativeDatabases)
	if r.Outcome != clusterprofile.OutcomeNo {
		t.Fatalf("declarative databases on 1.24.2 = %s, want no", r.Outcome)
	}
	for _, want := range []string{"1.25.0", "1.24.2", "too old", "upgrade"} {
		if !strings.Contains(r.Message, want) {
			t.Errorf("message %q does not mention %q", r.Message, want)
		}
	}
	if r.Found != "1.24.2" || r.Since != "1.25.0" {
		t.Errorf("result = %+v, want found 1.24.2 since 1.25.0", r)
	}

	v := SupportsPreset(p, PresetShared)
	if v.Outcome != clusterprofile.OutcomeNo {
		t.Fatalf("shared preset on 1.24.2 = %s, want no", v.Outcome)
	}
	if len(v.Blocking) != 1 || v.Blocking[0] != CapabilityDeclarativeDatabases {
		t.Errorf("blocking = %v, want just declarative-databases", v.Blocking)
	}
	// Dedicated presets are unaffected: 1.24 runs a Cluster with managed roles
	// perfectly well, and reporting otherwise would refuse a working cluster.
	if v := SupportsPreset(p, PresetSmall); v.Outcome != clusterprofile.OutcomeYes {
		t.Errorf("small preset on 1.24.2 = %s (%s), want yes", v.Outcome, v.Message)
	}
}

// TestCapabilityFloorsAreIndependent: 1.25 has the Database CRD but not the
// declarative schemas and extensions that landed in 1.26, and the report says
// exactly which one is missing rather than collapsing to one boolean.
func TestCapabilityFloorsAreIndependent(t *testing.T) {
	got := outcomes(t, installed("1.25.0", "clusters", "databases", "poolers"))
	if got[CapabilityDeclarativeDatabases].Outcome != clusterprofile.OutcomeYes {
		t.Errorf("databases on 1.25.0 = %s", got[CapabilityDeclarativeDatabases].Outcome)
	}
	schemas := got[CapabilityDeclarativeSchemas]
	if schemas.Outcome != clusterprofile.OutcomeNo {
		t.Fatalf("schemas on 1.25.0 = %s, want no", schemas.Outcome)
	}
	for _, want := range []string{"supported from CloudNativePG 1.26.0", "detected 1.25.0"} {
		if !strings.Contains(schemas.Message, want) {
			t.Errorf("message %q does not read as \"supported from X, detected Y\" (missing %q)", schemas.Message, want)
		}
	}
}

// TestManagedRolesFloor: declarative role management arrived in 1.20, and it is
// how kelson renders application credentials — an operator below that floor
// blocks every preset, not only the shared one.
func TestManagedRolesFloor(t *testing.T) {
	p := installed("1.19.1", "clusters")
	if r := Supports(p, CapabilityManagedRoles); r.Outcome != clusterprofile.OutcomeNo || r.Since != "1.20.0" {
		t.Fatalf("managed roles on 1.19.1 = %+v, want no with floor 1.20.0", r)
	}
	v := SupportsPreset(p, PresetSmall)
	if v.Outcome != clusterprofile.OutcomeNo {
		t.Fatalf("small preset on 1.19.1 = %s, want no", v.Outcome)
	}
	if !containsCapability(v.Blocking, CapabilityManagedRoles) {
		t.Errorf("blocking = %v, want managed-roles among them", v.Blocking)
	}
}

// TestAbsentIsNoAndSaysInstall: no CNPG at all is a definite no, and it is the
// only case whose remediation is an install — kelson never installs it as a
// side effect, so the message points at the explicit path (ADR-0005, #90).
func TestAbsentIsNoAndSaysInstall(t *testing.T) {
	p := clusterprofile.ClusterProfile{}
	for cap, r := range outcomes(t, p) {
		if r.Outcome != clusterprofile.OutcomeNo {
			t.Errorf("%s without CNPG = %s, want no", cap, r.Outcome)
		}
		if !strings.Contains(r.Message, "not installed") {
			t.Errorf("%s message %q does not say CNPG is absent", cap, r.Message)
		}
	}
	if v := SupportsPreset(p, PresetShared); v.Outcome != clusterprofile.OutcomeNo {
		t.Errorf("shared preset without CNPG = %s, want no", v.Outcome)
	}
}

// TestNeverSuggestsASecondOperator: with CNPG present every remediation must be
// an upgrade of the operator that is there. Suggesting an install into a
// cluster that already has one is the specific harm issue #90 calls out — CNPG
// is cluster-scoped and a second install fights the first.
func TestNeverSuggestsASecondOperator(t *testing.T) {
	for _, p := range []clusterprofile.ClusterProfile{
		installed("1.19.1", "clusters"),
		installed("1.24.2", "clusters", "poolers"),
		installed("1.25.0", "clusters", "databases"),
	} {
		for cap, r := range outcomes(t, p) {
			if r.Outcome != clusterprofile.OutcomeNo {
				continue
			}
			if strings.Contains(r.Message, "install the operator") || strings.Contains(r.Message, "not installed") {
				t.Errorf("%s on %s tells the user to install a second operator: %q", cap, r.Found, r.Message)
			}
		}
	}
}

// TestGapIsUnknown: a detection gap over cnpg (no RBAC to read the operator
// Deployment) is a third answer, and the reason travels into the message so the
// caller can name the permission that would settle it.
func TestGapIsUnknown(t *testing.T) {
	p := installed("", "clusters", "databases")
	p.Incomplete = []clusterprofile.Gap{{
		Field:  "cnpg.version",
		Reason: "forbidden: needs get,list on deployments.apps",
	}}
	for cap, r := range outcomes(t, p) {
		if r.Outcome != clusterprofile.OutcomeUnknown {
			t.Errorf("%s behind a gap = %s, want unknown", cap, r.Outcome)
		}
		if !strings.Contains(r.Message, "deployments.apps") {
			t.Errorf("%s message %q does not carry the gap reason", cap, r.Message)
		}
	}
	v := SupportsPreset(p, PresetShared)
	if v.Outcome != clusterprofile.OutcomeUnknown {
		t.Fatalf("shared preset behind a gap = %s, want unknown", v.Outcome)
	}
}

// TestUnreadableVersionFallsBackToServedCRDs: an operator whose version could
// not be read still serves resources, and a served Database CRD *is* the
// declarative-databases capability. Capabilities that live in CR fields cannot
// be inferred that way and stay unknown.
func TestUnreadableVersionFallsBackToServedCRDs(t *testing.T) {
	got := outcomes(t, installed("", "clusters", "databases", "poolers"))
	if got[CapabilityDeclarativeDatabases].Outcome != clusterprofile.OutcomeYes {
		t.Errorf("databases with a served CRD = %s, want yes", got[CapabilityDeclarativeDatabases].Outcome)
	}
	if got[CapabilityCluster].Outcome != clusterprofile.OutcomeYes {
		t.Errorf("cluster with a served CRD = %s, want yes", got[CapabilityCluster].Outcome)
	}
	for _, cap := range []Capability{CapabilityManagedRoles, CapabilityDeclarativeSchemas} {
		if got[cap].Outcome != clusterprofile.OutcomeUnknown {
			t.Errorf("%s = %s, want unknown: a CR field is invisible to discovery", cap, got[cap].Outcome)
		}
		if !strings.Contains(got[cap].Message, "version could not be read") {
			t.Errorf("%s message %q does not say why it is unknown", cap, got[cap].Message)
		}
	}
}

// TestUnparseableVersionIsUnknown: a tag like "latest" is not a version, and
// guessing either way would be worse than saying so.
func TestUnparseableVersionIsUnknown(t *testing.T) {
	r := Supports(installed("latest"), CapabilityDeclarativeSchemas)
	if r.Outcome != clusterprofile.OutcomeUnknown {
		t.Fatalf("outcome on an unparseable version = %s, want unknown", r.Outcome)
	}
	if !strings.Contains(r.Message, "could not be parsed") {
		t.Errorf("message %q does not explain the unparseable version", r.Message)
	}
}

// TestServedCRDsOverrideANewEnoughVersion: an operator new enough for a CRD the
// API server does not serve (a partial CRD apply) is a no, because the served
// set is what a manifest actually meets.
func TestServedCRDsOverrideANewEnoughVersion(t *testing.T) {
	r := Supports(installed("1.26.0", "clusters", "poolers"), CapabilityDeclarativeDatabases)
	if r.Outcome != clusterprofile.OutcomeNo {
		t.Fatalf("outcome = %s, want no when databases.postgresql.cnpg.io is not served", r.Outcome)
	}
	if !strings.Contains(r.Message, "databases.postgresql.cnpg.io") {
		t.Errorf("message %q does not name the missing resource", r.Message)
	}
}

// TestUnknownNamesAreUnknown, not silent yeses: an untracked capability or
// preset name must never read as a pass.
func TestUnknownNamesAreUnknown(t *testing.T) {
	p := installed("1.30.0", "clusters", "databases")
	if r := Supports(p, Capability("pgbouncer-magic")); r.Outcome != clusterprofile.OutcomeUnknown {
		t.Errorf("unknown capability = %s, want unknown", r.Outcome)
	}
	if v := SupportsPreset(p, Preset("enormous")); v.Outcome != clusterprofile.OutcomeUnknown {
		t.Errorf("unknown preset = %s, want unknown", v.Outcome)
	}
}

// TestPresetsAreAllTracked keeps the preset table complete against ADR-0007:
// a preset a spec can name but this table cannot judge would answer Unknown
// forever.
func TestPresetsAreAllTracked(t *testing.T) {
	for _, preset := range []Preset{PresetShared, PresetSmall, PresetHASmall, PresetHAMedium, PresetBranch} {
		if _, ok := presetNeeds[preset]; !ok {
			t.Errorf("preset %q has no capability requirements", preset)
		}
	}
}

// TestClusterCapabilityFloorComesFromTheMatrix: the Cluster API's floor is the
// declared support matrix's cnpg row, so bumping support in one place moves
// this judgement with it.
func TestClusterCapabilityFloorComesFromTheMatrix(t *testing.T) {
	r := Supports(installed("1.30.0", "clusters"), CapabilityCluster)
	if r.Since != "1.23.0" {
		t.Errorf("cluster floor = %q, want the support matrix's cnpg minimum", r.Since)
	}
}

func containsCapability(list []Capability, want Capability) bool {
	for _, c := range list {
		if c == want {
			return true
		}
	}
	return false
}
