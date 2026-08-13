package clusterprofile

import "testing"

// TestUnknownIsTheZeroValue is the whole reason the constants are ordered the
// way they are: a judgement nobody made must read as "we could not tell", not
// as a confident yes. Reordering them would silently turn every unset Outcome
// into a pass.
func TestUnknownIsTheZeroValue(t *testing.T) {
	var o Outcome
	if o != OutcomeUnknown {
		t.Fatalf("zero Outcome = %s, want unknown", o)
	}
	if OutcomeYes == OutcomeNo || OutcomeYes == OutcomeUnknown || OutcomeNo == OutcomeUnknown {
		t.Fatal("the three outcomes must be three distinct values")
	}
}

func TestOutcomeString(t *testing.T) {
	for _, c := range []struct {
		o    Outcome
		want string
	}{
		{OutcomeYes, "yes"},
		{OutcomeNo, "no"},
		{OutcomeUnknown, "unknown"},
		{Outcome(42), "unknown"},
	} {
		if got := c.o.String(); got != c.want {
			t.Errorf("Outcome(%d).String() = %q, want %q", c.o, got, c.want)
		}
	}
}

// TestGapForMatchesFieldAndBelow: detection reports a gap at whatever depth it
// failed, so a judgement about "certManager" must find a gap recorded on
// "certManager.clusterIssuers" — while a field that merely shares a prefix is
// a different field and must not match.
func TestGapForMatchesFieldAndBelow(t *testing.T) {
	p := ClusterProfile{Incomplete: []Gap{
		{Field: "certManager.clusterIssuers", Reason: "rbac: get clusterissuers.cert-manager.io"},
		{Field: "storageClasses", Reason: "list storageclasses.storage.k8s.io denied"},
	}}

	for _, c := range []struct {
		field  string
		want   bool
		reason string
	}{
		{"certManager", true, "rbac: get clusterissuers.cert-manager.io"},
		{"certManager.clusterIssuers", true, "rbac: get clusterissuers.cert-manager.io"},
		{"storageClasses", true, "list storageclasses.storage.k8s.io denied"},
		{"storageClassesExtra", false, ""},
		{"gatewayAPI", false, ""},
	} {
		gap, ok := p.GapFor(c.field)
		if ok != c.want {
			t.Errorf("GapFor(%q) hidden = %v, want %v", c.field, ok, c.want)
			continue
		}
		if gap.Reason != c.reason {
			t.Errorf("GapFor(%q) reason = %q, want %q", c.field, gap.Reason, c.reason)
		}
	}
}

// TestGapForOnACompleteProfile: no gaps means no excuse — every field is a
// finding, so nothing may be reported as unknown.
func TestGapForOnACompleteProfile(t *testing.T) {
	if gap, ok := (ClusterProfile{}).GapFor("storageClasses"); ok {
		t.Fatalf("empty profile reported a gap: %+v", gap)
	}
}
