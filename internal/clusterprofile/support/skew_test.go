package support

import (
	"strings"
	"testing"

	"github.com/dafrie/kelson/internal/clusterprofile"
)

// TestCeilingIsANoteNotARefusal is the asymmetry the ceiling exists to state: a
// cluster newer than anything kelson has tested stays supported. Refusing it
// would break a working cluster to prevent something kelson has no evidence of.
func TestCeilingIsANoteNotARefusal(t *testing.T) {
	comp, ok := Lookup("kubernetes")
	if !ok || comp.Tested == "" {
		t.Fatal("the kubernetes row must declare a tested ceiling for this test to mean anything")
	}
	p := clusterprofile.ClusterProfile{Kubernetes: &clusterprofile.Kubernetes{Version: "v1.99.0"}}
	res := findOutcome(t, Check(p), "kubernetes", clusterprofile.OutcomeYes)

	if !res.NewerThanTested {
		t.Fatal("v1.99.0 is above the ceiling but was not flagged as newer than tested")
	}
	if Check(p).Degraded() {
		t.Error("a cluster newer than tested must not count as degraded")
	}
	for _, want := range []string{"v1.99.0", comp.Tested, "newer than"} {
		if !strings.Contains(res.Message, want) {
			t.Errorf("ceiling note %q does not mention %q", res.Message, want)
		}
	}
}

// TestAtTheCeilingIsSilent: the note fires above the ceiling, not at it. A
// warning on the exact version the harness runs would be noise on every
// developer's cluster.
func TestAtTheCeilingIsSilent(t *testing.T) {
	comp, _ := Lookup("kubernetes")
	p := clusterprofile.ClusterProfile{Kubernetes: &clusterprofile.Kubernetes{Version: "v" + comp.Tested + ".0"}}
	res := findOutcome(t, Check(p), "kubernetes", clusterprofile.OutcomeYes)
	if res.NewerThanTested {
		t.Errorf("version %q equals the ceiling %q but was flagged as newer", res.Found, comp.Tested)
	}
	if len(Check(p).Statements()) != 0 {
		t.Errorf("a cluster at the ceiling produced statements: %v", Check(p).Statements())
	}
}

// TestNoCeilingClaimsNothing: a component whose row declares no Tested version
// must never produce a ceiling note, however new the detected version is.
// Inventing one from a blank field would be a claim nobody made.
func TestNoCeilingClaimsNothing(t *testing.T) {
	comp, _ := Lookup("cert-manager")
	if comp.Tested != "" {
		t.Skip("cert-manager now declares a ceiling; pick another uncapped row")
	}
	p := clusterprofile.ClusterProfile{CertManager: &clusterprofile.CertManager{Version: "v99.0.0"}}
	res := findOutcome(t, Check(p), "cert-manager", clusterprofile.OutcomeYes)
	if res.NewerThanTested {
		t.Error("a component with no declared ceiling must not be reported as newer than tested")
	}
}

// TestUnknownCarriesTheCeilingAndFloorFacts: an Unknown is only actionable if
// it says what is now unverified and what would settle it, so the version we
// could not read still names the floor and the affected surface (#144).
func TestUnknownNamesWhatIsUnverified(t *testing.T) {
	p := clusterprofile.ClusterProfile{CloudNativePG: &clusterprofile.CloudNativePG{}}
	res := findOutcome(t, Check(p), "cnpg", clusterprofile.OutcomeUnknown)
	for _, want := range []string{"cnpg version unknown", "Unverified:", "kind: postgres", "1.23.0"} {
		if !strings.Contains(res.Message, want) {
			t.Errorf("unknown message %q does not mention %q", res.Message, want)
		}
	}
}

// TestUnsupportedNamesTheDegradation is the heart of issue #57: a too-old
// component must produce a NAMED degradation, not a bare version comparison.
func TestUnsupportedNamesTheDegradation(t *testing.T) {
	p := clusterprofile.ClusterProfile{Valkey: &clusterprofile.ValkeyOperator{Version: "0.2.0"}}
	res := findOutcome(t, Check(p), "valkey-operator", clusterprofile.OutcomeNo)
	for _, want := range []string{"0.2.0", "0.5.0", "Degraded:", "kind: valkey", "upgrade valkey-operator"} {
		if !strings.Contains(res.Message, want) {
			t.Errorf("unsupported message %q does not mention %q", res.Message, want)
		}
	}
}

// TestStatementsForAMixedProfile is the composition the CLI prints: one
// operator below its floor, one current, one absent, one unreadable, and a
// cluster below the Kubernetes floor. Exactly the non-clean ones are stated,
// each labelled, and the supported and absent ones say nothing at all.
func TestStatementsForAMixedProfile(t *testing.T) {
	p := clusterprofile.ClusterProfile{
		// Below the floor: no Kubernetes release can serve what kelson renders.
		Kubernetes: &clusterprofile.Kubernetes{Version: "v1.24.9"},
		// Below the floor: helm components stop rendering.
		HelmController: &clusterprofile.HelmController{Version: "0.37.4"},
		// Current: says nothing.
		CloudNativePG: &clusterprofile.CloudNativePG{Version: "1.26.0"},
		// Present, version unreadable: Unknown, not a failure.
		Flux: &clusterprofile.Component{},
		// cert-manager is absent entirely: not a version-skew question.
	}
	rep := Check(p)

	if !rep.Degraded() {
		t.Fatal("a profile with two components below their floor is degraded")
	}
	statements := rep.Statements()
	if len(statements) != 3 {
		t.Fatalf("got %d statements, want 3 (two unsupported, one unknown):\n%s",
			len(statements), strings.Join(statements, "\n"))
	}

	joined := strings.Join(statements, "\n")
	for _, want := range []string{
		"[" + labelUnsupported + "] kubernetes v1.24.9",
		"[" + labelUnsupported + "] helm-controller 0.37.4",
		"[" + labelUnknown + "] flux version unknown",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("statements do not contain %q:\n%s", want, joined)
		}
	}
	for _, unwanted := range []string{"cnpg", "cert-manager"} {
		if strings.Contains(joined, unwanted) {
			t.Errorf("statements mention %q, which is supported or absent and has nothing to state:\n%s", unwanted, joined)
		}
	}
}

// TestStatementsFollowMatrixOrder keeps the report stable for a given cluster:
// a set of statements that reordered itself between runs would read as a
// changing cluster.
func TestStatementsFollowMatrixOrder(t *testing.T) {
	p := clusterprofile.ClusterProfile{
		Kubernetes:     &clusterprofile.Kubernetes{Version: "v1.24.9"},
		CertManager:    &clusterprofile.CertManager{Version: "v1.2.0"},
		HelmController: &clusterprofile.HelmController{Version: "0.37.4"},
	}
	statements := Check(p).Statements()
	if len(statements) != 3 {
		t.Fatalf("got %d statements, want 3", len(statements))
	}
	order := []string{"kubernetes", "cert-manager", "helm-controller"}
	for i, name := range order {
		if !strings.Contains(statements[i], name) {
			t.Errorf("statement %d = %q, want the %s row (matrix order)", i, statements[i], name)
		}
	}
}

// TestCleanProfileStatesNothing: a cluster inside the matrix on every count
// produces no statements at all. A report that listed every healthy component
// would bury the lines that matter.
func TestCleanProfileStatesNothing(t *testing.T) {
	p := clusterprofile.ClusterProfile{
		Kubernetes:     &clusterprofile.Kubernetes{Version: "v1.31.2+k3s1"},
		CloudNativePG:  &clusterprofile.CloudNativePG{Version: "1.26.0"},
		HelmController: &clusterprofile.HelmController{Version: "1.3.0"},
	}
	rep := Check(p)
	if got := rep.Statements(); len(got) != 0 {
		t.Errorf("a supported cluster produced statements: %v", got)
	}
	if rep.Degraded() {
		t.Error("a supported cluster must not be reported as degraded")
	}
}

// TestUnknownIsNotDegraded: "we could not tell" is not a finding of
// degradation. It is stated, so a caller can act on it, but a caller gating on
// Degraded must not be told a cluster failed a check that was never made.
func TestUnknownIsNotDegraded(t *testing.T) {
	p := clusterprofile.ClusterProfile{
		Prometheus: &clusterprofile.Prometheus{},
		Incomplete: []clusterprofile.Gap{{Field: "cnpg.version", Reason: "forbidden: needs get,list on deployments.apps"}},
		// The gap covers cnpg, which is present but unreadable.
		CloudNativePG: &clusterprofile.CloudNativePG{},
	}
	rep := Check(p)
	if rep.Degraded() {
		t.Error("a profile whose only findings are Unknown must not be reported as degraded")
	}
	if got := len(rep.Statements()); got != 2 {
		t.Fatalf("got %d statements, want 2 (both unknown)", got)
	}
	if !strings.Contains(rep.Statements()[0], "detection gap") &&
		!strings.Contains(rep.Statements()[1], "detection gap") {
		t.Errorf("neither statement names the detection gap that caused the Unknown: %v", rep.Statements())
	}
}
