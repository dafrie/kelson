package api

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Interim authentication: one shared password, two transports (issue #84).
//
// This is the interim cut of #84, taken with the project owner on 2026-08-13,
// and it is deliberately smaller than the design #84 owns. The rule it
// implements:
//
//   - Someone with a kube context already has RBAC-mediated access to the
//     cluster, so the CLI needs nothing: it talks to the cluster directly and
//     this file does not touch it.
//   - Click/web users get a normal username + password login against a single
//     shared password. The username is accepted, echoed and displayed; it is
//     NOT an identity and nothing authorizes on it. Project-level team OIDC is
//     the real answer and it is later.
//
// # Sessions are signed tokens, not server state
//
// ADR-0013 §1 says the process holds no state a restart or a second replica
// would lose or fork, and a session table would be exactly that. So a session
// is an HMAC-SHA256 token the server can verify without remembering it: the
// signing key is derived from the password and a per-process random salt, which
// means every restart mints a new key and every outstanding session dies with
// the old one. That is honest for v0 — a logged-in user is asked to log in
// again after a server restart, and nothing pretends otherwise — and it is why
// there is no revocation list, no store and no cleanup goroutine here.
//
// # One secret, two transports
//
// Browsers get the token in an HttpOnly, SameSite=Lax cookie. Non-browser
// clients — kelson-mcp, curl, a script — send `Authorization: Bearer <password>`
// instead: the shared password used directly as a bearer token. Two transports
// for one secret rather than a second credential nobody could rotate
// independently anyway.
//
// # What this is not
//
// It is not TLS, not per-user identity, not authorization, and not a defence
// against anyone who can read the process's environment or command line. It
// raises the floor from "anything that can reach the port is the operator" to
// "a caller must hold the shared secret", and #84 still owns the real answer.

// SessionCookie is the cookie a browser session travels in.
const SessionCookie = "kelson_session"

// DefaultSessionTTL bounds one login. It is shorter than any real working day
// on purpose: the token cannot be revoked (there is nothing to revoke it in),
// so its lifetime is the only bound that exists.
const DefaultSessionTTL = 12 * time.Hour

// apiPathPrefix is what every generated ConnectRPC route shares
// (`/<package>.<Service>/<Method>`). The middleware gates on it positively
// rather than gating everything and exempting: /healthz must answer a probe
// that holds no secret, and the UI's own static assets must load before there
// is any way to log in.
const apiPathPrefix = "/kelson.v1alpha1."

// maxLoginBody caps the login request. The body is two short strings; anything
// larger is a mistake or an attempt to make the server allocate.
const maxLoginBody = 4 << 10

// maxUsername bounds the display name. It is echoed into a header and signed
// into a token, so it gets a length like every other user-supplied string.
const maxUsername = 64

// Auth is the interim password gate. The zero value is a disabled gate, which
// is what a server started without --password gets: [Auth.Middleware] passes
// everything through and the /auth endpoints report "there is nothing to log
// in to".
type Auth struct {
	enabled bool
	// digest is SHA-256 of the password. Comparing fixed-length digests in
	// constant time keeps the compare from leaking the password's length, which
	// a direct subtle.ConstantTimeCompare of the raw strings would.
	digest [sha256.Size]byte
	// key signs session tokens: HMAC-SHA256(random per-process salt, password).
	// Deriving it from both means neither the password alone nor a stolen token
	// from a previous process is enough to mint a valid one.
	key []byte
	ttl time.Duration
	now func() time.Time
}

// NewAuth builds the gate. An empty password disables it, reproducing the
// pre-#84 behaviour exactly: every route open, no cookies, no login screen.
func NewAuth(password string) (*Auth, error) {
	if password == "" {
		return &Auth{now: time.Now}, nil
	}
	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("generating the session signing salt: %w", err)
	}
	mac := hmac.New(sha256.New, salt)
	mac.Write([]byte(password)) //nolint:errcheck // hash.Hash never returns an error
	return &Auth{
		enabled: true,
		digest:  sha256.Sum256([]byte(password)),
		key:     mac.Sum(nil),
		ttl:     DefaultSessionTTL,
		now:     time.Now,
	}, nil
}

// Enabled reports whether a password was configured.
func (a *Auth) Enabled() bool { return a.enabled }

// Register mounts the three session endpoints. They are mounted whether or not
// a password is set, because "is authentication on?" is a question the UI has
// to be able to ask — a 404 would be indistinguishable from an old server.
func (a *Auth) Register(mux *http.ServeMux) {
	mux.HandleFunc("/auth/login", a.login)
	mux.HandleFunc("/auth/logout", a.logout)
	mux.HandleFunc("/auth/session", a.session)
}

// Middleware gates the API routes on a session cookie or a bearer password.
// Everything else — /healthz, /auth/*, static assets — passes through.
//
// The rejection is written as a Connect error envelope so both connect-go and
// connect-es decode it as CodeUnauthenticated rather than as an opaque HTTP
// failure: a client that must tell "log in again" from "the server broke"
// branches on the code, not on prose.
func (a *Auth) Middleware(next http.Handler) http.Handler {
	if !a.enabled {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, apiPathPrefix) {
			next.ServeHTTP(w, r)
			return
		}
		if _, ok := a.authenticate(r); ok {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"code": "unauthenticated",
			"message": "kelson-server requires a session: log in at /auth/login, " +
				"or send Authorization: Bearer <password> from a non-browser client",
		})
	})
}

// authenticate resolves a request to a display name. The bool is the answer;
// the string is only ever a label, and a bearer-authenticated caller has none
// because a shared password names nobody.
func (a *Auth) authenticate(r *http.Request) (string, bool) {
	if a.bearer(r) {
		return "", true
	}
	cookie, err := r.Cookie(SessionCookie)
	if err != nil {
		return "", false
	}
	return a.verify(cookie.Value)
}

// bearer reports whether the request carries the shared password as a bearer
// token.
func (a *Auth) bearer(r *http.Request) bool {
	header := r.Header.Get("Authorization")
	const scheme = "bearer "
	if len(header) <= len(scheme) || !strings.EqualFold(header[:len(scheme)], scheme) {
		return false
	}
	return a.matches(strings.TrimSpace(header[len(scheme):]))
}

// matches is the password check, in constant time over fixed-length digests.
func (a *Auth) matches(candidate string) bool {
	got := sha256.Sum256([]byte(candidate))
	return subtle.ConstantTimeCompare(a.digest[:], got[:]) == 1
}

// token mints a session: base64url(payload) "." base64url(HMAC(payload)).
// The payload carries the display name and the expiry, so verification needs
// nothing the server would have to remember.
func (a *Auth) token(username string, expires time.Time) string {
	payload := base64.RawURLEncoding.EncodeToString(
		[]byte(strconv.FormatInt(expires.Unix(), 10) + ":" + username))
	return payload + "." + base64.RawURLEncoding.EncodeToString(a.sign(payload))
}

func (a *Auth) sign(payload string) []byte {
	mac := hmac.New(sha256.New, a.key)
	mac.Write([]byte(payload)) //nolint:errcheck // hash.Hash never returns an error
	return mac.Sum(nil)
}

// verify checks the signature first and the expiry second: an unsigned token's
// claims are not worth parsing.
func (a *Auth) verify(token string) (string, bool) {
	payload, signature, found := strings.Cut(token, ".")
	if !found {
		return "", false
	}
	got, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil || !hmac.Equal(a.sign(payload), got) {
		return "", false
	}
	claims, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return "", false
	}
	expiry, username, found := strings.Cut(string(claims), ":")
	if !found {
		return "", false
	}
	unix, err := strconv.ParseInt(expiry, 10, 64)
	if err != nil || a.now().After(time.Unix(unix, 0)) {
		return "", false
	}
	return username, true
}

// loginRequest is the browser's side of a login. The username is accepted and
// echoed; it is not checked, because there is one password and it belongs to
// nobody in particular (see this file's header).
type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (a *Auth) login(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		authError(w, http.StatusMethodNotAllowed, "POST a JSON {username, password} to /auth/login")
		return
	}
	if !a.enabled {
		// Auth disabled: the same 204 /auth/session gives, so a UI that posts a
		// login form to a server without a password learns that rather than
		// being told its credentials were wrong.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var req loginRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxLoginBody)).Decode(&req); err != nil {
		authError(w, http.StatusBadRequest, "the request body is not JSON {username, password}")
		return
	}
	username := strings.TrimSpace(req.Username)
	switch {
	case username == "":
		authError(w, http.StatusBadRequest, "a username is required; it is a display name, not an identity")
		return
	case len(username) > maxUsername:
		authError(w, http.StatusBadRequest, "the username is too long")
		return
	case strings.ContainsAny(username, ":\r\n"):
		// ":" separates the token's claims and the newlines would land in a
		// header; a display name needs none of the three.
		authError(w, http.StatusBadRequest, "the username may not contain ':' or a line break")
		return
	case !a.matches(req.Password):
		authError(w, http.StatusUnauthorized, "wrong password")
		return
	}

	expires := a.now().Add(a.ttl)
	http.SetCookie(w, &http.Cookie{
		Name:  SessionCookie,
		Value: a.token(username, expires),
		Path:  "/",
		// HttpOnly: script must not be able to read the token; the UI never
		// needs to, it asks /auth/session instead.
		HttpOnly: true,
		// Lax rather than Strict so the Vite dev proxy (ui/vite.config.ts) and
		// ordinary top-level navigation to the UI both keep the cookie, and
		// rather than None because nothing here is meant to be embedded
		// cross-site.
		SameSite: http.SameSiteLaxMode,
		Secure:   overTLS(r),
		Expires:  expires,
		MaxAge:   int(a.ttl / time.Second),
	})
	writeJSON(w, http.StatusOK, map[string]string{"username": username})
}

func (a *Auth) logout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		authError(w, http.StatusMethodNotAllowed, "POST to /auth/logout")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   overTLS(r),
		MaxAge:   -1,
	})
	w.WriteHeader(http.StatusNoContent)
}

// session is the tri-state the UI boots on:
//
//	204 No Content — authentication is disabled; there is nothing to log in to.
//	200 {"username"} — this request carries a valid session.
//	401 — a login is required.
//
// Three states rather than two because "no password configured" and "not logged
// in" are different facts and a UI that conflated them would show a login form
// no password could satisfy. A bearer-authenticated caller answers 200 with an
// empty username: it is authenticated and it has no display name.
func (a *Auth) session(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		authError(w, http.StatusMethodNotAllowed, "GET /auth/session")
		return
	}
	if !a.enabled {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	username, ok := a.authenticate(r)
	if !ok {
		authError(w, http.StatusUnauthorized, "no session")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"username": username})
}

// overTLS reports whether the connection reaching the client is encrypted, so
// the cookie is marked Secure behind a TLS-terminating proxy and is not marked
// Secure on plain loopback HTTP, where the browser would then refuse to store
// it. The forwarded header is a client claim, but a client that lies about it
// only breaks its own session.
func overTLS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func authError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, body map[string]string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// A body that cannot be written cannot be reported to the client it was for.
	_ = json.NewEncoder(w).Encode(body)
}
