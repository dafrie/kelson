package renderer

import (
	"strings"
	"testing"
)

// TestDigestPinnedImageRendersUnchanged guards the property #51 exists to
// deliver: a digest-pinned image reaches the rendered manifest byte-for-byte.
//
// It is a regression guard rather than a feature test. The renderer already
// renders whatever image string it is handed, so pinning is the deploy path's
// job — but that only stays true while nothing in the renderer normalises,
// re-tags or splits an image reference. If something ever does, reproducible
// deploys break quietly, and this test is what makes that loud.
func TestDigestPinnedImageRendersUnchanged(t *testing.T) {
	const pinned = "ghcr.io/acme/checkout@sha256:" +
		"aaaabbbbccccddddeeeeffff00001111222233334444555566667777888899990"

	resolved := resolvedFixture()
	// Every application, not just the first: the fixture's second app would
	// otherwise supply a tagged reference to the same repository and make the
	// "no mutable tag" assertion below meaningless.
	for i := range resolved.Components {
		resolved.Components[i].Image = pinned
	}

	// A gateway profile, because the fixture's web application declares
	// domains and a profile without Gateway API is now a capability gap (#140).
	manifests, err := Render(resolved, gatewayProfile(), nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	out, err := Encode(manifests)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !strings.Contains(string(out), pinned) {
		t.Errorf("digest-pinned image did not survive rendering; want %q in output:\n%s", pinned, out)
	}
	// A tag must not be reintroduced alongside the digest: "repo@sha256:..."
	// and "repo:tag" are different references, and emitting both would make
	// the manifest ambiguous about which one is deployed.
	if strings.Contains(string(out), "ghcr.io/acme/checkout:") {
		t.Errorf("a mutable tag appeared alongside the digest:\n%s", out)
	}
}
