package forgehttp

import (
	"encoding/json"
	"errors"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/dafrie/kelson/internal/model"
	"github.com/dafrie/kelson/internal/redact"
)

// The app-manifest flow (ADR-0033 decision 2). The GitHub end is an httptest
// server serving the one endpoint the exchange calls, reached by pointing the
// flow at it as a GitHub Enterprise host — which is also the only way to test
// the enterprise path, so the fixture and the feature cover each other.

const (
	appPEM        = "-----BEGIN RSA PRIVATE KEY-----\nmanifest-flow-probe-key\n-----END RSA PRIVATE KEY-----\n"
	appHookSecret = "manifest-flow-probe-webhook-secret"
)

func manifestHandler(t *testing.T, conns *fakeConnections, secrets *fakeSecrets, external string) *Handler {
	t.Helper()
	return handlerWith(t, Options{
		Sources:     resolverOver(storeWith()),
		Connections: conns,
		Secrets:     secrets,
		ExternalURL: external,
	})
}

func startFlow(t *testing.T, h *Handler, query string) *httptest.ResponseRecorder {
	t.Helper()
	target := ManifestStartPath
	if query != "" {
		target += "?" + query
	}
	return serve(h, httptest.NewRequest(http.MethodGet, target, nil))
}

// Start renders the auto-submitting form GitHub's manifest flow requires: the
// manifest is a JSON document in a form field and there is no GET spelling of
// it.
func TestManifestStartRendersTheForm(t *testing.T) {
	h := manifestHandler(t, newFakeConnections(), newFakeSecrets(), "https://kelson.acme.com")

	rec := startFlow(t, h, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`method="post"`,
		"https://github.com/settings/apps/new",
		`name="manifest"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the form must contain %q\n%s", want, body)
		}
	}

	manifest := manifestField(t, body)
	if got := manifest["hook_attributes"].(map[string]any)["url"]; got != "https://kelson.acme.com"+WebhookPath {
		t.Errorf("webhook URL = %v, want it derived from the external URL", got)
	}
	if got := manifest["redirect_url"]; got != "https://kelson.acme.com"+ManifestCallbackPath {
		t.Errorf("redirect URL = %v", got)
	}
	if got := manifest["name"]; got != "kelson-kelson-acme-com" {
		t.Errorf("app name = %v, want one derived from this instance's host", got)
	}

	// The state must be both in the registration URL and in a cookie scoped to
	// the forge paths — one alone is not a round trip that can be checked.
	if !strings.Contains(body, "state=") {
		t.Errorf("the registration URL carries no state parameter\n%s", body)
	}
	cookie := stateCookieFrom(t, rec)
	if !cookie.HttpOnly || cookie.Path != PathPrefix {
		t.Errorf("state cookie = %+v, want HttpOnly and scoped to %s", cookie, PathPrefix)
	}
}

// An organisation flow posts to a different path, which the browser cannot
// derive because the github.com/GHE host split lives in internal/forge.
func TestManifestStartHonoursTheOrganisation(t *testing.T) {
	h := manifestHandler(t, newFakeConnections(), newFakeSecrets(), "https://kelson.acme.com")
	rec := startFlow(t, h, "org=acme")
	if !strings.Contains(rec.Body.String(), "https://github.com/organizations/acme/settings/apps/new") {
		t.Errorf("an org flow must post to the organisation's path\n%s", rec.Body.String())
	}
}

// The webhook URL is baked into the created app, so a loopback base URL is
// refused where it costs a sentence rather than at the first delivery that
// never arrives.
func TestManifestStartRefusesALoopbackURL(t *testing.T) {
	h := manifestHandler(t, newFakeConnections(), newFakeSecrets(), "")

	rec := serve(h, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8420"+ManifestStartPath, nil))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want a redirect back to the connections page", rec.Code)
	}
	target := rec.Header().Get("Location")
	if !strings.HasPrefix(target, ConnectionsPath) {
		t.Fatalf("Location = %q, want the connections page", target)
	}
	q := queryOf(t, target)
	if q.Get("error") != "unreachable" {
		t.Errorf("error = %q, want it to say the address is unreachable", q.Get("error"))
	}
	if !strings.Contains(q.Get("message"), "--external-url") {
		t.Errorf("the message must name the way out: %q", q.Get("message"))
	}
}

// The whole round trip: start, follow the state, exchange the code, and check
// what was written.
func TestManifestCallbackWritesTheSecretAndTheConnection(t *testing.T) {
	github := githubStub(t)
	defer github.Close()

	conns, secrets := newFakeConnections(), newFakeSecrets()
	h := manifestHandler(t, conns, secrets, "https://kelson.acme.com")

	start := startFlow(t, h, "host="+url.QueryEscape(github.URL))
	state := stateFromForm(t, start.Body.String())

	rec := callback(t, h, start, "code=abc123&state="+url.QueryEscape(state))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want a redirect: %s", rec.Code, rec.Body.String())
	}
	q := queryOf(t, rec.Header().Get("Location"))
	if q.Get("connected") != "kelson-acme" {
		t.Errorf("connected = %q, want the app's slug", q.Get("connected"))
	}
	if q.Get("error") != "" {
		t.Errorf("the flow reported an error: %v", q)
	}
	// ADR-0033 decision 2 step 3: the app exists, and choosing repositories is
	// still ahead of the user.
	if !strings.Contains(q.Get("install"), "/installations/new") {
		t.Errorf("install = %q, want the installation page", q.Get("install"))
	}

	written, ok := secrets.written["kelson-acme-app"]
	if !ok {
		t.Fatalf("no Secret was written: %v", secrets.written)
	}
	if string(written[model.GitHubAppPrivateKeyKey]) != appPEM {
		t.Error("the private key was not stored")
	}
	if string(written[model.GitHubAppWebhookSecretKey]) != appHookSecret {
		t.Error("the webhook secret was not stored")
	}

	spec, ok := conns.created["kelson-acme"]
	if !ok {
		t.Fatalf("no connection was created: %v", conns.created)
	}
	if spec.Provider != model.GitProviderGitHub {
		t.Errorf("provider = %q", spec.Provider)
	}
	if spec.Auth.GitHubApp == nil || spec.Auth.GitHubApp.AppID != 12345 {
		t.Errorf("auth = %+v, want the created app's ID", spec.Auth)
	}
	if spec.Auth.GitHubApp.SecretRef != "kelson-acme-app" {
		t.Errorf("secretRef = %q, want the Secret that was just written", spec.Auth.GitHubApp.SecretRef)
	}
	// installationID stays zero: the app exists and nobody has installed it,
	// which is the state the `installation` webhook ends.
	if spec.Auth.GitHubApp.InstallationID != 0 {
		t.Errorf("installationID = %d, want the pre-install state", spec.Auth.GitHubApp.InstallationID)
	}
	if spec.Host != github.URL {
		t.Errorf("host = %q, want the enterprise host the flow started against", spec.Host)
	}

	// Nothing GitHub handed back may be printable from here on (issue #117).
	if got := redact.Scrub("key: " + appPEM); strings.Contains(got, "manifest-flow-probe-key") {
		t.Errorf("the app's private key survived redaction: %q", got)
	}
}

// The state parameter is the CSRF binding of the round trip: a callback the
// browser did not start here creates nothing.
func TestManifestCallbackRefusesAMismatchedState(t *testing.T) {
	github := githubStub(t)
	defer github.Close()
	conns, secrets := newFakeConnections(), newFakeSecrets()
	h := manifestHandler(t, conns, secrets, "https://kelson.acme.com")

	start := startFlow(t, h, "host="+url.QueryEscape(github.URL))

	for _, tc := range []struct{ name, query string }{
		{"wrong state", "code=abc123&state=not-the-state"},
		{"no state", "code=abc123"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := callback(t, h, start, tc.query)
			if q := queryOf(t, rec.Header().Get("Location")); q.Get("error") != "state" {
				t.Errorf("error = %q, want a state refusal", q.Get("error"))
			}
			if len(conns.created) != 0 || len(secrets.written) != 0 {
				t.Error("a callback with a bad state created something")
			}
		})
	}
}

// A callback with no cookie at all — a link somebody forwarded — is the same
// refusal, and creates nothing.
func TestManifestCallbackRefusesWithoutTheCookie(t *testing.T) {
	conns, secrets := newFakeConnections(), newFakeSecrets()
	h := manifestHandler(t, conns, secrets, "https://kelson.acme.com")

	rec := serve(h, httptest.NewRequest(http.MethodGet, ManifestCallbackPath+"?code=abc&state=whatever", nil))
	if q := queryOf(t, rec.Header().Get("Location")); q.Get("error") != "state" {
		t.Errorf("error = %q, want a state refusal", q.Get("error"))
	}
	if len(conns.created) != 0 || len(secrets.written) != 0 {
		t.Error("a cookieless callback created something")
	}
}

// The user pressing cancel on GitHub's page comes back with a state and no
// code. Nothing was created and nothing went wrong.
func TestManifestCallbackTreatsAMissingCodeAsCancelled(t *testing.T) {
	github := githubStub(t)
	defer github.Close()
	conns, secrets := newFakeConnections(), newFakeSecrets()
	h := manifestHandler(t, conns, secrets, "https://kelson.acme.com")

	start := startFlow(t, h, "host="+url.QueryEscape(github.URL))
	state := stateFromForm(t, start.Body.String())

	rec := callback(t, h, start, "state="+url.QueryEscape(state))
	if q := queryOf(t, rec.Header().Get("Location")); q.Get("error") != "cancelled" {
		t.Errorf("error = %q, want cancelled", q.Get("error"))
	}
	if len(conns.created) != 0 {
		t.Error("a cancelled flow created a connection")
	}
}

// The app exists on GitHub and its key comes back exactly once, so a failed
// Secret write must say "delete the app" rather than "try again".
func TestManifestCallbackSaysWhatToDoWhenTheKeyCannotBeStored(t *testing.T) {
	github := githubStub(t)
	defer github.Close()
	conns := newFakeConnections()
	secrets := newFakeSecrets()
	secrets.err = errors.New("forbidden")
	h := manifestHandler(t, conns, secrets, "https://kelson.acme.com")

	start := startFlow(t, h, "host="+url.QueryEscape(github.URL))
	state := stateFromForm(t, start.Body.String())
	rec := callback(t, h, start, "code=abc123&state="+url.QueryEscape(state))

	q := queryOf(t, rec.Header().Get("Location"))
	if q.Get("error") != "secret" {
		t.Fatalf("error = %q, want a secret failure", q.Get("error"))
	}
	if !strings.Contains(q.Get("message"), "Delete the app") {
		t.Errorf("the message must say the app is orphaned: %q", q.Get("message"))
	}
	if len(conns.created) != 0 {
		t.Error("a connection was created for a key that was never stored")
	}
}

// A server with no store sends nobody to GitHub: a flow that could not record
// its result would create an app kelson then loses.
func TestManifestStartRefusesWithoutAStore(t *testing.T) {
	h := handlerWith(t, Options{Sources: resolverOver(storeWith()), ExternalURL: "https://kelson.acme.com"})
	rec := startFlow(t, h, "")
	if q := queryOf(t, rec.Header().Get("Location")); q.Get("error") != "unavailable" {
		t.Errorf("error = %q, want unavailable", q.Get("error"))
	}
}

// --- helpers ---------------------------------------------------------------

// githubStub serves the one endpoint ExchangeManifestCode calls, at the GitHub
// Enterprise path (`/api/v3`) the exchange derives for a non-github.com host.
func githubStub(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/app-manifests/abc123/conversions", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":             12345,
			"slug":           "kelson-acme",
			"html_url":       "https://github.example.com/apps/kelson-acme",
			"pem":            appPEM,
			"webhook_secret": appHookSecret,
			"client_id":      "Iv1.abcdef",
			"client_secret":  "cs-secret",
		})
	})
	return httptest.NewServer(mux)
}

// callback replays the cookies the start handler set, which is what a browser
// does and what makes the state check meaningful.
func callback(t *testing.T, h *Handler, start *httptest.ResponseRecorder, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, ManifestCallbackPath+"?"+query, nil)
	for _, c := range start.Result().Cookies() {
		req.AddCookie(c)
	}
	return serve(h, req)
}

func stateCookieFrom(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == stateCookie {
			return c
		}
	}
	t.Fatalf("no %s cookie was set", stateCookie)
	return nil
}

// stateFromForm reads the state out of the registration URL the form posts to,
// which is where GitHub reads it from too.
func stateFromForm(t *testing.T, body string) string {
	t.Helper()
	const marker = `action="`
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatalf("no form action in:\n%s", body)
	}
	action, _, found := strings.Cut(body[i+len(marker):], `"`)
	if !found {
		t.Fatalf("unterminated form action in:\n%s", body)
	}
	// html/template escapes the URL's ampersands.
	u, err := url.Parse(html.UnescapeString(action))
	if err != nil {
		t.Fatalf("parsing the form action %q: %v", action, err)
	}
	state := u.Query().Get("state")
	if state == "" {
		t.Fatalf("no state in the form action %q", action)
	}
	return state
}

// manifestField pulls the manifest back out of the form field and unescapes it,
// which is what the browser hands GitHub.
func manifestField(t *testing.T, body string) map[string]any {
	t.Helper()
	const marker = `name="manifest" value="`
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatalf("no manifest field in:\n%s", body)
	}
	rest := body[i+len(marker):]
	end := strings.Index(rest, `">`)
	if end < 0 {
		t.Fatalf("unterminated manifest field in:\n%s", body)
	}
	raw := html.UnescapeString(rest[:end])
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("decoding the manifest %q: %v", raw, err)
	}
	return out
}

func queryOf(t *testing.T, location string) url.Values {
	t.Helper()
	u, err := url.Parse(location)
	if err != nil {
		t.Fatalf("parsing Location %q: %v", location, err)
	}
	return u.Query()
}
