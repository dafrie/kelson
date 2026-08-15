package forge

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// decodedJWT is an assertion taken apart far enough to assert on. The tests
// verify the signature rather than only the shape: an assertion GitHub would
// reject is not caught by checking that it has three dot-separated segments.
type decodedJWT struct {
	header struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
	}
	claims struct {
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
		Iss string `json:"iss"`
	}
}

func decodeJWT(t *testing.T, assertion string) decodedJWT {
	t.Helper()
	parts := strings.Split(assertion, ".")
	if len(parts) != 3 {
		t.Fatalf("assertion has %d segments, want 3: %q", len(parts), assertion)
	}
	var out decodedJWT
	for i, into := range []any{&out.header, &out.claims} {
		raw, err := base64.RawURLEncoding.DecodeString(parts[i])
		if err != nil {
			t.Fatalf("segment %d is not base64url: %v", i, err)
		}
		if err := json.Unmarshal(raw, into); err != nil {
			t.Fatalf("segment %d is not the JSON it claims: %v", i, err)
		}
	}

	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("the signature is not base64url: %v", err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&testKey().PublicKey, crypto.SHA256, digest[:], signature); err != nil {
		t.Fatalf("the assertion does not verify against the app's public key: %v", err)
	}
	return out
}

// The assertion is what proves kelson is the app, so its shape is asserted
// field by field: GitHub rejects an expiry more than ten minutes out and an
// issue time in its own future, and both failures arrive as an unhelpful 401.
func TestAppJWT(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

	t.Run("shape", func(t *testing.T) {
		assertion, err := appJWT(appConn(t, ""), now)
		if err != nil {
			t.Fatalf("appJWT: %v", err)
		}
		got := decodeJWT(t, assertion)

		if got.header.Alg != "RS256" {
			t.Errorf("alg = %q, want RS256", got.header.Alg)
		}
		if got.header.Typ != "JWT" {
			t.Errorf("typ = %q, want JWT", got.header.Typ)
		}
		if got.claims.Iss != "12345" {
			t.Errorf("iss = %q, want the app id as a string", got.claims.Iss)
		}
		if want := now.Add(-jwtBackdate).Unix(); got.claims.Iat != want {
			t.Errorf("iat = %d, want %d (backdated against clock skew)", got.claims.Iat, want)
		}
		if want := now.Add(jwtLifetime).Unix(); got.claims.Exp != want {
			t.Errorf("exp = %d, want %d", got.claims.Exp, want)
		}
		if lifetime := time.Duration(got.claims.Exp-got.claims.Iat) * time.Second; lifetime > 10*time.Minute {
			t.Errorf("the assertion is valid for %s; GitHub refuses more than 10m from iat", lifetime)
		}
	})

	t.Run("a PKCS#8 key is accepted too", func(t *testing.T) {
		c := appConn(t, "")
		c.PrivateKeyPEM = testKeyPKCS8PEM(t)
		assertion, err := appJWT(c, now)
		if err != nil {
			t.Fatalf("appJWT with a PKCS#8 key: %v", err)
		}
		if got := decodeJWT(t, assertion); got.claims.Iss != "12345" {
			t.Errorf("iss = %q, want 12345", got.claims.Iss)
		}
	})
}

func TestAppJWTRejects(t *testing.T) {
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating an EC key: %v", err)
	}
	ecDER, err := x509.MarshalPKCS8PrivateKey(ecKey)
	if err != nil {
		t.Fatalf("marshalling the EC key: %v", err)
	}
	ecPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ecDER})

	tests := []struct {
		name       string
		mutate     func(*Conn)
		wantAuthIs bool
	}{
		{
			name:       "key material but no app id",
			mutate:     func(c *Conn) { c.AppID = 0 },
			wantAuthIs: true,
		},
		{
			name:       "no key at all",
			mutate:     func(c *Conn) { c.PrivateKeyPEM = nil },
			wantAuthIs: true,
		},
		{
			name:       "not PEM",
			mutate:     func(c *Conn) { c.PrivateKeyPEM = []byte("-- not a key --") },
			wantAuthIs: true,
		},
		{
			name: "PEM that is not a key",
			mutate: func(c *Conn) {
				c.PrivateKeyPEM = []byte("-----BEGIN RSA PRIVATE KEY-----\nZm9v\n-----END RSA PRIVATE KEY-----\n")
			},
		},
		{
			name:   "an EC key cannot sign an RS256 assertion",
			mutate: func(c *Conn) { c.PrivateKeyPEM = ecPEM },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := appConn(t, "")
			tt.mutate(&c)
			assertion, err := appJWT(c, time.Now())
			if err == nil {
				t.Fatalf("appJWT = %q, want an error", assertion)
			}
			if tt.wantAuthIs && !errors.Is(err, ErrAuthFailed) {
				t.Errorf("error = %v, want it to wrap ErrAuthFailed", err)
			}
			for _, secret := range []string{"BEGIN RSA PRIVATE KEY", "a-webhook-secret"} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("the error leaked %q: %v", secret, err)
				}
			}
		})
	}
}

// mintServer answers the installation-token endpoint with a token that expires
// an hour out, counting how many times it was asked.
//
// It dates the expiry from the same clock the provider reads. A server using
// the wall clock while the provider is on a fixed one would put every freshly
// minted token inside the renewal margin, and the cache test would fail on the
// harness rather than on the cache.
func mintServer(t *testing.T, calls *counter, now func() time.Time, expiresIn time.Duration) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/app/installations/42/access_tokens" {
			t.Errorf("mint path = %q, want the installation's access_tokens endpoint", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Method != http.MethodPost {
			t.Errorf("mint method = %q, want POST", r.Method)
		}
		requireHeaders(t, r)

		// The mint is authenticated by the app assertion, not by a token: if
		// this ever sends something else, every mint fails against real GitHub.
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if bearer == r.Header.Get("Authorization") {
			t.Errorf("Authorization = %q, want a Bearer assertion", r.Header.Get("Authorization"))
		} else if got := decodeJWT(t, bearer); got.claims.Iss != "12345" {
			t.Errorf("the mint was authenticated as app %q, want 12345", got.claims.Iss)
		}

		calls.inc()
		expiry := now().Add(expiresIn).UTC().Format(time.RFC3339)
		_, _ = fmt.Fprintf(w, `{"token":"ghs_minted-%d","expires_at":%q}`, calls.get(), expiry)
	}))
	t.Cleanup(server.Close)
	return server
}

// The cache is what keeps a burst of builds from minting a token each. Its
// expiry window is the other half: a token handed out at the last minute of its
// life is a clone that fails after the credential was accepted.
func TestInstallationTokenCache(t *testing.T) {
	clock := newClock(time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC))
	var calls counter
	server := mintServer(t, &calls, clock.Now, time.Hour)

	g := newGitHub()
	g.now = clock.Now
	c := appConn(t, server.URL)

	first, err := g.MintCloneCredential(t.Context(), c, "https://example.test/acme/checkout")
	if err != nil {
		t.Fatalf("first mint: %v", err)
	}
	if first.Username != defaultTokenUsername {
		t.Errorf("username = %q, want %q", first.Username, defaultTokenUsername)
	}
	if first.Password != "ghs_minted-1" {
		t.Errorf("password = %q, want the minted token", first.Password)
	}
	if first.ExpiresAt.IsZero() {
		t.Error("a minted installation token must carry its expiry; zero means non-expiring")
	}

	steps := []struct {
		name      string
		advance   time.Duration
		wantCalls int
		wantToken string
	}{
		{name: "a second ask is served from the cache", wantCalls: 1, wantToken: "ghs_minted-1"},
		{name: "still cached well inside the window", advance: 50 * time.Minute, wantCalls: 1, wantToken: "ghs_minted-1"},
		{name: "re-minted once inside the renewal margin", advance: 6 * time.Minute, wantCalls: 2, wantToken: "ghs_minted-2"},
		{name: "and cached again", wantCalls: 2, wantToken: "ghs_minted-2"},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			clock.advance(step.advance)
			got, err := g.MintCloneCredential(t.Context(), c, "https://example.test/acme/checkout")
			if err != nil {
				t.Fatalf("mint: %v", err)
			}
			if calls.get() != step.wantCalls {
				t.Errorf("the forge was asked %d times, want %d", calls.get(), step.wantCalls)
			}
			if got.Password != step.wantToken {
				t.Errorf("token = %q, want %q", got.Password, step.wantToken)
			}
		})
	}
}

// App ids are only unique within a forge, so two connections that agree on
// every number but the host must not share a cache entry.
func TestInstallationTokenCacheIsPerHost(t *testing.T) {
	var callsA, callsB counter
	serverA := mintServer(t, &callsA, time.Now, time.Hour)
	serverB := mintServer(t, &callsB, time.Now, time.Hour)

	g := newGitHub()
	if _, err := g.MintCloneCredential(t.Context(), appConn(t, serverA.URL), "acme/checkout"); err != nil {
		t.Fatalf("minting against the first host: %v", err)
	}
	if _, err := g.MintCloneCredential(t.Context(), appConn(t, serverB.URL), "acme/checkout"); err != nil {
		t.Fatalf("minting against the second host: %v", err)
	}
	if callsA.get() != 1 || callsB.get() != 1 {
		t.Errorf("mints per host = %d/%d, want one each — the cache conflated two forges", callsA.get(), callsB.get())
	}
}

func TestInstallationTokenErrors(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		conn    func(Conn) Conn
		wantIs  error
		wantMsg string
	}{
		{
			name:   "no installation recorded yet",
			conn:   func(c Conn) Conn { c.InstallationID = 0; return c },
			wantIs: ErrNotInstalled,
		},
		{
			name:   "the installation is gone",
			status: http.StatusNotFound,
			body:   `{"message":"Not Found"}`,
			wantIs: ErrNotInstalled,
		},
		{
			name:   "the key is rejected",
			status: http.StatusUnauthorized,
			body:   `{"message":"A JSON web token could not be decoded"}`,
			wantIs: ErrAuthFailed,
		},
		{
			name:   "the app is suspended",
			status: http.StatusForbidden,
			body:   `{"message":"This installation has been suspended"}`,
			wantIs: ErrAuthFailed,
		},
		{
			name:   "an empty token is not a credential",
			status: http.StatusCreated,
			body:   `{"token":"","expires_at":"2026-08-15T13:00:00Z"}`,
			wantIs: ErrAuthFailed,
		},
		{
			name:    "a body that is not the documented shape",
			status:  http.StatusCreated,
			body:    `not json`,
			wantMsg: "decoding the installation token response",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()

			c := appConn(t, server.URL)
			if tt.conn != nil {
				c = tt.conn(c)
			}
			_, err := newGitHub().MintCloneCredential(t.Context(), c, "acme/checkout")
			if err == nil {
				t.Fatal("want an error")
			}
			if tt.wantIs != nil && !errors.Is(err, tt.wantIs) {
				t.Errorf("error = %v, want it to wrap %v", err, tt.wantIs)
			}
			if tt.wantMsg != "" && !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("error = %v, want it to mention %q", err, tt.wantMsg)
			}
		})
	}
}

// The three shapes MintCloneCredential answers, at the seam rather than inside
// the app half: an app connection mints, a token connection hands back what it
// stores, and a connection carrying neither fails as an auth problem rather
// than a nil credential the caller discovers at clone time.
func TestMintCloneCredential(t *testing.T) {
	var calls counter
	server := mintServer(t, &calls, time.Now, time.Hour)

	t.Run("app auth mints", func(t *testing.T) {
		got, err := newGitHub().MintCloneCredential(t.Context(), appConn(t, server.URL), "acme/checkout")
		if err != nil {
			t.Fatalf("MintCloneCredential: %v", err)
		}
		if got.ExpiresAt.IsZero() {
			t.Error("a minted credential expires; the caller re-mints on that")
		}
	})

	t.Run("token auth returns the stored token", func(t *testing.T) {
		got, err := newGitHub().MintCloneCredential(t.Context(), tokenConn("https://github.com"), "acme/checkout")
		if err != nil {
			t.Fatalf("MintCloneCredential: %v", err)
		}
		if got.Password != "a-personal-access-token" {
			t.Errorf("password = %q, want the stored token", got.Password)
		}
		if got.Username != defaultTokenUsername {
			t.Errorf("username = %q, want the gitref default %q", got.Username, defaultTokenUsername)
		}
		if !got.ExpiresAt.IsZero() {
			t.Error("a stored token does not rotate itself; ExpiresAt must stay zero")
		}
	})

	t.Run("a username on the connection wins", func(t *testing.T) {
		c := tokenConn("https://github.com")
		c.Username = "acme-bot"
		got, err := newGitHub().MintCloneCredential(t.Context(), c, "acme/checkout")
		if err != nil {
			t.Fatalf("MintCloneCredential: %v", err)
		}
		if got.Username != "acme-bot" {
			t.Errorf("username = %q, want the connection's", got.Username)
		}
	})

	t.Run("no credential at all", func(t *testing.T) {
		_, err := newGitHub().MintCloneCredential(t.Context(), Conn{Provider: nameGitHub}, "https://github.com/acme/checkout")
		if !errors.Is(err, ErrAuthFailed) {
			t.Fatalf("error = %v, want ErrAuthFailed", err)
		}
		if !strings.Contains(err.Error(), "acme/checkout") {
			t.Errorf("error = %v, want it to name the repository the user recognises", err)
		}
	})
}
