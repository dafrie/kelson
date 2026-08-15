package artifact_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/dafrie/kelson/internal/artifact"
	"github.com/dafrie/kelson/internal/build/registry"
)

// The read half is tested against a fake registry for the same reason the push
// half is: what is worth pinning is the conversation. Which requests, whether
// pagination is followed, what a 404 means, and — the one that would be a
// silent lie if it were wrong — that a repository too large to enumerate is
// refused rather than truncated.

// tagRegistry serves /v2/<name>/tags/list with pagination and /v2/<name>/
// manifests/<tag> HEADs.
type tagRegistry struct {
	tags []string
	// page is how many tags one response carries; zero serves them all at once.
	page int
	// digests are the manifest digests, keyed by tag. A tag that is not here is
	// a 404.
	digests map[string]string
	// scopes records the token scopes asked for, so a read can be shown to ask
	// for a read.
	scopes []string
	// challenge makes the registry demand a bearer token first.
	challenge bool
	requests  []string
}

func (f *tagRegistry) handler(base *string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests = append(f.requests, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)

		if r.URL.Path == "/token" {
			f.scopes = append(f.scopes, r.URL.Query().Get("scope"))
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "a-bearer-token"})
			return
		}
		if f.challenge && r.Header.Get("Authorization") != "Bearer a-bearer-token" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+*base+`/token",service="fake"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		if strings.HasSuffix(r.URL.Path, "/tags/list") {
			f.serveTags(w, r)
			return
		}
		if tag, ok := cutManifest(r.URL.Path); ok {
			digest, held := f.digests[tag]
			if !held {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Docker-Content-Digest", digest)
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
}

func (f *tagRegistry) serveTags(w http.ResponseWriter, r *http.Request) {
	from := 0
	if last := r.URL.Query().Get("last"); last != "" {
		for i, tag := range f.tags {
			if tag == last {
				from = i + 1
				break
			}
		}
	}
	end := len(f.tags)
	if f.page > 0 && from+f.page < end {
		end = from + f.page
	}
	page := f.tags[from:end]
	if end < len(f.tags) && len(page) > 0 {
		w.Header().Set("Link", `</v2/acme/kelson/shop-production/tags/list?n=`+
			strconv.Itoa(f.page)+`&last=`+page[len(page)-1]+`>; rel="next"`)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"name": "acme/kelson/shop-production", "tags": page})
}

func cutManifest(path string) (string, bool) {
	i := strings.Index(path, "/manifests/")
	if i < 0 {
		return "", false
	}
	return path[i+len("/manifests/"):], true
}

func serveTagRegistry(t *testing.T, f *tagRegistry) string {
	t.Helper()
	var base string
	server := httptest.NewServer(f.handler(&base))
	t.Cleanup(server.Close)
	base = server.URL
	return strings.TrimPrefix(server.URL, "http://")
}

func TestTagsListsEveryPage(t *testing.T) {
	fake := &tagRegistry{tags: []string{"1-aaaaaaaa", "10-cccccccc", "2-bbbbbbbb"}, page: 2}
	host := serveTagRegistry(t, fake)

	pusher := &artifact.Pusher{Insecure: true}
	tags, err := pusher.Tags(context.Background(), host+"/acme/kelson/shop-production")
	if err != nil {
		t.Fatalf("Tags: %v", err)
	}
	if got, want := strings.Join(tags, ","), "1-aaaaaaaa,10-cccccccc,2-bbbbbbbb"; got != want {
		t.Errorf("tags = %q, want %q — every page, in the registry's own order", got, want)
	}
	if len(fake.requests) != 2 {
		t.Errorf("requests = %v, want two pages", fake.requests)
	}
}

// A repository nothing was ever published to is an answer, not a failure: an
// environment with no artifacts has no revisions, and a caller must be able to
// say so without inspecting an error.
func TestTagsUnknownRepositoryIsEmpty(t *testing.T) {
	host := serveTagRegistry(t, &tagRegistry{})

	pusher := &artifact.Pusher{Insecure: true}
	tags, err := pusher.Tags(context.Background(), host+"/acme/kelson/never-published")
	if err != nil {
		t.Fatalf("Tags: %v", err)
	}
	if len(tags) != 0 {
		t.Errorf("tags = %v, want none", tags)
	}
}

// The bound is a refusal and not a truncation. A tag list arrives in lexical
// order, so a partial walk is an arbitrary subset — and a subset presented as a
// history is the failure the registry query exists to fix.
func TestTagsRefusesRatherThanTruncates(t *testing.T) {
	fake := &tagRegistry{page: 1}
	for i := 0; i <= artifact.MaxTagPages; i++ {
		fake.tags = append(fake.tags, fmt.Sprintf("%d-aaaaaaaa", i+1))
	}
	host := serveTagRegistry(t, fake)

	pusher := &artifact.Pusher{Insecure: true}
	_, err := pusher.Tags(context.Background(), host+"/acme/kelson/shop-production")
	var refusal artifact.Error
	if !errors.As(err, &refusal) || refusal.Reason != artifact.ReasonTagListTooLarge {
		t.Fatalf("Tags error = %v, want %s", err, artifact.ReasonTagListTooLarge)
	}
}

func TestTagsRefusesABodyThatIsNotATagList(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>please log in</html>"))
	}))
	t.Cleanup(server.Close)

	pusher := &artifact.Pusher{Insecure: true}
	_, err := pusher.Tags(context.Background(), strings.TrimPrefix(server.URL, "http://")+"/acme/x")
	var refusal artifact.Error
	if !errors.As(err, &refusal) || refusal.Reason != artifact.ReasonTagListUnreadable {
		t.Fatalf("Tags error = %v, want %s", err, artifact.ReasonTagListUnreadable)
	}
}

// A read asks for a read. A pull-only credential handed a "pull,push" scope
// gets a token that authorises nothing, and the 403 that follows says nothing
// about which half was missing.
func TestReadsAskForAPullScope(t *testing.T) {
	fake := &tagRegistry{tags: []string{"1-aaaaaaaa"}, challenge: true}
	host := serveTagRegistry(t, fake)

	pusher := &artifact.Pusher{Insecure: true, Credential: registry.Credential{Username: "u", Password: "p"}}
	if _, err := pusher.Tags(context.Background(), host+"/acme/kelson/shop-production"); err != nil {
		t.Fatalf("Tags: %v", err)
	}
	if len(fake.scopes) == 0 {
		t.Fatal("no token was requested")
	}
	for _, scope := range fake.scopes {
		if !strings.HasSuffix(scope, ":pull") {
			t.Errorf("token scope = %q, want a pull scope for a read", scope)
		}
	}
}

func TestResolveReportsTheDigestAndTheAbsence(t *testing.T) {
	fake := &tagRegistry{digests: map[string]string{"7-a1b2c3d4": "sha256:feedface"}}
	host := serveTagRegistry(t, fake)
	repository := host + "/acme/kelson/shop-production"
	pusher := &artifact.Pusher{Insecure: true}

	digest, found, err := pusher.Resolve(context.Background(), repository, "7-a1b2c3d4")
	if err != nil || !found || digest != "sha256:feedface" {
		t.Fatalf("Resolve(present) = %q, %v, %v; want the digest and found", digest, found, err)
	}

	// A revision the registry does not hold is an answer, not an error: it is
	// how a typo and a revision that never existed are told apart from a
	// registry that refused to say.
	digest, found, err = pusher.Resolve(context.Background(), repository, "9-99999999")
	if err != nil {
		t.Fatalf("Resolve(absent): %v", err)
	}
	if found || digest != "" {
		t.Errorf("Resolve(absent) = %q, %v; want not found", digest, found)
	}
}
