// Package forge is the seam between kelson and the git forges it holds
// credentials for ([ADR-0033](docs/adr/0033-git-connections.md) decision 3).
//
// # One operation is mandatory, everything else is a capability
//
// [Provider] has a single credential method, because exactly one thing has to
// work for a connection to be worth having: a private repository must be
// fetchable by a build pod (ADR-0033 decision 5, the gap that made a private
// repo resolve but not clone). Everything a forge offers on top of that — a
// repository picker, webhook deliveries, commit statuses, the one upserted
// pull-request comment — is an optional interface a caller type-asserts for:
// [RepoBrowser], [WebhookSource], [StatusReporter]. A `generic` token
// connection implements the mandatory one and none of the others, and that is
// a working connection rather than a broken one — absence degrades the UI,
// never the deploy. What this replaces is `if provider == "github"` spread
// through every caller, where the degradation path is invisible until it is
// hit.
//
// # No forge SDK, now or later
//
// The entire surface here is a dozen REST calls, one RS256-signed assertion
// and an HMAC. It is written against net/http rather than imported, because
// the alternative is an SDK-sized module tree in a repository whose lint
// allow-list is the standard library plus four things — the dependency-weight
// rule of [ADR-0022](docs/adr/0022-sops-age.md) and
// [ADR-0030](docs/adr/0030-flux-aio-install.md), enforced for this package by
// the `forge` rule in .golangci.yml.
//
// # This package never reads a Secret and never prints one
//
// A [Conn] is a connection already resolved: the GitConnection spec's
// identifiers joined with its Secret's material by a caller that has cluster
// access, which ADR-0033 decision 1 keeps in the server, the controller and
// the build plane. Nothing here speaks to Kubernetes, and nothing here logs.
// Credentials this package mints are returned to the caller and registered
// with internal/redact at the moment they are learned (ADR-0033 decision 2),
// so a value that escapes into a log through some other package's format
// string is still unprintable.
//
// PRProposer — ADR-0033 decision 3's fifth capability — is deliberately not
// declared. It lands with the propose-only flow ([ADR-0025](docs/adr/0025-agent-policy.md))
// that is its only caller; an interface with no implementation and no consumer
// describes nothing, which is the mistake [ADR-0028](docs/adr/0028-delivery-spine.md)
// decision 9 corrected one plane over.
package forge

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Conn is a resolved connection: the model's GitConnectionSpec joined with its
// Secret material by the caller. forge never reads a Secret itself.
type Conn struct {
	Provider string // "github" | "generic"
	Host     string // base URL, e.g. https://github.com
	// token auth
	Token, Username string
	// GitHub App auth
	AppID, InstallationID int64
	PrivateKeyPEM         []byte
	WebhookSecret         []byte
}

// String implements fmt.Stringer so a Conn interpolated with %v by an
// unsuspecting caller cannot spill the private key or the webhook secret. It
// reports the identity — which forge, which host, which auth shape — because
// that is what a diagnostic actually needs.
func (c Conn) String() string {
	return fmt.Sprintf("%s connection to %s (%s auth)", c.provider(), c.hostOrDefault(), c.authKind())
}

// authKind names which of the two auth shapes ADR-0033 decision 2 defines this
// connection carries, for error messages that must say which credential was
// rejected without saying what it was.
func (c Conn) authKind() string {
	switch {
	case c.usesApp():
		return "github app"
	case c.Token != "":
		return "token"
	default:
		return "no"
	}
}

// usesApp reports whether the connection carries GitHub App material. The
// installation id is deliberately not part of the test: an app connection
// whose installation has not been recorded yet is an app connection in a known
// intermediate state (ADR-0033 decision 2 step 3), and saying "no app
// configured" about it would send the user to the wrong screen.
func (c Conn) usesApp() bool { return c.AppID != 0 && len(c.PrivateKeyPEM) > 0 }

func (c Conn) provider() string {
	if c.Provider == "" {
		return "unknown"
	}
	return c.Provider
}

func (c Conn) hostOrDefault() string {
	if c.Host == "" {
		return defaultHost
	}
	return c.Host
}

// Credential is an HTTPS basic-auth pair for one repository operation.
//
// ExpiresAt is what makes the difference between the two auth shapes visible
// to a caller: an installation token is minted, lives about an hour and must
// be re-minted for the next build, while a stored token is the user's standing
// credential and expires when they say so.
type Credential struct {
	Username, Password string
	ExpiresAt          time.Time // zero for non-expiring (token auth)
}

// SecretValues returns the literal strings in this credential that must never
// appear in output, for registration with internal/redact (issue #117). It
// mirrors registry.Credential's method of the same name so both credential
// vocabularies register the same way.
//
// The username is not included: it is an identity, it is printed deliberately,
// and scrubbing "x-access-token" out of unrelated text helps nobody.
func (c Credential) SecretValues() []string {
	if c.Password == "" {
		return nil
	}
	return []string{c.Password}
}

// String implements fmt.Stringer so a Credential cannot be leaked by a log
// line or an error that interpolates it. Same envelope as
// registry.Credential.String, for the same reason (ADR-0009).
func (c Credential) String() string {
	if c.Username == "" {
		return "no credential"
	}
	if c.ExpiresAt.IsZero() {
		return fmt.Sprintf("username %q (password redacted)", c.Username)
	}
	return fmt.Sprintf("username %q (password redacted, expires %s)", c.Username, c.ExpiresAt.UTC().Format(time.RFC3339))
}

// Provider is the one interface every connection satisfies.
type Provider interface {
	Name() string
	// MintCloneCredential returns a short-lived HTTPS basic-auth credential for
	// read access to repoURL (App installation token), or the stored token.
	MintCloneCredential(ctx context.Context, c Conn, repoURL string) (Credential, error)
}

// Repo is one repository as a picker needs it: enough to show a row and to
// fill in a Project's source, and nothing more. Whatever else a forge reports
// about a repository is that forge's business.
type Repo struct {
	FullName, HTMLURL, DefaultBranch string
	Private                          bool
}

// RepoBrowser is the capability behind the New Project repository picker. Its
// absence is the difference between typing a URL and choosing from a list — a
// UI difference, never a delivery one.
type RepoBrowser interface {
	ListRepositories(ctx context.Context, c Conn) ([]Repo, error)
	ListBranches(ctx context.Context, c Conn, repoFullName string) ([]string, error)
}

// Event is a webhook delivery reduced to what the trigger pipeline acts on
// ([ADR-0034](docs/adr/0034-forge-driven-delivery.md) decision 1).
//
// It is deliberately lossy. An event never mutates state directly — it
// enqueues work that re-reads the spec, the connection and the forge — so the
// fields that survive parsing are the ones that identify *what to reconcile*,
// not the ones that describe what happened. A payload field that is not here
// is a field no reconcile needs, and a forged or replayed delivery can at
// worst cause a redundant reconcile of true state.
type Event struct {
	Kind           string // "ping" | "push" | "pull_request" | "installation"
	RepoFullName   string
	RepoHTMLURL    string
	Ref            string // push: the ref pushed
	HeadSHA        string // push: after; pull_request: head sha
	PRNumber       int
	Action         string // pull_request/installation action verbatim
	InstallationID int64
}

// WebhookSource is the capability behind the reactive half of ADR-0034: HMAC
// verification and the payload reduction above. A connection without it polls,
// which is slower and equally correct.
type WebhookSource interface {
	VerifySignature(c Conn, header http.Header, body []byte) error
	ParseEvent(header http.Header, body []byte) (Event, error)
}

// Status is a commit status write-back (ADR-0034 decision 5). State is one of
// pending, success, failure, error — the vocabulary GitHub takes, which the
// delivery state machine's phases map onto rather than the other way round.
type Status struct{ State, Context, Description, TargetURL string }

// StatusReporter is the write-back capability. ADR-0034 decision 5 makes its
// absence silent on purpose: a status is a courtesy of the integration, and a
// deploy that succeeded but could not say so on the commit is a deploy that
// succeeded.
type StatusReporter interface {
	ReportStatus(ctx context.Context, c Conn, repoFullName, sha string, s Status) error
	// UpsertPRComment finds the comment containing marker and edits it, or creates one.
	UpsertPRComment(ctx context.Context, c Conn, repoFullName string, pr int, marker, body string) error
}

const (
	nameGitHub  = "github"
	nameGeneric = "generic"
)

// providers is the adapter table [For] answers from. It is written once during
// package initialisation and only read afterwards, so it needs no lock of its
// own; the GitHub adapter's token cache carries the mutex that is actually
// contended.
var providers = map[string]Provider{
	nameGitHub:  newGitHub(),
	nameGeneric: genericProvider{},
}

// For returns the adapter for a GitConnection's spec.provider value.
//
// The second return is false for a forge nobody has written an adapter for,
// and callers report that rather than falling back to the generic one: a
// connection declaring a provider kelson does not speak is a mistake worth
// naming, and silently treating it as a bare git host would answer a question
// about GitLab merge requests with a token that can only clone. The enum grows
// per adapter (ADR-0033 decision 3).
func For(provider string) (Provider, bool) {
	p, ok := providers[strings.ToLower(strings.TrimSpace(provider))]
	return p, ok
}
