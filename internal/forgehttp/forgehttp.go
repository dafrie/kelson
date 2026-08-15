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
// # What is gated, and by what
//
// Four endpoints and three different gates, and the differences are the design
// rather than an accident of what was easy to mount.
//
// The **webhook** authenticates its caller by HMAC over the body, against the
// connections this instance holds — that is the whole gate, it is constant-time,
// and a delivery that verifies against nothing is refused with no state change
// and nothing about the secret material in the log. It is not behind kelson's
// own credential and must not be: GitHub holds no kelson session, and the shared
// secret it does hold is the per-connection webhook secret this check reads.
//
// The **manifest session** endpoint is behind kelson's credential, and behind
// exactly the one the RPCs are (issue #248): the same header, the same
// principals, the same code, reached through the [Authenticator] that
// cmd/kelson-server injects at [Handler.Register]. It mints nothing but a start
// URL.
//
// **Manifest start** requires the one-time ticket that URL carries (ticket.go)
// and refuses without it. The two-step exists because the browser reaches
// /start by top-level navigation, which carries no header a fetch would have
// added and must never carry a credential in its query. The ticket is what
// survives that hop: two minutes, one use, no identity, worthless once spent.
//
// The **manifest callback** takes no ticket and needs none. GitHub is what calls
// it, carrying the one-time code, and the authentication of that round trip is
// the state cookie /start set — a callback arriving at a browser that did not
// start a flow here has nothing to match and is refused (manifest.go). Requiring
// a ticket there would be requiring one of GitHub, which has none.
package forgehttp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"

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

	// ManifestSessionPath mints the one-time ticket ManifestStartPath requires.
	// It is the authenticated door into the flow, and the only endpoint here the
	// UI calls with its normal transport rather than by navigating to.
	ManifestSessionPath = "/forge/github/manifest/session"

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

// Push is one verified `push` delivery, reduced to what the trigger needs
// (ADR-0036 decision 3).
//
// Ref travels in whatever spelling the forge used — `refs/heads/main` — because
// exactly one piece of code decides what that reduces to, and it is on the other
// side of this seam beside the model contract that consumes it
// (internal/api's ShortRef). A second stripper here would be a second answer to
// a question with one.
type Push struct {
	// Project is the stored project this trigger is about. One delivery may
	// produce several, because two projects may build from one repository.
	Project string
	// Repo is the repository that moved, spelled the way a spec spells one so
	// the resolver can compare it against a `source.git`.
	Repo string
	// Ref is the ref the delivery named, unreduced.
	Ref string
	// SHA is the commit at the head of Ref after the push — GitHub's `after`.
	SHA string
	// Connection is the git connection whose webhook secret verified this
	// delivery. It is the audit trail's principal for everything the trigger
	// does (ADR-0036 decision 4: the connection as a system principal, never an
	// invented human), and it is the only identity in the transaction.
	Connection string
}

// PushPlan is what a push would do, answered from the stored spec alone.
type PushPlan struct {
	// Environments are the ones this push would move, with the components it
	// would move in each.
	Environments map[string][]string
	// Refused is a project-wide refusal — today `build/several-sources` (#252) —
	// or "" for a project a push may move.
	Refused string
}

// PushOutcome is what a trigger did.
type PushOutcome struct {
	Triggered []string
	Notes     []string
	Refused   string
}

// AutoDeployer is the `autoDeploy` trigger seam (ADR-0036 decision 3).
//
// It is two methods because the two halves have two deadlines: PlanPush is a
// decode and a resolve, answered inside the delivery's own ten seconds and
// written into the response, and RunPush can be a container build, which is
// enqueued behind it ([Handler.push] says why at length).
//
// The types are declared here rather than imported for [Authenticator]'s
// reason, stated in this package's doc: internal/api may hold no Kubernetes
// client and this package holds two, so an import edge either way would hand one
// plane the other's reach in a lint rule that only sees direct imports.
// cmd/kelson-server is where the two meet, and the adapter there is three field
// copies.
//
// A nil one is a server whose deliveries verify, resolve and then do nothing but
// say so — which is exactly what every `push` did before this existed.
type AutoDeployer interface {
	// PlanPush must not build, publish or write. The handler calls it while a
	// forge is waiting.
	PlanPush(ctx context.Context, p Push) (PushPlan, error)
	// RunPush does the work. It is called on a detached context with its own
	// budget, from a goroutine nothing waits on.
	RunPush(ctx context.Context, p Push) (PushOutcome, error)
}

// refusalSeveralSources is the code a refused push carries in the log and in the
// delivery's answer. It is internal/build's taxonomy spelled here rather than
// imported for the reason the seam types above are, and it is asserted against
// the plane that produces it by this package's tests.
const refusalSeveralSources = "build/several-sources"

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

	// AutoDeploy is what a verified `push` sets in motion (ADR-0036). Nil is a
	// server that verifies a push, resolves it and does nothing — the behaviour
	// every kelson had before this, and still the right one for a process with
	// no spec store to write into.
	AutoDeploy AutoDeployer

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

// Authenticator applies kelson-server's own credential check to one request.
// The returned string is a refusal message and is empty exactly when the
// request carries a credential the server accepts.
//
// It is a function rather than an interface or an imported type so that this
// package keeps no dependency on internal/api. The direction matters: the RPC
// plane gets no cluster client by design (the `api` depguard rule says so, and
// the stores are the seam that enforces it), while this package holds two — so
// an import edge either way would hand one plane the other's reach in a lint
// rule that only sees direct imports. A stdlib-shaped func crosses the gap with
// nothing attached, and it is what keeps this package testable with a literal.
//
// [github.com/dafrie/kelson/internal/api.Auth.CheckRequest] is the only
// implementation, and cmd/kelson-server is the only place the two meet.
type Authenticator func(r *http.Request) (refusal string)

// Handler serves the forge endpoints.
type Handler struct {
	opts Options
	log  *slog.Logger

	// tickets is this process's authority over the one-time tokens
	// ManifestStartPath requires (ticket.go).
	tickets *tickets

	// inflight counts the auto-deploy triggers this handler has started and not
	// yet finished. Nothing in production waits on it — a delivery is answered
	// before its trigger runs, deliberately — and it exists so a test can join
	// the goroutine it started rather than poll for its effect.
	inflight sync.WaitGroup

	// authenticate gates ManifestSessionPath. It arrives at Register rather than
	// in Options because kelson-server cannot build it any earlier: the gate is
	// made from the agent store this Handler's own plane creates, so the mount
	// is the first moment both exist. Nil is a wiring mistake and is refused as
	// one — never treated as "no authentication configured", which is a
	// different fact that [Authenticator] itself reports.
	authenticate Authenticator
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
	tickets, err := newTickets()
	if err != nil {
		return nil, fmt.Errorf("generating the manifest ticket key: %w", err)
	}
	return &Handler{opts: opts, log: log, tickets: tickets}, nil
}

// Register mounts the four routes on a mux, behind the credential check the
// caller hands in.
//
// The check is a parameter and not an option because it is a property of the
// mount: whoever puts this surface on a mux is the one that knows what gates the
// rest of that mux, and a signature that cannot be satisfied without deciding is
// better than a field that defaults to open (issue #248).
//
// The patterns carry their methods, so a GET to the webhook and a POST to the
// manifest start are 405s from the mux rather than handlers that have to check.
func (h *Handler) Register(mux *http.ServeMux, authenticate Authenticator) {
	h.authenticate = authenticate
	mux.HandleFunc("POST "+WebhookPath, h.webhook)
	mux.HandleFunc("POST "+ManifestSessionPath, h.manifestSession)
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
// writes is one of these, and three kinds of reader see them: GitHub's delivery
// log, whoever is debugging why a preview did not update, and — for the ticket
// refusals alone (ticket.go) — a person whose browser landed on /start without
// a live ticket. That last one is why those messages are sentences saying what
// to do next rather than error codes.
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
