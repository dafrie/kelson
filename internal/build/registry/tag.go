package registry

import (
	"strings"
)

// maxTagLength is the Docker tag length limit (the tag regex is
// [\w][\w.-]{0,127}).
const maxTagLength = 128

// Tag names the image a build produced, derived deterministically from the
// source revision (issue #51). The convention is:
//
//	<project>-<application>-<revision>
//
// each sanitized to Docker tag characters ([a-zA-Z0-9_.-]) and truncated so
// the whole tag fits the 128-byte limit. The tag is for humans only —
// reproducibility comes from the digest, which is what rendered manifests pin
// to. The revision is the most volatile component, so the truncation budget is
// taken from it (project and application prefix stay intact).
func Tag(project, application, revision string) string {
	tag := sanitize(project) + "-" + sanitize(application) + "-" + sanitize(revision)
	if len(tag) > maxTagLength {
		tag = strings.TrimRight(tag[:maxTagLength], "-.")
	}
	return tag
}

// sanitize maps s to a single, non-empty tag component. Runs of characters
// outside [a-zA-Z0-9_.-] collapse to a single '-', and the result never starts
// or ends with '.' or '-'.
func sanitize(s string) string {
	var b strings.Builder
	prevSep := true // suppress a leading separator
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_':
			b.WriteByte(c)
			prevSep = false
		case c == '.' || c == '-':
			if !prevSep && b.Len() > 0 {
				b.WriteByte(c)
				prevSep = true
			}
		default:
			// Any other character (e.g. '/', '@', ':') is an invalid tag char;
			// collapse a run of them into a single '-'.
			if !prevSep && b.Len() > 0 {
				b.WriteByte('-')
				prevSep = true
			}
		}
	}
	out := strings.TrimRight(b.String(), "-.")
	if out == "" {
		return "x"
	}
	return out
}
