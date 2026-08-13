package support

import (
	"strings"
	"testing"

	"github.com/dafrie/kelson/internal/clusterprofile"
)

// TestCertManagerTooOld is the issue's marquee case: a cert-manager that is
// present but below the floor must produce a clear message at preview time
// naming the component, the version found and the version required.
func TestCertManagerTooOld(t *testing.T) {
	p := clusterprofile.ClusterProfile{
		CertManager: &clusterprofile.CertManager{Version: "v1.12.1"},
	}
	rep := Check(p)
	res := findOutcome(t, rep, "cert-manager", clusterprofile.OutcomeNo)

	if !strings.Contains(res.Message, "cert-manager") {
		t.Errorf("message %q does not name the component", res.Message)
	}
	if !strings.Contains(res.Message, "v1.12.1") {
		t.Errorf("message %q does not name the version found", res.Message)
	}
	if !strings.Contains(res.Message, "1.14.0") {
		t.Errorf("message %q does not name the version required", res.Message)
	}
}

// TestOutcomesAreThree guards the central contract: a version at or above the
// floor answers yes, one below answers no, and a version we cannot judge is a
// third thing, unknown — never silently fine and never a failure.
func TestOutcomesAreThree(t *testing.T) {
	cases := []struct {
		name string
		ver  string
		want clusterprofile.Outcome
	}{
		{"supported", "v1.14.0", clusterprofile.OutcomeYes},
		{"supported-newer", "v1.31.2+k3s1", clusterprofile.OutcomeYes},
		{"unsupported", "v1.12.1", clusterprofile.OutcomeNo},
		{"empty-unknown", "", clusterprofile.OutcomeUnknown},
		{"unparseable-unknown", "vendor-custom", clusterprofile.OutcomeUnknown},
	}
	for _, c := range cases {
		p := clusterprofile.ClusterProfile{CertManager: &clusterprofile.CertManager{Version: c.ver}}
		res := findOutcome(t, Check(p), "cert-manager", c.want)
		if c.want == clusterprofile.OutcomeNo {
			if res.Degrade != DegradeRefuse {
				t.Errorf("cert-manager too old: degrade = %q, want refuse", res.Degrade)
			}
		}
	}
}

// TestKubernetesBuildSuffix: a distro build like v1.31.2+k3s1 still counts as
// supported because build metadata is excluded from precedence.
func TestKubernetesBuildSuffix(t *testing.T) {
	p := clusterprofile.ClusterProfile{
		Kubernetes: &clusterprofile.Kubernetes{Version: "v1.31.2+k3s1"},
	}
	if len(Check(p).Unsupported()) != 0 {
		t.Fatalf("v1.31.2+k3s1 must be supported")
	}
}

// TestGapIsUnknown: a detection Gap hiding cert-manager (e.g. no RBAC to list
// issuers) must be Unknown, not reported as supported or as a failure.
func TestGapIsUnknown(t *testing.T) {
	p := clusterprofile.ClusterProfile{
		CertManager: &clusterprofile.CertManager{},
		Incomplete: []clusterprofile.Gap{
			{Field: "certManager.clusterIssuers", Reason: "rbac: get clusterissuers.cert-manager.io"},
		},
	}
	res := findOutcome(t, Check(p), "cert-manager", clusterprofile.OutcomeUnknown)
	if !strings.Contains(res.Message, "cert-manager version unknown") {
		t.Errorf("gap message = %q, want an unknown-version framing", res.Message)
	}
}

// TestAbsentComponentIsSkipped: a component the profile does not report is not
// a version-skew problem, so it must not appear in the report at all.
func TestAbsentComponentIsSkipped(t *testing.T) {
	p := clusterprofile.ClusterProfile{}
	rep := Check(p)
	if len(rep.Results) != 0 {
		t.Fatalf("empty profile produced %d results, want 0", len(rep.Results))
	}
}

// TestPolicyEngineTrackedByName: policy engines are enumerated per name, and an
// engine the matrix does not track is ignored rather than being given a floor
// it does not have.
func TestPolicyEngineTrackedByName(t *testing.T) {
	p := clusterprofile.ClusterProfile{
		PolicyEngines: []clusterprofile.PolicyEngine{
			{Name: "kyverno", Version: "1.9.0"},
			{Name: "best-policy-outside-the-matrix", Version: "0.1.0"},
		},
	}
	rep := Check(p)
	if got := len(rep.Results); got != 1 {
		t.Fatalf("got %d results, want 1 (untracked engine skipped)", got)
	}
	if res := rep.Results[0]; res.Component != "kyverno" || res.Outcome != clusterprofile.OutcomeNo {
		t.Fatalf("kyverno result = %+v, want unsupported", res)
	}
}

// TestReportFiltering keeps the three-way grouping the callers lean on in one
// place with a mixed profile.
func TestReportFiltering(t *testing.T) {
	p := clusterprofile.ClusterProfile{
		Kubernetes:  &clusterprofile.Kubernetes{Version: "v1.31.2"},
		CertManager: &clusterprofile.CertManager{Version: "v1.12.1"},
		Prometheus:  &clusterprofile.Prometheus{Version: ""},
	}
	rep := Check(p)
	if len(rep.Supported()) != 1 || rep.Supported()[0].Component != "kubernetes" {
		t.Fatalf("supported = %+v, want the cluster only", rep.Supported())
	}
	if len(rep.Unsupported()) != 1 || rep.Unsupported()[0].Component != "cert-manager" {
		t.Fatalf("unsupported = %+v, want cert-manager", rep.Unsupported())
	}
	if len(rep.Unknown()) != 1 || rep.Unknown()[0].Component != "prometheus" {
		t.Fatalf("unknown = %+v, want prometheus", rep.Unknown())
	}
}

func findOutcome(t *testing.T, rep Report, name string, want clusterprofile.Outcome) Result {
	t.Helper()
	for _, r := range rep.Results {
		if r.Component == name {
			if r.Outcome != want {
				t.Fatalf("component %s outcome = %s (%s), want %s", name, r.Outcome, r.Message, want)
			}
			return r
		}
	}
	t.Fatalf("no result for component %s", name)
	return Result{}
}
