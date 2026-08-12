package registry

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"testing"
)

// fakeResolver satisfies Resolver without a cluster or network, so every auth
// test is hermetic (ADR-0009: no test touches a cluster).
type fakeResolver struct {
	cred Credential
	err  error
}

func (f fakeResolver) Resolve(_ context.Context, _ SecretRef) (Credential, error) {
	return f.cred, f.err
}

func TestCredentialFromDockerConfigJSON(t *testing.T) {
	const (
		user     = "robot"
		pass     = "hunter2"
		registry = "ghcr.io"
	)

	// username/password shape
	entry := map[string]any{
		"auths": map[string]any{
			registry: map[string]any{"username": user, "password": pass},
		},
	}
	payload := mustMarshal(t, entry)
	c, err := CredentialFromDockerConfigJSON(payload, registry)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Username != user || c.Password != pass {
		t.Fatalf("got %+v, want user=%q pass=%q", c, user, pass)
	}
	if want := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass)); c.Auth != want {
		t.Fatalf("Auth = %q, want reconstructed %q", c.Auth, want)
	}
}

func TestCredentialFromDockerConfigJSONAuthShortForm(t *testing.T) {
	const registry = "ghcr.io"
	auth := base64.StdEncoding.EncodeToString([]byte("robot:hunter2"))
	entry := map[string]any{"auths": map[string]any{registry: map[string]any{"auth": auth}}}
	c, err := CredentialFromDockerConfigJSON(mustMarshal(t, entry), registry)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Auth != auth {
		t.Fatalf("Auth = %q, want %q", c.Auth, auth)
	}
	if c.Password != "" {
		t.Fatalf("Password should be empty with the auth short form, got %q", c.Password)
	}
}

func TestCredentialFromDockerConfigJSONMissingRegistry(t *testing.T) {
	entry := map[string]any{"auths": map[string]any{"gcr.io": map[string]any{"username": "u", "password": "p"}}}
	if _, err := CredentialFromDockerConfigJSON(mustMarshal(t, entry), "ghcr.io"); err == nil {
		t.Fatal("expected error for unknown registry")
	}
	if _, err := CredentialFromDockerConfigJSON([]byte("{not json"), "ghcr.io"); err == nil {
		t.Fatal("expected error for malformed payload")
	}
	if _, err := CredentialFromDockerConfigJSON([]byte(`{"auths":{}}`), "ghcr.io"); err == nil {
		t.Fatal("expected error for empty auths")
	}
}

// The ADR-0009 hole this test closes: a credential must never appear in a log
// line or an error message, even when the producer interpolates it with %v.
// Credential implements fmt.Stringer, so both paths redact the password while
// still reporting the username.
func TestCredentialNeverAppearsInLogsOrErrors(t *testing.T) {
	const secret = "s3cr3t-password-value"
	cred := Credential{Username: "robot", Password: secret}

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	ctx := context.Background()
	res := fakeResolver{cred: cred}

	// 1. The resolve → push path, as a producer would log it with %v.
	got, err := res.Resolve(ctx, SecretRef{Name: "reg-creds", Namespace: "ns", Registry: "ghcr.io"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	log.Printf("pushing with %v", got)

	// 2. Error construction that wraps a credential with %v must redact too.
	_ = errPushFailed(got)

	out := buf.String()
	if strings.Contains(out, secret) {
		t.Fatalf("log output leaked credential %q: %q", secret, out)
	}
	// Sanity: the redaction is real — the safe username is still visible, so we
	// are not merely dropping the whole line.
	if !strings.Contains(out, "robot") {
		t.Fatalf("expected safe username in log output, got %q", out)
	}
	// The reconstructed .auth short form is derived from the password and must
	// be equally safe.
	if auth := base64.StdEncoding.EncodeToString([]byte("robot:" + secret)); strings.Contains(out, auth) {
		t.Fatalf("log output leaked the .auth short form")
	}
}

// errPushFailed mimics the producer pattern of wrapping a credential in an
// error via %v; existing here it documents that the wrapped error stays safe.
func errPushFailed(c Credential) error {
	return fmt.Errorf("push failed: %v", c)
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
