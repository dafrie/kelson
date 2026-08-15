package controller

import (
	"strings"
	"testing"

	"github.com/dafrie/kelson/api/kelson/v1alpha1"
)

const testHash = "1a2b3c4d5e6f7788990011223344556677889900aabbccddeeff001122334455"

func TestArtifactRef(t *testing.T) {
	cases := []struct {
		name        string
		prefix      string
		project     string
		environment string
		generation  int64
		hash        string
		repository  string
		tag         string
		reason      string
	}{
		{
			name:   "the shape ADR-0028 decision 2 specifies",
			prefix: "ghcr.io/acme", project: "checkout", environment: "production",
			generation: 7, hash: testHash,
			repository: "ghcr.io/acme/kelson/checkout-production", tag: "7-1a2b3c4d",
		},
		{
			// A registry host with no namespace is a legitimate local setup:
			// a kind cluster's in-cluster registry on a port.
			name: "a bare registry host", prefix: "localhost:5000",
			project: "checkout", environment: "staging", generation: 1, hash: testHash,
			repository: "localhost:5000/kelson/checkout-staging", tag: "1-1a2b3c4d",
		},
		{
			name: "a deep namespace", prefix: "registry.internal/team/apps",
			project: "shop", environment: "prod", generation: 42, hash: testHash,
			repository: "registry.internal/team/apps/kelson/shop-prod", tag: "42-1a2b3c4d",
		},
		{
			name: "a trailing slash on the prefix", prefix: "ghcr.io/acme/",
			project: "checkout", environment: "production", generation: 2, hash: testHash,
			repository: "ghcr.io/acme/kelson/checkout-production", tag: "2-1a2b3c4d",
		},
		{
			// No registry at all is a configuration gap, not a typo, and the
			// two get different reasons because they have different fixes.
			name: "no registry", prefix: "", project: "checkout", environment: "production",
			generation: 1, hash: testHash, reason: v1alpha1.ReasonRegistryNotConfigured,
		},
		{
			name: "a registry carrying a digest", prefix: "ghcr.io/acme@sha256:abc",
			project: "checkout", environment: "production", generation: 1, hash: testHash,
			reason: v1alpha1.ReasonArtifactRefInvalid,
		},
		{
			name: "an uppercase project", prefix: "ghcr.io/acme", project: "Checkout",
			environment: "production", generation: 1, hash: testHash,
			reason: v1alpha1.ReasonArtifactRefInvalid,
		},
		{
			// generation 0 means the object was never persisted, so there is no
			// revision number to publish under.
			name: "no generation", prefix: "ghcr.io/acme", project: "checkout",
			environment: "production", generation: 0, hash: testHash,
			reason: v1alpha1.ReasonArtifactRefInvalid,
		},
		{
			name: "a truncated hash", prefix: "ghcr.io/acme", project: "checkout",
			environment: "production", generation: 1, hash: "abc",
			reason: v1alpha1.ReasonArtifactRefInvalid,
		},
		{
			name: "no environment", prefix: "ghcr.io/acme", project: "checkout",
			environment: "", generation: 1, hash: testHash,
			reason: v1alpha1.ReasonArtifactRefInvalid,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repository, tag, err := ArtifactRef(tc.prefix, tc.project, tc.environment, tc.generation, tc.hash)
			if tc.reason != "" {
				if err == nil {
					t.Fatalf("got %s:%s, want a refusal", repository, tag)
				}
				de, ok := asDeliveryError(err)
				if !ok {
					t.Fatalf("error %v is not part of the taxonomy", err)
				}
				if de.Reason != tc.reason {
					t.Errorf("reason = %s, want %s", de.Reason, tc.reason)
				}
				return
			}
			if err != nil {
				t.Fatalf("ArtifactRef: %v", err)
			}
			if repository != tc.repository {
				t.Errorf("repository = %q, want %q", repository, tc.repository)
			}
			if tag != tc.tag {
				t.Errorf("tag = %q, want %q", tag, tc.tag)
			}
		})
	}
}

// TestArtifactRefTagIsBothIdentities: the generation alone could not tell a
// rollback from a re-publish, and the hash alone would collide with itself the
// moment a spec was reverted. An immutable tag written twice with different
// bytes is the one thing this scheme must never allow.
func TestArtifactRefTagIsBothIdentities(t *testing.T) {
	const other = "99887766554433221100ffeeddccbbaa99887766554433221100ffeeddccbbaa"
	_, first, err := ArtifactRef("ghcr.io/acme", "checkout", "production", 7, testHash)
	if err != nil {
		t.Fatal(err)
	}
	_, sameSpecLaterGeneration, _ := ArtifactRef("ghcr.io/acme", "checkout", "production", 8, testHash)
	if first == sameSpecLaterGeneration {
		t.Error("two generations of the same spec share a tag; a rollback would be indistinguishable from a re-publish")
	}
	_, sameGenerationOtherSpec, _ := ArtifactRef("ghcr.io/acme", "checkout", "production", 7, other)
	if first == sameGenerationOtherSpec {
		t.Error("two specs at one generation share a tag; the tag would be written twice with different bytes")
	}
	if !strings.HasPrefix(first, "7-") {
		t.Errorf("tag %q does not lead with the generation", first)
	}
}

// TestDeliveryPolicyCoversEveryReason: the taxonomy is closed, and a reason
// with no row would silently take the safest behaviour instead of the one it
// was designed for. This is what makes "which reason requeues how" a table
// rather than a habit.
func TestDeliveryPolicyCoversEveryReason(t *testing.T) {
	reasons := []string{
		v1alpha1.ReasonFluxNotInstalled,
		v1alpha1.ReasonRegistryNotConfigured,
		v1alpha1.ReasonArtifactRefInvalid,
		v1alpha1.ReasonRegistryUnreachable,
		v1alpha1.ReasonPushDenied,
		v1alpha1.ReasonFluxApplyForbidden,
		v1alpha1.ReasonFieldManagerConflict,
		v1alpha1.ReasonNameConflict,
		v1alpha1.ReasonRollbackTargetUnknown,
		v1alpha1.ReasonRegistryReadDenied,
	}
	for _, reason := range reasons {
		if _, ok := deliveryPolicy[reason]; !ok {
			t.Errorf("%s has no row in deliveryPolicy: its requeue behaviour is undefined", reason)
		}
	}
	if len(deliveryPolicy) != len(reasons) {
		t.Errorf("deliveryPolicy has %d rows for %d reasons: the set is not closed", len(deliveryPolicy), len(reasons))
	}
}

// TestDeliveryPolicyBehaviours pins the three answers, because getting one
// wrong is the difference between a controller that converges and one that
// burns a cluster's API budget.
func TestDeliveryPolicyBehaviours(t *testing.T) {
	// Never a crash loop: a cluster with no Flux is the expected state of a
	// fresh install, and it is fixed by an operator, on human time.
	for _, reason := range []string{
		v1alpha1.ReasonFluxNotInstalled,
		v1alpha1.ReasonPushDenied,
		v1alpha1.ReasonFluxApplyForbidden,
		v1alpha1.ReasonFieldManagerConflict,
	} {
		de := newDeliveryError(reason, "x", nil)
		if de.Backoff || de.Retry != operatorRetry {
			t.Errorf("%s: backoff=%v retry=%s, want a fixed %s timer and no error return",
				reason, de.Backoff, de.Retry, operatorRetry)
		}
	}
	// Nothing will change on its own, and everything that could is a watch
	// event: a requeue would be a timer with no question behind it.
	for _, reason := range []string{
		v1alpha1.ReasonRegistryNotConfigured,
		v1alpha1.ReasonArtifactRefInvalid,
		v1alpha1.ReasonNameConflict,
		v1alpha1.ReasonRollbackTargetUnknown,
	} {
		de := newDeliveryError(reason, "x", nil)
		if de.Backoff || de.Retry != 0 {
			t.Errorf("%s: backoff=%v retry=%s, want status only", reason, de.Backoff, de.Retry)
		}
	}
	// The one transient failure of something outside the cluster.
	if de := newDeliveryError(v1alpha1.ReasonRegistryUnreachable, "x", nil); !de.Backoff {
		t.Errorf("%s must take controller-runtime's exponential backoff", v1alpha1.ReasonRegistryUnreachable)
	}
}
