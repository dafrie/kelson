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

// TestDecryptionFailureIsNamed: a Kustomization that cannot decrypt reports a
// build failure whose message describes a mechanism ("Error getting data
// key"), not a situation. The situation is one of three setup mistakes, all of
// them far from where the reader is standing, so kelson names them (issue #81,
// ADR-0022).
func TestDecryptionFailureIsNamed(t *testing.T) {
	messages := []struct {
		reason  string
		message string
	}{
		{reasonBuildFailed, "failed to decrypt secret clusters/prod/secrets/checkout-db.enc.yaml: Error getting data key: 0 successful groups required, got 0"},
		{reasonBuildFailed, "cannot get sops metadata for file secrets/payments.enc.yaml"},
		{reasonDecryptionFailed, "no age identity found in the provided Secret"},
		{reasonBuildFailed, "no matching creation rules found"},
	}
	for _, m := range messages {
		k := Kustomization{
			Name: "checkout", Namespace: "flux-system", Path: "./clusters/prod",
			Ready: ConditionFalse, Reason: m.reason, Message: m.message,
			LastAttemptedRevision: "main@sha1:" + rev,
		}
		st := phaseFor(k, rev)
		if st.Phase != delivery.PhaseRejected {
			t.Fatalf("phase = %q, want rejected", st.Phase)
		}
		// The controller's own words are relayed verbatim ahead of kelson's,
		// exactly as every other reason's are.
		if !contains(st.Cause, m.message) {
			t.Errorf("cause must relay the controller's message: %q", st.Cause)
		}
		for _, want := range []string{"could not decrypt", "spec.decryption", "flux-system", "kelson secret rotate"} {
			if !contains(st.Cause, want) {
				t.Errorf("cause must name %q:\n%s", want, st.Cause)
			}
		}
	}
}

// TestOrdinaryBuildFailureGetsNoDecryptionCause: the markers must not fire on
// a build error that has nothing to do with SOPS, or every red deploy would
// come with three irrelevant things to check.
func TestOrdinaryBuildFailureGetsNoDecryptionCause(t *testing.T) {
	for _, message := range []string{
		"kustomize build failed: accumulating resources: missing metadata.name",
		"failed to pull image ghcr.io/acme/api: manifest unknown",
		"Deployment/apps/web dry-run failed: unknown field spec.templates",
	} {
		k := Kustomization{
			Name: "web", Namespace: "apps", Path: "./apps/web",
			Ready: ConditionFalse, Reason: reasonBuildFailed, Message: message,
			LastAttemptedRevision: "main@sha1:" + rev,
		}
		if contains(phaseFor(k, rev).Cause, "could not decrypt") {
			t.Errorf("a non-SOPS build failure must not be explained as a decryption failure: %q", message)
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
