package forge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// The [PRProposer] half: ADR-0033 decision 3's fifth capability, opening a
// pull request against the repository a document lives in (#248).
//
// # Why this is six calls and not one
//
// GitHub's "contents" API can write a single file in one PUT, and it is the
// wrong tool here: it commits per file, so a proposal touching a Project
// document and two Environment documents would land as three commits on the
// branch, each one a repository state that never existed as a proposal. The
// git-objects API assembles the whole change first — a blob per file, one tree,
// one commit, then the ref — so the branch's first and only commit is the
// change, and a failure anywhere before the ref leaves nothing behind but
// unreferenced objects the forge collects itself.
//
// # Read first, write second, and the write is where permission is decided
//
// The three reads (default branch, branch head, base tree) run under the same
// `contents: read` every other capability here needs. The four writes need
// `contents: write`, which ADR-0033 decision 2's app manifest deliberately does
// not request. So the ordinary refusal arrives on the *first blob*, after the
// reads have already succeeded — which is exactly the shape [ErrWriteNotPermitted]
// exists to describe: the connection works, this one operation is not granted.

const (
	// blobModeFile is git's file mode for a regular non-executable file. A tree
	// entry needs one, and kelson proposes YAML documents: nothing here is
	// executable, a symlink or a submodule, so this is the only mode written.
	blobModeFile = "100644"

	// blobTypeFile is the tree entry's object type for a file.
	blobTypeFile = "blob"
)

// OpenPullRequest implements [PRProposer].
//
// The returned URL is the pull request's `html_url` — the page a human opens —
// and not the API URL, because every caller of this shows it to somebody.
func (g *gitHubProvider) OpenPullRequest(ctx context.Context, c Conn, repoFullName string, p Proposal) (string, error) {
	branch := strings.TrimSpace(p.Branch)
	if branch == "" {
		return "", fmt.Errorf("forge/github: a proposal needs the branch name to create")
	}
	if len(p.Files) == 0 {
		return "", fmt.Errorf("forge/github: a proposal with no files would be an empty pull request")
	}
	if strings.TrimSpace(p.Title) == "" {
		return "", fmt.Errorf("forge/github: a proposal needs a title")
	}
	// Before anything is sent: a proposal this adapter cannot honour is refused
	// without spending a request, the same discipline ReportStatus applies to
	// its state vocabulary.
	paths, err := proposedPaths(p.Files)
	if err != nil {
		return "", err
	}
	base, err := apiBase(c.Host)
	if err != nil {
		return "", err
	}
	repo, err := repoPath(base, repoFullName)
	if err != nil {
		return "", err
	}
	authorization, err := g.authorization(ctx, c)
	if err != nil {
		return "", err
	}

	baseBranch := strings.TrimSpace(p.BaseBranch)
	if baseBranch == "" {
		if baseBranch, err = g.defaultBranch(ctx, repo, authorization); err != nil {
			return "", err
		}
	}
	head, err := g.branchHead(ctx, repo, authorization, baseBranch)
	if err != nil {
		return "", err
	}
	baseTree, err := g.commitTree(ctx, repo, authorization, head)
	if err != nil {
		return "", err
	}

	entries, err := g.writeBlobs(ctx, repo, authorization, paths, p.Files)
	if err != nil {
		return "", err
	}
	tree, err := g.writeTree(ctx, repo, authorization, baseTree, entries)
	if err != nil {
		return "", err
	}
	commit, err := g.writeCommit(ctx, repo, authorization, tree, head, p)
	if err != nil {
		return "", err
	}
	if err := g.writeRef(ctx, repo, authorization, branch, commit); err != nil {
		return "", err
	}
	return g.writePullRequest(ctx, repo, authorization, branch, baseBranch, p)
}

// defaultBranch answers the one thing [Proposal] lets the forge decide.
func (g *gitHubProvider) defaultBranch(ctx context.Context, repo, authorization string) (string, error) {
	data, _, err := g.send(ctx, http.MethodGet, repo, authorization, nil)
	if err != nil {
		return "", notInstalled(err)
	}
	var out githubRepo
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("forge/github: decoding the repository: %w", err)
	}
	if out.DefaultBranch == "" {
		return "", fmt.Errorf("forge/github: the repository reports no default branch, so there is nothing to " +
			"propose against; name the base branch explicitly")
	}
	return out.DefaultBranch, nil
}

// branchHead resolves a branch name to the commit it points at.
//
// A 404 here is re-labelled as a missing base branch rather than as a missing
// installation: the repository has already answered at least one call by the
// time this runs, so the thing that is absent is the ref.
func (g *gitHubProvider) branchHead(ctx context.Context, repo, authorization, branch string) (string, error) {
	u := repo + "/git/ref/heads/" + refPath(branch)
	data, _, err := g.send(ctx, http.MethodGet, u, authorization, nil)
	if err != nil {
		if isStatus(err, http.StatusNotFound) {
			return "", fmt.Errorf("forge/github: the repository has no branch %q to propose against: %w", branch, err)
		}
		return "", err
	}
	var out struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("forge/github: decoding the head of %s: %w", branch, err)
	}
	if out.Object.SHA == "" {
		return "", fmt.Errorf("forge/github: %s resolved to no commit", branch)
	}
	return out.Object.SHA, nil
}

// commitTree reads the tree a commit points at, which is what the new tree is
// layered onto. Without it the proposal's tree would hold the changed files and
// nothing else, and merging it would delete the entire repository.
func (g *gitHubProvider) commitTree(ctx context.Context, repo, authorization, commit string) (string, error) {
	u := repo + "/git/commits/" + url.PathEscape(commit)
	data, _, err := g.send(ctx, http.MethodGet, u, authorization, nil)
	if err != nil {
		return "", err
	}
	var out struct {
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("forge/github: decoding commit %s: %w", commit, err)
	}
	if out.Tree.SHA == "" {
		return "", fmt.Errorf("forge/github: commit %s carries no tree", commit)
	}
	return out.Tree.SHA, nil
}

// treeEntry is one file in the tree being written.
type treeEntry struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
}

// proposedPaths validates the proposal's paths and returns them sorted.
//
// Sorted, so a proposal of the same files produces the same sequence of
// requests every time. That is not cosmetic: it is what makes the
// recorded-request assertion in the tests an assertion about this code rather
// than about Go's map iteration, and it is what makes a partial failure land at
// a predictable point.
//
// Validated here rather than by the forge, because the forge's answer to a path
// like `../../elsewhere.yaml` is a 422 that names neither the path nor what is
// wrong with it — and because a path is the one field of a proposal that comes
// from a text box a person typed into.
func proposedPaths(files map[string][]byte) ([]string, error) {
	paths := make([]string, 0, len(files))
	for path := range files {
		clean := strings.TrimSpace(path)
		if clean == "" || strings.HasPrefix(clean, "/") || strings.Contains(clean, "..") {
			return nil, fmt.Errorf("forge/github: %q is not a path inside a repository: a proposal writes "+
				"repository-relative paths, so it cannot start at the root or climb out of one", path)
		}
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths, nil
}

// writeBlobs uploads each file's content and returns the tree entries naming
// them.
//
// Content is sent base64-encoded rather than as a UTF-8 string, because a
// document is bytes here and the seam must not decide that somebody's YAML is
// text in the encoding kelson happens to assume.
func (g *gitHubProvider) writeBlobs(ctx context.Context, repo, authorization string, paths []string, files map[string][]byte) ([]treeEntry, error) {
	entries := make([]treeEntry, 0, len(paths))
	for _, path := range paths {
		body := struct {
			Content  string `json:"content"`
			Encoding string `json:"encoding"`
		}{Content: base64.StdEncoding.EncodeToString(files[path]), Encoding: "base64"}
		data, _, err := g.send(ctx, http.MethodPost, repo+"/git/blobs", authorization, body)
		if err != nil {
			return nil, writeNotPermitted(err)
		}
		var out struct {
			SHA string `json:"sha"`
		}
		if err := json.Unmarshal(data, &out); err != nil {
			return nil, fmt.Errorf("forge/github: decoding the blob written for %s: %w", path, err)
		}
		entries = append(entries, treeEntry{Path: strings.TrimSpace(path), Mode: blobModeFile, Type: blobTypeFile, SHA: out.SHA})
	}
	return entries, nil
}

// writeTree layers the entries onto the base commit's tree.
func (g *gitHubProvider) writeTree(ctx context.Context, repo, authorization, baseTree string, entries []treeEntry) (string, error) {
	body := struct {
		BaseTree string      `json:"base_tree"`
		Tree     []treeEntry `json:"tree"`
	}{BaseTree: baseTree, Tree: entries}
	data, _, err := g.send(ctx, http.MethodPost, repo+"/git/trees", authorization, body)
	if err != nil {
		return "", writeNotPermitted(err)
	}
	var out struct {
		SHA string `json:"sha"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("forge/github: decoding the written tree: %w", err)
	}
	return out.SHA, nil
}

// writeCommit creates the single commit the branch will carry.
func (g *gitHubProvider) writeCommit(ctx context.Context, repo, authorization, tree, parent string, p Proposal) (string, error) {
	message := strings.TrimSpace(p.CommitMessage)
	if message == "" {
		message = p.Title
	}
	body := struct {
		Message string   `json:"message"`
		Tree    string   `json:"tree"`
		Parents []string `json:"parents"`
	}{Message: message, Tree: tree, Parents: []string{parent}}
	data, _, err := g.send(ctx, http.MethodPost, repo+"/git/commits", authorization, body)
	if err != nil {
		return "", writeNotPermitted(err)
	}
	var out struct {
		SHA string `json:"sha"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("forge/github: decoding the written commit: %w", err)
	}
	return out.SHA, nil
}

// writeRef points the new branch at the commit.
//
// A 422 is the branch already existing, and it is re-labelled here because
// GitHub's own message ("Reference already exists") names neither the branch nor
// the repository — and because this is the one failure a caller can act on
// without an administrator: propose again under another name.
func (g *gitHubProvider) writeRef(ctx context.Context, repo, authorization, branch, commit string) error {
	body := struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	}{Ref: "refs/heads/" + branch, SHA: commit}
	if _, _, err := g.send(ctx, http.MethodPost, repo+"/git/refs", authorization, body); err != nil {
		if isStatus(err, http.StatusUnprocessableEntity) {
			return fmt.Errorf("forge/github: branch %q already exists in this repository, and a proposal never "+
				"moves a branch somebody else may be reviewing: %w", branch, err)
		}
		return writeNotPermitted(err)
	}
	return nil
}

// writePullRequest opens the request itself and returns the page to send a
// human to.
//
// `pull_requests: write` is in the manifest's permission set, so this call
// ordinarily succeeds on a connection whose blob write did — the 403 is still
// re-labelled, because a token connection is scoped by whatever the user pasted
// and this is the last place a missing scope can surface.
func (g *gitHubProvider) writePullRequest(ctx context.Context, repo, authorization, branch, baseBranch string, p Proposal) (string, error) {
	body := struct {
		Title string `json:"title"`
		Head  string `json:"head"`
		Base  string `json:"base"`
		Body  string `json:"body,omitempty"`
	}{Title: p.Title, Head: branch, Base: baseBranch, Body: p.Body}
	data, _, err := g.send(ctx, http.MethodPost, repo+"/pulls", authorization, body)
	if err != nil {
		return "", writeNotPermitted(err)
	}
	var out struct {
		HTMLURL string `json:"html_url"`
		Number  int    `json:"number"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("forge/github: decoding the opened pull request: %w", err)
	}
	if out.HTMLURL == "" {
		return "", fmt.Errorf("forge/github: the pull request was opened and the forge reported no URL for it")
	}
	return out.HTMLURL, nil
}

// refPath escapes a branch name for a URL path without escaping its slashes.
//
// `feature/x` is one ref whose name contains a separator, and url.PathEscape
// would turn it into `feature%2Fx` — which GitHub answers 404 for, sending the
// caller to look for a branch that is right there. Escaping segment by segment
// keeps the separator and still escapes everything else.
func refPath(branch string) string {
	parts := strings.Split(strings.Trim(branch, "/"), "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}
