package helm

import (
	"strings"
	"testing"

	"github.com/dafrie/kelson/internal/clusterprofile"
)

// fluxWithHelm builds a profile for a cluster running Flux with
// helm-controller: the shape a chart component needs, and the one every "yes"
// case starts from.
func fluxWithHelm(fluxVersion, controllerVersion string, crds ...string) clusterprofile.ClusterProfile {
	return clusterprofile.ClusterProfile{
		Flux: &clusterprofile.Component{Version: fluxVersion, Namespace: "flux-system"},
		HelmController: &clusterprofile.HelmController{
			Version: controllerVersion, Namespace: "flux-system", CRDs: crds,
		},
	}
}

func outcomes(t *testing.T, p clusterprofile.ClusterProfile) map[Capability]Result {
	t.Helper()
	out := map[Capability]Result{}
	for _, r := range Report(p) {
		out[r.Capability] = r
	}
	if len(out) != len(Capabilities) {
		t.Fatalf("Report returned %d results for %d capabilities", len(out), len(Capabilities))
	}
	return out
}

// TestFullFluxHostsChartComponents is the baseline: source-controller and
// helm-controller both present and current answers yes to everything.
func TestFullFluxHostsChartComponents(t *testing.T) {
	p := fluxWithHelm("2.6.4", "1.3.0", "helmreleases")
	for capability, r := range outcomes(t, p) {
		if r.Outcome != clusterprofile.OutcomeYes {
			t.Errorf("%s = %s (%s), want yes", capability, r.Outcome, r.Message)
		}
	}
	v := Supported(p)
	if v.Outcome != clusterprofile.OutcomeYes {
		t.Fatalf("Supported = %s (%s), want yes", v.Outcome, v.Message)
	}
	if len(v.Blocking) != 0 {
		t.Errorf("nothing should block: %v", v.Blocking)
	}
}

// TestEmptyClusterIsNo: a cluster with no Flux at all cannot host a chart
// component, and both capabilities say so rather than one standing in for the
// other.
func TestEmptyClusterIsNo(t *testing.T) {
	v := Supported(clusterprofile.ClusterProfile{})
	if v.Outcome != clusterprofile.OutcomeNo {
		t.Fatalf("empty cluster = %s, want no", v.Outcome)
	}
	if len(v.Blocking) != len(Capabilities) {
		t.Errorf("every capability should block, got %v", v.Blocking)
	}
	if !strings.Contains(v.Message, "helm-controller is not installed") {
		t.Errorf("the message must name what is missing: %s", v.Message)
	}
}

// TestFluxWithoutHelmController is the case this package exists for: a
// FluxInstance that installs a components subset leaves the source APIs served
// and nothing to reconcile a HelmRelease (issue #60). The source capability
// must stay yes, so the report names the one thing to install rather than
// telling a working Flux install it is absent.
func TestFluxWithoutHelmController(t *testing.T) {
	p := clusterprofile.ClusterProfile{Flux: &clusterprofile.Component{Version: "2.6.4"}}
	got := outcomes(t, p)
	if got[CapabilitySource].Outcome != clusterprofile.OutcomeYes {
		t.Errorf("source = %s (%s), want yes — source-controller is right there",
			got[CapabilitySource].Outcome, got[CapabilitySource].Message)
	}
	if got[CapabilityRelease].Outcome != clusterprofile.OutcomeNo {
		t.Errorf("release = %s, want no", got[CapabilityRelease].Outcome)
	}
	v := Supported(p)
	if v.Outcome != clusterprofile.OutcomeNo {
		t.Fatalf("Supported = %s, want no", v.Outcome)
	}
	if len(v.Blocking) != 1 || v.Blocking[0] != CapabilityRelease {
		t.Errorf("blocking = %v, want only the release capability", v.Blocking)
	}
	if !strings.Contains(got[CapabilityRelease].Message, "components subset") {
		t.Errorf("the remediation should name the FluxInstance subset that fixes it: %s",
			got[CapabilityRelease].Message)
	}
}

// TestHelmControllerWithoutSourceController is the mirror image, and it is not
// symmetric prose: the release would be created and then stall with a chart
// reference nothing resolves, so the message names the fetch rather than the
// install.
func TestHelmControllerWithoutSourceController(t *testing.T) {
	p := clusterprofile.ClusterProfile{
		HelmController: &clusterprofile.HelmController{Version: "1.3.0", CRDs: []string{"helmreleases"}},
	}
	got := outcomes(t, p)
	if got[CapabilityRelease].Outcome != clusterprofile.OutcomeYes {
		t.Errorf("release = %s (%s), want yes", got[CapabilityRelease].Outcome, got[CapabilityRelease].Message)
	}
	if got[CapabilitySource].Outcome != clusterprofile.OutcomeNo {
		t.Fatalf("source = %s, want no", got[CapabilitySource].Outcome)
	}
	if !strings.Contains(got[CapabilitySource].Message, SourceGroup) {
		t.Errorf("the message must name the group nothing serves: %s", got[CapabilitySource].Message)
	}
}

// TestTooOldControllerIsNo: helm.toolkit.fluxcd.io/v2 is what kelson writes,
// and a controller that only serves the betas would reject it. The message has
// to name the floor and the detected version, not just fail.
func TestTooOldControllerIsNo(t *testing.T) {
	r := Supports(fluxWithHelm("2.2.0", "0.37.4", "helmreleases"), CapabilityRelease)
	if r.Outcome != clusterprofile.OutcomeNo {
		t.Fatalf("0.37.4 = %s, want no", r.Outcome)
	}
	for _, want := range []string{"1.0.0", "0.37.4", "upgrade"} {
		if !strings.Contains(r.Message, want) {
			t.Errorf("message must contain %q: %s", want, r.Message)
		}
	}
}

// TestUnreadableVersionFallsBackToTheServedCRD: an install whose controller
// image is digest-pinned has no readable version, and the served resource is
// still a fact. Judging from it beats reporting Unknown about a cluster that
// demonstrably serves the API.
func TestUnreadableVersionFallsBackToTheServedCRD(t *testing.T) {
	r := Supports(fluxWithHelm("2.6.4", "", "helmreleases"), CapabilityRelease)
	if r.Outcome != clusterprofile.OutcomeYes {
		t.Fatalf("served CRD with no version = %s (%s), want yes", r.Outcome, r.Message)
	}
	if !strings.Contains(r.Message, "could not be read") {
		t.Errorf("the message must say how it was judged: %s", r.Message)
	}

	// With neither a version nor a served set there is nothing to judge from,
	// and Unknown is the honest answer — not a yes, and not a no either.
	r = Supports(fluxWithHelm("2.6.4", ""), CapabilityRelease)
	if r.Outcome != clusterprofile.OutcomeUnknown {
		t.Fatalf("nothing readable = %s, want unknown", r.Outcome)
	}
}

// TestUnservedCRDIsNo: a controller new enough for the API kelson writes, on a
// cluster whose CRDs were never applied, accepts nothing. The served set is
// what a manifest actually meets.
func TestUnservedCRDIsNo(t *testing.T) {
	r := Supports(fluxWithHelm("2.6.4", "1.3.0", "helmcharts"), CapabilityRelease)
	if r.Outcome != clusterprofile.OutcomeNo {
		t.Fatalf("unserved helmreleases = %s, want no", r.Outcome)
	}
	if !strings.Contains(r.Message, "helmreleases."+Group) {
		t.Errorf("the message must name the resource: %s", r.Message)
	}
}

// TestDetectionGapIsUnknown: a probe that could not look must never produce a
// refusal. Unknown is not No (issue #144).
func TestDetectionGapIsUnknown(t *testing.T) {
	p := clusterprofile.ClusterProfile{
		Flux: &clusterprofile.Component{Version: "2.6.4"},
		Incomplete: []clusterprofile.Gap{
			{Field: "helmController", Reason: "forbidden: needs get on /apis"},
		},
	}
	v := Supported(p)
	if v.Outcome != clusterprofile.OutcomeUnknown {
		t.Fatalf("a gapped probe = %s, want unknown", v.Outcome)
	}
	if !strings.Contains(v.Message, "forbidden") {
		t.Errorf("the reason behind an Unknown must survive into the message: %s", v.Message)
	}
}

// TestUnknownCapabilityIsUnknown: a caller asking about something the table
// does not track has not been told no.
func TestUnknownCapabilityIsUnknown(t *testing.T) {
	r := Supports(fluxWithHelm("2.6.4", "1.3.0", "helmreleases"), Capability("kustomize"))
	if r.Outcome != clusterprofile.OutcomeUnknown {
		t.Fatalf("untracked capability = %s, want unknown", r.Outcome)
	}
}
