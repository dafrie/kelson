package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// The interim auth's tests (#84). What they hold the gate to:
//
//   - No password is the pre-#84 server, byte for byte: everything open, no
//     cookies, and /auth/session saying there is nothing to log in to.
//   - A password gates /kelson.v1alpha1.* and nothing else.
//   - Cookie and bearer are two transports for one secret, and both work.
//   - The session endpoint's three answers are three distinct facts.

const testPassword = "correct horse battery staple"

// gated builds the mux the tests exercise: one protected API route and one open
// health route, behind the gate under test.
func gated(t *testing.T, password string) (*httptest.Server, *Auth) {
	t.Helper()
	auth, err := NewAuth(password)
	if err != nil {
		t.Fatalf("NewAuth: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc(apiPathPrefix+"RenderService/Render", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	auth.Register(mux)

	srv := httptest.NewServer(auth.Middleware(mux))
	t.Cleanup(srv.Close)
	// The client must not follow the cookie jar's own rules by accident: every
	// test states the credential it sends.
	srv.Client().CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return srv, auth
}

func do(t *testing.T, srv *httptest.Server, method, path string, body string, mutate func(*http.Request)) *http.Response {
	t.Helper()
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	} else {
		reader = strings.NewReader("")
	}
	req, err := http.NewRequest(method, srv.URL+path, reader)
	if err != nil {
		t.Fatalf("building %s %s: %v", method, path, err)
	}
	if mutate != nil {
		mutate(req)
	}
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

// TestNoPasswordLeavesEverythingOpen: the pre-#84 posture must be unchanged by
// the existence of a gate that is switched off — no 401 anywhere, and no
// cookie set on anything.
func TestNoPasswordLeavesEverythingOpen(t *testing.T) {
	srv, auth := gated(t, "")
	if auth.Enabled() {
		t.Error("an empty password enabled authentication")
	}
	for _, path := range []string{apiPathPrefix + "RenderService/Render", "/healthz"} {
		res := do(t, srv, http.MethodPost, path, "", nil)
		if res.StatusCode != http.StatusOK {
			t.Errorf("%s with no password configured = %d, want 200", path, res.StatusCode)
		}
		if len(res.Cookies()) != 0 {
			t.Errorf("%s set a cookie with authentication disabled", path)
		}
	}
}

// TestSessionEndpointIsTriState: 204 is "there is nothing to log in to", 200 is
// "you are logged in", 401 is "log in". Three states because a UI that
// conflated the first two would show a login form no password could satisfy.
func TestSessionEndpointIsTriState(t *testing.T) {
	open, _ := gated(t, "")
	if res := do(t, open, http.MethodGet, "/auth/session", "", nil); res.StatusCode != http.StatusNoContent {
		t.Errorf("GET /auth/session with no password = %d, want 204", res.StatusCode)
	}

	srv, _ := gated(t, testPassword)
	if res := do(t, srv, http.MethodGet, "/auth/session", "", nil); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET /auth/session with no cookie = %d, want 401", res.StatusCode)
	}

	cookie := login(t, srv, "ada", testPassword)
	res := do(t, srv, http.MethodGet, "/auth/session", "", func(r *http.Request) { r.AddCookie(cookie) })
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /auth/session with a session = %d, want 200", res.StatusCode)
	}
	if got := decodeField(t, res, "username"); got != "ada" {
		t.Errorf("username = %q, want the name the login carried", got)
	}
}

// login posts a valid login and returns the session cookie it set.
func login(t *testing.T, srv *httptest.Server, username, password string) *http.Cookie {
	t.Helper()
	body, err := json.Marshal(loginRequest{Username: username, Password: password})
	if err != nil {
		t.Fatalf("encoding the login: %v", err)
	}
	res := do(t, srv, http.MethodPost, "/auth/login", string(body), nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("POST /auth/login = %d, want 200", res.StatusCode)
	}
	for _, c := range res.Cookies() {
		if c.Name == SessionCookie {
			return c
		}
	}
	t.Fatalf("a successful login set no %s cookie", SessionCookie)
	return nil
}

func decodeField(t *testing.T, res *http.Response, field string) string {
	t.Helper()
	var body map[string]string
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decoding the response: %v", err)
	}
	return body[field]
}

// TestTheCookieCarriesTheFlagsThatMatter: HttpOnly so script cannot read the
// token, SameSite=Lax so ordinary navigation and the Vite dev proxy keep it,
// Path=/ so every route sees it. Secure is off on plain HTTP because a browser
// would otherwise refuse to store it at all.
func TestTheCookieCarriesTheFlagsThatMatter(t *testing.T) {
	srv, _ := gated(t, testPassword)
	cookie := login(t, srv, "ada", testPassword)

	if !cookie.HttpOnly {
		t.Error("the session cookie is readable by script")
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax", cookie.SameSite)
	}
	if cookie.Path != "/" {
		t.Errorf("Path = %q, want /", cookie.Path)
	}
	if cookie.Secure {
		t.Error("Secure was set on a plain-HTTP connection; the browser would drop the cookie")
	}
	if cookie.MaxAge <= 0 {
		t.Errorf("MaxAge = %d, want a bounded session", cookie.MaxAge)
	}
	if strings.Contains(cookie.Value, testPassword) {
		t.Error("the cookie carries the password itself")
	}
}

// TestSecureIsSetBehindATLSProxy: the deployment this interim posture is for is
// "kelson-server behind a TLS-terminating proxy", and there the cookie must be
// marked Secure.
func TestSecureIsSetBehindATLSProxy(t *testing.T) {
	srv, _ := gated(t, testPassword)
	body, err := json.Marshal(loginRequest{Username: "ada", Password: testPassword})
	if err != nil {
		t.Fatalf("encoding the login: %v", err)
	}
	res := do(t, srv, http.MethodPost, "/auth/login", string(body), func(r *http.Request) {
		r.Header.Set("X-Forwarded-Proto", "https")
	})
	for _, c := range res.Cookies() {
		if c.Name == SessionCookie && !c.Secure {
			t.Error("the cookie was not marked Secure behind a TLS proxy")
		}
	}
}

// TestGateRefusesTheAPIAndOnlyTheAPI: /healthz must answer a probe that holds
// no credential, and /auth/* is how a client stops being unauthenticated — a
// gate over either would be a lock with the key inside.
func TestGateRefusesTheAPIAndOnlyTheAPI(t *testing.T) {
	srv, _ := gated(t, testPassword)

	res := do(t, srv, http.MethodPost, apiPathPrefix+"RenderService/Render", "", nil)
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("an RPC with no credential = %d, want 401", res.StatusCode)
	}
	if got := decodeField(t, res, "code"); got != "unauthenticated" {
		t.Errorf("the refusal's code = %q, want the Connect code a client branches on", got)
	}

	if res := do(t, srv, http.MethodGet, "/healthz", "", nil); res.StatusCode != http.StatusOK {
		t.Errorf("/healthz behind a password = %d, want 200", res.StatusCode)
	}
}

// TestBothTransportsOpenTheGate: one secret, two ways to present it — a cookie
// for browsers, a bearer token for kelson-mcp, curl and anything else without a
// cookie jar.
func TestBothTransportsOpenTheGate(t *testing.T) {
	srv, _ := gated(t, testPassword)
	path := apiPathPrefix + "RenderService/Render"

	cookie := login(t, srv, "ada", testPassword)
	res := do(t, srv, http.MethodPost, path, "", func(r *http.Request) { r.AddCookie(cookie) })
	if res.StatusCode != http.StatusOK {
		t.Errorf("an RPC with a session cookie = %d, want 200", res.StatusCode)
	}

	res = do(t, srv, http.MethodPost, path, "", func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+testPassword)
	})
	if res.StatusCode != http.StatusOK {
		t.Errorf("an RPC with the bearer password = %d, want 200", res.StatusCode)
	}

	// A bearer caller is authenticated and has no display name: a shared
	// password names nobody, and inventing a name here would be a lie the UI
	// would then render.
	res = do(t, srv, http.MethodGet, "/auth/session", "", func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+testPassword)
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /auth/session with a bearer token = %d, want 200", res.StatusCode)
	}
	if got := decodeField(t, res, "username"); got != "" {
		t.Errorf("a bearer caller was given the display name %q", got)
	}
}

// TestWrongCredentialsAreRefused walks every way a request can fail to hold the
// secret. The near-miss cases matter most: a prefix of the password and the
// password with one byte changed must be as wrong as an empty one.
func TestWrongCredentialsAreRefused(t *testing.T) {
	srv, _ := gated(t, testPassword)
	path := apiPathPrefix + "RenderService/Render"

	for _, tt := range []struct {
		name   string
		mutate func(*http.Request)
	}{
		{name: "nothing"},
		{name: "empty bearer", mutate: header("Authorization", "Bearer ")},
		{name: "prefix of the password", mutate: header("Authorization", "Bearer correct horse")},
		{name: "one byte off", mutate: header("Authorization", "Bearer correct horse battery stapleX")},
		{name: "another scheme", mutate: header("Authorization", "Basic "+testPassword)},
		{name: "forged cookie", mutate: func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: SessionCookie, Value: "eyJ1IjoiYWRhIn0.notasignature"})
		}},
		{name: "unsigned cookie", mutate: func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: SessionCookie, Value: "payload"})
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			res := do(t, srv, http.MethodPost, path, "", tt.mutate)
			if res.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s = %d, want 401", tt.name, res.StatusCode)
			}
		})
	}
}

// TestLoginValidatesItsInput: the username is a display name and not an
// identity, but it is still user input that gets signed into a token and echoed
// back, so it gets bounds like any other.
func TestLoginValidatesItsInput(t *testing.T) {
	srv, _ := gated(t, testPassword)

	for _, tt := range []struct {
		name string
		body string
		want int
	}{
		{name: "not json", body: "{", want: http.StatusBadRequest},
		{name: "no username", body: `{"username":"  ","password":"` + testPassword + `"}`, want: http.StatusBadRequest},
		{name: "username too long", body: `{"username":"` + strings.Repeat("a", maxUsername+1) + `","password":"` + testPassword + `"}`, want: http.StatusBadRequest},
		{name: "username with a separator", body: `{"username":"a:b","password":"` + testPassword + `"}`, want: http.StatusBadRequest},
		{name: "wrong password", body: `{"username":"ada","password":"nope"}`, want: http.StatusUnauthorized},
		{name: "right password", body: `{"username":"ada","password":"` + testPassword + `"}`, want: http.StatusOK},
	} {
		t.Run(tt.name, func(t *testing.T) {
			res := do(t, srv, http.MethodPost, "/auth/login", tt.body, nil)
			if res.StatusCode != tt.want {
				t.Errorf("login %s = %d, want %d", tt.name, res.StatusCode, tt.want)
			}
		})
	}

	// Any username is accepted with the right password, and that is the point
	// worth a test rather than a comment: usernames are not identities yet.
	for _, name := range []string{"ada", "grace", "someone-else-entirely"} {
		if cookie := login(t, srv, name, testPassword); cookie == nil {
			t.Errorf("username %q was refused", name)
		}
	}

	if res := do(t, srv, http.MethodGet, "/auth/login", "", nil); res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /auth/login = %d, want 405", res.StatusCode)
	}
}

// TestLoginWithAuthDisabledSaysSo: a UI that posts a login form to a server
// without a password must learn that, not be told its credentials were wrong.
func TestLoginWithAuthDisabledSaysSo(t *testing.T) {
	srv, _ := gated(t, "")
	res := do(t, srv, http.MethodPost, "/auth/login", `{"username":"ada","password":"anything"}`, nil)
	if res.StatusCode != http.StatusNoContent {
		t.Errorf("POST /auth/login with no password configured = %d, want 204", res.StatusCode)
	}
}

// TestLogoutClearsTheCookieAndTheSession.
func TestLogoutClearsTheCookieAndTheSession(t *testing.T) {
	srv, _ := gated(t, testPassword)
	cookie := login(t, srv, "ada", testPassword)

	res := do(t, srv, http.MethodPost, "/auth/logout", "", func(r *http.Request) { r.AddCookie(cookie) })
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("POST /auth/logout = %d, want 204", res.StatusCode)
	}
	cleared := false
	for _, c := range res.Cookies() {
		if c.Name == SessionCookie && c.MaxAge < 0 && c.Value == "" {
			cleared = true
		}
	}
	if !cleared {
		t.Error("logout did not expire the session cookie")
	}
	if res := do(t, srv, http.MethodGet, "/auth/logout", "", nil); res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /auth/logout = %d, want 405", res.StatusCode)
	}
}

// TestSessionsDieWithTheProcess is the honest half of the stateless design
// (ADR-0013 §1): the signing key is derived from a per-process random salt, so
// a restart invalidates every outstanding session. A cookie minted by one
// process must be worthless to the next.
func TestSessionsDieWithTheProcess(t *testing.T) {
	first, _ := gated(t, testPassword)
	cookie := login(t, first, "ada", testPassword)

	second, _ := gated(t, testPassword)
	res := do(t, second, http.MethodGet, "/auth/session", "", func(r *http.Request) { r.AddCookie(cookie) })
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("a cookie from a previous process = %d, want 401", res.StatusCode)
	}
}

// TestExpiredSessionsAreRefused: the token cannot be revoked — there is nothing
// to revoke it in — so its expiry is the only bound that exists and it has to
// be enforced.
func TestExpiredSessionsAreRefused(t *testing.T) {
	auth, err := NewAuth(testPassword)
	if err != nil {
		t.Fatalf("NewAuth: %v", err)
	}
	token := auth.token("ada", time.Now().Add(-time.Second))
	if _, ok := auth.verify(token); ok {
		t.Error("an expired token was accepted")
	}
	if _, ok := auth.verify(auth.token("ada", time.Now().Add(time.Hour))); !ok {
		t.Error("a live token was refused")
	}
}

// TestThePasswordCompareIsConstantTime.
//
// Timing is not observable from a test — a benchmark of two string compares
// measures the scheduler, not the branch — so the guard is on the code: the
// comparison must go through crypto/subtle over fixed-length digests. A future
// edit to `==` would pass every behavioural test in this file and reintroduce
// the leak, which is exactly the regression worth catching mechanically.
func TestThePasswordCompareIsConstantTime(t *testing.T) {
	source, err := os.ReadFile("auth.go")
	if err != nil {
		t.Fatalf("reading auth.go: %v", err)
	}
	if !strings.Contains(string(source), "subtle.ConstantTimeCompare(a.digest[:], got[:])") {
		t.Error("the password compare no longer goes through crypto/subtle over SHA-256 digests")
	}

	// And the behaviour that compare owes: near misses are misses.
	auth, err := NewAuth(testPassword)
	if err != nil {
		t.Fatalf("NewAuth: %v", err)
	}
	for _, wrong := range []string{"", "correct", testPassword + " ", " " + testPassword, strings.ToUpper(testPassword)} {
		if auth.matches(wrong) {
			t.Errorf("password %q was accepted", wrong)
		}
	}
	if !auth.matches(testPassword) {
		t.Error("the right password was refused")
	}
}

func header(name, value string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set(name, value) }
}
