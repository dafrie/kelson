package webui

import (
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// builtUI is what static/ looks like after `make ui`: Vite's entry document,
// its content-addressed bundle, and the one file public/ contributes.
func builtUI() fstest.MapFS {
	return fstest.MapFS{
		"index.html":                &fstest.MapFile{Data: []byte(`<!doctype html><title>kelson</title><div id="root"></div>`)},
		"assets/index-a1b2c3d4.js":  &fstest.MapFile{Data: []byte(`console.log("kelson")`)},
		"assets/index-e5f6a7b8.css": &fstest.MapFile{Data: []byte(`:root{color-scheme:dark}`)},
		"kelson-favicon-32.svg":     &fstest.MapFile{Data: []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`)},
	}
}

// unbuiltUI is what static/ looks like on a fresh clone: the committed
// placeholder and nothing else.
func unbuiltUI() fstest.MapFS {
	return fstest.MapFS{
		placeholderPage: &fstest.MapFile{Data: []byte(`<!doctype html><title>kelson</title>run make ui`)},
	}
}

func get(t *testing.T, h http.Handler, target string) *http.Response {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec.Result()
}

func body(t *testing.T, res *http.Response) string {
	t.Helper()
	defer res.Body.Close() //nolint:errcheck // read-only handle over a buffer
	read, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("reading the response body: %v", err)
	}
	return string(read)
}

// TestThePlaceholderStandsInForTheIndex is the fresh-clone posture: a binary
// built without Node still answers "/" with a page, and the page says what
// happened. A 404 there would be indistinguishable from a broken server.
func TestThePlaceholderStandsInForTheIndex(t *testing.T) {
	h := handlerFor(unbuiltUI())

	res := get(t, h, "/")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	if got := body(t, res); !strings.Contains(got, "make ui") {
		t.Errorf("the placeholder does not say how to get a real UI: %q", got)
	}
	if built(unbuiltUI()) {
		t.Error("built() reports a UI in a tree that has only the placeholder")
	}
}

// TestThePlaceholderIsNotARoute: it is a fallback, and a binary carrying the
// real UI must not also serve a page claiming it does not.
func TestThePlaceholderIsNotARoute(t *testing.T) {
	tree := builtUI()
	tree[placeholderPage] = &fstest.MapFile{Data: []byte(`the UI was not built into this binary`)}

	res := get(t, handlerFor(tree), "/"+placeholderPage)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /%s = %d, want the SPA fallback", placeholderPage, res.StatusCode)
	}
	if got := body(t, res); strings.Contains(got, "not built into this binary") {
		t.Errorf("the placeholder was served at its own path: %q", got)
	}
}

// TestAssetsAreServedAtTheirOwnPaths, with the content types a browser needs to
// execute them and the caching each name licenses: a content-addressed name may
// be cached forever, index.html may not — it is the only file whose name stays
// the same while its contents change.
func TestAssetsAreServedAtTheirOwnPaths(t *testing.T) {
	h := handlerFor(builtUI())

	for _, tc := range []struct {
		path      string
		wantType  string
		wantCache string
	}{
		{"/assets/index-a1b2c3d4.js", "javascript", cacheImmutable},
		{"/assets/index-e5f6a7b8.css", "text/css", cacheImmutable},
		{"/kelson-favicon-32.svg", "image/svg+xml", cacheRevalidate},
		{"/", "text/html", cacheRevalidate},
	} {
		res := get(t, h, tc.path)
		if res.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", tc.path, res.StatusCode)
			continue
		}
		if ct := res.Header.Get("Content-Type"); !strings.Contains(ct, tc.wantType) {
			t.Errorf("GET %s Content-Type = %q, want %q", tc.path, ct, tc.wantType)
		}
		if cc := res.Header.Get("Cache-Control"); cc != tc.wantCache {
			t.Errorf("GET %s Cache-Control = %q, want %q", tc.path, cc, tc.wantCache)
		}
		res.Body.Close() //nolint:errcheck // read-only handle over a buffer
	}
}

// TestUnknownRoutesFallBackToTheIndex is what makes a deep link work: the SPA's
// routes exist only in the browser, so a reload of /projects/shop must hand the
// browser the application rather than a 404 it cannot route.
func TestUnknownRoutesFallBackToTheIndex(t *testing.T) {
	h := handlerFor(builtUI())

	for _, target := range []string{"/projects/shop", "/projects/shop/environments/production", "/login"} {
		res := get(t, h, target)
		if res.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d, want the index", target, res.StatusCode)
			continue
		}
		if got := body(t, res); !strings.Contains(got, `id="root"`) {
			t.Errorf("GET %s served %q, want the index document", target, got)
		}
	}
}

// TestTheServersOwnPathsAreNeverAPage. The mux gives its registered patterns
// precedence anyway; this is about the paths it has *not* registered — a typo'd
// service name must answer as a missing endpoint, not as a page of HTML a
// ConnectRPC client would then fail to decode.
func TestTheServersOwnPathsAreNeverAPage(t *testing.T) {
	h := handlerFor(builtUI())

	for _, target := range []string{
		"/kelson.v1alpha1.NoSuchService/Nope",
		"/auth/nothing-here",
		"/healthz",
	} {
		res := get(t, h, target)
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", target, res.StatusCode)
		}
		res.Body.Close() //nolint:errcheck // read-only handle over a buffer
	}
}

// TestOnlyNavigationsGetTheFallback: a POST to an unrouted path is a client bug
// or a probe, and answering either with 200 and the index page would make both
// look like they worked.
func TestOnlyNavigationsGetTheFallback(t *testing.T) {
	rec := httptest.NewRecorder()
	handlerFor(builtUI()).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/projects/shop", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /projects/shop = %d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != "GET, HEAD" {
		t.Errorf("Allow = %q, want the two methods the UI serves", allow)
	}

	rec = httptest.NewRecorder()
	handlerFor(builtUI()).ServeHTTP(rec, httptest.NewRequest(http.MethodHead, "/", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("HEAD / = %d, want 200", rec.Code)
	}
}

// TestAPathCannotClimbOutOfTheEmbeddedTree. The tree is embedded so there is no
// filesystem above it to reach, but the handler must not depend on that being
// true: a path that tries gets the fallback or a refusal, never a file.
func TestAPathCannotClimbOutOfTheEmbeddedTree(t *testing.T) {
	tree := builtUI()
	tree["secret.txt"] = &fstest.MapFile{Data: []byte("SHOULD-NOT-ESCAPE")}
	h := handlerFor(tree)

	for _, target := range []string{"/../secret.txt", "/assets/../secret.txt/..", "//secret.txt/.."} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.URL.Path = target
		h.ServeHTTP(rec, req)
		if strings.Contains(rec.Body.String(), "SHOULD-NOT-ESCAPE") {
			t.Errorf("GET %s served a file from outside the tree", target)
		}
	}
}

// TestTheEmbeddedTreeAlwaysServesSomething runs against the real embed rather
// than a fixture, so it holds in both worlds: a checkout with no `make ui` (the
// placeholder) and a release build (the UI). The assertion is the invariant
// both share — "/" is a page, and it names its own state.
func TestTheEmbeddedTreeAlwaysServesSomething(t *testing.T) {
	if _, err := fs.Stat(assets, placeholderPage); err != nil {
		t.Fatalf("the committed placeholder is missing from the embedded tree: %v", err)
	}

	res := get(t, Handler(), "/")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	got := body(t, res)
	if !Built() && !strings.Contains(got, "make ui") {
		t.Errorf("a binary with no UI served a page that does not say so: %q", got)
	}
}
