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

	"github.com/dafrie/kelson/internal/controlstore"
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
// # Agent tokens are the third credential, and the only one that is a principal
//
// Issue #74 added agent identities beside the two transports above: a bearer
// header beginning `kagt.` is resolved against the agent store instead of being
// compared to the password, and the request is attributed to that identity
// (principal.go) for the authorization interceptor and the audit line to read.
// The human path is untouched — an agent token is a different prefix, a
// different lookup and a different principal type, and revoking one cannot
// affect a password or a session.
//
// Two properties of that lookup are load-bearing. It happens on *every* request,
// so a revocation is immediate and no allow decision is ever cached. And it is
// performed here, at the edge, so exactly one place in the process turns a
// credential into a principal.
//
// An agent token is honoured even on a server with no password. That is not a
// security boundary — a caller on such a server can simply omit the header and
// be anonymous — but it means an agent's scope behaves identically in a local
// dev server and in production, which is where scope bugs would otherwise hide.
//
// # One check, two mount points
//
// [Auth.Middleware] gates `/kelson.v1alpha1.*` and passes everything else, so a
// handler mounted beside the RPCs is ungated unless it asks. The GitHub App
// manifest flow's session endpoint is one (internal/forgehttp, ADR-0033
// decision 2, issue #248), and [Auth.CheckRequest] is its ask: the same
// credential order, the same principals, the same code as an RPC gets. Exported
// for that single purpose, and deliberately the only per-request check this
// package exports — a foreign handler that had to re-derive "is this caller
// authenticated" would be a second answer to a question with one.
//
// # What this is not
//
// It is not TLS, not per-user identity for humans, and not a defence against
// anyone who can read the process's environment or command line. It raises the
// floor from "anything that can reach the port is the operator" to "a caller
// must hold a credential, and an agent's credential says which agent", and #84
// still owns the human half of the real answer.

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
	// agents resolves agent bearer tokens (issue #74). Nil means this server
	// has no agent principals, and a `kagt.` token is then refused with a
	// message saying so rather than silently treated as a wrong password.
	agents AgentStore
}

// AuthOptions configures the gate.
type AuthOptions struct {
	// Password is the shared secret of #84's interim cut. Empty disables the
	// human gate.
	Password string
	// Agents is the agent identity store. Nil disables agent principals.
	Agents AgentStore
	// Now is the clock expiry is checked against. Nil selects time.Now.
	Now func() time.Time
}

// NewAuth builds the password-only gate. An empty password disables it,
// reproducing the pre-#84 behaviour exactly: every route open, no cookies, no
// login screen.
func NewAuth(password string) (*Auth, error) {
	return NewAuthWith(AuthOptions{Password: password})
}

// NewAuthWith builds the gate over every credential the server accepts.
func NewAuthWith(opts AuthOptions) (*Auth, error) {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	if opts.Password == "" {
		return &Auth{now: now, agents: opts.Agents}, nil
	}
	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("generating the session signing salt: %w", err)
	}
	mac := hmac.New(sha256.New, salt)
	mac.Write([]byte(opts.Password)) //nolint:errcheck // hash.Hash never returns an error
	return &Auth{
		enabled: true,
		digest:  sha256.Sum256([]byte(opts.Password)),
		key:     mac.Sum(nil),
		ttl:     DefaultSessionTTL,
		now:     now,
		agents:  opts.Agents,
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
	if !a.enabled && a.agents == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, apiPathPrefix) {
			next.ServeHTTP(w, r)
			return
		}
		p, message := a.principal(r)
		if message != "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"code":    "unauthenticated",
				"message": message,
			})
			return
		}
		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
	})
}

// CheckRequest applies the RPC routes' credential check to one request that is
// not an RPC, for a handler mounted outside [Auth.Middleware]'s prefix.
//
// The returned string is a refusal message, and is empty exactly when the
// request carries a credential this server accepts — including the case where
// no password and no agent store are configured, where an RPC is open and so is
// this. A caller writes the message as the body of its own refusal, in whatever
// shape its surface speaks.
//
// It returns no principal on purpose. Nothing outside this package authorizes,
// and handing a foreign handler an identity it cannot put through the scope
// table (scope.go) would invite an authorization decision written somewhere
// this package could not see.
func (a *Auth) CheckRequest(r *http.Request) string {
	_, message := a.principal(r)
	return message
}

// noCredential is the refusal a request with nothing usable gets. It names all
// three credentials rather than only the one the server happens to have,
// because a caller that sent the wrong kind needs to know the right kind exists.
const noCredential = "kelson-server requires a credential: log in at /auth/login, " +
	"send Authorization: Bearer <password> from a non-browser client, " +
	"or send Authorization: Bearer <agent token> as an agent identity (issue #74)"

// principal resolves a request to its caller. The string is a refusal message
// and is empty exactly when the Principal is usable.
//
// The order is deliberate. A bearer header is inspected first and its *prefix*
// decides which credential it is, so a mistyped password is never tried as an
// agent token and a revoked agent token is never reported as a wrong password.
func (a *Auth) principal(r *http.Request) (Principal, string) {
	if token, present := bearerToken(r); present {
		if strings.HasPrefix(token, controlstore.AgentTokenPrefix) {
			return a.agentPrincipal(r, token)
		}
		switch {
		case a.enabled && a.matches(token):
			// A shared password names nobody, so the principal has no name.
			return Principal{Type: PrincipalHuman}, ""
		case a.enabled:
			return Principal{}, noCredential
		default:
			return Principal{Type: PrincipalAnonymous}, ""
		}
	}
	if a.enabled {
		cookie, err := r.Cookie(SessionCookie)
		if err == nil {
			if username, ok := a.verify(cookie.Value); ok {
				return Principal{Type: PrincipalHuman, Name: username}, ""
			}
		}
		return Principal{}, noCredential
	}
	return Principal{Type: PrincipalAnonymous}, ""
}

// agentPrincipal resolves an agent token against the store, on this request,
// with no cache anywhere: that is what makes revocation immediate.
//
// Expiry and revocation are refused here rather than passed on as a live
// principal, because both are facts about the credential rather than about what
// it was asking to do — an expired token is not authenticated, it is stale.
func (a *Auth) agentPrincipal(r *http.Request, token string) (Principal, string) {
	if a.agents == nil {
		return Principal{}, "this kelson-server has no agent identity store, so agent tokens cannot be used against it; " +
			"it was started without one (issue #74)"
	}
	agent, err := a.agents.Authenticate(r.Context(), token)
	if err != nil {
		// Every failure is one message on purpose: which of "no such identity",
		// "malformed" and "wrong secret" happened is an enumeration oracle and
		// changes nothing a legitimate caller does.
		return Principal{}, "the agent token does not identify a live agent identity; " +
			"ask an operator to issue one with `kelson agent create`"
	}
	switch {
	case agent.Revoked:
		return Principal{}, "this agent identity was revoked; ask an operator for a new one with `kelson agent create`"
	case agent.Expired(a.now()):
		return Principal{}, "this agent credential has expired; rotation is `kelson agent create` for the successor, " +
			"then `kelson agent revoke` for this one"
	}
	return Principal{Type: PrincipalAgent, Name: agent.Name, Agent: agent}, ""
}

// bearerToken returns the Authorization header's bearer value.
func bearerToken(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	const scheme = "bearer "
	if len(header) <= len(scheme) || !strings.EqualFold(header[:len(scheme)], scheme) {
		return "", false
	}
	return strings.TrimSpace(header[len(scheme):]), true
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
//	200 {"username", "principal"} — this request carries a valid credential.
//	401 — a login is required.
//
// Three states rather than two because "no password configured" and "not logged
// in" are different facts and a UI that conflated them would show a login form
// no password could satisfy. A bearer-authenticated caller answers 200 with an
// empty username: it is authenticated and it has no display name.
//
// `principal` is additive (issue #74): it says human or agent, so a person
// debugging a token can see which identity the server resolved. The UI reads
// `username` and is unaffected.
func (a *Auth) session(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		authError(w, http.StatusMethodNotAllowed, "GET /auth/session")
		return
	}
	if !a.enabled && a.agents == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	p, message := a.principal(r)
	if message != "" || p.Type == PrincipalAnonymous {
		if !a.enabled {
			// No password: there is still nothing to log in to, whatever the
			// agent store says.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		authError(w, http.StatusUnauthorized, "no session")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"username":  p.Name,
		"principal": string(p.Type),
	})
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
