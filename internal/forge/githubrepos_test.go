package forge

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// pagedServer serves one path as a sequence of pages, linking each to the next
// the way GitHub does. Two pages where the first is exactly full is the case a
// page counter gets wrong, which is why the fixtures below are shaped that way
// rather than as one short page.
func pagedServer(t *testing.T, path string, pages []string) (*httptest.Server, *counter) {
	t.Helper()
	var requests counter
	mux := http.NewServeMux()
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		requireHeaders(t, r)
		n := requests.get()
		requests.inc()
		if n >= len(pages) {
			t.Errorf("the client asked for page %d of %d — it followed a link that was not offered", n+1, len(pages))
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if n < len(pages)-1 {
			next := fmt.Sprintf("<http://%s%s?page=%d>; rel=\"next\", <http://%s%s?page=%d>; rel=\"last\"",
				r.Host, r.URL.Path, n+2, r.Host, r.URL.Path, len(pages))
			w.Header().Set("Link", next)
		}
		_, _ = w.Write([]byte(pages[n]))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, &requests
}

// An app connection lists the installation's repositories — exactly the ones
// the user chose to expose — and a token connection lists the account's. The
// two endpoints disagree about their envelope, which is the reason the
// pagination loop hands raw bytes to a per-endpoint decoder.
func TestListRepositories(t *testing.T) {
	t.Run("app auth reads the installation, paginated", func(t *testing.T) {
		var mints counter
		mux := http.NewServeMux()
		server := httptest.NewUnstartedServer(mux)

		mux.HandleFunc("/api/v3/app/installations/42/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
			mints.inc()
			_, _ = fmt.Fprintf(w, `{"token":"ghs_installation-token","expires_at":%q}`,
				time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
		})
		var pages counter
		mux.HandleFunc("/api/v3/installation/repositories", func(w http.ResponseWriter, r *http.Request) {
			requireHeaders(t, r)
			if got := r.Header.Get("Authorization"); got != "Bearer ghs_installation-token" {
				t.Errorf("Authorization = %q, want the minted installation token", got)
			}
			if got := r.URL.Query().Get("per_page"); got != "100" {
				t.Errorf("per_page = %q, want the maximum page size on every page", got)
			}
			n := pages.get()
			pages.inc()
			if n == 0 {
				w.Header().Set("Link", fmt.Sprintf(`<http://%s/api/v3/installation/repositories?per_page=100&page=2>; rel="next"`, r.Host))
				_, _ = w.Write([]byte(`{"total_count":3,"repositories":[
					{"full_name":"acme/checkout","html_url":"https://ghe.test/acme/checkout","default_branch":"main","private":true},
					{"full_name":"acme/web","html_url":"https://ghe.test/acme/web","default_branch":"trunk"}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"total_count":3,"repositories":[
				{"full_name":"acme/docs","html_url":"https://ghe.test/acme/docs","default_branch":"main"}]}`))
		})
		server.Start()
		defer server.Close()

		got, err := newGitHub().ListRepositories(t.Context(), appConn(t, server.URL))
		if err != nil {
			t.Fatalf("ListRepositories: %v", err)
		}
		want := []Repo{
			{FullName: "acme/checkout", HTMLURL: "https://ghe.test/acme/checkout", DefaultBranch: "main", Private: true},
			{FullName: "acme/web", HTMLURL: "https://ghe.test/acme/web", DefaultBranch: "trunk"},
			{FullName: "acme/docs", HTMLURL: "https://ghe.test/acme/docs", DefaultBranch: "main"},
		}
		if !slices.Equal(got, want) {
			t.Errorf("ListRepositories() =\n\t%+v\nwant\n\t%+v", got, want)
		}
		if pages.get() != 2 {
			t.Errorf("read %d pages, want both", pages.get())
		}
		if mints.get() != 1 {
			t.Errorf("minted %d tokens for one listing, want 1 — the cache is not being used across pages", mints.get())
		}
	})

	t.Run("token auth reads the account, paginated", func(t *testing.T) {
		server, requests := pagedServer(t, "/api/v3/user/repos", []string{
			`[{"full_name":"acme/checkout","default_branch":"main","private":true}]`,
			`[{"full_name":"personal/dotfiles","default_branch":"master"}]`,
		})

		var seen string
		client := server.Client()
		client.Transport = headerSpy{next: client.Transport, seen: &seen}
		g := newGitHub()
		g.client = client

		got, err := g.ListRepositories(t.Context(), tokenConn(server.URL))
		if err != nil {
			t.Fatalf("ListRepositories: %v", err)
		}
		if len(got) != 2 || got[0].FullName != "acme/checkout" || got[1].FullName != "personal/dotfiles" {
			t.Errorf("ListRepositories() = %+v, want both pages in order", got)
		}
		if requests.get() != 2 {
			t.Errorf("read %d pages, want both", requests.get())
		}
		if seen != "Bearer a-personal-access-token" {
			t.Errorf("Authorization = %q, want the stored token", seen)
		}
	})

	t.Run("an installation that no longer exists", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/api/v3/app/installations/42/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprintf(w, `{"token":"ghs_t","expires_at":%q}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
		})
		mux.HandleFunc("/api/v3/installation/repositories", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
		})
		server := httptest.NewServer(mux)
		defer server.Close()

		_, err := newGitHub().ListRepositories(t.Context(), appConn(t, server.URL))
		if !errors.Is(err, ErrNotInstalled) {
			t.Fatalf("error = %v, want ErrNotInstalled — the remedy is adding the repository, not rotating the key", err)
		}
	})

	t.Run("a connection with no credential does not reach the network", func(t *testing.T) {
		server, requests := pagedServer(t, "/api/v3/user/repos", []string{`[]`})
		_, err := newGitHub().ListRepositories(t.Context(), Conn{Provider: nameGitHub, Host: server.URL})
		if !errors.Is(err, ErrAuthFailed) {
			t.Fatalf("error = %v, want ErrAuthFailed", err)
		}
		if requests.get() != 0 {
			t.Errorf("made %d requests with no credential, want none", requests.get())
		}
	})
}

// headerSpy records the Authorization header of the last request, which is how
// the token path asserts what it sent without the handler having to.
type headerSpy struct {
	next http.RoundTripper
	seen *string
}

func (s headerSpy) RoundTrip(r *http.Request) (*http.Response, error) {
	*s.seen = r.Header.Get("Authorization")
	next := s.next
	if next == nil {
		next = http.DefaultTransport
	}
	return next.RoundTrip(r)
}

func TestListBranches(t *testing.T) {
	t.Run("names only, paginated", func(t *testing.T) {
		server, requests := pagedServer(t, "/api/v3/repos/acme/checkout/branches", []string{
			`[{"name":"main","commit":{"sha":"aaa"}},{"name":"develop","commit":{"sha":"bbb"}}]`,
			`[{"name":"release/1.x","commit":{"sha":"ccc"}}]`,
		})

		got, err := newGitHub().ListBranches(t.Context(), tokenConn(server.URL), "acme/checkout")
		if err != nil {
			t.Fatalf("ListBranches: %v", err)
		}
		want := []string{"main", "develop", "release/1.x"}
		if !slices.Equal(got, want) {
			t.Errorf("ListBranches() = %v, want %v", got, want)
		}
		if requests.get() != 2 {
			t.Errorf("read %d pages, want both", requests.get())
		}
	})

	t.Run("a name that is not owner/repo never becomes a path", func(t *testing.T) {
		server, requests := pagedServer(t, "/api/v3/repos/", []string{`[]`})
		for _, full := range []string{"checkout", "acme/checkout/tree", "../../etc", ""} {
			if _, err := newGitHub().ListBranches(t.Context(), tokenConn(server.URL), full); err == nil {
				t.Errorf("ListBranches(%q) = nil error, want a refusal before the request", full)
			}
		}
		if requests.get() != 0 {
			t.Errorf("made %d requests for invalid names, want none", requests.get())
		}
	})

	t.Run("a name with a slash-bearing owner is escaped, not interpolated", func(t *testing.T) {
		var path string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			path = r.URL.EscapedPath()
			_, _ = w.Write([]byte(`[]`))
		}))
		defer server.Close()

		if _, err := newGitHub().ListBranches(t.Context(), tokenConn(server.URL), "ac me/check out"); err != nil {
			t.Fatalf("ListBranches: %v", err)
		}
		if strings.Contains(path, " ") {
			t.Errorf("path = %q, want the segments escaped", path)
		}
	})
}
