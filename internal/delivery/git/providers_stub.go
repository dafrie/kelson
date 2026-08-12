package git

import (
	"context"

	"github.com/dafrie/kelson/internal/delivery"
)

// gitLab and gitea are registered providers whose pull-request calls are not
// implemented yet (issue #39 delivers GitHub end to end). They exist as types
// rather than as a missing switch case so that:
//
//   - configuring delivery for a GitLab or Gitea repo fails at construction
//     with a structured, actionable error instead of a nil-provider panic at
//     commit time; and
//   - adding the implementation is a change to one method, with no change to
//     the writer, the adapters, or the configuration surface.
//
// Everything else — the git mechanics, branch naming, attribution, conflict
// detection — is already forge-independent and works against GitLab and Gitea
// today in commit mode.
type gitLab struct{ opts ProviderOptions }

func (gitLab) Name() string { return string(ProviderGitLab) }

func (g gitLab) CreatePullRequest(context.Context, PullRequestRequest) (PullRequest, error) {
	return PullRequest{}, notImplementedProvider(ProviderGitLab,
		"POST /projects/{id}/merge_requests with reviewer_ids resolved from usernames")
}

type gitea struct{ opts ProviderOptions }

func (gitea) Name() string { return string(ProviderGitea) }

func (g gitea) CreatePullRequest(context.Context, PullRequestRequest) (PullRequest, error) {
	return PullRequest{}, notImplementedProvider(ProviderGitea,
		"POST /repos/{owner}/{repo}/pulls (Gitea and Forgejo share this API)")
}

func notImplementedProvider(kind ProviderKind, todo string) error {
	return delivery.Error{
		Code:     delivery.ErrUnsupported,
		Resource: "git/pull-request",
		Field:    string(kind),
		Message:  string(kind) + " pull-request creation is not implemented yet",
		Remediation: "use commit mode for this repository (fully supported on " + string(kind) +
			"), or deliver through a GitHub repository for pull-request mode",
		DocsURL: "https://kelson.dev/delivery/git#providers",
		Cause:   "TODO(#39): " + todo,
	}
}

var (
	_ Provider = gitLab{}
	_ Provider = gitea{}
)
