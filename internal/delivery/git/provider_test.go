package git

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestGitHubCreatePullRequest exercises the thin GitHub REST client end to end
// against an httptest server: the create call, the bearer token, the response
// mapping, and reviewer request.
func TestGitHubCreatePullRequest(t *testing.T) {
	var createPath, createMethod, createAuth, createBody string
	var reviewersCalled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		switch {
		case r.URL.Path == "/repos/acme/deploy/pulls" && r.Method == http.MethodPost:
			createPath, createMethod, createAuth, createBody = r.URL.Path, r.Method, r.Header.Get("Authorization"), string(buf)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"number": 42, "html_url": "https://github.com/acme/deploy/pull/42"}`))
		case r.URL.Path == "/repos/acme/deploy/pulls/42/requested_reviewers":
			reviewersCalled = true
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	g := NewGitHub(ProviderOptions{
		BaseURL: srv.URL,
		Auth:    Token{Token: "ghs_test"},
	})
	pr, err := g.CreatePullRequest(context.Background(), PullRequestRequest{
		Slug:      Slug{Owner: "acme", Name: "deploy"},
		Head:      "kelson/shop-production-abc",
		Base:      "main",
		Title:     "Deploy shop",
		Body:      "hash abc",
		Reviewers: []string{"alice"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if pr.Number != 42 || pr.URL != "https://github.com/acme/deploy/pull/42" {
		t.Fatalf("pr = %+v", pr)
	}
	if len(pr.Warnings) != 0 {
		t.Fatalf("warnings = %v", pr.Warnings)
	}
	if !reviewersCalled {
		t.Fatalf("requested_reviewers was not called")
	}
	if createPath != "/repos/acme/deploy/pulls" || createMethod != http.MethodPost {
		t.Fatalf("create call = %s %s", createMethod, createPath)
	}
	if createAuth != "Bearer ghs_test" {
		t.Fatalf("auth header = %q", createAuth)
	}
	var body struct {
		Title string `json:"title"`
		Head  string `json:"head"`
		Base  string `json:"base"`
		Draft bool   `json:"draft"`
	}
	if err := json.Unmarshal([]byte(createBody), &body); err != nil {
		t.Fatal(err)
	}
	if body.Title != "Deploy shop" || body.Head != "kelson/shop-production-abc" || body.Base != "main" || body.Draft {
		t.Fatalf("create body = %+v", body)
	}
}

// TestGitHubCreatePullRequestIdempotent verifies a re-run of the same change
// finds the open PR when GitHub refuses a duplicate (#39 idempotency).
func TestGitHubCreatePullRequestIdempotent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/acme/deploy/pulls" && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"message": "A pull request already exists for acme:kelson/shop-abc."}`))
		case r.URL.Path == "/repos/acme/deploy/pulls" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`[{"number": 42, "html_url": "https://github.com/acme/deploy/pull/42"}]`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	g := NewGitHub(ProviderOptions{BaseURL: srv.URL, Auth: Token{Token: "t"}})
	pr, err := g.CreatePullRequest(context.Background(), PullRequestRequest{
		Slug: Slug{Owner: "acme", Name: "deploy"},
		Head: "kelson/shop-abc", Base: "main", Title: "t", Body: "b",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if pr.Number != 42 {
		t.Fatalf("pr = %+v, want the existing 42", pr)
	}
}

// TestGitHubCreatePullRequestNeedsAPIToken verifies SSH-only auth is refused
// clearly rather than producing a confusing 401.
func TestGitHubCreatePullRequestNeedsAPIToken(t *testing.T) {
	g := NewGitHub(ProviderOptions{BaseURL: "http://example.test", Auth: SSHKey{KeyPath: "/dev/null"}})
	_, err := g.CreatePullRequest(context.Background(), PullRequestRequest{
		Slug: Slug{Owner: "acme", Name: "deploy"},
	})
	if err == nil || !strings.Contains(err.Error(), "cannot provide") {
		t.Fatalf("error = %v, want clear API-credential refusal", err)
	}
}

// TestParseRepo covers the URL forms git actually uses.
func TestParseRepo(t *testing.T) {
	for _, tc := range []struct {
		in                string
		host, owner, name string
	}{
		{"https://github.com/acme/deploy.git", "github.com", "acme", "deploy"},
		{"https://gitlab.com/group/sub/deploy", "gitlab.com", "group/sub", "deploy"},
		{"git@github.com:acme/deploy.git", "github.com", "acme", "deploy"},
		{"ssh://git@github.com:2222/acme/deploy.git", "github.com", "acme", "deploy"},
	} {
		s, err := ParseRepo(tc.in)
		if err != nil {
			t.Fatalf("ParseRepo(%q): %v", tc.in, err)
		}
		if s.Host != tc.host || s.Owner != tc.owner || s.Name != tc.name {
			t.Fatalf("ParseRepo(%q) = %+v, want %s/%s on %s", tc.in, s, tc.owner, tc.name, tc.host)
		}
	}
	if _, err := ParseRepo("not a url"); err == nil {
		t.Fatalf("ParseRepo of a hostless string must fail")
	}
}

// TestProviderKindForHost guesses the forge from the host.
func TestProviderKindForHost(t *testing.T) {
	for _, tc := range []struct {
		repo string
		want ProviderKind
	}{
		{"https://github.com/acme/deploy.git", ProviderGitHub},
		{"https://gitlab.com/acme/deploy.git", ProviderGitLab},
		{"https://gitea.example.com/acme/deploy.git", ProviderGitea},
		{"https://codeberg.org/acme/deploy.git", ProviderGitea},
	} {
		kind, ok := ProviderKindForHost(tc.repo)
		if !ok || kind != tc.want {
			t.Fatalf("ProviderKindForHost(%q) = %q, %v; want %q", tc.repo, kind, ok, tc.want)
		}
	}
}
