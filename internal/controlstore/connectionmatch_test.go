package controlstore

import (
	"reflect"
	"testing"
)

// The resolution rule of ADR-0033 decision 4, pinned case by case.
//
// It is table-driven rather than narrative because the interesting part is the
// *ordering* between the tiers — explicit beats host, a longer host beats a
// shorter one, a matching account beats an unprobed connection — and a rule
// with an ordering is only tested by cases that disagree.

var (
	githubUnprobed = ConnectionRef{Name: "github", Host: "https://github.com"}
	githubAcme     = ConnectionRef{Name: "acme-github", Host: "https://github.com", Account: "acme"}
	githubGlobex   = ConnectionRef{Name: "globex-github", Host: "https://github.com", Account: "globex"}
	gheRoot        = ConnectionRef{Name: "ghe", Host: "https://git.acme.internal"}
	gheTeam        = ConnectionRef{Name: "ghe-team", Host: "https://git.acme.internal/team"}
)

func TestMatchConnection(t *testing.T) {
	cases := []struct {
		name   string
		git    string
		named  string
		conns  []ConnectionRef
		want   MatchKind
		winner string
		tied   []string
	}{{
		name:   "one connection, zero configuration",
		git:    "https://github.com/acme/checkout",
		conns:  []ConnectionRef{githubUnprobed},
		want:   MatchHost,
		winner: "github",
	}, {
		name:  "no connection covers the host",
		git:   "https://gitlab.com/acme/checkout",
		conns: []ConnectionRef{githubUnprobed, gheRoot},
		want:  MatchNone,
	}, {
		// ADR-0033 decision 4: never a silent pick.
		name:  "two unprobed connections to one host are ambiguous",
		git:   "https://github.com/acme/checkout",
		conns: []ConnectionRef{githubUnprobed, {Name: "other", Host: "https://github.com"}},
		want:  MatchAmbiguous,
		tied:  []string{"github", "other"},
	}, {
		name:   "the account that owns the repository wins the tie",
		git:    "https://github.com/acme/checkout",
		conns:  []ConnectionRef{githubAcme, githubGlobex, githubUnprobed},
		want:   MatchHost,
		winner: "acme-github",
	}, {
		// A probed connection for another account still loses to an unprobed
		// one, because "has never said" is a weaker claim than "said no".
		name:   "an unprobed connection beats one that belongs to another account",
		git:    "https://github.com/acme/checkout",
		conns:  []ConnectionRef{githubGlobex, githubUnprobed},
		want:   MatchHost,
		winner: "github",
	}, {
		// The single-connection case must keep working after the first probe
		// records a login that is not the repository's owner: a person's token
		// routinely sees repositories under other owners.
		name:   "a lone connection matches even when its account differs",
		git:    "https://github.com/acme/checkout",
		conns:  []ConnectionRef{githubGlobex},
		want:   MatchHost,
		winner: "globex-github",
	}, {
		name:   "the longer host path wins",
		git:    "https://git.acme.internal/team/acme/checkout",
		conns:  []ConnectionRef{gheRoot, gheTeam},
		want:   MatchHost,
		winner: "ghe-team",
	}, {
		name:   "a base path is matched by segment, not by prefix",
		git:    "https://git.acme.internal/team-archive/acme/checkout",
		conns:  []ConnectionRef{gheRoot, gheTeam},
		want:   MatchHost,
		winner: "ghe",
	}, {
		name:   "an explicit name beats every host match",
		git:    "https://github.com/acme/checkout",
		named:  "globex-github",
		conns:  []ConnectionRef{githubAcme, githubGlobex},
		want:   MatchExplicit,
		winner: "globex-github",
	}, {
		// The override exists to disambiguate; falling back to a host match
		// would make it advisory, which is the failure it prevents.
		name:   "an explicit name that does not exist is not a host match",
		git:    "https://github.com/acme/checkout",
		named:  "typo",
		conns:  []ConnectionRef{githubAcme},
		want:   MatchMissing,
		winner: "typo",
	}, {
		name:   "an explicit name resolves with no source URL at all",
		named:  "acme-github",
		conns:  []ConnectionRef{githubAcme},
		want:   MatchExplicit,
		winner: "acme-github",
	}, {
		name:   "a scp-style remote is read for its host",
		git:    "git@github.com:acme/checkout.git",
		conns:  []ConnectionRef{githubAcme},
		want:   MatchHost,
		winner: "acme-github",
	}, {
		name:   "a host written without a scheme still matches",
		git:    "github.com/acme/checkout",
		conns:  []ConnectionRef{githubAcme},
		want:   MatchHost,
		winner: "acme-github",
	}, {
		name:   "the host comparison is case-insensitive",
		git:    "https://GitHub.com/Acme/checkout",
		conns:  []ConnectionRef{githubAcme},
		want:   MatchHost,
		winner: "acme-github",
	}, {
		name:  "an empty source resolves to nothing",
		conns: []ConnectionRef{githubAcme},
		want:  MatchNone,
	}, {
		name:  "a port is part of the host",
		git:   "https://git.acme.internal:8443/acme/checkout",
		conns: []ConnectionRef{gheRoot},
		want:  MatchNone,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := MatchConnection(tc.git, tc.named, tc.conns)
			if got.Kind != tc.want {
				t.Fatalf("kind = %s, want %s (%+v)", got.Kind, tc.want, got)
			}
			if tc.winner != "" && got.Name != tc.winner {
				t.Errorf("name = %q, want %q", got.Name, tc.winner)
			}
			if tc.tied != nil && !reflect.DeepEqual(got.Candidates, tc.tied) {
				t.Errorf("candidates = %v, want %v", got.Candidates, tc.tied)
			}
		})
	}
}

func TestAffectedProjectsNamesEveryRouteToTheConnection(t *testing.T) {
	conns := []ConnectionRef{githubAcme, githubGlobex, gheRoot}
	sources := []ProjectSource{
		{Project: "checkout", Git: "https://github.com/acme/checkout"},
		{Project: "billing", Git: "https://github.com/globex/billing"},
		// Explicitly named, against a repository the host match would have
		// given to the other connection.
		{Project: "shop", Git: "https://github.com/globex/shop", Connection: "acme-github"},
		{Project: "internal", Git: "https://git.acme.internal/acme/tools"},
		{Project: "public", Git: "https://gitlab.com/acme/site"},
	}

	got := AffectedProjects("acme-github", sources, conns)
	want := []string{"checkout", "shop"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("affected = %v, want %v", got, want)
	}
	if got := AffectedProjects("ghe", sources, conns); !reflect.DeepEqual(got, []string{"internal"}) {
		t.Errorf("the self-hosted connection serves %v, want [internal]", got)
	}
	if got := AffectedProjects("nobody-uses-this", sources, conns); len(got) != 0 {
		t.Errorf("an unused connection affects %v, want nothing", got)
	}
}

// An ambiguous project is not listed, and the reason is in the doc comment: its
// builds already fail on the resolution error, and deleting one of the two
// connections is what fixes it. Naming it as collateral would describe the
// change backwards.
func TestAffectedProjectsExcludesAnAmbiguousProject(t *testing.T) {
	conns := []ConnectionRef{githubUnprobed, {Name: "second", Host: "https://github.com"}}
	sources := []ProjectSource{{Project: "checkout", Git: "https://github.com/acme/checkout"}}

	for _, name := range []string{"github", "second"} {
		if got := AffectedProjects(name, sources, conns); len(got) != 0 {
			t.Errorf("deleting %s reports %v affected; the project resolves through neither", name, got)
		}
	}
}
