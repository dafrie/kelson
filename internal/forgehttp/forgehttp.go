// Package forgehttp is kelson's browser- and forge-facing HTTP surface: the
// GitHub App manifest flow ([ADR-0033](docs/adr/0033-git-connections.md)
// decision 2) and the webhook listener
// ([ADR-0034](docs/adr/0034-forge-driven-delivery.md) decisions 1 and 2).
//
// # Why it is not in internal/api
//
// Nothing here is an RPC. A webhook delivery is a POST whose body is signed by
// somebody else's HMAC and whose schema is GitHub's; the manifest flow is three
// browser redirects and a form. Neither has a place in a ConnectRPC schema, and
// putting them there would mean a proto message per forge payload — a wire
// contract kelson would then owe compatibility to, for a shape GitHub owns.
// They are plain handlers on kelson-server's existing mux instead.
//
// It is also not internal/forge. That package is the provider seam: no cluster,
// no state, no logging (its own doc says so). This one has all three, and the
// split is what keeps a webhook verifier testable against fixtures while the
// endpoint that calls it is testable against fake stores.
//
// # Events enqueue reconciliation; they do not carry state
//
// ADR-0034 decision 1's replay-safety argument is the load-bearing rule of this
// package. A verified `pull_request` delivery causes exactly one thing: the
// environment's ResourceSetInputProvider gets `reconcile.fluxcd.io/requestedAt`
// stamped on it, so flux-operator re-polls the forge *now* rather than at its
// interval. Nothing in the payload becomes state. A forged or replayed delivery
// can therefore at worst cause a redundant reconcile of the truth, which is why
// a webhook is never load-bearing: lose every delivery and previews are as slow
// as `previews.interval`, never wrong.
//
// The one exception is bookkeeping the forge alone knows: an `installation`
// event records the installation ID on the connection whose secret verified it,
// because the app-manifest flow creates a connection *before* the app is
// installed and nothing else can learn that number (ADR-0033 decision 2 step 3).
//
// # What is not gated, and by what
//
// The webhook endpoint authenticates its caller by HMAC over the body, against
// the connections this instance holds — that is the whole gate, it is
// constant-time, and a delivery that verifies against nothing is refused with
// no state change and nothing about the secret material in the log.
//
// The manifest endpoints are *not* behind kelson's shared password. The auth
// gate in internal/api protects `/kelson.v1alpha1.*` and passes everything else
// (that is what lets the UI load before anyone can log in), and it exposes no
// per-request check this package could call. Under the interim trust model
// (ADR-0013 §3: loopback, or a password behind a TLS-terminating proxy) reaching
// this server unauthenticated is already the exposure #84 owns — but a reader
// should know the difference rather than assume the password covers this. The
// state parameter below is CSRF protection for the round trip, not
// authentication of the person taking it.
package forgehttp

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/dafrie/kelson/internal/controlstore"
	"github.com/dafrie/kelson/internal/forgeconn"
	"github.com/dafrie/kelson/internal/model"
)

// The routes. They are constants because three things must agree on them: this
// package's Register, the app manifest's webhook and redirect URLs, and the
// "Connect GitHub" anchor in the UI (ui/src/pages/ConnectionsPage.tsx) — plus
// the `/forge/` entry in the dev proxy that makes the anchor reach this server
// under `npm run dev`.
const (
	// PathPrefix is what the whole surface lives under, and what the UI's dev
	// proxy forwards.
	PathPrefix = "/forge/"

	// WebhookPath receives GitHub deliveries.
	WebhookPath = "/forge/github/webhook"

	// ManifestStartPath begins the app-manifest flow.
	ManifestStartPath = "/forge/github/manifest/start"

	// ManifestCallbackPath receives the one-time code.
	ManifestCallbackPath = "/forge/github/manifest/callback"

	// ConnectionsPath is where the browser lands when the flow ends, whichever
	// way it ended. It is the UI route the flow started from.
	ConnectionsPath = "/connections"
)

// maxBody bounds a webhook delivery. GitHub caps payloads at 25MB; kelson reads
// four event kinds whose useful fields fit in kilobytes, so this is the
// backstop against an endpoint that is not what it claims to be rather than a
// limit anyone should meet.
const maxBody = 5 << 20

// Connections is the store half this package needs.
//
// It is narrower than [controlstore.GitConnectionStore] on purpose: nothing
// here deletes a connection, and a webhook endpoint that could would be a
// webhook endpoint one forged delivery away from removing an instance's
// credentials.
type Connections interface {
	List(ctx context.Context) ([]controlstore.StoredConnection, error)
	Get(ctx context.Context, name string) (controlstore.StoredConnection, error)
	Create(ctx context.Context, name string, spec model.GitConnectionSpec, opts controlstore.CreateConnectionOptions) (controlstore.StoredConnection, error)
	RecordInstallation(ctx context.Context, name string, installationID int64) (controlstore.StoredConnection, error)
}

// Specs enumerates the stored projects, so a delivery about a repository can be
// matched to the environments that preview it.
//
// It is a list and not a query because there is no index: `previews.repo` is a
// field inside an Environment document, and an instance holds tens of projects,
// not thousands. When that stops being true the seam is where a real index goes.
type Specs interface {
	List(ctx context.Context) ([]controlstore.Stored, error)
}

// SecretWriter writes the material a freshly created GitHub App handed back,
// into kelson's own namespace (ADR-0033 decision 1: the CR carries identifiers,
// the Secret carries the material).
type SecretWriter interface {
	Write(ctx context.Context, name string, data map[string][]byte) error
}

// PreviewPoker asks flux-operator to re-poll one ResourceSetInputProvider now.
// [flux.InputProviderPoker] is the implementation; it is an interface here so a
// test asserts which object was poked without a cluster.
type PreviewPoker interface {
	Poke(ctx context.Context, namespace, name string) error
}

// Options configures a [Handler].
type Options struct {
	// Sources resolves connections and mints from them. Required.
	Sources *forgeconn.Resolver

	// Connections is the store the manifest callback writes into and the
	// installation event updates. Nil disables both manifest endpoints, which
	// is honest for a server with no cluster: a flow that could not record its
	// result would send the user to GitHub to create an app kelson then loses.
	Connections Connections

	// Specs enumerates projects, for matching a delivery to the environments
	// that preview its repository. Nil means no environment is ever matched,
	// so deliveries verify and then do nothing but say so.
	Specs Specs

	// Secrets writes the app's private key and webhook secret. Nil disables the
	// manifest endpoints, for Connections' reason.
	Secrets SecretWriter

	// Previews stamps the reconcile annotation. Nil means previews update at
	// their poll interval, which ADR-0034 decision 2 keeps as the only path on
	// instances that cannot receive deliveries anyway.
	Previews PreviewPoker

	// ExternalURL is the base URL GitHub and the user's browser reach this
	// server at, e.g. https://kelson.acme.com. Empty derives it per request
	// from the Host header and the forwarded scheme, which is right behind a
	// proxy that sets them and wrong behind one that does not — see
	// [Handler.baseURL].
	ExternalURL string

	// Logger records what a delivery did. Nil discards. Nothing written here
	// carries secret material: a signature is reported as verified or not, and
	// never quoted.
	Logger *slog.Logger

	// Now is injectable so a test can pin the state cookie's expiry.
	Now func() int64
}

// Handler serves the forge endpoints.
type Handler struct {
	opts Options
	log  *slog.Logger
}

// New returns a Handler. A resolver is required; everything else degrades to a
// stated refusal rather than to a nil dereference, because this surface is
// reachable by anyone who can reach the port and must answer rather than panic.
func New(opts Options) (*Handler, error) {
	if opts.Sources == nil {
		return nil, errNoResolver
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Handler{opts: opts, log: log}, nil
}

// Register mounts the three routes on a mux.
//
// The patterns carry their methods, so a GET to the webhook and a POST to the
// manifest start are 405s from the mux rather than handlers that have to check.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST "+WebhookPath, h.webhook)
	mux.HandleFunc("GET "+ManifestStartPath, h.manifestStart)
	mux.HandleFunc("GET "+ManifestCallbackPath, h.manifestCallback)
}

// baseURL is the URL this server is reachable at from outside.
//
// The configured value wins, because it is the only one that is a *fact* rather
// than an inference: the manifest's webhook URL is baked into the created app
// and an app created with the wrong one has to be deleted and remade. Without
// it the request is the best evidence available — the Host header the browser
// used, and the scheme a TLS-terminating proxy declared — which is correct
// behind a proxy that sets `X-Forwarded-Proto` and is what a single-origin
// server already assumes everywhere else (ADR-0013 §3).
func (h *Handler) baseURL(r *http.Request) string {
	if u := strings.TrimRight(strings.TrimSpace(h.opts.ExternalURL), "/"); u != "" {
		return u
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	host := r.Host
	if forwarded := strings.TrimSpace(r.Header.Get("X-Forwarded-Host")); forwarded != "" {
		host = strings.TrimSpace(strings.Split(forwarded, ",")[0])
	}
	return scheme + "://" + host
}

// writeJSON answers with a small object. Every response body this package
// writes is one of these: a browser never reads them (it is redirected), and
// what does read them is GitHub's delivery log and whoever is debugging why a
// preview did not update.
func writeJSON(w http.ResponseWriter, status int, body map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// redirectToConnections sends the browser back where the flow started, with the
// outcome in the query.
//
// The UI reads none of these parameters today — ConnectionsPage was written
// against "the flow completes on GitHub and returns here" and shows the
// connection list, so a success is visible as a new row and a failure as its
// absence. They are written anyway, and named plainly, because the alternative
// is a failed flow that looks identical to a cancelled one; the UI slice that
// reads them needs no change here.
func redirectToConnections(w http.ResponseWriter, r *http.Request, params map[string]string) {
	q := url.Values{}
	for k, v := range params {
		if v != "" {
			q.Set(k, v)
		}
	}
	target := ConnectionsPath
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}
