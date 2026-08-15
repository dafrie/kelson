package forgeconn

import (
	"errors"
	"testing"

	"github.com/dafrie/kelson/internal/controlstore"
	"github.com/dafrie/kelson/internal/forge"
)

// The two flux-operator shapes (ADR-0033 decision 4). Which one is emitted is
// the difference between a Secret that can go stale and one that cannot, so it
// is asserted key by key rather than by round-tripping a fixture.

const probePEM = "-----BEGIN RSA PRIVATE KEY-----\npreview-secret-probe\n-----END RSA PRIVATE KEY-----\n"

func appResolution(host string, installation int64) Resolution {
	return Resolution{
		Stored: controlstore.StoredConnection{Name: "acme-github"},
		Conn: forge.Conn{
			Provider:       "github",
			Host:           host,
			AppID:          12345,
			InstallationID: installation,
			PrivateKeyPEM:  []byte(probePEM),
		},
	}
}

// An app connection emits the githubApp* keys, which is the shape to prefer:
// flux-operator mints its own installation tokens from the app key, so nothing
// kelson wrote expires and staleness stops being a problem to manage.
func TestAppConnectionEmitsTheGitHubAppKeys(t *testing.T) {
	data, shape, err := PreviewSecretData(appResolution("", 678910))
	if err != nil {
		t.Fatalf("PreviewSecretData: %v", err)
	}
	if shape != ShapeGitHubApp {
		t.Fatalf("shape = %q, want %q", shape, ShapeGitHubApp)
	}
	want := map[string]string{
		FluxGitHubAppIDKey:             "12345",
		FluxGitHubAppInstallationIDKey: "678910",
		FluxGitHubAppPrivateKeyKey:     probePEM,
	}
	for key, value := range want {
		if got := string(data[key]); got != value {
			t.Errorf("%s = %q, want %q", key, got, value)
		}
	}
	if len(data) != len(want) {
		t.Errorf("keys = %v, want exactly %v: github.com needs no base URL, and writing a default as though it "+
			"were a choice is what makes a manifest unreadable", keysOf(data), keysOf(toBytes(want)))
	}
	if _, ok := data[FluxUsernameKey]; ok {
		t.Error("an app connection must not also emit basic-auth keys; flux-operator would have to choose")
	}
}

// A GitHub Enterprise host needs the API base URL, because flux-operator
// otherwise talks to api.github.com about an installation that is not there.
func TestEnterpriseHostEmitsTheBaseURL(t *testing.T) {
	data, _, err := PreviewSecretData(appResolution("https://git.acme.internal", 678910))
	if err != nil {
		t.Fatalf("PreviewSecretData: %v", err)
	}
	if got := string(data[FluxGitHubAppBaseURLKey]); got != "https://git.acme.internal/api/v3" {
		t.Errorf("%s = %q, want the enterprise REST base", FluxGitHubAppBaseURLKey, got)
	}

	// github.com, spelled explicitly, is still the default and still silent.
	data, _, err = PreviewSecretData(appResolution("https://github.com", 678910))
	if err != nil {
		t.Fatalf("PreviewSecretData: %v", err)
	}
	if _, ok := data[FluxGitHubAppBaseURLKey]; ok {
		t.Error("github.com must not write a base URL")
	}
}

// An app nobody has installed yet has no shape: the app exists and the
// installation does not, which is a state the `installation` webhook ends and
// not a Secret to write half of.
func TestUninstalledAppHasNoSecret(t *testing.T) {
	_, _, err := PreviewSecretData(appResolution("", 0))
	if !errors.Is(err, ErrNoInstallation) {
		t.Fatalf("error = %v, want ErrNoInstallation", err)
	}
}

// A token connection emits basic auth, which is the only shape it has.
func TestTokenConnectionEmitsBasicAuth(t *testing.T) {
	res := Resolution{
		Stored: controlstore.StoredConnection{Name: "acme-token"},
		Conn:   forge.Conn{Provider: "generic", Token: "ghp_probe_token"},
	}
	data, shape, err := PreviewSecretData(res)
	if err != nil {
		t.Fatalf("PreviewSecretData: %v", err)
	}
	if shape != ShapeBasicAuth {
		t.Fatalf("shape = %q, want %q", shape, ShapeBasicAuth)
	}
	if string(data[FluxPasswordKey]) != "ghp_probe_token" {
		t.Errorf("%s = %q", FluxPasswordKey, data[FluxPasswordKey])
	}
	// Forges ignore the value beside a token but require one to be present.
	if string(data[FluxUsernameKey]) != "x-access-token" {
		t.Errorf("%s = %q, want the default", FluxUsernameKey, data[FluxUsernameKey])
	}

	res.Conn.Username = "acme-bot"
	data, _, err = PreviewSecretData(res)
	if err != nil {
		t.Fatalf("PreviewSecretData: %v", err)
	}
	if string(data[FluxUsernameKey]) != "acme-bot" {
		t.Errorf("a stated username must win: %q", data[FluxUsernameKey])
	}
}

func TestConnectionWithNoCredentialHasNoSecret(t *testing.T) {
	res := Resolution{Stored: controlstore.StoredConnection{Name: "empty"}}
	if _, _, err := PreviewSecretData(res); err == nil {
		t.Fatal("a connection with no credential produced a Secret")
	}
}

// The Secret's name is the lifecycle pair's, so a preview's objects are one
// listing away from each other.
func TestPreviewSecretNameMatchesTheLifecyclePair(t *testing.T) {
	if got := PreviewSecretName("checkout", "staging"); got != "checkout-staging-previews" {
		t.Errorf("PreviewSecretName = %q", got)
	}
}

func keysOf(data map[string][]byte) []string {
	out := make([]string, 0, len(data))
	for k := range data {
		out = append(out, k)
	}
	return out
}

func toBytes(in map[string]string) map[string][]byte {
	out := make(map[string][]byte, len(in))
	for k, v := range in {
		out[k] = []byte(v)
	}
	return out
}
