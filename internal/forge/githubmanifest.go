package forge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/dafrie/kelson/internal/redact"
)

// The GitHub App manifest flow (ADR-0033 decision 2): the reason "Connect
// GitHub" does not ask anyone for a token.
//
// The instance describes the app it wants, the user approves creating it on
// their own account or organisation, and GitHub hands back a one-time code
// that [ExchangeManifestCode] turns into the app's own credentials. Every
// instance ends up with its *own* app — the private key never leaves the
// instance that minted it, and no central relay exists to be trusted or to go
// down.
//
// Only the two ends of the flow live here. What sits between them — rendering
// the form that POSTs the manifest, carrying `state` across the redirect,
// writing the result into a Secret and a GitConnection — belongs to the server
// that has a browser and cluster access.

// AppManifest is the app kelson asks GitHub to create.
//
// The permission set is not configurable, and that is the decision rather than
// an oversight: it is the least privilege that makes ADR-0033 and ADR-0034
// work — read the source, read metadata, write the preview's comment, write
// the commit status and check. An installation granted less silently loses
// features, which ADR-0034 decision 5 requires the connection to display
// honestly; an installation granted more would be kelson asking for write
// access to source it only ever reads.
type AppManifest struct {
	// Name is what the app is called on GitHub, e.g. "kelson-acme". GitHub
	// requires it to be unique across the whole forge, which is why it carries
	// the instance's name rather than being "kelson".
	Name string
	// URL is the homepage GitHub shows on the app's page — the instance's own
	// URL, so an admin looking at the app in a year can tell which deployment
	// created it.
	URL string
	// WebhookURL is where deliveries go: <server>/forge/github/webhook.
	WebhookURL string
	// RedirectURL is where GitHub sends the browser with the one-time code:
	// <server>/forge/github/manifest/callback.
	RedirectURL string
	// Description is optional prose on the app's page.
	Description string
}

// manifestPermissions and manifestEvents are ADR-0033 decision 2's list,
// spelled the way GitHub's manifest schema spells them.
var (
	manifestPermissions = map[string]string{
		"contents":      "read",
		"metadata":      "read",
		"pull_requests": "write",
		"statuses":      "write",
		"checks":        "write",
	}
	manifestEvents = []string{"push", "pull_request"}
)

// JSON renders the manifest GitHub's registration form takes as its `manifest`
// field.
//
// `public: false` is fixed: a per-instance app is for the account that created
// it, and a public one would offer itself for installation by strangers.
func (m AppManifest) JSON() ([]byte, error) {
	if strings.TrimSpace(m.Name) == "" {
		return nil, fmt.Errorf("forge/github: an app manifest needs a name")
	}
	if strings.TrimSpace(m.WebhookURL) == "" {
		return nil, fmt.Errorf("forge/github: an app manifest needs a webhook URL, or the instance receives no deliveries")
	}
	if strings.TrimSpace(m.RedirectURL) == "" {
		return nil, fmt.Errorf("forge/github: an app manifest needs a redirect URL to receive the one-time code on")
	}
	type hookAttributes struct {
		URL    string `json:"url"`
		Active bool   `json:"active"`
	}
	payload := struct {
		Name               string            `json:"name"`
		URL                string            `json:"url"`
		Description        string            `json:"description,omitempty"`
		HookAttributes     hookAttributes    `json:"hook_attributes"`
		RedirectURL        string            `json:"redirect_url"`
		Public             bool              `json:"public"`
		DefaultPermissions map[string]string `json:"default_permissions"`
		DefaultEvents      []string          `json:"default_events"`
	}{
		Name:               m.Name,
		URL:                m.URL,
		Description:        m.Description,
		HookAttributes:     hookAttributes{URL: m.WebhookURL, Active: true},
		RedirectURL:        m.RedirectURL,
		Public:             false,
		DefaultPermissions: manifestPermissions,
		DefaultEvents:      manifestEvents,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("forge/github: encoding the app manifest: %w", err)
	}
	return data, nil
}

// ManifestRegistrationURL is where the browser POSTs the manifest.
//
// The path differs for an organisation, and the caller cannot derive it
// because the github.com/GHE host split lives in this package. state is
// GitHub's round-trip parameter: the server generates it, GitHub returns it
// with the code, and a callback whose state does not match is a callback the
// server did not start.
func ManifestRegistrationURL(host, org, state string) (string, error) {
	base, err := webBase(host)
	if err != nil {
		return "", err
	}
	path := "/settings/apps/new"
	if o := strings.TrimSpace(org); o != "" {
		path = "/organizations/" + url.PathEscape(o) + "/settings/apps/new"
	}
	u := base + path
	if s := strings.TrimSpace(state); s != "" {
		u += "?state=" + url.QueryEscape(s)
	}
	return u, nil
}

// AppCredentials is what the exchange returns: everything needed to fill in a
// GitConnection and the Secret beside it (ADR-0033 decision 1). Three of its
// fields are secret material and none of them is logged here — they are
// registered with internal/redact on the way out and handed to the caller that
// writes the Secret.
type AppCredentials struct {
	AppID         int64
	Slug          string
	HTMLURL       string
	PEM           []byte
	WebhookSecret []byte
	ClientID      string
	ClientSecret  string
}

// SecretValues returns the literal strings that must never appear in output,
// for registration with internal/redact (issue #117). The PEM is included as a
// whole: it is one value, it is the credential, and a log line that echoed
// half of it would still be a leak of the half it echoed.
func (a AppCredentials) SecretValues() []string {
	out := make([]string, 0, 3)
	for _, v := range []string{string(a.PEM), string(a.WebhookSecret), a.ClientSecret} {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

// String implements fmt.Stringer so freshly created app credentials cannot be
// leaked by the log line that celebrates them.
func (a AppCredentials) String() string {
	return fmt.Sprintf("github app %d (%s), key and webhook secret redacted", a.AppID, a.Slug)
}

// ExchangeManifestCode trades the one-time code from the manifest redirect for
// the created app's credentials.
//
// The code is single-use and expires in an hour, so a failure here is not
// retryable with the same code — the user restarts the flow. That is why the
// error says which step failed rather than only what GitHub answered.
func ExchangeManifestCode(ctx context.Context, host, code string) (AppCredentials, error) {
	if strings.TrimSpace(code) == "" {
		return AppCredentials{}, fmt.Errorf("forge/github: the manifest callback carried no code")
	}
	base, err := apiBase(host)
	if err != nil {
		return AppCredentials{}, err
	}

	// Unauthenticated by design: the one-time code is the credential, which is
	// what lets an instance that holds nothing yet complete this call.
	g := newGitHub()
	u := base + "/app-manifests/" + url.PathEscape(strings.TrimSpace(code)) + "/conversions"
	data, _, err := g.send(ctx, http.MethodPost, u, "", nil)
	if err != nil {
		if isStatus(err, http.StatusNotFound) || isStatus(err, http.StatusUnprocessableEntity) {
			return AppCredentials{}, fmt.Errorf("forge/github: the app manifest code was refused (single-use, and it expires an hour after the redirect): %w", err)
		}
		return AppCredentials{}, err
	}

	var created struct {
		ID            int64  `json:"id"`
		Slug          string `json:"slug"`
		HTMLURL       string `json:"html_url"`
		PEM           string `json:"pem"`
		WebhookSecret string `json:"webhook_secret"`
		ClientID      string `json:"client_id"`
		ClientSecret  string `json:"client_secret"`
	}
	if err := json.Unmarshal(data, &created); err != nil {
		return AppCredentials{}, fmt.Errorf("forge/github: decoding the created app: %w", err)
	}
	if created.ID == 0 || created.PEM == "" {
		return AppCredentials{}, fmt.Errorf("forge/github: the manifest exchange returned no app id or private key: %w", ErrAuthFailed)
	}

	creds := AppCredentials{
		AppID:         created.ID,
		Slug:          created.Slug,
		HTMLURL:       created.HTMLURL,
		PEM:           []byte(created.PEM),
		WebhookSecret: []byte(created.WebhookSecret),
		ClientID:      created.ClientID,
		ClientSecret:  created.ClientSecret,
	}
	redact.Register(creds.SecretValues()...)
	return creds, nil
}
