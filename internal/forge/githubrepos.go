package forge

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
)

// The [RepoBrowser] half: what the New Project repository picker reads
// (ADR-0033 decision 3). Both list calls paginate, because an installation on
// an organisation of any size does not fit in one page and a picker that
// silently shows the first hundred repositories is a picker that hides the one
// the user came for.

// githubRepo is the subset of GitHub's repository object [Repo] is built from.
// Everything else the API returns is deliberately dropped at the seam.
type githubRepo struct {
	FullName      string `json:"full_name"`
	HTMLURL       string `json:"html_url"`
	DefaultBranch string `json:"default_branch"`
	Private       bool   `json:"private"`
}

// repo converts to the seam's shape. The conversion is a cast because the two
// structs are field-identical: githubRepo exists to carry the json tags, and
// keeping it a cast means a field added to one without the other stops
// compiling instead of being silently dropped.
func (r githubRepo) repo() Repo { return Repo(r) }

// ListRepositories implements [RepoBrowser].
//
// The endpoint depends on which credential the connection carries, and so does
// the answer's meaning: an app lists exactly the repositories its installation
// was granted, a token lists everything its owner can see. That difference is
// the reason ADR-0033 decision 2 prefers the app — the picker shows the
// repositories the user chose to expose, not their entire account.
func (g *gitHubProvider) ListRepositories(ctx context.Context, c Conn) ([]Repo, error) {
	base, err := apiBase(c.Host)
	if err != nil {
		return nil, err
	}
	authorization, err := g.authorization(ctx, c)
	if err != nil {
		return nil, err
	}

	var out []Repo
	if c.usesApp() {
		u := base + "/installation/repositories?per_page=" + strconv.Itoa(perPage)
		err = g.paginate(ctx, u, authorization, func(data []byte) error {
			var page struct {
				Repositories []githubRepo `json:"repositories"`
			}
			if err := json.Unmarshal(data, &page); err != nil {
				return fmt.Errorf("forge/github: decoding the installation's repositories: %w", err)
			}
			for _, r := range page.Repositories {
				out = append(out, r.repo())
			}
			return nil
		})
		if err != nil {
			return nil, notInstalled(err)
		}
		return out, nil
	}

	u := base + "/user/repos?per_page=" + strconv.Itoa(perPage)
	err = g.paginate(ctx, u, authorization, func(data []byte) error {
		var page []githubRepo
		if err := json.Unmarshal(data, &page); err != nil {
			return fmt.Errorf("forge/github: decoding the account's repositories: %w", err)
		}
		for _, r := range page {
			out = append(out, r.repo())
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListBranches implements [RepoBrowser]. Names only: a branch picker needs a
// name, and the commit each branch points at is a question the build plane
// asks `git ls-remote` (internal/gitref) at the moment it matters rather than
// a value a picker can cache and be wrong about.
func (g *gitHubProvider) ListBranches(ctx context.Context, c Conn, repoFullName string) ([]string, error) {
	base, err := apiBase(c.Host)
	if err != nil {
		return nil, err
	}
	repo, err := repoPath(base, repoFullName)
	if err != nil {
		return nil, err
	}
	authorization, err := g.authorization(ctx, c)
	if err != nil {
		return nil, err
	}

	var out []string
	err = g.paginate(ctx, repo+"/branches?per_page="+strconv.Itoa(perPage), authorization, func(data []byte) error {
		var page []struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(data, &page); err != nil {
			return fmt.Errorf("forge/github: decoding the branches of %s: %w", repoFullName, err)
		}
		for _, b := range page {
			out = append(out, b.Name)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
