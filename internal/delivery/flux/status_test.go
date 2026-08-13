package flux

import (
	"testing"

	"github.com/dafrie/kelson/internal/delivery"
)

// The three answers (docs/statemachine.md) must never be confused:
//  1. committed, reconciler has not picked it up yet → PhaseCommitted
//  2. reconciler rejected it → PhaseRejected, with the cause named
//  3. applied but unhealthy → PhaseDegraded, surfacing health detail

const rev = "abc123def"

func TestPhaseCommittedWhenNotObserved(t *testing.T) {
	k := Kustomization{Name: "web", Namespace: "apps", Ready: ConditionUnknown}
	st := phaseFor(k, rev)
	if st.Phase != delivery.PhaseCommitted {
		t.Fatalf("phase = %q, want committed", st.Phase)
	}
	if st.Cause == "" {
		t.Fatalf("committed must carry a \"not observed\" cause")
	}
}

func TestPhaseRejectedNamesTheCause(t *testing.T) {
	k := Kustomization{
		Name: "web", Namespace: "apps", Path: "./apps/web",
		Ready: ConditionFalse, Reason: reasonBuildFailed, Message: "image not found",
		LastAttemptedRevision: "main@sha1:" + rev,
	}
	st := phaseFor(k, rev)
	if st.Phase != delivery.PhaseRejected {
		t.Fatalf("phase = %q, want rejected", st.Phase)
	}
	if !containsAny(st.Cause, "flux:", "BuildFailed", "image not found") {
		t.Fatalf("cause = %q", st.Cause)
	}
}

func TestPhaseDegradedSurfacesHealth(t *testing.T) {
	k := Kustomization{
		Name: "web", Namespace: "apps",
		Ready: ConditionFalse, Reason: reasonHealthCheckFail, Message: "ready: 0/1",
		LastAppliedRevision: "abc123def",
	}
	st := phaseFor(k, rev)
	if st.Phase != delivery.PhaseDegraded {
		t.Fatalf("phase = %q, want degraded", st.Phase)
	}
	if st.Detail["lastAppliedRevision"] == "" {
		t.Fatalf("degraded must carry detail, got %v", st.Detail)
	}
}

func TestPhaseHealthy(t *testing.T) {
	k := Kustomization{Name: "web", Namespace: "apps", Ready: ConditionTrue, LastAppliedRevision: rev}
	st := phaseFor(k, rev)
	if st.Phase != delivery.PhaseHealthy {
		t.Fatalf("phase = %q, want healthy", st.Phase)
	}
}

func TestPhaseAppliedWhenAppliedNotReady(t *testing.T) {
	k := Kustomization{Name: "web", Namespace: "apps", Ready: ConditionUnknown, LastAppliedRevision: rev}
	st := phaseFor(k, rev)
	if st.Phase != delivery.PhaseApplied {
		t.Fatalf("phase = %q, want applied", st.Phase)
	}
}

func TestPhaseReconciling(t *testing.T) {
	k := Kustomization{Name: "web", Namespace: "apps", Ready: ConditionUnknown, LastAttemptedRevision: rev, Reconciling: true}
	st := phaseFor(k, rev)
	if st.Phase != delivery.PhaseReconciling {
		t.Fatalf("phase = %q, want reconciling", st.Phase)
	}
}

func TestPhaseCommittedWhenSuspended(t *testing.T) {
	k := Kustomization{Name: "web", Namespace: "apps", Suspended: true, Ready: ConditionFalse}
	st := phaseFor(k, rev)
	if st.Phase != delivery.PhaseCommitted {
		t.Fatalf("phase = %q, want committed (suspended)", st.Phase)
	}
}

// TestKustomizationCovers verifies path-scoped watching, including that a
// Kustomization whose source does not match a different repository is not
// mistaken for covered.
func TestKustomizationCovers(t *testing.T) {
	real := normalizeRepo("git@github.com:acme/deploy.git")
	cases := []struct {
		name string
		k    Kustomization
		repo string
		path string
		want bool
	}{
		{"exact path", Kustomization{Path: "./apps/web"}, "https://github.com/acme/deploy.git", "apps/web", true},
		{"parent path covers child", Kustomization{Path: "./apps"}, "x", "apps/web/checkout", true},
		{"disjoint path", Kustomization{Path: "./other"}, "x", "apps/web", false},
		{"matching source url", Kustomization{Path: "./apps", SourceURL: "git@github.com:acme/deploy.git"}, "https://github.com/acme/deploy.git", "apps/web", true},
		{"different repo", Kustomization{Path: "./apps", SourceURL: "https://github.com/other/deploy.git"}, "https://github.com/acme/deploy.git", "apps/web", false},
	}
	for _, tc := range cases {
		if got := tc.k.covers(tc.repo, tc.path); got != tc.want {
			t.Fatalf("%s: covers(%q, %q) = %v, want %v (real=%s)", tc.name, tc.repo, tc.path, got, tc.want, real)
		}
	}
}

// TestRevisionMatches covers the Flux revision spellings (branch@sha1:abc,
// branch/abc, abbreviated).
func TestRevisionMatches(t *testing.T) {
	for _, tc := range []struct {
		flux, sha string
		want      bool
	}{
		{"main@sha1:abc123def", "abc123def", true},
		{"main@sha1:abc123def", "abc123", true},
		{"main/abc123def", "abc123def", true},
		{"main@sha1:abc123def", "def456", false},
		{"", "abc", false},
	} {
		if got := revisionMatches(tc.flux, tc.sha); got != tc.want {
			t.Fatalf("revisionMatches(%q, %q) = %v, want %v", tc.flux, tc.sha, got, tc.want)
		}
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(sub) == 0 || contains(s, sub) {
			return true
		}
	}
	return false
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
