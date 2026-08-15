package forgeconn

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/dafrie/kelson/internal/model"
	"github.com/dafrie/kelson/internal/preview/naming"
)

// The flux-operator-shaped Secret a `previews:` block needs, built from a
// connection ([ADR-0033](docs/adr/0033-git-connections.md) decision 4: "kelson
// materializes the flux-operator-shaped Secret from it").
//
// # Two shapes, and the difference is staleness
//
// flux-operator's ResourceSetInputProvider reads its `secretRef` in one of two
// vocabularies, and they are not two spellings of one thing:
//
//   - **[ShapeGitHubApp]** — `githubAppID`, `githubAppInstallationID`,
//     `githubAppPrivateKey`, and `githubAppBaseURL` for a GitHub Enterprise
//     host. flux-operator holds the *app key* and mints its own installation
//     tokens on the schedule it polls on, so the Secret never expires and
//     kelson never has to refresh it. This is why an app connection is the
//     preferred one: the staleness problem does not exist rather than being
//     managed.
//   - **[ShapeBasicAuth]** — `username` and `password`. It is the only shape a
//     token connection has, and the value is the user's standing credential:
//     it changes when they rotate the Secret kelson read it from, and not on a
//     clock.
//
// An app connection with no installation recorded yet has no shape at all
// ([ErrNoInstallation]): the app exists and nobody has installed it, which is a
// state the `installation` webhook ends and not a Secret to write half of.
//
// # This function holds no cluster and writes nothing
//
// It turns a [Resolution] into bytes. Whoever applies them decides where they
// go and what owns them, which is the same split every other pure part of
// kelson draws.

// Shape names which of flux-operator's two vocabularies a Secret was written
// in. It is returned rather than inferred by the caller because the answer is
// what a status line says out loud: "previews authenticate as the GitHub App"
// and "previews authenticate with a token" are different facts about how long
// the credential lives.
type Shape string

const (
	// ShapeGitHubApp is the githubApp* key set. flux-operator mints its own
	// tokens from it, so nothing kelson wrote can go stale.
	ShapeGitHubApp Shape = "githubApp"
	// ShapeBasicAuth is username/password. The value is the stored token.
	ShapeBasicAuth Shape = "basicAuth"
)

// The keys flux-operator (and Flux's own git providers) read. They are spelled
// here once, because the writer of this Secret and the operator that reads it
// have to agree and there is no shared type to make them.
const (
	FluxUsernameKey                = "username"
	FluxPasswordKey                = "password"
	FluxGitHubAppIDKey             = "githubAppID"
	FluxGitHubAppInstallationIDKey = "githubAppInstallationID"
	FluxGitHubAppPrivateKeyKey     = "githubAppPrivateKey"
	FluxGitHubAppBaseURLKey        = "githubAppBaseURL"
)

// ErrNoInstallation is an app connection GitHub has not been installed for
// yet. It is a distinct condition rather than an error string because the
// remedy is a specific one — install the app and pick repositories — and the
// caller reports it rather than retrying.
var ErrNoInstallation = fmt.Errorf("the connection's GitHub App is not installed yet, so it can mint nothing")

// PreviewSecretName is what kelson calls the Secret it materializes for an
// environment's previews.
//
// It is [naming.Lifecycle] — the same `<project>-<environment>-previews` the
// ResourceSetInputProvider and ResourceSet already carry — because a preview's
// objects should be one `kubectl get -n <ns> | grep previews` away from each
// other, and because the name is then derivable by everything that needs it
// rather than stored anywhere.
func PreviewSecretName(project, environment string) string {
	return naming.Lifecycle(project, environment)
}

// PreviewSecretData builds the Secret's contents from a resolved connection.
//
// The bootstrap connection is deliberately usable here: a `KELSON_GIT_TOKEN`
// instance gets working previews out of the same materialization every other
// connection gets, which is the point of presenting it as a connection at all
// (ADR-0033 decision 5).
func PreviewSecretData(res Resolution) (map[string][]byte, Shape, error) {
	conn := res.Conn
	if conn.AppID != 0 && len(conn.PrivateKeyPEM) > 0 {
		if conn.InstallationID == 0 {
			return nil, "", fmt.Errorf("connection %q: %w", res.Name(), ErrNoInstallation)
		}
		data := map[string][]byte{
			FluxGitHubAppIDKey:             []byte(strconv.FormatInt(conn.AppID, 10)),
			FluxGitHubAppInstallationIDKey: []byte(strconv.FormatInt(conn.InstallationID, 10)),
			FluxGitHubAppPrivateKeyKey:     conn.PrivateKeyPEM,
		}
		if base := githubAPIBase(conn.Host); base != "" {
			data[FluxGitHubAppBaseURLKey] = []byte(base)
		}
		return data, ShapeGitHubApp, nil
	}

	if conn.Token == "" {
		return nil, "", fmt.Errorf("connection %q carries no credential previews could authenticate with", res.Name())
	}
	username := conn.Username
	if username == "" {
		// Forges ignore the value beside a token but require one; the same
		// default gitref.Token and the build plane's clone Secret apply.
		username = "x-access-token"
	}
	return map[string][]byte{
		FluxUsernameKey: []byte(username),
		FluxPasswordKey: []byte(conn.Token),
	}, ShapeBasicAuth, nil
}

// githubAPIBase is the REST base URL for a GitHub Enterprise Server host, or ""
// for github.com.
//
// It is empty for github.com because flux-operator defaults to api.github.com
// and writing it out would be stating a default as though it were a choice. The
// `/api/v3` suffix is the same derivation internal/forge's apiBase makes, and
// it is repeated rather than exported because that package must not grow a
// dependency on what flux-operator wants.
func githubAPIBase(host string) string {
	h := strings.TrimSpace(host)
	if h == "" || strings.EqualFold(h, model.DefaultGitHubHost) {
		return ""
	}
	if !strings.Contains(h, "://") {
		h = "https://" + h
	}
	u, err := url.Parse(h)
	if err != nil || u.Host == "" {
		return ""
	}
	switch strings.ToLower(u.Hostname()) {
	case "github.com", "www.github.com", "api.github.com":
		return ""
	}
	return u.Scheme + "://" + u.Host + strings.TrimSuffix(u.Path, "/") + "/api/v3"
}
