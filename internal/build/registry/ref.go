package registry

import (
	"fmt"
	"strings"
)

// Default registry and its single-component shorthand namespace, per the
// Docker distribution convention: "alpine" and "docker.io/library/alpine" are
// the same image, and a bare first path component is never a registry.
const (
	defaultRegistry = "docker.io"
	defaultLibrary  = "library"
)

// Ref is a parsed image reference split into its components.
//
// The registry defaults to docker.io and a single repository component maps
// into the library namespace, so "alpine", "docker.io/alpine" and
// "docker.io/library/alpine" all parse to the same Ref. A first path
// component is treated as a registry host only when it looks like one:
// it contains a '.' or ':', or it is "localhost".
type Ref struct {
	Registry   string // host, e.g. "ghcr.io" or "localhost:5000" ("docker.io")
	Namespace  string // e.g. "library" or "acme"; empty when the repo has none
	Repository string // final component, e.g. "alpine" or "web"
	Tag        string // human tag; empty when absent
	Digest     string // digest, e.g. "sha256:..."; empty when unpinned
}

// Parse splits s into its components, resolving registry and namespace
// defaults. A reference may carry a tag, a digest, or both
// ("ghcr.io/acme/web:v1@sha256:...").
func Parse(s string) (Ref, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Ref{}, fmt.Errorf("registry: empty image reference")
	}

	var r Ref

	// Split the digest first: '@' cannot appear in a repository or tag, so the
	// last '@' unambiguously introduces the digest.
	if i := strings.LastIndex(s, "@"); i >= 0 {
		r.Digest = s[i+1:]
		s = s[:i]
		if err := validateDigest(r.Digest); err != nil {
			return Ref{}, err
		}
	}

	// Registry detection follows the distribution convention: the first '/'
	// -delimited component is a registry host only if it contains a '.' or a
	// ':' or is "localhost". Otherwise the whole name belongs to docker.io.
	first, rest, hasSlash := strings.Cut(s, "/")
	if !hasSlash || (!strings.ContainsAny(first, ".:") && first != "localhost") {
		r.Registry = defaultRegistry
	} else {
		r.Registry = first
		s = rest
	}

	// The tag is introduced by the last ':' that trails a '/' (the host's
	// port, if any, was already consumed above).
	if i := strings.LastIndex(s, ":"); i >= 0 && !strings.Contains(s[i+1:], "/") {
		r.Tag = s[i+1:]
		s = s[:i]
	}

	// Whatever remains is the repository path: the final segment is the
	// repository and everything before it the namespace.
	segments := strings.Split(s, "/")
	r.Repository = segments[len(segments)-1]
	if len(segments) > 1 {
		r.Namespace = strings.Join(segments[:len(segments)-1], "/")
	}
	if r.Repository == "" {
		return Ref{}, fmt.Errorf("registry: reference %q has no repository", strings.TrimSpace(s))
	}

	// docker.io implies the library namespace when none was written.
	if r.Registry == defaultRegistry && r.Namespace == "" {
		r.Namespace = defaultLibrary
	}
	return r, nil
}

// String renders the reference fully qualified: the registry is always
// explicit and docker.io's single-component shorthand is expanded. Expanding
// removes all ambiguity about which registry and namespace a digest belongs
// to, which is what makes a pinned rendered reference reproducible.
func (r Ref) String() string {
	reg := r.Registry
	if reg == "" {
		reg = defaultRegistry
	}
	ns := r.Namespace
	if reg == defaultRegistry && ns == "" {
		ns = defaultLibrary
	}

	var b strings.Builder
	b.WriteString(reg)
	if ns != "" {
		b.WriteByte('/')
		b.WriteString(ns)
	}
	b.WriteByte('/')
	b.WriteString(r.Repository)
	if r.Tag != "" {
		b.WriteByte(':')
		b.WriteString(r.Tag)
	}
	if r.Digest != "" {
		b.WriteByte('@')
		b.WriteString(r.Digest)
	}
	return b.String()
}

// Pin drops any tag from ref and attaches digest, returning the canonical
// digest-pinned reference "repo@sha256:...". Pinning is deterministic, and
// re-pinning an already pinned reference is a no-op — the property that makes
// "redeploying an old revision produces the exact original image" hold
// (issue #51 criterion).
func Pin(ref, digest string) (string, error) {
	r, err := Parse(ref)
	if err != nil {
		return "", err
	}
	if err := validateDigest(digest); err != nil {
		return "", err
	}
	r.Tag = ""
	r.Digest = digest
	return r.String(), nil
}

// Mutable reports whether ref is not pinned by digest. A tag-only reference,
// an unparseable reference, or one that carries a tag alongside a digest is
// mutable; only a reference with a digest counts as pinned. Callers must fail
// closed: anything reported mutable must not reach a rendered manifest.
func Mutable(ref string) bool {
	r, err := Parse(ref)
	if err != nil {
		return true
	}
	return r.Digest == ""
}

func validateDigest(d string) error {
	alg, hex, ok := strings.Cut(d, ":")
	if !ok || alg == "" || hex == "" {
		return fmt.Errorf("registry: digest %q must be <algorithm>:<hex>", d)
	}
	for i := 0; i < len(hex); i++ {
		if !isHex(hex[i]) {
			return fmt.Errorf("registry: digest %q has a non-hex value", d)
		}
	}
	return nil
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}
