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
	st := PhaseFor(k, rev)
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
	st := PhaseFor(k, rev)
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
	st := PhaseFor(k, rev)
	if st.Phase != delivery.PhaseDegraded {
		t.Fatalf("phase = %q, want degraded", st.Phase)
	}
	if st.Detail["lastAppliedRevision"] == "" {
		t.Fatalf("degraded must carry detail, got %v", st.Detail)
	}
}

func TestPhaseHealthy(t *testing.T) {
	k := Kustomization{Name: "web", Namespace: "apps", Ready: ConditionTrue, LastAppliedRevision: rev}
	st := PhaseFor(k, rev)
	if st.Phase != delivery.PhaseHealthy {
		t.Fatalf("phase = %q, want healthy", st.Phase)
	}
}

func TestPhaseAppliedWhenAppliedNotReady(t *testing.T) {
	k := Kustomization{Name: "web", Namespace: "apps", Ready: ConditionUnknown, LastAppliedRevision: rev}
	st := PhaseFor(k, rev)
	if st.Phase != delivery.PhaseApplied {
		t.Fatalf("phase = %q, want applied", st.Phase)
	}
}

func TestPhaseReconciling(t *testing.T) {
	k := Kustomization{Name: "web", Namespace: "apps", Ready: ConditionUnknown, LastAttemptedRevision: rev, Reconciling: true}
	st := PhaseFor(k, rev)
	if st.Phase != delivery.PhaseReconciling {
		t.Fatalf("phase = %q, want reconciling", st.Phase)
	}
}

func TestPhaseCommittedWhenSuspended(t *testing.T) {
	k := Kustomization{Name: "web", Namespace: "apps", Suspended: true, Ready: ConditionFalse}
	st := PhaseFor(k, rev)
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

// TestRevisionMatches covers both source kinds. The git spellings
// (branch@sha1:abc, branch/abc, abbreviated) are what a Kustomization kelson
// merely observes writes; the OCI spelling (tag@sha256:digest) is what the
// spine's own OCIRepository writes, and it is compared whole — a tag is not a
// prefix of anything, and reading the digest as a commit is the bug that made a
// healthy deployment report Committed forever.
func TestRevisionMatches(t *testing.T) {
	for _, tc := range []struct {
		name      string
		flux, sha string
		want      bool
	}{
		{"git v2", "main@sha1:abc123def", "abc123def", true},
		{"git v2 abbreviated", "main@sha1:abc123def", "abc123", true},
		{"git v1", "main/abc123def", "abc123def", true},
		{"git, different commit", "main@sha1:abc123def", "def456", false},
		{"empty revision", "", "abc", false},

		{"oci tag", "7-1a2b3c4d@sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08", "7-1a2b3c4d", true},
		{"oci tag, wrong generation", "8-1a2b3c4d@sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08", "7-1a2b3c4d", false},
		{"oci tag, wrong hash", "7-9999aaaa@sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08", "7-1a2b3c4d", false},
		{"oci tag is not a prefix", "7-1a2b3c4de@sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08", "7-1a2b3c4d", false},
		{"oci digest is not the answer", "7-1a2b3c4d@sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08", "9f86d081", false},
		{"oci tag without a digest", "7-1a2b3c4d", "7-1a2b3c4d", true},
	} {
		if got := revisionMatches(tc.flux, tc.sha); got != tc.want {
			t.Fatalf("%s: revisionMatches(%q, %q) = %v, want %v", tc.name, tc.flux, tc.sha, got, tc.want)
		}
	}
}

// TestPhaseForOCIRevisionIsHealthy is the same fix seen from the caller: an
// OCIRepository-backed Kustomization that has applied kelson's tag and reports
// Ready must read as Healthy, not as "has not observed this revision yet".
func TestPhaseForOCIRevisionIsHealthy(t *testing.T) {
	const rev = "7-1a2b3c4d"
	k := Kustomization{
		Name: "checkout-production", Namespace: "kelson-system",
		SourceKind: "OCIRepository", SourceName: "checkout-production",
		Ready:                 ConditionTrue,
		LastAppliedRevision:   rev + "@sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
		LastAttemptedRevision: rev + "@sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
	}
	if st := PhaseFor(k, rev); st.Phase != delivery.PhaseHealthy {
		t.Fatalf("phase = %s (%s), want %s", st.Phase, st.Cause, delivery.PhaseHealthy)
	}
	// And the previous revision is still Committed: the spine must be able to
	// tell "Flux has not caught up" from "Flux is done".
	if st := PhaseFor(k, "8-99887766"); st.Phase != delivery.PhaseCommitted {
		t.Fatalf("phase for the next revision = %s, want %s", st.Phase, delivery.PhaseCommitted)
	}
}

// TestKustomizationFromReadsTheFieldPaths pins the extraction the controller's
// own observer depends on: same field paths, same conditions, one reader.
func TestKustomizationFromReadsTheFieldPaths(t *testing.T) {
	obj := map[string]any{
		"spec": map[string]any{
			"path":      "./",
			"suspend":   true,
			"sourceRef": map[string]any{"kind": "OCIRepository", "name": "checkout-production"},
		},
		"status": map[string]any{
			"lastAppliedRevision":   "7-1a2b3c4d@sha256:abc",
			"lastAttemptedRevision": "8-99887766@sha256:def",
			"conditions": []any{
				map[string]any{"type": "Ready", "status": "False", "reason": "BuildFailed", "message": "boom"},
				map[string]any{"type": "Reconciling", "status": "True"},
			},
		},
	}
	k := KustomizationFrom(obj, "checkout-production", "kelson-system")
	if k.Name != "checkout-production" || k.Namespace != "kelson-system" {
		t.Errorf("identity = %s/%s", k.Namespace, k.Name)
	}
	if k.Path != "./" || !k.Suspended {
		t.Errorf("spec = path %q, suspend %v", k.Path, k.Suspended)
	}
	if k.SourceKind != "OCIRepository" || k.SourceName != "checkout-production" {
		t.Errorf("sourceRef = %s/%s", k.SourceKind, k.SourceName)
	}
	if k.LastAppliedRevision != "7-1a2b3c4d@sha256:abc" || k.LastAttemptedRevision != "8-99887766@sha256:def" {
		t.Errorf("revisions = %q / %q", k.LastAppliedRevision, k.LastAttemptedRevision)
	}
	if k.Ready != ConditionFalse || k.Reason != "BuildFailed" || k.Message != "boom" {
		t.Errorf("ready = %s/%s/%s", k.Ready, k.Reason, k.Message)
	}
	if !k.Reconciling {
		t.Error("the Reconciling condition was dropped")
	}
	// Nobody looked: Unknown, never a confident False.
	if empty := KustomizationFrom(map[string]any{}, "x", "y"); empty.Ready != ConditionUnknown {
		t.Errorf("an object with no conditions reads as %s, want Unknown", empty.Ready)
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
		st := PhaseFor(k, rev)
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
		if contains(PhaseFor(k, rev).Cause, "could not decrypt") {
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
