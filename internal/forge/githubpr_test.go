package forge

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// prServer records the sequence of calls a proposal makes and answers each one
// the way GitHub does.
//
// The recorded sequence is the assertion this file is really about: the git
// objects have to be written in dependency order (blob before tree, tree before
// commit, commit before ref, ref before pull request), and every other check
// here is about one call's body. A handler that answered out of order would
// still satisfy the per-call assertions.
type prServer struct {
	mu sync.Mutex
	// calls is "METHOD path" per request, in order.
	calls []string
	// bodies is the decoded JSON body of each write, keyed by the path.
	bodies map[string]map[string]any

	// failAt, when non-empty, is the path suffix that answers with failStatus.
	failAt     string
	failStatus int
	failBody   string
}

func newPRServer() *prServer {
	return &prServer{bodies: map[string]map[string]any{}}
}

func (p *prServer) record(r *http.Request) map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, r.Method+" "+r.URL.Path)
	if r.Body == nil {
		return nil
	}
	raw, _ := io.ReadAll(r.Body)
	if len(raw) == 0 {
		return nil
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil
	}
	p.bodies[r.URL.Path] = body
	return body
}

// forget drops the last recorded call, for the requests every capability makes
// that this file is not about.
func (p *prServer) forget(r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	want := r.Method + " " + r.URL.Path
	if n := len(p.calls); n > 0 && p.calls[n-1] == want {
		p.calls = p.calls[:n-1]
	}
}

func (p *prServer) sequence() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.calls...)
}

func (p *prServer) body(t *testing.T, path string) map[string]any {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	body, ok := p.bodies[path]
	if !ok {
		t.Fatalf("no request was recorded for %s (recorded: %v)", path, p.calls)
	}
	return body
}

func (p *prServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requireHeaders(t, r)
		body := p.record(r)

		if p.failAt != "" && strings.HasSuffix(r.URL.Path, p.failAt) {
			w.WriteHeader(p.failStatus)
			_, _ = io.WriteString(w, p.failBody)
			return
		}

		switch {
		// The app connection mints an installation token first. It is not part
		// of the proposal's sequence assertion — every capability does it — so
		// it is answered and dropped from the record.
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/access_tokens"):
			p.forget(r)
			writeJSON(w, map[string]any{"token": "an-installation-token", "expires_at": "2099-01-01T00:00:00Z"})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/repos/acme/gitops"):
			writeJSON(w, map[string]any{"default_branch": "main", "full_name": "acme/gitops"})
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/git/ref/heads/"):
			writeJSON(w, map[string]any{"object": map[string]any{"sha": "base-commit-sha"}})
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/git/commits/"):
			writeJSON(w, map[string]any{"tree": map[string]any{"sha": "base-tree-sha"}})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git/blobs"):
			content, _ := body["content"].(string)
			writeJSON(w, map[string]any{"sha": "blob-" + shortHash(content)})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git/trees"):
			writeJSON(w, map[string]any{"sha": "new-tree-sha"})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git/commits"):
			writeJSON(w, map[string]any{"sha": "new-commit-sha"})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git/refs"):
			writeJSON(w, map[string]any{"ref": "refs/heads/proposed"})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls"):
			writeJSON(w, map[string]any{"html_url": "https://github.test/acme/gitops/pull/7", "number": 7})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(v)
}

// shortHash makes each blob's answer depend on its content, so the tree entries
// the test asserts on could not have come from a single canned sha.
func shortHash(content string) string {
	if len(content) > 8 {
		return content[:8]
	}
	return content
}

func proposal() Proposal {
	return Proposal{
		Branch:        "kelson/propose-checkout",
		CommitMessage: "chore(kelson): update checkout",
		Files: map[string][]byte{
			"clusters/prod/checkout-production.yaml": []byte("kind: Environment\n"),
			"clusters/prod/checkout.yaml":            []byte("kind: Project\n"),
		},
		Title: "Update checkout",
		Body:  "Proposed by kelson.",
	}
}

func TestOpenPullRequestWritesTheObjectsInOrder(t *testing.T) {
	server := newPRServer()
	ts := server.start(t)

	url, err := newGitHub().OpenPullRequest(t.Context(), tokenConn(ts.URL), "acme/gitops", proposal())
	if err != nil {
		t.Fatalf("OpenPullRequest: %v", err)
	}
	if want := "https://github.test/acme/gitops/pull/7"; url != want {
		t.Errorf("url = %q, want %q — the caller shows this to a human, so it is html_url and not the API URL", url, want)
	}

	want := []string{
		// This proposal names no base branch, so the repository is read for its
		// default. TestOpenPullRequestSendsTheChangeItWasGiven covers the case
		// where the caller named one and this call must not happen.
		"GET /api/v3/repos/acme/gitops",
		"GET /api/v3/repos/acme/gitops/git/ref/heads/main",
		"GET /api/v3/repos/acme/gitops/git/commits/base-commit-sha",
		"POST /api/v3/repos/acme/gitops/git/blobs",
		"POST /api/v3/repos/acme/gitops/git/blobs",
		"POST /api/v3/repos/acme/gitops/git/trees",
		"POST /api/v3/repos/acme/gitops/git/commits",
		"POST /api/v3/repos/acme/gitops/git/refs",
		"POST /api/v3/repos/acme/gitops/pulls",
	}
	got := server.sequence()
	if len(got) != len(want) {
		t.Fatalf("call sequence = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("call %d = %q, want %q (whole sequence: %v)", i, got[i], want[i], got)
		}
	}
}

func TestOpenPullRequestSendsTheChangeItWasGiven(t *testing.T) {
	server := newPRServer()
	ts := server.start(t)

	p := proposal()
	p.BaseBranch = "release/1.x"
	if _, err := newGitHub().OpenPullRequest(t.Context(), tokenConn(ts.URL), "acme/gitops", p); err != nil {
		t.Fatalf("OpenPullRequest: %v", err)
	}

	// A named base branch is used verbatim and the repository is never asked
	// for its default — the one derivation this seam makes is the one it must.
	for _, call := range server.sequence() {
		if call == "GET /api/v3/repos/acme/gitops" {
			t.Error("the repository was read for its default branch although the proposal named a base branch")
		}
	}
	if !strings.Contains(strings.Join(server.sequence(), " "), "/git/ref/heads/release/1.x") {
		t.Errorf("the branch head was not read for release/1.x: %v — a slash in a ref name must survive the URL path",
			server.sequence())
	}

	tree := server.body(t, "/api/v3/repos/acme/gitops/git/trees")
	if tree["base_tree"] != "base-tree-sha" {
		t.Errorf("base_tree = %v, want base-tree-sha — a tree without one deletes every file it does not name", tree["base_tree"])
	}
	entries, _ := tree["tree"].([]any)
	if len(entries) != 2 {
		t.Fatalf("tree entries = %v, want two", entries)
	}
	first, _ := entries[0].(map[string]any)
	if first["path"] != "clusters/prod/checkout-production.yaml" {
		t.Errorf("the first tree entry is %v; paths are walked in sorted order so the request sequence is stable", first["path"])
	}
	if first["mode"] != blobModeFile || first["type"] != blobTypeFile {
		t.Errorf("tree entry = %v, want mode %s and type %s", first, blobModeFile, blobTypeFile)
	}

	blob := server.body(t, "/api/v3/repos/acme/gitops/git/blobs")
	if blob["encoding"] != "base64" {
		t.Errorf("blob encoding = %v, want base64 — a document is bytes at this seam", blob["encoding"])
	}
	content, _ := blob["content"].(string)
	decoded, err := base64.StdEncoding.DecodeString(content)
	if err != nil {
		t.Fatalf("the blob content is not base64: %v", err)
	}
	if string(decoded) != "kind: Project\n" {
		// The last blob written wins the recorded body, and sorted order makes
		// that the Project document.
		t.Errorf("blob content = %q, want the Project document", decoded)
	}

	commit := server.body(t, "/api/v3/repos/acme/gitops/git/commits")
	if commit["message"] != "chore(kelson): update checkout" {
		t.Errorf("commit message = %v, want the caller's", commit["message"])
	}
	if commit["tree"] != "new-tree-sha" {
		t.Errorf("commit tree = %v, want the tree just written", commit["tree"])
	}
	parents, _ := commit["parents"].([]any)
	if len(parents) != 1 || parents[0] != "base-commit-sha" {
		t.Errorf("commit parents = %v, want the base branch's head alone", parents)
	}

	ref := server.body(t, "/api/v3/repos/acme/gitops/git/refs")
	if ref["ref"] != "refs/heads/kelson/propose-checkout" || ref["sha"] != "new-commit-sha" {
		t.Errorf("ref = %v, want the proposal's branch at the new commit", ref)
	}

	pull := server.body(t, "/api/v3/repos/acme/gitops/pulls")
	if pull["head"] != "kelson/propose-checkout" || pull["base"] != "release/1.x" {
		t.Errorf("pull request = %v, want head at the proposed branch and base at release/1.x", pull)
	}
	if pull["title"] != "Update checkout" || pull["body"] != "Proposed by kelson." {
		t.Errorf("pull request = %v, want the caller's title and body", pull)
	}
}

// TestOpenPullRequestRefusedForPermission is the failure ADR-0033 decision 2's
// permission set makes the *expected* one: the app manifest asks for
// `contents: read`, every read here succeeds, and the first write is refused.
func TestOpenPullRequestRefusedForPermission(t *testing.T) {
	server := newPRServer()
	server.failAt = "/git/blobs"
	server.failStatus = http.StatusForbidden
	server.failBody = `{"message":"Resource not accessible by integration"}`
	ts := server.start(t)

	_, err := newGitHub().OpenPullRequest(t.Context(), appConn(t, ts.URL), "acme/gitops", proposal())
	if err == nil {
		t.Fatal("OpenPullRequest succeeded although the forge refused the write")
	}
	if !errors.Is(err, ErrWriteNotPermitted) {
		t.Errorf("error = %v, want ErrWriteNotPermitted — a 403 on a write is a permission to grant, not a "+
			"credential to rotate", err)
	}
	if !errors.Is(err, ErrAuthFailed) {
		t.Errorf("error = %v, should still match ErrAuthFailed so a caller that knows only the original four "+
			"sentinels fails closed", err)
	}
	if !strings.Contains(err.Error(), "Resource not accessible by integration") {
		t.Errorf("error = %v, want the forge's own message quoted back", err)
	}

	// Nothing was written after the refusal: the tree, the commit, the ref and
	// the pull request are all downstream of a blob that does not exist.
	for _, call := range server.sequence() {
		if !strings.HasPrefix(call, "POST ") {
			continue
		}
		for _, downstream := range []string{"/git/trees", "/git/commits", "/git/refs", "/pulls"} {
			if strings.HasSuffix(call, downstream) {
				t.Errorf("%q ran after the write was refused", call)
			}
		}
	}
}

func TestOpenPullRequestExistingBranch(t *testing.T) {
	server := newPRServer()
	server.failAt = "/git/refs"
	server.failStatus = http.StatusUnprocessableEntity
	server.failBody = `{"message":"Reference already exists"}`
	ts := server.start(t)

	_, err := newGitHub().OpenPullRequest(t.Context(), tokenConn(ts.URL), "acme/gitops", proposal())
	if err == nil {
		t.Fatal("OpenPullRequest succeeded although the branch already exists")
	}
	if errors.Is(err, ErrWriteNotPermitted) {
		t.Errorf("error = %v, want a branch-collision error and not a permission one", err)
	}
	if !strings.Contains(err.Error(), "kelson/propose-checkout") {
		t.Errorf("error = %v, want the branch named: GitHub's own message names neither branch nor repository", err)
	}
}

func TestOpenPullRequestMissingBaseBranch(t *testing.T) {
	server := newPRServer()
	server.failAt = "/git/ref/heads/nope"
	server.failStatus = http.StatusNotFound
	server.failBody = `{"message":"Not Found"}`
	ts := server.start(t)

	p := proposal()
	p.BaseBranch = "nope"
	_, err := newGitHub().OpenPullRequest(t.Context(), tokenConn(ts.URL), "acme/gitops", p)
	if err == nil {
		t.Fatal("OpenPullRequest succeeded against a base branch that does not exist")
	}
	if errors.Is(err, ErrNotInstalled) {
		t.Errorf("error = %v: a 404 here is a missing branch, and calling it a missing installation sends the "+
			"user to the wrong screen", err)
	}
	if !strings.Contains(err.Error(), `"nope"`) {
		t.Errorf("error = %v, want the branch named", err)
	}
}

func TestOpenPullRequestRefusesBeforeAnyRequest(t *testing.T) {
	var requests counter
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.inc()
		w.WriteHeader(http.StatusCreated)
	}))
	defer ts.Close()

	tests := []struct {
		name string
		p    func(Proposal) Proposal
		want string
	}{
		{
			name: "no branch",
			p:    func(p Proposal) Proposal { p.Branch = " "; return p },
			want: "branch name",
		},
		{
			name: "no files",
			p:    func(p Proposal) Proposal { p.Files = nil; return p },
			want: "empty pull request",
		},
		{
			name: "no title",
			p:    func(p Proposal) Proposal { p.Title = ""; return p },
			want: "title",
		},
		{
			name: "an absolute path",
			p: func(p Proposal) Proposal {
				p.Files = map[string][]byte{"/etc/passwd": []byte("x")}
				return p
			},
			want: "inside a repository",
		},
		{
			name: "a path climbing out",
			p: func(p Proposal) Proposal {
				p.Files = map[string][]byte{"../../elsewhere.yaml": []byte("x")}
				return p
			},
			want: "inside a repository",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := newGitHub().OpenPullRequest(t.Context(), tokenConn(ts.URL), "acme/gitops", tc.p(proposal()))
			if err == nil {
				t.Fatal("OpenPullRequest accepted a proposal it cannot honour")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
	if requests.get() != 0 {
		t.Errorf("%d requests were made for proposals refused on their own contents; a proposal this adapter "+
			"cannot honour costs no call at all", requests.get())
	}
}

// TestGenericHasNoPRProposer is the other end of the capability range: the
// generic adapter offers exactly one thing, and a caller type-asserting for a
// proposer must be told no rather than handed something that fails at the
// forge.
func TestGenericHasNoPRProposer(t *testing.T) {
	p, ok := For("generic")
	if !ok {
		t.Fatal("For(generic) found no adapter")
	}
	if _, ok := p.(PRProposer); ok {
		t.Error("the generic adapter claims PRProposer; a bare git host has no pull requests to open")
	}
	github, _ := For("github")
	if _, ok := github.(PRProposer); !ok {
		t.Error("the github adapter does not claim PRProposer")
	}
}

func TestRefPath(t *testing.T) {
	tests := map[string]string{
		"main":            "main",
		"feature/x":       "feature/x",
		"release/1.x":     "release/1.x",
		"with space":      "with%20space",
		"/leading/slash/": "leading/slash",
	}
	for in, want := range tests {
		if got := refPath(in); got != want {
			t.Errorf("refPath(%q) = %q, want %q", in, got, want)
		}
	}
}
