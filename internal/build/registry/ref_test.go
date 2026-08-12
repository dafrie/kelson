package registry

import (
	"testing"
)

// The two acceptance criteria of issue #51, stated as properties and exercised
// table-driven over the awkward reference cases:
//
//  1. "Rendered manifests contain image digests" — a tag-only reference is
//     reported mutable, and pinning it produces a digest-pinned reference that
//     round-trips.
//  2. "Redeploying an old revision produces the exact original image" —
//     pinning is deterministic, and re-pinning an already pinned reference
//     leaves it unchanged.
func TestRenderedManifestsContainImageDigests(t *testing.T) {
	const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	cases := []struct {
		name string
		ref  string
		want string // canonical pinned reference
	}{
		{
			name: "implicit docker.io and library namespace",
			ref:  "alpine",
			want: "docker.io/library/alpine@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		},
		{
			name: "registry host with port and no namespace",
			ref:  "localhost:5000/app",
			want: "localhost:5000/app@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		},
		{
			name: "explicit registry with namespace and tag",
			ref:  "ghcr.io/acme/web:v1",
			want: "ghcr.io/acme/web@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		},
		{
			name: "registry host with port, namespace and tag",
			ref:  "localhost:5000/acme/web:v1",
			want: "localhost:5000/acme/web@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		},
		{
			name: "docker.io namespace without registry and with tag",
			ref:  "acme/web:2",
			want: "docker.io/acme/web@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		},
		{
			name: "reference carrying both tag and digest",
			ref:  "ghcr.io/acme/web:v1@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			want: "ghcr.io/acme/web@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Pin(tc.ref, digest)
			if err != nil {
				t.Fatalf("Pin(%q): unexpected error: %v", tc.ref, err)
			}
			if got != tc.want {
				t.Fatalf("Pin(%q) = %q, want %q", tc.ref, got, tc.want)
			}

			if Mutable(got) {
				t.Fatalf("Mutable(%q) = true, want false for a pinned reference", got)
			}

			// Round-trip: the pinned string must parse back to a pinned ref.
			parsed, err := Parse(got)
			if err != nil {
				t.Fatalf("Parse(%q): unexpected error: %v", got, err)
			}
			if parsed.Digest == "" {
				t.Fatalf("Parse(%q): digest was dropped", got)
			}
			if parsed.String() != got {
				t.Fatalf("Parse(%q).String() = %q, not a round-trip", got, parsed.String())
			}
		})
	}
}

func TestRedeployingAnOldRevisionProducesTheExactOriginalImage(t *testing.T) {
	const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	// Pinning is deterministic: the same input yields the same output.
	first, err := Pin("ghcr.io/acme/web:v1", digest)
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}
	second, err := Pin("ghcr.io/acme/web:v1", digest)
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}
	if first != second {
		t.Fatalf("pinning is not deterministic: %q != %q", first, second)
	}

	// A tag-only reference and its already-pinned form pin to the same thing,
	// and re-pinning a pinned reference is a no-op.
	fromTag, err := Pin("ghcr.io/acme/web:v1", digest)
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}
	fromPinned, err := Pin(first, digest)
	if err != nil {
		t.Fatalf("Pin on pinned ref: %v", err)
	}
	if fromTag != fromPinned {
		t.Fatalf("re-pinning changed the reference: %q != %q", fromTag, fromPinned)
	}
	if fromPinned != first {
		t.Fatalf("re-pinned reference %q differs from first pin %q", fromPinned, first)
	}
}

func TestMutable(t *testing.T) {
	cases := []struct {
		name string
		ref  string
		want bool
	}{
		{"tag-only", "ghcr.io/acme/web:v1", true},
		{"tagless", "ghcr.io/acme/web", true},
		{"implicit", "alpine", true},
		{"pinned", "ghcr.io/acme/web@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", false},
		{"tag and digest still pinned", "ghcr.io/acme/web:v1@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", false},
		{"unparseable fails closed", "", true},
		{"digest-only garbage fails closed", "ghcr.io/acme/web@not-a-digest", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Mutable(tc.ref); got != tc.want {
				t.Fatalf("Mutable(%q) = %v, want %v", tc.ref, got, tc.want)
			}
		})
	}
}

func TestPinRejectsBadDigest(t *testing.T) {
	for _, d := range []string{"", "sha256", "sha256:", "sha256:xyz", "sha256:0f0zz"} {
		if _, err := Pin("ghcr.io/acme/web:v1", d); err == nil {
			t.Fatalf("Pin with digest %q: expected error, got nil", d)
		}
	}
}

func TestParseRejects(t *testing.T) {
	for _, s := range []string{"", "   ", "ghcr.io/"} {
		if _, err := Parse(s); err == nil {
			t.Fatalf("Parse(%q): expected error, got nil", s)
		}
	}
}
