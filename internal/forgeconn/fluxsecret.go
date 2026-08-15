package forgeconn

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/dafrie/kelson/internal/model"
	"github.com/dafrie/kelson/internal/preview/naming"
)

// The previews credential: which connection an environment's previews
// authenticate with, and the flux-operator-shaped Secret built from it
// ([ADR-0033](docs/adr/0033-git-connections.md) decision 4: "the same
// resolution serves previews … kelson materializes the flux-operator-shaped
// Secret from it").
//
// The two halves are here together because they are one question asked twice.
// [Resolver.ResolvePreviews] picks the connection — the same rule the source
// path uses, with one documented difference for the repository a preview is
// allowed to watch instead — and [PreviewSecretData] turns what it picked into
// the bytes flux-operator reads.
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
// # Nothing here holds a cluster or writes anything
//
// [PreviewSecretData] turns a [Resolution] into bytes; whoever applies them
// decides where they go and what owns them, which is the same split every other
// pure part of kelson draws. The resolution beside it reaches a Secret only
// through the [Store] seam, exactly as [Resolver.Resolve] does.

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

// ResolvePreviews picks the connection an environment's previews authenticate
// with (ADR-0033 decision 4: "the same resolution serves previews").
//
// previewsRepo is `previews.repo` and source is the Project's `source:` block,
// or nil for a project that deploys a pre-built image. It answers through the
// same [Resolver.Resolve] every other caller uses, so an ambiguity is still a
// refusal naming both connections and a missing name is still a refusal naming
// the field — the previews path does not get its own idea of what resolution
// means.
//
// # The one thing that is different, and why
//
// `previews.repo` is not `source.git`. A preview watches the repository the
// pull requests are in, which is usually the source repository and is allowed
// not to be. So the explicit override cannot simply be passed through:
// `source.connection` is a statement about the *source* repository, and
// applying it to a previews repo on another forge would authenticate a GitLab
// poll with a GitHub App because a field two documents away said so.
//
// The rule is therefore: **the override applies when the two repositories are
// on the same forge host, and is ignored otherwise.** Same host is where the
// author's sentence still holds — "read this forge with that connection" — and
// it is the case the field was written for, since the overwhelmingly common
// shape is previews on the very repository the project builds from. A previews
// repo on a different host falls back to host matching against `previews.repo`
// alone, which is exactly what happened before the override was reachable here.
//
// Host granularity rather than repository granularity is deliberate: a project
// whose previews watch a sibling repository on the same forge wants the same
// credential, and requiring the URLs to be equal would make the override useless
// for the monorepo-adjacent case while protecting nothing — a connection is
// scoped to a host to begin with.
func (r *Resolver) ResolvePreviews(ctx context.Context, previewsRepo string, source *model.ResolvedSource) (Resolution, bool, error) {
	return r.Resolve(ctx, previewsRepo, PreviewsOverride(previewsRepo, source))
}

// PreviewsOverride is the `source.connection` the previews path may use for one
// repository, or "" when it may not. It is exported so the rule is testable and
// quotable on its own; [Resolver.ResolvePreviews] documents it.
func PreviewsOverride(previewsRepo string, source *model.ResolvedSource) string {
	if source == nil || source.Connection == "" {
		return ""
	}
	host := remoteHost(previewsRepo)
	if host == "" || host != remoteHost(source.Git) {
		return ""
	}
	return source.Connection
}

// remoteHost is a remote URL's host (with port, lowercased), or "" when there
// is nothing to read.
//
// It accepts the scp-like `git@host:owner/repo` spelling as well as a URL,
// because the two fields being compared are held to different formats:
// `previews.repo` must be HTTP(S) (it is reached through the forge's API) while
// `source.git` is commonly written as an SSH remote. They spell the same forge
// differently, and a comparison that could not see through that would silently
// drop the override for most projects that set one.
func remoteHost(remote string) string {
	raw := strings.TrimSpace(remote)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		if at := strings.Index(raw, "@"); at >= 0 {
			if colon := strings.Index(raw[at:], ":"); colon >= 0 {
				return strings.ToLower(raw[at+1 : at+colon])
			}
		}
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Host)
}

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
