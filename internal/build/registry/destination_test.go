package registry

import "testing"

func TestRepositoryDerivesOneRepositoryPerProject(t *testing.T) {
	cases := []struct {
		name    string
		prefix  string
		project string
		want    string
	}{
		{
			name:    "registry host and namespace",
			prefix:  "ghcr.io/acme",
			project: "shop",
			want:    "ghcr.io/acme/shop",
		},
		{
			name:    "trailing slash on the prefix is tolerated",
			prefix:  "ghcr.io/acme/",
			project: "shop",
			want:    "ghcr.io/acme/shop",
		},
		{
			name:    "surrounding whitespace is trimmed",
			prefix:  "  ghcr.io/acme  ",
			project: "shop",
			want:    "ghcr.io/acme/shop",
		},
		{
			name:    "local registry with a port",
			prefix:  "localhost:5000",
			project: "shop",
			want:    "localhost:5000/shop",
		},
		{
			name:    "nested namespace",
			prefix:  "registry.internal:5000/team/platform",
			project: "shop",
			want:    "registry.internal:5000/team/platform/shop",
		},
		{
			name:    "bare host pushes to the host root",
			prefix:  "ghcr.io",
			project: "shop",
			want:    "ghcr.io/shop",
		},
		{
			// A prefix with no host is a Docker Hub namespace, and the result
			// is fully qualified so a pinned reference is unambiguous.
			name:    "docker hub namespace expands",
			prefix:  "acme",
			project: "shop",
			want:    "docker.io/acme/shop",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Repository(tc.prefix, tc.project)
			if err != nil {
				t.Fatalf("Repository(%q, %q): %v", tc.prefix, tc.project, err)
			}
			if got != tc.want {
				t.Fatalf("Repository(%q, %q) = %q, want %q", tc.prefix, tc.project, got, tc.want)
			}
			// The derived repository is what build.Request.Image takes: no tag
			// and no digest, so Pin and Tag apply cleanly on top of it.
			ref, err := Parse(got)
			if err != nil {
				t.Fatalf("Parse(%q): %v", got, err)
			}
			if ref.Tag != "" || ref.Digest != "" {
				t.Fatalf("derived repository %q must carry neither tag nor digest", got)
			}
		})
	}
}

// A malformed destination must fail here rather than push somewhere nobody
// asked for: a tag or a digest on the prefix would otherwise be absorbed into
// the namespace path.
func TestRepositoryRejectsMalformedDestinations(t *testing.T) {
	cases := []struct {
		name    string
		prefix  string
		project string
	}{
		{name: "empty prefix", prefix: "", project: "shop"},
		{name: "whitespace-only prefix", prefix: "   ", project: "shop"},
		{name: "slash-only prefix", prefix: "///", project: "shop"},
		{name: "empty project", prefix: "ghcr.io/acme", project: ""},
		{name: "tag on the prefix", prefix: "ghcr.io/acme:v1", project: "shop"},
		{name: "digest on the prefix", prefix: "ghcr.io/acme@sha256:abc", project: "shop"},
		{name: "uppercase namespace", prefix: "ghcr.io/ACME", project: "shop"},
		{name: "empty namespace segment", prefix: "ghcr.io//acme", project: "shop"},
		{name: "invalid port", prefix: "localhost:not-a-port", project: "shop"},
		{name: "uppercase project", prefix: "ghcr.io/acme", project: "Shop"},
		{name: "project with a slash", prefix: "ghcr.io/acme", project: "shop/web"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Repository(tc.prefix, tc.project)
			if err == nil {
				t.Fatalf("Repository(%q, %q) = %q, want an error", tc.prefix, tc.project, got)
			}
		})
	}
}

// The whole point of deriving a repository is that a build can be pinned into
// a manifest: repository + digest must round-trip through Pin and Mutable.
func TestRepositoryPinsWithADigest(t *testing.T) {
	const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	repo, err := Repository("ghcr.io/acme", "shop")
	if err != nil {
		t.Fatalf("Repository: %v", err)
	}
	if !Mutable(repo) {
		t.Fatalf("a bare repository %q must report as mutable", repo)
	}
	pinned, err := Pin(repo, digest)
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}
	if want := "ghcr.io/acme/shop@" + digest; pinned != want {
		t.Fatalf("Pin = %q, want %q", pinned, want)
	}
	if Mutable(pinned) {
		t.Fatalf("pinned reference %q must not report as mutable", pinned)
	}
}
