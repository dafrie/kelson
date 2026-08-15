package forgehttp

import (
	"crypto/hmac"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/dafrie/kelson/internal/controlstore"
	"github.com/dafrie/kelson/internal/forge"
	"github.com/dafrie/kelson/internal/model"
)

// The GitHub App manifest flow's two ends (ADR-0033 decision 2). What sits
// between them is GitHub's: the user approves creating the app on their account
// or an organisation, and GitHub redirects back with a one-time code.
//
// # The state parameter, and what it is and is not
//
// GitHub round-trips `state` from the registration URL to the callback. kelson
// generates one per flow, keeps it in a cookie scoped to /forge/, and compares
// the two in constant time. That makes the callback unforgeable by a third
// party — a code delivered to a browser that did not start a flow here is
// refused — which is CSRF protection for the round trip.
//
// It is not authentication of the person taking it. See this package's doc for
// why, and for what would have to change in internal/api to make it one.
//
// The cookie carries the forge host and the app name beside the state, because
// the callback needs both to complete the exchange and neither is in GitHub's
// redirect. Keeping them in the cookie rather than in server memory is what
// makes the flow survive a restart and a second replica, which is the property
// ADR-0013 §1 asks of everything in this process.

const (
	// stateCookie holds the flow's state, host and app name. It is scoped to
	// the forge paths so it is not sent with anything else this server serves.
	stateCookie = "kelson_forge_manifest"

	// stateTTL is how long a started flow may take. Fifteen minutes is longer
	// than approving an app takes and far shorter than GitHub's own one-hour
	// code expiry, so the cookie is never the reason a flow fails.
	stateTTL = 15 * time.Minute
)

// flowState is what the cookie carries.
type flowState struct {
	// State is the value GitHub echoes back.
	State string `json:"s"`
	// Host is the forge this flow was started against, e.g. https://github.com
	// or a GitHub Enterprise Server URL.
	Host string `json:"h"`
	// Name is the app name that was submitted, so the connection can be named
	// after it if GitHub reports no slug.
	Name string `json:"n"`
	// Expires is a unix timestamp.
	Expires int64 `json:"e"`
}

// manifestStart handles GET /forge/github/manifest/start.
//
// It answers with an auto-submitting form rather than a redirect because
// GitHub's manifest flow is a POST: the manifest is a JSON document in a form
// field, and there is no GET spelling of it. The form is the documented shape,
// and the `noscript` submit button below is what makes the page work for
// somebody who has scripting off rather than leaving them on a blank screen.
func (h *Handler) manifestStart(w http.ResponseWriter, r *http.Request) {
	if h.opts.Connections == nil || h.opts.Secrets == nil {
		redirectToConnections(w, r, map[string]string{
			"error": "unavailable",
			"message": "this server cannot complete the GitHub App flow: it has no connection store to record the " +
				"result in, so it would send you to GitHub to create an app kelson then loses",
		})
		return
	}

	base := h.baseURL(r)
	if err := reachable(base); err != nil {
		redirectToConnections(w, r, map[string]string{"error": "unreachable", "message": err.Error()})
		return
	}

	host := strings.TrimSpace(r.URL.Query().Get("host"))
	if host == "" {
		host = model.DefaultGitHubHost
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		name = appName(base)
	}

	manifest := forge.AppManifest{
		Name:        name,
		URL:         base,
		WebhookURL:  base + WebhookPath,
		RedirectURL: base + ManifestCallbackPath,
		Description: "kelson deploys from this account's repositories: previews, commit statuses and private clones.",
	}
	payload, err := manifest.JSON()
	if err != nil {
		redirectToConnections(w, r, map[string]string{"error": "manifest", "message": err.Error()})
		return
	}

	state, err := randomState()
	if err != nil {
		redirectToConnections(w, r, map[string]string{"error": "state", "message": "could not generate a state parameter"})
		return
	}
	target, err := forge.ManifestRegistrationURL(host, r.URL.Query().Get("org"), state)
	if err != nil {
		redirectToConnections(w, r, map[string]string{"error": "host", "message": err.Error()})
		return
	}
	h.setState(w, r, flowState{State: state, Host: host, Name: name, Expires: h.now().Add(stateTTL).Unix()})

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// A page that posts a credentialless manifest to GitHub and nothing else;
	// html/template escapes both values, and the only dynamic parts are a URL
	// this package built and a JSON document internal/forge built.
	_ = manifestFormTemplate.Execute(w, map[string]string{
		"Action":   target,
		"Manifest": string(payload),
	})
}

var manifestFormTemplate = template.Must(template.New("manifest").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>Connecting to GitHub…</title></head>
<body>
<p>Sending you to GitHub to create kelson's app…</p>
<form id="manifest" method="post" action="{{.Action}}">
  <input type="hidden" name="manifest" value="{{.Manifest}}">
  <noscript><button type="submit">Continue to GitHub</button></noscript>
</form>
<script>document.getElementById("manifest").submit();</script>
</body></html>
`))

// manifestCallback handles GET /forge/github/manifest/callback?code=…&state=….
//
// Everything it does is in one order and nothing in it is retryable with the
// same code — GitHub's conversion code is single-use and expires in an hour —
// so each failure sends the user back to /connections with a reason rather than
// leaving them on an error page they cannot act on.
func (h *Handler) manifestCallback(w http.ResponseWriter, r *http.Request) {
	if h.opts.Connections == nil || h.opts.Secrets == nil {
		redirectToConnections(w, r, map[string]string{"error": "unavailable", "message": "this server cannot record a connection"})
		return
	}

	state, ok := h.takeState(w, r)
	if !ok {
		redirectToConnections(w, r, map[string]string{
			"error":   "state",
			"message": "this callback did not come from a flow this browser started here; start again from Connect GitHub",
		})
		return
	}
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	if code == "" {
		// The user cancelled on GitHub's page, which is not a failure worth a
		// message beyond saying nothing was created.
		redirectToConnections(w, r, map[string]string{"error": "cancelled", "message": "no app was created"})
		return
	}

	creds, err := forge.ExchangeManifestCode(r.Context(), state.Host, code)
	if err != nil {
		h.log.Warn("the github app manifest exchange failed", "host", state.Host, "error", err.Error())
		redirectToConnections(w, r, map[string]string{
			"error":   "exchange",
			"message": "GitHub refused the one-time code; the code is single-use and expires an hour after the redirect, so start again",
		})
		return
	}

	connection := connectionName(creds, state.Name)
	secretName := connection + "-app"
	if err := h.opts.Secrets.Write(r.Context(), secretName, map[string][]byte{
		model.GitHubAppPrivateKeyKey:    creds.PEM,
		model.GitHubAppWebhookSecretKey: creds.WebhookSecret,
	}); err != nil {
		// The app exists on GitHub and its key is now unrecoverable — GitHub
		// hands the PEM back exactly once — so the message says to delete it
		// rather than to retry into a second orphan.
		h.log.Error("could not write the github app secret", "secret", secretName, "error", err.Error())
		redirectToConnections(w, r, map[string]string{
			"error": "secret",
			"message": "the app was created on GitHub but kelson could not store its key, which GitHub hands back only " +
				"once. Delete the app on GitHub and connect again",
		})
		return
	}

	spec := model.GitConnectionSpec{
		Provider: model.GitProviderGitHub,
		Auth: model.GitConnectionAuth{GitHubApp: &model.GitHubAppAuth{
			AppID: creds.AppID,
			// No installation yet: the app exists and nobody has installed it.
			// That is the intermediate state ADR-0033 decision 2 step 3
			// describes, and the `installation` webhook is what ends it.
			SecretRef: secretName,
		}},
	}
	if !strings.EqualFold(state.Host, model.DefaultGitHubHost) {
		spec.Host = state.Host
	}
	if _, err := h.opts.Connections.Create(r.Context(), connection, spec, controlstore.CreateConnectionOptions{}); err != nil {
		h.log.Error("could not create the git connection", "connection", connection, "error", err.Error())
		redirectToConnections(w, r, map[string]string{
			"error":   "connection",
			"message": fmt.Sprintf("the app was created on GitHub and its key was stored, but the connection %q could not be written: %v", connection, err),
		})
		return
	}

	h.log.Info("connected a github app", "connection", connection, "app", creds.AppID, "slug", creds.Slug)
	// Back to /connections, where the new connection is a row. `install` is the
	// URL the user still has to visit to choose repositories — ADR-0033 decision
	// 2 step 3, the half of the ceremony that happens after the app exists. The
	// UI slice that turns it into a button reads this parameter; until then the
	// connection shows as not yet reachable, which is the honest state.
	redirectToConnections(w, r, map[string]string{
		"connected": connection,
		"install":   installURL(creds),
	})
}

// appName is what the created app is called on GitHub.
//
// GitHub requires it to be unique across the whole forge, which is why it
// carries the instance rather than being "kelson" (internal/forge's AppManifest
// says so). The instance is its own hostname, which is the one string that is
// both stable and already unique to this deployment; `?name=` overrides it for
// the case where it is taken anyway.
func appName(base string) string {
	host := base
	if u, err := url.Parse(base); err == nil && u.Host != "" {
		host = u.Host
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	var b strings.Builder
	b.WriteString("kelson-")
	lastDash := true
	for _, r := range strings.ToLower(host) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastDash = false
		case !lastDash:
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}

// connectionName is what the GitConnection is called. GitHub's slug is already
// a DNS-safe lowercase string and is what the app is known as on the forge, so
// using it keeps `kubectl get gitconnections` and the GitHub settings page
// naming the same thing.
func connectionName(creds forge.AppCredentials, fallback string) string {
	for _, candidate := range []string{creds.Slug, fallback} {
		if name := sanitizeName(candidate); name != "" {
			return name
		}
	}
	return "github"
}

func sanitizeName(s string) string {
	var b strings.Builder
	lastDash := true
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastDash = false
		case !lastDash:
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.TrimSuffix(b.String(), "-")
	if len(out) > 63 {
		out = strings.TrimSuffix(out[:63], "-")
	}
	return out
}

// installURL is where the user picks repositories. GitHub reports the app's
// page; its installation entry point is that page plus /installations/new.
func installURL(creds forge.AppCredentials) string {
	if creds.HTMLURL == "" {
		return ""
	}
	return strings.TrimRight(creds.HTMLURL, "/") + "/installations/new"
}

// reachable refuses to mint a manifest whose webhook URL GitHub cannot deliver
// to.
//
// The webhook URL is baked into the created app, and an app created against
// http://localhost:8420 is an app that has to be deleted and made again — the
// user cannot fix it from kelson. So a loopback base URL is refused here, where
// it costs a sentence, rather than at the first delivery that never arrives.
// The flag named in the message is the way through for a tunnelled development
// setup, which is the case this would otherwise block.
func reachable(base string) error {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return fmt.Errorf("kelson could not work out the URL it is reachable at (%q); set --external-url", base)
	}
	host := u.Hostname()
	if host == "localhost" || net.ParseIP(host).IsLoopback() {
		return fmt.Errorf("this server thinks it is reachable at %s, and GitHub cannot deliver webhooks to a "+
			"loopback address. The webhook URL is baked into the app GitHub creates, so an app made now would have "+
			"to be deleted and remade: set --external-url (or KELSON_EXTERNAL_URL) to the address GitHub can reach "+
			"— a tunnel's URL is fine — and connect again", base)
	}
	return nil
}

// randomState is 32 bytes of crypto/rand, base64url without padding.
func randomState() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func (h *Handler) now() time.Time {
	if h.opts.Now != nil {
		return time.Unix(h.opts.Now(), 0)
	}
	return time.Now()
}

// setState writes the flow cookie. HttpOnly because no script needs it,
// SameSite=Lax because the callback arrives as a top-level navigation from
// GitHub and a Strict cookie would not be sent with it, and Secure whenever the
// request itself was — the same rule the session cookie follows.
func (h *Handler) setState(w http.ResponseWriter, r *http.Request, state flowState) {
	payload, err := json.Marshal(state)
	if err != nil {
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     stateCookie,
		Value:    base64.RawURLEncoding.EncodeToString(payload),
		Path:     PathPrefix,
		HttpOnly: true,
		Secure:   r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(stateTTL / time.Second),
	})
}

// takeState reads the flow cookie, clears it, and checks it against the state
// GitHub echoed back. A flow is single-use: the cookie is cleared whether or
// not it matched, so a replayed callback has nothing left to match against.
func (h *Handler) takeState(w http.ResponseWriter, r *http.Request) (flowState, bool) {
	http.SetCookie(w, &http.Cookie{Name: stateCookie, Path: PathPrefix, MaxAge: -1, HttpOnly: true})

	cookie, err := r.Cookie(stateCookie)
	if err != nil {
		return flowState{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(cookie.Value)
	if err != nil {
		return flowState{}, false
	}
	var state flowState
	if err := json.Unmarshal(raw, &state); err != nil || state.State == "" {
		return flowState{}, false
	}
	if state.Expires != 0 && h.now().After(time.Unix(state.Expires, 0)) {
		return flowState{}, false
	}
	// Constant-time, because the comparison is against a value an attacker
	// supplies and must not leak how far it got.
	if !hmac.Equal([]byte(state.State), []byte(r.URL.Query().Get("state"))) {
		return flowState{}, false
	}
	return state, true
}
