package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// The GitHub adapter: the one forge kelson speaks in full (ADR-0033 decision
// 3, "GitHub ships first"). It implements every capability this seam declares,
// which is what makes the capability split testable — a seam whose only
// implementation satisfies everything proves nothing about degradation, so
// [genericProvider] exists beside it as the other end of the range.

const (
	// defaultHost is where a connection with no host points. Empty means
	// github.com rather than an error because the overwhelming majority of
	// connections are to github.com and a mandatory field that has one sane
	// value is vocabulary without information (ADR-0033 decision 4 makes the
	// same argument for the connection reference itself).
	defaultHost = "https://github.com"

	// dotComAPI is github.com's API host. GitHub Enterprise Server has no
	// equivalent — it serves the same API under /api/v3 on the instance's own
	// host — so the derivation below is a branch, not a template.
	dotComAPI = "https://api.github.com"

	// enterpriseAPIPath is the REST prefix on a GitHub Enterprise Server host.
	enterpriseAPIPath = "/api/v3"

	// apiVersion pins the REST API's dated version. Unpinned requests get
	// whatever GitHub currently defaults to, which is how an integration
	// breaks on a day nobody deployed anything.
	apiVersion = "2022-11-28"

	// userAgent identifies kelson to the forge. GitHub rejects requests
	// without one.
	userAgent = "kelson"

	// acceptJSON is GitHub's versioned media type.
	acceptJSON = "application/vnd.github+json"

	// perPage is the page size for every paginated read. 100 is the maximum
	// GitHub allows, which is the right choice for lists a human will scroll.
	perPage = 100

	// maxPages bounds a paginated read. A forge that answers every page with a
	// next link — a proxy in a loop, a bug, a hostile endpoint — must not turn
	// a repository picker into an unbounded fetch.
	maxPages = 100

	// maxResponse bounds a single response body. The lists here are bounded by
	// perPage; this is the backstop against an endpoint that is not what it
	// claims to be.
	maxResponse = 8 << 20

	// requestTimeout is the per-request ceiling for the default client. A
	// caller with its own deadline passes a context and gets the shorter of
	// the two.
	requestTimeout = 30 * time.Second
)

// gitHubProvider is the GitHub adapter and the cache of installation tokens it
// has minted.
type gitHubProvider struct {
	// now is the clock the JWT and the token cache are dated against. It is a
	// field rather than a call to time.Now so cache expiry is testable without
	// sleeping for an hour.
	now func() time.Time

	// client is the HTTP client; nil uses the package default. Tests reach the
	// httptest server through Conn.Host instead, so this exists for a caller
	// that must supply a proxy or a transport, not for the tests.
	client *http.Client

	// mu guards tokens only. It is deliberately not held across the mint
	// request: two concurrent builds racing to mint produce two valid tokens
	// and one wasted call, while a lock spanning the network call would queue
	// every build in the instance behind one slow forge.
	mu     sync.Mutex
	tokens map[string]Credential
}

func newGitHub() *gitHubProvider {
	return &gitHubProvider{now: time.Now, tokens: make(map[string]Credential)}
}

// Name implements [Provider].
func (g *gitHubProvider) Name() string { return nameGitHub }

var (
	_ Provider       = (*gitHubProvider)(nil)
	_ RepoBrowser    = (*gitHubProvider)(nil)
	_ WebhookSource  = (*gitHubProvider)(nil)
	_ StatusReporter = (*gitHubProvider)(nil)
	_ PRProposer     = (*gitHubProvider)(nil)
)

// MintCloneCredential implements [Provider].
//
// repoURL is not used to scope the credential, and that is a decision rather
// than an omission: an installation token already carries exactly the
// repositories the user chose to install on (ADR-0033 decision 2, "per-repository
// installation choice"), and asking for a per-repository token instead would
// mint one credential per build per repository — defeating the cache below for
// a scope the user has already narrowed by hand.
func (g *gitHubProvider) MintCloneCredential(ctx context.Context, c Conn, repoURL string) (Credential, error) {
	switch {
	case c.usesApp():
		return g.installationToken(ctx, c)
	case c.Token != "":
		return tokenCredential(c), nil
	default:
		return Credential{}, fmt.Errorf("forge/github: %s carries no credential for %s: %w", c, repoName(repoURL), ErrAuthFailed)
	}
}

// tokenCredential is the token-auth answer, shared with the generic adapter:
// the stored token, under the username forges ignore but require
// (internal/gitref/auth.go picks the same default, so a connection and a
// `git ls-remote` authenticate identically).
func tokenCredential(c Conn) Credential {
	user := c.Username
	if user == "" {
		user = defaultTokenUsername
	}
	return Credential{Username: user, Password: c.Token}
}

// defaultTokenUsername matches gitref.Token's default. GitHub ignores the
// username on a token; something non-empty is required.
const defaultTokenUsername = "x-access-token"

// repoName reduces a clone URL to owner/repo for an error message, so a
// failure names the repository the user recognises rather than the URL kelson
// assembled. A URL it cannot reduce is returned as-is.
func repoName(repoURL string) string {
	u, err := url.Parse(strings.TrimSpace(repoURL))
	if err != nil || u.Path == "" {
		return repoURL
	}
	return strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git")
}

// apiBase derives the REST base URL from a connection's host.
//
// github.com's API lives on a different hostname; every GitHub Enterprise
// Server instance serves it under /api/v3 on its own. This is the one place
// that knows the difference, so every call below is written as base + path.
func apiBase(host string) (string, error) {
	h := strings.TrimSpace(host)
	if h == "" {
		h = defaultHost
	}
	if !strings.Contains(h, "://") {
		// A host pasted without a scheme is the commonest form of this
		// mistake, and HTTPS is the only scheme this seam speaks anyway
		// (ADR-0033 "Revisit when": everything here is HTTPS-only).
		h = "https://" + h
	}
	u, err := url.Parse(h)
	if err != nil {
		return "", fmt.Errorf("forge/github: connection host %q is not a URL: %w", host, err)
	}
	if u.Host == "" {
		return "", fmt.Errorf("forge/github: connection host %q names no host", host)
	}
	switch strings.ToLower(u.Hostname()) {
	case "github.com", "www.github.com", "api.github.com":
		return dotComAPI, nil
	}
	return u.Scheme + "://" + u.Host + strings.TrimSuffix(u.Path, "/") + enterpriseAPIPath, nil
}

// webBase is the browser-facing base URL for a host, which is the connection's
// host with any trailing slash removed. It differs from [apiBase] only for
// github.com, and only because the API moved hostnames.
func webBase(host string) (string, error) {
	h := strings.TrimSpace(host)
	if h == "" {
		h = defaultHost
	}
	if !strings.Contains(h, "://") {
		h = "https://" + h
	}
	u, err := url.Parse(h)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("forge/github: connection host %q is not a URL", host)
	}
	return strings.TrimSuffix(u.Scheme+"://"+u.Host+u.Path, "/"), nil
}

// splitFullName validates owner/repo before it is interpolated into a path.
// The value reaches here from a spec field and a UI form, and a path segment
// assembled from unvalidated input is how a list call becomes a call to
// something else entirely.
func splitFullName(full string) (owner, repo string, err error) {
	parts := strings.Split(strings.Trim(strings.TrimSpace(full), "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("forge/github: %q is not an owner/repository name", full)
	}
	return parts[0], parts[1], nil
}

// repoPath is base + /repos/owner/repo, with both segments escaped.
func repoPath(base, fullName string) (string, error) {
	owner, repo, err := splitFullName(fullName)
	if err != nil {
		return "", err
	}
	return base + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo), nil
}

func (g *gitHubProvider) httpClient() *http.Client {
	if g.client != nil {
		return g.client
	}
	return defaultClient
}

// defaultClient is shared by every adapter instance: a client per request
// leaks connections, and the zero-value http.DefaultClient has no timeout at
// all, which is how a hung forge becomes a hung reconcile.
var defaultClient = &http.Client{Timeout: requestTimeout}

// send issues one API request and returns the body and headers.
//
// It returns raw bytes rather than decoding into an out parameter because the
// two list endpoints disagree about their envelope — /installation/repositories
// wraps its array in an object, /user/repos does not — and one signature that
// serves both keeps the pagination loop single.
func (g *gitHubProvider) send(ctx context.Context, method, u, authorization string, body any) ([]byte, http.Header, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, nil, fmt.Errorf("forge/github: encoding the request for %s: %w", u, err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, reader)
	if err != nil {
		return nil, nil, fmt.Errorf("forge/github: %s %s: %w", method, u, err)
	}
	req.Header.Set("Accept", acceptJSON)
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("User-Agent", userAgent)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := g.httpClient().Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("forge/github: %s %s: %w", method, u, err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return nil, resp.Header, fmt.Errorf("forge/github: reading the response to %s %s: %w", method, u, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return data, resp.Header, newHTTPError(method, u, resp.StatusCode, data)
	}
	return data, resp.Header, nil
}

// errStopPaging ends a paginated read early, returned by a page callback that
// has found what it came for. Without it a search walks every remaining page
// after the answer is already known — which for the comment lookup means
// reading a busy pull request's entire thread on every publish.
var errStopPaging = errors.New("forge: the paginated read found what it was looking for")

// paginate walks a list endpoint by following the Link header, which is what
// GitHub documents and what keeps this correct when a page is exactly full: a
// page counter would either stop one page early or make one extra request per
// list, and only one of those is a bug the tests would catch.
func (g *gitHubProvider) paginate(ctx context.Context, u, authorization string, page func([]byte) error) error {
	for i := 0; u != "" && i < maxPages; i++ {
		data, header, err := g.send(ctx, http.MethodGet, u, authorization, nil)
		if err != nil {
			return err
		}
		if err := page(data); err != nil {
			if errors.Is(err, errStopPaging) {
				return nil
			}
			return err
		}
		u = nextLink(header, u)
	}
	return nil
}

// nextLink returns the rel="next" URL from a Link header, if it points at the
// same host the current page came from. A next link to somewhere else is not
// followed: the pagination cursor is server-controlled, and following it
// wherever it points would send the connection's Authorization header to a
// host the connection is not for.
func nextLink(header http.Header, current string) string {
	link := header.Get("Link")
	if link == "" {
		return ""
	}
	from, err := url.Parse(current)
	if err != nil {
		return ""
	}
	// Segments are comma-separated; the URLs GitHub emits carry no commas.
	for _, segment := range strings.Split(link, ",") {
		parts := strings.Split(segment, ";")
		if len(parts) < 2 {
			continue
		}
		target := strings.TrimSpace(parts[0])
		if !strings.HasPrefix(target, "<") || !strings.HasSuffix(target, ">") {
			continue
		}
		rel := false
		for _, param := range parts[1:] {
			v := strings.TrimSpace(param)
			if v == `rel="next"` || v == "rel=next" {
				rel = true
			}
		}
		if !rel {
			continue
		}
		next, err := url.Parse(strings.Trim(target, "<>"))
		if err != nil || next.Scheme != from.Scheme || next.Host != from.Host {
			return ""
		}
		return next.String()
	}
	return ""
}

// authorization resolves the header value for an API call: a freshly minted
// installation token for app auth, the stored token otherwise. Both are sent
// as Bearer, which GitHub accepts for either.
func (g *gitHubProvider) authorization(ctx context.Context, c Conn) (string, error) {
	switch {
	case c.usesApp():
		cred, err := g.installationToken(ctx, c)
		if err != nil {
			return "", err
		}
		return "Bearer " + cred.Password, nil
	case c.Token != "":
		return "Bearer " + c.Token, nil
	default:
		return "", fmt.Errorf("forge/github: %s carries no credential: %w", c, ErrAuthFailed)
	}
}
