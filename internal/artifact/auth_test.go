package artifact_test

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/dafrie/kelson/internal/artifact"
	"github.com/dafrie/kelson/internal/redact"
)

// The default credential source is the runner's docker login, because the
// publisher runs in CI where a login step already ran. These tests pin the
// three answers it can give: the credential, nothing (which is a legitimate
// anonymous push), and a refusal.

func writeDockerConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCredentialFromDockerConfig(t *testing.T) {
	path := writeDockerConfig(t, `{"auths":{"ghcr.io":{"username":"robot","password":"s3cret-token"}}}`)

	cred, err := artifact.CredentialFromDockerConfig(path, "ghcr.io")
	if err != nil {
		t.Fatalf("CredentialFromDockerConfig: %v", err)
	}
	if cred.Username != "robot" || cred.Password != "s3cret-token" {
		t.Errorf("no credential was read: %+v", cred)
	}
	// The short form is derived, so it is registered too.
	if cred.Auth != base64.StdEncoding.EncodeToString([]byte("robot:s3cret-token")) {
		t.Errorf("the .auth short form was not reconstructed: %+v", cred)
	}

	// Learning a credential is what makes it unprintable process-wide
	// (issue #117): the registration happens here, not at each surface that
	// might echo it.
	if got := redact.Scrub("token is s3cret-token"); got == "token is s3cret-token" {
		t.Errorf("the credential was not registered for redaction: %q", got)
	}
}

// TestCredentialFromDockerConfigIsAnonymousWhenAbsent: a missing file, or a
// file with no entry for this registry, is an anonymous push — which a public
// registry accepts and a private one refuses with a message naming both ways to
// supply a credential.
func TestCredentialFromDockerConfigIsAnonymousWhenAbsent(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "config.json")
	cred, err := artifact.CredentialFromDockerConfig(missing, "ghcr.io")
	if err != nil {
		t.Fatalf("a missing docker config must not be an error: %v", err)
	}
	if cred.Username != "" || cred.Password != "" {
		t.Errorf("a credential appeared from nowhere: %+v", cred)
	}

	other := writeDockerConfig(t, `{"auths":{"docker.io":{"username":"u","password":"p"}}}`)
	cred, err = artifact.CredentialFromDockerConfig(other, "ghcr.io")
	if err != nil {
		t.Fatalf("a login to a different registry must not be an error: %v", err)
	}
	if cred.Username != "" {
		t.Errorf("a credential for the wrong registry was used: %+v", cred)
	}
}

func TestCredentialFromDockerConfigRejectsABrokenLogin(t *testing.T) {
	path := writeDockerConfig(t, "not json at all")
	if _, err := artifact.CredentialFromDockerConfig(path, "ghcr.io"); err == nil {
		t.Error("an unparseable docker config was treated as no login at all")
	}
}
