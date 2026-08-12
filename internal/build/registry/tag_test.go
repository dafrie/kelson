package registry

import (
	"regexp"
	"strings"
	"testing"
)

// validDockerTag mirrors the Docker tag grammar: one [\w] first character then
// up to 127 characters of [\w.-], and not all periods.
var validDockerTag = regexp.MustCompile(`^[\w][\w.-]{0,127}$`)

func TestTagValue(t *testing.T) {
	cases := []struct {
		name        string
		project     string
		application string
		revision    string
		want        string
	}{
		{
			name:        "plain identifiers and a short sha",
			project:     "checkout",
			application: "web",
			revision:    "abc1234",
			want:        "checkout-web-abc1234",
		},
		{
			name:        "40-character sha passes through",
			project:     "checkout",
			application: "web",
			revision:    "0123456789abcdef0123456789abcdef01234567",
			want:        "checkout-web-0123456789abcdef0123456789abcdef01234567",
		},
		{
			name:        "slash-heavy branch name collapses to dashes",
			project:     "checkout",
			application: "web",
			revision:    "feature/payments-v2",
			want:        "checkout-web-feature-payments-v2",
		},
		{
			name:        "revision with at-sign and colon",
			project:     "checkout",
			application: "web",
			revision:    "refs/heads/topic@2",
			want:        "checkout-web-refs-heads-topic-2",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Tag(tc.project, tc.application, tc.revision)
			if got != tc.want {
				t.Fatalf("Tag(%q,%q,%q) = %q, want %q", tc.project, tc.application, tc.revision, got, tc.want)
			}
			if !validDockerTag.MatchString(got) {
				t.Fatalf("Tag(%q,%q,%q) = %q is not a valid Docker tag", tc.project, tc.application, tc.revision, got)
			}
		})
	}
}

func TestTagIsDeterministic(t *testing.T) {
	for i := 0; i < 100; i++ {
		if a, b := Tag("checkout", "web", "0123456789abcdef0123456789abcdef01234567"), Tag("checkout", "web", "0123456789abcdef0123456789abcdef01234567"); a != b {
			t.Fatalf("Tag() is not deterministic: %q != %q", a, b)
		}
	}
}

// A long branch name or a 40-character SHA must still produce a valid Docker
// tag that obeys the length and character rules, and a pathological long input
// must be truncated to the limit without splitting into invalid trailing chars.
func TestTagHoldsForAwkwardInputs(t *testing.T) {
	inputs := []struct {
		name     string
		revision string
	}{
		{
			name:     "40-character sha",
			revision: "0123456789abcdef0123456789abcdef01234567",
		},
		{
			name:     "very long branch name with slash separators",
			revision: strings.Repeat("feature/very-long-branch-name/", 8) + "end",
		},
		{
			name:     "pathological run of invalid characters",
			revision: "@$%^&*():;!,       ",
		},
	}

	for _, in := range inputs {
		t.Run(in.name, func(t *testing.T) {
			got := Tag("a-very-long-project-name", "a-very-long-application-name", in.revision)
			t.Logf("Tag = %q (len %d)", got, len(got))
			if !validDockerTag.MatchString(got) {
				t.Fatalf("Tag() = %q is not a valid Docker tag", got)
			}
			if len(got) > maxTagLength {
				t.Fatalf("Tag() = %q exceeds the %d-byte tag limit", got, maxTagLength)
			}
		})
	}
}
