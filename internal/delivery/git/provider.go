package git

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/dafrie/kelson/internal/delivery"
)

// Slug identifies a repository on a forge.
type Slug struct {
	Host  string
	Owner string
	Name  string
}

func (s Slug) String() string { return s.Owner + "/" + s.Name }

// PullRequestRequest is a forge-independent pull-request creation request.
type PullRequestRequest struct {
	Slug      Slug
	Head      string // branch carrying the change
	Base      string // branch it should merge into
	Title     string
	Body      string
	Draft     bool
	Reviewers []string
	// TeamReviewers are org teams (GitHub) / approval groups (GitLab).
	TeamReviewers []string
}

// PullRequest is the created (or pre-existing) pull request.
type PullRequest struct {
	Number int
	URL    string
	// Warnings records non-fatal problems, e.g. a reviewer that could not be
	// requested. The pull request exists regardless; losing its URL because a
	// reviewer handle was wrong would be the worse failure.
	Warnings []string
}

// Provider creates pull requests on a forge. Pull-request mode is the same
// mechanism that gates agent action in production (issue #8), so it is built
// once here and reused by every Git-mode adapter.
type Provider interface {
	Name() string
	CreatePullRequest(ctx context.Context, req PullRequestRequest) (PullRequest, error)
}

// ProviderKind selects a forge implementation.
type ProviderKind string

const (
	ProviderGitHub ProviderKind = "github"
	ProviderGitLab ProviderKind = "gitlab"
	ProviderGitea  ProviderKind = "gitea" // also Forgejo
)

// ProviderOptions configures a forge client.
type ProviderOptions struct {
	// BaseURL overrides the API endpoint (GitHub Enterprise, self-hosted
	// GitLab/Gitea). Empty uses the public endpoint.
	BaseURL string
	// Auth supplies the API token.
	Auth Auth
	// HTTP is an optional client (tests inject httptest transports).
	HTTP HTTPDoer
}

// NewProvider returns the forge client for kind. GitHub is fully implemented;
// GitLab and Gitea/Forgejo are wired as providers that fail with a clear,
// structured error until their REST calls land — the interface is the
// extension point, so adding them touches one file each.
func NewProvider(kind ProviderKind, opts ProviderOptions) (Provider, error) {
	switch kind {
	case ProviderGitHub:
		return NewGitHub(opts), nil
	case ProviderGitLab:
		return gitLab{opts: opts}, nil
	case ProviderGitea:
		return gitea{opts: opts}, nil
	default:
		return nil, delivery.Error{
			Code:        delivery.ErrUnsupported,
			Resource:    "git/provider",
			Field:       "kind",
			Message:     fmt.Sprintf("unknown git provider %q", kind),
			Remediation: "use one of: github, gitlab, gitea",
			DocsURL:     "https://kelson.dev/delivery/git#providers",
		}
	}
}

// ProviderKindForHost guesses the forge from a repository URL host. Callers
// may always configure the kind explicitly; this only saves the common case.
func ProviderKindForHost(repo string) (ProviderKind, bool) {
	slug, err := ParseRepo(repo)
	if err != nil {
		return "", false
	}
	host := strings.ToLower(slug.Host)
	switch {
	case strings.Contains(host, "github"):
		return ProviderGitHub, true
	case strings.Contains(host, "gitlab"):
		return ProviderGitLab, true
	case strings.Contains(host, "gitea"), strings.Contains(host, "forgejo"), strings.Contains(host, "codeberg"):
		return ProviderGitea, true
	}
	return "", false
}

// ParseRepo extracts host/owner/name from the remote URL forms git actually
// uses: https://host/owner/repo(.git), ssh://git@host:22/owner/repo(.git) and
// the scp-like git@host:owner/repo(.git).
func ParseRepo(repo string) (Slug, error) {
	raw := strings.TrimSpace(repo)
	if raw == "" {
		return Slug{}, badRepo(repo, "repository URL is empty")
	}

	var host, repoPath string
	switch {
	case strings.Contains(raw, "://"):
		u, err := url.Parse(raw)
		if err != nil {
			return Slug{}, badRepo(repo, "repository URL is not parseable")
		}
		host, repoPath = u.Hostname(), u.Path
	case strings.Contains(raw, ":"):
		// scp-like: [user@]host:owner/repo
		hostPart, rest, _ := strings.Cut(raw, ":")
		if _, h, ok := strings.Cut(hostPart, "@"); ok {
			hostPart = h
		}
		host, repoPath = hostPart, rest
	default:
		return Slug{}, badRepo(repo, "repository URL has no host")
	}

	repoPath = strings.Trim(repoPath, "/")
	repoPath = strings.TrimSuffix(repoPath, ".git")
	owner, name, ok := cutLast(repoPath, "/")
	if !ok || owner == "" || name == "" {
		return Slug{}, badRepo(repo, "repository URL does not contain an owner/name pair")
	}
	return Slug{Host: host, Owner: owner, Name: name}, nil
}

// cutLast splits around the final occurrence of sep, so nested GitLab groups
// (group/subgroup/repo) keep their full owner path.
func cutLast(s, sep string) (before, after string, found bool) {
	i := strings.LastIndex(s, sep)
	if i < 0 {
		return s, "", false
	}
	return s[:i], s[i+len(sep):], true
}

func badRepo(repo, msg string) error {
	e := delivery.ApplyFailed("git/provider", "repo", msg,
		"configure delivery.git.repo as https://host/owner/repo or git@host:owner/repo, "+
			"or set the owner/name overrides explicitly")
	e.Cause = repo
	return e
}
