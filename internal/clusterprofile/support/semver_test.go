package support

import (
	"testing"

	"github.com/dafrie/kelson/internal/clusterprofile"
)

// TestParseVersion exercises the ragged version space the check actually sees:
// distro suffixes, a bare major.minor, pre-releases and casing. Anything the
// parser cannot make sense of must report ok=false — parse failure is the
// Unknown outcome upstream, never a guessed number.
func TestParseVersion(t *testing.T) {
	cases := []struct {
		in  string
		ok  bool
		maj uint64
		min uint64
		pat uint64
		pre string
	}{
		{"v1.31.2", true, 1, 31, 2, ""},
		{"v1.31.2+k3s1", true, 1, 31, 2, ""},
		{"1.31", true, 1, 31, 0, ""},
		{"1.31.0", true, 1, 31, 0, ""},
		{"V1.2.3", true, 1, 2, 3, ""},
		{"v1.16.0-beta.0", true, 1, 16, 0, "beta.0"},
		{"1.2.3-rc1+build5", true, 1, 2, 3, "rc1"},
		{"0.0.0", true, 0, 0, 0, ""},

		{"", false, 0, 0, 0, ""},
		{"  ", false, 0, 0, 0, ""},
		{"v", false, 0, 0, 0, ""},
		{"v1.2.3.4", false, 0, 0, 0, ""},
		{"1.x.3", false, 0, 0, 0, ""},
		{"latest", false, 0, 0, 0, ""},
		{"v1-", false, 0, 0, 0, ""},
		{"1..2", false, 0, 0, 0, ""},
	}
	for _, c := range cases {
		got, ok := parseVersion(c.in)
		if ok != c.ok {
			t.Errorf("parseVersion(%q) ok = %v, want %v", c.in, ok, c.ok)
			continue
		}
		if !ok {
			continue
		}
		if got.major != c.maj || got.minor != c.min || got.patch != c.pat {
			t.Errorf("parseVersion(%q) core = %d.%d.%d, want %d.%d.%d",
				c.in, got.major, got.minor, got.patch, c.maj, c.min, c.pat)
		}
		if pre := join(got.prerelease); pre != c.pre {
			t.Errorf("parseVersion(%q) prerelease = %q, want %q", c.in, pre, c.pre)
		}
	}
}

// TestCompareVersion pins semver precedence, including the subtleties that
// break naive lexicographic comparison: pre-releases sort below their release,
// numeric identifiers below alphanumeric ones, longer pre-releases below
// shorter, and build metadata is ignored entirely.
func TestCompareVersion(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.31.2", "1.31.2", 0},
		{"1.31.2+k3s1", "1.31.2", 0},
		{"1.31", "1.31.0", 0},
		{"v1.2.3", "v1.2.3", 0},
		{"1.2.3", "1.2.4", -1},
		{"1.2.4", "1.2.3", 1},
		{"2.0.0", "1.99.99", 1},
		{"1.2.3", "1.2.3-beta", 1},  // release > pre-release
		{"1.2.3-beta", "1.2.3", -1}, // pre-release < release
		{"1.2.3-beta.1", "1.2.3-beta.2", -1},
		{"1.2.3-beta.2", "1.2.3-beta.10", -1}, // numeric, not lexicographic
		{"1.2.3-beta.1", "1.2.3-alpha.1", 1},  // b > a alphabetically
		{"1.2.3-beta", "1.2.3-beta.1", -1},    // shorter pre-release ranks higher
		{"1.2.3", "1.2.3-alpha.1", 1},
		{"1.2.3-alpha", "1.2.3-beta", -1},
	}
	for _, c := range cases {
		a, aok := parseVersion(c.a)
		b, bok := parseVersion(c.b)
		if !aok || !bok {
			t.Fatalf("test inputs should parse: %q %q", c.a, c.b)
		}
		if got := a.compare(b); got != c.want {
			t.Errorf("compare(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
		if c.want != 0 {
			if inv := b.compare(a); inv != -c.want {
				t.Errorf("compare(%q, %q) = %d, want %d (antisymmetry)", c.b, c.a, inv, -c.want)
			}
		}
	}
}

// TestAtLeast covers the exported floor comparison the CNPG capability
// judgement uses (issue #90): the three answers, including the Unknown that an
// unreadable version must produce rather than a guessed pass or fail.
func TestAtLeast(t *testing.T) {
	cases := []struct {
		found, minimum string
		want           clusterprofile.Outcome
	}{
		{"1.26.0", "1.25.0", clusterprofile.OutcomeYes},
		{"1.25.0", "1.25.0", clusterprofile.OutcomeYes},
		{"v1.30.1", "1.26.0", clusterprofile.OutcomeYes},
		{"1.24.2", "1.25.0", clusterprofile.OutcomeNo},
		{"1.19.1", "1.20.0", clusterprofile.OutcomeNo},
		{"", "1.25.0", clusterprofile.OutcomeUnknown},
		{"latest", "1.25.0", clusterprofile.OutcomeUnknown},
		{"1.26.0", "nonsense", clusterprofile.OutcomeUnknown},
	}
	for _, c := range cases {
		if got := AtLeast(c.found, c.minimum); got != c.want {
			t.Errorf("AtLeast(%q, %q) = %s, want %s", c.found, c.minimum, got, c.want)
		}
	}
}

func join(p []string) string {
	out := ""
	for i, s := range p {
		if i > 0 {
			out += "."
		}
		out += s
	}
	return out
}
