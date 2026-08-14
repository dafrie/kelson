package registry

import (
	"fmt"
	"strings"
)

// Plain-HTTP registries, and why they are an operator list rather than a
// guess.
//
// The local story pushes to a registry with no TLS at all — a kind cluster
// with `localhost:5000`, an in-cluster registry reached by its Service name.
// Neither BuildKit nor the CNB lifecycle will talk plain HTTP to a host it was
// not told about, and neither should: silently downgrading a push because the
// TLS handshake failed is how a credential ends up on the wire in clear.
//
// So the exception is named, once, by the operator (`--insecure-registries`,
// `KELSON_INSECURE_REGISTRIES`), it is a list of hosts rather than a boolean,
// and a driver marks a registry insecure only when it appears in that list.
// The parsing lives here, next to Ref, because `kelson build` and
// kelson-server both read the same variable and a second spelling of the split
// would be a second answer to "is this host insecure?".

// ParseInsecure splits the comma-separated host[:port] list that
// --insecure-registries and $KELSON_INSECURE_REGISTRIES carry. Empty entries
// and surrounding whitespace are ignored, so "a, b," is the two hosts it looks
// like; anything that is not a bare host is an error rather than an entry that
// silently matches nothing.
func ParseInsecure(list string) ([]string, error) {
	var hosts []string
	for _, entry := range strings.Split(list, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		hosts = append(hosts, entry)
	}
	if err := ValidateInsecure(hosts); err != nil {
		return nil, err
	}
	return hosts, nil
}

// ValidateInsecure checks an already-split list. Drivers call it from their
// pure render step: an entry carrying a scheme or a repository path would be
// written into a buildkitd registry key or a CNB_INSECURE_REGISTRIES entry
// that matches no image at all, and a push that then fails on TLS is a
// confusing way to learn about a typo.
func ValidateInsecure(hosts []string) error {
	for _, h := range hosts {
		switch {
		case h == "":
			return fmt.Errorf("registry: an insecure registry entry is empty; each entry is a host with an optional port, e.g. localhost:5000")
		case strings.Contains(h, "://"):
			return fmt.Errorf("registry: insecure registry %q must not carry a scheme; write the host only, e.g. localhost:5000", h)
		case strings.Contains(h, "/"):
			return fmt.Errorf("registry: insecure registry %q must not carry a repository path; write the host only, e.g. localhost:5000", h)
		case !registryHost.MatchString(h):
			return fmt.Errorf("registry: insecure registry %q is not a host with an optional port, e.g. localhost:5000 or registry.internal", h)
		}
	}
	return nil
}

// IsInsecure reports whether ref is pushed to or pulled from one of hosts.
//
// The comparison is on the parsed registry host, so it is the same "where does
// the host end and the namespace begin?" rule Repository uses, and it is
// case-insensitive because a registry host is a DNS name. A reference that
// does not parse is not insecure: failing closed here means the caller's own
// validation reports the malformed reference, and nothing is downgraded to
// plain HTTP because of a parse error.
func IsInsecure(hosts []string, ref string) bool {
	if len(hosts) == 0 {
		return false
	}
	r, err := Parse(ref)
	if err != nil {
		return false
	}
	for _, h := range hosts {
		if strings.EqualFold(strings.TrimSpace(h), r.Registry) {
			return true
		}
	}
	return false
}
