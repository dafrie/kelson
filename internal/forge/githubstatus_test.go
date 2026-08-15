package forge

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReportStatus(t *testing.T) {
	t.Run("posts the status to the commit", func(t *testing.T) {
		var body map[string]string
		var path string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requireHeaders(t, r)
			if r.Method != http.MethodPost {
				t.Errorf("method = %q, want POST", r.Method)
			}
			path = r.URL.Path
			raw, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("the request body is not JSON: %v", err)
			}
			w.WriteHeader(http.StatusCreated)
		}))
		defer server.Close()

		s := Status{
			State:       "success",
			Context:     "kelson/preview",
			Description: "preview is live",
			TargetURL:   "https://kelson.test/previews/412",
		}
		if err := newGitHub().ReportStatus(t.Context(), tokenConn(server.URL), "acme/checkout", "ccc222", s); err != nil {
			t.Fatalf("ReportStatus: %v", err)
		}
		if want := "/api/v3/repos/acme/checkout/statuses/ccc222"; path != want {
			t.Errorf("path = %q, want %q", path, want)
		}
		want := map[string]string{
			"state":       "success",
			"context":     "kelson/preview",
			"description": "preview is live",
			"target_url":  "https://kelson.test/previews/412",
		}
		for k, v := range want {
			if body[k] != v {
				t.Errorf("body[%q] = %q, want %q", k, body[k], v)
			}
		}
	})

	t.Run("an unset context gets the stable default", func(t *testing.T) {
		var body map[string]string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			w.WriteHeader(http.StatusCreated)
		}))
		defer server.Close()

		if err := newGitHub().ReportStatus(t.Context(), tokenConn(server.URL), "acme/checkout", "ccc", Status{State: "pending"}); err != nil {
			t.Fatalf("ReportStatus: %v", err)
		}
		if body["context"] != defaultStatusContext {
			t.Errorf("context = %q, want %q — a changing context leaves stale checks on every commit", body["context"], defaultStatusContext)
		}
	})

	t.Run("refused before the request", func(t *testing.T) {
		var requests counter
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			requests.inc()
			w.WriteHeader(http.StatusCreated)
		}))
		defer server.Close()

		tests := []struct {
			name string
			sha  string
			full string
			s    Status
		}{
			{name: "a state GitHub does not accept", sha: "ccc", full: "acme/checkout", s: Status{State: "green"}},
			{name: "no state at all", sha: "ccc", full: "acme/checkout"},
			{name: "no commit", full: "acme/checkout", s: Status{State: "success"}},
			{name: "not a repository name", sha: "ccc", full: "checkout", s: Status{State: "success"}},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				if err := newGitHub().ReportStatus(t.Context(), tokenConn(server.URL), tt.full, tt.sha, tt.s); err == nil {
					t.Error("want an error")
				}
			})
		}
		if requests.get() != 0 {
			t.Errorf("made %d requests for invalid input, want none", requests.get())
		}
	})

	t.Run("a repository the installation cannot see", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
		}))
		defer server.Close()

		err := newGitHub().ReportStatus(t.Context(), tokenConn(server.URL), "acme/checkout", "ccc", Status{State: "success"})
		if !errors.Is(err, ErrNotInstalled) {
			t.Fatalf("error = %v, want ErrNotInstalled", err)
		}
	})
}

// The upsert is the difference between one comment per preview and one per
// push, so both halves are exercised: the create path when the marker is
// nowhere in the thread, and the edit path when it is.
func TestUpsertPRComment(t *testing.T) {
	const marker = "<!-- kelson:preview:checkout -->"

	t.Run("creates when no comment carries the marker", func(t *testing.T) {
		var created, patched counter
		var createdBody struct {
			Body string `json:"body"`
		}
		mux := http.NewServeMux()
		mux.HandleFunc("/api/v3/repos/acme/checkout/issues/412/comments", func(w http.ResponseWriter, r *http.Request) {
			requireHeaders(t, r)
			if r.Method == http.MethodGet {
				_, _ = w.Write([]byte(`[{"id":1,"body":"looks good to me"},{"id":2,"body":"<!-- someone-else -->"}]`))
				return
			}
			created.inc()
			raw, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(raw, &createdBody); err != nil {
				t.Errorf("the created comment is not JSON: %v", err)
			}
			w.WriteHeader(http.StatusCreated)
		})
		mux.HandleFunc("/api/v3/repos/acme/checkout/issues/comments/", func(w http.ResponseWriter, _ *http.Request) {
			patched.inc()
			w.WriteHeader(http.StatusOK)
		})
		server := httptest.NewServer(mux)
		defer server.Close()

		err := newGitHub().UpsertPRComment(t.Context(), tokenConn(server.URL), "acme/checkout", 412, marker, "preview is live")
		if err != nil {
			t.Fatalf("UpsertPRComment: %v", err)
		}
		if created.get() != 1 || patched.get() != 0 {
			t.Fatalf("created %d / patched %d, want 1 / 0", created.get(), patched.get())
		}
		if !strings.Contains(createdBody.Body, marker) {
			t.Errorf("the created body %q does not carry the marker; the next publish could never find it", createdBody.Body)
		}
		if !strings.Contains(createdBody.Body, "preview is live") {
			t.Errorf("the created body %q lost the caller's text", createdBody.Body)
		}
	})

	t.Run("edits the comment carrying the marker, and stops looking once found", func(t *testing.T) {
		var listed, created, patched counter
		var patchedID string
		var patchedBody struct {
			Body string `json:"body"`
		}
		mux := http.NewServeMux()
		mux.HandleFunc("/api/v3/repos/acme/checkout/issues/412/comments", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				created.inc()
				w.WriteHeader(http.StatusCreated)
				return
			}
			n := listed.get()
			listed.inc()
			// Three pages, the marker on the second: the walk must stop there
			// rather than read a busy thread to the end on every publish.
			w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?per_page=100&page=%d>; rel="next"`, r.Host, r.URL.Path, n+2))
			switch n {
			case 0:
				_, _ = w.Write([]byte(`[{"id":1,"body":"looks good"}]`))
			case 1:
				_, _ = fmt.Fprintf(w, `[{"id":77,"body":%q}]`, marker+"\npreview was live")
			default:
				t.Errorf("the walk read page %d after the marker was found on page 2", n+1)
				_, _ = w.Write([]byte(`[]`))
			}
		})
		mux.HandleFunc("/api/v3/repos/acme/checkout/issues/comments/", func(w http.ResponseWriter, r *http.Request) {
			requireHeaders(t, r)
			if r.Method != http.MethodPatch {
				t.Errorf("method = %q, want PATCH", r.Method)
			}
			patched.inc()
			patchedID = strings.TrimPrefix(r.URL.Path, "/api/v3/repos/acme/checkout/issues/comments/")
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &patchedBody)
			w.WriteHeader(http.StatusOK)
		})
		server := httptest.NewServer(mux)
		defer server.Close()

		body := marker + "\npreview is live at https://checkout-412.kelson.test"
		if err := newGitHub().UpsertPRComment(t.Context(), tokenConn(server.URL), "acme/checkout", 412, marker, body); err != nil {
			t.Fatalf("UpsertPRComment: %v", err)
		}
		if patched.get() != 1 || created.get() != 0 {
			t.Fatalf("patched %d / created %d, want 1 / 0 — a second comment per push is the failure this upsert exists to avoid", patched.get(), created.get())
		}
		if patchedID != "77" {
			t.Errorf("patched comment %q, want 77", patchedID)
		}
		if listed.get() != 2 {
			t.Errorf("read %d pages, want 2 — the search kept going after it had its answer", listed.get())
		}
		if patchedBody.Body != body {
			t.Errorf("body = %q, want the caller's body unchanged (it already carries the marker)", patchedBody.Body)
		}
	})

	t.Run("refused before the request", func(t *testing.T) {
		var requests counter
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			requests.inc()
			_, _ = w.Write([]byte(`[]`))
		}))
		defer server.Close()

		tests := []struct {
			name   string
			marker string
			pr     int
			full   string
		}{
			{name: "no marker to find it by", pr: 412, full: "acme/checkout"},
			{name: "a blank marker", marker: "   ", pr: 412, full: "acme/checkout"},
			{name: "not a pull request number", marker: marker, full: "acme/checkout"},
			{name: "a negative pull request", marker: marker, pr: -1, full: "acme/checkout"},
			{name: "not a repository name", marker: marker, pr: 412, full: "acme"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				err := newGitHub().UpsertPRComment(t.Context(), tokenConn(server.URL), tt.full, tt.pr, tt.marker, "body")
				if err == nil {
					t.Error("want an error")
				}
			})
		}
		if requests.get() != 0 {
			t.Errorf("made %d requests for invalid input, want none", requests.get())
		}
	})
}
