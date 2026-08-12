package git

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/dafrie/kelson/internal/delivery"
)

// HTTPDoer is the slice of *http.Client the forge clients need, so tests can
// point them at an httptest server (or a recording transport) without a real
// network.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// DefaultGitHubAPI is the public github.com REST endpoint.
const DefaultGitHubAPI = "https://api.github.com"

// GitHub creates pull requests through the github.com REST API (v3). It is a
// thin client on purpose: three calls, no SDK, so the dependency surface of a
// PaaS control plane stays small.
type GitHub struct {
	baseURL string
	auth    Auth
	http    HTTPDoer
}

// NewGitHub returns a GitHub pull-request provider.
func NewGitHub(opts ProviderOptions) *GitHub {
	base := strings.TrimSuffix(opts.BaseURL, "/")
	if base == "" {
		base = DefaultGitHubAPI
	}
	var doer HTTPDoer = http.DefaultClient
	if opts.HTTP != nil {
		doer = opts.HTTP
	}
	auth := opts.Auth
	if auth == nil {
		auth = Anonymous{}
	}
	return &GitHub{baseURL: base, auth: auth, http: doer}
}

func (g *GitHub) Name() string { return string(ProviderGitHub) }

// CreatePullRequest opens a PR for req.Head against req.Base. If GitHub
// reports one already exists for that head (a re-run of the same change), the
// existing PR is looked up and returned rather than failing — pull-request
// mode must be idempotent, since the same spec can be delivered twice.
func (g *GitHub) CreatePullRequest(ctx context.Context, req PullRequestRequest) (PullRequest, error) {
	body := map[string]any{
		"title": req.Title,
		"head":  req.Head,
		"base":  req.Base,
		"body":  req.Body,
	}
	if req.Draft {
		body["draft"] = true
	}

	path := fmt.Sprintf("/repos/%s/%s/pulls", req.Slug.Owner, req.Slug.Name)
	var created struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	status, raw, err := g.do(ctx, http.MethodPost, path, body, &created)
	switch {
	case err != nil:
		return PullRequest{}, err
	case status == http.StatusCreated:
		// created below
	case status == http.StatusUnprocessableEntity && alreadyExists(raw):
		existing, err := g.findOpenPR(ctx, req)
		if err != nil {
			return PullRequest{}, err
		}
		created.Number, created.HTMLURL = existing.Number, existing.URL
	default:
		return PullRequest{}, apiError("github", "creating the pull request", status, raw)
	}

	pr := PullRequest{Number: created.Number, URL: created.HTMLURL}
	if len(req.Reviewers) > 0 || len(req.TeamReviewers) > 0 {
		if err := g.requestReviewers(ctx, req, created.Number); err != nil {
			// The PR exists; a bad reviewer handle must not lose its URL.
			pr.Warnings = append(pr.Warnings, err.Error())
		}
	}
	return pr, nil
}

func (g *GitHub) requestReviewers(ctx context.Context, req PullRequestRequest, number int) error {
	body := map[string]any{}
	if len(req.Reviewers) > 0 {
		body["reviewers"] = req.Reviewers
	}
	if len(req.TeamReviewers) > 0 {
		body["team_reviewers"] = req.TeamReviewers
	}
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d/requested_reviewers", req.Slug.Owner, req.Slug.Name, number)
	status, raw, err := g.do(ctx, http.MethodPost, path, body, nil)
	if err != nil {
		return err
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return apiError("github", "requesting reviewers", status, raw)
	}
	return nil
}

func (g *GitHub) findOpenPR(ctx context.Context, req PullRequestRequest) (PullRequest, error) {
	q := url.Values{}
	q.Set("head", req.Slug.Owner+":"+req.Head)
	q.Set("base", req.Base)
	q.Set("state", "open")
	path := fmt.Sprintf("/repos/%s/%s/pulls?%s", req.Slug.Owner, req.Slug.Name, q.Encode())

	var list []struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	status, raw, err := g.do(ctx, http.MethodGet, path, nil, &list)
	if err != nil {
		return PullRequest{}, err
	}
	if status != http.StatusOK {
		return PullRequest{}, apiError("github", "looking up the existing pull request", status, raw)
	}
	if len(list) == 0 {
		return PullRequest{}, delivery.Conflict("git/pull-request", "head",
			"github reports a pull request already exists for this branch but none is open",
			"inspect the branch on the forge; a closed pull request for it must be reopened or the branch deleted")
	}
	return PullRequest{Number: list[0].Number, URL: list[0].HTMLURL}, nil
}

// do performs one API call. It returns the status and body so callers can
// distinguish "already exists" from a genuine failure.
func (g *GitHub) do(ctx context.Context, method, path string, in any, out any) (int, []byte, error) {
	var reader io.Reader
	if in != nil {
		buf, err := json.Marshal(in)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.baseURL+path, reader)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "kelson")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	token, ok, err := g.auth.APIToken(ctx)
	if err != nil {
		return 0, nil, err
	}
	if !ok {
		return 0, nil, delivery.Error{
			Code:     delivery.ErrUnsupported,
			Resource: "git/pull-request",
			Field:    "auth",
			Message: fmt.Sprintf("pull-request mode needs an API credential but the %q auth method cannot provide one",
				g.auth.Name()),
			Remediation: "configure a token (or pair the ssh key with a token: git.Pair{Transport: ssh, API: token})",
			DocsURL:     "https://kelson.dev/delivery/git#authentication",
		}
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := g.http.Do(req)
	if err != nil {
		e := delivery.ApplyFailed("git/pull-request", "",
			"the forge API call failed",
			"check network reachability of the forge API endpoint and the credential")
		e.Cause = err.Error()
		return 0, nil, e
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	if out != nil && resp.StatusCode < 300 && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			e := delivery.ApplyFailed("git/pull-request", "",
				"the forge API returned a response kelson could not parse",
				"check that the configured base URL points at the forge API, not its web UI")
			e.Cause = err.Error()
			return resp.StatusCode, raw, e
		}
	}
	return resp.StatusCode, raw, nil
}

func alreadyExists(raw []byte) bool {
	return strings.Contains(strings.ToLower(string(raw)), "already exists")
}

func apiError(provider, op string, status int, raw []byte) error {
	msg := strings.TrimSpace(string(raw))
	if len(msg) > 512 {
		msg = msg[:512] + "…"
	}
	e := delivery.ApplyFailed("git/pull-request", "",
		fmt.Sprintf("%s rejected %s (HTTP %d)", provider, op, status),
		"check the credential's scopes (pull requests: write) and that the base branch exists")
	e.Cause = msg
	return e
}

var _ Provider = (*GitHub)(nil)
