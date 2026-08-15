package forge

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/dafrie/kelson/internal/redact"
)

// GitHub App authentication in two steps: the app proves it is the app with an
// RS256-signed assertion over its private key, and exchanges that for a token
// scoped to one installation. Both are written here rather than imported —
// signing a fixed three-field claim set with crypto/rsa is a dozen lines, and
// a JWT library would bring a parser, a key-loader and an algorithm table for
// an assertion this package only ever *produces* (ADR-0033 decision 3, no
// forge SDK).

const (
	// jwtLifetime is how long the assertion is valid for. GitHub refuses
	// anything more than ten minutes ahead; nine leaves room for the backdate
	// below and for the request itself.
	jwtLifetime = 9 * time.Minute

	// jwtBackdate moves iat into the past. GitHub rejects an assertion issued
	// in its future, and a control plane whose clock is a minute fast would
	// otherwise fail every mint with an error that names the signature rather
	// than the clock.
	jwtBackdate = 60 * time.Second

	// tokenRenewBefore is how long before expiry a cached token stops being
	// handed out. An installation token lives about an hour; a build that
	// takes the token at 59 minutes and clones at 61 fails on a credential
	// that was valid when it was handed over, which is the failure this margin
	// exists to prevent.
	tokenRenewBefore = 5 * time.Minute
)

// appJWT signs the assertion that authenticates the app itself.
//
// The claim set is the whole of what GitHub reads: who is asserting (the app
// id), from when, until when. now is a parameter so the shape is testable
// against a fixed clock.
func appJWT(c Conn, now time.Time) (string, error) {
	if c.AppID == 0 {
		return "", fmt.Errorf("forge/github: the connection has app key material but no app id: %w", ErrAuthFailed)
	}
	key, err := parsePrivateKey(c.PrivateKeyPEM)
	if err != nil {
		return "", err
	}

	header, err := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	if err != nil {
		return "", fmt.Errorf("forge/github: encoding the assertion header: %w", err)
	}
	claims, err := json.Marshal(map[string]any{
		"iat": now.Add(-jwtBackdate).Unix(),
		"exp": now.Add(jwtLifetime).Unix(),
		// A string rather than a number: GitHub takes either, and a string
		// cannot be mangled by a JSON reader that decodes every number as a
		// float.
		"iss": strconv.FormatInt(c.AppID, 10),
	})
	if err != nil {
		return "", fmt.Errorf("forge/github: encoding the assertion claims: %w", err)
	}

	signing := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signing))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("forge/github: signing the app assertion: %w", err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

// parsePrivateKey reads the PEM GitHub hands back from the manifest exchange.
//
// It accepts both encodings because the file a user downloads from the app
// settings page is PKCS#1 ("RSA PRIVATE KEY") while key material that has been
// through a conversion tool is often PKCS#8 ("PRIVATE KEY"), and refusing the
// second would be a support question with no diagnostic value.
func parsePrivateKey(material []byte) (*rsa.PrivateKey, error) {
	if len(material) == 0 {
		return nil, fmt.Errorf("forge/github: the connection has no app private key: %w", ErrAuthFailed)
	}
	rest := material
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		switch block.Type {
		case "RSA PRIVATE KEY":
			key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("forge/github: the app private key is not a usable RSA key: %w", err)
			}
			return key, nil
		case "PRIVATE KEY":
			parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("forge/github: the app private key is not a usable key: %w", err)
			}
			key, ok := parsed.(*rsa.PrivateKey)
			if !ok {
				return nil, fmt.Errorf("forge/github: the app private key is %T, and GitHub app assertions are RS256", parsed)
			}
			return key, nil
		}
	}
	return nil, fmt.Errorf("forge/github: the app private key is not PEM-encoded key material: %w", ErrAuthFailed)
}

// tokenCacheKey identifies one installation's token. The host is part of it
// because app ids are only unique within a forge: a github.com app and a
// GitHub Enterprise app can both be app 1, and a cache that conflated them
// would hand one host's token to the other.
func tokenCacheKey(c Conn) string {
	return fmt.Sprintf("%s|%d/%d", c.hostOrDefault(), c.AppID, c.InstallationID)
}

// installationToken returns a token for the connection's installation, minting
// one if the cached token is missing or close enough to expiry to be unsafe to
// hand out.
func (g *gitHubProvider) installationToken(ctx context.Context, c Conn) (Credential, error) {
	if c.InstallationID == 0 {
		return Credential{}, fmt.Errorf("forge/github: %s has no installation id yet: %w", c, ErrNotInstalled)
	}
	key := tokenCacheKey(c)

	g.mu.Lock()
	cached, ok := g.tokens[key]
	g.mu.Unlock()
	if ok && g.now().Before(cached.ExpiresAt.Add(-tokenRenewBefore)) {
		return cached, nil
	}

	base, err := apiBase(c.Host)
	if err != nil {
		return Credential{}, err
	}
	assertion, err := appJWT(c, g.now())
	if err != nil {
		return Credential{}, err
	}

	u := base + "/app/installations/" + strconv.FormatInt(c.InstallationID, 10) + "/access_tokens"
	data, _, err := g.send(ctx, http.MethodPost, u, "Bearer "+assertion, nil)
	if err != nil {
		// A 404 here is GitHub's answer for an installation this app cannot
		// see: revoked, suspended, or transferred away. The key is fine.
		return Credential{}, notInstalled(err)
	}

	var minted struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(data, &minted); err != nil {
		return Credential{}, fmt.Errorf("forge/github: decoding the installation token response: %w", err)
	}
	if minted.Token == "" {
		return Credential{}, fmt.Errorf("forge/github: %s minted an empty installation token: %w", c, ErrAuthFailed)
	}

	cred := Credential{Username: defaultTokenUsername, Password: minted.Token, ExpiresAt: minted.ExpiresAt}
	// The moment the process learns the value is the moment it becomes
	// unprintable (ADR-0033 decision 2; issue #117). Callers projecting it
	// into a build namespace register it too — this is the net underneath.
	redact.Register(cred.SecretValues()...)

	g.mu.Lock()
	g.tokens[key] = cred
	g.mu.Unlock()
	return cred, nil
}
