package forge

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Nothing in this package's tests touches a network: every GitHub call goes to
// an httptest server named by Conn.Host, which is also what exercises the
// Enterprise branch of the API-base derivation — a loopback host is a
// self-hosted host as far as apiBase is concerned.

// testKey is generated once for the whole test binary. Signing is what the app
// half of this package does, so the tests need a real RSA key rather than a
// fixture, and one key for every test keeps the cost to a single keygen.
var testKey = sync.OnceValue(func() *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic("generating the test key: " + err.Error())
	}
	return key
})

// testKeyPEM is the PKCS#1 encoding, which is the one GitHub hands out.
func testKeyPEM(t *testing.T) []byte {
	t.Helper()
	return pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(testKey()),
	})
}

func testKeyPKCS8PEM(t *testing.T) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(testKey())
	if err != nil {
		t.Fatalf("marshalling the test key as PKCS#8: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

// counter counts handler invocations. A handler runs on its own goroutine per
// request, so the count is guarded rather than left to the race detector's
// good will.
type counter struct {
	mu sync.Mutex
	n  int
}

func (c *counter) inc() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
}

func (c *counter) get() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// fixedClock is a settable clock for the token cache's expiry window.
type fixedClock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock(t time.Time) *fixedClock { return &fixedClock{now: t} }

func (c *fixedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fixedClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// appConn is a connection with GitHub App material, pointed at a test server.
func appConn(t *testing.T, host string) Conn {
	t.Helper()
	return Conn{
		Provider:       nameGitHub,
		Host:           host,
		AppID:          12345,
		InstallationID: 42,
		PrivateKeyPEM:  testKeyPEM(t),
		WebhookSecret:  []byte("a-webhook-secret"),
	}
}

// tokenConn is a connection with token auth, pointed at a test server.
func tokenConn(host string) Conn {
	return Conn{Provider: nameGitHub, Host: host, Token: "a-personal-access-token"}
}

// requireAuth fails the test unless the request carries the expected
// Authorization header, and asserts the two headers every call must send.
func requireHeaders(t *testing.T, r *http.Request) {
	t.Helper()
	if got := r.Header.Get("User-Agent"); got != userAgent {
		t.Errorf("User-Agent = %q, want %q", got, userAgent)
	}
	if got := r.Header.Get("Accept"); got != acceptJSON {
		t.Errorf("Accept = %q, want %q", got, acceptJSON)
	}
	if got := r.Header.Get("X-GitHub-Api-Version"); got != apiVersion {
		t.Errorf("X-GitHub-Api-Version = %q, want %q", got, apiVersion)
	}
}

func TestFor(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		want     string
		wantOK   bool
	}{
		{name: "github", provider: "github", want: nameGitHub, wantOK: true},
		{name: "generic", provider: "generic", want: nameGeneric, wantOK: true},
		{name: "case and space are tolerated", provider: "  GitHub ", want: nameGitHub, wantOK: true},
		{name: "an unwritten adapter is not silently generic", provider: "gitlab"},
		{name: "empty is not a provider", provider: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, ok := For(tt.provider)
			if ok != tt.wantOK {
				t.Fatalf("For(%q) ok = %v, want %v", tt.provider, ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if p.Name() != tt.want {
				t.Errorf("For(%q).Name() = %q, want %q", tt.provider, p.Name(), tt.want)
			}
		})
	}
}

// The capability split is the seam's whole point, so it is asserted rather
// than assumed: GitHub answers every type assertion, generic answers none.
func TestCapabilities(t *testing.T) {
	github, _ := For("github")
	generic, _ := For("generic")

	if _, ok := github.(RepoBrowser); !ok {
		t.Error("the github adapter must browse repositories")
	}
	if _, ok := github.(WebhookSource); !ok {
		t.Error("the github adapter must verify and parse webhooks")
	}
	if _, ok := github.(StatusReporter); !ok {
		t.Error("the github adapter must report statuses")
	}
	if _, ok := generic.(RepoBrowser); ok {
		t.Error("the generic adapter must not claim a repository picker it cannot serve")
	}
	if _, ok := generic.(WebhookSource); ok {
		t.Error("the generic adapter must not claim webhook verification it cannot perform")
	}
	if _, ok := generic.(StatusReporter); ok {
		t.Error("the generic adapter must not claim status write-back it cannot perform")
	}
}

func TestAPIBase(t *testing.T) {
	tests := []struct {
		name    string
		host    string
		want    string
		wantErr bool
	}{
		{name: "empty means github.com", host: "", want: dotComAPI},
		{name: "github.com moves hostname", host: "https://github.com", want: dotComAPI},
		{name: "trailing slash", host: "https://github.com/", want: dotComAPI},
		{name: "no scheme", host: "github.com", want: dotComAPI},
		{name: "www", host: "https://www.github.com", want: dotComAPI},
		{name: "the api host itself", host: "https://api.github.com", want: dotComAPI},
		{name: "enterprise serves /api/v3", host: "https://ghe.acme.example", want: "https://ghe.acme.example/api/v3"},
		{name: "enterprise on a subpath", host: "https://acme.example/github/", want: "https://acme.example/github/api/v3"},
		{name: "enterprise on a port", host: "http://127.0.0.1:8080", want: "http://127.0.0.1:8080/api/v3"},
		{name: "not a URL", host: "://nope", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := apiBase(tt.host)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("apiBase(%q) = %q, want an error", tt.host, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("apiBase(%q): %v", tt.host, err)
			}
			if got != tt.want {
				t.Errorf("apiBase(%q) = %q, want %q", tt.host, got, tt.want)
			}
		})
	}
}

func TestSplitFullName(t *testing.T) {
	tests := []struct {
		name    string
		full    string
		wantErr bool
	}{
		{name: "owner and repo", full: "acme/checkout"},
		{name: "leading slash is tolerated", full: "/acme/checkout"},
		{name: "three segments are not a repository", full: "acme/checkout/tree", wantErr: true},
		{name: "one segment is not a repository", full: "checkout", wantErr: true},
		{name: "traversal is not a repository", full: "acme/../../app", wantErr: true},
		{name: "empty", full: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := splitFullName(tt.full)
			if (err != nil) != tt.wantErr {
				t.Fatalf("splitFullName(%q) error = %v, wantErr %v", tt.full, err, tt.wantErr)
			}
		})
	}
}

// A Conn or a Credential formatted with %v must not print key material. This
// is the envelope half of the redaction rule (ADR-0009); the registration in
// internal/redact is the net underneath it.
func TestStringersHideSecrets(t *testing.T) {
	c := appConn(t, "https://github.com")
	c.Token = "a-personal-access-token"
	rendered := c.String()
	for _, secret := range []string{"BEGIN RSA PRIVATE KEY", "a-webhook-secret", "a-personal-access-token"} {
		if strings.Contains(rendered, secret) {
			t.Errorf("Conn.String() leaked %q: %s", secret, rendered)
		}
	}

	cred := Credential{Username: "x-access-token", Password: "ghs_secret", ExpiresAt: time.Unix(0, 0).UTC()}
	if strings.Contains(cred.String(), "ghs_secret") {
		t.Errorf("Credential.String() leaked the password: %s", cred.String())
	}
	if got := cred.SecretValues(); len(got) != 1 || got[0] != "ghs_secret" {
		t.Errorf("Credential.SecretValues() = %v, want just the password", got)
	}
	if got := (Credential{Username: "x"}).SecretValues(); len(got) != 0 {
		t.Errorf("an empty credential has nothing to register, got %v", got)
	}
}

func TestNextLink(t *testing.T) {
	tests := []struct {
		name    string
		current string
		link    string
		want    string
	}{
		{
			name:    "next is followed",
			current: "https://api.github.com/user/repos?page=1",
			link:    `<https://api.github.com/user/repos?page=2>; rel="next", <https://api.github.com/user/repos?page=9>; rel="last"`,
			want:    "https://api.github.com/user/repos?page=2",
		},
		{
			name:    "last page has no next",
			current: "https://api.github.com/user/repos?page=9",
			link:    `<https://api.github.com/user/repos?page=1>; rel="first", <https://api.github.com/user/repos?page=8>; rel="prev"`,
		},
		{
			name:    "a next link to another host is not followed",
			current: "https://api.github.com/user/repos?page=1",
			link:    `<https://elsewhere.example/user/repos?page=2>; rel="next"`,
		},
		{name: "no header", current: "https://api.github.com/user/repos"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			if tt.link != "" {
				h.Set("Link", tt.link)
			}
			if got := nextLink(h, tt.current); got != tt.want {
				t.Errorf("nextLink() = %q, want %q", got, tt.want)
			}
		})
	}
}

// A forge that answers every page with a next link must not spin forever.
func TestPaginateIsBounded(t *testing.T) {
	var calls counter
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.inc()
		w.Header().Set("Link", `<http://`+r.Host+r.URL.Path+`?page=next>; rel="next"`)
		_, _ = w.Write([]byte(`[]`))
	}))
	defer server.Close()

	g := newGitHub()
	if err := g.paginate(t.Context(), server.URL+"/loop", "Bearer x", func([]byte) error { return nil }); err != nil {
		t.Fatalf("paginate: %v", err)
	}
	if got := calls.get(); got != maxPages {
		t.Errorf("paginate made %d requests, want it capped at %d", got, maxPages)
	}
}
