// Package webui carries the kelson web UI inside kelson-server and serves it
// from the same listener the API answers on (milestone M6, issues
// [#7](https://github.com/dafrie/kelson/issues/7) and
// [#61](https://github.com/dafrie/kelson/issues/61)).
//
// # One origin, because the UI assumes one
//
// The UI is a Vite SPA whose base URL is "/" and whose session is a cookie
// (ui/src/api/clients.ts, internal/api/auth.go). Serving it from the process
// that answers /kelson.v1alpha1.* means the browser's origin and the API's
// origin are the same one, so there is no CORS middleware to write, no second
// hostname to configure and no cross-site cookie to argue with. In development
// the Vite dev proxy manufactures that same property (ui/vite.config.ts); here
// it is simply true. That is also why a Helm install needs nothing but a
// port-forward to be usable: one Service port is the whole product.
//
// # Where the bytes come from, and why a placeholder exists
//
// The embed directive can only read a directory that exists when the compiler
// runs, and two facts collide there: ui/dist is a build artifact this
// repository does not commit, and `go build ./...` must succeed on a fresh
// clone with no Node installed (.github/workflows/ci.yml cross-compiles without
// it). So the embedded directory is static/, which is committed but nearly
// empty:
//
//   - `make ui` builds ui/ and copies ui/dist/* into static/ before the
//     compiler runs. The contents are ignored by static/.gitignore, so a UI
//     build leaves `git status` clean — which matters beyond tidiness, because
//     goreleaser refuses to release from a dirty tree.
//   - The one committed page is placeholder.html, and it is deliberately not
//     named index.html: a tracked index.html would be overwritten by every UI
//     build and then committed by accident. It is served in index.html's place
//     when nobody ran the build, and it says so — a binary with no UI must
//     announce that rather than answer 404 and look broken.
//
// [Built] reports which of the two a binary carries, so the startup banner can
// say it out loud instead of leaving an operator to discover it in a browser.
//
// # What it does not do
//
// It does not authenticate. The SPA, its assets and the placeholder are served
// to anyone who can reach the port, exactly as the Vite dev server serves them:
// the login happens inside the app against /auth/login, and every route that
// can touch the cluster is gated (internal/api/auth.go). Shipping the login
// screen behind the login would be a loop.
package webui

import (
	"embed"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
)

//go:embed static
var embedded embed.FS

// assets is the embedded tree with its directory prefix stripped, so a request
// path maps onto a file name directly. fs.Sub fails only on a path name that is
// not valid for io/fs, and "static" is a constant, so the error it returns here
// is unreachable.
var assets, _ = fs.Sub(embedded, "static")

const (
	// indexPage is the SPA's entry document, and the fallback every route the
	// client owns resolves to. It exists only after `make ui`.
	indexPage = "index.html"

	// placeholderPage stands in for indexPage in a binary built without the UI.
	// It is never served at its own path: it is a fallback, not a route, and a
	// binary that has the real UI must not also expose a page saying it does
	// not.
	placeholderPage = "placeholder.html"

	// hashedAssetDir is where Vite emits content-addressed files. Their names
	// change whenever their bytes do, which is what makes an immutable cache
	// safe for them and for nothing else here.
	hashedAssetDir = "assets/"
)

// Cache-Control values. index.html is revalidated on every load because its
// name never changes while its contents do — a cached copy would keep pointing
// at the previous build's asset names long after a redeploy.
const (
	cacheImmutable  = "public, max-age=31536000, immutable"
	cacheRevalidate = "no-cache"
)

func init() {
	// The release image is FROM scratch, so there is no /etc/mime.types for the
	// mime package to fall back on: an extension missing from Go's builtin
	// table would be sniffed, and a font sniffs as application/octet-stream.
	// These are the extensions a Vite build emits that the builtin table does
	// not know.
	for ext, typ := range map[string]string{
		".woff":        "font/woff",
		".woff2":       "font/woff2",
		".ttf":         "font/ttf",
		".otf":         "font/otf",
		".ico":         "image/x-icon",
		".webmanifest": "application/manifest+json",
	} {
		// AddExtensionType errors only on an extension that does not begin with
		// a dot; every key above is a literal that does.
		_ = mime.AddExtensionType(ext, typ)
	}
}

// Built reports whether a real UI build was copied into static/ before this
// binary was compiled. False means [Handler] serves the placeholder.
func Built() bool { return built(assets) }

// Handler serves the embedded UI: files at their own paths, and index.html for
// every other GET or HEAD, because the SPA's routes exist only in the browser.
//
// It is safe to mount at "/" beside the API. Go's ServeMux already gives the
// registered API patterns precedence over a catch-all, and this handler refuses
// the API's prefixes on its own as well — belt and braces, so a route the
// server has not registered (an unknown RPC, a typo'd service name) answers 404
// rather than 200 and a page of HTML that no client asked for.
func Handler() http.Handler { return handlerFor(assets) }

func built(fsys fs.FS) bool {
	_, err := fs.Stat(fsys, indexPage)
	return err == nil
}

func handlerFor(fsys fs.FS) http.Handler {
	index := indexPage
	if !built(fsys) {
		index = placeholderPage
	}
	return &spa{fsys: fsys, index: index}
}

// spa serves one embedded tree with a single-page application's routing rules.
type spa struct {
	fsys fs.FS
	// index is the document unmatched routes fall back to: indexPage, or
	// placeholderPage in a binary built without the UI.
	index string
}

func (s *spa) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if reserved(r.URL.Path) {
		http.NotFound(w, r)
		return
	}
	// Only a navigation gets the fallback. A POST to an unrouted path is a
	// client bug or a probe, and answering it with the index page would turn
	// both into something that looks like it worked.
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "the web UI serves GET and HEAD", http.StatusMethodNotAllowed)
		return
	}
	if name, ok := s.asset(r.URL.Path); ok {
		cache := cacheRevalidate
		if strings.HasPrefix(name, hashedAssetDir) {
			cache = cacheImmutable
		}
		w.Header().Set("Cache-Control", cache)
		http.ServeFileFS(w, r, s.fsys, name)
		return
	}
	w.Header().Set("Cache-Control", cacheRevalidate)
	http.ServeFileFS(w, r, s.fsys, s.index)
}

// asset resolves a request path to an embedded file. It reports false for
// anything that is not one — a client-side route, a directory, a path that
// tries to climb out of the tree — and the caller then serves the index.
func (s *spa) asset(urlPath string) (string, bool) {
	name := path.Clean(strings.TrimPrefix(urlPath, "/"))
	if name == "." || name == placeholderPage || !fs.ValidPath(name) {
		return "", false
	}
	info, err := fs.Stat(s.fsys, name)
	if err != nil || info.IsDir() {
		return "", false
	}
	return name, true
}

// reserved reports whether a path belongs to the server rather than to the UI.
// The API's own routes are registered on the mux and win there anyway; this is
// what keeps an *unregistered* path under those prefixes answering as a missing
// endpoint instead of as a missing page.
func reserved(urlPath string) bool {
	return strings.HasPrefix(urlPath, "/kelson.v1alpha1.") ||
		strings.HasPrefix(urlPath, "/auth/") ||
		urlPath == "/healthz"
}
