package support

// Semver comparison for version-skew checks (issue #57).
//
// Kubernetes and the operators kelson adopts do not report clean semver.
// Real versions seen in the wild include v1.31.2, v1.31.2+k3s1, the bare 1.31
// a hand-written profile might carry, and v1.16.0-beta.0 for the Gateway API.
// This file parses that ragged space defensively and compares by semver
// precedence, so an unsupported version is a concrete finding and an
// unparseable one degrades to Unknown rather than failing loudly.
//
// It is deliberately hand-rolled rather than a third-party library: depguard
// restricts the module to an allow-list (AGENTS.md), and the subset of semver
// that matters here — numeric core plus pre-release ordering, with build
// metadata ignored — is small enough to get exactly right.

import (
	"strconv"
	"strings"

	"github.com/dafrie/kelson/internal/clusterprofile"
)

// AtLeast answers "is found at or above minimum?" in the codebase's three-way
// vocabulary: OutcomeYes at or above, OutcomeNo below, and OutcomeUnknown when
// either string cannot be parsed as a version — including the empty version a
// component reports when it is installed but unreadable.
//
// It is exported because version floors exist outside the declared support
// matrix: CNPG's declarative capabilities each arrived in a different release
// (issue #90), and those floors are judged in
// internal/clusterprofile/postgres. Duplicating this parser there would be two
// answers to "is 1.31.2+k3s1 above 1.25?", which is exactly one too many.
func AtLeast(found, minimum string) clusterprofile.Outcome {
	req, ok := parseVersion(minimum)
	if !ok {
		return clusterprofile.OutcomeUnknown
	}
	has, ok := parseVersion(found)
	if !ok {
		return clusterprofile.OutcomeUnknown
	}
	if has.compare(req) >= 0 {
		return clusterprofile.OutcomeYes
	}
	return clusterprofile.OutcomeNo
}

// version is a parsed, precedence-comparable version. Missing minor and patch
// are treated as zero per semver, so "1.31" equals "1.31.0". Build metadata
// (the +k3s1 suffix) never participates in precedence.
type version struct {
	major      uint64
	minor      uint64
	patch      uint64
	prerelease []string
	hasMinor   bool
	hasPatch   bool
}

// parseVersion parses a version defensively. ok is false when the input cannot
// be interpreted as a version at all — the caller must then return Unknown,
// not guess.
//
// Accepted shapes (leading "v"/"V" optional, "+build" ignored):
//
//	v1.31.2          v1.31.2+k3s1   1.31   v1.16.0-beta.0
func parseVersion(s string) (v version, ok bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	s = strings.TrimPrefix(s, "V")
	if s == "" {
		return version{}, false
	}

	// Drop build metadata; it is irrelevant to precedence.
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}

	// Split off the pre-release suffix. A trailing dash ("v1-") is not a
	// version: the separator must be followed by a non-empty identifier.
	var pre string
	if i := strings.IndexByte(s, '-'); i >= 0 {
		pre, s = s[i+1:], s[:i]
		if pre == "" {
			return version{}, false
		}
	}

	core := strings.Split(s, ".")
	if len(core) == 0 || len(core) > 3 || core[0] == "" {
		return version{}, false
	}
	nums := make([]uint64, 0, 3)
	for _, seg := range core {
		if !isDigits(seg) {
			return version{}, false
		}
		n, err := strconv.ParseUint(seg, 10, 64)
		if err != nil {
			return version{}, false
		}
		nums = append(nums, n)
	}

	v.major = nums[0]
	if len(nums) > 1 {
		v.minor, v.hasMinor = nums[1], true
	}
	if len(nums) > 2 {
		v.patch, v.hasPatch = nums[2], true
	}

	if pre != "" {
		v.prerelease = strings.Split(pre, ".")
	}
	return v, true
}

// compare reports the semver precedence of v against o: -1 if v < o, 0 if
// equal, +1 if v > o. Build metadata is ignored. A release outranks any
// pre-release sharing its core; otherwise identifiers compare dot by dot, with
// numeric identifiers ranking below alphanumeric ones.
func (v version) compare(o version) int {
	if c := cmpUint64(v.major, o.major); c != 0 {
		return c
	}
	if c := cmpUint64(v.minor, o.minor); c != 0 {
		return c
	}
	if c := cmpUint64(v.patch, o.patch); c != 0 {
		return c
	}
	return comparePrerelease(v.prerelease, o.prerelease)
}

func comparePrerelease(a, b []string) int {
	switch {
	case len(a) == 0 && len(b) == 0:
		return 0
	case len(a) == 0:
		return 1 // release outranks any pre-release
	case len(b) == 0:
		return -1
	}
	for i := 0; i < len(a) && i < len(b); i++ {
		if c := compareIdent(a[i], b[i]); c != 0 {
			return c
		}
	}
	// Equal prefixes: the larger set of pre-release fields ranks higher
	// (semver 10.3).
	if len(a) != len(b) {
		if len(a) > len(b) {
			return 1
		}
		return -1
	}
	return 0
}

// compareIdent compares two pre-release identifiers: numeric by value (a
// numeric ranks below anything alphanumeric), alphanumeric by ASCII order,
// which happens to be Go's string order.
func compareIdent(a, b string) int {
	an, aok := numericIdent(a)
	bn, bok := numericIdent(b)
	switch {
	case aok && bok:
		return cmpUint64(an, bn)
	case aok && !bok:
		return -1
	case !aok && bok:
		return 1
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// numericIdent reports whether id is a non-negative decimal integer and, if
// so, its value.
func numericIdent(id string) (uint64, bool) {
	if id == "" || !isDigits(id) {
		return 0, false
	}
	n, err := strconv.ParseUint(id, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func cmpUint64(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}
