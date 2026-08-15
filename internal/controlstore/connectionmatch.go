package controlstore

import (
	"net/url"
	"sort"
	"strings"
)

// Which connection a project's source resolves to (ADR-0033 decision 4).
//
// # Why it is here, and why it touches no cluster
//
// Three callers need this answer and they run in different planes: the API's
// DeleteConnection names the projects a delete will break, the build plane
// mints the credential a clone uses, and the preview path materializes the
// flux-operator Secret from whichever connection matches `previews.repo`. A
// resolution each of them implemented would be three answers to a question with
// one right one, so the rule lives once, as a pure function over values the
// caller has already read.
//
// Nothing in this file opens a connection, reads a Secret or names a
// Kubernetes type: [MatchConnection] takes the source URL, the explicit
// override and the connections the instance holds, and returns which one wins.
// That is what lets it live in the store package without dragging the store's
// dependencies into a caller that only wants the rule.
//
// # The rule
//
// `Project.spec.source.connection` wins outright when it is set — an explicit
// name is the field ADR-0033 decision 4 provides to disambiguate, and a host
// match that overrode it would make the field advisory. A name that matches no
// connection is [MatchMissing] rather than a silent fall back to host matching:
// the author said which one, and quietly using another is the failure mode the
// override exists to prevent.
//
// Otherwise it is longest host-then-owner match:
//
//  1. A connection is a candidate when the source URL's host equals the
//     connection's host, and the source's path lies under the connection's own
//     base path. (A base path is empty for github.com and non-empty only for a
//     forge served under a prefix, e.g. https://git.acme.internal/gitlab.)
//  2. Candidates are ranked by how much of the URL they matched: the longer
//     host-plus-base-path prefix wins, and among equal prefixes a connection
//     whose provider-reported account *is* the repository's owner beats one
//     that has never been probed, which in turn beats one whose account is a
//     different owner.
//  3. One winner is the answer. A tie at the top rank is [MatchAmbiguous]
//     naming every tied connection — ADR-0033 decision 4: "a resolution error
//     naming both and the field that disambiguates, never a silent pick".
//
// The third rank is deliberately a rank and not a disqualification. A token
// connection's account is whoever the token belongs to, and that account can
// legitimately see repositories under other owners; refusing them would break
// the single-connection case the moment the first probe recorded a login.
//
// # Public repositories resolve to nothing, which is not a failure
//
// [MatchNone] is the ordinary answer for a source no connection covers.
// Anonymous is still the default (`gitref.Anonymous`), and a public repository
// needs no connection at all — so a caller turns MatchNone into "clone
// anonymously", never into an error.

// MatchKind is what [MatchConnection] concluded.
type MatchKind int

const (
	// MatchNone: no connection covers this source. Clone anonymously.
	MatchNone MatchKind = iota

	// MatchExplicit: `source.connection` named this one, and it exists.
	MatchExplicit

	// MatchHost: no name was given and exactly one connection won the
	// host-then-owner ranking.
	MatchHost

	// MatchAmbiguous: several connections tied at the top rank. Candidates
	// holds every one of them, sorted, so the error can name them all.
	MatchAmbiguous

	// MatchMissing: `source.connection` named a connection the instance does
	// not hold. Name carries the name it asked for.
	MatchMissing
)

// String renders the kind for a diagnostic.
func (k MatchKind) String() string {
	switch k {
	case MatchExplicit:
		return "explicit"
	case MatchHost:
		return "host"
	case MatchAmbiguous:
		return "ambiguous"
	case MatchMissing:
		return "missing"
	default:
		return "none"
	}
}

// ConnectionRef is one connection as the matcher sees it: its name, the forge
// base URL it means, and who the provider last said the credential acts as.
//
// It is a value rather than [StoredConnection] because the matcher must be
// callable by a plane that reads connections some other way — the controller
// from an informer cache, a test from a literal — and because a rule that took
// the store's own type would be a rule only the store could run.
type ConnectionRef struct {
	// Name is the CR's metadata.name, and what `source.connection` names.
	Name string
	// Host is the *effective* forge base URL: model.GitConnectionSpec's
	// EffectiveHost, so a `provider: github` connection that named no host has
	// already become https://github.com before it gets here.
	Host string
	// Account is `status.account` — provider-reported, empty until a probe has
	// succeeded. Empty is "not known yet", never "no account".
	Account string
}

// ConnectionMatch is the resolution.
type ConnectionMatch struct {
	Kind MatchKind
	// Name is the winning connection for MatchExplicit and MatchHost, and the
	// name that was asked for and not found for MatchMissing.
	Name string
	// Candidates are the tied connections for MatchAmbiguous, sorted by name.
	Candidates []string
}

// Resolved reports whether the match names a connection a caller may use.
func (m ConnectionMatch) Resolved() bool {
	return m.Kind == MatchExplicit || m.Kind == MatchHost
}

// MatchConnection resolves one project source against the connections an
// instance holds. named is `Project.spec.source.connection` and may be empty;
// sourceGit is `Project.spec.source.git`.
func MatchConnection(sourceGit, named string, conns []ConnectionRef) ConnectionMatch {
	if name := strings.TrimSpace(named); name != "" {
		for _, c := range conns {
			if c.Name == name {
				return ConnectionMatch{Kind: MatchExplicit, Name: name}
			}
		}
		return ConnectionMatch{Kind: MatchMissing, Name: name}
	}

	host, path, ok := splitRemote(sourceGit)
	if !ok {
		return ConnectionMatch{Kind: MatchNone}
	}
	owner := ownerOf(path)

	best := -1
	var winners []string
	for _, c := range conns {
		score, matched := rank(host, path, owner, c)
		if !matched {
			continue
		}
		switch {
		case score > best:
			best, winners = score, []string{c.Name}
		case score == best:
			winners = append(winners, c.Name)
		}
	}

	switch len(winners) {
	case 0:
		return ConnectionMatch{Kind: MatchNone}
	case 1:
		return ConnectionMatch{Kind: MatchHost, Name: winners[0]}
	default:
		sort.Strings(winners)
		return ConnectionMatch{Kind: MatchAmbiguous, Candidates: winners}
	}
}

// AffectedProjects reports which of the given sources resolve through one named
// connection, sorted and deduplicated — the answer DeleteConnection puts in
// `affected_projects`.
//
// A project whose resolution is [MatchAmbiguous] is deliberately not listed,
// even when this connection is one of the tied candidates. The field means "the
// builds that will start failing", and an ambiguous project's builds are
// already failing on the resolution error; deleting one of the two connections
// is what *fixes* it, so naming it as collateral would describe the change
// backwards.
func AffectedProjects(connection string, sources []ProjectSource, conns []ConnectionRef) []string {
	seen := map[string]bool{}
	var out []string
	for _, src := range sources {
		m := MatchConnection(src.Git, src.Connection, conns)
		if !m.Resolved() || m.Name != connection || seen[src.Project] {
			continue
		}
		seen[src.Project] = true
		out = append(out, src.Project)
	}
	sort.Strings(out)
	return out
}

// ProjectSource is one project's source as [AffectedProjects] needs it: which
// project, where its code is, and whether it named a connection outright.
type ProjectSource struct {
	Project    string
	Git        string
	Connection string
}

// rank scores one connection against a source. The host-plus-base-path prefix
// length dominates, because a more specific host is a more specific claim on
// the URL; the owner comparison only breaks ties between equally specific
// hosts.
func rank(host, path, owner string, c ConnectionRef) (int, bool) {
	chost, cpath, ok := splitRemote(c.Host)
	if !ok || chost != host {
		return 0, false
	}
	// The connection's base path must be a *path segment* prefix of the
	// source's, so a connection at /gitlab does not claim /gitlab-mirror.
	if cpath != "" && !strings.HasPrefix(path, cpath+"/") {
		return 0, false
	}

	owned := 0
	switch {
	case c.Account != "" && strings.EqualFold(c.Account, owner):
		owned = 2
	case c.Account == "":
		// Never probed. It may well be the right connection; it just has not
		// said so yet, so it ranks below one that has.
		owned = 1
	}
	return (len(chost)+len(cpath))*10 + owned, true
}

// splitRemote reduces a remote URL to its host (with port, lowercased) and its
// path (no leading or trailing slash, no `.git` suffix).
//
// It accepts the scp-like `git@host:owner/repo` spelling as well as a URL.
// Everything kelson does with a connection is HTTPS (ADR-0033 "Revisit when":
// SSH is a different credential class) — but a spec may still carry an SSH
// remote, and reading its host to say "no connection covers this" is better
// than failing to parse and answering the same thing by accident.
func splitRemote(remote string) (host, path string, ok bool) {
	raw := strings.TrimSpace(remote)
	if raw == "" {
		return "", "", false
	}
	if !strings.Contains(raw, "://") {
		if at := strings.Index(raw, "@"); at >= 0 {
			if colon := strings.Index(raw[at:], ":"); colon >= 0 {
				h := raw[at+1 : at+colon]
				return strings.ToLower(h), trimPath(raw[at+colon+1:]), h != ""
			}
		}
		// A bare host or host/path, which is how a hand-written spec often
		// spells one. Parsing it as https is the reading that lets it match.
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", "", false
	}
	return strings.ToLower(u.Host), trimPath(u.Path), true
}

func trimPath(p string) string {
	return strings.TrimSuffix(strings.Trim(p, "/"), ".git")
}

// ownerOf is the first path segment: the organisation or user a repository
// belongs to. A path with no segment has no owner, which ranks a connection the
// way an unprobed one ranks.
func ownerOf(path string) string {
	if path == "" {
		return ""
	}
	if i := strings.Index(path, "/"); i >= 0 {
		return path[:i]
	}
	return path
}
